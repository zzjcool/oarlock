package pi

// Resume tests (issue #12). Three layers:
//
//   - the resolver (resume.go) against files in t.TempDir(): path rules, the
//     evidence discipline of SessionNotFound, the cwd refusal;
//   - open (open.go) against a fake Pi, which is this very test binary re-executed
//     (see conn_test.go) and which mirrors what pi 1.4.0 was MEASURED to do
//     (docs/runtimes/pi.md §12): a session file appears with the first message,
//     `--session <missing path>` silently starts a new session, the cwd is the
//     header's;
//   - one live test against the real pi, off unless OAR_PI_LIVE=1.
//
// Nothing here touches the real ~/.pi except the live test, which owns a unique
// directory and removes it.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

const (
	argvEnv    = "OARLOCK_PI_TEST_ARGV"
	forceIDEnv = "OARLOCK_PI_TEST_FORCE_ID"
	hangEnv    = "OARLOCK_PI_TEST_HANG_AFTER_USER"
)

func init() { helperModes["fake-pi"] = helperFakePi }

// ---- fake pi (child side) ----------------------------------------------------

var codewordRE = regexp.MustCompile(`[A-Z]+-[0-9]+`)

// helperFakePi speaks the slice of Pi's RPC that open and Session use, with the
// persistence and flag behaviour measured on pi 1.4.0.
func helperFakePi() int {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// `pi --version` is the gate's probe (gate.go). It is answered before the
	// session-started marker is written, so a test can tell the probe from a session.
	for _, a := range args {
		if a == "--version" {
			return helperFakeVersion(args)
		}
	}
	if p := os.Getenv(markerEnv); p != "" {
		_ = os.WriteFile(p, []byte("started"), 0o600)
	}
	if p := os.Getenv(argvEnv); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(args, "\n")), 0o600)
	}
	var sessionDir, sessionFile string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--session-dir":
			sessionDir = args[i+1]
		case "--session":
			sessionFile = args[i+1]
		}
	}
	cwd, _ := os.Getwd()
	id := fmt.Sprintf("fake-%d", time.Now().UnixNano())
	var history []string
	persisted := false
	if sessionFile != "" {
		if b, err := os.ReadFile(sessionFile); err == nil {
			// An existing file is loaded: the id comes from its header.
			for _, l := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(l) == "" {
					continue
				}
				var v struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				}
				_ = json.Unmarshal([]byte(l), &v)
				if v.Type == "session" {
					id = v.ID
				}
				history = append(history, l)
			}
			persisted = len(history) > 0
		}
		// A missing file is what real pi turns into a brand-new session bound to
		// that path: keep the fresh id and the path.
	} else if sessionDir != "" {
		sessionFile = filepath.Join(sessionDir, time.Now().UTC().Format("2006-01-02T15-04-05-000Z")+"_"+id+".jsonl")
	}
	reportID := id
	if f := os.Getenv(forceIDEnv); f != "" {
		reportID = f
	}

	appendFile := func(lines ...string) {
		if sessionFile == "" {
			return
		}
		flag := os.O_APPEND | os.O_WRONLY | os.O_CREATE
		if !persisted {
			_ = os.MkdirAll(filepath.Dir(sessionFile), 0o755)
			flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
			h, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": "2026-10-28T00:00:00.000Z", "cwd": cwd})
			lines = append([]string{string(h)}, lines...)
		}
		f, err := os.OpenFile(sessionFile, flag, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake-pi: persist: %v\n", err)
			return
		}
		for _, l := range lines {
			_, _ = f.WriteString(l + "\n")
		}
		_ = f.Close()
		persisted = true
	}

	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return 0 // stdin EOF, which is what dispose sends
		}
		var c struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &c) != nil || c.ID == "" || c.Type == "" {
			fmt.Fprintf(os.Stderr, "fake-pi: not a command line: %q\n", line)
			return 9
		}
		cmd := wireCommand{ID: c.ID, Type: c.Type}
		switch c.Type {
		case "get_state":
			respond(cmd, fmt.Sprintf(`{"sessionId":%q,"sessionFile":%q,"messageCount":%d}`, reportID, sessionFile, len(history)))
		case "prompt":
			respond(cmd, `{"disposition":"started"}`)
			emit(`{"type":"agent_start"}`)
			emit(`{"type":"turn_start"}`)
			emit(`{"type":"message_start","message":{"role":"system","content":""}}`)
			emit(`{"type":"message_end","message":{"role":"system","content":""}}`)
			user := fmt.Sprintf(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, c.Message)
			emit(fmt.Sprintf(`{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, c.Message))
			emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, c.Message))
			appendFile(user)
			history = append(history, user)
			if os.Getenv(hangEnv) == "1" {
				select {} // the instance "dies" mid-turn: the file holds the user message only
			}
			answer := "noted"
			if strings.Contains(c.Message, "codeword?") {
				for i := len(history) - 1; i >= 0; i-- {
					if w := codewordRE.FindString(history[i]); w != "" {
						answer = w
						break
					}
				}
			}
			asst := fmt.Sprintf(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, answer)
			emit(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)
			emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, answer))
			appendFile(asst)
			history = append(history, asst)
			emit(`{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}`)
			emit(`{"type":"agent_end","willRetry":false}`)
			emit(`{"type":"agent_settled","aborted":false}`)
		default:
			respond(cmd, `{}`)
		}
	}
}

// ---- helpers ------------------------------------------------------------------

type fakeEnv struct {
	cwd, agentDir, marker, argv string
	env                         map[string]*string
}

func newFakeEnv(t *testing.T, extra map[string]string) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	e := &fakeEnv{
		cwd:      filepath.Join(root, "proj"),
		agentDir: filepath.Join(root, "agent"),
		marker:   filepath.Join(root, "marker"),
		argv:     filepath.Join(root, "argv"),
	}
	if err := os.MkdirAll(e.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// The temp dir may itself be a symlink (macOS): Pi would record the physical
	// path, so the tests use it too.
	if real, err := filepath.EvalSymlinks(e.cwd); err == nil {
		e.cwd = real
	}
	mode := "fake-pi"
	e.env = map[string]*string{
		modeEnv:       &mode,
		envAgentDir:   &e.agentDir,
		markerEnv:     &e.marker,
		argvEnv:       &e.argv,
		"GORACE":      strp("atexit_sleep_ms=0"),
		forceIDEnv:    nil,
		hangEnv:       nil,
		"HOME":        strp(root),
		"USERPROFILE": strp(root),
	}
	for k, v := range extra {
		v := v
		e.env[k] = &v
	}
	return e
}

func strp(s string) *string { return &s }

// chdir changes the working directory until the test ends. testing.T.Chdir is
// Go 1.24 and this module builds with 1.23. The tests of this package do not run
// in parallel, so the process-wide change is safe.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func (e *fakeEnv) options(resume string) oarlock.Options {
	env := make(map[string]*string, len(e.env))
	for k, v := range e.env {
		env[k] = v
	}
	return oarlock.Options{Cwd: e.cwd, Resume: resume, Env: env}
}

func (e *fakeEnv) spec(t *testing.T) openSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return openSpec{
		launch: Options{Command: exe, CommandTimeout: 10 * time.Second, KillGrace: time.Second, WaitDelay: 2 * time.Second},
		prefix: []string{"-test.run=TestHelperProcess$", "--"},
	}
}

func (e *fakeEnv) open(t *testing.T, resume string) (*Session, error) {
	t.Helper()
	s, err := open(context.Background(), e.spec(t), e.options(resume))
	if s != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.Dispose(ctx)
		})
	}
	return s, err
}

func (e *fakeEnv) mustOpen(t *testing.T, resume string) *Session {
	t.Helper()
	s, err := e.open(t, resume)
	if err != nil {
		t.Fatalf("open(resume=%q) = %v", resume, err)
	}
	return s
}

func (e *fakeEnv) started() bool {
	_, err := os.Stat(e.marker)
	return err == nil
}

func (e *fakeEnv) sessionDir() string { return SessionDir(e.cwd, e.agentDir) }

// writeSession writes a session file in dir and returns its path.
func writeSession(t *testing.T, dir, id, cwd string, extra ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hdr, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": "2026-10-28T00:00:00.000Z", "cwd": cwd})
	body := string(hdr) + "\n" + strings.Join(extra, "")
	p := filepath.Join(dir, "2026-10-28T00-00-00-000Z_"+id+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func recordsOf(s *Session) []oarlock.Record { return s.Records(oarlock.Cursor{}) }

func idOf(t *testing.T, s *Session) string {
	t.Helper()
	coreWait(t, "first record", func() bool { return len(recordsOf(s)) > 0 })
	return recordsOf(s)[0].SessionID
}

func turnsEnded(s *Session) int { return countEvents(recordsOf(s), oarlock.EventTurnEnded) }

// promptAndWait sends text and waits for the n-th turn_ended.
func promptAndWait(t *testing.T, s *Session, text string, n int) {
	t.Helper()
	out, err := s.Prompt(context.Background(), text, oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt(%q) = %+v, %v; want accepted", text, out, err)
	}
	coreWait(t, "turn_ended", func() bool { return turnsEnded(s) >= n })
}

// assistantTexts are the texts of the message_end frames of role assistant.
func assistantTexts(records []oarlock.Record) (out []string) {
	for _, r := range records {
		var v struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(r.Native, &v) == nil && v.Type == "message_end" && v.Message.Role == "assistant" {
			for _, c := range v.Message.Content {
				if c.Type == "text" {
					out = append(out, c.Text)
				}
			}
		}
	}
	return out
}

func asSessionNotFound(err error) (*oarlock.SessionNotFound, bool) {
	var nf *oarlock.SessionNotFound
	return nf, errors.As(err, &nf)
}

// ---- resolver: directory and environment rules ---------------------------------

func TestSessionDirIsAPortOfPiGetDefaultSessionDirPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the expectations are POSIX paths")
	}
	for _, c := range []struct{ cwd, agent, want string }{
		{"/tmp/x/y", "/a", "/a/sessions/--tmp-x-y--"},
		{"/tmp/x/y/", "/a", "/a/sessions/--tmp-x-y--"},
		{"/", "/a", "/a/sessions/----"},
		{"/root/code/oarlock", "/root/.pi/agent", "/root/.pi/agent/sessions/--root-code-oarlock--"},
		{"/a/b-c", "/g", "/g/sessions/--a-b-c--"}, // lossy by design: /a-b/c collides, hence the header cwd check
		{"/a-b/c", "/g", "/g/sessions/--a-b-c--"},
		{"/with space/é", "/g", "/g/sessions/--with space-é--"},
		{`/odd\name:x`, "/g", "/g/sessions/--odd-name-x--"},
	} {
		if got := SessionDir(c.cwd, c.agent); got != c.want {
			t.Errorf("SessionDir(%q, %q) = %q, want %q", c.cwd, c.agent, got, c.want)
		}
	}
	// A relative cwd is made absolute first, like Pi's resolvePath.
	wd, _ := os.Getwd()
	if got, want := SessionDir("rel/dir", "/g"), SessionDir(filepath.Join(wd, "rel", "dir"), "/g"); got != want {
		t.Errorf("relative cwd: %q, want %q", got, want)
	}
}

func TestAgentDirFollowsPiGetAgentDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the expectations are POSIX paths")
	}
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"override wins", []string{"HOME=/h", "PI_CODING_AGENT_DIR=/x/agent"}, "/x/agent"},
		{"empty counts as unset", []string{"HOME=/h", "PI_CODING_AGENT_DIR="}, "/h/.pi/agent"},
		{"home fallback", []string{"HOME=/h"}, "/h/.pi/agent"},
		{"tilde alone", []string{"HOME=/h", "PI_CODING_AGENT_DIR=~"}, "/h"},
		{"tilde path", []string{"HOME=/h", "PI_CODING_AGENT_DIR=~/cfg"}, "/h/cfg"},
		{"last duplicate wins", []string{"HOME=/h", "PI_CODING_AGENT_DIR=/first", "PI_CODING_AGENT_DIR=/second"}, "/second"},
		{"result is cleaned", []string{"PI_CODING_AGENT_DIR=/x//y/../agent/"}, "/x/agent"},
	} {
		got, err := AgentDir(c.env)
		if err != nil || got != c.want {
			t.Errorf("%s: AgentDir(%v) = %q, %v; want %q", c.name, c.env, got, err, c.want)
		}
	}
	rel, err := AgentDir([]string{"PI_CODING_AGENT_DIR=rel/agent"})
	if err != nil || !filepath.IsAbs(rel) {
		t.Errorf("a relative override must come back absolute, got %q, %v", rel, err)
	}
}

func TestEnvForAppliesSetsAndRemovals(t *testing.T) {
	base := []string{"A=1", "B=2", "A=3", "KEEP=x=y"}
	snapshot := append([]string(nil), base...)
	v, empty := "new", ""
	got := EnvFor(base, map[string]*string{"A": &v, "B": nil, "C": &empty, "D": nil})
	want := []string{"KEEP=x=y", "A=new", "C="}
	// EnvFor puts additions after the kept entries, in key order.
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("EnvFor = %v, want %v", got, want)
	}
	if fmt.Sprint(base) != fmt.Sprint(snapshot) {
		t.Errorf("base was modified: %v", base)
	}
	if kept := EnvFor(base, nil); fmt.Sprint(kept) != fmt.Sprint(base) {
		t.Errorf("EnvFor(base, nil) = %v, want base unchanged", kept)
	}
}

// ---- resolver: by id ------------------------------------------------------------

func TestResolveResumeByIDFindsTheExactSession(t *testing.T) {
	agent, cwd := t.TempDir(), t.TempDir()
	dir := SessionDir(cwd, agent)
	writeSession(t, dir, "aaaa-1", cwd)
	want := writeSession(t, dir, "bbbb-2", cwd)

	got, err := ResolveResume("bbbb-2", cwd, agent)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "bbbb-2" || got.File != want || got.Cwd != cwd || !filepath.IsAbs(got.File) {
		t.Fatalf("target = %+v, want id bbbb-2 file %s cwd %s", got, want, cwd)
	}
	if zero, err := ResolveResume("", cwd, agent); err != nil || zero != (ResumeTarget{}) {
		t.Fatalf("empty resume = %+v, %v; want the zero target and no error", zero, err)
	}
}

func TestResolveResumeByIDNotFoundCarriesScopeAndNativeCause(t *testing.T) {
	agent, cwd, other := t.TempDir(), t.TempDir(), t.TempDir()
	dir := SessionDir(cwd, agent)
	writeSession(t, dir, "abcdef-1", cwd)
	writeSession(t, dir, "abcdef-2", cwd)
	writeSession(t, SessionDir(other, agent), "elsewhere-9", other)

	t.Run("a prefix is not a match and the scan is counted", func(t *testing.T) {
		_, err := ResolveResume("abcdef", cwd, agent)
		nf, ok := asSessionNotFound(err)
		if !ok || nf.SessionID != "abcdef" {
			t.Fatalf("err = %v, want SessionNotFound{abcdef}", err)
		}
		var le *LookupError
		if !errors.As(err, &le) || le.Scanned != 2 || le.Err != nil || le.SessionDir != dir || le.Cwd != cwd {
			t.Fatalf("cause = %+v, want a complete scan of 2 in %s", le, dir)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Error("a complete scan has no native error: ErrNotExist must not hold")
		}
	})
	t.Run("another cwd is never searched", func(t *testing.T) {
		_, err := ResolveResume("elsewhere-9", cwd, agent)
		var le *LookupError
		if _, ok := asSessionNotFound(err); !ok || !errors.As(err, &le) || le.SessionDir != dir {
			t.Fatalf("err = %v; want SessionNotFound scoped to %s", err, dir)
		}
		if got, err := ResolveResume("elsewhere-9", other, agent); err != nil || got.Cwd != other {
			t.Fatalf("in its own cwd it resolves, got %+v, %v", got, err)
		}
	})
	t.Run("a missing session directory keeps the native error", func(t *testing.T) {
		fresh := t.TempDir()
		_, err := ResolveResume("whatever", fresh, agent)
		nf, ok := asSessionNotFound(err)
		if !ok || nf.SessionID != "whatever" {
			t.Fatalf("err = %v, want SessionNotFound", err)
		}
		var pe *fs.PathError
		if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) {
			t.Fatalf("Cause must unwrap to the native *PathError ENOENT, got %v", err)
		}
		if !strings.Contains(nf.Error(), SessionDir(fresh, agent)) {
			t.Errorf("the message must name the directory searched: %v", nf)
		}
	})
	t.Run("an empty file is skipped, not proof of anything", func(t *testing.T) {
		d := t.TempDir()
		sd := SessionDir(d, agent)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sd, "empty.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveResume("x", d, agent)
		var le *LookupError
		if !errors.As(err, &le) || le.Scanned != 0 {
			t.Fatalf("err = %v, want SessionNotFound after scanning 0 headers", err)
		}
	})
}

// Absence is only claimed on positive evidence: every case here must be a plain
// error, because a host that read it as "gone" might start over a recoverable
// conversation (errors.md, "SessionNotFound ... positive evidence").
func TestResolveResumeIsNotNotFoundWithoutPositiveEvidence(t *testing.T) {
	agent := t.TempDir()
	notNF := func(t *testing.T, name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: err = nil", name)
		}
		if _, ok := asSessionNotFound(err); ok {
			t.Fatalf("%s: err = %v is a SessionNotFound without proof of absence", name, err)
		}
		var uo *oarlock.UnsupportedOption
		if errors.As(err, &uo) {
			t.Fatalf("%s: err = %v is an UnsupportedOption", name, err)
		}
	}

	t.Run("the session directory is a regular file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("ENOTDIR semantics differ")
		}
		cwd := t.TempDir()
		sd := SessionDir(cwd, agent)
		if err := os.MkdirAll(filepath.Dir(sd), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sd, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveResume("some-id", cwd, agent)
		notNF(t, "dir is a file", err)
	})
	t.Run("the resume path is a directory", func(t *testing.T) {
		cwd := t.TempDir()
		_, err := ResolveResume(cwd, cwd, agent)
		notNF(t, "path is a dir", err)
	})
	t.Run("the resume path is not a pi session", func(t *testing.T) {
		cwd := t.TempDir()
		p := filepath.Join(cwd, "notes.jsonl")
		if err := os.WriteFile(p, []byte("this is not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveResume(p, cwd, agent)
		notNF(t, "garbage file", err)
		if !errors.Is(err, errNotSession) {
			t.Errorf("err = %v, want it to unwrap to errNotSession", err)
		}
	})
	t.Run("a non-empty file with no valid header blocks the verdict", func(t *testing.T) {
		cwd := t.TempDir()
		sd := SessionDir(cwd, agent)
		writeSession(t, sd, "real-1", cwd)
		if err := os.WriteFile(filepath.Join(sd, "corrupt.jsonl"), []byte("{broken\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveResume("real-1-typo", cwd, agent)
		notNF(t, "corrupt neighbour", err)
		if got, err := ResolveResume("real-1", cwd, agent); err != nil || got.SessionID != "real-1" {
			t.Fatalf("an existing id must still resolve next to a corrupt file, got %+v, %v", got, err)
		}
	})
	t.Run("two files with one id are ambiguous", func(t *testing.T) {
		cwd := t.TempDir()
		sd := SessionDir(cwd, agent)
		writeSession(t, sd, "twin", cwd)
		if err := os.WriteFile(filepath.Join(sd, "copy_twin.jsonl"), []byte(`{"type":"session","id":"twin","cwd":`+fmt.Sprintf("%q", cwd)+"}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveResume("twin", cwd, agent)
		notNF(t, "duplicate id", err)
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("err = %v, want it to say ambiguous", err)
		}
	})
	t.Run("an unreadable file blocks the verdict but not a match", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink loops need unix semantics")
		}
		cwd := t.TempDir()
		sd := SessionDir(cwd, agent)
		good := writeSession(t, sd, "good-1", cwd)
		// open(2) fails with ELOOP: an I/O error that is neither "missing" nor "not a session".
		if err := os.Symlink("loop.jsonl", filepath.Join(sd, "loop.jsonl")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := ResolveResume("good-1-typo", cwd, agent)
		notNF(t, "ELOOP neighbour", err)
		if !errors.Is(err, syscall.ELOOP) {
			t.Errorf("err = %v, want the native ELOOP reachable", err)
		}
		got, err := ResolveResume("good-1", cwd, agent)
		if err != nil || got.File != good {
			t.Fatalf("a match is positive evidence by itself, got %+v, %v", got, err)
		}
	})
}

// ---- resolver: by file ----------------------------------------------------------

func TestResolveResumeByFile(t *testing.T) {
	agent, cwd := t.TempDir(), t.TempDir()
	file := writeSession(t, t.TempDir(), "file-1", cwd)

	t.Run("an absolute file in the same cwd", func(t *testing.T) {
		got, err := ResolveResume(file, cwd, agent)
		if err != nil || got.SessionID != "file-1" || got.File != file {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("a relative file is relative to cwd", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(cwd, "rel.jsonl"), mustRead(t, file), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveResume("rel.jsonl", cwd, agent)
		if err != nil || got.File != filepath.Join(cwd, "rel.jsonl") || got.SessionID != "file-1" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("a missing file is not found with the native cause", func(t *testing.T) {
		missing := filepath.Join(cwd, "nope", "gone.jsonl")
		_, err := ResolveResume(missing, cwd, agent)
		nf, ok := asSessionNotFound(err)
		if !ok || nf.SessionID != missing || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want SessionNotFound wrapping ErrNotExist", err)
		}
		var le *LookupError
		if !errors.As(err, &le) || le.Path != missing {
			t.Fatalf("cause = %+v, want Path %s", le, missing)
		}
	})
	t.Run("a header without a cwd is accepted", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "nocwd.jsonl")
		if err := os.WriteFile(p, []byte(`{"type":"session","id":"nocwd-1"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := ResolveResume(p, cwd, agent); err != nil || got.SessionID != "nocwd-1" || got.Cwd != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The different-cwd conclusion (issue #12 criterion 4): refused, never run. Pi
// itself would accept the file and run in the header's directory (docs/runtimes/
// pi.md §12, E2).
func TestResolveResumeRefusesASessionRecordedUnderAnotherCwd(t *testing.T) {
	agent, a, b := t.TempDir(), t.TempDir(), t.TempDir()

	t.Run("by file", func(t *testing.T) {
		file := writeSession(t, SessionDir(a, agent), "moved-1", a)
		_, err := ResolveResume(file, b, agent)
		var uo *oarlock.UnsupportedOption
		if !errors.As(err, &uo) || uo.Option != "cwd" {
			t.Fatalf("err = %v, want UnsupportedOption{cwd}", err)
		}
		if !strings.Contains(uo.Reason, a) || !strings.Contains(uo.Reason, b) {
			t.Errorf("Reason must name both directories: %q", uo.Reason)
		}
		if _, ok := asSessionNotFound(err); ok {
			t.Error("the session exists: this is not a SessionNotFound")
		}
	})
	t.Run("by id the other cwd's directory is not searched", func(t *testing.T) {
		writeSession(t, SessionDir(a, agent), "moved-2", a)
		_, err := ResolveResume("moved-2", b, agent)
		if _, ok := asSessionNotFound(err); !ok {
			t.Fatalf("err = %v, want SessionNotFound scoped to cwd %s", err, b)
		}
	})
	t.Run("a directory-name collision is still caught by the header", func(t *testing.T) {
		// /x/a-b and /x/a/b encode to the same directory name.
		root := t.TempDir()
		c1, c2 := filepath.Join(root, "a-b"), filepath.Join(root, "a", "b")
		for _, d := range []string{c1, c2} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if SessionDir(c1, agent) != SessionDir(c2, agent) {
			t.Skip("the two cwds do not collide on this platform")
		}
		writeSession(t, SessionDir(c1, agent), "collide-1", c1)
		_, err := ResolveResume("collide-1", c2, agent)
		var uo *oarlock.UnsupportedOption
		if !errors.As(err, &uo) || uo.Option != "cwd" {
			t.Fatalf("err = %v, want UnsupportedOption{cwd}", err)
		}
	})
	t.Run("a symlinked cwd is the same directory", func(t *testing.T) {
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		file := writeSession(t, t.TempDir(), "link-1", real) // Pi records the physical path
		if got, err := ResolveResume(file, link, agent); err != nil || got.SessionID != "link-1" {
			t.Fatalf("got %+v, %v; want the symlinked cwd accepted", got, err)
		}
	})
}

// ---- open against the fake pi ---------------------------------------------------

func TestOpenNotFoundStartsNoProcess(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	e := newFakeEnv(t, nil)
	s, err := e.open(t, "no-such-session-00000")
	nf, ok := asSessionNotFound(err)
	if s != nil || !ok || nf.SessionID != "no-such-session-00000" {
		t.Fatalf("open = %v, %v; want SessionNotFound and no session", s, err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing session directory must keep ENOENT reachable, got %v", err)
	}
	if e.started() {
		t.Fatal("a process was started for a session that does not exist")
	}
	noLeaks(t)
}

func TestOpenRefusesUnimplementedOptionsBeforeAnyProcess(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	e := newFakeEnv(t, nil)
	opt := e.options("")
	opt.SystemPrompt = "be brief"
	_, err := open(context.Background(), e.spec(t), opt)
	var uo *oarlock.UnsupportedOption
	if !errors.As(err, &uo) || uo.Option != oarlock.OptionSystemPrompt {
		t.Fatalf("err = %v, want UnsupportedOption{systemPrompt}", err)
	}
	if e.started() {
		t.Fatal("a refused option must be refused before any process starts")
	}
	// Every frozen option except env is refused, and env is honoured (the fake
	// sees the variables the test set, which is how it knows it is the fake).
	refused := refusedOptions
	for _, o := range oarlock.AllRefusableOptions {
		_, has := refused[o]
		if has == (o == oarlock.OptionEnv) {
			t.Errorf("option %s: refused=%v", o, has)
		}
	}
	s := e.mustOpen(t, "")
	if idOf(t, s) == "" || !e.started() {
		t.Fatal("Env did not reach the child")
	}
}

func TestOpenPassesTheSessionFileNotTheID(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e := newFakeEnv(t, nil)
	s1 := e.mustOpen(t, "")
	id := idOf(t, s1)
	promptAndWait(t, s1, "Remember the codeword PELICAN-42.", 1)
	if err := s1.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}

	s2 := e.mustOpen(t, id)
	_ = idOf(t, s2)
	argv := strings.Split(string(mustRead(t, e.argv)), "\n")
	file := ""
	for i, a := range argv {
		if a == "--session" && i+1 < len(argv) {
			file = argv[i+1]
		}
		if a == id {
			t.Fatalf("the bare id %q was passed to pi (argv %v): a bare id from another cwd makes pi wait for a fork prompt on stdin", id, argv)
		}
	}
	if !filepath.IsAbs(file) || !strings.HasSuffix(file, "_"+id+".jsonl") || filepath.Dir(file) != e.sessionDir() {
		t.Fatalf("--session = %q, want the absolute file of %s in %s (argv %v)", file, id, e.sessionDir(), argv)
	}
	want := []string{"--mode", "rpc", "--session-dir", e.sessionDir()}
	if strings.Join(argv[:4], " ") != strings.Join(want, " ") {
		t.Errorf("argv starts %v, want %v", argv[:4], want)
	}
}

func TestOpenResumeReturnsPriorContextWithAFreshStream(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e := newFakeEnv(t, nil)

	s1 := e.mustOpen(t, "")
	id := idOf(t, s1)
	promptAndWait(t, s1, "Remember the codeword PELICAN-42. Reply with just the word: noted", 1)
	if got := assistantTexts(recordsOf(s1)); len(got) != 1 || got[0] != "noted" {
		t.Fatalf("first turn answered %v, want [noted]", got)
	}
	first := recordsOf(s1)
	if err := s1.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}

	s2 := e.mustOpen(t, id)
	if got := idOf(t, s2); got != id {
		t.Fatalf("resumed session id = %q, want %q", got, id)
	}
	before := recordsOf(s2)
	if before[0].Seq != 1 {
		t.Fatalf("resumed stream starts at Seq %d, want 1", before[0].Seq)
	}
	assertDense(t, before)
	for _, r := range before {
		if r.Kind != oarlock.KindFrame {
			t.Fatalf("record %d is a %s before the host did anything: resume invented a control", r.Seq, r.Kind)
		}
		if len(r.Events) != 0 {
			t.Fatalf("record %d carries events %v before any turn: resume must not synthesize facts", r.Seq, r.Events)
		}
	}
	if len(before) >= len(first) && string(before[len(before)-1].Native) == string(first[len(first)-1].Native) {
		t.Error("the old stream's tail was replayed into the new one")
	}

	promptAndWait(t, s2, "What was the codeword? Answer with just the codeword.", 1)
	got := assistantTexts(recordsOf(s2))
	if len(got) != 1 || got[0] != "PELICAN-42" {
		t.Fatalf("after resume the model answered %v, want [PELICAN-42] (the context lives in the session file)", got)
	}
	if n := countEvents(recordsOf(s2), oarlock.EventTurnActive); n != 1 {
		t.Errorf("turn_active = %d, want exactly the one turn the host started", n)
	}
	if got := turnOutcomes(recordsOf(s2)); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Errorf("outcomes = %v, want [completed]", got)
	}
	assertDense(t, recordsOf(s2))

	// The same conversation by file gives the same result.
	if err := s2.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var file string
	for _, a := range strings.Split(string(mustRead(t, e.argv)), "\n") {
		if strings.HasSuffix(a, ".jsonl") {
			file = a
		}
	}
	s3 := e.mustOpen(t, file)
	promptAndWait(t, s3, "What was the codeword? Answer with just the codeword.", 1)
	if got := assistantTexts(recordsOf(s3)); len(got) != 1 || got[0] != "PELICAN-42" {
		t.Fatalf("resume by file answered %v, want [PELICAN-42]", got)
	}
}

// The cursor is per stream INSTANCE (docs/spec/record-stream.md, "Cursor and
// stream instances"): Seq restarts at 1 on resume while the native session id
// stays the same, so a cursor of the previous instance carries the same
// SessionID and cannot be told apart by the kernel. The host must keep the
// instance identity itself. This test pins both halves of that sentence.
func TestResumedStreamCursorSemantics(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e := newFakeEnv(t, nil)
	s1 := e.mustOpen(t, "")
	id := idOf(t, s1)
	promptAndWait(t, s1, "Remember the codeword HERON-7.", 1)
	// A position held from the old instance: "I have seen all but the last 4".
	oldCursor := oarlock.Cursor{SessionID: id, AfterSeq: uint64(len(recordsOf(s1)) - 4)}
	if err := s1.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}

	s2 := e.mustOpen(t, id)
	if got := idOf(t, s2); got != id {
		t.Fatalf("resumed id %q, want %q", got, id)
	}
	promptAndWait(t, s2, "What was the codeword? Answer with just the codeword.", 1)
	all := recordsOf(s2)
	assertDense(t, all)
	if all[0].Seq != 1 {
		t.Fatalf("first Seq = %d, want 1", all[0].Seq)
	}

	// (1) A cursor that names another session is refused outright.
	foreign := oarlock.Cursor{SessionID: "some-other-session", AfterSeq: 1}
	if got := s2.Records(foreign); got != nil {
		t.Errorf("Records(foreign cursor) = %d records, want nil", len(got))
	}
	var refused int
	unsubForeign := s2.Events(&foreign, func(oarlock.Record) { refused++ })
	if unsubForeign == nil {
		t.Fatal("Events returned a nil unsubscribe for a refused cursor")
	}
	unsubForeign()

	// (2) The previous instance's cursor has the SAME SessionID: it is applied to
	// the new stream's records, by Seq alone. That is the documented rule, not a
	// promise that the cursor still means what the host thinks it means.
	got := s2.Records(oldCursor)
	if want := len(recordsOf(s2)) - int(oldCursor.AfterSeq); len(got) != want || got[0].Seq != oldCursor.AfterSeq+1 {
		t.Fatalf("an old-instance cursor replays %d records from Seq %d; want the new stream's suffix of %d from Seq %d",
			len(got), got[0].Seq, want, oldCursor.AfterSeq+1)
	}

	// (3) A cursor of this instance replays the suffix, then continues live with
	// no gap and no duplicate.
	k := uint64(3)
	if uint64(len(all)) <= k {
		t.Fatalf("the stream has only %d records; the test needs more than %d", len(all), k)
	}
	var (
		mu   sync.Mutex
		seen []uint64
	)
	unsub := s2.Events(&oarlock.Cursor{SessionID: id, AfterSeq: k}, func(r oarlock.Record) {
		mu.Lock()
		seen = append(seen, r.Seq)
		mu.Unlock()
	})
	defer unsub()
	promptAndWait(t, s2, "And again: what was the codeword? Answer with just the codeword.", 2)
	coreWait(t, "the live tail is delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0 && seen[len(seen)-1] == uint64(len(recordsOf(s2)))
	})
	mu.Lock()
	defer mu.Unlock()
	for i, seq := range seen {
		if seq != k+1+uint64(i) {
			t.Fatalf("Events from cursor %d saw %v: a gap or duplicate at index %d", k, seen, i)
		}
	}
	if refused != 0 {
		t.Errorf("a refused cursor delivered %d records", refused)
	}
}

// Dispose interaction (issue #12 item 4): the previous instance is ended by
// Dispose while a turn is open. Pi holds only the user message in the file and
// runs nothing; the new stream says idle and the first prompt is taken over.
func TestResumeAfterADisposedMidTurnInstanceTakesTheNextPrompt(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e := newFakeEnv(t, map[string]string{hangEnv: "1"})

	s1 := e.mustOpen(t, "")
	id := idOf(t, s1)
	out, err := s1.Prompt(context.Background(), "Remember the codeword HERON-7, then count to 400.", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt = %+v, %v", out, err)
	}
	coreWait(t, "the user message is persisted", func() bool {
		b, err := os.ReadFile(filepath.Join(e.sessionDir(), mustOneFile(t, e.sessionDir())))
		return err == nil && strings.Contains(string(b), "HERON-7")
	})
	coreWait(t, "the first instance to be mid-turn", func() bool { return statusOf(s1).Kind == fold.StatusRunning })
	if err := s1.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := turnsEnded(s1); got != 0 {
		t.Fatalf("a mid-turn dispose produced %d turn_ended; none may be invented", got)
	}

	e.env[hangEnv] = nil // a nil value removes the variable: the resumed Pi behaves normally
	s2 := e.mustOpen(t, id)
	_ = idOf(t, s2)
	if st := statusOf(s2); st.Kind != fold.StatusIdle {
		t.Fatalf("the resumed stream says %q, want idle: the old turn is not this instance's", st.Kind)
	}
	promptAndWait(t, s2, "What was the codeword? Answer with just the codeword.", 1)
	if got := assistantTexts(recordsOf(s2)); len(got) != 1 || got[0] != "HERON-7" {
		t.Fatalf("answered %v, want [HERON-7]: the dangling user message must still be context", got)
	}
	if got := turnOutcomes(recordsOf(s2)); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Fatalf("outcomes = %v, want [completed]", got)
	}

	// Dispose stays idempotent on the resumed session.
	for i := 0; i < 2; i++ {
		if err := s2.Dispose(context.Background()); err != nil {
			t.Fatalf("Dispose #%d = %v, want nil", i+1, err)
		}
	}
	disposes, exited := 0, 0
	for _, r := range recordsOf(s2) {
		if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionDispose {
			disposes++
		}
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			exited++
		}
	}
	if disposes != 1 || exited != 1 {
		t.Fatalf("dispose requests = %d, exited = %d; want 1 and 1", disposes, exited)
	}
	late, _ := s2.Prompt(context.Background(), "after dispose", oarlock.PromptOptions{})
	if late.Kind != oarlock.ControlRejected || (late.Code != oarlock.Disposed && late.Code != oarlock.RuntimeExited) {
		t.Fatalf("prompt after dispose = %+v, want rejected disposed/runtime_exited", late)
	}
}

func mustOneFile(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("session dir %s: %v, %d entries; want exactly one session file", dir, err, len(ents))
	}
	return ents[0].Name()
}

// Pi opens a --session path that does not exist as a brand-new session (E3).
// open must notice that the id is not the one it resolved.
func TestOpenFailsClosedWhenPiOpensADifferentSession(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	e := newFakeEnv(t, map[string]string{forceIDEnv: "some-other-id"})
	writeSession(t, e.sessionDir(), "wanted-1", e.cwd)

	s, err := e.open(t, "wanted-1")
	if s != nil || err == nil {
		t.Fatalf("open = %v, %v; want an error", s, err)
	}
	if _, ok := asSessionNotFound(err); ok {
		t.Fatalf("a mismatch is not proof of absence: %v", err)
	}
	var uo *oarlock.UnsupportedOption
	if errors.As(err, &uo) {
		t.Fatalf("a mismatch is not an unsupported option: %v", err)
	}
	if !strings.Contains(err.Error(), "wanted-1") || !strings.Contains(err.Error(), "some-other-id") {
		t.Errorf("the error must name both ids: %v", err)
	}
	if !e.started() {
		t.Fatal("the fake never started: the mismatch path was not exercised")
	}
	noLeaks(t)
}

func TestOpenUsesTheCurrentDirectoryWhenCwdIsEmpty(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	e := newFakeEnv(t, nil)
	opt := e.options("")
	opt.Cwd = ""
	chdir(t, e.cwd)
	s, err := open(context.Background(), e.spec(t), opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Dispose(context.Background()) })
	_ = idOf(t, s)
}

// ---- live ------------------------------------------------------------------------

// TestResumeLive is issue #12's real-machine acceptance: prompt → dispose →
// resume → follow-up, and the answer reflects the earlier turn. It needs the
// real pi, its login and the frozen model, so it is off unless OAR_PI_LIVE=1. It
// owns one unique session directory under the real agent dir and removes it.
func TestResumeLive(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to run the live Pi resume test")
	}
	exe, err := osexec.LookPath("pi")
	if err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	cwd := t.TempDir()
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	agent, err := AgentDir(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(SessionDir(cwd, agent)) })

	spec := openSpec{
		launch: Options{Command: exe},
		// The frozen argv of docs/runtimes/pi.md §1 minus `--mode rpc` (open adds
		// it) and `--no-session`. No --no-extensions: it removes the provider.
		extra: []string{"--offline", "--no-tools", "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-context-files",
			"--model", "any-endpoint/workbuddy/deepseek-v4.1-flash"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	wait := func(s *Session, n int) {
		t.Helper()
		deadline := time.Now().Add(150 * time.Second)
		for turnsEnded(s) < n {
			if time.Now().After(deadline) {
				t.Fatalf("no turn_ended #%d within 150s; stream: %d records", n, len(recordsOf(s)))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	lastText := func(s *Session) string {
		texts := assistantTexts(recordsOf(s))
		if len(texts) == 0 {
			return ""
		}
		return texts[len(texts)-1]
	}

	s1, err := open(ctx, spec, oarlock.Options{Cwd: cwd})
	if err != nil {
		t.Skipf("pi could not be opened here (login or model missing?): %v", err)
	}
	defer func() { _ = s1.Dispose(context.Background()) }()
	id := idOf(t, s1)
	if out, err := s1.Prompt(ctx, "Remember the codeword PELICAN-42. Reply with just the word: noted", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	wait(s1, 1)
	if err := s1.Dispose(ctx); err != nil {
		t.Fatal(err)
	}

	s2, err := open(ctx, spec, oarlock.Options{Cwd: cwd, Resume: id})
	if err != nil {
		t.Fatalf("resume %s: %v", id, err)
	}
	defer func() { _ = s2.Dispose(context.Background()) }()
	if got := idOf(t, s2); got != id {
		t.Fatalf("resumed id %q, want %q", got, id)
	}
	if first := recordsOf(s2)[0]; first.Seq != 1 {
		t.Fatalf("resumed stream starts at Seq %d, want 1", first.Seq)
	}
	for _, r := range recordsOf(s2) {
		if len(r.Events) != 0 || r.Kind != oarlock.KindFrame {
			t.Fatalf("resume synthesized %+v before the host prompted", r)
		}
	}
	if out, err := s2.Prompt(ctx, "What was the codeword I told you to remember? Answer with just the codeword.", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("follow-up = %+v, %v", out, err)
	}
	wait(s2, 1)
	if ans := lastText(s2); !strings.Contains(ans, "PELICAN-42") {
		t.Fatalf("the follow-up answered %q; want it to contain PELICAN-42 (context did not survive the resume)", ans)
	}
	if err := s2.Dispose(ctx); err != nil {
		t.Fatal(err)
	}

	// A bogus id in the same cwd is a typed not-found, and no pi is started for it.
	_, err = open(ctx, spec, oarlock.Options{Cwd: cwd, Resume: "no-such-session-00000"})
	if _, ok := asSessionNotFound(err); !ok {
		t.Fatalf("bogus id = %v, want SessionNotFound", err)
	}
}
