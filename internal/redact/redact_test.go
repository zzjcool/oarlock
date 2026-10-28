package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
)

func TestNameIsCredential_Table(t *testing.T) {
	protected := []string{
		"API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MYSQL_PWD",
		"DB_PASSWORD", "CLIENT_SECRET", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"PGPASSWORD", "apiKey", "X_API_KEY", "SOME_PASSWD", "SSH_PASSPHRASE",
		"SESSION_COOKIE", "SERVICE_CREDENTIAL", "SERVICE_CREDENTIALS", "PWD",
		"lowercase_secret", "my_password", "A_B_CREDENTIAL",
	}
	for _, key := range protected {
		if !NameIsCredential(key) {
			t.Errorf("NameIsCredential(%q) = false, want true", key)
		}
	}

	readable := []string{
		"SSH_AUTH_SOCK", "GIT_AUTHOR_EMAIL", "PASSWORD_STORE_DIR",
		"GNUPG_PASSPHRASE_FILE", "KEYTIMEOUT", "AWS_ACCESS_KEY_ID", "HOME",
		"PATH", "PORT", "MODEL", "PGPASSWORD_X", "API_KEY_FILE", "TOKENIZER",
	}
	for _, key := range readable {
		if NameIsCredential(key) {
			t.Errorf("NameIsCredential(%q) = true, want false", key)
		}
	}
}

func TestHeaderIsCredential_Table(t *testing.T) {
	protected := []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "Api-Key", "X-Auth-Token", "X-Client-Secret"}
	for _, key := range protected {
		if !HeaderIsCredential(key) {
			t.Errorf("HeaderIsCredential(%q) = false, want true", key)
		}
	}
	readable := []string{"Content-Type", "Accept", "X-Trace-Id", "User-Agent", "X-Request-Id"}
	for _, key := range readable {
		if HeaderIsCredential(key) {
			t.Errorf("HeaderIsCredential(%q) = true, want false", key)
		}
	}
}

func TestSelectValue_Table(t *testing.T) {
	selected := []string{
		"hunter2hunter2", "abcdefgh", "sk-test-abc123", "postgres://u:p@h/db",
		"x y z 123", "ghp_test_1234567890", "averylongtoken",
	}
	for _, value := range selected {
		if !SelectValue(value) {
			t.Errorf("SelectValue(%q) = false, want true", value)
		}
	}
	rejected := []string{
		"", "short", "1234567", "/", "/etc/passwd", "~/secret",
		`C:\Users\test`, `C:/Users/test`, "abcdefg",
	}
	for _, value := range rejected {
		if SelectValue(value) {
			t.Errorf("SelectValue(%q) = true, want false", value)
		}
	}
}

func TestText_ReplacesLongestFirst(t *testing.T) {
	// The short value is a prefix of the long one. Replacing the long value
	// first consumes the whole thing; replacing the short one first would
	// leave a suffix behind.
	r := New([]string{"sk-test-abcdef", "sk-test-abcdef123"})
	got, changed := r.Text("prefix sk-test-abcdef123 suffix")
	if !changed {
		t.Fatal("no replacement happened")
	}
	if got != "prefix [redacted] suffix" {
		t.Fatalf("got %q; the shorter prefix was replaced first", got)
	}
}

func TestText_NoKnownValuesReturnsTheSameStringWithoutChange(t *testing.T) {
	const s = "nothing secret here"
	got, changed := New(nil).Text(s)
	if changed || got != s {
		t.Fatalf("unexpected change: changed=%v got=%q", changed, got)
	}
}

func TestAny_EnvAndHeaders(t *testing.T) {
	const apiKey = "test-api-key-0001"
	const bearer = "Bearer dummy-token-0001"
	const cookie = "session=session-value-0001"

	r := New(nil)
	in := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  apiKey,
			"SSH_AUTH_SOCK":      "/tmp/ssh.sock",
			"HOME":               "/root",
			"PASSWORD_STORE_DIR": "/root/.password-store",
			"PATH":               "/usr/bin",
		},
		"headers": map[string]any{
			"Authorization": bearer,
			"X-Api-Key":     "short",
			"Content-Type":  "application/json",
			"Cookie":        cookie,
		},
	}

	got, changed := r.Any(in)
	if !changed {
		t.Fatal("nothing was redacted")
	}
	out := got.(map[string]any)
	env := out["env"].(map[string]any)
	if env["ANTHROPIC_API_KEY"] != RedactedPlaceholder {
		t.Fatalf("API key not redacted: %v", env["ANTHROPIC_API_KEY"])
	}
	for _, safe := range []string{"SSH_AUTH_SOCK", "HOME", "PASSWORD_STORE_DIR", "PATH"} {
		if env[safe] == RedactedPlaceholder {
			t.Fatalf("over-redacted %s", safe)
		}
	}
	headers := out["headers"].(map[string]any)
	if headers["Authorization"] != RedactedPlaceholder {
		t.Fatalf("Authorization not redacted: %v", headers["Authorization"])
	}
	if headers["Cookie"] != RedactedPlaceholder {
		t.Fatalf("Cookie not redacted: %v", headers["Cookie"])
	}
	if headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type was over-redacted: %v", headers["Content-Type"])
	}
	if headers["X-Api-Key"] != "short" {
		t.Fatalf("a sub-eight-character value was replaced: %v", headers["X-Api-Key"])
	}
}

func TestAny_AuthorizationSuffixIsProtectedElsewhere(t *testing.T) {
	const token = "dummy-token-0001"
	in := map[string]any{
		"headers": map[string]any{"Authorization": "Bearer " + token},
		"echo":    token,
	}
	got, changed := New(nil).Any(in)
	if !changed {
		t.Fatal("nothing was redacted")
	}
	out := got.(map[string]any)
	if out["echo"] != RedactedPlaceholder {
		t.Fatalf("the bare token echoed elsewhere survived: %v", out["echo"])
	}
}

func TestAny_PasswordBearingURL(t *testing.T) {
	const password = "p@ssw0rd123"
	const rawURL = "postgres://user:p%40ssw0rd123@db/app"
	in := map[string]any{
		"url":  rawURL,
		"note": password,
		"safe": "https://example.com/path",
	}
	got, changed := New(nil).Any(in)
	if !changed {
		t.Fatal("nothing was redacted")
	}
	out := got.(map[string]any)
	if out["url"] != RedactedPlaceholder {
		t.Fatalf("URL not redacted: %v", out["url"])
	}
	if out["note"] != RedactedPlaceholder {
		t.Fatalf("the URL password survived outside the URL: %v", out["note"])
	}
	if out["safe"] != "https://example.com/path" {
		t.Fatalf("a password-free URL was over-redacted: %v", out["safe"])
	}
}

func TestAny_DoesNotMutateTheInput(t *testing.T) {
	in := map[string]any{"env": map[string]any{"API_KEY": "abcdefgh"}}
	_, changed := New(nil).Any(in)
	if !changed {
		t.Fatal("nothing was redacted")
	}
	if in["env"].(map[string]any)["API_KEY"] != "abcdefgh" {
		t.Fatal("Any mutated the caller's map")
	}
}

func TestBytes_NoChangeReturnsTheOriginalSlice(t *testing.T) {
	raw := []byte(`{"env":{"HOME":"/root"}}`)
	out, changed := New(nil).Bytes(raw)
	if changed {
		t.Fatalf("unexpected change: %s", out)
	}
	if &out[0] != &raw[0] {
		t.Fatal("no-change path copied the slice")
	}
}

func TestBytes_InvalidJSONFallsBackToText(t *testing.T) {
	raw := []byte(`not json but contains dummy-token-0001`)
	out, changed := New([]string{"dummy-token-0001"}).Bytes(raw)
	if !changed {
		t.Fatal("a known value survived invalid JSON")
	}
	if strings.Contains(string(out), "dummy-token-0001") {
		t.Fatalf("secret survived: %s", out)
	}
}

func TestRecord_RedactsNativeAndLeavesTheEnvelopeAlone(t *testing.T) {
	const secret = "dummy-token-0001"
	rec := oarlock.Record{
		Seq:       9,
		Kind:      oarlock.KindFrame,
		SessionID: "s1",
		Native:    json.RawMessage(`{"env":{"API_KEY":"` + secret + `"},"text":"` + secret + `"}`),
		Events:    []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: secret}},
	}

	out, changed := New([]string{secret}).Record(rec)
	if !changed {
		t.Fatal("nothing was redacted")
	}
	if out.Seq != 9 || out.Kind != oarlock.KindFrame || out.SessionID != "s1" {
		t.Fatalf("envelope changed: %+v", out)
	}
	if strings.Contains(string(out.Native), secret) {
		t.Fatalf("native still contains the secret: %s", out.Native)
	}
	if len(out.Events) != 1 || out.Events[0].Text != RedactedPlaceholder {
		t.Fatalf("event text not redacted: %+v", out.Events)
	}
}

func TestRecord_NoCredentialLeavesTheRecordByteIdentical(t *testing.T) {
	rec := oarlock.Record{
		Seq:    1,
		Kind:   oarlock.KindFrame,
		Native: json.RawMessage(`{"type":"agent_start"}`),
	}
	out, changed := New(nil).Record(rec)
	if changed {
		t.Fatalf("unexpected change: %+v", out)
	}
	if string(out.Native) != string(rec.Native) {
		t.Fatalf("native changed: %s", out.Native)
	}
}

func TestError_RedactsMessageAndCauseAndKeepsErrorsIs(t *testing.T) {
	const secret = "dummy-token-0001"
	cause := errors.New("provider rejected " + secret)
	err := fmt.Errorf("auth failed: %w", cause)

	out := New([]string{secret}).Error(err)
	if strings.Contains(out.Error(), secret) {
		t.Fatalf("secret survived: %v", out)
	}
	if !errors.Is(out, cause) {
		t.Fatal("redaction broke the errors.Is chain")
	}
}

func TestError_NoChangeReturnsTheOriginalError(t *testing.T) {
	err := errors.New("plain failure")
	if got := New(nil).Error(err); got != err {
		t.Fatalf("no-change path replaced the error: %v", got)
	}
}

func TestNew_IgnoresUnselectableValues(t *testing.T) {
	r := New([]string{"short", "/", "~/path", `C:\x`, "longenough"})
	if len(r.secrets) != 1 || r.secrets[0] != "longenough" {
		t.Fatalf("unexpected secret set: %#v", r.secrets)
	}
}

func TestNew_ExpandsPasswordBearingURLs(t *testing.T) {
	const encoded = "p%40ssw0rd123"
	const rawURL = "postgres://user:" + encoded + "@db/app"
	r := New([]string{rawURL})
	joined := strings.Join(r.secrets, "\n")

	if !strings.Contains(joined, rawURL) {
		t.Fatalf("the whole URL was not protected: %#v", r.secrets)
	}
	if !strings.Contains(joined, encoded) {
		t.Fatalf("the encoded password was not protected: %#v", r.secrets)
	}
	if !strings.Contains(joined, "p@ssw0rd123") {
		t.Fatalf("the decoded password was not protected: %#v", r.secrets)
	}
}
