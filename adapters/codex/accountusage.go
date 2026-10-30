package codex

// Account usage for Codex (issue #89): the ChatGPT subscription's rate-limit
// windows, read from codex's own app-server.
//
// Codex has no HTTP usage endpoint. `codex --help` lists no usage subcommand,
// the wham/* API surface has no usage path, and OAR reads this capability from
// `account/rateLimits/read` on `codex app-server` (OAR
// runtimes/codex/account-usage.ts). That request is the only native surface, so
// this file drives it and invents nothing else.
//
// The read is one throwaway app-server. `codex app-server --listen stdio://` is
// started through internal/exec (so the process tree is owned and killed,
// stdin included), then
//
//	initialize               clientInfo oarlock/0.0.0, experimentalApi
//	initialized              notification
//	account/read             tolerated absent: older builds do not answer it
//	account/rateLimits/read
//
// are written as LF-terminated JSON lines and the reply is correlated by
// numeric id, exactly the dialect rpc.go speaks. Notification and server-request
// frames are ignored, and a line that is not a JSON-RPC object is skipped the
// way rpc.go skips it. The process is stopped and reaped on every path, timeout
// included, so a read never leaves a codex behind.
//
// The projection (projectCodexUsage) mirrors OAR's projectCodexUsage: the
// historical `rateLimits` bucket first, then `rateLimitsByLimitId` in wire order
// with a bucket skipped when the historical one already covers its limitId,
// `planType` taken from the first bucket that carries one, and the
// `primary`/`secondary` windows of every bucket after the ones whose
// usedPercent is outside [0,100]. A payload with no usable window is a
// malformed/quota_unavailable *ops.UsageError, never an empty snapshot.
//
// Where this deliberately differs from OAR, and why:
//
//   - OAR labels a window "<limitName|limitId> · <duration label>" and also
//     carries an id and a durationMs. oarlock.UsageWindow has no id, so the
//     window is named "<limitName|limitId|duration label> · <primary|secondary>"
//     (the frozen adapter contract): the kind is what tells the primary window
//     of a bucket from its secondary one in a flat list, and the duration label
//     stays as the fallback when the bucket names neither a limitName nor a
//     limitId.
//   - OAR maps an API-key account to unsupported/unsupported_auth_mode before
//     the rate-limit read. oarlock.AccountUsage reports no auth mode, and the
//     frozen failure table maps the app-server's own "ChatGPT authentication
//     required" to reauth/not_authenticated, so `account/read` is only
//     tolerated, never gated on.
//   - OAR returns the account email; oarlock.AccountUsage has no such field, so
//     the `account/read` result is not read at all.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// usageRuntime labels every *ops.UsageError this file returns. It is the
	// runtime id, not the executable name.
	usageRuntime = "codex"

	// usageKillGrace bounds the polite stop of the throwaway app-server: the
	// process exists for one read, so a tree that ignores SIGTERM is killed
	// rather than waited out.
	usageKillGrace = 2 * time.Second
	// usageWaitDelay bounds the drain of the output pipes after it exits.
	usageWaitDelay = time.Second
	// usageReadChunk is the stdout reader's initial buffer and usageMaxLine the
	// longest line accepted: a rate-limit reply is a few KiB, and a line past
	// the cap is a protocol failure, not a snapshot.
	usageReadChunk = 64 << 10
	usageMaxLine   = 1 << 20
	// usageLabelLimit is the longest limitName/limitId OAR accepts; a longer one
	// is not a label and is dropped (OAR's text()).
	usageLabelLimit = 80

	// usageNameSeparator joins the bucket label and the window kind.
	usageNameSeparator = " · "
	// usagePrimary/usageSecondary are the two window kinds a bucket carries.
	usagePrimary   = "primary"
	usageSecondary = "secondary"
	// codexDefaultLimitID is the one limitId OAR renders as a name.
	codexDefaultLimitID = "codex"

	// OAR's windowLabel buckets windowDurationMins by these.
	usageMinutesPerHour = 60
	usageMinutesPerDay  = 24 * usageMinutesPerHour
	usageMinutesPerWeek = 7 * usageMinutesPerDay
	// usageNoDuration is the label OAR uses when codex reports no duration.
	usageNoDuration = "Usage limit"

	// codexMethodNotFound is the JSON-RPC code for an unknown method, which an
	// older app-server answers for account/rateLimits/read.
	codexMethodNotFound = -32601
)

// accountUsageReader reads codex's ChatGPT rate-limit windows from one
// throwaway app-server. The zero value is what Runtime() installs: it finds
// `codex` the way the probe does, inherits the process environment and uses
// ops.UsageTimeout.
type accountUsageReader struct {
	Command string        // "" = `codex` from PATH or the installer's location
	Env     []string      // nil = os.Environ()
	Timeout time.Duration // 0 = ops.UsageTimeout
}

var _ oarlock.AccountUsageReader = accountUsageReader{}

// AccountUsage reads the account's rate-limit windows. Every failure is a
// *ops.UsageError and never an empty snapshot: a wrong plan or a zero quota is
// worse than a typed "the read did not answer".
func (r accountUsageReader) AccountUsage(ctx context.Context) (oarlock.AccountUsage, error) {
	if ctx == nil {
		return oarlock.AccountUsage{}, usageFailure(ops.UsageKindTransport, "")
	}
	command := r.Command
	if command == "" {
		// The same lookup the probe does, so a host that probed and a host that
		// reads disagree on nothing.
		found, err := findCodex()
		if err != nil {
			return oarlock.AccountUsage{}, usageFailure(ops.UsageKindTransport, "")
		}
		command = found
	}
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = ops.UsageTimeout
	}
	payload, err := readCodexRateLimits(ctx, command, env, timeout)
	if err != nil {
		return oarlock.AccountUsage{}, err
	}
	return projectCodexUsage(payload)
}

// ---- transport -----------------------------------------------------------

// codexLine is one line the reader goroutine took off stdout, or the failure
// that ended the stream.
type codexLine struct {
	raw []byte
	err error
}

// codexWireReply is the part of an inbound line this reader reads: an id and a
// result mark a reply, a method marks a notification or a server request.
type codexWireReply struct {
	ID     json.RawMessage  `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result"`
	Error  *codexRPCProblem `json:"error"`
}

// codexRPCProblem is a JSON-RPC error object. Its message is classified and
// then dropped: no vendor text reaches a returned error.
type codexRPCProblem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codexClient is the request side of the read: one request in flight, replies
// correlated by numeric id, everything else ignored.
type codexClient struct {
	stdin io.Writer
	lines <-chan codexLine
	next  int64
}

// call sends one request and returns its result. A reply for another id is
// ignored, because only one request is in flight and a stale id cannot be the
// answer to this one.
func (c *codexClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.next + 1
	c.next = id
	if err := c.send(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		line, err := c.nextLine(ctx)
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line.raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue // not a JSON-RPC frame
		}
		var reply codexWireReply
		if json.Unmarshal(trimmed, &reply) != nil {
			continue // an unreadable line is not an answer
		}
		if reply.Method != "" {
			continue // a notification or a server request
		}
		if !codexReplyID(reply.ID, id) {
			continue
		}
		if reply.Error != nil {
			return nil, codexRPCError(reply.Error)
		}
		return reply.Result, nil
	}
}

// send writes one LF-terminated JSON-RPC line.
func (c *codexClient) send(ctx context.Context, message any) error {
	line, err := json.Marshal(message)
	if err != nil {
		return usageFailure(ops.UsageKindTransport, "")
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		// A write fails when the process is already gone, which is the same
		// event as a stream that ends early.
		return usageEndedFailure(ctx)
	}
	return nil
}

// nextLine yields the next stdout line, or the classification of why there is
// none.
func (c *codexClient) nextLine(ctx context.Context) (codexLine, error) {
	select {
	case line, ok := <-c.lines:
		if !ok || line.err != nil {
			return codexLine{}, usageEndedFailure(ctx)
		}
		return line, nil
	case <-ctx.Done():
		return codexLine{}, usageFailure(ops.UsageKindTimeout, "")
	}
}

// codexReplyID reports whether raw is the numeric id wanted.
func codexReplyID(raw json.RawMessage, id int64) bool {
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == id
}

// codexReadLines frames stdout into LF-terminated lines for the client. The
// scanner caps one line at usageMaxLine; the consumer skips a line that is not
// a JSON-RPC frame, exactly as rpc.go does.
func codexReadLines(stdout io.Reader, lines chan<- codexLine, stop <-chan struct{}) {
	defer close(lines)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, usageReadChunk), usageMaxLine)
	for scanner.Scan() {
		line := codexLine{raw: append([]byte(nil), scanner.Bytes()...)}
		select {
		case lines <- line:
		case <-stop:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case lines <- codexLine{err: err}:
		case <-stop:
		}
	}
}

// readCodexRateLimits runs the four-message read and returns the
// account/rateLimits/read result. One timeout covers the whole exchange: a read
// that outlives it is a timeout, not a slow success.
func readCodexRateLimits(ctx context.Context, command string, env []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := exec.Start(ctx, &exec.Command{
		Path:      command,
		Args:      []string{"app-server", "--listen", "stdio://"},
		Env:       env,
		KillGrace: usageKillGrace,
		WaitDelay: usageWaitDelay,
	})
	if err != nil {
		return nil, usageEndedFailure(ctx)
	}
	// Claim both streams before anything can drain them (the seam ops.Run uses),
	// then drop stderr: a real app-server logs there, and a full pipe would stop
	// it mid-reply. Its output is never part of an error.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	go func() { _, _ = io.Copy(io.Discard, cmd.Stderr()) }()

	lines := make(chan codexLine)
	stop := make(chan struct{})
	go codexReadLines(cmd.Stdout(), lines, stop)

	// Nothing outlives this read: the reader is released, the tree is stopped
	// (stdin closed, SIGTERM, then SIGKILL after the grace) and reaped.
	defer func() {
		close(stop)
		_ = cmd.Close(context.Background())
		cancel()
		_, _ = cmd.Wait(context.Background())
	}()

	client := &codexClient{stdin: cmd.Stdin(), lines: lines}
	if _, err := client.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": ClientName, "version": ClientVersion},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return nil, err
	}
	if err := client.send(ctx, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	// account/read is optional: an older app-server does not answer it. Its
	// refusal is tolerated only while the process is still there to read rate
	// limits from; a timeout or a dead process is the real failure.
	if _, err := client.call(ctx, "account/read", map[string]any{}); err != nil {
		var usageErr *ops.UsageError
		if !errors.As(err, &usageErr) || usageErr.Kind == ops.UsageKindTimeout || usageErr.Kind == ops.UsageKindTransport {
			return nil, err
		}
	}
	return client.call(ctx, "account/rateLimits/read", map[string]any{})
}

// codexRPCError classifies one JSON-RPC error. The message is matched in
// lower case and never copied: a vendor refusal can quote a credential back.
func codexRPCError(problem *codexRPCProblem) *ops.UsageError {
	message := strings.ToLower(problem.Message)
	switch {
	case strings.Contains(message, "authentication required"),
		strings.Contains(message, "not logged in"),
		strings.Contains(message, "not authenticated"),
		strings.Contains(message, "unauthenticated"):
		return usageFailure(ops.UsageKindReauth, ops.ReasonNotAuthenticated)
	case problem.Code == codexMethodNotFound,
		strings.Contains(message, "method not found"),
		strings.Contains(message, "not supported"),
		strings.Contains(message, "unsupported"),
		strings.Contains(message, "unknown method"):
		return usageFailure(ops.UsageKindUnsupported, ops.ReasonEndpointUnavailable)
	}
	return usageFailure(ops.UsageKindTransport, "")
}

// ---- failure classification ----------------------------------------------

// usageFailure is the typed failure this file returns: the runtime, the kind and
// the stable reason, and no vendor output.
func usageFailure(kind ops.UsageErrorKind, reason string) *ops.UsageError {
	return &ops.UsageError{Runtime: usageRuntime, Kind: kind, Reason: reason}
}

// usageQuotaUnavailable is the answer to a payload that carried no window.
func usageQuotaUnavailable() *ops.UsageError {
	return usageFailure(ops.UsageKindMalformed, ops.ReasonQuotaUnavailable)
}

// usageEndedFailure classifies a read that lost its process: a stopped context
// is a timeout, anything else is a transport failure.
func usageEndedFailure(ctx context.Context) *ops.UsageError {
	if ctx.Err() != nil {
		return usageFailure(ops.UsageKindTimeout, "")
	}
	return usageFailure(ops.UsageKindTransport, "")
}

// ---- projection ----------------------------------------------------------

// projectCodexUsage turns one account/rateLimits/read result into an
// AccountUsage snapshot. It is pure and takes the wire payload as bytes, so the
// projection is testable without a process. A payload that yields no usable
// window is malformed/quota_unavailable, never an empty success.
func projectCodexUsage(payload []byte) (oarlock.AccountUsage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return oarlock.AccountUsage{}, usageQuotaUnavailable()
	}
	usage := oarlock.AccountUsage{}
	for _, bucket := range codexRateBuckets(root) {
		label, _ := codexText(bucket["limitName"])
		if label == "" {
			if id, ok := codexText(bucket["limitId"]); ok {
				label = id
				if id == codexDefaultLimitID {
					label = "Codex"
				}
			}
		}
		if usage.Plan == "" {
			if plan, ok := codexText(bucket["planType"]); ok {
				usage.Plan = plan
			}
		}
		for _, kind := range []string{usagePrimary, usageSecondary} {
			window, ok := codexWindow(bucket[kind], label, kind)
			if ok {
				usage.Windows = append(usage.Windows, window)
			}
		}
	}
	if len(usage.Windows) == 0 {
		return oarlock.AccountUsage{}, usageQuotaUnavailable()
	}
	return usage, nil
}

// codexRateBuckets is OAR's rateLimitBuckets: the historical single-bucket view
// first, then the optional multi-bucket view in wire order, skipping a bucket
// whose limitId the historical one already covers.
func codexRateBuckets(root map[string]json.RawMessage) []map[string]json.RawMessage {
	historical, hasHistorical := codexObject(root["rateLimits"])
	var buckets []map[string]json.RawMessage
	if hasHistorical {
		buckets = append(buckets, historical)
	}
	primaryID, hasPrimaryID := codexText(historical["limitId"])
	order, indexed := codexOrderedObject(root["rateLimitsByLimitId"])
	for _, key := range order {
		bucket, ok := codexObject(indexed[key])
		if !ok {
			continue
		}
		if id, hasID := codexText(bucket["limitId"]); hasPrimaryID && (primaryID == key || (hasID && primaryID == id)) {
			continue
		}
		buckets = append(buckets, bucket)
	}
	return buckets
}

// codexWindow projects one primary/secondary window. An entry whose usedPercent
// is absent, not a number or outside [0,100] is skipped, never clamped into a
// measurement codex did not report.
func codexWindow(raw json.RawMessage, label, kind string) (oarlock.UsageWindow, bool) {
	fields, ok := codexObject(raw)
	if !ok {
		return oarlock.UsageWindow{}, false
	}
	percent, ok := codexNumber(fields["usedPercent"])
	if !ok || percent < 0 || percent > 100 {
		return oarlock.UsageWindow{}, false
	}
	if label == "" {
		// No limitName and no limitId: the duration is the only name there is.
		label = codexWindowLabel(fields["windowDurationMins"])
	}
	resetsAt, _ := codexNumber(fields["resetsAt"])
	return ops.PercentWindow(label+usageNameSeparator+kind, percent, ops.ParseEpochSeconds(resetsAt))
}

// codexObject decodes one JSON object. null, an absent field and a scalar are
// all "not an object".
func codexObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, false
	}
	return object, true
}

// codexOrderedObject decodes one JSON object and its key order. Go maps carry no
// order and OAR walks Object.entries, so the order is read once with a token
// pass over the same bytes.
func codexOrderedObject(raw json.RawMessage) ([]string, map[string]json.RawMessage) {
	object, ok := codexObject(raw)
	if !ok {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	if _, err := decoder.Token(); err != nil {
		return nil, nil
	}
	var order []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil
		}
		order = append(order, key)
	}
	return order, object
}

// codexText reads one string field the way OAR's text() does: trimmed, and
// dropped when empty or longer than a label.
func codexText(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > usageLabelLimit {
		return "", false
	}
	return value, true
}

// codexNumber reads one JSON number. A string, a boolean or null is not a
// number (OAR's asNumber), so the window it belongs to is skipped instead of
// being read as zero.
func codexNumber(raw json.RawMessage) (float64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return 0, false
	}
	return value, true
}

// codexWindowLabel is OAR's windowLabel: the duration of one window as weeks,
// days, hours or minutes.
func codexWindowLabel(raw json.RawMessage) string {
	minutes, ok := codexNumber(raw)
	if !ok || minutes <= 0 {
		return usageNoDuration
	}
	switch {
	case math.Mod(minutes, usageMinutesPerWeek) == 0:
		return codexWindowCount(minutes/usageMinutesPerWeek, "week")
	case math.Mod(minutes, usageMinutesPerDay) == 0:
		return codexWindowCount(minutes/usageMinutesPerDay, "day")
	case math.Mod(minutes, usageMinutesPerHour) == 0:
		return codexWindowCount(minutes/usageMinutesPerHour, "hour")
	}
	return strconv.FormatFloat(minutes, 'f', -1, 64) + " minutes"
}

// codexWindowCount spells one count with its unit, singular or plural.
func codexWindowCount(count float64, unit string) string {
	text := strconv.FormatFloat(count, 'f', -1, 64)
	if count == 1 {
		return text + " " + unit
	}
	return text + " " + unit + "s"
}
