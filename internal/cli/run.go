package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

// stringList is a repeatable string flag (--image A --image B).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// forcedDisposeBound bounds the dispose on the second-SIGINT path. The normal
// path has no bound: it is the only chance to record the session's last words.
const forcedDisposeBound = 2 * time.Second

// runFlags is the parsed flag set of one `run` command.
type runFlags struct {
	cwd         string
	model       string
	effort      string
	serviceTier string
	resume      string
	images      stringList
	timeout     time.Duration
	json        bool
	record      string
}

func (a *App) runRun(args []string) int {
	fs := a.newFlagSet("run")
	var f runFlags
	fs.StringVar(&f.cwd, "cwd", "", "directory the runtime operates in (default: current directory)")
	fs.StringVar(&f.model, "model", "", "runtime-native model id")
	fs.StringVar(&f.effort, "effort", "", "runtime-native reasoning level")
	fs.StringVar(&f.serviceTier, "service-tier", "", "runtime-native service tier")
	fs.StringVar(&f.resume, "resume", "", "reopen a native conversation")
	fs.Var(&f.images, "image", "attach an image (repeatable)")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Minute, "turn deadline (0 disables it)")
	fs.BoolVar(&f.json, "json", false, "emit one stable JSON DTO")
	fs.StringVar(&f.record, "record", "", "write a voyage log after the run (never overwrites)")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 2 {
		fmt.Fprintln(a.stderr(), "usage: oarlock run <runtime> <prompt> [flags]")
		return ExitUsage
	}
	if f.timeout < 0 {
		fmt.Fprintln(a.stderr(), "oarlock run: --timeout must not be negative (0 disables the deadline)")
		return ExitUsage
	}

	inv := &runInvocation{
		app:       a,
		json:      f.json,
		runtimeID: positional[0],
		prompt:    positional[1],
	}
	if err := inv.prepare(&f); err != nil {
		return inv.fail(err)
	}
	return inv.execute()
}

// runInvocation is one run, after flags are parsed but before the session
// opens. Everything checked here is a pre-run error: no session was started.
type runInvocation struct {
	app *App
	// json selects the one-document output; it also turns progress off.
	json      bool
	runtimeID string
	prompt    string

	cwd         string
	model       string
	effort      string
	serviceTier string
	resume      string
	images      []oarlock.InputImage
	timeout     time.Duration
	recordPath  string
	startedAt   time.Time

	rt   oarlock.Runtime
	inst oarlock.Installation
	opts oarlock.Options
}

// prepare validates every input and opens nothing. --record is checked here so
// an existing file fails before a runtime process is started.
func (inv *runInvocation) prepare(f *runFlags) error {
	cwd, err := absoluteDir(f.cwd)
	if err != nil {
		return err
	}
	inv.cwd = cwd
	inv.model = f.model
	inv.effort = f.effort
	inv.serviceTier = f.serviceTier
	inv.resume = f.resume
	inv.timeout = f.timeout
	inv.images, err = absoluteImages(f.images)
	if err != nil {
		return err
	}
	if f.record != "" {
		path, err := filepath.Abs(f.record)
		if err != nil {
			return fmt.Errorf("resolving --record %q: %w", f.record, err)
		}
		if err := checkRecordPath(path); err != nil {
			return err
		}
		inv.recordPath = path
	}

	rt, err := inv.app.lookup(inv.runtimeID)
	if err != nil {
		return err
	}
	inv.rt = rt
	inv.runtimeID = rt.ID
	inv.opts = oarlock.Options{
		Cwd:         cwd,
		Model:       f.model,
		Effort:      f.effort,
		ServiceTier: f.serviceTier,
		Resume:      f.resume,
	}
	// Refuse a declared-and-present option before a process starts. StartSession
	// checks the same table; doing it here turns the refusal into a pre-run
	// error with the CLI's own exit code.
	if err := rt.CheckOptions(inv.opts); err != nil {
		return err
	}
	if rt.Probe != nil {
		inst, err := rt.Installation(context.Background())
		if err != nil {
			return fmt.Errorf("probing %s: %w", inv.runtimeID, err)
		}
		if !inst.Available {
			return fmt.Errorf("%s is not installed (the probe found none)", inv.runtimeID)
		}
		inv.inst = inst
	}
	return nil
}

// fail reports a pre-run error. In JSON mode it is still one DTO, on stdout,
// with error set; the human path keeps stderr for diagnostics.
func (inv *runInvocation) fail(err error) int {
	if inv.json {
		dto := RunDTO{
			Schema:      Schema,
			Command:     "run",
			Runtime:     inv.runtimeID,
			Model:       inv.model,
			Effort:      inv.effort,
			ServiceTier: inv.serviceTier,
			Error:       err.Error(),
		}
		_ = inv.app.writeJSON(dto)
		return ExitError
	}
	fmt.Fprintf(inv.app.stderr(), "oarlock: run: %v\n", err)
	return ExitError
}

// execute opens the session, prompts, and reports one result. The signal guard
// is the same for every phase after prepare: the first SIGINT cancels the ctx
// (PromptAndWait aborts), the second disposes and exits 130 immediately.
func (inv *runInvocation) execute() int {
	inv.startedAt = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, stopSignals := inv.app.interruptChannel()
	guard := newSignalGuard(ch, stopSignals)
	defer guard.close()
	go func() {
		select {
		case <-guard.first:
			cancel()
		case <-guard.done:
		}
	}()

	sess, err := inv.rt.Session(ctx, inv.inst, inv.opts)
	if err != nil {
		if guard.interrupted() {
			return inv.failInterrupted(fmt.Errorf("opening %s: %w", inv.runtimeID, err))
		}
		return inv.fail(fmt.Errorf("opening %s: %w", inv.runtimeID, err))
	}

	var disposeOnce sync.Once
	dispose := func(disposeCtx context.Context) {
		disposeOnce.Do(func() { _ = sess.Dispose(disposeCtx) })
	}
	// The first SIGINT cancelled ctx; dispose must still be allowed to run so
	// the stream gets its dispose record.
	defer dispose(context.WithoutCancel(ctx))

	prog := newProgress(inv.app.stdout())
	stopEvents := func() {}
	if !inv.json {
		inv.printHeader(sess)
		var cursor oarlock.Cursor
		stop := sess.Events(&cursor, prog.record)
		if stop != nil {
			var once sync.Once
			stopEvents = func() { once.Do(stop) }
		}
	}
	defer stopEvents()

	waitOpts := []oarlock.WaitOption{
		oarlock.WithPromptOptions(oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: inv.images},
		}),
	}
	if inv.timeout > 0 {
		waitOpts = append(waitOpts, oarlock.WithTimeout(inv.timeout))
	}

	type waitResult struct {
		res oarlock.RunResult
		err error
	}
	finished := make(chan waitResult, 1)
	go func() {
		res, err := oarlock.PromptAndWait(ctx, sess, inv.prompt, waitOpts...)
		finished <- waitResult{res: res, err: err}
	}()

	var got waitResult
	forced := false
	// A completion already in hand wins over the signal edge, the same rule the
	// first SIGINT path applies. Only block when neither has happened yet.
	select {
	case got = <-finished:
	default:
		select {
		case got = <-finished:
		case <-guard.second:
			forced = true
		}
	}

	// The DTO answers "what does the session say while the turn is over and
	// dispose has not added its exit yet"; the record wants the whole stream.
	beforeDispose := sess.Records(oarlock.Cursor{})
	stopEvents()
	if forced {
		// The second SIGINT promises not to wait: bound the dispose instead of
		// letting a slow adapter's own exit grace hold the shell.
		disposeCtx, cancelDispose := context.WithTimeout(context.Background(), forcedDisposeBound)
		dispose(disposeCtx)
		cancelDispose()
		return inv.forcedInterrupt()
	}
	dispose(context.WithoutCancel(ctx))
	all := sess.Records(oarlock.Cursor{})
	return inv.finish(got.res, got.err, guard, prog, beforeDispose, all)
}

// printHeader names the runtime and the directory before the first event. The
// model and effort come from the stream when the runtime already reported them
// (or the requested flag otherwise), so the header never guesses.
func (inv *runInvocation) printHeader(sess oarlock.Session) {
	model, effort := inv.model, inv.effort
	if inst := sess.Records(oarlock.Cursor{}); len(inst) != 0 {
		if reported := observe.ModelOf(inst).Value; reported != "" {
			model = reported
		}
		if reported := observe.EffortOf(inst).Value; reported != "" {
			effort = reported
		}
	}
	fmt.Fprintf(inv.app.stdout(), "[runtime %s] cwd=%s", inv.runtimeID, inv.cwd)
	if model != "" {
		fmt.Fprintf(inv.app.stdout(), " model=%s", model)
	}
	if effort != "" {
		fmt.Fprintf(inv.app.stdout(), " effort=%s", effort)
	}
	fmt.Fprintln(inv.app.stdout())
}

// finish folds the result into the DTO, writes the optional voyage log and
// renders one final answer. The record is evidence: a write failure is a
// command failure even when the turn itself completed.
func (inv *runInvocation) finish(res oarlock.RunResult, runErr error, guard *signalGuard, prog *progress, beforeDispose, all []oarlock.Record) int {
	exit := runExit(res, runErr, guard)
	dto := runDTO(inv.runtimeID, beforeDispose, res, inv.model, inv.effort, inv.serviceTier)
	// A completion that raced the first SIGINT is still a completion: the
	// caller's cancelled context is not a run failure in that case.
	if runErr != nil && res.Outcome != oarlock.RunCompleted {
		dto.Error = runErr.Error()
	}

	if inv.recordPath != "" {
		reason := string(res.Outcome)
		if reason == "" {
			reason = "error"
		}
		if err := inv.writeRecord(all, dto.SessionID, reason); err != nil {
			dto.Error = "writing record: " + err.Error()
			if exit != ExitInterrupt {
				exit = ExitError
			}
		}
	}

	if inv.json {
		if code := inv.app.writeJSON(dto); code != ExitOK && exit == ExitOK {
			exit = code
		}
		return exit
	}
	prog.summary(dto)
	return exit
}

// failInterrupted reports an open that the first SIGINT cancelled. It is the
// same 130 as a turn interrupted after opening.
func (inv *runInvocation) failInterrupted(err error) int {
	if inv.json {
		_ = inv.app.writeJSON(RunDTO{
			Schema:  Schema,
			Command: "run",
			Runtime: inv.runtimeID,
			Outcome: string(oarlock.RunInterrupted),
			Error:   err.Error(),
		})
		return ExitInterrupt
	}
	fmt.Fprintf(inv.app.stderr(), "oarlock: run: interrupted: %v\n", err)
	return ExitInterrupt
}

// forcedInterrupt is the second SIGINT path: the session was already disposed
// and the command leaves without waiting for the runtime, so there is no
// complete voyage to write.
func (inv *runInvocation) forcedInterrupt() int {
	if inv.json {
		_ = inv.app.writeJSON(RunDTO{
			Schema:  Schema,
			Command: "run",
			Runtime: inv.runtimeID,
			Outcome: string(oarlock.RunInterrupted),
			Error:   "interrupted by second SIGINT",
		})
		return ExitInterrupt
	}
	fmt.Fprintln(inv.app.stdout(), "interrupted: second SIGINT; session disposed")
	return ExitInterrupt
}

// runExit maps the run result to the CLI's exit codes. A real completion wins
// over a signal that raced it; the first SIGINT's interrupt is 130, a timeout
// is 1 (the same outcome with a different cause).
func runExit(res oarlock.RunResult, err error, guard *signalGuard) int {
	if res.Outcome == oarlock.RunCompleted {
		// The runtime's own completion wins over a signal that raced it.
		return ExitOK
	}
	if guard.interrupted() {
		return ExitInterrupt
	}
	return ExitError
}

// absoluteDir resolves --cwd (empty means the process cwd) and checks that it
// is an existing directory.
func absoluteDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("reading the current directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving --cwd %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--cwd %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--cwd %s is not a directory", abs)
	}
	return abs, nil
}

// absoluteImages resolves every --image against the process cwd. The runtime's
// cwd (--cwd) is a different thing: a shell argument is relative to the shell.
func absoluteImages(paths []string) ([]oarlock.InputImage, error) {
	out := make([]oarlock.InputImage, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			return nil, errors.New("--image path is empty")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolving --image %q: %w", p, err)
		}
		out = append(out, oarlock.InputImage{Path: abs})
	}
	return out, nil
}
