package scripted

// Goroutine-leak tests (issue #18). No third-party goleak: the check is that
// runtime.NumGoroutine converges back to the baseline once a session is over,
// and that an idle session holds no goroutine at all.

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// leakSettle polls until the goroutine count is back at base, then fails with a
// full dump if it never gets there. Goroutines of the testing package and of
// earlier tests are part of base, so only what this test leaks can fail it.
func leakSettle(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines did not converge: %d now, %d at the start\n%s", runtime.NumGoroutine(), base, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// leakBase lets earlier tests' goroutines finish before the baseline is taken.
func leakBase() int {
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()
	return runtime.NumGoroutine()
}

func leakNew(t *testing.T, grace time.Duration, script ...Turn) *Session {
	t.Helper()
	s, err := NewWithOptions(Options{AbortGrace: grace}, script...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func leakWait(t *testing.T, s *Session, what string, ok func(fold.Status) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok(fold.StatusOf(s.Records(oarlock.Cursor{})).Value) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func leakRunning(st fold.Status) bool { return st.Kind == fold.StatusRunning }

func TestNoGoroutineLeakAfterDispose(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	base := leakBase()
	for i := 0; i < 20; i++ {
		s := leakNew(t, time.Minute, Turn{Say("a"), Hang()}, Turn{Say("b")})
		_ = s.Events(nil, func(oarlock.Record) {}) // left un-unsubscribed on purpose
		if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt = %+v", out)
		}
		leakWait(t, s, "running", leakRunning)
		if out, _ := s.Queue(context.Background(), "later", oarlock.InputOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("queue = %+v", out)
		}
		if err := s.Dispose(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	leakSettle(t, base)
}

func TestNoGoroutineLeakAfterDie(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	base := leakBase()
	for i := 0; i < 20; i++ {
		s := leakNew(t, time.Minute, Turn{Say("a"), Die(1)})
		if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt = %+v", out)
		}
		leakWait(t, s, "exited", func(st fold.Status) bool { return st.Kind == fold.StatusExited })
		_ = s.Dispose(context.Background())
	}
	leakSettle(t, base)
}

func TestNoGoroutineLeakAfterTakeOver(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	base := leakBase()
	for i := 0; i < 5; i++ {
		s := leakNew(t, 30*time.Millisecond, Turn{IgnoreAbort(), Hang()})
		if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt = %+v", out)
		}
		leakWait(t, s, "running", leakRunning)
		if out, _ := s.Abort(context.Background()); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("abort = %+v", out)
		}
		leakWait(t, s, "exited", func(st fold.Status) bool { return st.Kind == fold.StatusExited })
		_ = s.Dispose(context.Background())
	}
	leakSettle(t, base)
}

// A take-over needs no Dispose to release its goroutines: the process is gone
// and the turn goroutine and the timer ended with it.
func TestNoGoroutineLeakAfterTakeOverWithoutDispose(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	base := leakBase()
	s := leakNew(t, 30*time.Millisecond, Turn{IgnoreAbort(), Hang()})
	if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	leakWait(t, s, "running", leakRunning)
	if out, _ := s.Abort(context.Background()); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	leakSettle(t, base)
}

func TestIdleSessionHoldsNoGoroutine(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	base := leakBase()
	s := New(Turn{Say("never prompted")})
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("a freshly opened idle session holds %d goroutine(s)", n-base)
	}
	if err := s.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	leakSettle(t, base)
}

func TestFinishedTurnHoldsNoGoroutineWithoutDispose(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	base := leakBase()
	s := New(Turn{Say("done")})
	if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	leakWait(t, s, "idle", func(st fold.Status) bool { return st.Kind == fold.StatusIdle && st.LastTurnOutcome != nil })
	leakSettle(t, base) // idle again: no goroutine remains although the session was never disposed
	_ = s.Dispose(context.Background())
}

func TestNoGoroutineLeakConcurrentControls(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	base := leakBase()
	script := make([]Turn, 40)
	for i := range script {
		script[i] = Turn{Say("x"), Delay(time.Millisecond), Hang()}
	}
	s := leakNew(t, 20*time.Millisecond, script...)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 25; i++ {
				switch (g + i) % 4 {
				case 0:
					_, _ = s.Prompt(ctx, "p", oarlock.PromptOptions{})
				case 1:
					_, _ = s.Queue(ctx, "q", oarlock.InputOptions{})
				case 2:
					_, _ = s.Abort(ctx)
				default:
					_ = s.Records(oarlock.Cursor{})
				}
				if g == 7 && i == 12 {
					_ = s.Dispose(ctx)
				}
			}
		}(g)
	}
	wg.Wait()
	if err := s.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := s.Records(oarlock.Cursor{})
	for i, r := range records {
		if r.Seq != uint64(i+1) {
			t.Fatalf("Seq %d at index %d: the stream is not dense after concurrent controls", r.Seq, i)
		}
	}
	leakSettle(t, base)
}
