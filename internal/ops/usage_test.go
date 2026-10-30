package ops_test

// Tests for the account-usage seam (issue #89): the HTTP mapping table, the
// credential resolution rules and the redaction guarantee. The fakes are
// httptest servers and temp files, so nothing here touches a vendor, a network
// or the real credential store.
//
// The redaction cases are the important ones: an error, a log line or a test
// failure must never carry the token, because the host that prints a failure
// usually writes it to a file that outlives the process.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock/internal/ops"
)

const testToken = "not-a-real-token-0123456789"

// server answers every request with status and body and records what it saw.
type server struct {
	*httptest.Server
	requests int
	headers  http.Header
}

func newServer(t *testing.T, status int, body string) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests++
		s.headers = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestFetchUsageJSON_MapsEveryOutcome(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantKind   ops.UsageErrorKind
		wantReason string
		wantStatus int
	}{
		{"ok", http.StatusOK, `{"five_hour":{"utilization":12}}`, "", "", 0},
		{"unauthorized", http.StatusUnauthorized, `{"error":"invalid token"}`, ops.UsageKindReauth, ops.ReasonCredentialsRejected, http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden, ``, ops.UsageKindReauth, ops.ReasonCredentialsRejected, http.StatusForbidden},
		{"rate limited", http.StatusTooManyRequests, ``, ops.UsageKindRateLimited, ops.ReasonRequestRejected, http.StatusTooManyRequests},
		{"not found", http.StatusNotFound, ``, ops.UsageKindUnsupported, ops.ReasonEndpointUnavailable, http.StatusNotFound},
		{"server error", http.StatusInternalServerError, ``, ops.UsageKindTransport, "", http.StatusInternalServerError},
		{"empty body", http.StatusOK, ``, ops.UsageKindMalformed, ops.ReasonQuotaUnavailable, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.status, tc.body)
			body, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{
				Runtime: "test", Endpoint: srv.URL, Client: srv.Client(),
			})
			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("FetchUsageJSON: %v", err)
				}
				if string(body) != tc.body {
					t.Fatalf("body = %q, want %q", body, tc.body)
				}
				return
			}
			if !ops.IsUsageKind(err, tc.wantKind) {
				t.Fatalf("kind = %v (err %v), want %s", kindOf(err), err, tc.wantKind)
			}
			var usageErr *ops.UsageError
			if !errors.As(err, &usageErr) {
				t.Fatalf("err %v is not a *UsageError", err)
			}
			if usageErr.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", usageErr.Reason, tc.wantReason)
			}
			if usageErr.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", usageErr.Status, tc.wantStatus)
			}
			if len(body) != 0 {
				t.Fatalf("a failed read returned a body: %q", body)
			}
			// The provider's own text must not reach the error.
			if tc.body != "" && strings.Contains(err.Error(), tc.body) {
				t.Fatalf("error carries the provider body: %v", err)
			}
		})
	}
}

// kindOf reports the classification of err for a failure message.
func kindOf(err error) ops.UsageErrorKind {
	var usageErr *ops.UsageError
	if errors.As(err, &usageErr) {
		return usageErr.Kind
	}
	return ""
}

func TestFetchUsageJSON_SendsBearerOnlyWhenPresent(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{}`)
	if _, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{
		Runtime:    "test",
		Endpoint:   srv.URL,
		Credential: ops.NewUsageCredential(testToken, "env"),
		Header:     http.Header{"Anthropic-Beta": []string{"oauth-2025-04-20"}},
		Client:     srv.Client(),
	}); err != nil {
		t.Fatalf("FetchUsageJSON: %v", err)
	}
	// Compared as a boolean so a failure message can never print the fixture
	// token: the invariant this file pins is "no token in output", and a test
	// report is output.
	if got := srv.headers.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatal("Authorization header does not carry the credential's bearer token")
	}
	if got := srv.headers.Get("Anthropic-Beta"); got != "oauth-2025-04-20" {
		t.Fatalf("Anthropic-Beta = %q", got)
	}

	// No credential: no Authorization header at all, not an empty bearer.
	empty := newServer(t, http.StatusOK, `{}`)
	if _, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{Runtime: "test", Endpoint: empty.URL, Client: empty.Client()}); err != nil {
		t.Fatalf("FetchUsageJSON: %v", err)
	}
	// Boolean, not %q: if this invariant ever breaks, the report must not print
	// the header value.
	if _, ok := empty.headers["Authorization"]; ok {
		t.Fatal("an unauthenticated read sent an Authorization header")
	}
}

func TestFetchUsageJSON_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{
		Runtime:  "test",
		Endpoint: srv.URL,
		Timeout:  50 * time.Millisecond,
		Client:   srv.Client(),
	})
	if !ops.IsUsageKind(err, ops.UsageKindTimeout) {
		t.Fatalf("err = %v (kind %v), want timeout", err, kindOf(err))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestFetchUsageJSON_RefusesAnEmptyEndpoint(t *testing.T) {
	_, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{Runtime: "test"})
	if !ops.IsUsageKind(err, ops.UsageKindUnsupported) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

// credentialFile writes a vendor-style credential document and returns its path.
func credentialFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	return path
}

func TestResolveUsageCredential(t *testing.T) {
	doc := `{"claudeAiOauth":{"accessToken":"` + testToken + `","subscriptionType":"max"}}`
	spec := ops.CredentialSpec{Env: "OAR_TEST_TOKEN", Path: credentialFile(t, doc), Field: []string{"claudeAiOauth", "accessToken"}}

	// The vendor's own document: the token, its non-secret source and the
	// decoded document (so an adapter can read subscriptionType without a
	// second parse).
	creds, decoded, err := ops.ResolveUsageCredential(nil, spec)
	if err != nil {
		t.Fatalf("ResolveUsageCredential: %v", err)
	}
	if creds.Token() != testToken || creds.Source() != "file" || !creds.Present() {
		t.Fatalf("credential = %v (token match %v)", creds, creds.Token() == testToken)
	}
	if got := ops.JSONStringAt(decoded, "claudeAiOauth", "subscriptionType"); got != "max" {
		t.Fatalf("subscriptionType = %q", got)
	}

	// An explicit environment token wins, and there is no document to read.
	env := []string{"OAR_TEST_TOKEN=" + testToken + "-env", "PATH=/bin"}
	creds, decoded, err = ops.ResolveUsageCredential(env, spec)
	if err != nil {
		t.Fatalf("ResolveUsageCredential: %v", err)
	}
	if creds.Token() != testToken+"-env" || creds.Source() != "env" {
		t.Fatalf("env credential = %v", creds)
	}
	if decoded != nil {
		t.Fatalf("env credential returned a document: %v", decoded)
	}

	// Every remaining shape is a credentials problem, never an empty token.
	failures := map[string]ops.CredentialSpec{
		"missing file":     {Path: filepath.Join(t.TempDir(), "absent.json"), Field: []string{"claudeAiOauth", "accessToken"}},
		"unreadable json":  {Path: credentialFile(t, `not json`), Field: []string{"claudeAiOauth", "accessToken"}},
		"missing field":    {Path: credentialFile(t, `{"claudeAiOauth":{}}`), Field: []string{"claudeAiOauth", "accessToken"}},
		"empty token":      {Path: credentialFile(t, `{"claudeAiOauth":{"accessToken":"  "}}`), Field: []string{"claudeAiOauth", "accessToken"}},
		"no source at all": {},
		"wrong field type": {Path: credentialFile(t, `{"claudeAiOauth":{"accessToken":42}}`), Field: []string{"claudeAiOauth", "accessToken"}},
	}
	for name, badSpec := range failures {
		t.Run(name, func(t *testing.T) {
			creds, _, err := ops.ResolveUsageCredential(nil, badSpec)
			if !ops.IsUsageKind(err, ops.UsageKindReauth) {
				t.Fatalf("err = %v, want reauth_required", err)
			}
			var usageErr *ops.UsageError
			if errors.As(err, &usageErr) && usageErr.Reason != ops.ReasonCredentialsMissing {
				t.Fatalf("reason = %q, want %q", usageErr.Reason, ops.ReasonCredentialsMissing)
			}
			if creds.Present() {
				t.Fatalf("a failed resolve returned a credential")
			}
			// The document's content and the path stay out of the message.
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

// TestUsageCredential_RedactsEveryFormattingPath pins the guarantee that a
// credential can be printed by accident without printing the secret. A host
// that logs a struct with %+v, or a test that fails on an unexpected value,
// must not write the token to a log file.
func TestUsageCredential_RedactsEveryFormattingPath(t *testing.T) {
	creds := ops.NewUsageCredential(testToken, "file")
	rendered := []string{
		fmt.Sprintf("%v", creds),
		fmt.Sprintf("%+v", creds),
		fmt.Sprintf("%#v", creds),
		fmt.Sprintf("%s", creds),
		creds.String(),
		creds.GoString(),
	}
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered = append(rendered, string(encoded))
	// A struct that embeds a credential must redact it too.
	encoded, err = json.Marshal(struct {
		Cred ops.UsageCredential `json:"cred"`
	}{Cred: creds})
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	rendered = append(rendered, string(encoded))

	for _, text := range rendered {
		if strings.Contains(text, testToken) {
			t.Fatalf("rendered credential leaks the token: %s", text)
		}
	}

	// The one accessor that returns the secret exists for the Authorization
	// header only; it is not part of any format verb.
	if creds.Token() != testToken {
		t.Fatal("Token() does not return the credential's token")
	}
	// Zero and explicitly empty credentials are not present.
	if ops.NewUsageCredential("", "env").Present() {
		t.Fatal("empty credential reports present")
	}
}

func TestUsageError_Text(t *testing.T) {
	err := &ops.UsageError{Runtime: "claude", Kind: ops.UsageKindReauth, Reason: ops.ReasonCredentialsRejected, Status: 401}
	want := "claude: account usage: reauth_required (credentials_rejected): HTTP 401"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	// A bare kind reads without a dangling separator.
	if got := (&ops.UsageError{Runtime: "codex", Kind: ops.UsageKindTimeout}).Error(); got != "codex: account usage: timeout" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestPercentWindow(t *testing.T) {
	reset := time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	window, ok := ops.PercentWindow("Current session", 42.5, &reset)
	if !ok {
		t.Fatal("a valid reading was refused")
	}
	if window.Name != "Current session" || window.Used != 42.5 || window.Limit != 100 || window.Unit != "percent" {
		t.Fatalf("window = %+v", window)
	}
	if window.ResetsAt == nil || window.ResetsAt.Location() != time.UTC || !window.ResetsAt.Equal(reset) {
		t.Fatalf("resetsAt = %v, want %v in UTC", window.ResetsAt, reset)
	}

	// Absent reset time stays absent rather than becoming the zero time.
	if window, _ := ops.PercentWindow("x", 0, nil); window.ResetsAt != nil {
		t.Fatalf("nil reset became %v", window.ResetsAt)
	}

	// An unusable reading is skipped, never clamped into a fake measurement.
	for _, percent := range []float64{-0.1, 100.1, 1e9} {
		if _, ok := ops.PercentWindow("x", percent, nil); ok {
			t.Fatalf("%v was accepted", percent)
		}
	}
}

func TestParseInstantAndEpoch(t *testing.T) {
	instant := ops.ParseInstant("2026-03-01T12:00:00.000Z")
	if instant == nil || !instant.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) || instant.Location() != time.UTC {
		t.Fatalf("ParseInstant = %v", instant)
	}
	// Absent and unreadable both mean "no reset time reported".
	for _, value := range []string{"", "   ", "not-a-time", "2026-13-01T00:00:00Z"} {
		if got := ops.ParseInstant(value); got != nil {
			t.Fatalf("ParseInstant(%q) = %v, want nil", value, got)
		}
	}
	if got := ops.ParseEpochSeconds(1772366400); got == nil || got.Location() != time.UTC {
		t.Fatalf("ParseEpochSeconds = %v", got)
	}
	for _, seconds := range []float64{0, -1} {
		if got := ops.ParseEpochSeconds(seconds); got != nil {
			t.Fatalf("ParseEpochSeconds(%v) = %v, want nil", seconds, got)
		}
	}
}

// TestReasonCodesAreStable pins the documented spellings as literals. Every
// other test compares against the exported constants, which stays green if a
// constant's value is renamed while docs/ops.md silently goes stale; these are
// the strings a host actually matches on.
func TestReasonCodesAreStable(t *testing.T) {
	codes := map[string]string{
		"reauth: missing":          ops.ReasonCredentialsMissing,
		"reauth: rejected":         ops.ReasonCredentialsRejected,
		"reauth: signed out":       ops.ReasonNotAuthenticated,
		"unsupported: no endpoint": ops.ReasonEndpointUnavailable,
		"unsupported: no quota":    ops.ReasonQuotaUnavailable,
		"unsupported: auth mode":   ops.ReasonUnsupportedAuthMode,
		"rate limited: request":    ops.ReasonRequestRejected,
	}
	want := map[string]string{
		"reauth: missing":          "credentials_missing",
		"reauth: rejected":         "credentials_rejected",
		"reauth: signed out":       "not_authenticated",
		"unsupported: no endpoint": "endpoint_unavailable",
		"unsupported: no quota":    "quota_unavailable",
		"unsupported: auth mode":   "unsupported_auth_mode",
		"rate limited: request":    "request_rejected",
	}
	for name, got := range codes {
		if got != want[name] {
			t.Fatalf("%s = %q, want %q", name, got, want[name])
		}
	}
	kinds := map[string]string{
		"reauth":       string(ops.UsageKindReauth),
		"unsupported":  string(ops.UsageKindUnsupported),
		"rate limited": string(ops.UsageKindRateLimited),
		"timeout":      string(ops.UsageKindTimeout),
		"malformed":    string(ops.UsageKindMalformed),
		"transport":    string(ops.UsageKindTransport),
	}
	wantKinds := map[string]string{
		"reauth":       "reauth_required",
		"unsupported":  "unsupported",
		"rate limited": "rate_limited",
		"timeout":      "timeout",
		"malformed":    "malformed",
		"transport":    "transport",
	}
	for name, got := range kinds {
		if got != wantKinds[name] {
			t.Fatalf("%s kind = %q, want %q", name, got, wantKinds[name])
		}
	}
}

func TestLabelUsageError(t *testing.T) {
	// A runtime-less seam failure gains the adapter's label; the original value
	// is not mutated, so a shared error cannot be relabelled by a second adapter.
	unlabelled := &ops.UsageError{Kind: ops.UsageKindReauth, Reason: ops.ReasonCredentialsMissing}
	labelled := ops.LabelUsageError(unlabelled, "claude")
	if unlabelled.Runtime != "" {
		t.Fatalf("the original error was mutated: %q", unlabelled.Runtime)
	}
	var usageErr *ops.UsageError
	if !errors.As(labelled, &usageErr) || usageErr.Runtime != "claude" {
		t.Fatalf("labelled error = %v", labelled)
	}
	// A second label does not rename an error that already names its runtime,
	// and a non-UsageError passes through untouched.
	if got := ops.LabelUsageError(labelled, "codex"); got.Error() != "claude: account usage: reauth_required (credentials_missing)" {
		t.Fatalf("relabelled error = %v", got)
	}
	plain := errors.New("plain")
	if got := ops.LabelUsageError(plain, "claude"); got != plain {
		t.Fatalf("a plain error was replaced: %v", got)
	}
}

// TestFetchUsageJSON_UnusableBodies covers the two shapes that are not a status
// code: an oversized body and the method-not-allowed status.
func TestFetchUsageJSON_UnusableBodies(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	srv := newServer(t, http.StatusOK, big)
	if _, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{Runtime: "test", Endpoint: srv.URL, Client: srv.Client()}); !ops.IsUsageKind(err, ops.UsageKindMalformed) {
		t.Fatalf("oversized body: err = %v, want malformed", err)
	}

	method := newServer(t, http.StatusMethodNotAllowed, ``)
	if _, err := ops.FetchUsageJSON(context.Background(), ops.UsageRequest{Runtime: "test", Endpoint: method.URL, Client: method.Client()}); !ops.IsUsageKind(err, ops.UsageKindUnsupported) {
		t.Fatalf("405: err = %v, want unsupported", err)
	}
}
