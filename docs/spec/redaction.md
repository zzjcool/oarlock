# Credential redaction

**Status:** frozen for M0. Implementation:
[`internal/redact`](../../internal/redact/redact.go). Reference: OAR
`docs/spec/record-stream.md` ("Credential configuration values do not enter
records"). Related: [record stream](record-stream.md) invariant 5.

Redaction happens **before** the kernel retains or publishes a record. Live
observers and replay therefore see the same redacted record. The runtime's own
configuration is not modified: the command still receives the real value, and
only the retained copy changes.

The rules are deliberately narrower than "filter anything that looks like a
key" and deliberately broader than "filter `API_KEY`". Over-redaction is a
bug too: it hides the information an operator needs to debug a session.

## 1. Name rules

Select a value when its **last underscore-separated segment** is one of:

```
KEY  APIKEY  TOKEN  SECRET  PASSWORD  PASSWD  PASSPHRASE
CREDENTIAL  CREDENTIALS  COOKIE  PWD
```

Comparison is case-insensitive. `PGPASSWORD` is selected as an exact name.

For HTTP headers, select `Authorization`, `Proxy-Authorization`, or a name
whose **last hyphen-separated segment** is in the same word list. This covers
`X-Api-Key`, `Api-Key`, `X-Auth-Token` and `Cookie`, and leaves
`Content-Type` and `Accept` readable.

The last-segment rule is what keeps ordinary variables readable:

| selected | not selected | why |
|---|---|---|
| `ANTHROPIC_API_KEY` | `SSH_AUTH_SOCK` | final segment `SOCK` |
| `MYSQL_PWD` | `GIT_AUTHOR_EMAIL` | final segment `EMAIL` |
| `CLIENT_SECRET` | `AWS_ACCESS_KEY_ID` | final segment `ID` |
| `PGPASSWORD` | `PASSWORD_STORE_DIR` | final segment `DIR` |
| `X_API_KEY` | `GNUPG_PASSPHRASE_FILE` | final segment `FILE` |
| `SESSION_COOKIE` | `KEYTIMEOUT` | the whole key is not `KEY` |
| | `HOME`, `PATH` | not credential words |

A name selects a value only if the value passes the value rule below. A
three-character placeholder under `API_KEY` stays readable on purpose.

## 2. Value rules

A value is protected when **all** of these hold:

- length is at least 8 characters;
- it is not empty;
- it does not start with `/` or `~/`;
- it does not start with a Windows drive path (`C:\` or `C:/`).

A selected value is replaced with `[redacted]`.

Values are replaced **longest first**, so a short value that is a prefix of a
longer one cannot hide the longer one.

*Counterexample (why longest first):* with `sk-test-abcdef` and
`sk-test-abcdef123` both known, replacing the short value first turns
`sk-test-abcdef123` into `[redacted]123`; the long value no longer matches and
a suffix survives. Replacing the long value first yields the whole placeholder.
This is the "最短优先" failure named in the issue, implemented as a test in
`redact_test.go`.

## 3. Structural rules

### `env` and `headers`

Inside an `env` map, every key is tested by the name rule. Inside a `headers`
map, every key is tested by the header rule. This applies to session options
and to MCP server configuration (`env` on a stdio server, `headers` on an HTTP
server).

An `Authorization: Bearer <token>` value contributes two selections: the whole
header value (when it passes the value rule) and the part after the scheme.
The second selection protects a bare echoed token elsewhere in the same value
tree.

### Password-bearing URLs

A string that parses as a URL with a non-empty password protects:

- the whole connection string (`postgres://user:pw@host/db`),
- the decoded password,
- its encoded form as it appears in the userinfo.

A URL without a password is not selected by this shape rule. A password
shorter than eight characters is not replaced outside its complete URL; the
complete URL is still protected.

### Error text

`message`, `stack`, and a structured `cause` use the same known-value
replacement. `(*Redactor).Error` returns a value whose printed text is
redacted and which unwraps to the original error, so `errors.Is/As` still
reaches an `E2BIG`, a deadline, or a native vendor error. The original error
object is not mutated; a caller that prints it directly is outside this
guarantee.

## 4. Known session values

The adapter collects the session's credential values (session `env`, MCP
`env`/`headers`, and for a runtime whose SDK resolves a model key, that key)
and builds a `*Redactor` with `redact.New(values)`. The redactor replaces those
values anywhere they appear in a retained record, including:
`Record.Native`, request input and native payloads, response reasons, and
event text such as a complete tool output.

Split streaming fragments are **not** covered: a key that a model echoes can
be split across `text_delta` frames. The complete tool-output frame is
redacted; the fragments and any later coalescing of them are outside the
guarantee. Do not rely on redaction to stop a model from revealing a key.

## 5. Reverse guarantee: what must stay readable

These are test cases, not aspirations:

```
SSH_AUTH_SOCK   GIT_AUTHOR_EMAIL   AWS_ACCESS_KEY_ID
HOME            PATH               PASSWORD_STORE_DIR
GNUPG_PASSPHRASE_FILE              KEYTIMEOUT
Content-Type    Accept             X-Request-Id
```

A rule that redacts one of these has over-reached. The name table is the
single place to change; a new credential word must be added with a positive
and a negative test.

## What this explicitly does not do

- **No host environment scan.** oarlock does not read the process environment
  or credential files to discover values. It protects what the session or MCP
  configuration names, plus values it is handed.
- **No historical log cleaning.** Records stored before a rule existed are not
  rewritten. A host may run the current redactor over stored records on
  startup; that is the host's migration, not an implicit mutation.
- **No split-fragment guarantee.** See §4.
- **No transport or sandbox security.** TLS, process isolation and a vendor's
  own logs are out of scope.
- **No mutation of the runtime's configuration.** The child process still gets
  the real value; only the retained record is redacted.

## Performance

- `Text` with no known values returns the input and `false` without scanning or
  copying.
- `Bytes` returns the **same slice** when nothing changes; it only re-encodes
  a changed document.
- `Any` returns the original map/slice when no child changed; changed
  containers are copied, never mutated in place.
- With no known values, the structural walk still visits maps to apply the
  name rules (there is no way to know a key is `env` without looking). It does
  not copy or re-encode unchanged values.

## Verification

```
gofmt -l .            # empty
go vet ./...
go test -race ./...
go test ./internal/redact -v
```

`redact_test.go` is table-driven and covers more than 40 protected/readable
pairs across name, header, value, URL, structural, record and error rules. The
tests assert both directions: a selected value is replaced, and a name or URL
that must stay readable is not touched.

## Non-goals

- A general DLP product.
- Redacting the host's own logs or the vendor's logs.
- Encrypting retained records. Redaction removes values; storage protection is
  the host's job.
