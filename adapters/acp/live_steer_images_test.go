package acp

// live_steer_images_test.go — the two opencode depth claims that need a real
// agent on a real endpoint:
//
//  1. Steering (v1): a concurrent `session/prompt` with an empty `_meta` joins
//     the running turn. Both prompt RPCs are answered at idle with the same
//     usage and only the original answer ends the turn, so the stream keeps
//     exactly one `turn_ended` per turn and the steered instruction lands in
//     the reply. A steer with no running turn is refused by oarlock itself.
//  2. Images: `initialize.promptCapabilities.image` alone decides whether the
//     prompt may carry blocks of `type:"image"`; an advertised agent accepts
//     them natively (what the model then says is the provider's business).
//
// Both skip through liveAgentOrSkip when OAR_ACP_LIVE is not 1.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// onePixelPNG is a real 1×1 PNG (8-bit RGBA, one IDAT), base64: small enough
// that the transport, not the model, is what the assertion is about.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// steerMarker is the instruction a steer injects; only its last word is
// asserted, because the model may wrap it in prose.
const steerMarker = "IMPORTANT: end your reply with the single word BANANA."

func TestLiveOpenCodeSteer(t *testing.T) {
	a := liveAgentOrSkip(t)
	if a.profile.SteerMeta == nil {
		t.Skipf("NOT VERIFIED: opencode %q is not the v1 line, so this session is not a Steerer (v2 refuses concurrent prompts)", a.version)
	}
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})
	steer := steerSession{s}
	var _ oarlock.Steerer = steer

	// The counting prompt must still be running when the steer is written; the
	// second (longer) one is the retry when the model answered too fast.
	prompts := []string{
		"Count slowly from 1 to 100, one number per line, with a short comment on every line. Do not stop early.",
		"Count slowly from 1 to 300, one number per line, with two comments on every line. Do not stop early.",
	}
	var out oarlock.ControlOutcome
	var baseEnds, baseAnswers int
	accepted := false
	for attempt := 0; attempt < len(prompts) && !accepted; attempt++ {
		// Baseline right before this attempt's prompt: an earlier attempt that
		// answered itself is already on the stream (its answer carries the
		// turn_ended that made the status idle again).
		before := records(s)
		baseEnds, baseAnswers = len(liveTurnEnds(before)), livePromptAnswers(before)
		var err error
		out, err = s.Prompt(ctx, prompts[attempt], oarlock.PromptOptions{})
		if err != nil || out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt %d = %+v, %v", attempt+1, out, err)
		}
		waitLiveStatus(t, s, fold.StatusRunning, 2*time.Minute)
		time.Sleep(3 * time.Second)
		if fold.StatusOf(records(s)).Value.Kind != fold.StatusRunning {
			continue // the turn ended before the steer could land: ask for a longer one
		}
		out, err = steer.Steer(ctx, steerMarker, oarlock.InputOptions{InputID: "steer-1"})
		if err != nil {
			t.Fatalf("steer: %v", err)
		}
		if out.Kind == oarlock.ControlAccepted {
			accepted = true
			break
		}
		if out.Code != oarlock.NoActiveTurn {
			t.Fatalf("steer = %+v, %v; want accepted on the running turn", out, err)
		}
	}
	if !accepted {
		t.Fatal("the steer was never accepted: the runtime answered both counting prompts before the steer RPC could land")
	}

	// The steered turn is bounded: a turn that keeps running past 60 s is
	// aborted (and the abort is what the failure reports).
	if !waitIdleWithin(t, s, 60*time.Second) {
		_, _ = s.Abort(ctx)
		t.Fatalf("the steered turn was still running 60 s after the steer\n%s", dump(records(s)))
	}

	recs := records(s)
	assertDense(t, recs)
	// One turn, one end: the steer answer never ends a turn (SteerSupersedes
	// is false on this line).
	if ends := len(liveTurnEnds(recs)) - baseEnds; ends != 1 {
		t.Fatalf("turn_ended = %d for the steered turn, want exactly one:\n%s", ends, dump(recs))
	}
	// Two session/prompt answers: the turn's own prompt and the steer, both
	// answered once the runtime went idle.
	if answers := livePromptAnswers(recs) - baseAnswers; answers != 2 {
		t.Fatalf("session/prompt answers = %d, want exactly two (the turn and the steer):\n%s", answers, dump(recs))
	}
	if got := text(recs); !strings.Contains(strings.ToUpper(got), "BANANA") {
		t.Fatalf("the steered instruction never landed; the reply was %q\n%s", got, dump(recs))
	}
	// The steer was recorded as a steer, not as a second turn.
	if n := liveCount(recs, oarlock.ActionSteer); n != 1 {
		t.Fatalf("steer requests = %d, want exactly one:\n%s", n, dump(recs))
	}

	// With the turn over there is nothing to steer, and oarlock says so without
	// inventing a turn or a dropped input.
	endsBefore := len(liveTurnEnds(records(s)))
	out, err := steer.Steer(ctx, steerMarker, oarlock.InputOptions{InputID: "steer-2"})
	if err != nil {
		t.Fatalf("second steer: %v", err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("steer with no active turn = %+v, want rejected{%s}", out, oarlock.NoActiveTurn)
	}
	if ends := len(liveTurnEnds(records(s))); ends != endsBefore {
		t.Fatalf("a refused steer changed the turn ends: %d -> %d\n%s", endsBefore, ends, dump(records(s)))
	}
	t.Logf("steer: %d records, one turn_ended, two prompt answers, marker landed", len(recs))
}

func TestLiveOpenCodeImagePrompt(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})
	if !s.Capabilities().Images {
		t.Fatalf("images = false: the live opencode did not advertise promptCapabilities.image, so the image prompt cannot be sent at all")
	}
	p := writeOnePixelPNG(t)

	out, err := s.Prompt(ctx, "Describe this image in one short sentence.", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{
		InputID: "img-1",
		Images:  []oarlock.InputImage{{Path: p, MediaType: "image/png"}},
	}})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("image prompt = %+v, %v; want accepted (the agent advertised image blocks)", out, err)
	}
	if !waitIdleWithin(t, s, 2*time.Minute) {
		_, _ = s.Abort(ctx)
		t.Fatalf("the image turn was still running after 2 minutes\n%s", dump(records(s)))
	}

	recs := records(s)
	assertDense(t, recs)
	if ends := liveTurnEnds(recs); len(ends) != 1 || ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn ends = %+v, want exactly one completed\n%s", ends, dump(recs))
	}
	// The image travelled with the turn: the prompt request the host recorded
	// names the file and its media type. (The native content block's shape is
	// pinned by TestOpenCodeImageBlockShape below; this adapter does not record
	// outbound native frames, so the wire block cannot be read off the stream.)
	var req *oarlock.Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToRuntime && r.Request != nil &&
			r.Request.Action == oarlock.ActionPrompt && len(r.Request.Images) > 0 {
			req = r
		}
	}
	if req == nil {
		t.Fatalf("the image prompt request is not on the stream:\n%s", dump(recs))
	}
	if got := req.Request.Images[0]; got.Path != p || got.MediaType != "image/png" {
		t.Fatalf("prompt image = %+v, want {path:%s mediaType:image/png}", got, p)
	}
	// What the model says is the provider's concern (the live provider's model
	// may have no vision); the transport is what this test proves.
	t.Logf("image: %d records, one completed turn, image block accepted natively", len(recs))
}

// TestOpenCodeImageBlockShape pins the exact wire block blocks() builds for an
// image prompt: the ACP image content block, JSON-encoded as it goes out. It
// runs without a process, so it holds even when the live gate is off.
func TestOpenCodeImageBlockShape(t *testing.T) {
	p := writeOnePixelPNG(t)
	s := &Session{images: true} // what initialize advertised
	blocks, err := s.blocks(&oarlock.RequestBody{Input: "describe", Images: []oarlock.InputImage{{Path: p, MediaType: "image/png"}}})
	if err != nil {
		t.Fatalf("blocks: %v", err)
	}
	native, err := json.Marshal(sdk.PromptRequest{SessionId: "sid", Prompt: blocks})
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	for _, want := range []string{`"type":"image"`, `"image/png"`} {
		if !strings.Contains(string(native), want) {
			t.Fatalf("the session/prompt params carry no %s: %s", want, native)
		}
	}
	// An agent that did not advertise image blocks is refused, never sent a
	// silently text-only prompt.
	refused := &Session{}
	_, err = refused.blocks(&oarlock.RequestBody{Images: []oarlock.InputImage{{Path: p, MediaType: "image/png"}}})
	var rej *oarlock.Rejected
	if err == nil || !errors.As(err, &rej) || rej.Code != oarlock.Unsupported {
		t.Fatalf("blocks without promptCapabilities.image = %v, want an Unsupported rejection", err)
	}
}

// writeOnePixelPNG writes the 1×1 PNG into the test's temp dir and returns the
// path. The bytes are decoded, not trusted: a bad fixture fails loudly.
func writeOnePixelPNG(t *testing.T) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("decode the 1×1 PNG fixture: %v", err)
	}
	if len(raw) < 8 || string(raw[1:4]) != "PNG" {
		t.Fatalf("the fixture is not a PNG: % x", raw[:min(8, len(raw))])
	}
	p := filepath.Join(t.TempDir(), "one-pixel.png")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write the 1×1 PNG: %v", err)
	}
	return p
}

// livePromptAnswers counts the agent's session/prompt answers in the stream: a
// JSON-RPC response (has an id, no method) whose result is a PromptResponse
// (it carries stopReason). Errors are not answers.
func livePromptAnswers(recs []oarlock.Record) int {
	n := 0
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result struct {
				StopReason *string `json:"stopReason"`
			} `json:"result"`
		}
		if json.Unmarshal(r.Native, &m) != nil {
			continue
		}
		if len(m.ID) == 0 || string(m.ID) == "null" || m.Method != "" {
			continue
		}
		if m.Result.StopReason != nil {
			n++
		}
	}
	return n
}

// waitIdleWithin waits for the folded status to be idle, bounded by limit, and
// reports whether it got there (the caller aborts its own turn on false).
func waitIdleWithin(t *testing.T, s oarlock.Session, limit time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if fold.StatusOf(records(s)).Value.Kind == fold.StatusIdle {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
