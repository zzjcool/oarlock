package acp

// opencode's auth surface against a fake `opencode` (issue #69, docs/ops.md
// §2/§5): the box is the vendor's stdout, so a #!/bin/sh script printing a
// captured box pins the parse without a vendor account.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzjcool/oarlock"
)

const (
	// esc is the start of an ANSI escape sequence.
	esc = "\x1b["

	// liveZeroCredentials is `opencode auth list` stdout on opencode 1.19.5 on a
	// machine with no stored credential: a dimmed box whose last line is the
	// count. (The leading ESC[0m reset goes to stderr and is not here.)
	liveZeroCredentials = "┌  Credentials " + esc + "90m~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n"

	// liveTwoCredentials is the same box with two stored credentials: an
	// Anthropic API key and an OpenAI OAuth login.
	liveTwoCredentials = "┌  Credentials " + esc + "90m~/.local/share/opencode/auth.json\n│\n" +
		"●  Anthropic " + esc + "90mapi\n│\n●  OpenAI " + esc + "90moauth\n│\n└  2 credentials\n\n"
)

// fakeOpenCode puts one fake `opencode` on an isolated PATH and returns its
// fakePATH, so a test can also add a marker the fake would leave if it ran.
func fakeOpenCode(t *testing.T, body string) *fakePATH {
	t.Helper()
	f := newFakePATH(t)
	f.add(t, "opencode", body)
	return f
}

// authListBody is a fake `opencode auth list`: it prints stdout on stdout with
// exit 0 for that argv and refuses every other invocation, so a test cannot
// pass by accident.
func authListBody(stdout string) string {
	return "if [ \"$1\" = auth ] && [ \"$2\" = list ]; then\nprintf '%s' '" + stdout + "'\nexit 0\nfi\nexit 9\n"
}

// TestOpenCodeAuthStatus_ZeroCredentialsIsSignedOut is the live shape: the
// vendor said zero, so the answer is signed out — and it is an answer, not an
// error.
func TestOpenCodeAuthStatus_ZeroCredentialsIsSignedOut(t *testing.T) {
	fakeOpenCode(t, authListBody(liveZeroCredentials))

	status, err := Profile{ID: "opencode"}.Runtime().Login.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if status.Authenticated {
		t.Fatalf("status = %+v, want signed out: the box says 0 credentials", status)
	}
	if status.Detail != "0 credentials" {
		t.Fatalf("Detail = %q, want %q", status.Detail, "0 credentials")
	}
	if status.Account != "" {
		t.Fatalf("Account = %q, want empty: the box prints no identity", status.Account)
	}
}

// TestOpenCodeAuthStatus_TwoCredentialsNamesBothProviders pins the ANSI-stripped
// parse of the credential rows behind a live-shaped box.
func TestOpenCodeAuthStatus_TwoCredentialsNamesBothProviders(t *testing.T) {
	fakeOpenCode(t, authListBody(liveTwoCredentials))

	status, err := Profile{ID: "opencode"}.Runtime().Login.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if !status.Authenticated {
		t.Fatalf("status = %+v, want signed in: the box says 2 credentials", status)
	}
	if want := "Anthropic (api), OpenAI (oauth)"; status.Detail != want {
		t.Fatalf("Detail = %q, want %q", status.Detail, want)
	}
	if status.Account != "" {
		t.Fatalf("Account = %q, want empty: the box prints no identity", status.Account)
	}
}

// TestOpenCodeAuthStatus_CountWithoutRowsKeepsTheCount is the Detail fallback:
// a positive count with no parsable row still answers, and says how many.
func TestOpenCodeAuthStatus_CountWithoutRowsKeepsTheCount(t *testing.T) {
	fakeOpenCode(t, authListBody("└  1 credentials\n"))

	status, err := Profile{ID: "opencode"}.Runtime().Login.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if !status.Authenticated || status.Detail != "1 credentials" {
		t.Fatalf("status = %+v, want signed in with the count as Detail", status)
	}
}

// TestOpenCodeAuthStatus_UnparsableOutputIsAnError holds the "never a fake
// signed-out" rule: output oarlock cannot read (no count line, or two) is an
// error, because only the vendor saying zero means zero.
func TestOpenCodeAuthStatus_UnparsableOutputIsAnError(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
	}{
		{"garbage", "signed in, probably\n"},
		{"no-count-line", "┌  Credentials ~/.local/share/opencode/auth.json\n│\n└  no credentials\n"},
		{"two-count-lines", liveZeroCredentials + liveTwoCredentials},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeOpenCode(t, authListBody(tc.stdout))
			status, err := Profile{ID: "opencode"}.Runtime().Login.AuthStatus(context.Background())
			if err == nil {
				t.Fatalf("AuthStatus = %+v, want an error: unreadable output is not a sign-out", status)
			}
			if status.Authenticated {
				t.Fatalf("status = %+v, want no claim", status)
			}
		})
	}
}

// TestOpenCodeAuthStatus_NonZeroExitIsAnError is the other unreadable case: a
// crash or a timeout must not read as signed out, even when the box looks fine.
func TestOpenCodeAuthStatus_NonZeroExitIsAnError(t *testing.T) {
	fakeOpenCode(t, "if [ \"$1\" = auth ] && [ \"$2\" = list ]; then\nprintf '%s' '"+liveZeroCredentials+"'\nexit 2\nfi\nexit 9\n")

	status, err := Profile{ID: "opencode"}.Runtime().Login.AuthStatus(context.Background())
	if err == nil {
		t.Fatalf("AuthStatus = %+v, want an error: exit 2 says nothing about the credential store", status)
	}
	if status.Authenticated {
		t.Fatalf("status = %+v, want no claim", status)
	}
}

// TestOpenCodeLoginAndLogoutAreTypedRefusalsThatStartNothing pins the two
// refusal strings and proves neither command runs the vendor CLI: the fake
// opencode would leave a marker.
func TestOpenCodeLoginAndLogoutAreTypedRefusalsThatStartNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	fakeOpenCode(t, "touch '"+marker+"'\n")

	login := Profile{ID: "opencode"}.Runtime().Login
	if login == nil {
		t.Fatal("Login is nil; opencode exposes a read-only auth status")
	}

	challenge, err := login.Login(context.Background())
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("Login err = %v, want *oarlock.UnsupportedOperation", err)
	}
	if unsupported.Operation != "login" {
		t.Fatalf("Login Operation = %q, want login", unsupported.Operation)
	}
	if want := "opencode auth login is an interactive TUI that needs provider selection on stdin; oarlock relays no prompt"; unsupported.Reason != want {
		t.Fatalf("Login Reason = %q, want %q", unsupported.Reason, want)
	}
	if challenge != (oarlock.LoginChallenge{}) {
		t.Fatalf("challenge = %+v, want the zero value: a refusal returns no challenge", challenge)
	}

	err = login.Logout(context.Background())
	unsupported = nil
	if !errors.As(err, &unsupported) {
		t.Fatalf("Logout err = %v, want *oarlock.UnsupportedOperation", err)
	}
	if unsupported.Operation != "logout" {
		t.Fatalf("Logout Operation = %q, want logout", unsupported.Operation)
	}
	if want := "opencode auth logout selects a provider on stdin when it is not named; oarlock drives no prompt"; unsupported.Reason != want {
		t.Fatalf("Logout Reason = %q, want %q", unsupported.Reason, want)
	}

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Login or Logout started the vendor CLI (it left %s)", marker)
	}
}
