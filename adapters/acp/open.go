package acp

// Open: the oarlock.StartSession of the family. Everything that can say "no"
// happens before a process exists: the refused-option table, the profile's own
// validation. Then: launch, initialize (+authenticate), session/new or
// session/load, the yolo mode, and the model/effort the host asked for.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
)

const openTimeout = 60 * time.Second

func start(ctx context.Context, prof Profile, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	if ctx == nil {
		return nil, errors.New("acp: Open(nil context)")
	}
	if prof.Select != nil {
		prof = prof.Select(inst)
	}
	if err := prof.Refused.Check(opt); err != nil {
		return nil, err
	}
	if prof.Validate != nil {
		if err := prof.Validate(opt); err != nil {
			return nil, err
		}
	}
	command := inst.Path
	if command == "" {
		command = prof.ID
	}
	cwd := opt.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("acp: cannot determine the working directory: %w", err)
		}
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	args := prof.Args(opt)
	if n := len(opt.LaunchArgs); n > 0 {
		if prof.LaunchArgsBeforeLast && len(args) > 0 {
			args = append(append(append([]string(nil), args[:len(args)-1]...), opt.LaunchArgs...), args[len(args)-1])
		} else {
			args = append(append([]string(nil), args...), opt.LaunchArgs...)
		}
	}

	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd, err := exec.Start(pctx, &exec.Command{Path: command, Args: args, Dir: cwd, Env: envFor(os.Environ(), opt.Env)})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp: launch %s: %w", prof.ID, err)
	}
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	s := &Session{prof: prof, opt: opt, cmd: cmd, cwd: cwd, grace: defaultGrace, term: newTerminals(), ctx: pctx, cancel: cancel,
		sink: newSink(prof), readDone: make(chan struct{}), exitRecorded: make(chan struct{}), stderr: &tail{}}
	go func() { _, _ = io.Copy(s.stderr, cmd.Stderr()) }()
	reader := newTapReader(cmd.Stdout(), s.sink.fromAgent, func() { close(s.readDone) })
	writer := &tapWriter{w: cmd.Stdin(), line: s.sink.toAgent}
	s.conn = sdk.NewClientSideConnection(&client{s: s, t: s.term}, writer, reader)
	s.conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil))) // the stream is the log

	hctx, hcancel := context.WithTimeout(ctx, openTimeout)
	defer hcancel()
	if err := s.handshake(hctx, opt); err != nil {
		_ = s.cmd.Close(context.Background())
		cancel()
		var nf *oarlock.SessionNotFound
		if errors.As(err, &nf) {
			return nil, err
		}
		if tl := strings.TrimSpace(s.stderr.String()); tl != "" {
			return nil, fmt.Errorf("acp: %s: %w; stderr: %s", prof.ID, err, tl)
		}
		return nil, fmt.Errorf("acp: %s: %w", prof.ID, err)
	}
	s.k = kernel.New(s.id, redact.New(secretValues(opt)).Record)
	s.sink.attach(s.k)
	go s.watchExit()
	if prof.SteerMeta != nil {
		return steerSession{s}, nil
	}
	return s, nil
}

func (s *Session) handshake(ctx context.Context, opt oarlock.Options) error {
	prof := s.prof
	init := sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{Terminal: true},
		ClientInfo:         &sdk.Implementation{Name: "oarlock", Version: "0"},
	}
	if prof.InitializeMeta != nil {
		init.Meta = prof.InitializeMeta(opt)
	}
	ir, err := s.conn.Initialize(ctx, init)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	s.images = ir.AgentCapabilities.PromptCapabilities.Image
	if prof.Images != nil {
		s.images = *prof.Images
	}
	if prof.AuthMethod != nil {
		if id := prof.AuthMethod(s.sink.result("initialize")); id != "" {
			if _, err := s.conn.Authenticate(ctx, sdk.AuthenticateRequest{MethodId: id}); err != nil {
				return fmt.Errorf("authenticate: %w", err)
			}
		}
	}

	mcp := mcpServers(opt.MCPServers)
	var meta map[string]any
	if prof.SessionMeta != nil {
		meta = prof.SessionMeta(opt)
	}
	// additionalDirectories travels only when the agent advertised the
	// capability; a profile whose vendor ignores it refuses the option instead.
	var dirs []string
	if len(opt.AdditionalDirectories) > 0 {
		if ir.AgentCapabilities.SessionCapabilities.AdditionalDirectories == nil {
			return &oarlock.UnsupportedOption{Option: oarlock.OptionAdditionalDirectories, Reason: "the agent did not advertise sessionCapabilities.additionalDirectories"}
		}
		for _, d := range opt.AdditionalDirectories {
			if !filepath.IsAbs(d) {
				d = filepath.Join(s.cwd, d)
			}
			dirs = append(dirs, d)
		}
	}

	var openResult map[string]any
	if opt.Resume != "" {
		s.id = opt.Resume
		s.sink.quiet.Store(true) // history replays as frames, never as new events
		var err error
		if ir.AgentCapabilities.SessionCapabilities.Resume != nil {
			_, err = s.conn.ResumeSession(ctx, sdk.ResumeSessionRequest{SessionId: sdk.SessionId(s.id), Cwd: s.cwd, McpServers: mcp, AdditionalDirectories: dirs, Meta: meta})
			openResult = s.sink.result("session/resume")
		} else if ir.AgentCapabilities.LoadSession {
			_, err = s.conn.LoadSession(ctx, sdk.LoadSessionRequest{SessionId: sdk.SessionId(s.id), Cwd: s.cwd, McpServers: mcp, AdditionalDirectories: dirs, Meta: meta})
			openResult = s.sink.result("session/load")
		} else {
			err = &oarlock.UnsupportedOption{Option: "resume", Reason: "the agent advertises neither session/resume nor session/load"}
		}
		s.sink.quiet.Store(false)
		if err != nil {
			var un *oarlock.UnsupportedOption
			var re *sdk.RequestError
			if errors.As(err, &un) {
				return err
			}
			if errors.As(err, &re) && prof.IsResumeNotFound != nil && prof.IsResumeNotFound(re.Code, re.Data) {
				return &oarlock.SessionNotFound{SessionID: s.id, Cause: err}
			}
			return fmt.Errorf("resume %s: %w", s.id, err)
		}
	} else {
		nr, err := s.conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: s.cwd, McpServers: mcp, AdditionalDirectories: dirs, Meta: meta})
		if err != nil {
			return fmt.Errorf("session/new: %w", err)
		}
		if nr.SessionId == "" {
			return errors.New("session/new returned no session id")
		}
		s.id = string(nr.SessionId)
		openResult = s.sink.result("session/new")
	}
	if prof.YoloMode && advertisesYolo(openResult) {
		if _, err := s.conn.SetSessionMode(ctx, sdk.SetSessionModeRequest{SessionId: sdk.SessionId(s.id), ModeId: "yolo"}); err != nil {
			return fmt.Errorf("session/set_mode yolo: %w", err)
		}
	}
	return s.applyModelEffort(ctx, opt, openResult)
}

// applyModelEffort sets what the host asked for through session/set_config_option.
// An effort the agent has no thought_level option for is refused, not echoed.
func (s *Session) applyModelEffort(ctx context.Context, opt oarlock.Options, open map[string]any) error {
	set := func(id, value string) (map[string]any, error) {
		if _, err := s.conn.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
			SessionId: sdk.SessionId(s.id), ConfigId: sdk.SessionConfigId(id), Value: sdk.SessionConfigValueId(value)}}); err != nil {
			return nil, fmt.Errorf("session/set_config_option %s=%s: %w", id, value, err)
		}
		return s.sink.result("session/set_config_option"), nil
	}
	if opt.Model != "" {
		r, err := set("model", opt.Model)
		if err != nil {
			return err
		}
		if r != nil {
			open = r
		}
	}
	if opt.Effort != "" {
		id := ""
		if opts, ok := open["configOptions"].([]any); ok {
			for _, o := range opts {
				if m := asMap(o); asString(m["category"]) == "thought_level" {
					id = asString(m["id"])
				}
			}
		}
		if id == "" {
			return &oarlock.UnsupportedOption{Option: "effort", Reason: "the session advertises no thought_level config option"}
		}
		if _, err := set(id, opt.Effort); err != nil {
			return err
		}
	}
	return nil
}

func advertisesYolo(result map[string]any) bool {
	for _, m := range asSlice(asMap(result["modes"])["availableModes"]) {
		if asString(asMap(m)["id"]) == "yolo" {
			return true
		}
	}
	for _, o := range asSlice(result["configOptions"]) {
		if m := asMap(o); asString(m["id"]) == "mode" {
			for _, v := range asSlice(m["options"]) {
				if asString(asMap(v)["value"]) == "yolo" {
					return true
				}
			}
		}
	}
	return false
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func mcpServers(in []oarlock.MCPServer) []sdk.McpServer {
	out := make([]sdk.McpServer, 0, len(in))
	for _, m := range in {
		if m.IsHTTP() {
			h := &sdk.McpServerHttpInline{Name: m.Name, Type: "http", Url: m.URL, Headers: []sdk.HttpHeader{}}
			for _, k := range sortedKeys(m.Headers) {
				h.Headers = append(h.Headers, sdk.HttpHeader{Name: k, Value: m.Headers[k]})
			}
			out = append(out, sdk.McpServer{Http: h})
			continue
		}
		st := &sdk.McpServerStdio{Name: m.Name, Command: m.Command, Args: append([]string{}, m.Args...), Env: []sdk.EnvVariable{}}
		for _, k := range sortedKeys(m.Env) {
			st.Env = append(st.Env, sdk.EnvVariable{Name: k, Value: m.Env[k]})
		}
		out = append(out, sdk.McpServer{Stdio: st})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// envFor overlays the session's Env on base; a nil value removes the variable.
func envFor(base []string, set map[string]*string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
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

func envWith(vars []sdk.EnvVariable) []string {
	env := os.Environ()
	for _, v := range vars {
		env = append(env, v.Name+"="+v.Value)
	}
	return env
}

// secretValues are the session's own credential values: every Env value and
// every MCP env/header value is a redaction candidate (docs/spec/redaction.md).
func secretValues(opt oarlock.Options) []string {
	var known []string
	for _, v := range opt.Env {
		if v != nil {
			known = append(known, *v)
		}
	}
	for _, m := range opt.MCPServers {
		for _, v := range m.Env {
			known = append(known, v)
		}
		for _, v := range m.Headers {
			known = append(known, v)
		}
	}
	return known
}
