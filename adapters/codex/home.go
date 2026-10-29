package codex

// CODEX_HOME (issue #54): codex keeps its whole user state under one directory
// — config.toml, auth.json, the SQLite databases and log/ (`codex doctor
// --json` of the installed codex-cli 0.165.0).
//
// userConfiguration=exclude runs a session on an isolated home instead: the
// login travels, configuration does not. The isolated home is a sibling of the
// user's own (`<dir>/<base>.oarlock-exclude`), it is persistent and per user
// home rather than per session, and it never holds a config.toml of ours — so a
// session that excludes the user's configuration cannot pick ours up by
// accident.
//
// The two directions are one-way: this file reads the user's home and writes
// only the isolated one. Live-unverified: there is no logged-in codex here, so
// the layout is the one `codex doctor --json` reports and nothing else.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// codexHomeEnv is the variable codex reads its home directory from.
	codexHomeEnv = "CODEX_HOME"
	// defaultCodexHomeDir is codex's home inside the user home when CODEX_HOME
	// is unset.
	defaultCodexHomeDir = ".codex"
	// isolatedHomeSuffix marks the home of a userConfiguration=exclude session.
	isolatedHomeSuffix = ".oarlock-exclude"
	// loginFile is the only file of the user's home the isolated home takes:
	// it is the login, not configuration.
	loginFile = "auth.json"
)

// codexHome resolves the user's codex home from env and cwd: $CODEX_HOME (relative to cwd
// when relative), else <user home dir>/.codex.
func codexHome(env []string, cwd string) (string, error) {
	home := ""
	for _, kv := range env {
		// The last entry wins: env is a list, and a later set is the newer one.
		if v, ok := strings.CutPrefix(kv, codexHomeEnv+"="); ok {
			home = v
		}
	}
	switch {
	case home == "":
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("codex: cannot resolve the user's codex home: no %s and no user home directory: %w", codexHomeEnv, err)
		}
		if userHome == "" {
			return "", fmt.Errorf("codex: cannot resolve the user's codex home: neither %s nor a user home directory is set", codexHomeEnv)
		}
		home = filepath.Join(userHome, defaultCodexHomeDir)
	case !filepath.IsAbs(home):
		// A relative CODEX_HOME is relative to the session's working directory,
		// not to the process's (a relative cwd is resolved against the process's
		// by filepath.Abs below).
		home = filepath.Join(cwd, home)
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("codex: cannot resolve the codex home %q: %w", home, err)
	}
	return filepath.Clean(abs), nil
}

// isolatedHome prepares the persistent CODEX_HOME of a userConfiguration=exclude session
// and returns it. The user's own home is only read, never written.
func isolatedHome(home string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		// An isolated home must not depend on the process's working directory:
		// it is created once and reused by every later session.
		return "", fmt.Errorf("codex: the isolated codex home needs the user's codex home as an absolute path, got %q", home)
	}
	home = filepath.Clean(home)
	base := filepath.Base(home)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("codex: cannot derive an isolated codex home from %q", home)
	}
	isolated := filepath.Join(filepath.Dir(home), base+isolatedHomeSuffix)
	if isolated == home {
		return "", fmt.Errorf("codex: the isolated codex home would be the user's own home %q", home)
	}
	if err := os.MkdirAll(isolated, 0o700); err != nil {
		return "", fmt.Errorf("codex: cannot create the isolated codex home %q: %w", isolated, err)
	}
	// Only the login is carried over: config.toml is never created here and
	// never copied, so the isolated session reads no configuration of the
	// user's.
	if err := copyLogin(home, isolated); err != nil {
		return "", err
	}
	return isolated, nil
}

// copyLogin carries <home>/auth.json into the isolated home when the user's copy
// is missing there or newer than it, so a re-login in the user's home is picked
// up by the next open. A missing login is not an error: a signed-out machine
// stays signed out.
//
// The copy is written beside the destination and renamed into place: a second
// session opening while another copies sees the old login or the new one, never
// half of one.
func copyLogin(home, isolated string) error {
	src := filepath.Join(home, loginFile)
	dst := filepath.Join(isolated, loginFile)
	st, err := os.Stat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("codex: cannot read the codex login %q: %w", src, err)
	case !st.Mode().IsRegular():
		return fmt.Errorf("codex: the codex login %q is not a regular file", src)
	}
	if cur, err := os.Stat(dst); err == nil {
		if !st.ModTime().After(cur.ModTime()) {
			return nil // the isolated copy is at least as new
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("codex: cannot read the isolated codex login %q: %w", dst, err)
	}
	login, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("codex: cannot read the codex login %q: %w", src, err)
	}
	tmp, err := os.CreateTemp(isolated, loginFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("codex: cannot write the isolated codex login in %q: %w", isolated, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename below succeeded
	if _, err := tmp.Write(login); err != nil {
		tmp.Close()
		return fmt.Errorf("codex: cannot write the isolated codex login %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex: cannot write the isolated codex login %q: %w", name, err)
	}
	// CreateTemp asks for 0o600 and a umask may take bits away; chmod pins the
	// mode. The rename below replaces the destination, so this temporary file's
	// mode becomes the login's mode.
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("codex: cannot set the mode of the isolated codex login %q: %w", name, err)
	}
	if err := os.Rename(name, dst); err != nil {
		return fmt.Errorf("codex: cannot install the isolated codex login %q: %w", dst, err)
	}
	return nil
}
