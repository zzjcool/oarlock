package claude

// The live claude tests: the claims only a real binary can prove. Opt-in
// (OAR_CLAUDE_LIVE=1 plus a claude on PATH and credentials in the environment);
// without them they skip as NOT VERIFIED, and nothing else in this file turns a
// failure into a skip.
//
//   - TestLiveSteerEcho: a mid-turn user message inside a tool-backed turn (a
//     request boundary) is echoed back, and the adapter accepts it only on that
//     echo. Live probe evidence 2026-10-29 (claude 2.1.312, third-party
//     Anthropic-compatible endpoint): a steer written after system/task_started
//     during `Bash sleep 8` is echoed ~8.5 s later; a steer inside a one-message
//     text answer has no boundary, is queued natively and applied as a
//     follow-up turn after the result, so it can only be rejected.
//   - TestLivePromptAndWait: the public oarlock.PromptAndWait reads a completed
//     turn off the same stream.
//   - TestLiveImages: a prompt carrying one native image block (the hard-coded
//     67-byte PNG below, written to a temp dir) is ControlAccepted and a turn
//     ends. Gated on OAR_CLAUDE_LIVE_IMAGES=1 as well: image input depends on
//     the endpoint, so the shared live gate proves nothing about images.
//   - TestLiveCancelQueued: the live init must advertise
//     interrupt_cancel_queued_v1 (a false means the cancel_queued path is dead
//     here and the documented claim must be withdrawn); one long tool turn is
//     aborted the moment its prompt is accepted, the abort must be accepted and
//     the turn must end, and every command_lifecycle state plus whether the
//     prompt was cancelled or started is logged as the evidence (observed, not
//     asserted: the unstarted window is a race).
//   - TestLiveNoPermissionRequest: one ordinary Bash tool turn ends with no
//     `answered` response record anywhere in the stream and every toApp request
//     left unanswered — the live evidence for the record-only default; each
//     toApp request's native type is logged.
//   - TestLiveAnswerSkipped: never runs, skipped on purpose — headless claude
//     always runs with --dangerously-skip-permissions, so no can_use_tool
//     request is ever emitted and Answer is proven against the fake process
//     only. It is the one unverified test in this file.
//
// Each opens its own session, bounds every wait, and prints redacted diagnostics
// on failure (user frames with isReplay/uuid, command_lifecycle states, result
// subtypes) — never the environment or a credential.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	// liveTurnBound bounds one whole live test: open, turn, steer/echo.
	liveTurnBound = 150 * time.Second
	// liveOpenBound bounds the opening handshake (a hang there is a failure).
	liveOpenBound = 60 * time.Second
	// liveBoundaryWait is how long a prompt gets to reach a tool/request
	// boundary before the steer leg gives up.
	liveBoundaryWait = 60 * time.Second

	// liveSteerPrompt asks for a tool round on purpose: the request boundary is
	// what makes a mid-turn user message land inside the running turn.
	liveSteerPrompt = "Use the Bash tool to run exactly: sleep 4. Then reply with the single word: done."
	// liveSteerInput is the steer text; its echo is the acceptance evidence.
	liveSteerInput = "STEER-LIVE: also say banana."
	// livePongPrompt is the PromptAndWait leg: one short answer, no tools.
	livePongPrompt = "Reply with the single word: pong"

	// liveImagePrompt asks for the colour of the one solid image attached. The
	// answer is logged, never asserted: it depends on the endpoint and the model
	// behind it, while the claim under test is that an image is accepted at all.
	liveImagePrompt = "The attached image is one solid colour. Reply with the single word naming the colour."
	// liveToolPrompt is the permission-request leg: one ordinary Bash round.
	liveToolPrompt = "Use the Bash tool to run exactly: echo oar-live. Then reply with the single word: done."
	// liveCancelPrompt opens a long tool turn that the abort interrupts as soon
	// as claude has the prompt.
	liveCancelPrompt = "Use the Bash tool to run exactly: sleep 20. Then reply with the single word: done."

	// liveAbortBound is how long an aborted turn gets to end on its own: the
	// abort grace is when the take-over closes the process group, so a turn that
	// has not ended by then never ended itself.
	liveAbortBound = DefaultAbortGrace + 5*time.Second
)

// liveImagePNG is the 1×1 solid-white image the image test writes to its temp
// dir: 67 bytes — the 8-byte signature, IHDR, a 10-byte IDAT (a hand-built
// fixed-Huffman zlib stream of the two-byte grayscale scanline) and IEND. Raw
// bytes, no encoder and no fixture file, so what is sent cannot drift from the
// test.
var liveImagePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR, length 13
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1×1
	0x08, 0x00, 0x00, 0x00, 0x00, 0x3a, 0x7e, 0x9b, // 8-bit grayscale, CRC
	0x55, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, // end of IHDR, IDAT, length 10
	0x54, 0x78, 0x01, 0x63, 0xf8, 0x0f, 0x00, 0x01, // "T", zlib 78 01, deflate
	0x01, 0x01, 0x00, 0x8a, 0xe2, 0x53, 0x7d, 0x00, // adler32, IDAT CRC
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, // IEND, length 0
	0x42, 0x60, 0x82, // IEND CRC
}

// TestLiveSteerEcho proves the steer echo on a real claude: the steer is
// written inside a tool-backed turn, so claude applies it to the active request
// and replays the user frame with the same uuid. Acceptance without that echo is
// never reported.
func TestLiveSteerEcho(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	if out, err := sess.Prompt(ctx, liveSteerPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt rejected: %+v (err %v)\n%s", out, err, liveDiagnostics(sess, "", liveSteerPrompt))
	}
	// The request boundary: claude is inside a Bash call, so the mid-turn user
	// message goes to the active request instead of being queued for a
	// follow-up turn (which would only echo after the result).
	waitLiveBoundary(t, sess, liveBoundaryWait)

	steerID := newUUID()
	out, err := sess.Steer(ctx, liveSteerInput, oarlock.InputOptions{InputID: steerID})
	if err != nil {
		t.Fatalf("Steer: %v\n%s", err, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("live steer rejected: %+v (reason %q)\n%s", out, out.Reason, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	// Acceptance is not the proof: the replayed user frame with the steer uuid is.
	var echo *oarlock.Event
	for _, r := range sess.Records(oarlock.Cursor{}) {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUserMessage && e.InputID == steerID {
				ev := e
				echo = &ev
			}
		}
	}
	if echo == nil {
		t.Fatalf("steer accepted without an echoed user_message\n%s", liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	if echo.Evidence != oarlock.EvidenceAcknowledged || !strings.Contains(echo.Text, "STEER-LIVE") {
		t.Fatalf("echo event = %+v\n%s", echo, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	logSteerTimeline(t, sess, steerID)
}

// TestLivePromptAndWait proves the public convenience on a real claude: one
// prompt, the runtime's own completed turn, and the streamed text.
func TestLivePromptAndWait(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	run, err := oarlock.PromptAndWait(ctx, sess, livePongPrompt, oarlock.WithTimeout(90*time.Second))
	if err != nil {
		t.Fatalf("PromptAndWait: %v\n%s", err, liveDiagnostics(sess, "", livePongPrompt))
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run outcome = %q (turn %+v, reason %q)\n%s", run.Outcome, run.Turn, run.Reason, liveDiagnostics(sess, "", livePongPrompt))
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn = %+v, want a completed turn\n%s", run.Turn, liveDiagnostics(sess, "", livePongPrompt))
	}
	if !strings.Contains(strings.ToLower(run.Text), "pong") {
		t.Fatalf("text = %q, want it to contain pong\n%s", run.Text, liveDiagnostics(sess, "", livePongPrompt))
	}
	t.Logf("PromptAndWait completed the turn; prompt answered at seq %d; %d records", run.Seq, len(sess.Records(oarlock.Cursor{})))
}

// TestLiveImages proves native image input on a real claude: one hard-coded
// 1×1 PNG travels as an image content block and claude answers a turn. The
// adapter-level rejection of a png would be a failure, never a skip, and a turn
// that does not end is a failure with the real reason printed. Gated on
// OAR_CLAUDE_LIVE_IMAGES=1 on top of liveOpen: image input depends on the
// endpoint, so the shared live gate proves nothing about images.
func TestLiveImages(t *testing.T) {
	if os.Getenv("OAR_CLAUDE_LIVE_IMAGES") != "1" {
		t.Skip("NOT VERIFIED: image input depends on the endpoint, so the shared live gate (OAR_CLAUDE_LIVE=1) proves nothing about images; set OAR_CLAUDE_LIVE_IMAGES=1 as well to run this one")
	}
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	pngPath := filepath.Join(t.TempDir(), "solid-white.png")
	if err := os.WriteFile(pngPath, liveImagePNG, 0o600); err != nil {
		t.Fatalf("write the test image: %v", err)
	}
	inputID := newUUID()
	out, err := sess.Prompt(ctx, liveImagePrompt, oarlock.PromptOptions{InputOptions: oarlock.InputOptions{
		InputID: inputID,
		Images:  []oarlock.InputImage{{Path: pngPath, MediaType: "image/png"}},
	}})
	if err != nil {
		t.Fatalf("Prompt with an image: %v\n%s", err, liveDiagnostics(sess, inputID, liveImagePrompt))
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("image prompt rejected: %+v (reason %q); an adapter-level refusal of a png is a failure\n%s",
			out, out.Reason, liveDiagnostics(sess, inputID, liveImagePrompt))
	}
	end := waitLiveTurnEnd(t, sess, out.Seq, liveTurnBound, liveImagePrompt)
	t.Logf("image prompt accepted at seq %d (Capabilities.Images=%v); the turn ended at seq %d (%s, outcome %s)",
		out.Seq, sess.Capabilities().Images, end.seq, end.frame, end.outcome.Kind)
	logLiveLifecycle(t, sess, inputID)
}

// TestLiveCancelQueued is the live evidence for cancel_queued. The live init
// must advertise interrupt_cancel_queued_v1: a false means the cancel_queued
// path is dead here and the documented claim must be withdrawn, so it fails and
// never skips. Then one long tool turn is aborted as soon as claude has the
// prompt and has declared its capabilities, the abort must be accepted and
// claude must end the turn itself (a result frame, within the abort grace — an
// exit means the take-over closed the process group instead). What the prompt
// uuid's command_lifecycle frames say — cancelled or started — is logged as the
// observation, never asserted: whether the prompt was still unstarted when the
// interrupt landed is a race.
func TestLiveCancelQueued(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	inputID := newUUID()
	out, err := sess.Prompt(ctx, liveCancelPrompt, oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: inputID}})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt rejected: %+v (err %v)\n%s", out, err, liveDiagnostics(sess, inputID, liveCancelPrompt))
	}
	// The capability is declared by the root system/init frame, which claude emits
	// when the first input starts the session (the recorded probe runs
	// lifecycle frames before init), so it is read here and not at open.
	waitLiveInit(t, sess, liveOpenBound)
	if !sess.SupportsCancelQueued() {
		t.Fatalf("the live claude advertised no interrupt_cancel_queued_v1 in system/init: cancel_queued is unsupported on this build, so the documented claim must be withdrawn\n%s",
			liveDiagnostics(sess, inputID, liveCancelPrompt))
	}
	// As soon as the prompt is accepted (init is the only thing in between): the
	// shorter the gap, the more likely the input is still in claude's unstarted
	// window — the cancel_queued case.
	abort, err := sess.Abort(ctx)
	if err != nil {
		t.Fatalf("Abort: %v\n%s", err, liveDiagnostics(sess, inputID, liveCancelPrompt))
	}
	if abort.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort rejected: %+v (reason %q)\n%s", abort, abort.Reason, liveDiagnostics(sess, inputID, liveCancelPrompt))
	}
	end := waitLiveTurnEnd(t, sess, out.Seq, liveAbortBound, liveCancelPrompt)
	t.Logf("abort accepted at seq %d; the turn ended at seq %d (%s, outcome %s)",
		abort.Seq, end.seq, end.frame, end.outcome.Kind)
	logLiveLifecycle(t, sess, inputID)
}

// TestLiveNoPermissionRequest is the live evidence for the documented
// record-only default: one ordinary Bash tool turn, then no `answered` response
// record exists anywhere in the stream (oarlock never fabricates a permission
// answer), and any runtime→app request that did arrive was left unanswered
// while the turn still ended. Every toApp request's native type is printed.
func TestLiveNoPermissionRequest(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	out, err := sess.Prompt(ctx, liveToolPrompt, oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt rejected: %+v (err %v)\n%s", out, err, liveDiagnostics(sess, "", liveToolPrompt))
	}
	end := waitLiveTurnEnd(t, sess, out.Seq, liveTurnBound, liveToolPrompt)

	var requests, answered []oarlock.Record
	for _, r := range sess.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == oarlock.ResponseAnswered {
			answered = append(answered, r)
		}
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			requests = append(requests, r)
		}
	}
	if len(answered) != 0 {
		t.Fatalf("oarlock fabricated %d answered response(s) in a record-only session; first = %+v\n%s",
			len(answered), answered[0].Response, liveDiagnostics(sess, "", liveToolPrompt))
	}
	for _, r := range requests {
		native := ""
		if r.Request != nil {
			native = r.Request.NativeType
		}
		t.Logf("runtime→app request seq %d id %s nativeType %q, left unanswered", r.Seq, r.ID, native)
		for _, resp := range sess.Records(oarlock.Cursor{}) {
			if resp.Kind == oarlock.KindResponse && resp.RequestID == r.ID {
				t.Fatalf("request %s was answered (%+v) although the policy is record-only\n%s",
					r.ID, resp.Response, liveDiagnostics(sess, "", liveToolPrompt))
			}
		}
	}
	t.Logf("turn ended at seq %d (%s, outcome %s): %d runtime→app request(s), no answered response record",
		end.seq, end.frame, end.outcome.Kind, len(requests))
}

// TestLiveAnswerSkipped records why Answer cannot be live-verified here, so the
// absence of live evidence is a written fact rather than an omission.
func TestLiveAnswerSkipped(t *testing.T) {
	t.Skip("Answer is unverified live: headless claude runs with --dangerously-skip-permissions (adapters/claude/args.go), so no can_use_tool request is ever emitted and Answer is proven against the fake process only")
}

// liveOpen starts one real claude session in a scratch cwd and disposes it with
// a bounded context. The only skips are "not opted in" and "no binary".
func liveOpen(t *testing.T) *Session {
	t.Helper()
	if os.Getenv("OAR_CLAUDE_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CLAUDE_LIVE=1 (needs a claude binary and credentials) to run against live claude")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("NOT VERIFIED: no claude binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveOpenBound)
	defer cancel()
	sess, err := Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{
		Cwd: t.TempDir(),
		Env: map[string]*string{"IS_SANDBOX": one()},
	})
	if err != nil {
		t.Fatalf("Open live claude: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("Open returned %T, not *claude.Session", sess)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Dispose(ctx)
	})
	return s
}

// waitLiveBoundary waits until the turn is inside a tool call: a started task or
// a tool call is the model-request boundary a mid-turn steer needs. A turn with
// none (a single streaming text answer) cannot prove the echo, so it fails and
// says which frames did arrive.
func waitLiveBoundary(t *testing.T, sess oarlock.Session, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var tasks, tools int
		for _, r := range sess.Records(oarlock.Cursor{}) {
			m := sj.ParseRec(r.Native)
			if sj.Str(m["type"]) == "system" && sj.Str(m["subtype"]) == "task_started" {
				tasks++
			}
			for _, e := range r.Events {
				switch e.Kind {
				case oarlock.EventTaskStarted:
					tasks++
				case oarlock.EventToolCallStarted:
					tools++
				}
			}
		}
		if tasks > 0 || tools > 0 {
			t.Logf("request boundary reached: %d task_started, %d tool_call_started frames", tasks, tools)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no tool task start within %v: the prompt produced no request boundary, so a mid-turn steer would only be queued\n%s",
				d, liveDiagnostics(sess, "", liveSteerPrompt))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitLiveInit waits until the root system/init frame is retained, so a fact
// that init declares (a negotiated capability) is read from the stream instead
// of from a handshake that may not have happened yet. An init that never
// arrives, or a process that exits first, is a failure.
func waitLiveInit(t *testing.T, sess oarlock.Session, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, r := range sess.Records(oarlock.Cursor{}) {
			if len(r.AgentPath) != 0 {
				continue
			}
			if m := sj.ParseRec(r.Native); sj.Str(m["type"]) == "system" && sj.Str(m["subtype"]) == "init" {
				t.Logf("root system/init retained at seq %d", r.Seq)
				return
			}
			if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == oarlock.ResponseExited {
				t.Fatalf("the process exited before its system/init frame\n%s", liveDiagnostics(sess, "", ""))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no system/init frame within %v: what the runtime declares cannot be read\n%s", d, liveDiagnostics(sess, "", ""))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveTurnEnd is the evidence that ended one turn: the record the runtime's own
// turn_ended event was read from (on claude, always its root result frame) and
// that outcome as claude reported it.
type liveTurnEnd struct {
	seq     uint64
	frame   string
	outcome oarlock.TurnOutcome
}

// waitLiveTurnEnd waits for the root turn's own end and returns the frame that
// carried it. An exit without a result is a failure, not an end: nothing in the
// stream would then say the turn ended (an accepted abort whose grace elapsed
// closes the process group, and a crash lands here too).
func waitLiveTurnEnd(t *testing.T, sess oarlock.Session, after uint64, d time.Duration, prompt string) liveTurnEnd {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, r := range sess.Records(oarlock.Cursor{AfterSeq: after}) {
			if len(r.AgentPath) != 0 {
				continue
			}
			if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == oarlock.ResponseExited {
				t.Fatalf("the process exited before a result frame ended the turn\n%s", liveDiagnostics(sess, "", prompt))
			}
			for _, e := range r.Events {
				if e.Kind == oarlock.EventTurnEnded && e.Outcome != nil {
					return liveTurnEnd{seq: r.Seq, frame: liveFrameType(r), outcome: *e.Outcome}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn end within %v\n%s", d, liveDiagnostics(sess, "", prompt))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveFrameType names the native frame a record carries ("" when it has none).
func liveFrameType(r oarlock.Record) string {
	if len(r.Native) == 0 {
		return ""
	}
	return sj.Str(sj.ParseRec(r.Native)["type"])
}

// logLiveLifecycle prints claude's own receipt of one input: every
// command_lifecycle state the retained records carry for its uuid, then the
// word the cancel_queued claim turns on — cancelled (claude dropped the queued
// input, so it is the caller's again) or started (the turn began before the
// interrupt took effect). Values only; no payloads.
func logLiveLifecycle(t *testing.T, sess oarlock.Session, inputID string) {
	t.Helper()
	var states []string
	for _, r := range sess.Records(oarlock.Cursor{}) {
		m := sj.ParseRec(r.Native)
		if sj.Str(m["type"]) != "command_lifecycle" || sj.Str(m["command_uuid"]) != inputID {
			continue
		}
		states = append(states, sj.Str(m["state"]))
		t.Logf("seq %d command_lifecycle state=%s for input %s", r.Seq, sj.Str(m["state"]), inputID)
	}
	seen := strings.Join(states, " -> ")
	switch {
	case len(states) == 0:
		t.Logf("no command_lifecycle frame names input %s: claude said nothing about this input", inputID)
	case liveLifecycleHas(states, "cancelled") && liveLifecycleHas(states, "started"):
		t.Logf("prompt %s: started and then cancelled (states: %s)", inputID, seen)
	case liveLifecycleHas(states, "cancelled"):
		t.Logf("prompt %s: cancelled, never started (states: %s) — the queued input was taken back", inputID, seen)
	case liveLifecycleHas(states, "started"):
		t.Logf("prompt %s: started, not cancelled (states: %s) — the turn began before the interrupt took effect", inputID, seen)
	default:
		t.Logf("prompt %s lifecycle states: %s", inputID, seen)
	}
}

// liveLifecycleHas reports whether one of the observed states is want.
func liveLifecycleHas(states []string, want string) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// liveDiagnostics is the redacted evidence a failing live test reports: the
// prompt, each user frame's isReplay/uuid, each command_lifecycle command_uuid
// and state, each result subtype/is_error and each system frame's subtype. It
// prints frame metadata only — no environment, no credential, no frame payload.
func liveDiagnostics(sess oarlock.Session, steerID, prompt string) string {
	var b strings.Builder
	b.WriteString("--- live diagnostics (redacted) ---\n")
	if prompt != "" {
		fmt.Fprintf(&b, "prompt: %q\n", prompt)
	}
	if steerID != "" {
		b.WriteString("steer uuid: " + steerID + "\n")
	}
	rs := sess.Records(oarlock.Cursor{})
	fmt.Fprintf(&b, "records: %d\n", len(rs))
	for _, r := range rs {
		m := sj.ParseRec(r.Native)
		switch sj.Str(m["type"]) {
		case "user":
			isReplay, _ := m["isReplay"].(bool)
			fmt.Fprintf(&b, "  seq %d user isReplay=%v uuid=%s parent_tool_use_id=%v\n",
				r.Seq, isReplay, sj.Str(m["uuid"]), frameValue(m["parent_tool_use_id"]))
		case "command_lifecycle":
			fmt.Fprintf(&b, "  seq %d command_lifecycle command_uuid=%s state=%s\n",
				r.Seq, sj.Str(m["command_uuid"]), sj.Str(m["state"]))
		case "result":
			isErr, _ := m["is_error"].(bool)
			fmt.Fprintf(&b, "  seq %d result subtype=%s is_error=%v terminal_reason=%q\n",
				r.Seq, sj.Str(m["subtype"]), isErr, sj.Str(m["terminal_reason"]))
		case "system":
			fmt.Fprintf(&b, "  seq %d system subtype=%s task_id=%q\n",
				r.Seq, sj.Str(m["subtype"]), sj.Str(m["task_id"]))
		}
	}
	b.WriteString("--- end diagnostics ---")
	return b.String()
}

// frameValue renders a frame field for a diagnostic line without assuming it is
// a string (parent_tool_use_id is null on root frames).
func frameValue(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok && s == "" {
		return `""`
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// logSteerTimeline logs, with record sequence numbers and wall-clock offsets,
// what claude said about one steer uuid: its command_lifecycle states and the
// replayed user frame. Values only; no payloads.
func logSteerTimeline(t *testing.T, sess oarlock.Session, steerID string) {
	t.Helper()
	rs := sess.Records(oarlock.Cursor{})
	if len(rs) == 0 {
		return
	}
	base := rs[0].ReceivedAt
	for _, r := range rs {
		m := sj.ParseRec(r.Native)
		at := "+" + r.ReceivedAt.Sub(base).Round(10*time.Millisecond).String()
		switch {
		case sj.Str(m["type"]) == "command_lifecycle" && sj.Str(m["command_uuid"]) == steerID:
			t.Logf("seq %d %s command_lifecycle state=%s for steer %s", r.Seq, at, sj.Str(m["state"]), steerID)
		case sj.Str(m["type"]) == "user":
			for _, e := range r.Events {
				if e.Kind == oarlock.EventUserMessage && e.InputID == steerID {
					t.Logf("seq %d %s user echo for steer %s (evidence %s)", r.Seq, at, steerID, e.Evidence)
				}
			}
		}
	}
}

// one returns a pointer to "1" for Options.Env.
func one() *string { v := "1"; return &v }
