package oarlock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// sessionStub embeds the interface so it satisfies Session without a hand
// written method set; the embedded value is never called in these tests.
type sessionStub struct{ Session }

type steerStub struct{ Session }

func (steerStub) Steer(context.Context, string, InputOptions) (ControlOutcome, error) {
	return ControlOutcome{}, nil
}

func TestCapability_SteerIsInterfacePresenceNotABoolean(t *testing.T) {
	var plain Session = sessionStub{}
	if _, ok := plain.(Steerer); ok {
		t.Fatal("a plain session unexpectedly implements Steerer")
	}

	var steering Session = steerStub{}
	if _, ok := steering.(Steerer); !ok {
		t.Fatal("a session with Steer does not satisfy Steerer")
	}
}

func TestRefusedOptions_DeclaredAndPresentIsRefused(t *testing.T) {
	refused := RefusedOptions{OptionEnv: "this runtime has no per-session environment"}
	opt := Options{Env: map[string]*string{"A": ptr("b")}}

	err := refused.Check(opt)

	var unsupported *UnsupportedOption
	if !errors.As(err, &unsupported) {
		t.Fatalf("want *UnsupportedOption, got %T (%v)", err, err)
	}
	if unsupported.Option != OptionEnv || unsupported.Reason == "" {
		t.Fatalf("refusal lost its identity: %+v", unsupported)
	}
}

func TestRefusedOptions_DeclaredButAbsentIsAccepted(t *testing.T) {
	refused := RefusedOptions{OptionEnv: "no env"}
	if err := refused.Check(Options{}); err != nil {
		t.Fatalf("an absent option must not be refused: %v", err)
	}
}

func TestRefusedOptions_UndeclaredButPresentIsNotRefusedByTheTable(t *testing.T) {
	if err := (RefusedOptions{}).Check(Options{Name: "mine"}); err != nil {
		t.Fatalf("the shared table cannot refuse an undeclared option: %v", err)
	}
}

// TestRefusedOptions_EveryDeclaredOptionIsActuallyRefused is the anti-drift
// test: each frozen Option must be triggerable by a present value. A new
// constant without a check fails here.
func TestRefusedOptions_EveryDeclaredOptionIsActuallyRefused(t *testing.T) {
	cases := map[Option]Options{
		OptionSystemPrompt:          {SystemPrompt: "x"},
		OptionAppendSystemPrompt:    {AppendSystemPrompt: "x"},
		OptionEnv:                   {Env: map[string]*string{"A": ptr("b")}},
		OptionMCPServers:            {MCPServers: []MCPServer{{Name: "s", Command: "x"}}},
		OptionMCPServerCredentials:  {MCPServers: []MCPServer{{Name: "s", Command: "x", Env: map[string]string{"K": "V"}}}},
		OptionDisallowedTools:       {DisallowedTools: []string{"bash"}},
		OptionLaunchArgs:            {LaunchArgs: []string{"--x"}},
		OptionServiceTier:           {ServiceTier: "fast"},
		OptionAdditionalDirectories: {AdditionalDirectories: []string{"../x"}},
		OptionOutputSchema:          {OutputSchema: json.RawMessage(`{}`)},
		OptionName:                  {Name: "n"},
		OptionUserConfiguration:     {UserConfiguration: UserConfigurationExclude},
	}

	if len(cases) != len(AllRefusableOptions) {
		t.Fatalf("the option table has %d entries, the trigger table has %d", len(AllRefusableOptions), len(cases))
	}
	for _, option := range AllRefusableOptions {
		opt, ok := cases[option]
		if !ok {
			t.Fatalf("frozen option %q has no trigger case", option)
		}
		refused := RefusedOptions{option: "no"}
		err := refused.Check(opt)
		var unsupported *UnsupportedOption
		if !errors.As(err, &unsupported) || unsupported.Option != option {
			t.Fatalf("option %q declared but not refused: %v", option, err)
		}
	}
}

func TestRefusedOptions_MCPCredentialsNeedAnActualCredential(t *testing.T) {
	refused := RefusedOptions{OptionMCPServerCredentials: "no credentials"}
	if err := refused.Check(Options{MCPServers: []MCPServer{{Name: "s", Command: "x"}}}); err != nil {
		t.Fatalf("a credential-free server must not be refused: %v", err)
	}
	if err := refused.Check(Options{MCPServers: []MCPServer{{Name: "s", Type: "http", URL: "https://x", Headers: map[string]string{"Authorization": "Bearer y"}}}}); err == nil {
		t.Fatal("an http server with headers must be refused")
	}
}

func TestRefusedOptions_UserConfigurationOnlyRefusesExclude(t *testing.T) {
	refused := RefusedOptions{OptionUserConfiguration: "no separation"}
	if err := refused.Check(Options{UserConfiguration: UserConfigurationInclude}); err != nil {
		t.Fatalf("include must not be refused: %v", err)
	}
	if err := refused.Check(Options{UserConfiguration: UserConfigurationExclude}); err == nil {
		t.Fatal("exclude must be refused")
	}
}

func TestRuntime_CheckOptionsUsesTheDeclaredTable(t *testing.T) {
	rt := Runtime{Refused: RefusedOptions{OptionName: "cannot name sessions"}}
	if err := rt.CheckOptions(Options{Name: "n"}); err == nil {
		t.Fatal("Runtime.CheckOptions ignored its own declaration")
	}
}

func TestAttributionTier_HasTheFourFrozenValues(t *testing.T) {
	got := []AttributionTier{AttributionNone, AttributionOpaque, AttributionAttributed, AttributionNested}
	want := []string{"none", "opaque", "attributed", "nested"}
	for i, tier := range got {
		if string(tier) != want[i] {
			t.Fatalf("tier %d = %q, want %q", i, tier, want[i])
		}
	}
}
