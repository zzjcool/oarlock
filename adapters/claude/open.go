package claude

// Open: oarlock.StartSession for Claude (issue #19). It checks the options
// before anything starts, launches `claude -p --input-format stream-json ...`
// through internal/exec (own process group), wraps the Conn in a Session and,
// on resume, confirms claude really opened the conversation.
//
// Resume (`--resume <id>`): a resumed Session is a new stream instance, so a new
// kernel and Seq starts at 1, with the same native session id. Nothing is read
// back from claude's history and no record is replayed. A missing target is
// *oarlock.SessionNotFound, and only on positive evidence: claude's result
// error_during_execution whose first error says "No conversation found with
// session ID" (pinned by OAR's claude-missing-resume fixture; prose is the last
// resort because claude gives no code). Anything else that goes wrong while
// opening is a plain error: absence was not shown.
//
// A resumed process continues the previous process's running token total, so
// before the first turn the adapter asks get_usage and counts from that
// baseline; a baseline that cannot be read leaves usage unreported rather than
// guessed (usage.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// OpenReadbackTimeout bounds each open-time readback (OAR: 30 s).
const OpenReadbackTimeout = 30 * time.Second

const missingResumePrefix = "No conversation found with session ID"

// openSpec is how open starts the child. Open fills it from the Installation;
// tests point Launch.Command at a fake claude.
type openSpec struct {
	launch  sj.Options
	session SessionOptions // AbortGrace / EchoTimeout; SessionID, Redact, Resumed are set by open

	readbackTimeout time.Duration // 0 = OpenReadbackTimeout

	// Test seams: arguments placed before oarlock's own (a fake claude is the
	// test binary re-executed) and extra environment. Production leaves them nil.
	extraPrefix []string
	extraEnv    []string
}

// Open starts a Claude stream-json session and implements oarlock.StartSession.
// ctx bounds the opening handshake only: the process tree lives until Dispose.
func Open(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	cmd := inst.Path
	if cmd == "" {
		cmd = "claude"
	}
	s, err := open(ctx, openSpec{launch: sj.Options{Command: cmd}}, opt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

var _ oarlock.StartSession = Open

func open(ctx context.Context, spec openSpec, opt oarlock.Options) (*Session, error) {
	if ctx == nil {
		return nil, errors.New("claude: Open(nil context)")
	}
	// The same table Refused() declares: nothing has started.
	if err := refusedTable.Check(opt); err != nil {
		return nil, err
	}
	if err := checkLaunchArgs(opt.LaunchArgs); err != nil {
		return nil, err
	}
	if opt.Resume != "" {
		for _, p := range []struct {
			o oarlock.Option
			v string
		}{{oarlock.OptionSystemPrompt, opt.SystemPrompt}, {oarlock.OptionAppendSystemPrompt, opt.AppendSystemPrompt}} {
			if p.v != "" {
				return nil, &oarlock.UnsupportedOption{Option: p.o, Reason: fmt.Sprintf("Claude ignores %s when resuming; omit it to keep the saved prompts, or start a new session to change them", p.o)}
			}
		}
	}
	cwd := opt.Cwd
	sessionID := opt.Resume
	if sessionID == "" {
		sessionID = newUUID()
	}

	lo := spec.launch
	lo.Args = append(append([]string(nil), spec.extraPrefix...), launchArgs(opt, sessionID)...)
	lo.Dir = cwd
	lo.Env = append(sessionEnv(os.Environ(), opt.Env), spec.extraEnv...)
	if lo.EventBuffer == 0 {
		lo.EventBuffer = 4096
	}
	p, err := sj.Launch(context.WithoutCancel(ctx), lo)
	if err != nil {
		return nil, fmt.Errorf("claude: launch: %w", err)
	}

	so := spec.session
	so.SessionID = sessionID
	so.Redact = redactor(opt.Env)
	so.Resumed = opt.Resume != ""
	sess, err := NewSession(p, so)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	if opt.Resume != "" {
		timeout := spec.readbackTimeout
		if timeout == 0 {
			timeout = OpenReadbackTimeout
		}
		if err := sess.confirmResume(ctx, opt.Resume, timeout); err != nil {
			_ = sess.Dispose(context.Background())
			return nil, err
		}
	}
	return sess, nil
}

// confirmResume asks claude to initialize and reads its answer. A missing
// conversation reports a result error and exits without an initialize answer, so
// the startup failure is watched for at the same time. Then the usage baseline.
func (s *Session) confirmResume(ctx context.Context, id string, timeout time.Duration) error {
	failed := make(chan sj.Rec, 1)
	s.mu.Lock()
	s.opening = failed
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.opening = nil
		s.mu.Unlock()
	}()

	type answer struct {
		rec sj.Rec
		err error
	}
	got := make(chan answer, 1)
	go func() {
		rec, err := s.request(ctx, s.nextPrivateID("resume"), map[string]any{"subtype": "initialize"}, timeout)
		got <- answer{rec, err}
	}()
	select {
	case rec := <-failed:
		return resumeFailure(id, rec)
	case a := <-got:
		// A startup failure can race the answer: look once more.
		select {
		case rec := <-failed:
			return resumeFailure(id, rec)
		default:
		}
		if a.err != nil {
			return fmt.Errorf("claude: %v, so resume %s cannot be confirmed", a.err, id)
		}
		if resp := sj.AsRec(a.rec["response"]); sj.Str(resp["subtype"]) != "success" {
			reason := sj.Str(resp["error"])
			if reason == "" {
				reason = "no success answer"
			}
			return fmt.Errorf("claude could not resume %s (initialize: %s)", id, reason)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	// The baseline never fails the open: unknown means usage stays unreported.
	rec, err := s.request(ctx, s.nextPrivateID("usage"), map[string]any{"subtype": "get_usage", "skip_behaviors": true}, timeout)
	var models map[string]oarlock.TokenTotals
	known := false
	if err == nil {
		models, known = usageBaseline(rec)
	}
	s.fold.Do(func() { s.prof.usage.withBaseline(models, known) })
	return nil
}

func resumeFailure(id string, result sj.Rec) error {
	var first string
	if errs := sj.AsList(result["errors"]); len(errs) > 0 {
		first, _ = sj.AsStr(errs[0])
	}
	if strings.HasPrefix(first, missingResumePrefix) {
		return &oarlock.SessionNotFound{SessionID: id, Cause: errors.New(first)}
	}
	if first == "" {
		first = "claude initialization failed (result/error_during_execution)"
	}
	return fmt.Errorf("claude could not resume %s: %s", id, first)
}

// refusedTable is the ONE table: Refused() hands a copy to a host and open
// checks this very table, so declaration and behaviour cannot drift. An option
// added to oarlock.AllRefusableOptions later is refused until it is decided.
var refusedTable = buildRefused()

func buildRefused() oarlock.RefusedOptions {
	t := make(oarlock.RefusedOptions, len(oarlock.AllRefusableOptions))
	for _, o := range oarlock.AllRefusableOptions {
		if reason, ok := refusedReasons[o]; ok {
			t[o] = reason
		} else if !honoured[o] {
			t[o] = "the Claude adapter has not decided this option yet; it is refused rather than dropped"
		}
	}
	return t
}

var honoured = map[oarlock.Option]bool{
	oarlock.OptionSystemPrompt: true, oarlock.OptionAppendSystemPrompt: true, oarlock.OptionEnv: true,
	oarlock.OptionDisallowedTools: true, oarlock.OptionLaunchArgs: true, oarlock.OptionAdditionalDirectories: true,
	oarlock.OptionOutputSchema: true, oarlock.OptionName: true,
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
