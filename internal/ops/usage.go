package ops

// Account-usage transport (issue #89). One read-only HTTP GET per vendor quota
// endpoint, a bearer token resolved from the vendor's own store or an explicit
// environment variable, and a typed error for every way the read can fail.
//
// The rules this file exists to keep:
//
//   - A failed read is a *UsageError. It never yields an empty AccountUsage,
//     so no caller can mistake "the endpoint said nothing" for "no plan, no
//     quota". Absence is a nil Runtime.Usage field, not a zero snapshot.
//   - The token is not printed, not marshalled, not logged and not part of any
//     error text: UsageCredential keeps it unexported and redacts String,
//     GoString and MarshalJSON. Vendor response bodies are never copied into
//     an error either, because a provider may echo a credential back.
//   - The response is judged by its content, never by an exit code: only a 2xx
//     body that projects to at least one window is a successful read, and the
//     adapter decides that part.
//   - One context bound covers the whole read, including the body. A read that
//     outlives it is a timeout, not a slow success.
//
// Windows are reported as a percentage of the vendor's own quota: Used is the
// percent the vendor reports, Limit is 100 and Unit is "percent". OAR's
// usedRatio is the same number divided by 100, so nothing is invented and
// nothing is rescaled into a fabricated absolute limit (the vendors do not
// report one on these endpoints).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
)

const (
	// UsageTimeout bounds one whole account-usage read when the adapter does
	// not choose its own. OAR allows 30 s for Claude and 8 s for Codex; one
	// bound is used here so a host has a single number to reason about, and it
	// is the middle of those two rather than an optimistic floor.
	UsageTimeout = 10 * time.Second

	// usageBodyLimit caps the response body. A quota snapshot is a few KiB; a
	// larger body is reported as malformed instead of being read into memory.
	usageBodyLimit = 1 << 20

	// usageEnvSource is the credential source name for a token read from an
	// explicit environment variable.
	usageEnvSource = "env"
	// usageFileSource is the credential source name for the vendor's document.
	usageFileSource = "file"
)

// UsageErrorKind classifies why an account-usage read produced no snapshot.
// The set mirrors OAR's kind/reason table, collapsed into one Go error type:
// callers branch on Kind (and, for the stable ones, Reason) with errors.As or
// IsUsageKind, never on prose.
type UsageErrorKind string

const (
	// UsageKindUnsupported: this runtime or installation has no account-usage
	// surface this adapter can read (a missing endpoint, a non-subscription
	// account, a CLI that does not answer the query).
	UsageKindUnsupported UsageErrorKind = "unsupported"
	// UsageKindReauth: the read needs a login — a missing credential, or one
	// the provider rejected (401/403).
	UsageKindReauth UsageErrorKind = "reauth_required"
	// UsageKindRateLimited: the provider's own limiter answered 429. It says
	// nothing about the account's quota, so it is not reported as usage.
	UsageKindRateLimited UsageErrorKind = "rate_limited"
	// UsageKindTimeout: the context bound expired before the read finished.
	UsageKindTimeout UsageErrorKind = "timeout"
	// UsageKindMalformed: the endpoint answered, but the body carried no
	// usable quota (unreadable JSON, or no window at all).
	UsageKindMalformed UsageErrorKind = "malformed"
	// UsageKindTransport: a network failure, or a non-2xx status this file
	// does not classify more precisely. Status carries the HTTP code when the
	// answer reached us.
	UsageKindTransport UsageErrorKind = "transport"
)

// Stable reason codes. They are safe to show and safe to match: no token, no
// credential path and no provider body is ever part of them.
const (
	// ReasonCredentialsMissing: no usable persisted credential and no explicit
	// environment token.
	ReasonCredentialsMissing = "credentials_missing"
	// ReasonCredentialsRejected: the endpoint rejected the credential.
	ReasonCredentialsRejected = "credentials_rejected"
	// ReasonNotAuthenticated: the vendor says this machine is signed out.
	ReasonNotAuthenticated = "not_authenticated"
	// ReasonEndpointUnavailable: the vendor/installation does not expose the
	// queried usage endpoint.
	ReasonEndpointUnavailable = "endpoint_unavailable"
	// ReasonQuotaUnavailable: the answer carried no quota configuration.
	ReasonQuotaUnavailable = "quota_unavailable"
	// ReasonRequestRejected: the provider rate-limited the usage read itself.
	// OAR has no 429 mapping, so this code is oarlock's own; it is documented in
	// docs/ops.md so a caller can match it.
	ReasonRequestRejected = "request_rejected"
	// ReasonUnsupportedAuthMode: the account is authenticated in a mode that has
	// no subscription quota to read (codex: an API key or a Bedrock account).
	// This is the adapter's decision, not proof that a token was rejected.
	ReasonUnsupportedAuthMode = "unsupported_auth_mode"
)

// UsageError is the typed failure of one account-usage read. Runtime names the
// adapter ("claude"), Kind and Reason are the stable classification, and
// Status is the HTTP status when an answer was received (0 otherwise).
type UsageError struct {
	Runtime string
	Kind    UsageErrorKind
	Reason  string
	Status  int
}

// Error renders the classification only. It deliberately carries no provider
// output: a 401 body, a redirect URL and a token are all things a log line
// must not gain from an error.
func (e *UsageError) Error() string {
	msg := fmt.Sprintf("%s: account usage: %s", e.Runtime, e.Kind)
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	if e.Status != 0 {
		msg += fmt.Sprintf(": HTTP %d", e.Status)
	}
	return msg
}

// IsUsageKind reports whether err is a *UsageError of the given kind. It
// unwraps, so a wrapped error still classifies.
func IsUsageKind(err error, kind UsageErrorKind) bool {
	var usageErr *UsageError
	return errors.As(err, &usageErr) && usageErr.Kind == kind
}

// LabelUsageError stamps a *UsageError that carries no runtime with one, and
// returns err unchanged otherwise. The seam builds a credential failure before
// it knows which adapter asked, so the adapter that does know labels it here
// instead of re-deriving the classification. A labelled error is not relabelled:
// the first runtime to see it owns the message.
func LabelUsageError(err error, runtime string) error {
	var usageErr *UsageError
	if !errors.As(err, &usageErr) || usageErr.Runtime != "" || runtime == "" {
		return err
	}
	labelled := *usageErr
	labelled.Runtime = runtime
	return &labelled
}

// usageError builds a *UsageError with the runtime and classification set.
func usageError(runtime string, kind UsageErrorKind, reason string) *UsageError {
	return &UsageError{Runtime: runtime, Kind: kind, Reason: reason}
}

// UsageCredential is one bearer token and where it came from. The token field
// is unexported on purpose: %v, %+v, %#v and json.Marshal of a credential all
// print a redaction instead of the secret, so a stray log line or a failing
// test cannot leak it. Token returns the value only for building the
// Authorization header.
type UsageCredential struct {
	token  string
	source string
}

// NewUsageCredential returns a credential holding token. source is a short
// non-secret label ("env", "file") used in messages.
func NewUsageCredential(token, source string) UsageCredential {
	return UsageCredential{token: token, source: source}
}

// Token returns the bearer token, or "" when the credential is empty.
func (c UsageCredential) Token() string { return c.token }

// Source returns the non-secret label of where the token came from.
func (c UsageCredential) Source() string { return c.source }

// Present reports whether a non-empty token is held.
func (c UsageCredential) Present() bool { return c.token != "" }

// String redacts the token for %v and %s.
func (c UsageCredential) String() string {
	return "usage credential (" + c.redactedSource() + ", [redacted])"
}

// GoString redacts the token for %#v.
func (c UsageCredential) GoString() string { return c.String() }

// MarshalJSON redacts the token if a credential is ever marshalled into a
// record or a test fixture.
func (c UsageCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source string `json:"source"`
		Token  string `json:"token"`
	}{Source: c.redactedSource(), Token: "[redacted]"})
}

func (c UsageCredential) redactedSource() string {
	if c.source == "" {
		return "unknown source"
	}
	return c.source
}

// CredentialSpec names where a vendor keeps the token an account-usage read
// sends: an explicit environment variable first, then the vendor's own
// credential document and the JSON path of the token inside it.
type CredentialSpec struct {
	// Env is the variable that overrides the vendor's store. Empty disables it.
	Env string
	// Path is the vendor's credential document. Empty refuses the file source.
	Path string
	// Field is the JSON path of the token inside Path,
	// e.g. ["claudeAiOauth","accessToken"].
	Field []string
}

// ResolveUsageCredential reads the token from base (an explicit environment, so
// a test never has to mutate the process) or from the vendor's document. The
// decoded document is returned too, so an adapter can read a non-secret sibling
// field such as a subscription tier without parsing the file twice; it is nil
// when the token came from the environment.
//
// Every failure is *UsageError{Kind: reauth_required}: no credential is a
// login problem, and an unreadable document is reported as a missing
// credential rather than as "none of this account's quota was used". The path
// and the document's content are never part of the error.
func ResolveUsageCredential(base []string, spec CredentialSpec) (UsageCredential, map[string]any, error) {
	if spec.Env != "" {
		if token := envValue(base, spec.Env); token != "" {
			return NewUsageCredential(token, usageEnvSource), nil, nil
		}
	}
	if spec.Path == "" {
		return UsageCredential{}, nil, usageError("", UsageKindReauth, ReasonCredentialsMissing)
	}
	raw, err := os.ReadFile(spec.Path)
	if err != nil {
		return UsageCredential{}, nil, usageError("", UsageKindReauth, ReasonCredentialsMissing)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// An unreadable credential document is a credentials problem, not a
		// quota problem: never report it as success.
		return UsageCredential{}, nil, usageError("", UsageKindReauth, ReasonCredentialsMissing)
	}
	token, ok := jsonStringAt(doc, spec.Field)
	if !ok || token == "" {
		return UsageCredential{}, doc, usageError("", UsageKindReauth, ReasonCredentialsMissing)
	}
	return NewUsageCredential(token, usageFileSource), doc, nil
}

// envValue returns the value of name in base, "" when unset. The environment is
// passed explicitly so callers control what a read can see.
func envValue(base []string, name string) string {
	for i := len(base) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(base[i], "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

// jsonStringAt walks a decoded JSON document along path and returns the string
// it names. ok is false when any step is missing or holds another type.
func jsonStringAt(doc map[string]any, path []string) (value string, ok bool) {
	var current any = doc
	for _, key := range path {
		object, isObject := current.(map[string]any)
		if !isObject {
			return "", false
		}
		current, ok = object[key]
		if !ok {
			return "", false
		}
	}
	value, ok = current.(string)
	return strings.TrimSpace(value), ok
}

// JSONStringAt is the exported form of the walk above, for an adapter that
// reads a non-secret sibling field of the credential document (a plan or a
// tier). Absent fields return "".
func JSONStringAt(doc map[string]any, path ...string) string {
	if doc == nil {
		return ""
	}
	value, _ := jsonStringAt(doc, path)
	return value
}

// UsageRequest is one read-only account-usage HTTP request.
type UsageRequest struct {
	// Runtime labels the error ("claude"). It is the only identity in a message.
	Runtime string
	// Endpoint is the absolute URL to GET.
	Endpoint string
	// Credential is sent as a bearer token only when Present.
	Credential UsageCredential
	// Header carries the vendor's non-secret protocol headers.
	Header http.Header
	// Timeout bounds the whole request; 0 means UsageTimeout.
	Timeout time.Duration
	// Client overrides the HTTP client. A test injects an httptest server's
	// client here; a host leaves it nil and gets the stdlib default.
	Client *http.Client
}

// FetchUsageJSON performs the GET and returns a 2xx body. Every other outcome
// is a *UsageError (see UsageKind), so a caller cannot mistake a rejected
// credential or a rate limiter for a quota reading of zero.
func FetchUsageJSON(ctx context.Context, req UsageRequest) ([]byte, error) {
	if req.Endpoint == "" {
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindUnsupported, Reason: ReasonEndpointUnavailable}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = UsageTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.Endpoint, nil)
	if err != nil {
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindUnsupported, Reason: ReasonEndpointUnavailable}
	}
	for key, values := range req.Header {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	if req.Credential.Present() {
		httpReq.Header.Set("Authorization", "Bearer "+req.Credential.Token())
	}
	httpReq.Header.Set("Accept", "application/json")

	client := req.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// A provider error wrapped in *url.Error can carry the URL, never the
		// Authorization header; the error text below is ours, not the transport's.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindTimeout}
		}
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindTransport}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindReauth, Reason: ReasonCredentialsRejected, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindRateLimited, Reason: ReasonRequestRejected, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindUnsupported, Reason: ReasonEndpointUnavailable, Status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindTransport, Status: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, usageBodyLimit+1))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindTimeout}
		}
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindTransport, Status: resp.StatusCode}
	}
	if len(body) == 0 || len(body) > usageBodyLimit {
		return nil, &UsageError{Runtime: req.Runtime, Kind: UsageKindMalformed, Reason: ReasonQuotaUnavailable, Status: resp.StatusCode}
	}
	return body, nil
}

// PercentWindow turns a vendor percent reading into one window. It reports
// false for a reading outside [0,100] or a non-finite one: an unusable entry is
// skipped, never clamped into a value that would read as a real measurement.
// resetsAt is normalised to UTC; nil stays nil (the vendor reported no reset).
func PercentWindow(name string, percent float64, resetsAt *time.Time) (oarlock.UsageWindow, bool) {
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return oarlock.UsageWindow{}, false
	}
	var reset *time.Time
	if resetsAt != nil {
		utc := resetsAt.UTC()
		reset = &utc
	}
	return oarlock.UsageWindow{Name: name, Used: percent, Limit: 100, Unit: "percent", ResetsAt: reset}, true
}

// ParseInstant reads a vendor RFC 3339 timestamp. An absent or unreadable value
// is nil (no reset time), never the zero time: OAR maps an invalid resetsAt to
// "the runtime did not report one".
func ParseInstant(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

// ParseEpochSeconds reads a vendor unix-seconds reading. A zero or negative
// value is nil, for the same reason as ParseInstant.
func ParseEpochSeconds(seconds float64) *time.Time {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return nil
	}
	reset := time.Unix(int64(seconds), 0).UTC()
	return &reset
}
