package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zzjcool/oarlock"
)

func fakePi(t *testing.T, version, models string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake pi is a POSIX shell script; the pure parse/scan tests still run on Windows")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"--version) echo " + version + " ; exit 0;;\n" +
		"--list-models) cat <<'EOF'\n" + models + "\nEOF\nexit 0;;\n" +
		"esac\n" +
		"exit 2\n"
	path := filepath.Join(dir, "pi")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestParseModels_SkipsTheHeaderAndKeepsProviderAndID(t *testing.T) {
	out := "provider      model                          context  max-out  thinking  images\n" +
		"any-endpoint  workbuddy/deepseek-v4.1-flash  1M       128K     yes       yes\n" +
		"other         model-b                        2M       64K      no        no\n"
	models := parseModels(out)
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].Provider != "any-endpoint" || models[0].ID != "workbuddy/deepseek-v4.1-flash" {
		t.Fatalf("first model = %+v", models[0])
	}
	if models[1].Provider != "other" || models[1].ID != "model-b" {
		t.Fatalf("second model = %+v", models[1])
	}
}

func TestInventory_ReadsSkillsAndCommandsWithPerSectionStatus(t *testing.T) {
	fakePi(t, "1.4.0", "provider model\nany-endpoint workbuddy/x 1M 1K yes yes")
	root := t.TempDir()
	agent := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agent)

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(agent, "skills", "user-skill", "SKILL.md"), "---\nname: user-skill\ndescription: from user\n---\nbody\n")
	write(filepath.Join(root, ".pi", "skills", "project-skill", "SKILL.md"), "---\nname: project-skill\ndescription: from project\n---\nbody\n")
	write(filepath.Join(agent, "prompts", "global.md"), "# Global command\ndo it\n")
	write(filepath.Join(root, ".pi", "prompts", "deploy.md"), "# Deploy\nship it\n")

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	inv, err := (piInventories{}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Sections) != 4 {
		t.Fatalf("got %d sections, want 4: %+v", len(inv.Sections), inv.Sections)
	}
	byKind := map[oarlock.InventoryKind]oarlock.InventorySection{}
	for _, section := range inv.Sections {
		byKind[section.Kind] = section
	}
	models := byKind[oarlock.InventoryModels]
	if models.Status != oarlock.InventoryOK || len(models.Items) != 1 || models.Items[0].Name != "any-endpoint/workbuddy/x" {
		t.Fatalf("models = %+v", models)
	}
	skills := byKind[oarlock.InventorySkills]
	if skills.Status != oarlock.InventoryOK || len(skills.Items) != 2 {
		t.Fatalf("skills = %+v", skills)
	}
	commands := byKind[oarlock.InventoryCommands]
	if commands.Status != oarlock.InventoryOK || len(commands.Items) != 2 {
		t.Fatalf("commands = %+v", commands)
	}
	if byKind[oarlock.InventoryTools].Status != oarlock.InventoryUnsupported {
		t.Fatalf("tools = %+v, want unsupported", byKind[oarlock.InventoryTools])
	}
}

func TestInventory_OneBrokenSectionKeepsTheOthers(t *testing.T) {
	fakePi(t, "1.4.0", "provider model\nany-endpoint workbuddy/x 1M 1K yes yes")
	agent := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	// A file where the skills directory should be makes that reader fail.
	if err := os.WriteFile(filepath.Join(agent, "skills"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	inv, err := (piInventories{}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	for _, section := range inv.Sections {
		switch section.Kind {
		case oarlock.InventorySkills:
			if section.Status != oarlock.InventoryError || section.Reason == "" {
				t.Fatalf("skills = %+v, want an error with a reason", section)
			}
		case oarlock.InventoryModels:
			if section.Status != oarlock.InventoryOK {
				t.Fatalf("models = %+v, the broken skills section must not erase it", section)
			}
		}
	}
}

func TestProbe_ReportsInstalledVersionAndMismatch(t *testing.T) {
	fakePi(t, "1.4.0", "provider model")
	inst, err := (piProbe{}).Installation(context.Background())
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if !inst.Available || inst.Version != "1.4.0" || inst.Path == "" {
		t.Fatalf("installation = %+v", inst)
	}

	fakePi(t, "9.9.9", "provider model")
	inst, err = (piProbe{}).Installation(context.Background())
	var mismatch *VersionError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want *VersionError, got %T (%v)", err, err)
	}
	if !inst.Available || inst.Version != "9.9.9" {
		t.Fatalf("installation = %+v, want available with the actual version", inst)
	}
}

func TestRuntime_ExposesThePiAdoptionUnit(t *testing.T) {
	rt := Runtime()
	if rt.ID != "pi" || rt.Session == nil || rt.Refused == nil || rt.Probe == nil || rt.Models == nil || rt.Inventory == nil {
		t.Fatalf("Runtime() is missing a field: %+v", rt)
	}
	if rt.Refused.Unsupported(oarlock.OptionLaunchArgs) == nil {
		t.Fatal("launchArgs must stay refused until the adapter owns it")
	}
}

func TestInventoryLive(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to inventory the real Pi installation")
	}
	rt := Runtime()
	inst, err := rt.Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("installation: %v", err)
	}
	if !inst.Available || inst.Version != "1.4.0" {
		t.Fatalf("installation = %+v, want an available pi 1.4.0", inst)
	}
	inv, err := rt.Inventory.Inventory(context.Background())
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	var models, skills, commands int
	for _, section := range inv.Sections {
		switch section.Kind {
		case oarlock.InventoryModels:
			if section.Status != oarlock.InventoryOK {
				t.Fatalf("models = %+v", section)
			}
			models = len(section.Items)
		case oarlock.InventorySkills:
			skills = len(section.Items)
		case oarlock.InventoryCommands:
			commands = len(section.Items)
		}
	}
	if models < 1 {
		t.Fatalf("got %d models, want at least one on this host", models)
	}
	// The host may have no local skills or prompts; the issue only requires a
	// skill when one exists. Log what was found so the live evidence is visible.
	t.Logf("live inventory: models=%d skills=%d commands=%d", models, skills, commands)
}
