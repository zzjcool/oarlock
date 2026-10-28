// Package pifixture builds voyage fixtures from the REAL Pi 1.4.0 wire frames
// kept in testdata/pi.
//
// # THIS IS A TEST STAND-IN, NOT THE PI PROJECTION
//
// The real Pi projection (issue #11) does not exist yet. The projection below
// is the smallest rule set that lets a recorded Pi session fold to meaningful
// Status / Usage / ContextUsage / Conversation answers. It follows the rules of
// the reference TS projection (oar packages/oar/src/runtimes/pi/projection.ts)
// only where listed, and it MUST be deleted or replaced once #11 lands:
//
//	agent_start                     -> turn_active
//	message_update text_delta       -> text_delta{Text}
//	message_update thinking_delta   -> reasoning{Text}
//	message_end (assistant, usage)  -> usage{Tokens}: cumulative running total,
//	                                   input counts cacheRead+cacheWrite back in
//	agent_settled                   -> turn_ended{completed|aborted}
//	                                   + usage{Context} from frame data only
//	every other frame               -> frame record, NO events, kept verbatim
//
// Invariants: every Native is the exact Pi line (never trimmed, never
// re-shaped); no frame is dropped and none is synthesized; every record goes
// through the REAL kernel (kernel.New) so Seq/redaction/retention are the
// production ones. Output is byte-deterministic: no time.Now, no map order.
package pifixture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	"github.com/zzjcool/oarlock/internal/redact"
	"github.com/zzjcool/oarlock/internal/voyage"
)

// Recorder is the header recorder string of every generated fixture.
const Recorder = "oarlock/voyage-gen (testdata/pi stand-in)"

// baseTime is the fixed origin of every ReceivedAt: baseTime + Seq ms (UTC).
var baseTime = time.Date(2026, time.October, 28, 0, 0, 0, 0, time.UTC)

// Trace is a built scenario: Records are what the real kernel retained.
type Trace struct {
	Header    voyage.Header
	Records   []oarlock.Record
	EndReason string
	EndAt     time.Time
}

// Scenarios lists the committed scenarios, sorted.
func Scenarios() []string { return []string{"abort-turn", "normal-turn"} }

// Name is the fixture name (file = testdata/voyage/<Name>.jsonl).
func Name(scenario string) string { return "pi-1.4.0-" + scenario }

// Build reads <piDir>/<name>.frames.jsonl and <name>.requests.jsonl and
// projects them through the real kernel. red may be nil.
func Build(piDir, name string, red *redact.Redactor) (Trace, error) {
	frames, err := readLines(filepath.Join(piDir, name+".frames.jsonl"))
	if err != nil {
		return Trace{}, err
	}
	requests, err := readLines(filepath.Join(piDir, name+".requests.jsonl"))
	if err != nil {
		return Trace{}, err
	}
	return BuildFrom(frames, requests, red)
}

// BuildFrom is Build over in-memory lines (one JSON object per element). It is
// the seam the redaction test uses to inject a fake credential into a frame.
func BuildFrom(frames, requests [][]byte, red *redact.Redactor) (Trace, error) {
	if red == nil {
		red = redact.New(nil)
	}
	p := &projector{}
	for i, line := range frames {
		f, err := parseFrame(line)
		if err != nil {
			return Trace{}, fmt.Errorf("pifixture: frame %d: %w", i+1, err)
		}
		p.frames = append(p.frames, f)
		p.observe(f)
	}
	if p.sessionID == "" {
		return Trace{}, errors.New("pifixture: no get_state response with data.sessionId in frames")
	}
	reqs, err := parseRequests(requests)
	if err != nil {
		return Trace{}, err
	}

	k := kernel.New(p.sessionID, red.Record)
	last := baseTime
	at := func(id string) kernel.RecordAt {
		last = last.Add(time.Millisecond)
		return kernel.RecordAt{ID: id, ReceivedAt: last}
	}
	emitReq := func(r request) {
		body := oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: r.Message}
		if r.Type == "abort" {
			body = oarlock.RequestBody{Action: oarlock.ActionAbort}
		}
		k.Request(oarlock.ToRuntime, body, at(r.ID))
	}

	abortDone := make(map[string]bool)
	promptDone := make(map[string]bool)
	seenUpdate := false
	st := &runState{}
	for i, f := range p.frames {
		// prompt request goes immediately before the response carrying its id.
		if f.Type == "response" && f.ID != "" {
			for _, r := range reqs {
				if r.Type == "prompt" && r.ID == f.ID && !promptDone[r.ID] {
					promptDone[r.ID] = true
					emitReq(r)
				}
			}
		}
		k.Frame(kernel.FrameBody{
			Native: json.RawMessage(append([]byte(nil), frames[i]...)),
			Events: st.events(f, p.window),
		}, at(""))
		// abort request goes right after the first message_update.
		if f.Type == "message_update" && !seenUpdate {
			seenUpdate = true
			for _, r := range reqs {
				if r.Type == "abort" && !abortDone[r.ID] {
					abortDone[r.ID] = true
					emitReq(r)
				}
			}
		}
	}

	records := k.Records(oarlock.Cursor{SessionID: p.sessionID, AfterSeq: 0})
	endAt := baseTime
	if n := len(records); n > 0 {
		endAt = records[n-1].ReceivedAt
	}
	return Trace{
		Header: voyage.Header{
			Runtime:   "pi",
			Model:     p.model,
			Cwd:       ".",
			SessionID: p.sessionID,
			StartedAt: baseTime.UnixMilli(),
			Recorder:  Recorder,
		},
		Records:   records,
		EndReason: "settled",
		EndAt:     endAt,
	}, nil
}

// Write builds one scenario and writes outDir/<Name>.jsonl (an old file is
// removed first; voyage.Create is O_EXCL).
func Write(piDir, outDir, scenario string, red *redact.Redactor) error {
	tr, err := Build(piDir, scenario, red)
	if err != nil {
		return err
	}
	return WriteTrace(outDir, scenario, tr, red)
}

// WriteTrace writes an already built trace as outDir/<Name(scenario)>.jsonl.
func WriteTrace(outDir, scenario string, tr Trace, red *redact.Redactor) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(outDir, Name(scenario)+".jsonl")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	w, err := voyage.Create(path, tr.Header, red)
	if err != nil {
		return err
	}
	for _, rec := range tr.Records {
		if err := w.Append(rec); err != nil {
			_ = w.EndAt("error", tr.EndAt)
			return err
		}
	}
	return w.EndAt(tr.EndReason, tr.EndAt)
}

// --- wire shapes (only what the stand-in reads) ---

type frame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Aborted bool   `json:"aborted"`
	Data    *struct {
		SessionID string `json:"sessionId"`
		Model     *struct {
			ID            string `json:"id"`
			Provider      string `json:"provider"`
			ContextWindow int64  `json:"contextWindow"`
		} `json:"model"`
	} `json:"data"`
	// MessageRaw is decoded lazily into Message: extension_ui_request frames
	// carry "message" as a plain string, message_* frames as an object.
	MessageRaw            json.RawMessage `json:"message"`
	Message               *wireMessage    `json:"-"`
	AssistantMessageEvent *struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

// wireMessage is the object form of a Pi "message" field.
type wireMessage struct {
	Role  string `json:"role"`
	Usage *struct {
		Input       int64  `json:"input"`
		Output      int64  `json:"output"`
		CacheRead   *int64 `json:"cacheRead"`
		CacheWrite  *int64 `json:"cacheWrite"`
		Reasoning   *int64 `json:"reasoning"`
		TotalTokens int64  `json:"totalTokens"`
	} `json:"usage"`
}

// parseFrame decodes the fields the stand-in reads from one Pi line.
func parseFrame(line []byte) (frame, error) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return frame{}, err
	}
	if raw := bytes.TrimSpace(f.MessageRaw); len(raw) > 0 && raw[0] == '{' {
		var m wireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return frame{}, err
		}
		f.Message = &m
	}
	return f, nil
}

type request struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

func parseRequests(lines [][]byte) ([]request, error) {
	var out []request
	for i, line := range lines {
		var r request
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("pifixture: request %d: %w", i+1, err)
		}
		if r.Type == "prompt" || r.Type == "abort" { // get_state is not a control
			out = append(out, r)
		}
	}
	return out, nil
}

// projector gathers the session-level facts the header and the context report
// need from the frames themselves.
type projector struct {
	frames    []frame
	sessionID string
	model     string
	window    int64
}

func (p *projector) observe(f frame) {
	if f.Type == "response" && f.Command == "get_state" && f.Success && f.Data != nil && p.sessionID == "" {
		p.sessionID = f.Data.SessionID
		if m := f.Data.Model; m != nil {
			p.model = m.ID
			if m.Provider != "" {
				p.model = m.Provider + "/" + m.ID
			}
			p.window = m.ContextWindow
		}
	}
}

// runState carries the cumulative usage across frames.
type runState struct {
	in, out                   int64
	reasoning, cacheR, cacheW *int64
	lastTotal                 *int64
}

func i64(v int64) *int64 { return &v }

func add(dst **int64, v *int64) {
	if v == nil {
		return
	}
	if *dst == nil {
		*dst = i64(0)
	}
	**dst += *v
}

func (s *runState) events(f frame, window int64) []oarlock.Event {
	switch f.Type {
	case "agent_start":
		return []oarlock.Event{{Kind: oarlock.EventTurnActive}}
	case "message_update":
		if f.AssistantMessageEvent == nil {
			return nil
		}
		switch f.AssistantMessageEvent.Type {
		case "text_delta":
			return []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: f.AssistantMessageEvent.Delta}}
		case "thinking_delta":
			return []oarlock.Event{{Kind: oarlock.EventReasoning, Text: f.AssistantMessageEvent.Delta}}
		}
	case "message_end":
		if f.Message == nil || f.Message.Role != "assistant" || f.Message.Usage == nil {
			return nil
		}
		u := f.Message.Usage
		s.in += u.Input
		if u.CacheRead != nil {
			s.in += *u.CacheRead
		}
		if u.CacheWrite != nil {
			s.in += *u.CacheWrite
		}
		s.out += u.Output
		add(&s.reasoning, u.Reasoning)
		add(&s.cacheR, u.CacheRead)
		add(&s.cacheW, u.CacheWrite)
		s.lastTotal = i64(u.TotalTokens)
		tok := &oarlock.TokenTotals{Input: s.in, Output: s.out}
		if s.reasoning != nil {
			tok.Reasoning = i64(*s.reasoning)
		}
		if s.cacheR != nil {
			tok.CacheRead = i64(*s.cacheR)
		}
		if s.cacheW != nil {
			tok.CacheWrite = i64(*s.cacheW)
		}
		return []oarlock.Event{{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: tok}}}
	case "agent_settled":
		kind := oarlock.OutcomeCompleted
		if f.Aborted {
			kind = oarlock.OutcomeAborted
		}
		evs := []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: kind}}}
		if s.lastTotal != nil && window > 0 {
			pct := float64(*s.lastTotal) * 100 / float64(window)
			evs = append(evs, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{
				Context: &oarlock.ContextUsage{Tokens: i64(*s.lastTotal), ContextWindow: i64(window), Percent: &pct},
			}})
		}
		return evs
	}
	return nil
}

func readLines(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out, sc.Err()
}
