package leader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	fakecoordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// testLeaseTiming is the fastest timing client-go accepts that still records
// a nonzero Lease duration (whole seconds).
var testLeaseTiming = leaseTiming{
	LeaseDuration: time.Second,
	RenewDeadline: 500 * time.Millisecond,
	RetryPeriod:   100 * time.Millisecond,
}

const (
	testNamespace = "diderot"
	testLeaseName = "diderot-leader"
)

var leasesResource = coordinationv1.SchemeGroupVersion.WithResource("leases")

// fakeAPIServer is the coordination/v1 fake clientset backed by an object
// tracker, plus the apiserver behavior an election depends on that the
// tracker lacks: resourceVersion conflicts, so two contenders cannot both
// win one Lease. The fake serializes every reactor call.
type fakeAPIServer struct {
	*fakecoordinationv1.FakeCoordinationV1
	tracker k8stesting.ObjectTracker
	version int
	// failUpdates makes every Lease write fail, as during a partition from
	// the apiserver.
	failUpdates atomic.Bool
	// released counts writes that clear the holder (ReleaseOnCancel).
	released atomic.Int32
}

func newFakeAPIServer() *fakeAPIServer {
	f := &fakeAPIServer{
		FakeCoordinationV1: &fakecoordinationv1.FakeCoordinationV1{Fake: &k8stesting.Fake{}},
		tracker:            k8stesting.NewObjectTracker(scheme.Scheme, scheme.Codecs.UniversalDecoder()),
	}
	f.AddReactor("get", "leases", k8stesting.ObjectReaction(f.tracker))
	f.AddReactor("create", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.CreateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		lease.ResourceVersion = f.nextVersion()
		return true, lease, f.tracker.Create(leasesResource, lease, action.GetNamespace())
	})
	f.AddReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if f.failUpdates.Load() {
			return true, nil, errors.New("apiserver unreachable")
		}
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		current, err := f.tracker.Get(leasesResource, action.GetNamespace(), lease.Name)
		if err != nil {
			return true, nil, err
		}
		if current.(*coordinationv1.Lease).ResourceVersion != lease.ResourceVersion {
			return true, nil, apierrors.NewConflict(leasesResource.GroupResource(), lease.Name, errors.New("resourceVersion changed"))
		}
		lease.ResourceVersion = f.nextVersion()
		if holder := lease.Spec.HolderIdentity; holder == nil || *holder == "" {
			f.released.Add(1)
		}
		return true, lease, f.tracker.Update(leasesResource, lease, action.GetNamespace())
	})
	return f
}

func (f *fakeAPIServer) nextVersion() string {
	f.version++
	return strconv.Itoa(f.version)
}

func (f *fakeAPIServer) lock(identity string) *resourcelock.LeaseLock {
	return &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: testLeaseName, Namespace: testNamespace},
		Client:     f,
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
}

func (f *fakeAPIServer) holder(t *testing.T) string {
	t.Helper()
	obj, err := f.tracker.Get(leasesResource, testNamespace, testLeaseName)
	require.NoError(t, err)
	if holder := obj.(*coordinationv1.Lease).Spec.HolderIdentity; holder != nil {
		return *holder
	}
	return ""
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// terms records leader terms across contenders and fails the test if two
// ever overlap.
type terms struct {
	t      *testing.T
	active atomic.Int32
	mu     sync.Mutex
	leader []string
	start  chan string
}

func newTerms(t *testing.T) *terms {
	return &terms{t: t, start: make(chan string, 64)}
}

// run is a leaderFn for identity: it records the term, then calls work.
func (r *terms) run(identity string, work func(ctx context.Context, term int)) func(context.Context) {
	return func(ctx context.Context) {
		if n := r.active.Add(1); n != 1 {
			r.t.Errorf("%s started leading while %d other terms were active", identity, n-1)
		}
		defer r.active.Add(-1)
		r.mu.Lock()
		r.leader = append(r.leader, identity)
		term := len(r.leader)
		r.mu.Unlock()
		r.start <- identity
		work(ctx, term)
	}
}

func (r *terms) next(t *testing.T) string {
	t.Helper()
	select {
	case identity := <-r.start:
		return identity
	case <-time.After(10 * time.Second):
		t.Fatal("no leader term started")
		return ""
	}
}

func untilDone(ctx context.Context, _ int) { <-ctx.Done() }

// runContender runs runLease in the background; the returned func cancels
// it and waits for it to return.
func runContender(t *testing.T, api *fakeAPIServer, identity string, leaderFn func(context.Context)) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runLease(ctx, discardLogger(), api.lock(identity), testLeaseTiming, leaderFn) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Errorf("%s: runLease did not return after cancellation", identity)
		}
	})
	t.Cleanup(stop)
	return stop
}

// TestRunLeaseReContendsAfterLeaderWorkReturns: leader work that returns
// while the Lease is still held releases it, and the process contends again
// instead of serving with no leader work until restart.
func TestRunLeaseReContendsAfterLeaderWorkReturns(t *testing.T) {
	api := newFakeAPIServer()
	record := newTerms(t)
	runContender(t, api, "pod-a", record.run("pod-a", func(ctx context.Context, term int) {
		if term >= 3 {
			<-ctx.Done()
		}
	}))
	for range 3 {
		assert.Equal(t, "pod-a", record.next(t))
	}
	assert.GreaterOrEqual(t, api.released.Load(), int32(2), "each returned term releases the Lease")
}

// TestRunLeaseReContendsAfterLosingLease: a leader cut off from the apiserver
// loses its term at the renew deadline, and contends again once leader work
// has stopped.
func TestRunLeaseReContendsAfterLosingLease(t *testing.T) {
	api := newFakeAPIServer()
	record := newTerms(t)
	lost := make(chan struct{})
	runContender(t, api, "pod-a", record.run("pod-a", func(ctx context.Context, term int) {
		if term == 1 {
			api.failUpdates.Store(true)
			<-ctx.Done()
			close(lost)
			api.failUpdates.Store(false)
			return
		}
		<-ctx.Done()
	}))
	require.Equal(t, "pod-a", record.next(t))
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("leader context outlived the lost Lease")
	}
	assert.Equal(t, "pod-a", record.next(t), "no new term after losing the Lease")
	assert.Equal(t, "pod-a", api.holder(t))
}

// TestRunLeaseHandsOverWithoutOverlap: a second contender waits while the
// leader renews, a returned term hands the Lease on, and terms never overlap.
func TestRunLeaseHandsOverWithoutOverlap(t *testing.T) {
	api := newFakeAPIServer()
	record := newTerms(t)
	release := make(chan struct{})
	runContender(t, api, "pod-a", record.run("pod-a", func(ctx context.Context, term int) {
		if term == 1 {
			select {
			case <-release:
				return
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}))
	require.Equal(t, "pod-a", record.next(t))
	runContender(t, api, "pod-b", record.run("pod-b", untilDone))

	// pod-b cannot take a Lease pod-a keeps renewing.
	select {
	case identity := <-record.start:
		t.Fatalf("%s led while pod-a held the Lease", identity)
	case <-time.After(3 * testLeaseTiming.LeaseDuration):
	}
	close(release)
	winner := record.next(t)
	assert.Contains(t, []string{"pod-a", "pod-b"}, winner)
	assert.Equal(t, winner, api.holder(t))
}

// TestRunLeaseShutdownReleasesLease: cancellation stops leader work and
// releases the Lease, so a successor need not wait out its duration.
func TestRunLeaseShutdownReleasesLease(t *testing.T) {
	api := newFakeAPIServer()
	record := newTerms(t)
	workDone := make(chan struct{})
	stop := runContender(t, api, "pod-a", record.run("pod-a", func(ctx context.Context, _ int) {
		<-ctx.Done()
		close(workDone)
	}))
	require.Equal(t, "pod-a", record.next(t))
	stop()
	select {
	case <-workDone:
	default:
		t.Fatal("runLease returned before leader work stopped")
	}
	assert.Empty(t, api.holder(t))
	select {
	case identity := <-record.start:
		t.Fatalf("%s started a term after shutdown", identity)
	default:
	}
}

func TestRunLeaseRejectsInvalidTiming(t *testing.T) {
	api := newFakeAPIServer()
	err := runLease(context.Background(), discardLogger(), api.lock("pod-a"),
		leaseTiming{LeaseDuration: time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: time.Second},
		func(context.Context) { t.Error("leader work ran under an invalid election") })
	require.ErrorContains(t, err, "leader elector")
}
