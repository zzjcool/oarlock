package codex

// CODEX_HOME isolation (issue #54) on a real filesystem: the login travels, the
// configuration never does, and the two directions of the login copy are one-way
// (a logout in the user's home must reach the isolated home).

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsolatedHomeCarriesTheLoginOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	login := []byte(`{"tokens":{"access_token":"user-login"}}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[model]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	isolated, err := isolatedHome(home)
	if err != nil {
		t.Fatalf("isolatedHome: %v", err)
	}
	if want := filepath.Join(filepath.Dir(home), filepath.Base(home)+isolatedHomeSuffix); isolated != want {
		t.Fatalf("isolated home = %q, want %q", isolated, want)
	}
	got, err := os.ReadFile(filepath.Join(isolated, "auth.json"))
	if err != nil || string(got) != string(login) {
		t.Fatalf("the isolated login = %q, %v; want the user's %q", got, err, login)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(isolated, "auth.json")); err == nil && st.Mode().Perm() != 0o600 {
			t.Errorf("the isolated login is %v; it must be 0600", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(isolated, "config.toml")); err == nil {
		t.Error("the isolated home holds a config.toml; it must hold none")
	}
}

func TestIsolatedHomeDropsTheLoginAfterALogout(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(home, "auth.json")
	if err := os.WriteFile(src, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	isolated, err := isolatedHome(home)
	if err != nil {
		t.Fatalf("isolatedHome: %v", err)
	}
	if _, err := os.Stat(filepath.Join(isolated, "auth.json")); err != nil {
		t.Fatalf("the login did not travel: %v", err)
	}
	// codex logout removes the user's login: the isolated copy must go with it,
	// or an exclude session would stay signed in.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedHome(home); err != nil {
		t.Fatalf("isolatedHome after the logout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(isolated, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("the isolated login survived the user's logout (stat: %v)", err)
	}
}

func TestCodexHomeResolvesFromEnvOrTheUserHome(t *testing.T) {
	dir := t.TempDir()
	got, err := codexHome([]string{"CODEX_HOME=rel/codex", "PATH=/bin"}, dir)
	if err != nil || got != filepath.Join(dir, "rel", "codex") {
		t.Errorf("a relative CODEX_HOME = %q, %v; want it resolved against the session's cwd", got, err)
	}
	got, err = codexHome([]string{"PATH=/bin"}, dir)
	if err != nil {
		t.Fatalf("codexHome without CODEX_HOME: %v", err)
	}
	if filepath.Base(got) != defaultCodexHomeDir {
		t.Errorf("codexHome without CODEX_HOME = %q, want the user home's %q", got, defaultCodexHomeDir)
	}
}
