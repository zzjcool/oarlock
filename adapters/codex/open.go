package codex

// Open: oarlock.StartSession for Codex (issue #20). It launches
// `codex app-server --listen stdio://`, performs the initialize/initialized
// handshake, opens a thread (thread/start or thread/resume) and wraps the Conn
// in the Session.
//
// Everything that can say "no" happens before a process exists: a refused
// option (the table in gate.go) and a codex outside the version window
// (`codex --version`). After the process starts, the initialize reply's
// userAgent is checked against the same window and the thread reply against the
// schema the projection reads; either failure kills the process and fails the
// open, so a host never gets a session that runs on a dialect we do not read.
//
// Resume: Options.Resume is a thread id. A thread that has no rollout is a
// *oarlock.SessionNotFound (positive evidence: codex's own -32600 "no rollout
// found for thread id"); every other refusal (archived, active writer) is a
// plain error that names the method. The resumed Session is a new stream
// instance (Seq restarts at 1) and token totals count from codex's re-report.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
)

// ClientName / ClientVersion identify oarlock in the initialize handshake.
const (
	ClientName    = "oarlock"
	ClientVersion = "0.0.0"
)

// Config is what Options does not carry.
type Config struct {
	// ApprovalPolicy is codex's approvalPolicy for the thread. Default "never":
	// no approval is expected and a dangling toApp request is the honest record
	// when one arrives anyway. Set "on-request" (or any policy codex knows) to
	// receive approvals as toApp requests the host answers with Session.Answer.
	ApprovalPolicy string
	// Sandbox, when non-empty, is passed as `-c sandbox_mode="<Sandbox>"`.
	// Empty keeps the user's own codex configuration.
	Sandbox string
	// CallTimeout bounds one RPC; 0 = DefaultCallTimeout.
	CallTimeout time.Duration
	// AbortGrace: see SessionOptions.
	AbortGrace time.Duration
	// Prefix: extra arguments before `app-server` (a launcher's own),
	// used by tests to re-execute the test binary as a fake app-server.
	Prefix []string
	// VersionArgs are the arguments before --version of the probe; nil = Prefix.
	VersionArgs []string
	// VersionTimeout bounds the probe; 0 = VersionTimeout.
	VersionTimeout time.Duration
	// Command overrides inst.Path (tests).
	Command string
}

// Open starts a Codex app-server session and implements oarlock.StartSession.
func Open(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	s, err := open(ctx, inst, opt, Config{})
	if err != nil {
		return nil, err
	}
	return s, nil
}

var _ oarlock.StartSession = Open

// OpenWith is Open with a Config.
func OpenWith(ctx context.Context, inst oarlock.Installation, opt oarlock.Options, cfg Config) (*Session, error) {
	return open(ctx, inst, opt, cfg)
}

// Runtime is the adoption unit: the session opener and the same refusal table
// the open path checks.
func Runtime() oarlock.Runtime {
	return oarlock.Runtime{ID: "codex", Session: Open, Refused: Refused()}
}

func open(ctx context.Context, inst oarlock.Installation, opt oarlock.Options, cfg Config) (*Session, error) {
	if ctx == nil {
		return nil, errors.New("codex: Open(nil context)")
	}
	if err := refusedOptions.Check(opt); err != nil {
		return nil, err
	}
	if opt.Resume != "" && opt.AppendSystemPrompt != "" {
		return nil, &oarlock.UnsupportedOption{Option: oarlock.OptionAppendSystemPrompt,
			Reason: "codex ignores appendSystemPrompt when resuming; omit it on resume to keep the saved developer instructions, or create a new session to change them"}
	}
	command := cfg.Command
	if command == "" {
		command = inst.Path
	}
	if command == "" {
		command = "codex"
	}
	cwd := opt.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("codex: cannot determine the working directory: %w", err)
		}
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	env := envFor(os.Environ(), opt.Env)

	version, err := CheckVersion(ctx, VersionCheck{
		Command: command, Args: pick(cfg.VersionArgs, cfg.Prefix), Dir: cwd, Env: env, Timeout: cfg.VersionTimeout,
	})
	if err != nil {
		return nil, err
	}

	method, params := threadOpen(opt, cwd, cfg)
	args := append([]string(nil), cfg.Prefix...)
	args = append(args, "app-server")
	if cfg.Sandbox != "" {
		args = append(args, "-c", `sandbox_mode="`+cfg.Sandbox+`"`)
	}
	args = append(args, opt.LaunchArgs...)
	args = append(args, "--listen", "stdio://")

	// The process lives until Dispose, not until ctx ends: a host that bounds
	// the open with a timeout must not lose the session when it cancels that
	// timeout. ctx still bounds the handshake below.
	p, err := Launch(context.WithoutCancel(ctx), launchOptions(command, args, cwd, env, cfg))
	if err != nil {
		return nil, fmt.Errorf("codex: launch: %w", err)
	}
	fail := func(err error) (*Session, error) {
		_ = p.Close()
		return nil, err
	}

	init, err := p.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": ClientName, "version": ClientVersion},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	if err != nil {
		return fail(fmt.Errorf("codex: initialize: %w", err))
	}
	if ua, ok := parseObj(init.Result).str("userAgent"); ok && ua != "" && !InWindow(ua) {
		return fail(&VersionError{Command: command, Min: MinVersion, Max: MaxVersionExclusive, Actual: clipVersion(ua)})
	}
	if err := p.Notify("initialized", map[string]any{}); err != nil {
		return fail(fmt.Errorf("codex: initialized: %w", err))
	}

	reply, err := p.Call(ctx, method, params, nil)
	if err != nil {
		var rpc *RPCError
		if errors.As(err, &rpc) && method == "thread/resume" && rpc.Code == -32600 && strings.HasPrefix(rpc.Message, "no rollout found for thread id") {
			return fail(&oarlock.SessionNotFound{SessionID: opt.Resume, Cause: err})
		}
		return fail(fmt.Errorf("codex: %s failed: %w", method, err))
	}
	res := parseObj(reply.Result)
	threadID, _ := res.obj("thread").str("id")
	if threadID == "" {
		return fail(&SchemaError{Method: method, Detail: "no thread.id"})
	}
	events, refusal := openReadback(method, opt, cwd, res)
	if refusal != "" {
		return fail(errors.New(refusal))
	}
	if name := threadName(method, opt, res); name != "" {
		if _, err := p.Call(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": name}, nil); err != nil {
			return fail(fmt.Errorf("codex: thread/name/set failed: %w", err))
		}
	}

	sess, err := NewSession(p, SessionOptions{
		ThreadID: threadID, Resumed: method == "thread/resume", OpenEvents: events,
		OutputSchema: opt.OutputSchema, AbortGrace: cfg.AbortGrace, Redact: redactor(opt.Env), CodexVersion: version,
	})
	if err != nil {
		return fail(err)
	}
	return sess, nil
}

// launchOptions builds the transport options (kept apart so tests can see them).
func launchOptions(command string, args []string, dir string, env []string, cfg Config) Options {
	return Options{Command: command, Args: args, Dir: dir, Env: env, CallTimeout: cfg.CallTimeout}
}

func pick(a, b []string) []string {
	if a != nil {
		return a
	}
	return b
}

// threadOpen builds the thread/start or thread/resume request from the options.
func threadOpen(opt oarlock.Options, cwd string, cfg Config) (string, map[string]any) {
	policy := cfg.ApprovalPolicy
	if policy == "" {
		policy = "never"
	}
	p := map[string]any{"cwd": cwd, "approvalPolicy": policy}
	if roots := workspaceRoots(opt, cwd); roots != nil {
		p["runtimeWorkspaceRoots"] = roots
	}
	if opt.Model != "" {
		p["model"] = opt.Model
	}
	if opt.ServiceTier != "" {
		p["serviceTier"] = opt.ServiceTier
	}
	// baseInstructions REPLACES codex's base prompt; developerInstructions
	// APPENDS as a developer message ("instructions" is silently ignored).
	if opt.SystemPrompt != "" {
		p["baseInstructions"] = opt.SystemPrompt
	}
	if opt.AppendSystemPrompt != "" {
		p["developerInstructions"] = opt.AppendSystemPrompt
	}
	if opt.Resume == "" {
		// Required in addition to initialize.experimentalApi: exposes the
		// completed Responses API reasoning item.
		p["experimentalRawEvents"] = true
		if opt.Effort != "" {
			// A config override on thread/start reaches every turn's request,
			// queued turns included, and is persisted with the thread. On a resume
			// any override rebuilds the thread's settings from config.toml, so a
			// resume never carries one.
			p["config"] = map[string]any{"model_reasoning_effort": opt.Effort}
		}
		return "thread/start", p
	}
	p["threadId"] = opt.Resume
	p["excludeTurns"] = true
	return "thread/resume", p
}

// workspaceRoots: the native list replaces, so the primary cwd is retained.
func workspaceRoots(opt oarlock.Options, cwd string) []string {
	if len(opt.AdditionalDirectories) == 0 {
		return nil
	}
	seen := map[string]bool{cwd: true}
	out := []string{cwd}
	for _, d := range opt.AdditionalDirectories {
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func effortIn(o obj) string {
	if e, ok := o.str("reasoningEffort"); ok && e != "" {
		return e
	}
	e, _ := o.str("effort")
	return e
}

// openReadback reads the thread reply back against the request: the model,
// effort and service tier codex says the thread runs become the open frame's
// events; anything but what was requested refuses the open (a caller who asked
// for a model must not get one silently running another).
func openReadback(method string, opt oarlock.Options, cwd string, res obj) ([]oarlock.Event, string) {
	var events []oarlock.Event
	model, hasModel := res.str("model")
	if hasModel && model != "" {
		events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: model})
	}
	effort := effortIn(res)
	if effort != "" {
		events = append(events, oarlock.Event{Kind: oarlock.EventEffort, Effort: effort})
	}
	events = append(events, tierEvents(res)...)

	if roots := workspaceRoots(opt, cwd); roots != nil {
		reported := map[string]bool{}
		for _, r := range res.arr("runtimeWorkspaceRoots") {
			if s, ok := r.(string); ok {
				reported[s] = true
			}
		}
		for _, r := range roots {
			if !reported[r] {
				return events, fmt.Sprintf("codex %s did not confirm the requested additionalDirectories in runtimeWorkspaceRoots", method)
			}
		}
	}
	if opt.Model != "" && hasModel && model != "" && model != opt.Model {
		return events, fmt.Sprintf("codex %s kept model %s although %s was requested", method, model, opt.Model)
	}
	if opt.ServiceTier != "" {
		if got, _ := serviceTier(res); got != opt.ServiceTier {
			if got == "" {
				got = "unreported"
			}
			return events, fmt.Sprintf("codex %s reports serviceTier %s although %s was requested", method, got, opt.ServiceTier)
		}
	}
	if opt.Effort != "" && effort != opt.Effort {
		if method == "thread/resume" {
			return events, fmt.Sprintf("codex thread/resume runs effort %q although %q was requested; setting effort on a resumed thread (thread/settings/update) is not implemented", effort, opt.Effort)
		}
		return events, fmt.Sprintf("codex %s reports effort %q although %q was requested", method, effort, opt.Effort)
	}
	return events, ""
}

// threadName: a new thread gets the name; a resumed one only when it shows none,
// so a name the user gave in their app is kept.
func threadName(method string, opt oarlock.Options, res obj) string {
	if opt.Name == "" {
		return ""
	}
	if method == "thread/resume" {
		if cur, _ := res.obj("thread").str("name"); cur != "" {
			return ""
		}
	}
	return opt.Name
}

// redactor builds the kernel hook for the session's own credential values.
func redactor(env map[string]*string) kernel.Redactor {
	var known []string
	for _, v := range env {
		if v != nil {
			known = append(known, *v)
		}
	}
	return redact.New(known).Record
}

// envFor applies set to base: a nil value removes the variable.
func envFor(base []string, set map[string]*string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			k = kv[:i+min(1, len(kv))]
		}
		if _, ok := set[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := set[k]; v != nil {
			out = append(out, k+"="+*v)
		}
	}
	return out
}
