// Package acp is the Agent Client Protocol family adapter: one client for six
// runtimes (grok, kimi, opencode, qwen, qoder, antigravity). The transport is
// github.com/coder/acp-go-sdk; everything this package adds is the record
// projection (wire.go, project.go), the control plane (session.go), the
// client-side handlers (permission.go, terminal.go) and, per runtime, a
// Profile that states only what that vendor does differently.
package acp

import (
	"context"
	"encoding/json"

	"github.com/zzjcool/oarlock"
)

// Profile is the per-runtime quirk table. The zero value of every optional
// member means "the protocol default"; a profile file only sets what differs.
type Profile struct {
	// ID is the runtime id and the default executable name.
	ID string
	// Args builds the arguments after the executable. LaunchArgs go where
	// LaunchArgsAt says (default: appended).
	Args func(opt oarlock.Options) []string
	// LaunchArgsBeforeLast inserts Options.LaunchArgs before the final
	// argument instead of after it (grok: before `stdio`).
	LaunchArgsBeforeLast bool

	// Attribution is the declared tier. Evidence says where it comes from; a
	// profile with no live recording says "unverified" in it. Tests pin both.
	Attribution oarlock.AttributionTier
	Evidence    string
	// Images overrides what `initialize` advertised when the vendor is known
	// to accept images anyway (grok). nil = trust initialize.
	Images *bool

	// Refused is the pre-open table (the same one Runtime.Refused declares).
	Refused oarlock.RefusedOptions
	// Validate refuses option combinations the table cannot express.
	Validate func(opt oarlock.Options) error
	// Select returns the profile to use for one installation (opencode v1/v2).
	Select func(inst oarlock.Installation) Profile

	// AuthMethod picks an `authenticate` method id from the initialize result.
	AuthMethod func(initialized map[string]any) string
	// InitializeMeta / SessionMeta / PromptMeta are vendor `_meta` payloads.
	InitializeMeta func(opt oarlock.Options) map[string]any
	SessionMeta    func(opt oarlock.Options) map[string]any
	PromptMeta     func(opt oarlock.Options) map[string]any
	// YoloMode applies `session/set_mode yolo` when the open advertises it.
	YoloMode bool
	// IsResumeNotFound reads a native resume refusal; nil never claims absence.
	IsResumeNotFound func(code int, data any) bool
	// ResumeAbsence: after a failed session/resume|load, a complete session/list
	// walk that does not contain the id is positive evidence of absence and may
	// be reported as *oarlock.SessionNotFound. false = never claim absence from a
	// list (the native refusal is returned unchanged).
	ResumeAbsence bool

	// SteerMeta makes the session a Steerer: the `_meta` of the steering
	// prompt RPC. nil = the runtime cannot steer and the session has no Steer.
	SteerMeta func() map[string]any
	// SteerSupersedes: a steer replaces the running prompt, so the cancelled
	// answer of the original is not a turn end (grok sendNow).
	SteerSupersedes bool
	// PromptTokens reads the tokens one prompt answer bills; PromptContext the
	// context fullness. Both read the raw result object.
	PromptTokens  func(result map[string]any) *oarlock.TokenTotals
	PromptContext func(result map[string]any) *oarlock.ContextUsage

	// Annotate attributes one inbound frame and may add events (child
	// sessions, vendor usage). It never invents what the frame does not say.
	Annotate func(f *FrameInfo)

	// TerminalShellCommand: the agent puts a whole shell line in `command`.
	TerminalShellCommand bool
}

// FrameInfo is what Annotate sees and edits for one inbound notification.
type FrameInfo struct {
	Method string
	Params map[string]any
	// Update is Params["update"] when it is an object.
	Update map[string]any
	// Root is the session the Session was opened for.
	Root string
	// SessionID and AgentPath are where the record will be attributed; the
	// defaults are the envelope's sessionId and the root agent.
	SessionID string
	AgentPath []string
	// Events are appended after the generic projection.
	Events []oarlock.Event

	bill func(key string, t oarlock.TokenTotals) []oarlock.Event
}

// Bill adds a vendor ledger that is independent of every prompt answer, once
// per native key, as the next cumulative usage event.
func (f *FrameInfo) Bill(key string, t oarlock.TokenTotals) {
	f.Events = append(f.Events, f.bill(key, t)...)
}

// Link reports runtime-stated lineage between two sessions.
func (f *FrameInfo) Link(parent, child, via string) {
	if parent != "" && child != "" && parent != child {
		f.Events = append(f.Events, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: parent, Child: child, Via: via})
	}
}

// Runtime builds the oarlock.Runtime for a profile. Probe, Install and Login
// are wired per id, so the three cannot disagree: the probe is the one every
// installer re-probes through, a profile without a vendor installer has no
// Install at all (antigravity), and a profile without a verified auth channel
// has no Login.
func (p Profile) Runtime() oarlock.Runtime {
	return oarlock.Runtime{
		ID:        p.ID,
		Session:   p.Start,
		Refused:   p.Refused,
		Probe:     probe(p.ID),
		Install:   installFor(p.ID),
		Login:     loginFor(p.ID),
		Inventory: inventories{id: p.ID},
	}
}

// Capabilities is the declaration before a session exists: the tier is known,
// images are not (they come from initialize).
func (p Profile) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{Attribution: p.Attribution}
}

// Start implements oarlock.StartSession.
func (p Profile) Start(ctx context.Context, inst oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
	return start(ctx, p, inst, opt)
}

var _ oarlock.StartSession = Profile{}.Start

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func ptr[T any](v T) *T { return &v }

// tokenParts reads the usual vendor token ledger keys.
func tokenParts(m map[string]any, in, out string) *oarlock.TokenTotals {
	i, ok1 := asInt64(m[in])
	o, ok2 := asInt64(m[out])
	if !ok1 || !ok2 {
		return nil // both or nothing: a half ledger would invent a zero
	}
	t := &oarlock.TokenTotals{Input: i, Output: o}
	if v, ok := asInt64(m["cachedReadTokens"]); ok {
		t.CacheRead = &v
	}
	if v, ok := asInt64(m["cacheCreationTokens"]); ok {
		t.CacheWrite = &v
	}
	if v, ok := asInt64(m["reasoningTokens"]); ok {
		t.Reasoning = &v
	}
	return t
}
