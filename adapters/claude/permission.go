package claude

// Permission answers: the runtime→app half of the control surface (issue #53).
//
// A claude control_request is a question the runtime asks the app. Ingress
// records it as a toApp request and remembers its payload; the host answers it
// with Answer, which speaks claude's own control_response dialect and records
// the answer as an `answered` response. Nothing is invented here: a request
// nobody answers just stays unanswered in the stream.
//
// The claude permission shape is UNVERIFIED LIVE: headless claude runs with
// --dangerously-skip-permissions (args.go:21-29), so a can_use_tool request
// cannot arrive from the sessions this adapter starts; the reply shapes below
// follow OAR's qwen/qoder stream-json fixtures, not a claude recording. Because
// of that, an unanswered request is the honest cancelled tier: the adapter never
// fabricates a deny, and answering automatically is opt-in (PermissionAllow,
// see SessionOptions.Permissions).

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// ErrUnknownRequest is returned by Answer for an id that is not an unanswered
// runtime request of this session: it was never asked, or a
// control_cancel_request already withdrew it.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
var ErrUnknownRequest = errors.New("claude: no unanswered runtime request with that id")

// ErrAlreadyAnswered is returned by Answer for a request that was answered
// once. A control request is answerable exactly once, so the second answer is
// refused before anything reaches the wire.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
var ErrAlreadyAnswered = errors.New("claude: the runtime request is already answered")

// PermissionPolicy is how the adapter treats a runtime permission request
// (can_use_tool). The zero value is the default and answers nothing.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
type PermissionPolicy string

const (
	// PermissionRecordOnly (zero value): a runtime request is recorded as a toApp
	// request and never answered. This is the default because the claude
	// permission shape is unverified live — headless claude runs with
	// --dangerously-skip-permissions (args.go:21-29) and sends no can_use_tool,
	// and an invented allow would be a claim the runtime never made. An
	// unanswered request is the honest cancelled tier, never a fabricated deny.
	PermissionRecordOnly PermissionPolicy = ""
	// PermissionAllow: the adapter answers a can_use_tool with the broadest allow
	// the request offers (allow_always > allow_once > no answer at all). Opt-in:
	// the claude reply shape is unverified live (headless claude runs with
	// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29), so
	// a host that turns this on is choosing the OAR qwen/qoder dialect on
	// claude's behalf. An unanswered request is the honest cancelled tier:
	// nothing to allow means NO answer, never a fabricated deny.
	PermissionAllow PermissionPolicy = "allow"
)

// permissionOption is one native option a request offers: the kind the ladder
// ranks (allow_always / allow_once) and the id the runtime gave it, which the
// reply must name back.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
type permissionOption struct{ kind, id string }

// Known option kinds, in the ladder's order. The claude permission shape is
// unverified live (headless claude runs with --dangerously-skip-permissions and
// sends no can_use_tool, args.go:21-29); an unanswered request is the honest
// cancelled tier, never a fabricated deny.
const (
	kindAllowAlways = "allow_always"
	kindAllowOnce   = "allow_once"
)

// choosePermission picks the broadest allow among the options offered:
// allow_always, else allow_once, else nothing to allow (ok=false — the
// cancelled tier, where the request stays unanswered rather than being denied).
// Ties keep the FIRST matching option: the runtime's own order is the only
// preference information available.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func choosePermission(options []permissionOption) (permissionOption, bool) {
	var once permissionOption
	foundOnce := false
	for _, o := range options {
		switch o.kind {
		case kindAllowAlways:
			return o, true
		case kindAllowOnce:
			if !foundOnce {
				once, foundOnce = o, true
			}
		}
	}
	if foundOnce {
		return once, true
	}
	return permissionOption{}, false
}

// Answer replies to a pending runtime→app request with the runtime's own
// response shape and records an `answered` response. The reply is written first;
// the record exists only when the write succeeded (never a fabricated answer),
// and a write that fails rolls the answer-once gate back, so the request is
// still answerable.
//
// Order of the gates: ctx, then reachability (k.Unreachable: exit or dispose →
// its error), then the pending checks under s.mu — a request already in
// s.answered is ErrAlreadyAnswered, an id that is not in s.runtimeReqs is
// ErrUnknownRequest. Both are refused before anything is written, so a request
// cannot be answered twice.
//
// The line is claude's own dialect, `result` marshalled verbatim:
//
//	{"type":"control_response","response":{"subtype":"success","request_id":<id>,"response":<result>}}
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func (s *Session) Answer(ctx context.Context, requestID string, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if un := s.k.Unreachable(); un != nil {
		return un
	}

	s.mu.Lock()
	request, pending := s.runtimeReqs[requestID]
	if s.answered[requestID] {
		s.mu.Unlock()
		return ErrAlreadyAnswered
	}
	if !pending {
		s.mu.Unlock()
		return ErrUnknownRequest
	}
	s.answered[requestID] = true
	delete(s.runtimeReqs, requestID)
	s.mu.Unlock()

	native, err := json.Marshal(result)
	if err != nil {
		s.unanswerPermission(requestID, request)
		return err
	}
	line, err := json.Marshal(controlResponse{
		Type:     "control_response",
		Response: controlResponseBody{Subtype: "success", RequestID: requestID, Response: native},
	})
	if err != nil {
		s.unanswerPermission(requestID, request)
		return err
	}
	if err := s.conn.Write(ctx, append(line, '\n')); err != nil {
		s.unanswerPermission(requestID, request)
		return err
	}
	s.k.Respond(requestID, oarlock.ResponseBody{Kind: oarlock.ResponseAnswered, Native: native}, kernel.RecordAt{})
	return nil
}

// controlResponse is claude's answer to a control_request: the subtype, the
// request id it answers and the result verbatim. A struct (not a map) so the
// line has one stable field order.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
type controlResponse struct {
	Type     string              `json:"type"`
	Response controlResponseBody `json:"response"`
}

// controlResponseBody is the inner "response" object of a control_response.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
type controlResponseBody struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
}

// unanswerPermission rolls the answer-once gate back: the answer never reached
// the wire, so the request is pending again.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func (s *Session) unanswerPermission(requestID string, request sj.Rec) {
	s.mu.Lock()
	delete(s.answered, requestID)
	s.runtimeReqs[requestID] = request
	s.mu.Unlock()
}

// answerPermission is the policy hook ingest calls for a runtime control_request.
// It returns true only when an answer was written and recorded.
//
// Record-only (the default) and every other policy value answer nothing: an
// unanswered request is the honest cancelled tier, and the request stays in the
// stream for the host to answer through Answer.
//
// Under PermissionAllow, a can_use_tool is answered with permissionReply. The
// claude dialect has no allow_always reply we can prove, so with no options list
// the allow tier is the one-shot allow; when an options list offers nothing
// allowed, nothing is answered. The answer goes through the same Answer path, so
// the record cannot claim more than the wire saw. It is written with
// context.Background() — an ingest answer must not be cancelled by whatever
// context happens to be current — so the ctx parameter only marks where this
// hook sits in ingest.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func (s *Session) answerPermission(ctx context.Context, requestID string, request sj.Rec) bool {
	if s.perm != PermissionAllow {
		return false
	}
	body := requestPayload(request)
	if sj.Str(body["subtype"]) != "can_use_tool" {
		return false
	}
	reply, ok := permissionReply(body)
	if !ok {
		return false
	}
	return s.Answer(context.Background(), requestID, reply) == nil
}

// requestPayload is the control_request's own body — the object that carries
// subtype, input and options. Ingest keeps the native frame in s.runtimeReqs
// (`{"type":"control_request","request":{…}}`) and hands that same record
// here, while a caller holding the body alone passes it directly: both spell the
// same request, so both are read.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func requestPayload(rec sj.Rec) sj.Rec {
	if body := sj.AsRec(rec["request"]); body != nil {
		return body
	}
	return rec
}

// permissionReply is the pure half of the allow policy: the reply body for one
// can_use_tool request, and whether there is anything to answer at all.
//
// When the request carries an options list, the ladder picks the broadest allow
// and the reply names the chosen option id; nothing allowed → ok=false (the
// cancelled tier: NO answer, never a fabricated deny). With no options list the
// allow tier is the one-shot allow, carrying request.input back as updatedInput
// when it is an object.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func permissionReply(request sj.Rec) (any, bool) {
	reply := map[string]any{"behavior": "allow"}
	if input, ok := request["input"].(map[string]any); ok {
		reply["updatedInput"] = input
	}
	options, offered := permissionOptions(request["options"])
	if !offered {
		return reply, true
	}
	chosen, ok := choosePermission(options)
	if !ok {
		return nil, false
	}
	reply["optionId"] = chosen.id
	return reply, true
}

// permissionOptions reads a native options list: only a JSON list is one, and
// only entries that are objects carrying a `kind` string are options (anything
// else offers nothing to allow). ok=false means the request has no options list
// at all, i.e. the one-shot allow tier.
//
// The claude permission shape is unverified live (headless claude runs with
// --dangerously-skip-permissions and sends no can_use_tool, args.go:21-29); an
// unanswered request is the honest cancelled tier, never a fabricated deny.
func permissionOptions(v any) ([]permissionOption, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	options := make([]permissionOption, 0, len(list))
	for _, e := range list {
		m := sj.AsRec(e)
		if m == nil {
			continue
		}
		kind, ok := sj.AsStr(m["kind"])
		if !ok {
			continue
		}
		options = append(options, permissionOption{kind: kind, id: sj.Str(m["id"])})
	}
	return options, true
}
