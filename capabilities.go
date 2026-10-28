package oarlock

import (
	"encoding/json"
	"fmt"
)

// Option names one session option a runtime may refuse. The spelling matches
// the field in Options, so a host can show the refusal without knowing the
// runtime.
type Option string

const (
	OptionSystemPrompt          Option = "systemPrompt"
	OptionAppendSystemPrompt    Option = "appendSystemPrompt"
	OptionEnv                   Option = "env"
	OptionMCPServers            Option = "mcpServers"
	OptionMCPServerCredentials  Option = "mcpServerCredentials"
	OptionDisallowedTools       Option = "disallowedTools"
	OptionLaunchArgs            Option = "launchArgs"
	OptionServiceTier           Option = "serviceTier"
	OptionAdditionalDirectories Option = "additionalDirectories"
	OptionOutputSchema          Option = "outputSchema"
	OptionName                  Option = "name"
	OptionUserConfiguration     Option = "userConfiguration"
)

// AllRefusableOptions is the frozen set. Tests use it to prove the declaration
// table and the refusal behaviour cannot drift.
var AllRefusableOptions = []Option{
	OptionSystemPrompt,
	OptionAppendSystemPrompt,
	OptionEnv,
	OptionMCPServers,
	OptionMCPServerCredentials,
	OptionDisallowedTools,
	OptionLaunchArgs,
	OptionServiceTier,
	OptionAdditionalDirectories,
	OptionOutputSchema,
	OptionName,
	OptionUserConfiguration,
}

// UnsupportedOption is returned before anything starts when a runtime cannot
// honour an option. The option is named; the reason is the runtime's. A caller
// may try another runtime or fall back without reading prose.
type UnsupportedOption struct {
	Option Option
	Reason string
}

func (e *UnsupportedOption) Error() string {
	return fmt.Sprintf("unsupported option %q: %s", e.Option, e.Reason)
}

// RefusedOptions maps an option to the reason an open would refuse it. It is
// the single source of truth for both the pre-open declaration and the check.
type RefusedOptions map[Option]string

// Unsupported returns the typed refusal for opt, or nil when the table does
// not refuse it.
func (r RefusedOptions) Unsupported(opt Option) *UnsupportedOption {
	reason, ok := r[opt]
	if !ok {
		return nil
	}
	return &UnsupportedOption{Option: opt, Reason: reason}
}

// Check refuses every option that is both declared in the table and actually
// present in opt. A declaration alone is not an error: a host may send the
// same Options to every runtime and only a present refused value fails.
func (r RefusedOptions) Check(opt Options) error {
	refuse := func(o Option, present bool) error {
		if !present {
			return nil
		}
		if unsupported := r.Unsupported(o); unsupported != nil {
			return unsupported
		}
		return nil
	}

	if err := refuse(OptionSystemPrompt, opt.SystemPrompt != ""); err != nil {
		return err
	}
	if err := refuse(OptionAppendSystemPrompt, opt.AppendSystemPrompt != ""); err != nil {
		return err
	}
	if err := refuse(OptionEnv, len(opt.Env) > 0); err != nil {
		return err
	}
	if err := refuse(OptionMCPServers, len(opt.MCPServers) > 0); err != nil {
		return err
	}
	if err := refuse(OptionMCPServerCredentials, anyMCPServerHasCredentials(opt.MCPServers)); err != nil {
		return err
	}
	if err := refuse(OptionDisallowedTools, len(opt.DisallowedTools) > 0); err != nil {
		return err
	}
	if err := refuse(OptionLaunchArgs, len(opt.LaunchArgs) > 0); err != nil {
		return err
	}
	if err := refuse(OptionServiceTier, opt.ServiceTier != ""); err != nil {
		return err
	}
	if err := refuse(OptionAdditionalDirectories, len(opt.AdditionalDirectories) > 0); err != nil {
		return err
	}
	if err := refuse(OptionOutputSchema, len(opt.OutputSchema) > 0); err != nil {
		return err
	}
	if err := refuse(OptionName, opt.Name != ""); err != nil {
		return err
	}
	if err := refuse(OptionUserConfiguration, opt.UserConfiguration == UserConfigurationExclude); err != nil {
		return err
	}
	return nil
}

func anyMCPServerHasCredentials(servers []MCPServer) bool {
	for _, s := range servers {
		if len(s.Env) > 0 || len(s.Headers) > 0 {
			return true
		}
	}
	return false
}

// UserConfiguration selects whether the runtime's user-level configuration is
// loaded. The zero value means the native default (include).
type UserConfiguration string

const (
	UserConfigurationDefault UserConfiguration = ""
	UserConfigurationInclude UserConfiguration = "include"
	UserConfigurationExclude UserConfiguration = "exclude"
)

// MCPServer is one MCP server attached to a session. A stdio server sets
// Command (and optionally Args, Env); an HTTP server sets Type "http", URL and
// optionally Headers.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Type    string
	URL     string
	Headers map[string]string
}

// IsHTTP reports whether this entry is a streamable HTTP server.
func (s MCPServer) IsHTTP() bool { return s.Type == "http" }

// Options is what a caller asks for when opening a session. An option a
// runtime cannot honour is refused with *UnsupportedOption before anything
// starts; it is never silently dropped.
//
// The zero value is valid: the runtime's defaults, the process's working
// directory, and the native login and configuration.
type Options struct {
	// Cwd is the directory the runtime operates in.
	Cwd string
	// AdditionalDirectories are extra workspace roots, relative to Cwd are
	// resolved by the adapter.
	AdditionalDirectories []string
	// Model is the runtime-native model identifier. Empty means the runtime's
	// default.
	Model string
	// Effort is the runtime-native reasoning level. Empty means the runtime's
	// default or the resumed value.
	Effort string
	// ServiceTier is the runtime-native tier; "default" explicitly opts out.
	ServiceTier string
	// OutputSchema is a JSON Schema passed through unchanged. oarlock never
	// validates it or the output.
	OutputSchema json.RawMessage
	// Name is the session's display name in the runtime's own UI.
	Name string
	// Resume reopens the native conversation with this id. The stream starts
	// fresh at Seq 1.
	Resume string
	// Env changes the environment of processes this session spawns. A nil
	// value removes the variable. The host environment is never changed.
	Env map[string]*string
	// UserConfiguration defaults to include.
	UserConfiguration UserConfiguration
	// SystemPrompt replaces the runtime's built-in system prompt.
	SystemPrompt string
	// AppendSystemPrompt appends to the runtime's built-in system prompt.
	AppendSystemPrompt string
	// MCPServers are attached on top of the user's own servers.
	MCPServers []MCPServer
	// DisallowedTools disables native tool names through the runtime's own
	// deny channel.
	DisallowedTools []string
	// LaunchArgs are extra runtime arguments passed unchecked. A runtime that
	// owns its RPC startup arguments refuses this option.
	LaunchArgs []string
}
