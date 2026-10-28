// Package voyage records and replays an oarlock record stream as an
// oar-voyage/3 JSONL log: the asset that turns a vendor's behaviour into a
// test that can run forever without the vendor (ADR-0006).
//
//	{"kind":"header","format":"oar-voyage/3","dialect":"oarlock","runtime":"pi",...}
//	{"kind":"record","record":{"seq":1,"kind":"frame",...}}
//	{"kind":"record","record":{"seq":2,...}}
//	{"kind":"end","at":1761609600000,"reason":"settled"}
//
// # Writing
//
// A Writer writes one line per Append with a single write call and fsyncs it,
// so a process killed mid-run loses at most the line in flight. Nothing is
// buffered in the process. End (or Close) appends the end line and closes the
// file.
//
// # Truncation
//
// A log without an end line is truncated. Decode never reports a truncated log
// as complete: it returns the salvaged prefix together with ErrTruncated, and
// Replay (strict) returns no records at all. A line is committed only once its
// LF is on disk, so the half line a crash leaves behind is ignored.
//
// # Redaction
//
// The Writer applies the redactor to each marshalled line before it reaches the
// file descriptor, so a credential never exists in a log, not even briefly. A
// kernel that already redacts (internal/kernel with (*redact.Redactor).Record)
// feeds the Writer records that pass through the same rules again, which is
// idempotent: replay and the live observers see the same bytes.
//
// # Dialect
//
// The header names dialect "oarlock". OAR's TypeScript recorder writes the same
// line envelope with a different record body, so its logs are refused with
// ErrFormat instead of being decoded into records that mean something else.
// Converting an OAR-recorded log is a separate, unwritten tool.
//
// Replay needs only the file: no network, no process, no clock.
package voyage
