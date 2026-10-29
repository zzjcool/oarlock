package claude

// A failed turn, classified from what claude says (OAR runtimes/claude/
// failure.ts, observed on 2.1.292): the category on the turn's error
// `assistant` frame (`error`), then the result's `terminal_reason` and
// `api_error_status`. The spec's four families map as: auth → auth, rate_limit →
// rate_limited (or quota when a subscription limit names a reset),
// context_overflow → input_too_large, everything else → the provider status or
// unknown. Prose is the documented last resort and only where claude has no
// structured field for it.

import (
	"regexp"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

var categories = map[string]oarlock.FailureClass{
	"authentication_failed": oarlock.FailureAuth,
	"model_not_found":       oarlock.FailureModelUnavail,
	"rate_limit":            oarlock.FailureRateLimited,
	"billing_error":         oarlock.FailureBilling,
	"invalid_request":       oarlock.FailureInvalidRequest,
}

// Result subtypes for a limit or schema the host set: its own options stopped
// the turn.
var subtypes = map[string]oarlock.FailureClass{
	"error_max_turns":                     oarlock.FailureLimitReached,
	"error_max_budget_usd":                oarlock.FailureLimitReached,
	"error_max_structured_output_retries": oarlock.FailureOutputSchema,
}

var (
	creditLow   = regexp.MustCompile(`(?i)credit balance is too low`)
	usageLimit  = regexp.MustCompile(`(?i)reached your specified API usage limits`)
	tooLongText = regexp.MustCompile(`(?i)prompt is too long|exceeds the context window|context length`)
)

// failureFacts is what claude's error frames say about one failed turn.
type failureFacts struct {
	category       string     // the error of the turn's last root assistant frame
	status         int        // result.api_error_status; 0 = no request was sent
	terminalReason string     // result.terminal_reason
	subtype        string     // result.subtype when it is not success
	limitResetsAt  *time.Time // the reset of a limit that refuses requests
}

// limitReset reads a rate_limit_event's rate_limit_info: a `rejected` status
// with a `resetsAt` (unix seconds) and no paid overage still taking requests.
// nil when the limit refuses nothing or names no reset.
func limitReset(info sj.Rec) *time.Time {
	if sj.Str(info["status"]) != "rejected" {
		return nil
	}
	if o := sj.Str(info["overageStatus"]); o == "allowed" || o == "allowed_warning" {
		return nil
	}
	secs, ok := sj.AsNum(info["resetsAt"])
	if !ok {
		return nil
	}
	t := time.Unix(int64(secs), int64((secs-float64(int64(secs)))*1e9)).UTC()
	return &t
}

// resultReason is an error result's own words: the subtype, then claude's
// `errors` text when it gives any.
func resultReason(subtype string, errs any) string {
	var parts []string
	for _, e := range sj.AsList(errs) {
		if s, ok := sj.AsStr(e); ok && s != "" {
			parts = append(parts, s)
		}
	}
	detail := strings.Join(parts, "; ")
	switch {
	case subtype == "" && detail == "":
		return "error"
	case subtype == "":
		return detail
	case detail == "":
		return subtype
	}
	return subtype + ": " + detail
}

func failureFromStatus(status int) oarlock.FailureClass {
	switch status {
	case 400:
		return oarlock.FailureInvalidRequest
	case 401:
		return oarlock.FailureAuth
	case 402:
		return oarlock.FailureBilling
	case 404:
		return oarlock.FailureModelUnavail
	case 429:
		return oarlock.FailureRateLimited
	case 503, 529:
		return oarlock.FailureOverloaded
	}
	if status >= 500 && status < 600 {
		return oarlock.FailureProvider
	}
	return oarlock.FailureUnknown
}

// anthropicLimitText: Anthropic's 400s with no type of their own.
func anthropicLimitText(msg string) (oarlock.FailureClass, bool) {
	switch {
	case creditLow.MatchString(msg):
		return oarlock.FailureBilling, true
	case usageLimit.MatchString(msg):
		return oarlock.FailureQuota, true
	}
	return "", false
}

// classify builds the failed outcome of a result whose is_error is true.
func classify(reason string, f failureFacts) *oarlock.TurnOutcome {
	out := &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Reason: reason, Status: f.status}
	fail := func(c oarlock.FailureClass) *oarlock.TurnOutcome { out.Failure = c; return out }

	if f.terminalReason == "prompt_too_long" {
		return fail(oarlock.FailureInputTooLarge)
	}
	if c, ok := subtypes[f.subtype]; ok {
		return fail(c)
	}
	if f.category == "authentication_failed" {
		out.Failure = oarlock.FailureAuth
		if f.status == 0 {
			out.Credential = "missing" // no request sent: no credential at all
		} else if f.status == 401 {
			out.Credential = "rejected"
		}
		return out
	}
	if f.category == "rate_limit" && f.limitResetsAt != nil {
		// A subscription limit refuses requests until a reset claude named.
		out.ResetsAt = f.limitResetsAt
		return fail(oarlock.FailureQuota)
	}
	if c, ok := categories[f.category]; ok {
		return fail(c)
	}
	if f.status == 0 {
		if tooLongText.MatchString(reason) {
			return fail(oarlock.FailureInputTooLarge)
		}
		return fail(oarlock.FailureUnknown)
	}
	if f.status == 400 {
		if c, ok := anthropicLimitText(reason); ok {
			return fail(c)
		}
		if tooLongText.MatchString(reason) {
			return fail(oarlock.FailureInputTooLarge)
		}
		return fail(oarlock.FailureInvalidRequest)
	}
	return fail(failureFromStatus(f.status))
}
