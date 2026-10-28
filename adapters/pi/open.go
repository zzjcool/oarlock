package pi

// Open: the minimal oarlock.StartSession for Pi (issue #12). It launches
// `pi --mode rpc`, reads get_state and wraps the Conn in the issue #10 Session.
// It exists to carry Options.Resume; the version gate and the full refused-option
// table are issue #15's, and model/effort readback is #13's.
//
// Resume semantics (docs/runtimes/pi.md §12):
//
//   - Pi resumes a session FILE, never an id. ResolveResume turns Options.Resume
//     (a Pi session id or a session file) into an absolute file BEFORE any process
//     starts, so "not found" and "recorded under another cwd" leave nothing
//     behind. The child is then told `--session <file>`.
//   - Pi opens a missing `--session` path as a brand-new session without
//     complaint, so the id Pi reports in get_state must equal the id that was
//     resolved. A different id is a plain error (the file changed under us, or Pi
//     did not open it), never SessionNotFound: absence was not shown.
//   - A resumed Session is a new stream instance: a new kernel, Seq starts at 1,
//     the same native session id. The conversation is restored inside Pi; nothing
//     is read back from the file and no record is replayed. The frames Pi writes
//     while starting are ordinary frames; resume synthesizes no turn_active,
//     turn_ended or user_message (record-stream.md invariant 1).
//   - A previous instance that died mid-turn leaves its user message in the file
//     but no running turn in Pi: the fresh stream says idle and the first Prompt
//     is taken over like any other (measured, §12).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
)

// handshakeEvents is the Events capacity of a session opened here: the room for
// whatever Pi and its host extensions write before get_state is answered.
const handshakeEvents = 4096

// openSpec is how open starts the child. Open fills it from the Installation;
// tests point launch.Command at a fake Pi and use prefix / extra to reach it.
type openSpec struct {
	launch Options  // Command and the numeric knobs; Args, Dir and Env are set by open
	prefix []string // arguments before oarlock's own
	extra  []string // arguments after oarlock's own

	versionTimeout time.Duration // the --version probe's bound; 0 = VersionTimeout (15s)
}

// Open starts a Pi RPC session and implements oarlock.StartSession. It honours
// Cwd, Env, Model, Effort and Resume; every other option is refused with
// *oarlock.UnsupportedOption (the table in gate.go, the same one Refused()
// declares) before anything starts, and a Pi that is not SupportedVersion is a
// *VersionError before the session process starts (never silently dropped, never
// a warning).
// Model and Effort are passed to Pi as --model and --thinking and are NOT read
// back (issue #13). ctx bounds the handshake only: the process tree lives until
// Dispose, so cancelling an open-time timeout later does not kill the session.
func Open(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	s, err := open(ctx, openSpec{launch: Options{Command: inst.Path}}, opt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

var _ oarlock.StartSession = Open

func open(ctx context.Context, spec openSpec, opt oarlock.Options) (*Session, error) {
	if ctx == nil {
		return nil, errors.New("pi: Open(nil context)")
	}
	// The same table Refused() declares: nothing has started, not even the probe.
	if err := refusedOptions.Check(opt); err != nil {
		return nil, err
	}

	env := EnvFor(os.Environ(), opt.Env)
	agentDir, err := AgentDir(env)
	if err != nil {
		return nil, fmt.Errorf("pi: %w", err)
	}
	cwd := opt.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("pi: cannot determine the working directory: %w", err)
		}
	}
	cwd = absClean(cwd)

	// Everything that can say "no" happens before a process exists.
	target, err := ResolveResume(opt.Resume, cwd, agentDir)
	if err != nil {
		return nil, err
	}

	// The version gate is the first process this open starts, and it runs after
	// every check that needs none: a refused option or a missing resume target
	// never costs a probe. The probe sees the session's cwd and environment, so it
	// asks the very Pi (and agent dir) the session would run. Exact match or a
	// typed error: there is no warn-and-run.
	version, err := CheckVersion(ctx, VersionCheck{
		Command:   spec.launch.Command,
		Args:      spec.prefix,
		Dir:       cwd,
		Env:       EnvFor(env, map[string]*string{envAgentDir: &agentDir}),
		Timeout:   spec.versionTimeout,
		KillGrace: spec.launch.KillGrace,
		WaitDelay: spec.launch.WaitDelay,
	})
	if err != nil {
		return nil, err
	}

	args := append([]string(nil), spec.prefix...)
	args = append(args, "--mode", "rpc", "--session-dir", SessionDir(cwd, agentDir))
	if target.File != "" {
		// Always the absolute file, never the id: a bare id from another cwd
		// makes Pi wait for a fork confirmation on stdin.
		args = append(args, "--session", target.File)
	}
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	if opt.Effort != "" {
		args = append(args, "--thinking", opt.Effort)
	}
	args = append(args, spec.extra...)

	lo := spec.launch
	lo.Args = args
	lo.Dir = cwd
	lo.Env = EnvFor(env, map[string]*string{envAgentDir: &agentDir})
	if lo.EventBuffer == 0 {
		lo.EventBuffer = handshakeEvents
	}
	// The process lives until Dispose, not until ctx ends: a host that bounds
	// the open with a timeout must not lose the session when it cancels that
	// timeout. ctx still bounds the handshake below.
	p, err := Launch(context.WithoutCancel(ctx), lo)
	if err != nil {
		return nil, fmt.Errorf("pi: launch: %w", err)
	}

	// Nothing reads Events until NewSession (it needs the id get_state returns),
	// so every frame Pi writes before the reply must fit the Events buffer or the
	// reader would never reach the reply. Pi wrote 3 in the measured runs.
	// The frames stay buffered and become the first records of the stream.
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if err := p.Do(ctx, Command{Type: "get_state"}, &state); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("pi: get_state after launch: %w", err)
	}
	switch {
	case state.SessionID == "":
		_ = p.Close()
		return nil, errors.New("pi: get_state reported no sessionId")
	case target.SessionID != "" && state.SessionID != target.SessionID:
		// Pi opens a --session path that does not exist as a new session.
		_ = p.Close()
		return nil, fmt.Errorf("pi: asked to resume session %q (%s) but Pi opened session %q", target.SessionID, target.File, state.SessionID)
	}

	sess, err := NewSession(p, SessionOptions{SessionID: state.SessionID, Redact: redactor(opt.Env), PiVersion: version})
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return sess, nil
}

// redactor builds the kernel hook for the session's own credential values
// (docs/spec/redaction.md §4): every Options.Env value is a candidate, and the
// value rule keeps only the ones that can be told apart from ordinary text.
func redactor(env map[string]*string) kernel.Redactor {
	var known []string
	for _, v := range env {
		if v != nil {
			known = append(known, *v)
		}
	}
	return redact.New(known).Record
}
