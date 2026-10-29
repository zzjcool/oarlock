package acp

// The wire tap. The SDK owns JSON-RPC framing, correlation and the typed
// client calls; this file only observes the bytes it reads and writes, so the
// record stream is the wire in wire order, verbatim:
//
//   - a line the agent wrote that is a notification or a response is a frame;
//   - a line the agent wrote that is a request (id + method) is a toApp
//     request, recorded under the agent's own request id;
//   - a line oarlock wrote that is a response is the `answered` response to
//     that toApp request (the reply that was actually sent, not a guess);
//   - a line oarlock wrote that is a request is a toRuntime control, which the
//     session records itself through the kernel (prompt, abort, dispose); the
//     tap only learns the JSON-RPC id of a prompt from it.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

type msgClass int

const (
	classNone msgClass = iota
	classRequest
	classNotification
	classResponse
)

// wireMsg is one decoded JSON-RPC line. Raw is the line as it travelled.
type wireMsg struct {
	Class  msgClass
	ID     string // canonical request id ("" for a notification)
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
	Raw    []byte
}

func parseWire(line []byte) (wireMsg, bool) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return wireMsg{}, false
	}
	w := wireMsg{Method: m.Method, Params: m.Params, Result: m.Result, Error: m.Error, Raw: line}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	if hasID {
		w.ID = canonicalID(m.ID)
	}
	switch {
	case m.Method != "" && hasID:
		w.Class = classRequest
	case m.Method != "":
		w.Class = classNotification
	case hasID:
		w.Class = classResponse
	default:
		return wireMsg{}, false
	}
	return w, true
}

func canonicalID(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// lineSplitter hands each complete line to fn. The partial tail is flushed as
// a line at EOF, as the SDK's own scanner would.
type lineSplitter struct {
	buf []byte
	fn  func([]byte)
}

func (l *lineSplitter) write(p []byte) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimRight(l.buf[:i], "\r")
		if len(bytes.TrimSpace(line)) > 0 {
			l.fn(append([]byte(nil), line...))
		}
		l.buf = l.buf[i+1:]
	}
}

func (l *lineSplitter) flush() {
	if len(bytes.TrimSpace(l.buf)) > 0 {
		l.fn(append([]byte(nil), bytes.TrimRight(l.buf, "\r\n")...))
	}
	l.buf = nil
}

// tapReader observes everything read from the agent before the SDK sees it.
type tapReader struct {
	r    io.Reader
	sp   lineSplitter
	done func() // called once at EOF or error, after the last line
}

func newTapReader(r io.Reader, line func([]byte), done func()) *tapReader {
	return &tapReader{r: r, sp: lineSplitter{fn: line}, done: done}
}

func (t *tapReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.sp.write(p[:n])
	}
	if err != nil {
		t.sp.flush()
		if t.done != nil {
			t.done()
			t.done = nil
		}
	}
	return n, err
}

// tapWriter observes every message the SDK writes to the agent. The SDK
// writes one whole message per Write call.
type tapWriter struct {
	w    io.Writer
	line func([]byte)
}

func (t *tapWriter) Write(p []byte) (int, error) {
	if line := bytes.TrimSpace(p); len(line) > 0 {
		t.line(append([]byte(nil), line...))
	}
	return t.w.Write(p)
}
