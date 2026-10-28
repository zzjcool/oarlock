package voyage_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/voyage"
)

func fxHeader() voyage.Header {
	return voyage.Header{Runtime: "pi", Cwd: ".", SessionID: "sess-fx", StartedAt: 1, Recorder: "fixture_test"}
}

func fxFrame(seq uint64, native string) oarlock.Record {
	return oarlock.Record{
		Seq:        seq,
		ReceivedAt: time.UnixMilli(1000 + int64(seq)).UTC(),
		Kind:       oarlock.KindFrame,
		SessionID:  "sess-fx",
		Native:     json.RawMessage(native),
	}
}

// fxWrite writes a complete, deterministic log named name.jsonl in dir with one
// frame whose native payload is pad, and returns its path.
func fxWrite(t *testing.T, dir, name, pad string) string {
	t.Helper()
	path := filepath.Join(dir, name+".jsonl")
	w, err := voyage.Create(path, fxHeader(), nil)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	if err := w.Append(fxFrame(1, `{"type":"x","pad":"`+pad+`"}`)); err != nil {
		t.Fatalf("Append %s: %v", name, err)
	}
	if err := w.EndAt("settled", time.UnixMilli(5000)); err != nil {
		t.Fatalf("EndAt %s: %v", name, err)
	}
	return path
}

func fxRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fxGlob(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCommittedFixturesPassTheCredentialScanAndSizeCap(t *testing.T) {
	t.Run("testdata/pi", func(t *testing.T) {
		files := fxGlob(t, filepath.Join("..", "..", "testdata", "pi", "*.jsonl"))
		if len(files) == 0 {
			t.Fatal("no testdata/pi/*.jsonl found: the Pi wire frames the golden fixtures are built from are missing")
		}
		for _, f := range files {
			if found := voyage.ScanCredentials(fxRead(t, f)); len(found) > 0 {
				t.Errorf("%s: %d credential finding(s), first: %+v", filepath.Base(f), len(found), found[0])
			}
		}
	})
	t.Run("testdata/voyage", func(t *testing.T) {
		files := fxGlob(t, filepath.Join("..", "..", "testdata", "voyage", "*.jsonl"))
		if len(files) == 0 {
			t.Skip("testdata/voyage has no .jsonl fixture yet; run `go generate ./internal/voyage`")
		}
		for _, f := range files {
			if err := voyage.CheckFixture(f); err != nil {
				t.Errorf("%s: %v", filepath.Base(f), err)
			}
		}
	})
}

func TestCommittedIndexIsCurrent(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "voyage")
	if len(fxGlob(t, filepath.Join(dir, "*.jsonl"))) == 0 {
		t.Skip("testdata/voyage has no .jsonl fixture yet; run `go generate ./internal/voyage`")
	}
	if err := voyage.VerifyIndex(dir); err != nil {
		t.Fatal(err)
	}
}

// fxSecret builds a credential-shaped string at run time, so this file never
// contains one as a literal.
func fxSecret(parts ...string) string { return strings.Join(parts, "") }

func TestScanCredentialsPatternsFire(t *testing.T) {
	cases := []struct {
		pattern string
		text    string
	}{
		{"openai-anthropic-key", fxSecret("sk", "-live-", strings.Repeat("a", 20))},
		{"openai-anthropic-key", fxSecret("sk", "-ant-", "api03-", strings.Repeat("b", 24))},
		{"openai-anthropic-key", `{"text":"line\n` + fxSecret("sk", "-live-", strings.Repeat("c", 20)) + `"}`},
		{"openai-anthropic-key", `{"env":"` + fxSecret("sk", "-live-", strings.Repeat("d", 20)) + `"}`},
		{"github-token", fxSecret("gh", "p_", strings.Repeat("A", 24))},
		{"github-token", fxSecret("gh", "o_", strings.Repeat("B", 24))},
		{"github-token", fxSecret("gh", "u_", strings.Repeat("C", 24))},
		{"github-token", fxSecret("gh", "s_", strings.Repeat("D", 24))},
		{"github-token", fxSecret("gh", "r_", strings.Repeat("E", 24))},
		{"github-fine-grained-token", fxSecret("github", "_pat_", strings.Repeat("c", 30))},
		{"aws-access-key-id", fxSecret("AK", "IA", strings.Repeat("Q", 16))},
		{"google-api-key", fxSecret("AI", "za", strings.Repeat("d", 35))},
		{"slack-token", fxSecret("xo", "xb-", strings.Repeat("1", 12))},
		{"pem-private-key", fxSecret("-----BEGIN ", "RSA PRIVATE", " KEY-----")},
		{"pem-private-key", fxSecret("-----BEGIN ", "PRIVATE", " KEY-----")},
		{"bearer-token", fxSecret("Bearer", " ", strings.Repeat("t", 24))},
		{"jwt", fxSecret("ey", "Jhbgcioijiuzi1nij9", ".", "ey", "Jzdwiioixmjm0nty3odkwin0", ".", "abc123_-sig")},
		{"url-password", fxSecret("https://user:", "hunter2pw", "@example.com/x")},
		{"redacted-adjacent", fxSecret("abc", "[redacted]")},
		{"redacted-adjacent", fxSecret("[redacted]", "xyz")},
		{"home-path", `{"cwd":"` + fxSecret("/ho", "me/alice/project") + `"}`},
		{"home-path", fxSecret("/ro", "ot/work")},
		{"home-path", fxSecret("see /Us", "ers/bob/x")},
		{"home-path", `{"text":"line\n` + fxSecret("/ho", "me/alice/x") + `"}`},
	}
	for _, c := range cases {
		found := voyage.ScanCredentials([]byte(c.text))
		var hit *voyage.Finding
		for i := range found {
			if found[i].Pattern == c.pattern {
				hit = &found[i]
			}
		}
		if hit == nil {
			t.Errorf("pattern %q did not fire on %d-byte input; findings: %+v", c.pattern, len(c.text), found)
			continue
		}
		if len(hit.Excerpt) > 40 {
			t.Errorf("pattern %q excerpt is %d bytes, want <= 40", c.pattern, len(hit.Excerpt))
		}
		if c.pattern != "home-path" && c.pattern != "pem-private-key" && c.pattern != "redacted-adjacent" && strings.Contains(hit.Excerpt, c.text) {
			t.Errorf("pattern %q excerpt echoes the whole credential", c.pattern)
		}
	}
}

func TestScanCredentialsNegatives(t *testing.T) {
	for _, text := range []string{
		`"[redacted]"`,
		`[redacted]`,
		`{"Authorization":"Bearer [redacted]"}`,
		`{"env":{"API_KEY":"[redacted]"}}`,
		`https://user:[redacted]@example.com/x`,
		`$HOME/.pi/agent/sessions`,
		`SSH_AUTH_SOCK`,
		`GIT_AUTHOR_EMAIL`,
		`123e4567-e89b-12d3-a456-426614174000`,
		`sk-short`,
		`task-` + strings.Repeat("a", 30),
		`the password is stored elsewhere; the apiKey and the secret are not shown`,
		`{"text":"line\n[redacted]"}`,
		`https://example.com/docs`,
		`/usr/local/bin and /tmp/x and ./relative/home/x`,
		`/homework/assignment`,
	} {
		if found := voyage.ScanCredentials([]byte(text)); len(found) != 0 {
			t.Errorf("false positive on %q: %+v", text, found)
		}
	}
}

func TestScanCredentialsReportsOneBasedLines(t *testing.T) {
	secret := fxSecret("sk", "-live-", strings.Repeat("z", 20))
	data := []byte("clean\n\nsecond " + secret + " and again " + secret + "\nclean\n" + secret)
	found := voyage.ScanCredentials(data)
	var lines []int
	for _, f := range found {
		lines = append(lines, f.Line)
	}
	if want := []int{3, 3, 5}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("finding lines = %v, want %v (1-based)", lines, want)
	}
	if got := voyage.ScanCredentials(nil); len(got) != 0 {
		t.Fatalf("empty input produced findings: %+v", got)
	}
}

func TestCheckFixtureAcceptsACompleteCleanLog(t *testing.T) {
	path := fxWrite(t, t.TempDir(), "ok", "hello")
	if err := voyage.CheckFixture(path); err != nil {
		t.Fatal(err)
	}
	if err := voyage.CheckFixture(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("a missing file passed CheckFixture")
	}
}

func TestCheckFixtureRejectsAnOversizeFile(t *testing.T) {
	path := fxWrite(t, t.TempDir(), "big", strings.Repeat("a", voyage.MaxFixtureBytes+1))
	err := voyage.CheckFixture(path)
	if err == nil {
		t.Fatal("an oversize fixture passed CheckFixture")
	}
	if !strings.Contains(err.Error(), "big.jsonl") || !strings.Contains(err.Error(), "MaxFixtureBytes") {
		t.Fatalf("error should name the file and the cap: %v", err)
	}
}

func TestCheckFixtureRejectsATruncatedFixture(t *testing.T) {
	path := fxWrite(t, t.TempDir(), "cut", "x")
	data := fxRead(t, path)
	body := bytes.TrimSuffix(data, []byte("\n"))
	cut := body[:bytes.LastIndexByte(body, '\n')+1] // drop the end line
	if err := os.WriteFile(path, cut, 0o644); err != nil {
		t.Fatal(err)
	}
	err := voyage.CheckFixture(path)
	if !errors.Is(err, voyage.ErrTruncated) {
		t.Fatalf("a fixture without an end line: err = %v, want ErrTruncated", err)
	}
}

func TestCheckFixtureRejectsALeakedCredentialWithoutEchoingIt(t *testing.T) {
	path := fxWrite(t, t.TempDir(), "leaky", "MARKER")
	secret := fxSecret("sk", "-live-", strings.Repeat("q", 40))
	// The Writer redacts structurally, so build the leak from raw bytes.
	leaky := bytes.Replace(fxRead(t, path), []byte("MARKER"), []byte(secret), 1)
	if err := os.WriteFile(path, leaky, 0o644); err != nil {
		t.Fatal(err)
	}
	err := voyage.CheckFixture(path)
	if err == nil {
		t.Fatal("a fixture holding a credential passed CheckFixture")
	}
	msg := err.Error()
	if !strings.Contains(msg, "leaky.jsonl") || !strings.Contains(msg, "openai-anthropic-key") || !strings.Contains(msg, "line 2") {
		t.Fatalf("error should name the file, the pattern and the line: %v", err)
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, secret[:30]) {
		t.Fatalf("error echoes the credential: %v", err)
	}
}

func TestBuildIndexIsDeterministicAndSorted(t *testing.T) {
	a := t.TempDir()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		fxWrite(t, a, n, n)
	}
	b := t.TempDir()
	for _, n := range []string{"mid", "zeta", "alpha"} {
		fxWrite(t, b, n, n)
	}
	// Files that must be ignored.
	if err := os.WriteFile(filepath.Join(a, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a, "sub.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := voyage.BuildIndex(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := voyage.BuildIndex(a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := voyage.BuildIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("BuildIndex is not repeatable")
	}
	if !reflect.DeepEqual(first, other) {
		t.Fatal("BuildIndex depends on creation order")
	}
	var names []string
	for _, e := range first.Fixtures {
		names = append(names, e.Name)
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if first.Format != voyage.IndexFormat {
		t.Fatalf("format = %q", first.Format)
	}
	e := first.Fixtures[0]
	if e.File != "alpha.jsonl" || e.Runtime != "pi" || e.SessionID != "sess-fx" || e.EndReason != "settled" ||
		!e.Complete || e.Records != 1 || e.Frames != 1 || e.Requests != 0 || e.Responses != 0 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Bytes != int64(len(fxRead(t, filepath.Join(a, "alpha.jsonl")))) || len(e.SHA256) != 64 {
		t.Fatalf("entry bytes/sha = %d/%q", e.Bytes, e.SHA256)
	}
}

func TestBuildIndexCountsRecordKindsAndMarksTruncatedLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kinds.jsonl")
	w, err := voyage.Create(path, fxHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := oarlock.Record{
		Seq: 2, ReceivedAt: time.UnixMilli(1002).UTC(), Kind: oarlock.KindRequest, SessionID: "sess-fx",
		ID: "req-2", Direction: oarlock.ToRuntime, Request: &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"},
	}
	resp := oarlock.Record{
		Seq: 3, ReceivedAt: time.UnixMilli(1003).UTC(), Kind: oarlock.KindResponse, SessionID: "sess-fx",
		RequestID: "req-2", Response: &oarlock.ResponseBody{Kind: oarlock.ResponseAccepted},
	}
	for _, r := range []oarlock.Record{fxFrame(1, `{"type":"x"}`), req, resp, fxFrame(4, `{"type":"y"}`)} {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.EndAt("settled", time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	idx, err := voyage.BuildIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := idx.Fixtures[0]
	if e.Records != 4 || e.Frames != 2 || e.Requests != 1 || e.Responses != 1 {
		t.Fatalf("counts = %+v", e)
	}

	// The same log without its end line is listed, but not Complete.
	data := fxRead(t, path)
	body := bytes.TrimSuffix(data, []byte("\n"))
	if err := os.WriteFile(path, body[:bytes.LastIndexByte(body, '\n')+1], 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err = voyage.BuildIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := idx.Fixtures[0]; e.Complete || e.EndReason != "" || e.Records != 4 {
		t.Fatalf("truncated entry = %+v", e)
	}
}

func TestBuildIndexRejectsFixturesOverTheTotalCap(t *testing.T) {
	dir := t.TempDir()
	per := voyage.MaxFixtureBytes - 1024
	for i := 0; i <= voyage.MaxTotalBytes/per; i++ {
		fxWrite(t, dir, "f"+string(rune('a'+i)), strings.Repeat("a", per-512))
	}
	_, err := voyage.BuildIndex(dir)
	if err == nil || !strings.Contains(err.Error(), "MaxTotalBytes") {
		t.Fatalf("err = %v, want a MaxTotalBytes failure", err)
	}
}

func TestWriteIndexThenVerifyIndex(t *testing.T) {
	dir := t.TempDir()
	path := fxWrite(t, dir, "one", "p")
	fxWrite(t, dir, "two", "q")

	if err := voyage.WriteIndex(dir); err != nil {
		t.Fatal(err)
	}
	raw := fxRead(t, filepath.Join(dir, voyage.IndexName))
	if !bytes.HasSuffix(raw, []byte("}\n")) || !bytes.HasPrefix(raw, []byte("{\n  \"format\": \"oar-voyage-index/1\",\n")) {
		t.Fatalf("index is not 2-space indented JSON with a trailing LF:\n%s", raw)
	}
	var round voyage.Index
	if err := json.Unmarshal(raw, &round); err != nil || len(round.Fixtures) != 2 {
		t.Fatalf("index does not round-trip: %v %+v", err, round)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".index-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	if err := voyage.VerifyIndex(dir); err != nil {
		t.Fatalf("fresh index does not verify: %v", err)
	}
	if err := voyage.WriteIndex(dir); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Equal(raw, fxRead(t, filepath.Join(dir, voyage.IndexName))) {
		t.Fatal("WriteIndex is not idempotent")
	}

	// Change one byte of a fixture without changing its length.
	data := fxRead(t, path)
	mutated := bytes.Replace(data, []byte(`"type":"x"`), []byte(`"type":"y"`), 1)
	if bytes.Equal(data, mutated) {
		t.Fatal("test bug: nothing mutated")
	}
	if err := os.WriteFile(path, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	err := voyage.VerifyIndex(dir)
	if err == nil || !strings.Contains(err.Error(), "go generate ./internal/voyage") {
		t.Fatalf("VerifyIndex after a fixture edit: %v, want the go-generate hint", err)
	}

	// Regenerating repairs it.
	if err := voyage.WriteIndex(dir); err != nil {
		t.Fatal(err)
	}
	if err := voyage.VerifyIndex(dir); err != nil {
		t.Fatal(err)
	}

	// A new fixture makes the index stale.
	fxWrite(t, dir, "three", "r")
	if err := voyage.VerifyIndex(dir); err == nil || !strings.Contains(err.Error(), "go generate ./internal/voyage") {
		t.Fatalf("VerifyIndex with an unlisted fixture: %v", err)
	}
}

func TestVerifyIndexFailsWithoutAnIndex(t *testing.T) {
	dir := t.TempDir()
	fxWrite(t, dir, "one", "p")
	err := voyage.VerifyIndex(dir)
	if err == nil || !strings.Contains(err.Error(), "go generate ./internal/voyage") {
		t.Fatalf("VerifyIndex without index.json: %v", err)
	}
}

func TestWriteIndexRefusesAFailingFixture(t *testing.T) {
	dir := t.TempDir()
	fxWrite(t, dir, "good", "p")
	bad := fxWrite(t, dir, "bad", "MARKER")
	leaky := bytes.Replace(fxRead(t, bad), []byte("MARKER"), []byte(fxSecret("gh", "p_", strings.Repeat("Z", 30))), 1)
	if err := os.WriteFile(bad, leaky, 0o644); err != nil {
		t.Fatal(err)
	}
	err := voyage.WriteIndex(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.jsonl") {
		t.Fatalf("WriteIndex over a leaky fixture: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, voyage.IndexName)); !os.IsNotExist(statErr) {
		t.Fatalf("index.json was written despite the failing fixture (stat err %v)", statErr)
	}
}
