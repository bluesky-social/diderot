// Package leader picks the one diderot process that owns all leader-only
// work (Slack Socket Mode, embed worker, etc.). Run holds a Kubernetes
// Lease through client-go's leaderelection; RunPostgres holds a Postgres
// advisory lock for deployments without a Kubernetes API (postgres.go).
package leader

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RunInline skips leader election and invokes leaderFn directly — for
// local dev, or a deployment known to run exactly one process. Blocks
// until ctx is cancelled. The caller is responsible for knowing this
// is safe: nothing stops a second process from running the same work.
func RunInline(ctx context.Context, logger *slog.Logger, leaderFn func(context.Context)) {
	logger.Warn("leader election disabled; running leader-only work inline")
	leaderFn(ctx)
}

// leaseTiming is a Lease election's timing (see
// leaderelection.LeaderElectionConfig). The Lease records its duration in
// whole seconds.
type leaseTiming struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

var defaultLeaseTiming = leaseTiming{
	LeaseDuration: 15 * time.Second,
	RenewDeadline: 10 * time.Second,
	RetryPeriod:   2 * time.Second,
}

// Run blocks until ctx is cancelled, contending for a Kubernetes Lease and
// re-contending after every lost term, as RunPostgres does. While this
// instance is the leader, leaderFn runs with a context that is cancelled the
// moment leadership is lost or SIGTERM arrives; Run does not contend again
// until leaderFn has returned. leaderFn runs once per term, so it must be
// safe to call again in the same process, and is expected to respect its
// context and return promptly when cancelled. If leaderFn returns while
// leadership is still held, the Lease is released and contention starts
// over. An empty namespace is the pod's own.
//
// Run returns an error only when the election cannot start: no namespace, no
// in-cluster credentials, or an invalid election config. No retry fixes
// those. Failures once it runs, such as a Lease update the API server
// refuses, are retried for as long as ctx lasts.
func Run(ctx context.Context, logger *slog.Logger, namespace, leaseName string, leaderFn func(context.Context)) error {
	namespace, err := leaseNamespace(namespace, serviceAccountNamespace)
	if err != nil {
		return err
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("the Kubernetes Lease election can't load this pod's service account (%w); "+
			"mount its token (automountServiceAccountToken), or elect through Postgres with DIDEROT_LEADER_BACKEND=postgres", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("new kubernetes client: %w", err)
	}

	id := os.Getenv("POD_NAME")
	if id == "" {
		id = fmt.Sprintf("diderot-unknown-%d", time.Now().UnixNano())
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}
	return runLease(ctx, logger, lock, defaultLeaseTiming, leaderFn)
}

// serviceAccountNamespace is where Kubernetes writes a pod's namespace, beside
// the service account token the election authenticates with.
const serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// leaseNamespace is namespace, or when it is empty the pod's own, read from
// path: the Lease and the Role that grants it live where the pod runs.
func leaseNamespace(namespace, path string) (string, error) {
	if namespace != "" {
		return namespace, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the pod's namespace (set POD_NAMESPACE instead): %w", err)
	}
	if namespace = strings.TrimSpace(string(raw)); namespace == "" {
		return "", fmt.Errorf("%s is empty; set POD_NAMESPACE", path)
	}
	return namespace, nil
}

// runLease contends for lock, one election term at a time, until ctx is
// canceled. It returns an error only for an invalid election config.
func runLease(ctx context.Context, logger *slog.Logger, lock resourcelock.Interface, timing leaseTiming, leaderFn func(context.Context)) error {
	for {
		if err := runLeaseTerm(ctx, logger, lock, timing, leaderFn); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Info("re-contending for leadership", "id", lock.Identity(), "retry", timing.RetryPeriod)
		if !sleepContext(ctx, timing.RetryPeriod) {
			return nil
		}
	}
}

// runLeaseTerm runs one election: it waits for the Lease, leads until
// leadership is lost, ctx is canceled, or leaderFn returns, and returns once
// leaderFn has returned and the election has stopped. A leader elector
// serves a single term, so each term builds a new one.
func runLeaseTerm(ctx context.Context, logger *slog.Logger, lock resourcelock.Interface, timing leaseTiming, leaderFn func(context.Context)) error {
	id := lock.Identity()
	// Ending the term's context stops the election and, with ReleaseOnCancel,
	// releases a Lease still held after leaderFn returns.
	termCtx, endTerm := context.WithCancel(ctx)
	defer endTerm()
	startedLeading := make(chan context.Context, 1)
	leaderCallbackDone := make(chan struct{})
	electionDone := make(chan struct{})

	config := leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   timing.LeaseDuration,
		RenewDeadline:   timing.RenewDeadline,
		RetryPeriod:     timing.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				logger.Info("became leader", "id", id)
				startedLeading <- leaderCtx
				// client-go invokes this callback in a goroutine and does not
				// join it when election.Run returns. Keep the callback alive
				// until Run has synchronously joined leaderFn below.
				select {
				case <-leaderCallbackDone:
				case <-electionDone:
				}
			},
			OnStoppedLeading: func() {
				logger.Info("lost leadership", "id", id)
			},
			OnNewLeader: func(identity string) {
				if identity != id {
					logger.Info("new leader observed", "leader", identity)
				}
			},
		},
	}

	elector, err := leaderelection.NewLeaderElector(config)
	if err != nil {
		return fmt.Errorf("new leader elector: %w", err)
	}
	go func() {
		elector.Run(termCtx)
		close(electionDone)
	}()

	select {
	case leaderCtx := <-startedLeading:
		leaderFn(leaderCtx)
		if leaderCtx.Err() == nil {
			logger.Warn("leader work returned while holding leadership; releasing the lease", "id", id)
		}
		endTerm()
		close(leaderCallbackDone)
		<-electionDone
	case <-electionDone:
		// The process stopped before this replica acquired leadership, or
		// lost it before leaderFn could start.
	}
	return nil
}
