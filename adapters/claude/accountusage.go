package claude

// Account usage (issue #89): the one read of Claude's own quota endpoint,
// GET https://api.anthropic.com/api/oauth/usage.
//
// Why an HTTP read: subscription quota is not on Claude's CLI surface. The only
// native query is the stream-json `get_usage` control request, which is marked
// experimental, changes shape between versions and needs a full `claude -p`
// session (spawn, handshake, teardown) to answer one number. The endpoint below
// is the one Claude's own client uses for the same figures and needs no
// session, no prompt and no writes, so an account-usage read cannot start the
// user's hooks or MCP servers to report a percentage.
//
// This projection is UNVERIFIED against a live subscription: the payload shape
// is the flat one OAR's pre-0.3.1 reader consumed (`five_hour`, `seven_day`,
// `seven_day_oauth_apps`, `seven_day_opus`, `seven_day_sonnet`, `extra_usage`,
// each with `utilization` and `resets_at`). A vendor that moves or renames a
// key makes this read fail as malformed/quota_unavailable — the honest answer —
// instead of reporting a window that no longer exists or a zero that reads as
// "nothing used yet".
//
// What the read is not: it is not a plan claim. Plan comes from the
// non-secret subscriptionType of the credential document, and stays empty when
// the token came from the environment (there is no document to read). A
// defaulted or guessed plan would become a real subscription claim in a host's
// record, so an absent one stays absent.
//
// Every failure is a *ops.UsageError raised by internal/ops (see the status
// mapping in FetchUsageJSON and the credential mapping in
// ResolveUsageCredential). This file deliberately adds no second status table:
// two tables would drift, and the seam is the one place where "401 means log in
// again" is written down. A read that yields no window is
// malformed/quota_unavailable, never an empty AccountUsage with a nil error: a
// caller must be able to tell "the endpoint said nothing" from "no quota is
// used".

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// claudeUsageTokenEnv is the escape hatch for an installation whose token
	// is not in Claude's document (a CI box, a different HOME, a container that
	// mounts the credential elsewhere). It is an override, not the default: the
	// vendor's own store stays the source of truth unless the environment says
	// otherwise, and a set-but-empty value does not count as a token.
	claudeUsageTokenEnv = "OARLOCK_CLAUDE_OAUTH_TOKEN"

	// claudeUsageEndpoint is Claude's quota endpoint, the address its own client
	// queries. It is a constant rather than a host-supplied URL so a read can
	// never be redirected to a third party by configuration; a test overrides
	// accountUsageReader.Endpoint instead.
	claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"

	// claudeUsageBetaHeader/-Value: the endpoint serves the OAuth usage payload
	// only to a client that declares the OAuth beta. Anthropic's own client
	// sends this pair, so it is protocol, not policy. It carries no secret and
	// is therefore safe to name in a test.
	claudeUsageBetaHeader = "anthropic-beta"
	claudeUsageBetaValue  = "oauth-2025-04-20"

	// claudeUsageRuntime labels every error this read produces. It matches
	// Runtime().ID, so a host that logs one failure sees which runtime raised it
	// without a lookup.
	claudeUsageRuntime = "claude"

	// claudeUsageCredentialDir/-Name is Claude's own credential document inside
	// the home directory it is read from, split into the two path segments so it
	// is composed the same way on every platform. Only the token and the
	// subscription type are taken from it; the refresh token and the expiry stay
	// untouched, because this adapter refreshes nothing.
	claudeUsageCredentialDir  = ".claude"
	claudeUsageCredentialName = ".credentials.json"
)

// claudeUsageTokenField is the JSON path of the bearer token inside the
// credential document, and claudeUsagePlanField the path of the non-secret
// subscription type next to it. They are paths into one document rather than
// two structs so the read stays in step with ops.ResolveUsageCredential, which
// walks the same document with the same list.
var (
	claudeUsageTokenField = []string{"claudeAiOauth", "accessToken"}
	claudeUsagePlanField  = []string{"claudeAiOauth", "subscriptionType"}
)

// accountUsageReader is Claude's AccountUsageReader. Its fields are the seam a
// test needs and nothing else: the endpoint, the HTTP client, the environment
// and the home directory a credential could come from, and the read bound. The
// zero value must be the production reader — a host that forgets to populate it
// still gets the real endpoint, the real environment and the shared timeout
// instead of a silently broken read.
type accountUsageReader struct {
	// Endpoint overrides the URL to GET; "" means claudeUsageEndpoint.
	Endpoint string
	// Client overrides the HTTP client; nil means the stdlib default. A test
	// injects an httptest server here, so no request can leave the process.
	Client *http.Client
	// Env is the environment the credential is resolved from; nil means
	// os.Environ(). It is passed explicitly so a test never mutates the process
	// environment and so a host can scope what a read may see.
	Env []string
	// Home is the directory the credential document is read from; "" means
	// os.UserHomeDir(). A read is built per call so a home directory that
	// changes at runtime is honoured.
	Home string
	// Timeout bounds the whole read; 0 means ops.UsageTimeout. A caller that
	// needs a shorter bound (a UI refresh) sets it here rather than wrapping the
	// context, because the seam's bound also covers reading the body.
	Timeout time.Duration
}

// The zero value is the production reader; this line fails the build if the
// method set ever stops satisfying the interface a host branches on.
var _ oarlock.AccountUsageReader = accountUsageReader{}

// AccountUsage reads Claude's quota snapshot once. It returns either a snapshot
// with at least one window or an error; there is no third outcome, because an
// empty snapshot with a nil error is indistinguishable from a real "no quota"
// answer downstream.
func (r accountUsageReader) AccountUsage(ctx context.Context) (oarlock.AccountUsage, error) {
	credential, document, err := r.credential()
	if err != nil {
		// A missing or unreadable credential is a login problem and is reported
		// as one, unmapped and unwrapped: the seam already classified it
		// (reauth/credentials_missing), and re-deriving that here would be a
		// second, drifting table.
		return oarlock.AccountUsage{}, err
	}

	body, err := ops.FetchUsageJSON(ctx, ops.UsageRequest{
		Runtime:    claudeUsageRuntime,
		Endpoint:   r.endpoint(),
		Credential: credential,
		Header:     http.Header{claudeUsageBetaHeader: {claudeUsageBetaValue}},
		Timeout:    r.Timeout,
		Client:     r.Client,
	})
	if err != nil {
		return oarlock.AccountUsage{}, err
	}

	// Plan is read from the document the token came from, never from the
	// response: the endpoint does not report a subscription tier, and an
	// environment-supplied token has no document, so Plan stays empty rather
	// than being guessed from the windows it produced.
	return claudeUsageSnapshot(body, ops.JSONStringAt(document, claudeUsagePlanField...))
}

// credential resolves the bearer token: the explicit environment variable
// first, then Claude's own document under the home directory in force. It is a
// method so the redaction test can inspect the very credential the reader would
// send instead of a reconstruction of it.
func (r accountUsageReader) credential() (ops.UsageCredential, map[string]any, error) {
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	spec := ops.CredentialSpec{
		Env:   claudeUsageTokenEnv,
		Field: claudeUsageTokenField,
	}
	if home := r.home(); home != "" {
		spec.Path = filepath.Join(home, claudeUsageCredentialDir, claudeUsageCredentialName)
	}
	// An empty Path (no home directory) leaves only the environment source; the
	// seam turns both-absent into reauth/credentials_missing, which is the true
	// answer for a process with no credential anywhere.
	return ops.ResolveUsageCredential(env, spec)
}

// home returns the directory the credential document is resolved under. A
// failed lookup yields "", so the read reports a missing credential instead of
// panicking or guessing at another path: on a box with no home directory there
// genuinely is no vendor credential.
func (r accountUsageReader) home() string {
	if r.Home != "" {
		return r.Home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// endpoint returns the URL to GET, defaulting to Claude's own.
func (r accountUsageReader) endpoint() string {
	if r.Endpoint != "" {
		return r.Endpoint
	}
	return claudeUsageEndpoint
}

// claudeUsageWindow is one quota entry of the answer. Utilization is a pointer
// so a missing value is distinguishable from a reported 0: 0% used is a real
// reading (a fresh window), while an absent field names no measurement at all
// and must not be published as zero. ResetsAt stays a string because the
// endpoint may report an instant this adapter cannot read; that degrades to
// "no reset time" (ops.ParseInstant) instead of failing a read whose percentage
// is still good. IsEnabled is a pointer for the same reason on the one entry
// whose presence is a boolean: a missing flag is not "disabled", it is
// "unstated", and extra usage is only reported when the endpoint says it is on.
type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
	IsEnabled   *bool    `json:"is_enabled"`
}

// claudeUsageJSON is the endpoint's flat payload: one key per quota window.
// Every field is a pointer so `null` (a window this account does not have, the
// usual way a non-Opus plan reports seven_day_opus) is skipped rather than
// projected as a window with no measurement.
type claudeUsageJSON struct {
	FiveHour          *claudeUsageWindow `json:"five_hour"`
	SevenDay          *claudeUsageWindow `json:"seven_day"`
	SevenDayOAuthApps *claudeUsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus      *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet    *claudeUsageWindow `json:"seven_day_sonnet"`
	ExtraUsage        *claudeUsageWindow `json:"extra_usage"`
}

// claudeUsageSnapshot projects the payload into the OAR snapshot. The order is
// fixed (session, all models, OAuth apps, Opus, Sonnet, extra usage) because a
// host renders the windows in the order it receives them, and a stable order is
// what makes two reads comparable; entries the account does not have, or whose
// percent is unusable, are skipped rather than filled in.
func claudeUsageSnapshot(body []byte, plan string) (oarlock.AccountUsage, error) {
	var payload claudeUsageJSON
	if err := json.Unmarshal(body, &payload); err != nil {
		return oarlock.AccountUsage{}, claudeUsageQuotaUnavailable()
	}
	windows := make([]oarlock.UsageWindow, 0, 6)
	windows = appendClaudeUsageWindow(windows, "Current session", payload.FiveHour)
	windows = appendClaudeUsageWindow(windows, "Current week (all models)", payload.SevenDay)
	windows = appendClaudeUsageWindow(windows, "Current week (OAuth apps)", payload.SevenDayOAuthApps)
	windows = appendClaudeUsageWindow(windows, "Current week (Opus)", payload.SevenDayOpus)
	windows = appendClaudeUsageWindow(windows, "Current week (Sonnet)", payload.SevenDaySonnet)
	if extra := payload.ExtraUsage; extra != nil && extra.IsEnabled != nil && *extra.IsEnabled {
		// Extra usage is spend above the plan's included quota, so it is only a
		// window when the account has it switched on; a disabled or unstated
		// flag names no quota to report.
		windows = appendClaudeUsageWindow(windows, "Extra usage", extra)
	}
	if len(windows) == 0 {
		// A readable body with no usable window is a missing answer, not an
		// empty one: it is reported as malformed so no caller can record "0%
		// used" for an account whose quota this read could not see.
		return oarlock.AccountUsage{}, claudeUsageQuotaUnavailable()
	}
	return oarlock.AccountUsage{Plan: plan, Windows: windows}, nil
}

// appendClaudeUsageWindow adds one window when the entry carries a percent the
// seam accepts. A nil entry, an absent utilization and an out-of-range percent
// are all skipped: ops.PercentWindow refuses a reading outside [0,100] instead
// of clamping it, because a clamped 100 would read as "exhausted" for a vendor
// value that said something else. resets_at is parsed only for an entry that is
// reported, so an unreadable instant costs the window its reset time, not its
// percentage.
func appendClaudeUsageWindow(windows []oarlock.UsageWindow, name string, entry *claudeUsageWindow) []oarlock.UsageWindow {
	if entry == nil || entry.Utilization == nil {
		return windows
	}
	window, ok := ops.PercentWindow(name, *entry.Utilization, ops.ParseInstant(entry.ResetsAt))
	if !ok {
		return windows
	}
	return append(windows, window)
}

// claudeUsageQuotaUnavailable is the single failure for "the endpoint answered,
// but the body carried no quota this adapter can project". It is built here, not
// in the seam, because only the adapter knows what its payload should have
// contained; the reason code is the stable one a host matches on.
func claudeUsageQuotaUnavailable() error {
	return &ops.UsageError{
		Runtime: claudeUsageRuntime,
		Kind:    ops.UsageKindMalformed,
		Reason:  ops.ReasonQuotaUnavailable,
	}
}
