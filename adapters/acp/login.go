package acp

// opencode is the only ACP profile with a verified auth channel: `opencode auth
// list` is a read-only ANSI box whose last line is `N credentials` and which
// prints one `●  <provider> <method>` row per stored credential (live opencode
// 1.19.5: `└  0 credentials`, exit 0). oarlock reads that box and holds no
// token: the credential store stays the vendor's.
//
// Login and Logout are typed refusals because both opencode commands are
// interactive — they select a provider on stdin — and oarlock relays no prompt,
// so running one would either block a host on a pipe or act on a selection
// nobody made. Every other ACP profile keeps Runtime.Login nil: no verified
// channel, so none is guessed.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// authStatusTimeout bounds the read-only `opencode auth list` (OAR uses 20 s
	// for a status read).
	authStatusTimeout = 20 * time.Second

	// loginReason and logoutReason are the two typed refusals, one line each.
	loginReason  = "opencode auth login is an interactive TUI that needs provider selection on stdin; oarlock relays no prompt"
	logoutReason = "opencode auth logout selects a provider on stdin when it is not named; oarlock drives no prompt"

	// outputTailLimit bounds how much vendor output an error carries: an error
	// message is the only place a caller sees why the vendor refused.
	outputTailLimit = 400
)

// loginFor is the LoginProvider a profile exposes: opencode's read-only auth
// status plus two typed refusals, nil for every other profile.
func loginFor(id string) oarlock.LoginProvider {
	if id == "opencode" {
		return opencodeLogin{}
	}
	return nil
}

// ansiCSI matches one CSI escape sequence: ESC [ parameters intermediates
// final. `opencode auth list` dims its box header and each credential's method
// with `ESC[90m`, so the box is parsed only after these are gone.
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// credentialCount is the box's last line, `└  N credentials`. (?m) keeps the
// match on one line; requiring exactly one match is what tells a count from
// prose that happens to mention credentials.
var credentialCount = regexp.MustCompile(`(?m)^.*?(\d+)\s+credential`)

// credentialRow is one stored credential, `●  <provider> <method>`.
var credentialRow = regexp.MustCompile(`(?m)^.*●\s*(.+)$`)

// opencodeLogin is opencode's auth surface: a read-only status and two
// refusals that start nothing.
type opencodeLogin struct{}

var _ oarlock.LoginProvider = opencodeLogin{}

// Login is a typed refusal and starts nothing: `opencode auth login` is a TUI
// that needs provider selection on stdin.
func (opencodeLogin) Login(context.Context) (oarlock.LoginChallenge, error) {
	return oarlock.LoginChallenge{}, &oarlock.UnsupportedOperation{Operation: "login", Reason: loginReason}
}

// Logout is a typed refusal and starts nothing: `opencode auth logout` asks for
// a provider on stdin when it is not named.
func (opencodeLogin) Logout(context.Context) error {
	return &oarlock.UnsupportedOperation{Operation: "logout", Reason: logoutReason}
}

// AuthStatus reads `opencode auth list`, the runtime's own answer to "is this
// machine signed in", and runs the binary the probe reports. A non-zero exit is
// an error, never "signed out": a timeout or a crash says nothing about the
// credential store.
func (opencodeLogin) AuthStatus(ctx context.Context) (oarlock.AuthStatus, error) {
	path, err := probe("opencode").find()
	if err != nil {
		return oarlock.AuthStatus{}, err
	}
	res, err := ops.Run(ctx, ops.Command{Path: path, Args: []string{"auth", "list"}}, authStatusTimeout)
	if err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("opencode: auth list: %w", err)
	}
	if res.ExitCode != 0 {
		return oarlock.AuthStatus{}, fmt.Errorf("opencode: auth list: exit %d: %s", res.ExitCode, clipOutput(res.Stdout+"\n"+res.Stderr))
	}
	return parseAuthList(res.Stdout)
}

// parseAuthList turns the `opencode auth list` box into a status. The count
// line is the vendor's answer and the only completion source: exactly one is
// required, because no count at all (a broken store, a changed box, output that
// is not a box) is "oarlock cannot read the answer", never "signed out".
func parseAuthList(stdout string) (oarlock.AuthStatus, error) {
	text := ansiCSI.ReplaceAllString(stdout, "")
	counts := credentialCount.FindAllStringSubmatch(text, -1)
	if len(counts) != 1 {
		detail := clipOutput(text)
		if detail == "" {
			detail = "(no output)"
		}
		return oarlock.AuthStatus{}, fmt.Errorf("opencode: auth list: want exactly one `N credentials` line, read %d: %s", len(counts), detail)
	}
	n, err := strconv.Atoi(counts[0][1])
	if err != nil {
		return oarlock.AuthStatus{}, fmt.Errorf("opencode: auth list: unreadable credential count %q: %w", counts[0][1], err)
	}
	status := oarlock.AuthStatus{Authenticated: n > 0, Detail: fmt.Sprintf("%d credentials", n)}
	if n > 0 {
		if providers := credentialProviders(text); len(providers) > 0 {
			status.Detail = strings.Join(providers, ", ")
		}
	}
	// Account stays empty: the box prints no identity, and oarlock does not
	// read the auth.json it points at.
	return status, nil
}

// credentialProviders renders each `●` row as `Provider (method)`. The vendor
// prints the provider and the stored credential's method on one line
// (`●  Anthropic api`, `●  OpenAI oauth`), so the last word is the method; a
// one-word row is the provider alone.
func credentialProviders(text string) []string {
	var out []string
	for _, row := range credentialRow.FindAllStringSubmatch(text, -1) {
		fields := strings.Fields(strings.Trim(row[1], " \t│|"))
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 1 {
			out = append(out, fields[0])
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s)", strings.Join(fields[:len(fields)-1], " "), fields[len(fields)-1]))
	}
	return out
}

// clipOutput bounds vendor text kept in an error without splitting a rune.
func clipOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= outputTailLimit {
		return s
	}
	start := len(s) - outputTailLimit
	for start < len(s) && s[start]&0xC0 == 0x80 {
		start++
	}
	return "...[truncated]..." + s[start:]
}
