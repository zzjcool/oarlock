package pi

// Inventories: read-only answers to "what can this runtime run now?" (issue
// #14). Four sections share one result type, each with its own status, so one
// unreadable directory does not erase the others. Nothing here installs or
// changes configuration, and the only command run is `pi --list-models`
// (models can require the vendor's network; skills/commands are local reads).
//
// Tools are deliberately `unsupported`: Pi 1.4.0's RPC surface does not expose
// its built-in/MCP tool inventory without opening a session and inspecting
// extensions, and guessing from --help would be a false answer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

// Runtime returns the Pi adoption unit. Session is Open; Refused is the same
// table Open checks (issue #15); Probe, Models and Inventory are read-only.
func Runtime() oarlock.Runtime {
	return oarlock.Runtime{
		ID:        "pi",
		Session:   Open,
		Refused:   Refused(),
		Probe:     piProbe{},
		Models:    piModels{},
		Inventory: piInventories{},
	}
}

// piProbe answers whether `pi` is installed, without an account or network.
type piProbe struct{}

func (piProbe) Installation(ctx context.Context) (oarlock.Installation, error) {
	path, err := osexec.LookPath(DefaultExecutable)
	if err != nil {
		return oarlock.Installation{Available: false, Source: "path"}, nil
	}
	version, err := CheckVersion(ctx, VersionCheck{Command: path})
	if err != nil {
		var mismatch *VersionError
		if errors.As(err, &mismatch) {
			// Installed, but outside the support window. The host sees both
			// facts: Available is true, and the gate's error is returned.
			return oarlock.Installation{Available: true, Version: mismatch.Actual, Path: path, Source: "path"}, err
		}
		return oarlock.Installation{Available: true, Path: path, Source: "path"}, err
	}
	return oarlock.Installation{Available: true, Version: version, Path: path, Source: "path"}, nil
}

// piModels lists what `pi --list-models` reports.
type piModels struct{}

func (piModels) ListModels(ctx context.Context) ([]oarlock.Model, error) {
	out, stderr, err := runPi(ctx, "--list-models")
	if err != nil {
		return nil, fmt.Errorf("pi --list-models: %w: %s", err, strings.TrimSpace(stderr))
	}
	return parseModels(out), nil
}

func parseModels(out string) []oarlock.Model {
	var models []oarlock.Model
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.EqualFold(fields[0], "provider") {
			continue
		}
		model := oarlock.Model{Provider: fields[0], ID: fields[1], Name: fields[1]}
		models = append(models, model)
	}
	return models
}

// piInventories reads the local skill/command directories and the model list.
type piInventories struct{}

func (piInventories) Inventory(ctx context.Context) (oarlock.Inventory, error) {
	agentDir, err := AgentDir(os.Environ())
	if err != nil {
		return oarlock.Inventory{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return oarlock.Inventory{}, err
	}

	inv := oarlock.Inventory{Sections: []oarlock.InventorySection{
		modelsSection(ctx),
		skillsSection(cwd, agentDir),
		commandsSection(cwd, agentDir),
		{
			Kind:   oarlock.InventoryTools,
			Status: oarlock.InventoryUnsupported,
			Reason: "Pi 1.4.0 does not expose its tool/MCP inventory over RPC without a session",
		},
	}}
	return inv, nil
}

func modelsSection(ctx context.Context) oarlock.InventorySection {
	section := oarlock.InventorySection{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK}
	models, err := (piModels{}).ListModels(ctx)
	if err != nil {
		section.Status = oarlock.InventoryError
		section.Reason = err.Error()
		return section
	}
	for _, model := range models {
		section.Items = append(section.Items, oarlock.InventoryItem{
			Name:   model.Provider + "/" + model.ID,
			Source: "runtime",
		})
	}
	return section
}

func skillsSection(cwd, agentDir string) oarlock.InventorySection {
	section := oarlock.InventorySection{Kind: oarlock.InventorySkills, Status: oarlock.InventoryOK}
	add := func(dir, source string) error {
		items, err := readSkills(dir, source)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		section.Items = append(section.Items, items...)
		return nil
	}
	for _, root := range []struct{ dir, source string }{
		{filepath.Join(agentDir, "skills"), "user"},
		{filepath.Join(cwd, ".pi", "skills"), "project"},
		{filepath.Join(cwd, ".agents", "skills"), "project"},
	} {
		if err := add(root.dir, root.source); err != nil {
			section.Status = oarlock.InventoryError
			section.Reason = err.Error()
		}
	}
	return section
}

func commandsSection(cwd, agentDir string) oarlock.InventorySection {
	section := oarlock.InventorySection{Kind: oarlock.InventoryCommands, Status: oarlock.InventoryOK}
	addDir := func(dir, source string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".md")
			description, _ := readPromptDescription(filepath.Join(dir, entry.Name()))
			section.Items = append(section.Items, oarlock.InventoryItem{
				Name: name, Source: source, Description: description,
			})
		}
		return nil
	}
	for _, root := range []struct{ dir, source string }{
		{filepath.Join(agentDir, "prompts"), "user"},
		{filepath.Join(cwd, ".pi", "prompts"), "project"},
	} {
		if err := addDir(root.dir, root.source); err != nil {
			section.Status = oarlock.InventoryError
			section.Reason = err.Error()
		}
	}
	return section
}

func readSkills(dir, source string) ([]oarlock.InventoryItem, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	var items []oarlock.InventoryItem
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		name, description := readSkillHeader(path)
		if name == "" {
			name = entry.Name()
		}
		items = append(items, oarlock.InventoryItem{Name: name, Source: source, Description: description})
	}
	return items, nil
}

func readSkillHeader(path string) (name, description string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(string(data), "\n")
	inFront := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFront = true
			continue
		}
		if inFront {
			if trimmed == "---" {
				inFront = false
				continue
			}
			if key, value, ok := strings.Cut(trimmed, ":"); ok {
				switch strings.TrimSpace(key) {
				case "name":
					name = strings.TrimSpace(value)
				case "description":
					description = strings.Trim(strings.TrimSpace(value), `"'`)
				}
			}
			continue
		}
		if description == "" && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			description = trimmed
		}
	}
	return strings.Trim(name, `"'`), description
}

func readPromptDescription(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "---") {
			continue
		}
		return strings.TrimPrefix(line, "# "), nil
	}
	return "", nil
}

// runPi runs one read-only Pi command and returns stdout and stderr. It uses
// internal/exec so every member of the command's tree is closed on return.
func runPi(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd, err := exec.Start(ctx, &exec.Command{Path: DefaultExecutable, Args: args})
	if err != nil {
		return "", "", err
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	var (
		wg             sync.WaitGroup
		stdout, stderr string
	)
	wg.Add(2)
	go func() { defer wg.Done(); b, _ := io.ReadAll(cmd.Stdout()); stdout = string(b) }()
	go func() { defer wg.Done(); b, _ := io.ReadAll(cmd.Stderr()); stderr = string(b) }()
	status, waitErr := cmd.Wait(ctx)
	wg.Wait()
	if waitErr != nil {
		return stdout, stderr, waitErr
	}
	if status.Signal != nil {
		return stdout, stderr, fmt.Errorf("killed by signal %v", status.Signal)
	}
	if status.Code != 0 {
		return stdout, stderr, fmt.Errorf("exit code %d", status.Code)
	}
	return stdout, stderr, nil
}
