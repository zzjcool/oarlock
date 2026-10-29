package acp

// sink turns tapped wire lines into records. The kernel needs the session id,
// which `session/new` only returns after the handshake, so lines seen before
// attach are queued and replayed in their original order.

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
)

type promptKind int

const (
	kindTurn  promptKind = iota // the prompt that opened the turn
	kindSteer                   // a steering prompt sent mid-turn
)

// promptCall is a session/prompt on the wire, bound to its JSON-RPC id once
// the writer tap saw it. inputID is the host's id for that input, so a native
// refusal can be reported against the input it refused.
type promptCall struct {
	kind       promptKind
	written    chan struct{}
	id         string
	inputID    string
	superseded atomic.Bool
}

type sink struct {
	prof Profile
	proj *projector

	mu      sync.Mutex
	k       kernel.Kernel
	queue   []func(kernel.Kernel)
	methods map[string]string // our request id -> method
	appReq  map[string]bool   // agent request ids recorded as toApp
	calls   map[string]*promptCall
	pending *promptCall
	order   []*promptCall // prompts in flight, oldest first
	results map[string]map[string]any

	quiet atomic.Bool // session/load replay: frames kept, no events read
}

func newSink(prof Profile) *sink {
	return &sink{prof: prof, proj: newProjector(prof, ""), methods: map[string]string{}, appReq: map[string]bool{},
		calls: map[string]*promptCall{}, results: map[string]map[string]any{}}
}

func (s *sink) do(f func(kernel.Kernel)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.k == nil {
		s.queue = append(s.queue, f)
		return
	}
	f(s.k)
}

// attach creates the stream's identity and replays what the handshake said.
func (s *sink) attach(k kernel.Kernel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proj.setRoot(k.SessionID())
	s.k = k
	for _, f := range s.queue {
		f(k)
	}
	s.queue = nil
}

func (s *sink) result(method string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[method]
}

// fromAgent is the reader tap: one agent line, in wire order.
func (s *sink) fromAgent(line []byte) {
	w, ok := parseWire(line)
	if !ok {
		return
	}
	quiet := s.quiet.Load()
	s.mu.Lock()
	var method string
	var call *promptCall
	final := true
	switch w.Class {
	case classRequest:
		s.appReq[w.ID] = true
	case classResponse:
		method = s.methods[w.ID]
		delete(s.methods, w.ID)
		if method != "" && len(w.Result) > 0 {
			s.results[method] = decodeMap(w.Result)
		}
		if call = s.calls[w.ID]; call != nil {
			delete(s.calls, w.ID)
			for i, c := range s.order {
				if c == call {
					s.order = append(s.order[:i:i], s.order[i+1:]...)
					break
				}
			}
			final = (call.kind == kindTurn && !call.superseded.Load()) || (call.kind == kindSteer && s.prof.SteerSupersedes)
		}
	}
	s.mu.Unlock()

	s.do(func(k kernel.Kernel) {
		raw := json.RawMessage(w.Raw)
		switch w.Class {
		case classRequest:
			sid := asString(decodeMap(w.Params)["sessionId"])
			k.Request(oarlock.ToApp, oarlock.RequestBody{Action: oarlock.ActionNative, NativeType: w.Method, Native: raw},
				kernel.RecordAt{ID: w.ID, SessionID: sid})
		case classNotification:
			var sid string
			var path []string
			var events []oarlock.Event
			if w.Method == "$/cancel_request" {
				rid, _ := json.Marshal(decodeMap(w.Params)["requestId"])
				id := canonicalID(rid)
				events = []oarlock.Event{{Kind: oarlock.EventAppRequestCancelled, RequestID: id}}
			} else if !quiet {
				sid, path, events = s.proj.notification(w)
			} else if w.Method == "session/update" {
				sid = asString(decodeMap(w.Params)["sessionId"])
			}
			if sid == k.SessionID() {
				sid = ""
			}
			k.Frame(kernel.FrameBody{Native: raw, Events: events}, kernel.RecordAt{SessionID: sid, AgentPath: path})
		case classResponse:
			var events []oarlock.Event
			if !quiet || call != nil {
				events = s.proj.response(w, method, final)
			}
			// A steer answered with a JSON-RPC error is the runtime refusing
			// that input: oarlock accepted it, the native side dropped it, so the
			// drop is stated on this answer frame. It is not a turn end; the
			// original prompt's answer stays the only one.
			if call != nil && call.kind == kindSteer && len(w.Error) > 0 && string(w.Error) != "null" {
				events = append(events, oarlock.Event{Kind: oarlock.EventInputDropped, InputID: call.inputID, Dropped: oarlock.DroppedRuntimeRefused})
			}
			k.Frame(kernel.FrameBody{Native: raw, Events: events}, kernel.RecordAt{})
		}
	})
}

// toAgent is the writer tap: one line oarlock wrote.
func (s *sink) toAgent(line []byte) {
	w, ok := parseWire(line)
	if !ok {
		return
	}
	s.mu.Lock()
	switch w.Class {
	case classRequest:
		s.methods[w.ID] = w.Method
		if w.Method == "session/prompt" && s.pending != nil {
			c := s.pending
			s.pending = nil
			c.id = w.ID
			s.calls[w.ID] = c
			if c.kind == kindSteer && s.prof.SteerSupersedes {
				for _, o := range s.order {
					o.superseded.Store(true)
				}
			}
			s.order = append(s.order, c)
			close(c.written)
		}
	case classResponse:
		if !s.appReq[w.ID] {
			s.mu.Unlock()
			return
		}
		delete(s.appReq, w.ID)
	}
	s.mu.Unlock()
	if w.Class != classResponse {
		return
	}
	s.do(func(k kernel.Kernel) {
		k.Respond(w.ID, oarlock.ResponseBody{Kind: oarlock.ResponseAnswered, Native: json.RawMessage(w.Raw)}, kernel.RecordAt{})
	})
}

func (s *sink) arm(c *promptCall) {
	s.mu.Lock()
	s.pending = c
	s.mu.Unlock()
}

func (s *sink) disarm(c *promptCall) {
	s.mu.Lock()
	if s.pending == c {
		s.pending = nil
	}
	s.mu.Unlock()
}
