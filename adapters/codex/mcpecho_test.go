package codex

// A local stdio MCP echo server, written the way the fake app-server in
// fake_test.go is: this very test binary re-executed in a helper mode
// (TestMCPEchoServerProcess), so there is no sh, no Node, no network and the
// same binary on every platform.
//
// A live session attaches it as an oarlock.MCPServer{Command: os.Args[0], Args:
// []string{"-test.run=TestMCPEchoServerProcess"}, Env: gate + marker} and reads
// the marker file to prove codex launched it and asked it for its tools. The
// offline TestMCPEchoServerSpeaksMCP drives the same server without codex, so
// the process, both stdio framings and the marker are covered by `go test ./...`.
//
// The server accepts the two ways a stdio MCP client frames JSON-RPC — one bare
// JSON line, or one `Content-Length: N\r\n\r\n<body>` block — and answers in the
// framing the request used (newline-delimited JSON unless the client framed its
// request). Nothing but protocol messages is written to stdout; stderr carries
// one line and only when the marker cannot be written.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

const (
	// mcpEchoGateEnv turns TestMCPEchoServerProcess from a skipped test into the
	// body of the re-executed echo server.
	mcpEchoGateEnv = "OARLOCK_MCP_ECHO_TEST"
	// mcpEchoMarkerEnv names the file the server appends one line per received
	// method to, so a test can prove the server was launched and what it was
	// asked. An unset or unwritable marker never breaks the server.
	mcpEchoMarkerEnv = "OARLOCK_MCP_ECHO_MARKER"

	// What initialize reports about this server.
	mcpEchoName    = "oarlock-echo"
	mcpEchoVersion = "0.0.0"
	// mcpEchoProtocol is the protocol version answered when the client names
	// none; a client that names one gets its own back.
	mcpEchoProtocol = "2025-06-18"
	// mcpEchoShout is the one tool this server lists.
	mcpEchoShout = "shout"
)

// TestMCPEchoServerProcess is not a test: it is the echo server's body, this
// test binary re-executed. With the gate unset it skips, so `go test ./...`
// runs it as an ordinary skipped test. With it set it serves MCP on
// stdin/stdout until stdin reaches EOF, and leaves through os.Exit so the
// testing framework's own "PASS" banner never lands in the protocol stream
// (TestHelperProcess exits for the same reason).
func TestMCPEchoServerProcess(t *testing.T) {
	if os.Getenv(mcpEchoGateEnv) != "1" {
		t.Skip("set " + mcpEchoGateEnv + "=1 to run the MCP echo server body")
	}
	mcpEchoServe(os.Stdin, os.Stdout, os.Getenv(mcpEchoMarkerEnv))
	os.Exit(0)
}

// TestMCPEchoServerSpeaksMCP drives the echo server the way a session's codex
// does, without codex: it spawns the process itself and speaks MCP over its
// pipes. Everything is bounded by a context (the command is killed when it
// expires) and by lockstep reads, so a server that stops answering fails the
// test instead of hanging it, on every platform.
func TestMCPEchoServerSpeaksMCP(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker.txt")
	srv := mcpEchoServer(t, marker)
	if got, want := srv.Name, "echo"; got != want {
		t.Fatalf("mcpEchoServer name = %q, want %q", got, want)
	}
	if got, want := srv.Command, os.Args[0]; got != want {
		t.Fatalf("mcpEchoServer command = %q, want this test binary %q", got, want)
	}
	if len(srv.Args) != 1 || srv.Args[0] != "-test.run=TestMCPEchoServerProcess" {
		t.Fatalf("mcpEchoServer args = %q, want the one helper-mode flag", srv.Args)
	}
	if got, want := srv.Env[mcpEchoGateEnv], "1"; got != want {
		t.Fatalf("mcpEchoServer env[%s] = %q, want %q", mcpEchoGateEnv, got, want)
	}
	if got := srv.Env[mcpEchoMarkerEnv]; got != marker {
		t.Fatalf("mcpEchoServer env[%s] = %q, want %q", mcpEchoMarkerEnv, got, marker)
	}
	if srv.IsHTTP() {
		t.Fatalf("mcpEchoServer = %+v, want a stdio entry (no url, no type)", srv)
	}

	t.Run("newline framing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "marker.txt")
		c := mcpEchoStart(t, mcpEchoServer(t, path))

		init := c.roundTrip(mcpEchoCall(t, 1, "initialize", map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "oarlock-test", "version": "0.0.0"},
		}))
		if got, want := string(init.ID), "1"; got != want {
			t.Fatalf("initialize reply id = %s, want the numeric id %s the request carried", got, want)
		}
		if got, want := mcpEchoAt(t, init.Result, "protocolVersion"), "2024-11-05"; got != want {
			t.Fatalf("initialize result.protocolVersion = %v, want the client's %q", got, want)
		}
		// Exists, so a client may call tools/list at all.
		mcpEchoAt(t, init.Result, "capabilities", "tools")
		if got, want := mcpEchoAt(t, init.Result, "serverInfo", "name"), mcpEchoName; got != want {
			t.Fatalf("initialize result.serverInfo.name = %v, want %q", got, want)
		}

		// A notification is never answered. Sending tools/list straight after it
		// and demanding the tools/list id is what proves it: a reply to
		// notifications/initialized would be the line read here instead.
		c.send(mcpEchoNotify(t, "notifications/initialized"))
		list := c.roundTrip(mcpEchoCall(t, 2, "tools/list", map[string]any{}))
		if got, want := string(list.ID), "2"; got != want {
			t.Fatalf("tools/list reply id = %s, want %s (a reply to notifications/initialized would have been read first)", got, want)
		}
		if tools, ok := mcpEchoAt(t, list.Result, "tools").([]any); !ok || len(tools) != 1 {
			t.Fatalf("tools/list result.tools = %v, want exactly one tool", mcpEchoAt(t, list.Result, "tools"))
		}
		if got, want := mcpEchoAt(t, list.Result, "tools", 0, "name"), mcpEchoShout; got != want {
			t.Fatalf("tools/list result.tools[0].name = %v, want %q", got, want)
		}
		if got, want := mcpEchoAt(t, list.Result, "tools", 0, "description"), "Echo text back with an ECHO: prefix"; got != want {
			t.Fatalf("tools/list result.tools[0].description = %v, want %q", got, want)
		}
		if got, want := mcpEchoAt(t, list.Result, "tools", 0, "inputSchema", "properties", "text", "type"), "string"; got != want {
			t.Fatalf("tools/list result.tools[0].inputSchema.properties.text.type = %v, want %q", got, want)
		}
		required, ok := mcpEchoAt(t, list.Result, "tools", 0, "inputSchema", "required").([]any)
		if !ok || len(required) != 1 || required[0] != "text" {
			t.Fatalf("tools/list result.tools[0].inputSchema.required = %v, want [\"text\"]", required)
		}

		call := c.roundTrip(mcpEchoCall(t, 3, "tools/call", map[string]any{
			"name":      mcpEchoShout,
			"arguments": map[string]any{"text": "hello, mcp"},
		}))
		if got, want := string(call.ID), "3"; got != want {
			t.Fatalf("tools/call reply id = %s, want %s", got, want)
		}
		if got, want := mcpEchoAt(t, call.Result, "content", 0, "type"), "text"; got != want {
			t.Fatalf("tools/call result.content[0].type = %v, want %q", got, want)
		}
		if got, want := mcpEchoAt(t, call.Result, "content", 0, "text"), "ECHO:hello, mcp"; got != want {
			t.Fatalf("tools/call result.content[0].text = %v, want %q", got, want)
		}
		if got, want := mcpEchoAt(t, call.Result, "isError"), false; got != want {
			t.Fatalf("tools/call result.isError = %v, want %v", got, want)
		}

		ping := c.roundTrip(mcpEchoCall(t, 4, "ping", map[string]any{}))
		if got, want := string(ping.ID), "4"; got != want {
			t.Fatalf("ping reply id = %s, want %s", got, want)
		}
		if len(ping.Result) != 0 {
			t.Fatalf("ping result = %v, want the empty object {}", ping.Result)
		}

		// An unknown method is a JSON-RPC error naming that method.
		unknown := c.roundTrip(mcpEchoCall(t, 5, "tools/nope", map[string]any{}))
		if unknown.Error == nil {
			t.Fatalf("tools/nope reply = %+v, want a JSON-RPC error", unknown)
		}
		if got, want := unknown.Error.Code, -32601; got != want {
			t.Fatalf("tools/nope error code = %d, want %d", got, want)
		}
		if !strings.Contains(unknown.Error.Message, "tools/nope") {
			t.Fatalf("tools/nope error message = %q, want it to name the method", unknown.Error.Message)
		}

		// A malformed line is ignored: it is neither answered nor fatal, so the
		// next request is answered normally.
		c.send([]byte(`{"jsonrpc":"2.0","id":6,"method":`))
		again := c.roundTrip(mcpEchoCall(t, 7, "ping", map[string]any{}))
		if got, want := string(again.ID), "7"; got != want {
			t.Fatalf("reply after a malformed line has id = %s, want %s: a malformed line must be ignored, not answered and not fatal", got, want)
		}

		mcpEchoWantMarker(t, path,
			"initialize",
			"notifications/initialized",
			"tools/list",
			"tools/call "+mcpEchoShout,
			"ping",
			"tools/nope",
			"ping",
		)

		// Closing stdin is how a client stops a stdio server: it has to exit.
		c.stop()
	})

	t.Run("content-length framing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "marker.txt")
		c := mcpEchoStart(t, mcpEchoServer(t, path))

		// The other framing a stdio MCP client uses: a Content-Length header
		// block. The reply comes back in that framing, and a string id comes
		// back exactly as it arrived.
		c.sendFramed(mcpEchoCall(t, "framed-1", "ping", map[string]any{}))
		framed := mcpEchoDecode(t, c.readFramed())
		if got, want := string(framed.ID), `"framed-1"`; got != want {
			t.Fatalf("framed reply id = %s, want the string id %s the request carried", got, want)
		}

		// Back to a bare JSON line: the framing follows the last request, and a
		// client that names no protocol version gets this server's default.
		line := c.roundTrip(mcpEchoCall(t, 2, "initialize", map[string]any{}))
		if got, want := mcpEchoAt(t, line.Result, "protocolVersion"), mcpEchoProtocol; got != want {
			t.Fatalf("initialize without a protocol version answered %v, want the default %q", got, want)
		}

		mcpEchoWantMarker(t, path, "ping", "initialize")
		c.stop()
	})

	t.Run("unwritable marker", func(t *testing.T) {
		// The marker is a convenience: a server whose marker cannot be written
		// still has to answer every request. The directory below does not exist,
		// so every append fails.
		path := filepath.Join(t.TempDir(), "no-such-dir", "marker.txt")
		c := mcpEchoStart(t, mcpEchoServer(t, path))
		ping := c.roundTrip(mcpEchoCall(t, 1, "ping", map[string]any{}))
		if got, want := string(ping.ID), "1"; got != want {
			t.Fatalf("ping reply id = %s, want %s: an unwritable marker must not stop the server", got, want)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("os.Stat(%s) = %v, want the unwritable marker to be absent", path, err)
		}
		// Only after the process is gone: until Wait returns the stderr copy
		// belongs to another goroutine.
		c.stop()
		if got := c.stderr.String(); !strings.Contains(got, "marker") {
			t.Fatalf("server stderr = %q, want the one line about the unwritable marker and nothing else", got)
		}
	})
}

// mcpEchoServer is the MCP server entry a session attaches: this test binary,
// re-executed as the echo server, gated by mcpEchoGateEnv and told where to
// append its marker. A live test passes a path under t.TempDir(); the marker is
// the proof that codex launched the server and what it asked it.
func mcpEchoServer(t *testing.T, marker string) oarlock.MCPServer {
	t.Helper()
	return oarlock.MCPServer{
		Name:    "echo",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPEchoServerProcess"},
		Env: map[string]string{
			mcpEchoGateEnv:   "1",
			mcpEchoMarkerEnv: marker,
		},
	}
}

// mcpEchoState is the server's whole state: where the marker goes and whether
// the one permitted stderr line has been written yet.
type mcpEchoState struct {
	marker string
	warned bool
}

// mcpEchoServe answers JSON-RPC on r and writes replies to w until r reaches
// EOF, in the framing the request used.
func mcpEchoServe(r io.Reader, w io.Writer, marker string) {
	st := &mcpEchoState{marker: marker}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		body, framed, err := mcpEchoRead(br)
		if len(body) > 0 {
			if reply, ok := st.answer(body); ok {
				if mcpEchoWrite(w, reply, framed) != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// mcpEchoRead takes one request body off br, and reports the framing it used:
// false for a bare JSON line (the newline-delimited form), true for one
// `Content-Length: N` header block. The header block has to start with
// Content-Length, which is the form a stdio MCP client writes; any other header
// after it is skipped. A line that is not JSON is returned as a body and
// dropped by the caller, never diagnosed here.
func mcpEchoRead(br *bufio.Reader) (body []byte, framed bool, err error) {
	head, err := br.ReadString('\n')
	if head == "" {
		return nil, false, err
	}
	line := strings.TrimRight(head, "\r\n")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "content-length:") {
		return []byte(strings.TrimSpace(line)), false, err
	}
	n, perr := mcpEchoLength(line)
	if perr != nil {
		return nil, true, perr
	}
	// Drain the rest of the header block up to the blank line that ends it.
	for {
		next, herr := br.ReadString('\n')
		if strings.TrimRight(next, "\r\n") == "" {
			if herr != nil && next == "" {
				return nil, true, herr
			}
			break
		}
		if herr != nil {
			return nil, true, herr
		}
	}
	buf := make([]byte, n)
	if _, rerr := io.ReadFull(br, buf); rerr != nil {
		return nil, true, rerr
	}
	return buf, true, nil
}

// mcpEchoLength parses the body length out of one `Content-Length: N` header.
func mcpEchoLength(header string) (int, error) {
	_, value, ok := strings.Cut(header, ":")
	if !ok {
		return 0, fmt.Errorf("mcpecho: %q is not a Content-Length header", header)
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("mcpecho: %q names no body length", header)
	}
	return n, nil
}

// mcpEchoWrite sends one reply: a Content-Length block when the request came
// framed, a bare JSON line otherwise.
func mcpEchoWrite(w io.Writer, reply any, framed bool) error {
	b, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	if framed {
		_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// answer handles one request body: it records the method in the marker and
// returns the reply, or ok == false when the message gets no reply at all — a
// notification, or a line that is not JSON-RPC (malformed input is ignored, not
// fatal).
func (st *mcpEchoState) answer(body []byte) (reply any, ok bool) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
			Name            string `json:"name"`
			Arguments       struct {
				Text string `json:"text"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		return nil, false
	}
	// One line per received method; a tools/call line carries the tool name, so
	// a test can see which tool a session asked for.
	marker := req.Method
	if req.Method == "tools/call" && req.Params.Name != "" {
		marker += " " + req.Params.Name
	}
	st.mark(marker)

	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = mcpEchoProtocol
		}
		return mcpEchoResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpEchoName, "version": mcpEchoVersion},
		})
	case "notifications/initialized":
		return nil, false // a notification, and this server has nothing to say about it
	case "ping":
		return mcpEchoResult(req.ID, map[string]any{})
	case "tools/list":
		return mcpEchoResult(req.ID, map[string]any{"tools": []any{mcpEchoShoutTool()}})
	case "tools/call":
		if req.Params.Name != mcpEchoShout {
			return mcpEchoError(req.ID, -32602, "unknown tool "+strconv.Quote(req.Params.Name))
		}
		return mcpEchoResult(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "ECHO:" + req.Params.Arguments.Text}},
			"isError": false,
		})
	default:
		return mcpEchoError(req.ID, -32601, "method not found: "+req.Method)
	}
}

// mcpEchoResult is one JSON-RPC result. The id travels as raw JSON, so a number
// comes back a number and a string a string; a message with no id is a
// notification and is never answered.
func mcpEchoResult(id json.RawMessage, result any) (any, bool) {
	if len(id) == 0 {
		return nil, false
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, true
}

// mcpEchoError is one JSON-RPC error reply, and — like a result — never an
// answer to a notification.
func mcpEchoError(id json.RawMessage, code int, message string) (any, bool) {
	if len(id) == 0 {
		return nil, false
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}, true
}

// mcpEchoShoutTool is the one tool this server lists.
func mcpEchoShoutTool() map[string]any {
	return map[string]any{
		"name":        mcpEchoShout,
		"description": "Echo text back with an ECHO: prefix",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
	}
}

// mark appends one line to the marker file. The file is created on the first
// write and appended to after that; a marker that cannot be written must not
// break the server, so the only trace is one line on stderr — never stdout,
// which carries protocol messages only.
func (st *mcpEchoState) mark(line string) {
	if st.marker == "" {
		return
	}
	f, err := os.OpenFile(st.marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		st.warn(err)
		return
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		st.warn(err)
	}
	_ = f.Close()
}

// warn says once, and only on stderr, that the marker is unwritable (a marker
// outlives one request, so the same failure would otherwise repeat per method).
func (st *mcpEchoState) warn(err error) {
	if st.warned {
		return
	}
	st.warned = true
	fmt.Fprintf(os.Stderr, "mcpecho: marker %q is not writable: %v\n", st.marker, err)
}

// mcpEchoClient is the parent side of one spawned echo server: a request per
// call and one reply read, so a server that stops talking fails the test
// instead of hanging it.
type mcpEchoClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
	// waited records that a Wait is in flight or done: a second Wait on the same
	// command is an error, and the process is reaped by whichever one runs.
	waited bool
}

// mcpEchoMessage is one decoded reply.
type mcpEchoMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  map[string]any   `json:"result"`
	Error   *mcpEchoRPCError `json:"error"`
}

// mcpEchoRPCError is a JSON-RPC error member.
type mcpEchoRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpEchoStart spawns one echo server and returns the parent side of its pipes.
// The command is bound to a context, so a server that stops answering is killed
// when the context expires rather than hanging the test; the process is killed
// and reaped when the test ends, whatever happened before.
func mcpEchoStart(t *testing.T, srv oarlock.MCPServer) *mcpEchoClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	// Registered first, so it runs last: the process is gone by the time the
	// context is cancelled.
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	cmd.Env = mcpEchoChildEnv(srv.Env)
	c := &mcpEchoClient{t: t, cmd: cmd, stderr: &strings.Builder{}}
	cmd.Stderr = c.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	c.in = in
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	c.out = bufio.NewReaderSize(out, 1<<20)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %q: %v", srv.Command, srv.Args, err)
	}
	t.Cleanup(func() {
		_ = c.in.Close()
		_ = cmd.Process.Kill()
		if !c.waited {
			c.waited = true
			_ = cmd.Wait()
		}
		// Only now, after Wait, is the stderr copy this test's to read.
		if t.Failed() && c.stderr.Len() > 0 {
			t.Logf("MCP echo server stderr:\n%s", c.stderr.String())
		}
	})
	return c
}

// mcpEchoChildEnv is this process's environment with the entry's keys replaced
// by the entry's values: a gate the caller already exported must not shadow the
// server's own, whichever duplicate the platform's environment lookup finds
// first.
func mcpEchoChildEnv(set map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(set))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && mcpEchoHasKey(set, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, mcpEchoEnvList(set)...)
}

// mcpEchoHasKey reports whether name is one of the entry's keys, folding case
// because a Windows environment is keyed case-insensitively.
func mcpEchoHasKey(set map[string]string, name string) bool {
	for k := range set {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// mcpEchoEnvList turns the entry's env map into KEY=VALUE pairs, in key order so
// the child's environment is the same every run.
func mcpEchoEnvList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// send writes one request as the newline-delimited framing a stdio MCP client
// uses by default.
func (c *mcpEchoClient) send(req []byte) {
	c.t.Helper()
	line := append(append([]byte{}, req...), '\n')
	if _, err := c.in.Write(line); err != nil {
		c.t.Fatalf("write request %s: %v", req, err)
	}
}

// sendFramed writes one request as a Content-Length header block.
func (c *mcpEchoClient) sendFramed(req []byte) {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(req), req); err != nil {
		c.t.Fatalf("write framed request %s: %v", req, err)
	}
}

// read takes the next reply as one bare JSON line, which is the framing the
// server answers an unframed request in.
func (c *mcpEchoClient) read() []byte {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read reply: %v (got %q)", err, line)
	}
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		c.t.Fatalf("read reply: empty line")
	}
	if bytes.HasPrefix(bytes.ToLower(line), []byte("content-length:")) {
		c.t.Fatalf("reply %q carries a Content-Length header, want the bare JSON line the request used", line)
	}
	return line
}

// readFramed takes the next reply as a Content-Length header block.
func (c *mcpEchoClient) readFramed() []byte {
	c.t.Helper()
	head, err := c.out.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read reply header: %v (got %q)", err, head)
	}
	header := strings.TrimRight(head, "\r\n")
	if !strings.HasPrefix(strings.ToLower(header), "content-length:") {
		c.t.Fatalf("reply header %q is not a Content-Length header, want the framing the request used", header)
	}
	n, err := mcpEchoLength(header)
	if err != nil {
		c.t.Fatalf("reply header %q: %v", header, err)
	}
	blank, err := c.out.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read reply header terminator: %v (got %q)", err, blank)
	}
	if strings.TrimRight(blank, "\r\n") != "" {
		c.t.Fatalf("reply header terminator = %q, want the blank line that ends the headers", blank)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.out, body); err != nil {
		c.t.Fatalf("read %d-byte reply body: %v", n, err)
	}
	return body
}

// roundTrip sends one request as a bare JSON line and returns the decoded reply.
func (c *mcpEchoClient) roundTrip(req []byte) mcpEchoMessage {
	c.t.Helper()
	c.send(req)
	return mcpEchoDecode(c.t, c.read())
}

// stop closes the server's stdin — a stdio server's end-of-stream signal — and
// waits for it to exit, so a server that ignores EOF fails the test instead of
// leaking past it.
func (c *mcpEchoClient) stop() {
	c.t.Helper()
	_ = c.in.Close()
	// Waited before the goroutine starts: the cleanup must not start a second
	// Wait if this one is still in flight when the test ends.
	c.waited = true
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			c.t.Fatalf("server exited with %v after stdin EOF, want a clean exit", err)
		}
	case <-time.After(30 * time.Second):
		c.t.Fatalf("server did not exit within 30s of stdin EOF")
	}
}

// mcpEchoCall builds one JSON-RPC request. The id is any JSON value the caller
// names — a number or a string — so a test can prove it comes back unchanged.
func mcpEchoCall(t *testing.T, id any, method string, params any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	return b
}

// mcpEchoNotify builds one JSON-RPC notification: the same message without an
// id, which is what makes it one.
func mcpEchoNotify(t *testing.T, method string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{}})
	if err != nil {
		t.Fatalf("marshal %s notification: %v", method, err)
	}
	return b
}

// mcpEchoDecode parses one reply and fails the test when it is not JSON-RPC at
// all.
func mcpEchoDecode(t *testing.T, raw []byte) mcpEchoMessage {
	t.Helper()
	var m mcpEchoMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode reply %q: %v", raw, err)
	}
	if m.JSONRPC != "2.0" {
		t.Fatalf("reply %q has jsonrpc %q, want \"2.0\"", raw, m.JSONRPC)
	}
	return m
}

// mcpEchoAt walks a decoded reply along a path of object keys (string) and array
// indexes (int), failing the test when the path is not there, so one assertion
// reads as one line.
func mcpEchoAt(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("reply value %v is not an object, want key %q", v, key)
			}
			next, ok := m[key]
			if !ok {
				t.Fatalf("reply object %v has no key %q", m, key)
			}
			v = next
		case int:
			a, ok := v.([]any)
			if !ok || key < 0 || key >= len(a) {
				t.Fatalf("reply value %v is not an array carrying index %d", v, key)
			}
			v = a[key]
		default:
			t.Fatalf("mcpEchoAt path step %v is neither a key nor an index", step)
		}
	}
	return v
}

// mcpEchoWantMarker fails unless the marker file holds exactly the methods the
// server was asked for, in the order it received them.
func mcpEchoWantMarker(t *testing.T, path string, want ...string) {
	t.Helper()
	got := mcpEchoMarkerLines(t, path)
	if len(got) != len(want) {
		t.Fatalf("marker %s = %q, want %q", path, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("marker %s line %d = %q, want %q (whole file %q)", path, i, got[i], want[i], got)
		}
	}
}

// mcpEchoMarkerLines reads the marker file the server appended to.
func mcpEchoMarkerLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
