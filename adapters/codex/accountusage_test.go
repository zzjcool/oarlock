package codex

// Tests for the account-usage read (accountusage.go).
//
// Two halves. The projection is pure: a payload in, a snapshot or a typed
// failure out, no process. The transport is exercised through a fake `codex`
// that is a `#!/bin/sh` script on PATH, logs every JSON-RPC line it is handed
// and answers by method, so a test can prove which requests were sent without a
// codex binary, a login or a network. Windows skips the script tests and only
// compile-checks, like adapters/codex/ops_test.go.
//
// The vendor's reply is never executed and no real codex is ever started: the
// fake is first on a narrowed PATH.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// oarRateLimitsPayload is the representative `account/rateLimits/read` result
// from OAR's runtimes/codex/account-usage.ts comment block: the historical
// single-bucket view plus the multi-bucket view, in which `codex` appears twice
// with the same content, and the secondary window of `codex` is null.
const oarRateLimitsPayload = `{
  "rateLimits": {
    "limitId": "codex",
    "limitName": null,
    "primary": { "usedPercent": 50, "windowDurationMins": 10080, "resetsAt": 1787820410 },
    "secondary": null,
    "planType": "pro",
    "rateLimitReachedType": null
  },
  "rateLimitsByLimitId": {
    "codex_bengalfox": {
      "limitId": "codex_bengalfox",
      "limitName": "GPT-5.3-Codex-Spark",
      "primary": { "usedPercent": 0, "windowDurationMins": 300, "resetsAt": 1787322278 },
      "secondary": { "usedPercent": 0, "windowDurationMins": 10080, "resetsAt": 1787909078 },
      "planType": "pro"
    },
    "codex": {
      "limitId": "codex",
      "limitName": null,
      "primary": { "usedPercent": 50, "windowDurationMins": 10080, "resetsAt": 1787820410 },
      "secondary": null,
      "planType": "pro",
      "rateLimitReachedType": null
    }
  }
}`

// wantUsageWindow is one expected window of the projection.
type wantUsageWindow struct {
	name  string
	used  float64
	reset int64 // unix seconds; 0 = no reset reported
}

// assertWindows compares windows field by field: name, usage as a percentage of
// the vendor's own quota, and a UTC reset (nil when the vendor reported none).
func assertWindows(t *testing.T, got []oarlock.UsageWindow, want []wantUsageWindow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("windows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, expected := range want {
		window := got[i]
		if window.Name != expected.name {
			t.Fatalf("window %d name = %q, want %q", i, window.Name, expected.name)
		}
		if window.Used != expected.used || window.Limit != 100 || window.Unit != "percent" {
			t.Fatalf("window %d = %+v, want used %v of 100 percent", i, window, expected.used)
		}
		if expected.reset == 0 {
			if window.ResetsAt != nil {
				t.Fatalf("window %d resetsAt = %v, want none", i, window.ResetsAt)
			}
			continue
		}
		if window.ResetsAt == nil {
			t.Fatalf("window %d resetsAt = nil, want %d", i, expected.reset)
		}
		if want := time.Unix(expected.reset, 0).UTC(); !window.ResetsAt.Equal(want) || window.ResetsAt.Location() != time.UTC {
			t.Fatalf("window %d resetsAt = %v, want %v in UTC", i, window.ResetsAt, want)
		}
	}
}

// assertUsageError requires a *ops.UsageError of kind and reason, and no
// snapshot next to it: a failed read must never look like an empty account.
func assertUsageError(t *testing.T, usage oarlock.AccountUsage, err error, kind ops.UsageErrorKind, reason string) *ops.UsageError {
	t.Helper()
	var usageErr *ops.UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("err = %v (%T), want *ops.UsageError", err, err)
	}
	if usageErr.Kind != kind {
		t.Fatalf("kind = %q, want %q (err = %v)", usageErr.Kind, kind, err)
	}
	if reason != "" && usageErr.Reason != reason {
		t.Fatalf("reason = %q, want %q (err = %v)", usageErr.Reason, reason, err)
	}
	if usageErr.Runtime != usageRuntime {
		t.Fatalf("runtime = %q, want %q", usageErr.Runtime, usageRuntime)
	}
	if usage.Plan != "" || usage.Windows != nil {
		t.Fatalf("usage = %+v next to an error, want the zero snapshot", usage)
	}
	return usageErr
}

// TestProjectCodexUsageMirrorsOARPayload is case (a): the OAR payload projects
// to plan "pro" and three windows - the duplicate `codex` bucket of the indexed
// view is dropped, and the null secondary of the historical bucket adds none.
func TestProjectCodexUsageMirrorsOARPayload(t *testing.T) {
	usage, err := projectCodexUsage([]byte(oarRateLimitsPayload))
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if usage.Plan != "pro" {
		t.Fatalf("plan = %q, want pro", usage.Plan)
	}
	assertWindows(t, usage.Windows, []wantUsageWindow{
		// Historical bucket first; limitName is null, so limitId "codex" names
		// it (OAR renders that one id as "Codex").
		{name: "Codex · primary", used: 50, reset: 1787820410},
		// Then the indexed view in wire order: primary before secondary.
		{name: "GPT-5.3-Codex-Spark · primary", used: 0, reset: 1787322278},
		{name: "GPT-5.3-Codex-Spark · secondary", used: 0, reset: 1787909078},
	})
}

// TestProjectCodexUsageNamesAnAnonymousBucketByItsDuration pins the one label
// fallback: a bucket with no limitName and no limitId is named by the window's
// own duration (OAR's windowLabel).
func TestProjectCodexUsageNamesAnAnonymousBucketByItsDuration(t *testing.T) {
	payload := `{"rateLimits": {
		"primary": {"usedPercent": 12.5, "windowDurationMins": 300},
		"secondary": {"usedPercent": 99, "windowDurationMins": 10080}
	}}`
	usage, err := projectCodexUsage([]byte(payload))
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if usage.Plan != "" {
		t.Fatalf("plan = %q, want none", usage.Plan)
	}
	assertWindows(t, usage.Windows, []wantUsageWindow{
		{name: "5 hours · primary", used: 12.5},
		{name: "1 week · secondary", used: 99},
	})
}

// TestProjectCodexUsageSkipsUnusableWindows pins the per-window rules: a
// usedPercent outside [0,100], a non-numeric one and an absent one are skipped
// instead of being read as a measurement.
func TestProjectCodexUsageSkipsUnusableWindows(t *testing.T) {
	payload := `{"rateLimits": {
		"limitId": "codex",
		"planType": "plus",
		"primary": {"usedPercent": 100, "windowDurationMins": 60, "resetsAt": 0},
		"secondary": {"usedPercent": 101, "windowDurationMins": 60}
	}, "rateLimitsByLimitId": {
		"extra": {"limitId": "extra", "primary": {"usedPercent": "50", "windowDurationMins": 60}},
		"other": {"limitId": "other", "primary": {"usedPercent": -1, "windowDurationMins": 60}}
	}}`
	usage, err := projectCodexUsage([]byte(payload))
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if usage.Plan != "plus" {
		t.Fatalf("plan = %q, want plus", usage.Plan)
	}
	// usedPercent 100 is inside [0,100] and therefore a real reading; resetsAt
	// 0 is "no reset" (ops.ParseEpochSeconds), never 1970.
	assertWindows(t, usage.Windows, []wantUsageWindow{{name: "Codex · primary", used: 100}})
}

// TestProjectCodexUsageRejectsAPayloadWithoutWindows is case (b): a payload
// that projects to zero windows is malformed/quota_unavailable, and the
// returned snapshot stays zero even when the payload named a plan.
func TestProjectCodexUsageRejectsAPayloadWithoutWindows(t *testing.T) {
	for name, payload := range map[string]string{
		"empty object":    `{}`,
		"null buckets":    `{"rateLimits": null, "rateLimitsByLimitId": null}`,
		"null windows":    `{"rateLimits": {"limitId": "codex", "planType": "pro", "primary": null, "secondary": null}}`,
		"out of range":    `{"rateLimits": {"limitId": "codex", "planType": "pro", "primary": {"usedPercent": 150}}}`,
		"no usedPercent":  `{"rateLimits": {"limitId": "codex", "primary": {"windowDurationMins": 60}}}`,
		"unreadable JSON": `{"rateLimits":`,
		"not an object":   `[1, 2, 3]`,
	} {
		t.Run(name, func(t *testing.T) {
			usage, err := projectCodexUsage([]byte(payload))
			assertUsageError(t, usage, err, ops.UsageKindMalformed, ops.ReasonQuotaUnavailable)
		})
	}
}

// ---- transport -----------------------------------------------------------

// usageFakeServer puts a fake `codex` on PATH whose app-server answers by
// method. Every request line it is handed is appended to <script>.requests, so
// a test can prove what was sent; the script logs its own arguments through
// fakeCodexScript. A method without an answer line is not answered at all.
func usageFakeServer(t *testing.T, answers map[string]string) (log, requests string) {
	t.Helper()
	methods := make([]string, 0, len(answers))
	for method := range answers {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	body := []string{
		`while IFS= read -r line; do`,
		`  printf '%s\n' "$line" >> "$0.requests"`,
		`  case $line in`,
	}
	for _, method := range methods {
		body = append(body, `    *'"method":"`+method+`"'*) printf '%s\n' `+shQuote(answers[method])+` ;;`)
	}
	body = append(body, `  esac`, `done`)
	dir, log := fakeCodexScript(t, strings.Join(body, "\n"))
	return log, filepath.Join(dir, executableName+".requests")
}

// compactPayload is the OAR payload on one line: one JSON-RPC frame per line is
// the dialect codex speaks, and the fake prints exactly what it is given.
func compactPayload(t *testing.T) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, []byte(oarRateLimitsPayload)); err != nil {
		t.Fatalf("compact the payload: %v", err)
	}
	return buffer.String()
}

// usageAnswers is the happy-path app-server: initialize, account/read and the
// rate limits, with the ids this reader assigns (1, 2, 3).
func usageAnswers(t *testing.T, rateLimits string) map[string]string {
	t.Helper()
	return map[string]string{
		`initialize`:              `{"id": 1, "result": {"userAgent": "codex-cli 0.165.0"}}`,
		`account/read`:            `{"id": 2, "result": {"account": {"type": "chatgpt", "email": "usage@example.com"}, "requiresOpenaiAuth": true}}`,
		`account/rateLimits/read`: rateLimits,
	}
}

// readRequests returns the request lines the fake logged.
func readRequests(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request log %s: %v", path, err)
	}
	return string(raw)
}

// TestAccountUsageReadsRateLimitsFromTheAppServer is case (c): the zero-value
// reader finds the fake `codex` on PATH, drives initialize -> initialized ->
// account/read -> account/rateLimits/read, and projects the reply.
func TestAccountUsageReadsRateLimitsFromTheAppServer(t *testing.T) {
	log, requests := usageFakeServer(t, usageAnswers(t, `{"id": 3, "result": `+compactPayload(t)+`}`))

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if usage.Plan != "pro" {
		t.Fatalf("plan = %q, want pro", usage.Plan)
	}
	assertWindows(t, usage.Windows, []wantUsageWindow{
		{name: "Codex · primary", used: 50, reset: 1787820410},
		{name: "GPT-5.3-Codex-Spark · primary", used: 0, reset: 1787322278},
		{name: "GPT-5.3-Codex-Spark · secondary", used: 0, reset: 1787909078},
	})

	// The native surface, invoked the way open.go invokes it.
	if lines := logLines(t, log); len(lines) != 1 || lines[0] != "app-server --listen stdio://" {
		t.Fatalf("invocations = %q, want exactly `app-server --listen stdio://`", lines)
	}

	// The handshake and both reads really were sent: this is the evidence that
	// the projection above came from codex's own account/rateLimits/read.
	sent := readRequests(t, requests)
	for _, want := range []string{
		`"method":"initialize"`,
		`"clientInfo":{"name":"oarlock","version":"0.0.0"}`,
		`"capabilities":{"experimentalApi":true}`,
		`"method":"initialized"`,
		`"method":"account/read"`,
		`"method":"account/rateLimits/read"`,
	} {
		if !strings.Contains(sent, want) {
			t.Fatalf("requests do not carry %s:\n%s", want, sent)
		}
	}
	// The initialized notification is a notification: no id, no reply expected.
	for _, line := range strings.Split(strings.TrimSpace(sent), "\n") {
		if strings.Contains(line, `"method":"initialized"`) && strings.Contains(line, `"id"`) {
			t.Fatalf("initialized was sent as a request: %s", line)
		}
	}
}

// TestAccountUsageToleratesAMissingAccountRead covers the older build that has
// no account/read: its refusal must not cost the rate-limit read.
func TestAccountUsageToleratesAMissingAccountRead(t *testing.T) {
	answers := usageAnswers(t, `{"id": 3, "result": `+compactPayload(t)+`}`)
	answers[`account/read`] = `{"id": 2, "error": {"code": -32601, "message": "method not found: account/read"}}`
	usageFakeServer(t, answers)

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if usage.Plan != "pro" || len(usage.Windows) != 3 {
		t.Fatalf("usage = %+v, want the rate limits of the payload", usage)
	}
}

// TestAccountUsageMapsAuthenticationRequiredToReauth is case (d): the
// app-server's own "ChatGPT authentication required" is a login problem, and
// the vendor text never reaches the caller.
func TestAccountUsageMapsAuthenticationRequiredToReauth(t *testing.T) {
	answers := usageAnswers(t, `{"id": 3, "result": {}}`)
	answers[`account/rateLimits/read`] = `{"id": 3, "error": {"code": -32000, "message": "ChatGPT authentication required"}}`
	usageFakeServer(t, answers)

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	usageErr := assertUsageError(t, usage, err, ops.UsageKindReauth, ops.ReasonNotAuthenticated)
	if strings.Contains(usageErr.Error(), "ChatGPT") || strings.Contains(usageErr.Error(), "authentication") {
		t.Fatalf("the error quotes the vendor: %v", usageErr)
	}
}

// TestAccountUsageMapsAnUnknownMethodToUnsupported covers a build without the
// rate-limit request at all.
func TestAccountUsageMapsAnUnknownMethodToUnsupported(t *testing.T) {
	answers := usageAnswers(t, `{"id": 3, "result": {}}`)
	answers[`account/rateLimits/read`] = `{"id": 3, "error": {"code": -32601, "message": "method not found"}}`
	usageFakeServer(t, answers)

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	assertUsageError(t, usage, err, ops.UsageKindUnsupported, ops.ReasonEndpointUnavailable)
}

// TestAccountUsageMapsAnEarlyExitToTransport is case (e): a codex that starts
// and dies without a reply is a transport failure, not an empty account.
func TestAccountUsageMapsAnEarlyExitToTransport(t *testing.T) {
	fakeCodexScript(t, "exit 1")

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	assertUsageError(t, usage, err, ops.UsageKindTransport, "")

	// The same classification for a codex that cannot be started at all.
	missing := accountUsageReader{Command: filepath.Join(t.TempDir(), "no-such-codex")}
	usage, err = missing.AccountUsage(context.Background())
	assertUsageError(t, usage, err, ops.UsageKindTransport, "")
}

// TestAccountUsageTimesOut is case (f): a read that outlives its bound is a
// timeout and returns promptly (the elapsed check is a bound, not proof that the
// child was reaped — the fake has no way to observe its own death).
func TestAccountUsageTimesOut(t *testing.T) {
	fakeCodexScript(t, "sleep 30")

	start := time.Now()
	usage, err := accountUsageReader{Timeout: 100 * time.Millisecond}.AccountUsage(context.Background())
	assertUsageError(t, usage, err, ops.UsageKindTimeout, "")
	// The bound plus the kill grace, not the sleep: the reader stopped the tree.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the read took %v, far past its 100ms bound", elapsed)
	}
}

// TestAccountUsageRefusesANonSubscriptionAccount is the fix for the review's
// one blocker: codex answers the rate-limit endpoint with "ChatGPT
// authentication required" when the machine is signed in with an API key, so
// reporting that as reauth would send the host after a login that cannot fix
// the state. The account/read reply is the evidence, and each refused shape is
// checked separately.
func TestAccountUsageRefusesANonSubscriptionAccount(t *testing.T) {
	cases := map[string]string{
		"api key":              `{"id": 2, "result": {"account": {"type": "apiKey"}, "requiresOpenaiAuth": true}}`,
		"amazon bedrock":       `{"id": 2, "result": {"account": {"type": "amazonBedrock"}, "requiresOpenaiAuth": true}}`,
		"no openai auth asked": `{"id": 2, "result": {"account": {"type": "chatgpt"}, "requiresOpenaiAuth": false}}`,
	}
	for name, accountRead := range cases {
		t.Run(name, func(t *testing.T) {
			answers := usageAnswers(t, `{"id": 3, "result": `+compactPayload(t)+`}`)
			answers[`account/read`] = accountRead
			_, requests := usageFakeServer(t, answers)

			usage, err := accountUsageReader{}.AccountUsage(context.Background())
			assertUsageError(t, usage, err, ops.UsageKindUnsupported, ops.ReasonUnsupportedAuthMode)
			// The gate must run BEFORE the rate-limit read: a reordering that
			// still discarded the reply would otherwise keep this test green.
			if sent := readRequests(t, requests); strings.Contains(sent, `"method":"account/rateLimits/read"`) {
				t.Fatalf("a refused account still asked for rate limits:\n%s", sent)
			}
			if len(usage.Windows) != 0 || usage.Plan != "" {
				t.Fatalf("a refused account returned usage: %+v", usage)
			}
		})
	}

	// Positive subscription evidence keeps the read: a chatgpt account with
	// auth required is the happy path the other tests already cover, and an
	// unreadable account/read result is not positive evidence either.
	for name, accountRead := range map[string]string{
		"chatgpt account": `{"id": 2, "result": {"account": {"type": "chatgpt"}, "requiresOpenaiAuth": true}}`,
		"unreadable":      `{"id": 2, "result": "not an object"}`,
	} {
		t.Run(name, func(t *testing.T) {
			answers := usageAnswers(t, `{"id": 3, "result": `+compactPayload(t)+`}`)
			answers[`account/read`] = accountRead
			usageFakeServer(t, answers)
			usage, err := accountUsageReader{}.AccountUsage(context.Background())
			if err != nil {
				t.Fatalf("AccountUsage: %v", err)
			}
			if usage.Plan != "pro" {
				t.Fatalf("plan = %q, want pro", usage.Plan)
			}
		})
	}
}

// TestAccountUsageSkipsNotificationsAndServerRequests is the branch a real
// app-server exercises constantly: codex pushes notification frames
// (account/updated, account/rateLimits/updated) and may ask a server request
// while our replies are pending. A frame without our id must be skipped, not
// mistaken for a reply, and a non-JSON line must be skipped too.
func TestAccountUsageSkipsNotificationsAndServerRequests(t *testing.T) {
	answers := usageAnswers(t, `{"id": 3, "result": `+compactPayload(t)+`}`)
	// The fake prints these lines before the initialize reply, so the reader
	// meets them while its first request is outstanding.
	answers["initialize"] = `{"method": "account/updated", "params": {}}` + "\n" +
		`{"id": 99, "method": "account/rateLimits/updated", "params": {}}` + "\n" +
		`not json at all` + "\n" +
		`{"id": 1, "result": {"userAgent": "codex-cli 0.165.0"}}`
	usageFakeServer(t, answers)

	usage, err := accountUsageReader{}.AccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if usage.Plan != "pro" || len(usage.Windows) != 3 {
		t.Fatalf("usage = %+v, want the rate limits of the payload", usage)
	}
}

// TestAccountUsageRefusesANilContext pins the guard every other entry point in
// oarlock has: a nil context is an error, not a panic inside the transport.
func TestAccountUsageRefusesANilContext(t *testing.T) {
	usage, err := accountUsageReader{}.AccountUsage(nil)
	assertUsageError(t, usage, err, ops.UsageKindTransport, "")
}
