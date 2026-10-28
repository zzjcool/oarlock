package pi

// Resume resolution for Pi (issue #12): Options.Resume → one session file.
//
// Pi has no session-id resume command. A session is a FILE
// (<agentDir>/sessions/--<cwd>--/<timestamp>_<id>.jsonl) that `pi --session
// <path>` opens, so resuming by id is "scan the directory of this cwd, match
// the header id, hand Pi the path". Everything below was measured against pi
// 1.4.0 (docs/runtimes/pi.md §12):
//
//   - `--session <bare id>` from another cwd makes Pi ask "Fork this session
//     into current directory? [y/N]" on stdin and exit with no frame at all on
//     EOF. oarlock therefore never passes an id: it always passes an absolute
//     file path, which Pi takes through its path branch.
//   - `--session <path that does not exist>` does NOT fail: Pi silently starts
//     a brand-new session bound to that path. Likewise `--session-id <unknown>`
//     only warns on stderr and creates one. So the file is checked here, before
//     any process exists, and open.go verifies the id Pi reports afterwards.
//   - `--session <file recorded under cwd A>` run from cwd B keeps working in A
//     (a shell tool printed A). Pi would silently run somewhere the host did not
//     ask for, so a header cwd that is not Options.Cwd is refused, never run.
//   - A session file only exists once the session holds a user or assistant
//     message, so the id of a session that never got one is genuinely absent.
//
// What may be called SessionNotFound is deliberately narrow (errors.md: "with
// positive evidence"): a path that does not exist, a session directory that does
// not exist, or a COMPLETE scan of the directory that found no such id. Every
// other failure to look (permissions, I/O, a directory where a file should be, a
// file that is not a session, a header too large to read) is an ordinary error:
// absence was not shown, and a host that read it as "gone" might start a fresh
// conversation over a recoverable one. Pi's own discovery skips such files; this
// code refuses to treat them as proof.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/zzjcool/oarlock"
)

const (
	// envAgentDir is the variable Pi reads for its configuration home
	// (config.js ENV_AGENT_DIR).
	envAgentDir = "PI_CODING_AGENT_DIR"

	// headerScanLimit bounds how much of a file is read to find its header. It is
	// Pi's own MAX_SESSION_HEADER_SCAN_BYTES.
	headerScanLimit = 1 << 20

	// optionCwd names Options.Cwd in an UnsupportedOption. The frozen refusable
	// list (capabilities.go) has no constant for it; the spelling is the field's,
	// as the other options are, and OAR uses the same word for the same refusal.
	optionCwd oarlock.Option = "cwd"
)

var (
	errNotSession = errors.New("not a pi session file")
	errEmptyFile  = errors.New("empty file")
	errIsDir      = errors.New("is a directory")
)

// ResumeTarget is the session file Pi will be told to open.
type ResumeTarget struct {
	// SessionID is the native id from the file's header: what get_state must
	// report after the launch.
	SessionID string
	// File is the absolute session file.
	File string
	// Cwd is the working directory recorded in the header; "" if it has none.
	Cwd string
}

// LookupError is the Cause of a SessionNotFound: what was looked at, so a host
// can tell "wrong cwd" from "never persisted". Err is the native error when the
// path or directory was missing (errors.Is(err, fs.ErrNotExist) then holds
// through SessionNotFound and LookupError) and nil after a complete scan.
type LookupError struct {
	Cwd        string // the cwd whose session directory was searched
	SessionDir string // the directory scanned (by id), "" for a file path
	Path       string // the file looked at (by path), "" for an id
	Scanned    int    // session headers read in the complete scan
	Err        error
}

func (e *LookupError) Error() string {
	switch {
	case e.Path != "":
		return fmt.Sprintf("pi session file %s: %v", e.Path, e.Err)
	case e.Err != nil:
		return fmt.Sprintf("pi session directory %s (cwd %s): %v", e.SessionDir, e.Cwd, e.Err)
	default:
		return fmt.Sprintf("no session file with that id under %s (cwd %s; %d session(s) there; pi writes a session file with its first message)",
			e.SessionDir, e.Cwd, e.Scanned)
	}
}

// Unwrap returns the native error, if any.
func (e *LookupError) Unwrap() error { return e.Err }

// AgentDir is Pi's configuration home for env (KEY=VALUE entries, the last
// entry of a key wins): PI_CODING_AGENT_DIR when it is non-empty (a leading ~ is
// expanded), else <home>/.pi/agent. The home is HOME, then USERPROFILE (the
// other way round on Windows, where Pi's runtime prefers USERPROFILE), then
// os.UserHomeDir. The result is absolute; a relative PI_CODING_AGENT_DIR is
// resolved against this process's working directory, and open.go hands the
// absolute result to the child, so both sides agree.
func AgentDir(env []string) (string, error) {
	home := func() (string, error) {
		keys := []string{"HOME", "USERPROFILE"}
		if runtime.GOOS == "windows" {
			keys = []string{"USERPROFILE", "HOME"}
		}
		for _, k := range keys {
			if v := lookupEnv(env, k); v != "" {
				return v, nil
			}
		}
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the home directory for %s: %w", envAgentDir, err)
		}
		return h, nil
	}
	if dir := lookupEnv(env, envAgentDir); dir != "" {
		if dir == "~" || strings.HasPrefix(dir, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(dir, `~\`)) {
			h, err := home()
			if err != nil {
				return "", err
			}
			if dir == "~" {
				dir = h
			} else {
				dir = filepath.Join(h, dir[2:])
			}
		}
		return absClean(dir), nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return absClean(filepath.Join(h, ".pi", "agent")), nil
}

// SessionDir is the directory Pi keeps the sessions of cwd in: a port of
// getDefaultSessionDirPath (session-manager.js, pi 1.4.0). The cwd is made
// absolute, ONE leading separator is dropped, every / \ : becomes -, and the
// result is wrapped in -- -- under <agentDir>/sessions. The encoding is lossy
// (/a/b-c and /a-b/c share a directory), which is why a found file's header cwd
// is still checked.
func SessionDir(cwd, agentDir string) string {
	c := absClean(cwd)
	if c != "" && (c[0] == '/' || c[0] == '\\') {
		c = c[1:]
	}
	enc := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':':
			return '-'
		}
		return r
	}, c)
	return filepath.Join(absClean(agentDir), "sessions", "--"+enc+"--")
}

// EnvFor returns base with set applied: a non-nil value sets the variable, a nil
// value removes it. Entries of base that set does not name are kept in order,
// duplicates included (the last still wins); base is not modified. The additions
// follow in key order so the result is deterministic.
func EnvFor(base []string, set map[string]*string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		if k, _, ok := splitEnv(kv); ok && inSet(set, k) {
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

// ResolveResume turns Options.Resume into the session file Pi will open.
//
// resume is a path when it contains a separator or ends in ".jsonl" (Pi's own
// test, main.js resolveSessionPath); a relative path is relative to cwd. Anything
// else is a session id, matched exactly (no prefix match) against the header id
// of the *.jsonl files in SessionDir(cwd, agentDir) and nowhere else.
//
// Errors, by what they prove:
//   - *oarlock.SessionNotFound: positive evidence only (see the file comment).
//   - *oarlock.UnsupportedOption{Option: "cwd"}: the session exists but was
//     recorded under another directory, and Pi would keep running there.
//   - anything else: the lookup could not be completed or the file is not
//     usable; absence was not shown.
//
// An empty resume returns the zero ResumeTarget and nil.
func ResolveResume(resume, cwd, agentDir string) (ResumeTarget, error) {
	if resume == "" {
		return ResumeTarget{}, nil
	}
	abs := absClean(cwd)
	if strings.ContainsAny(resume, `/\`) || strings.HasSuffix(resume, ".jsonl") {
		return resolveFile(resume, abs)
	}
	return resolveID(resume, abs, agentDir)
}

func resolveFile(resume, cwd string) (ResumeTarget, error) {
	path := resume
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	h, err := readHeader(path)
	if err != nil {
		if isGone(err) {
			return ResumeTarget{}, notFound(resume, &LookupError{Cwd: cwd, Path: path, Err: err})
		}
		return ResumeTarget{}, fmt.Errorf("pi: resume %q: %w", resume, err)
	}
	return checkCwd(path, h, cwd)
}

func resolveID(id, cwd, agentDir string) (ResumeTarget, error) {
	dir := SessionDir(cwd, agentDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ResumeTarget{}, notFound(id, &LookupError{Cwd: cwd, SessionDir: dir, Err: err})
		}
		return ResumeTarget{}, fmt.Errorf("pi: resume %q: cannot read the session directory, so absence cannot be proven: %w", id, err)
	}
	type hit struct {
		path string
		h    sessionHeader
	}
	var (
		hits       []hit
		unreadable error // the first file whose header could not be read
		scanned    int
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		h, err := readHeader(path)
		switch {
		case err == nil:
			scanned++
			if h.ID == id {
				hits = append(hits, hit{path, h})
			}
		case errors.Is(err, errEmptyFile), errors.Is(err, fs.ErrNotExist):
			// Nothing to resume, or gone since the listing.
		case unreadable == nil:
			unreadable = err
		}
	}
	switch {
	case len(hits) == 1:
		// A match is positive evidence by itself; an unreadable neighbour only
		// blocks the verdict "absent".
		return checkCwd(hits[0].path, hits[0].h, cwd)
	case len(hits) > 1:
		paths := make([]string, len(hits))
		for i, h := range hits {
			paths[i] = h.path
		}
		return ResumeTarget{}, fmt.Errorf("pi: resume %q is ambiguous: %d session files carry that id: %s", id, len(hits), strings.Join(paths, ", "))
	case unreadable != nil:
		return ResumeTarget{}, fmt.Errorf("pi: resume %q: a session file in %s could not be read, so absence cannot be proven: %w", id, dir, unreadable)
	}
	return ResumeTarget{}, notFound(id, &LookupError{Cwd: cwd, SessionDir: dir, Scanned: scanned})
}

// checkCwd refuses a session that Pi would run in a directory other than cwd. A
// header without a cwd is accepted (nothing to contradict).
func checkCwd(path string, h sessionHeader, cwd string) (ResumeTarget, error) {
	if h.Cwd != "" && !sameDir(h.Cwd, cwd) {
		return ResumeTarget{}, &oarlock.UnsupportedOption{
			Option: optionCwd,
			Reason: fmt.Sprintf("pi session %s was recorded under %q and Pi would keep running there; Options.Cwd is %q. Resume with the recorded cwd, or start a new session", h.ID, h.Cwd, cwd),
		}
	}
	return ResumeTarget{SessionID: h.ID, File: path, Cwd: h.Cwd}, nil
}

func notFound(id string, cause *LookupError) error {
	return &oarlock.SessionNotFound{SessionID: id, Cause: cause}
}

// isGone reports positive evidence that a path names nothing: it does not exist,
// or a parent component is not a directory.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

type sessionHeader struct {
	ID  string
	Cwd string
}

// readHeader reads the session header: the first non-blank line, which must be a
// JSON object {"type":"session","id":<string>,...}. An empty file is
// errEmptyFile, anything else that is not a header errNotSession.
func readHeader(path string) (sessionHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return sessionHeader{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return sessionHeader{}, err
	}
	if fi.IsDir() {
		return sessionHeader{}, &fs.PathError{Op: "read", Path: path, Err: errIsDir}
	}
	br := bufio.NewReaderSize(io.LimitReader(f, headerScanLimit+1), 4096)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if len(line) > headerScanLimit {
				return sessionHeader{}, fmt.Errorf("%s: header line exceeds %d bytes: %w", path, headerScanLimit, errNotSession)
			}
			var v struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Cwd  any    `json:"cwd"`
			}
			if json.Unmarshal(line, &v) != nil || v.Type != "session" || v.ID == "" {
				return sessionHeader{}, fmt.Errorf("%s: %w", path, errNotSession)
			}
			cwd, _ := v.Cwd.(string)
			return sessionHeader{ID: v.ID, Cwd: cwd}, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sessionHeader{}, fmt.Errorf("%s: %w", path, errEmptyFile)
			}
			return sessionHeader{}, err
		}
	}
}

// sameDir reports whether a and b are one directory: equal after cleaning, or
// the same file system object (symlinks, case-insensitive volumes). Pi stores
// process.cwd(), which is the physical path, so a symlinked Options.Cwd must
// still match the header it produced.
func sameDir(a, b string) bool {
	a, b = absClean(a), absClean(b)
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func absClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// splitEnv splits KEY=VALUE. A leading "=" belongs to the key (Windows keeps
// per-drive entries such as "=C:=C:\x").
func splitEnv(kv string) (key, val string, ok bool) {
	if kv == "" {
		return "", "", false
	}
	i := strings.IndexByte(kv[1:], '=')
	if i < 0 {
		return "", "", false
	}
	return kv[:i+1], kv[i+2:], true
}

func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// lookupEnv is the value of key in env; the last entry wins.
func lookupEnv(env []string, key string) (val string) {
	for _, kv := range env {
		if k, v, ok := splitEnv(kv); ok && envKeyEqual(k, key) {
			val = v
		}
	}
	return val
}

func inSet(set map[string]*string, key string) bool {
	for k := range set {
		if envKeyEqual(k, key) {
			return true
		}
	}
	return false
}
