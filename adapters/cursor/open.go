package cursor

// open.go — oarlock.StartSession for Cursor, issue #22.
//
// Open refuses the options this adapter cannot honour before anything starts
// (CheckOptions), then builds the Session. It starts no process: cursor-agent
// is one-shot, so the first process belongs to the first prompt (session.go).
//
// A resumed chat (`Options.Resume <chat id>`) is a new stream instance, so a
// new kernel and Seq starts at 1. Nothing is read back from Cursor's history
// and no record is replayed: the chat id is what `--resume` takes, and the CLI
// reports it again in `system/init` of the first turn.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// openSpec is how open starts the child. Open fills it from the Installation;
// tests point launch.Command at the test binary re-executed as a fake
// cursor-agent.
type openSpec struct {
	launch  sj.Options
	session SessionOptions

	// Test seams: arguments placed before oarlock's own (a fake cursor-agent is
	// the test binary re-executed) and extra environment. Production leaves
	// them nil.
	extraPrefix []string
	extraEnv    []string
}

// Open starts a Cursor session and implements oarlock.StartSession. It is
// cheap: nothing runs until the first prompt.
func Open(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	cmd := inst.Path
	if cmd == "" {
		cmd = "cursor-agent"
	}
	s, err := open(ctx, openSpec{launch: sj.Options{Command: cmd}}, opt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

var _ oarlock.StartSession = Open

// Runtime is the adoption unit this package declares: a session, and the
// refused table Open checks.
func Runtime() oarlock.Runtime {
	return oarlock.Runtime{ID: "cursor", Session: Open, Refused: Refused()}
}

func open(ctx context.Context, spec openSpec, opt oarlock.Options) (*Session, error) {
	if ctx == nil {
		return nil, errors.New("cursor: Open(nil context)")
	}
	// The same table Refused() declares: nothing has started.
	if err := CheckOptions(opt); err != nil {
		return nil, err
	}
	if spec.session.AbortGrace < 0 {
		return nil, fmt.Errorf("cursor: SessionOptions.AbortGrace is negative (%v)", spec.session.AbortGrace)
	}
	if spec.launch.Command == "" {
		return nil, errors.New("cursor: openSpec.launch.Command is required")
	}

	// The kernel's own session id is the chat the host asked to continue; a
	// fresh run has none until system/init reports one (session.go learns it).
	chatID := spec.session.ChatID
	if opt.Resume != "" {
		chatID = opt.Resume
	}
	red := spec.session.Redact
	if red == nil {
		red = redactor(opt.Env)
	}

	dir := opt.Cwd
	if dir == "" {
		dir = spec.launch.Dir
	}
	cfg := sessionConfig{
		command: spec.launch.Command,
		args: func(chat string) []string {
			return append(append([]string(nil), spec.extraPrefix...), LaunchArgs(opt, chat)...)
		},
		dir:        dir,
		env:        append(sessionEnv(os.Environ(), opt.Env), spec.extraEnv...),
		chatID:     chatID,
		redact:     red,
		abortGrace: spec.session.AbortGrace,
	}
	return newSession(cfg), nil
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
