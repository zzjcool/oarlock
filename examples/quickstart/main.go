// Command quickstart is the shortest runnable oarlock host: it registers a
// runtime, probes the installation, opens one session, sends one prompt, and
// reads the answer back through the public observe surface.
//
//	go run ./examples/quickstart
//
// The default run stops after the read-only installation probe (`pi --version`
// on PATH): no session is opened, no `pi --mode rpc` child is started and no
// network call is made. Set OAR_QUICKSTART_LIVE=1 to run the live half, which
// needs a logged-in `pi` 1.4.0.
package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/zzjcool/oarlock"

	// This is how a runtime registers itself: the package's init calls
	// oarlock.Register(Runtime()), and the registry is process-wide. Importing
	// it under its own name would register it exactly the same way; the
	// underscore is only Go's marking for "the import is for the side effect,
	// nothing here is referenced by name".
	_ "github.com/zzjcool/oarlock/adapters/pi"

	"github.com/zzjcool/oarlock/observe"
)

// prompt asks for the smallest possible answer, so one turn finishes quickly
// and the readback below has something to report.
const prompt = "Reply with the single word: ok"

func main() {
	if err := run(context.Background()); err != nil {
		// The error text carries the failing step; printing it and exiting
		// non-zero is the whole of this command's error policy.
		fmt.Fprintln(os.Stderr, "quickstart:", err)
		os.Exit(1)
	}
}

// run is the example in the order a host does it. A path that means "this
// machine cannot run the live half" prints why and returns nil: an uninstalled
// or signed-out runtime is a skip, never a fake success and never a panic.
// Only a failure after a session is already open is an error, and main turns
// that into exit status 1.
func run(ctx context.Context) error {
	// Require is what proves the registration above really happened: without
	// the import this fails with oarlock.ErrRuntimeNotFound.
	rt, err := oarlock.Require("pi")
	if err != nil {
		return fmt.Errorf("looking up the pi runtime: %w", err)
	}

	// Installation is a read-only probe: it runs `pi --version` on PATH and
	// reads no account, no session file and no network. That is why it can run
	// before the live opt-in.
	inst, probeErr := rt.Installation(ctx)
	fmt.Printf("runtime:   %s\n", rt.ID)
	fmt.Printf("available: %v\n", inst.Available)
	fmt.Printf("version:   %s\n", orNotReported(inst.Version))
	fmt.Printf("path:      %s\n", orNotReported(inst.Path))
	fmt.Printf("source:    %s\n", orNotReported(inst.Source))

	if !inst.Available {
		fmt.Println("skipped: the probe found no pi on PATH; install pi 1.4.0, put it on PATH, and run again")
		return nil
	}
	if probeErr != nil {
		// Installed but unusable — for pi that means a version outside the
		// support window. A broken setup is not a missing one, so this is a
		// failure, not a skip.
		return fmt.Errorf("probing the pi installation: %w", probeErr)
	}

	if os.Getenv("OAR_QUICKSTART_LIVE") != "1" {
		// Everything that spawns the runtime and spends the account sits behind
		// this gate, so the default run never gets past it. Saying exactly what
		// the live half would do is more useful than a bare "skipped".
		fmt.Println()
		fmt.Println("notice:    no session was opened. The live half starts `pi --mode rpc`")
		fmt.Println("           in a throwaway directory, sends one prompt, and prints")
		fmt.Println("           the reply plus the folded status, usage and context.")
		fmt.Println("           It needs a logged-in pi 1.4.0 (it uses your account).")
		fmt.Println("           Opt in with: OAR_QUICKSTART_LIVE=1 go run ./examples/quickstart")
		return nil
	}

	// A throwaway directory keeps the example from reading or writing the
	// checkout it was launched from. Getwd is the fallback for a machine with
	// no usable temp directory.
	cwd, tempErr := os.MkdirTemp("", "oarlock-quickstart-")
	if tempErr != nil {
		wd, wdErr := os.Getwd()
		if wdErr != nil {
			return fmt.Errorf("choosing a working directory: %w", wdErr)
		}
		cwd = wd
	} else {
		defer func() { _ = os.RemoveAll(cwd) }()
	}

	// Model and Effort stay empty on purpose: the empty value means "the
	// runtime's own default". This example names no model id, because an id it
	// cannot verify would be a claim about your account, not a fact.
	fmt.Printf("\nsession:   cwd=%s\n", cwd)
	sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: cwd})
	if err != nil {
		// The runtime is installed but cannot open a session here — most often
		// "not signed in". That is exactly the case this example must report as
		// a skip instead of dying.
		fmt.Printf("skipped: opening the pi session: %v\n", err)
		return nil
	}
	// Dispose runs even when the wait below is cancelled or the process is
	// unwinding, so it gets a context detached from cancellation; otherwise the
	// dispose record would be dropped exactly when it matters.
	defer sess.Dispose(context.WithoutCancel(ctx))

	// Two minutes is generous for one word. On expiry PromptAndWait aborts the
	// turn and reports RunInterrupted rather than inventing a turn end.
	res, err := oarlock.PromptAndWait(ctx, sess, prompt, oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	fmt.Printf("outcome:   %s\n", res.Outcome)
	if res.Turn != nil {
		fmt.Printf("turn:      %s\n", turnLine(res.Turn))
	}
	if res.Code != "" {
		fmt.Printf("rejected:  %s: %s\n", res.Code, res.Reason)
	}
	fmt.Printf("text:      %q\n", res.Text)

	// The read surface has two shapes and they must agree: the Readback
	// assertion (one check, then the methods) and the free folds applied to the
	// raw records. Both are printed so the equality is visible, not asserted.
	rb, ok := sess.(observe.Readback)
	if !ok {
		return fmt.Errorf("%T does not implement observe.Readback", sess)
	}
	fmt.Println()
	fmt.Println("readback (sess.(observe.Readback)):")
	fromReadback := rb.Status()
	fmt.Printf("  status:  %s\n", statusLine(fromReadback))
	fmt.Printf("  usage:   %s\n", usageLine(rb.Usage()))
	fmt.Printf("  context: %s\n", contextLine(rb.ContextUsage()))

	records := sess.Records(oarlock.Cursor{}) // zero cursor replays the whole stream
	folded := observe.StatusOf(records)
	fmt.Println("fold (observe.StatusOf over sess.Records(oarlock.Cursor{})):")
	fmt.Printf("  status:  %s\n", statusLine(folded))
	fmt.Printf("  records: %d\n", len(records))
	// DeepEqual, not ==: a fold clones the pointers it returns, so
	// Status.LastTurnOutcome is an equal-but-distinct pointer on every call and
	// == would report a disagreement that does not exist. Only status is shown
	// both ways; Usage carries slices, so == is illegal there, and both paths
	// call the same fold anyway.
	switch {
	case reflect.DeepEqual(folded, fromReadback):
		fmt.Println("  match:   yes (the Readback method and the fold read the same stream)")
	case folded.Seq != fromReadback.Seq:
		// pi can append a record between the two reads. A newer answer to the same
		// question is not a disagreement.
		fmt.Printf("  match:   n/a (records advanced between the reads: seq %d vs %d)\n", fromReadback.Seq, folded.Seq)
	default:
		fmt.Println("  match:   NO (same seq, different status)")
	}

	if res.Outcome != oarlock.RunCompleted {
		fmt.Printf("\nnote: the turn ended %q, not completed; the turn line above says why\n", res.Outcome)
	}
	return nil
}

// statusLine renders a status fold, including the phase while a turn runs.
func statusLine(r observe.QueryResult[observe.Status]) string {
	line := string(r.Value.Kind)
	if r.Value.Phase.Kind != "" {
		line += "/" + string(r.Value.Phase.Kind)
	}
	if r.Value.Phase.Tool != "" {
		line += " " + r.Value.Phase.Tool
	}
	return fmt.Sprintf("%s (seq %d)", line, r.Seq)
}

// usageLine renders the usage fold. A nil Total is the runtime saying "not
// reported", which is not the same as zero tokens.
func usageLine(r observe.QueryResult[observe.Usage]) string {
	if r.Value.Total == nil {
		return fmt.Sprintf("not reported (seq %d)", r.Seq)
	}
	return fmt.Sprintf("input=%d output=%d agents=%d (seq %d)",
		r.Value.Total.Input, r.Value.Total.Output, len(r.Value.ByAgent), r.Seq)
}

// contextLine renders the latest reported context fullness.
func contextLine(r observe.QueryResult[*observe.ContextReport]) string {
	if r.Value == nil {
		return fmt.Sprintf("not reported (seq %d)", r.Seq)
	}
	u := r.Value.Usage
	return fmt.Sprintf("tokens=%s window=%s percent=%s (seq %d)",
		ptrInt(u.Tokens), ptrInt(u.ContextWindow), ptrPct(u.Percent), r.Seq)
}

// turnLine is the runtime's own completion report: kind, and its failure class
// or reason when it gave one.
func turnLine(t *oarlock.TurnOutcome) string {
	line := string(t.Kind)
	if t.Failure != "" {
		line += " (" + string(t.Failure) + ")"
	}
	if t.Reason != "" {
		line += ": " + t.Reason
	}
	return line
}

// orNotReported keeps an empty probe field honest: the probe answers "" rather
// than guessing, and a bare empty column would read like a bug.
func orNotReported(s string) string {
	if s == "" {
		return "(not reported)"
	}
	return s
}

// ptrInt and ptrPct keep the "nil means the runtime did not report this
// number" distinction visible. Printing 0 for a missing count would reintroduce
// exactly the lie the folds avoid.
func ptrInt(v *int64) string {
	if v == nil {
		return "?"
	}
	return strconv.FormatInt(*v, 10)
}

func ptrPct(v *float64) string {
	if v == nil {
		return "?"
	}
	return strconv.FormatFloat(*v, 'f', 1, 64) + "%"
}
