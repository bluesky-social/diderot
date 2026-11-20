package worker

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// handlerPanicsTotal counts a panic Recover absorbed, labelled by the site
// that launched the goroutine it ran on ("slack-handler", "slack-ask",
// "discord-ask", "mcp-ask-task", and so on). It is one metric shared by
// every serving-path call site, registered lazily against
// prometheus.DefaultRegisterer (the registry cmd/diderot actually serves
// /metrics from) so a site deep inside a connector or the MCP server does
// not need its own registerer threaded in just for this counter.
var (
	handlerPanicsOnce sync.Once
	handlerPanicsVec  *prometheus.CounterVec
)

func handlerPanicsTotal() *prometheus.CounterVec {
	handlerPanicsOnce.Do(func() {
		handlerPanicsVec = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_handler_panics_total",
			Help: "Panics recovered on a serving-path goroutine (a Slack/Discord event handler or Ask turn, an MCP ask task) by the site that launched it. Each occurrence is a bug: without this recovery the goroutine's panic would otherwise crash the whole process instead of just failing its one unit of work.",
		}, []string{"site"})
		if err := prometheus.DefaultRegisterer.Register(handlerPanicsVec); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				handlerPanicsVec = already.ExistingCollector.(*prometheus.CounterVec)
			} else {
				panic(err)
			}
		}
	})
	return handlerPanicsVec
}

// Recover, deferred directly (see below) at the top of a goroutine spawned
// on a serving path -- a Slack or Discord event handler, an Ask turn, an
// MCP ask task -- stops an in-flight panic from unwinding past it. Left
// unrecovered, that panic would reach the Go runtime and kill the whole
// process -- every other in-flight Slack thread, Ask turn, and the
// HTTP/MCP server along with it -- rather than just failing the one event
// or turn that triggered it. This is the gap package worker's own error
// policy (above) does not close on its own: safeRun's recover only
// protects the single goroutine it calls a worker's Run on, not a further
// goroutine that Run (or a handler it dispatches to) spawns.
//
// Recover logs the panic at error level with its stack and site, and
// counts it in diderot_handler_panics_total{site}. Any onPanic funcs run
// afterward, only when a panic was absorbed, so the caller can still end
// its one unit of work the way an ordinary failure would -- post a
// failure reply, finish a task as failed, release a claim -- instead of
// silently doing nothing. Recover only stops the crash; it never swallows
// a panic without a trace, and a site's onPanic must not leave a claim,
// lease, lock, or working-record row held. Each onPanic func gets its own
// recovery too, so a second panic inside cleanup (a DB write, a network
// call) is contained the same way rather than escaping uncontained.
//
// Go's recover only has an effect when called directly by the function a
// defer statement names, not by a function that one calls in turn. So
// every call site defers Recover ITSELF -- `defer worker.Recover(logger,
// "site")`, or with cleanup, `defer worker.Recover(logger, "site", cleanup)`
// -- never `defer func() { worker.Recover(...) }()`, which would wrap it
// in exactly the extra frame that breaks recover().
func Recover(logger *slog.Logger, site string, onPanic ...func()) bool {
	r := recover()
	if r == nil {
		return false
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("recovered panic on a serving-path goroutine",
		"site", site, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	handlerPanicsTotal().WithLabelValues(site).Inc()
	for _, f := range onPanic {
		// A site's cleanup (finishing a task as failed, posting a reply)
		// does real work -- a DB write, a network call -- and can itself
		// panic. That second panic must not escape uncontained: recover
		// only protects the single already-panicking call it stops, not
		// whatever runs after it, including another onPanic func.
		func() {
			defer Recover(logger, site+"-cleanup")
			f()
		}()
	}
	return true
}
