package claude

// Account usage (issue #89) against an httptest server: every request in this
// file goes to a server the test started, and no test needs a real `claude`, a
// real credential or the network. The credential document is written under a
// temporary home directory and handed to the reader through its Home/Env fields
// instead of through the process environment, so a test cannot leak a token (or
// pick one up from the host running it) and two tests cannot see each other's
// credential.
//
// The tokens below are obviously fake and recognisable on purpose: the
// redaction tests search every error message and every printed credential for
// them, and a token that looked plausible would make a leaked one hard to spot
// in a failing test's output.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// usageFileToken is the token Claude's own document holds in these tests.
	usageFileToken = "fake-usage-file-token-not-a-secret"
	// usageEnvToken is the token the explicit environment override holds; it
	// must never be mistaken for the file's.
	usageEnvToken = "fake-usage-env-token-not-a-secret"
)

// usageTestServer is an httptest server that remembers every request it
// answered: how many arrived, and the two headers this adapter is responsible
// for. Counting requests is how the "no credential, no request" case is proven
// — an error alone would be compatible with a read that had already gone out.
type usageTestServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests int
	auth     []string
	beta     []string
}

// requestCount reports how many requests reached the server.
func (s *usageTestServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// lastAuthorization reports the Authorization header of the last request.
func (s *usageTestServer) lastAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auth) == 0 {
		return ""
	}
	return s.auth[len(s.auth)-1]
}

// lastBeta reports the anthropic-beta header of the last request.
func (s *usageTestServer) lastBeta() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.beta) == 0 {
		return ""
	}
	return s.beta[len(s.beta)-1]
}

// newUsageTestServer answers every request with status and body.
func newUsageTestServer(t *testing.T, status int, body string) *usageTestServer {
	t.Helper()
	s := &usageTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.beta = append(s.beta, r.Header.Get(claudeUsageBetaHeader))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// lazyUsageTestServer answers with JSON, but only after delay: the timeout case
// needs a request that is still open when the read's bound expires.
func lazyUsageTestServer(t *testing.T, delay time.Duration) *usageTestServer {
	t.Helper()
	s := &usageTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			// The read gave up, which is the point of the test: return without
			// writing so nothing here races the closed connection.
			return
		}
		_, _ = io.WriteString(w, `{"five_hour":{"utilization":1}}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// usageTestHome writes a credential document under a fresh home directory and
// returns the directory. Pass "" for a home with no document at all (the
// "signed out" case).
func usageTestHome(t *testing.T, document string) string {
	t.Helper()
	home := t.TempDir()
	if document == "" {
		return home
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(document), 0o600); err != nil {
		t.Fatalf("write credential document: %v", err)
	}
	return home
}

// usageReader is a reader aimed at srv with an explicit environment and home:
// nothing here depends on the process environment, so a host's real
// OARLOCK_CLAUDE_OAUTH_TOKEN can never answer for a test.
func usageReader(srv *usageTestServer, home string, env []string, timeout time.Duration) accountUsageReader {
	return accountUsageReader{
		Endpoint: srv.URL,
		Client:   srv.Client(),
		Env:      env,
		Home:     home,
		Timeout:  timeout,
	}
}

// usageCredentialDocument is Claude's credential document with an explicit
// subscription type, which is where the plan of a snapshot comes from.
func usageCredentialDocument(token, subscriptionType string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"fake-refresh-token-not-used","expiresAt":1790000000000,"scopes":["user:inference"],"subscriptionType":%q}}`, token, subscriptionType)
}

// usageError fails unless err is a *UsageError of kind. The reason is returned
// so a caller can also pin the stable code a host matches on.
func usageError(t *testing.T, err error, kind ops.UsageErrorKind) *ops.UsageError {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s", kind)
	}
	var usageErr *ops.UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("err = %v (%T), want *ops.UsageError", err, err)
	}
	if !ops.IsUsageKind(err, kind) {
		t.Fatalf("kind = %q, want %q (err: %v)", usageErr.Kind, kind, err)
	}
	return usageErr
}

// timePtr is the one-line helper the reset assertions need.
func timePtr(t time.Time) *time.Time { return &t }

// TestAccountUsage_ProjectsEveryWindowInOrder pins the success path: the flat
// payload becomes windows in the fixed order a host renders them, each as a
// percentage of the vendor's own quota, with the reset instant in UTC and the
// plan taken from the credential document's subscription type.
func TestAccountUsage_ProjectsEveryWindowInOrder(t *testing.T) {
	srv := newUsageTestServer(t, http.StatusOK, `{
		"five_hour": {"utilization": 7, "resets_at": "2026-08-22T09:59:00Z"},
		"seven_day": {"utilization": 14, "resets_at": "2026-08-28T00:00:00+02:00"},
		"seven_day_oauth_apps": {"utilization": 3},
		"seven_day_opus": {"utilization": 100},
		"seven_day_sonnet": {"utilization": 0, "resets_at": "invalid"},
		"seven_day_overage_included": {"utilization": 99},
		"extra_usage": {"is_enabled": false, "utilization": 25, "monthly_limit": 100}
	}`)
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))

	usage, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if usage.Plan != "max" {
		t.Fatalf("Plan = %q, want the document's subscriptionType", usage.Plan)
	}

	wantNames := []string{
		"Current session",
		"Current week (all models)",
		"Current week (OAuth apps)",
		"Current week (Opus)",
		"Current week (Sonnet)",
	}
	if len(usage.Windows) != len(wantNames) {
		t.Fatalf("windows = %d (%+v), want %d", len(usage.Windows), usage.Windows, len(wantNames))
	}
	for i, want := range wantNames {
		if usage.Windows[i].Name != want {
			t.Fatalf("window %d = %q, want %q (all: %+v)", i, usage.Windows[i].Name, want, usage.Windows)
		}
	}

	wantPercent := []float64{7, 14, 3, 100, 0}
	for i, want := range wantPercent {
		window := usage.Windows[i]
		if window.Used != want || window.Limit != 100 || window.Unit != "percent" {
			t.Fatalf("window %q = {%v/%v %q}, want {%v/100 percent}", window.Name, window.Used, window.Limit, window.Unit, want)
		}
	}

	// The resets_at instants are reported in UTC whichever offset the vendor
	// wrote, and an unreadable one is no reset time rather than a zero time.
	wantResets := map[string]*time.Time{
		"Current session":           timePtr(time.Date(2026, 8, 22, 9, 59, 0, 0, time.UTC)),
		"Current week (all models)": timePtr(time.Date(2026, 8, 27, 22, 0, 0, 0, time.UTC)),
	}
	for _, window := range usage.Windows {
		want := wantResets[window.Name]
		if want == nil {
			if window.ResetsAt != nil {
				t.Fatalf("window %q has resetsAt %v, want none", window.Name, *window.ResetsAt)
			}
			continue
		}
		if window.ResetsAt == nil {
			t.Fatalf("window %q has no resetsAt, want %v", window.Name, want.UTC())
		}
		if !window.ResetsAt.Equal(*want) || window.ResetsAt.Location() != time.UTC {
			t.Fatalf("window %q resetsAt = %v, want %v in UTC", window.Name, *window.ResetsAt, want.UTC())
		}
	}

	// Extra usage is disabled here, so it is not a window; the unknown
	// overage-included key is not one either.
	for _, window := range usage.Windows {
		if window.Name == "Extra usage" {
			t.Fatalf("a disabled extra_usage became a window: %+v", usage.Windows)
		}
	}

	// The request is the vendor's: one GET, the bearer token, the OAuth beta.
	if got := srv.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	// Boolean, not %q: a test report must not print the fixture token.
	if got, want := srv.lastAuthorization(), "Bearer "+usageFileToken; got != want {
		t.Fatal("Authorization header does not carry the document token")
	}
	if got := srv.lastBeta(); got != claudeUsageBetaValue {
		t.Fatalf("%s = %q, want %q", claudeUsageBetaHeader, got, claudeUsageBetaValue)
	}
}

// TestAccountUsage_ExtraUsageOnlyWhenEnabled pins the one entry whose presence
// is a flag: an enabled extra-usage bucket is a window, an unstated flag is not.
func TestAccountUsage_ExtraUsageOnlyWhenEnabled(t *testing.T) {
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "pro"))

	t.Run("enabled", func(t *testing.T) {
		srv := newUsageTestServer(t, http.StatusOK, `{
			"extra_usage": {"is_enabled": true, "utilization": 25, "monthly_limit": 100}
		}`)
		usage, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
		if err != nil {
			t.Fatalf("AccountUsage: %v", err)
		}
		if usage.Plan != "pro" {
			t.Fatalf("Plan = %q, want pro", usage.Plan)
		}
		if len(usage.Windows) != 1 || usage.Windows[0].Name != "Extra usage" || usage.Windows[0].Used != 25 {
			t.Fatalf("windows = %+v, want one Extra usage window at 25 percent", usage.Windows)
		}
	})

	t.Run("flag unstated", func(t *testing.T) {
		srv := newUsageTestServer(t, http.StatusOK, `{"extra_usage": {"utilization": 25}}`)
		_, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
		usageErr := usageError(t, err, ops.UsageKindMalformed)
		if usageErr.Reason != ops.ReasonQuotaUnavailable {
			t.Fatalf("reason = %q, want %q", usageErr.Reason, ops.ReasonQuotaUnavailable)
		}
	})
}

// TestAccountUsage_HTTPStatusIsTyped pins the mapping the seam owns: a rejected
// credential asks for a login, a limiter says the read was rate limited, and
// nothing else is reported as a quota reading of zero.
func TestAccountUsage_HTTPStatusIsTyped(t *testing.T) {
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))
	cases := []struct {
		name   string
		status int
		kind   ops.UsageErrorKind
		reason string
	}{
		{"unauthorized", http.StatusUnauthorized, ops.UsageKindReauth, ops.ReasonCredentialsRejected},
		{"forbidden", http.StatusForbidden, ops.UsageKindReauth, ops.ReasonCredentialsRejected},
		{"rate limited", http.StatusTooManyRequests, ops.UsageKindRateLimited, ops.ReasonRequestRejected},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			srv := newUsageTestServer(t, testCase.status, `{"error":"nope"}`)
			usage, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
			usageErr := usageError(t, err, testCase.kind)
			if usageErr.Reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", usageErr.Reason, testCase.reason)
			}
			if usageErr.Status != testCase.status {
				t.Fatalf("status = %d, want %d", usageErr.Status, testCase.status)
			}
			// A failure is never a snapshot: no plan, no windows.
			if usage.Plan != "" || len(usage.Windows) != 0 {
				t.Fatalf("failed read returned %+v, want the zero snapshot", usage)
			}
			if srv.requestCount() != 1 {
				t.Fatalf("requests = %d, want 1", srv.requestCount())
			}
		})
	}
}

// TestAccountUsage_NoUsableWindowIsMalformed pins the third outcome: the
// endpoint answered 2xx with nothing this adapter can project. It must be a
// typed error, because an empty snapshot with a nil error would be recorded as
// "quota known and unused".
func TestAccountUsage_NoUsableWindowIsMalformed(t *testing.T) {
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))
	bodies := map[string]string{
		"empty object": `{}`,
		"every window null": `{
			"five_hour": null, "seven_day": null, "seven_day_oauth_apps": null,
			"seven_day_opus": null, "seven_day_sonnet": null, "extra_usage": null
		}`,
		"utilization absent":       `{"five_hour": {"resets_at": "2026-08-22T09:59:00Z"}}`,
		"utilization out of range": `{"five_hour": {"utilization": 101}, "seven_day": {"utilization": -1}}`,
		"json null":                `null`,
		"unreadable json":          `not json`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := newUsageTestServer(t, http.StatusOK, body)
			usage, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
			usageErr := usageError(t, err, ops.UsageKindMalformed)
			if usageErr.Reason != ops.ReasonQuotaUnavailable {
				t.Fatalf("reason = %q, want %q", usageErr.Reason, ops.ReasonQuotaUnavailable)
			}
			if usage.Plan != "" || len(usage.Windows) != 0 {
				t.Fatalf("malformed read returned %+v, want the zero snapshot", usage)
			}
			if srv.requestCount() != 1 {
				t.Fatalf("requests = %d, want 1", srv.requestCount())
			}
		})
	}
}

// TestAccountUsage_TimeoutIsTimeout pins that a slow endpoint is reported as a
// timeout and not as a slow success or as malformed quota.
func TestAccountUsage_TimeoutIsTimeout(t *testing.T) {
	srv := lazyUsageTestServer(t, 2*time.Second)
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))

	started := time.Now()
	usage, err := usageReader(srv, home, []string{}, 50*time.Millisecond).AccountUsage(context.Background())
	elapsed := time.Since(started)

	usageError(t, err, ops.UsageKindTimeout)
	if usage.Plan != "" || len(usage.Windows) != 0 {
		t.Fatalf("timed-out read returned %+v, want the zero snapshot", usage)
	}
	if elapsed > time.Second {
		t.Fatalf("read took %v, want it bounded by the 50ms Timeout", elapsed)
	}
}

// TestAccountUsage_MissingCredentialMakesNoRequest pins that a signed-out
// machine is a login problem discovered before any network I/O: no request may
// reach the endpoint with no token.
func TestAccountUsage_MissingCredentialMakesNoRequest(t *testing.T) {
	cases := map[string]string{
		"no document":        "",
		"unreadable json":    `not json`,
		"no access token":    `{"claudeAiOauth":{"subscriptionType":"max"}}`,
		"empty access token": `{"claudeAiOauth":{"accessToken":"   "}}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newUsageTestServer(t, http.StatusOK, `{"five_hour":{"utilization":1}}`)
			home := usageTestHome(t, document)
			usage, err := usageReader(srv, home, []string{}, 0).AccountUsage(context.Background())
			usageErr := usageError(t, err, ops.UsageKindReauth)
			if usageErr.Reason != ops.ReasonCredentialsMissing {
				t.Fatalf("reason = %q, want %q", usageErr.Reason, ops.ReasonCredentialsMissing)
			}
			if usage.Plan != "" || len(usage.Windows) != 0 {
				t.Fatalf("failed read returned %+v, want the zero snapshot", usage)
			}
			if got := srv.requestCount(); got != 0 {
				t.Fatalf("requests = %d, want 0: a read with no credential must not be sent", got)
			}
		})
	}
}

// TestAccountUsage_NeverLeaksTheToken pins the redaction invariant for this
// adapter: the token it sends is absent from the error text of a rejected read,
// from every printed form of the credential and from its JSON encoding. A host
// logs errors and records snapshots; neither may carry a bearer token.
func TestAccountUsage_NeverLeaksTheToken(t *testing.T) {
	srv := newUsageTestServer(t, http.StatusUnauthorized, `{"error":"invalid token `+usageFileToken+`"}`)
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))
	reader := usageReader(srv, home, []string{}, 0)

	_, err := reader.AccountUsage(context.Background())
	usageError(t, err, ops.UsageKindReauth)
	if strings.Contains(err.Error(), usageFileToken) {
		t.Fatalf("the error echoes the token: %v", err)
	}
	// The vendor body itself carries the token in this fake; a provider may echo
	// a credential back, so nothing from the body may reach an error either.
	if strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("the error echoes the response body: %v", err)
	}

	credential, document, err := reader.credential()
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if credential.Token() != usageFileToken || credential.Source() != "file" {
		t.Fatalf("credential = %v, want the document token from file", credential)
	}
	if got := ops.JSONStringAt(document, claudeUsagePlanField...); got != "max" {
		t.Fatalf("plan from the document = %q, want max", got)
	}

	printed := []string{
		fmt.Sprintf("%v", credential),
		fmt.Sprintf("%+v", credential),
		fmt.Sprintf("%#v", credential),
		credential.String(),
	}
	for _, form := range printed {
		if strings.Contains(form, usageFileToken) {
			t.Fatalf("a printed credential leaks the token: %s", form)
		}
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if strings.Contains(string(encoded), usageFileToken) {
		t.Fatalf("the marshalled credential leaks the token: %s", encoded)
	}
	if !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("marshalled credential = %s, want a redaction", encoded)
	}
}

// TestAccountUsage_EnvTokenWinsAndPlanStaysEmpty pins the override: an explicit
// environment token is used even when a document exists, and because the token
// did not come from a document there is no subscription type to report — an
// empty plan, never the file's plan presented as the environment token's.
func TestAccountUsage_EnvTokenWinsAndPlanStaysEmpty(t *testing.T) {
	srv := newUsageTestServer(t, http.StatusOK, `{"five_hour":{"utilization":42,"resets_at":"2026-08-22T09:59:00Z"}}`)
	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))
	env := []string{"PATH=/nonexistent", claudeUsageTokenEnv + "=" + usageEnvToken}

	usage, err := usageReader(srv, home, env, 0).AccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if got, want := srv.lastAuthorization(), "Bearer "+usageEnvToken; got != want {
		t.Fatal("Authorization header does not carry the environment token")
	}
	if strings.Contains(srv.lastAuthorization(), usageFileToken) {
		t.Fatalf("the file token was sent although the environment overrode it")
	}
	if usage.Plan != "" {
		t.Fatalf("Plan = %q, want empty: an environment token has no credential document", usage.Plan)
	}
	if len(usage.Windows) != 1 || usage.Windows[0].Name != "Current session" || usage.Windows[0].Used != 42 {
		t.Fatalf("windows = %+v, want one Current session window at 42 percent", usage.Windows)
	}
}

// TestAccountUsageReader_Defaults pins the promises the zero value makes: the
// production endpoint, the read bound the seam chose, and the home directory in
// force when the field is empty.
func TestAccountUsageReader_Defaults(t *testing.T) {
	var reader accountUsageReader
	if got := reader.endpoint(); got != claudeUsageEndpoint {
		t.Fatalf("endpoint = %q, want %q", got, claudeUsageEndpoint)
	}
	if claudeUsageEndpoint != "https://api.anthropic.com/api/oauth/usage" {
		t.Fatalf("claudeUsageEndpoint = %q", claudeUsageEndpoint)
	}
	if got := (accountUsageReader{Timeout: 0}).Timeout; got != 0 {
		t.Fatalf("zero Timeout = %v, want 0 (the seam then applies ops.UsageTimeout)", got)
	}

	home := usageTestHome(t, usageCredentialDocument(usageFileToken, "max"))
	if got := (accountUsageReader{Home: home}).home(); got != home {
		t.Fatalf("home = %q, want %q", got, home)
	}
	// With no override it is the process home directory, which is a directory
	// (the document may or may not exist there).
	if got := (accountUsageReader{}).home(); got == "" {
		t.Fatal("home is empty, want the process home directory")
	}
}

// TestRuntime_UsageReaderIsPresent pins the wiring: Runtime() names a reader,
// and the reader it names is the HTTP one this file tests rather than a stub
// that would report nothing while looking implemented.
func TestRuntime_UsageReaderIsPresent(t *testing.T) {
	var raw any = Runtime().Usage
	reader, ok := raw.(accountUsageReader)
	if !ok {
		t.Fatalf("Runtime().Usage = %T, want accountUsageReader", raw)
	}
	if reader.Endpoint != "" || reader.Client != nil || reader.Home != "" || reader.Timeout != 0 {
		t.Fatalf("Runtime().Usage = %+v, want the zero-valued production reader", reader)
	}
	// A host branches on presence; this is the presence check it uses.
	var iface oarlock.AccountUsageReader = Runtime().Usage
	if iface == nil {
		t.Fatal("Runtime().Usage is nil, want a reader")
	}
}

// TestAccountUsage_RefusesANilContext pins the guard the rest of oarlock has:
// a nil context is an error, not a panic inside the transport's deadline.
func TestAccountUsage_RefusesANilContext(t *testing.T) {
	srv := newUsageTestServer(t, http.StatusOK, `{"five_hour":{"utilization":1}}`)
	_, err := usageReader(srv, usageTestHome(t, usageCredentialDocument(usageFileToken, "max")), nil, 0).AccountUsage(nil)
	usageErr := usageError(t, err, ops.UsageKindTransport)
	if usageErr.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", usageErr.Runtime)
	}
	if srv.requestCount() != 0 {
		t.Fatalf("a nil context still made %d request(s)", srv.requestCount())
	}
}

// TestAccountUsage_CredentialFailureIsLabelled pins the runtime label on the
// seam's credential error: the seam resolves a token before it knows which
// adapter asked, so the adapter must add the label. Without it the message is
// "`\": account usage: …\"` — the one failure a signed-out host sees first.
func TestAccountUsage_CredentialFailureIsLabelled(t *testing.T) {
	srv := newUsageTestServer(t, http.StatusOK, `{}`)
	usage, err := usageReader(srv, usageTestHome(t, ""), nil, 0).AccountUsage(context.Background())
	usageErr := usageError(t, err, ops.UsageKindReauth)
	if usageErr.Reason != ops.ReasonCredentialsMissing {
		t.Fatalf("reason = %q, want %q", usageErr.Reason, ops.ReasonCredentialsMissing)
	}
	if usageErr.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", usageErr.Runtime)
	}
	if strings.HasPrefix(usageErr.Error(), ":") {
		t.Fatalf("the message has no runtime: %q", usageErr.Error())
	}
	if len(usage.Windows) != 0 || usage.Plan != "" {
		t.Fatalf("a failed read returned usage: %+v", usage)
	}
}
