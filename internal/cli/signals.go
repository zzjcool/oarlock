package cli

import (
	"os"
	"os/signal"
	"sync/atomic"
)

// signalGuard turns the first two SIGINTs into two edges:
//
//   - first closes: the caller cancels the prompt context, so PromptAndWait
//     aborts the turn and reports RunInterrupted;
//   - second closes: the caller disposes the session immediately and exits 130
//     without waiting for the runtime's own end.
//
// Further signals are ignored: the process is already leaving. Closing the
// guard stops the watcher and unregisters signal.Notify.
type signalGuard struct {
	first  chan struct{}
	second chan struct{}
	done   chan struct{}
	stopFn func()
	fired  atomic.Bool
}

// newSignalGuard starts one watcher goroutine. ch must not be nil; stop is the
// matching signal.Stop (or a no-op for an injected channel).
func newSignalGuard(ch <-chan os.Signal, stop func()) *signalGuard {
	g := &signalGuard{
		first:  make(chan struct{}),
		second: make(chan struct{}),
		done:   make(chan struct{}),
		stopFn: stop,
	}
	go func() {
		select {
		case <-ch:
			g.fired.Store(true)
			close(g.first)
		case <-g.done:
			return
		}
		select {
		case <-ch:
			close(g.second)
		case <-g.done:
		}
	}()
	return g
}

// close stops the watcher and the underlying signal registration.
func (g *signalGuard) close() {
	close(g.done)
	if g.stopFn != nil {
		g.stopFn()
	}
}

// interrupted reports whether the first SIGINT was seen.
func (g *signalGuard) interrupted() bool { return g.fired.Load() }

// interruptChannel returns the channel to watch and the cleanup for it.
func (a *App) interruptChannel() (<-chan os.Signal, func()) {
	if a.Signals != nil {
		return a.Signals, func() {}
	}
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}
