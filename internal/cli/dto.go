package cli

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

// Every --json document is one of these types. They are the CLI's public
// schema: the root package's structs may evolve, these fields do not silently
// change shape.

// ListDTO is `list --json`.
type ListDTO struct {
	Schema   string       `json:"schema"`
	Command  string       `json:"command"`
	Runtimes []RuntimeDTO `json:"runtimes"`
}

// RuntimeDTO is one registered runtime and the operations it declares.
type RuntimeDTO struct {
	ID           string          `json:"id"`
	Capabilities CapabilitiesDTO `json:"capabilities"`
}

// CapabilitiesDTO is the boolean presence table: true means the operation is
// implemented by this runtime, never "this account can do it".
type CapabilitiesDTO struct {
	Session   bool `json:"session"`
	Probe     bool `json:"probe"`
	Models    bool `json:"models"`
	Install   bool `json:"install"`
	Login     bool `json:"login"`
	Usage     bool `json:"usage"`
	Upgrade   bool `json:"upgrade"`
	Inventory bool `json:"inventory"`
}

// ProbeDTO is `probe --json`. A per-runtime error is isolated to its result and
// does not erase the others.
type ProbeDTO struct {
	Schema  string           `json:"schema"`
	Command string           `json:"command"`
	Results []ProbeResultDTO `json:"results"`
}

// ProbeResultDTO is one runtime's probe answer. Installation is present when
// the runtime was resolved; Error is set when the probe failed. An unavailable
// installation is not an error.
type ProbeResultDTO struct {
	Runtime      string           `json:"runtime"`
	Installation *InstallationDTO `json:"installation,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// InstallationDTO is the read-only installation probe.
type InstallationDTO struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	Source    string `json:"source,omitempty"`
}

// ModelsDTO is `models --json`.
type ModelsDTO struct {
	Schema  string            `json:"schema"`
	Command string            `json:"command"`
	Results []ModelsResultDTO `json:"results"`
}

// ModelsResultDTO is one runtime's model list. Exactly one of Models,
// Unsupported and Error tells the story: an empty list with no error is the
// runtime's honest "none", never a fabricated placeholder.
type ModelsResultDTO struct {
	Runtime      string           `json:"runtime"`
	Installation *InstallationDTO `json:"installation,omitempty"`
	Models       []ModelDTO       `json:"models,omitempty"`
	Unsupported  string           `json:"unsupported,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// ModelDTO is one selectable model as the runtime reports it.
type ModelDTO struct {
	Provider     string   `json:"provider,omitempty"`
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	EffortLevels []string `json:"effortLevels,omitempty"`
	ServiceTiers []string `json:"serviceTiers,omitempty"`
}

// RunDTO is `run --json`: the one final document, emitted after the turn and
// dispose. Unknown numbers stay absent rather than becoming zero.
type RunDTO struct {
	Schema      string      `json:"schema"`
	Command     string      `json:"command"`
	Runtime     string      `json:"runtime"`
	SessionID   string      `json:"sessionId,omitempty"`
	Outcome     string      `json:"outcome,omitempty"`
	Text        string      `json:"text,omitempty"`
	Turn        *TurnDTO    `json:"turn,omitempty"`
	Status      *StatusDTO  `json:"status,omitempty"`
	Usage       *UsageDTO   `json:"usage,omitempty"`
	Context     *ContextDTO `json:"context,omitempty"`
	Model       string      `json:"model,omitempty"`
	Effort      string      `json:"effort,omitempty"`
	ServiceTier string      `json:"serviceTier,omitempty"`
	RequestID   string      `json:"requestId,omitempty"`
	Seq         uint64      `json:"seq,omitempty"`
	Code        string      `json:"code,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// TurnDTO is the turn's end: the runtime's own report, or oarlock's aborted
// summary when the runtime never reported one inside the abort grace (see
// PromptAndWait). It is never an invented success.
type TurnDTO struct {
	Kind    string `json:"kind"`
	Failure string `json:"failure,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// StatusDTO is the folded root-agent status.
type StatusDTO struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase,omitempty"`
}

// UsageDTO is the root session's token accounting. The optional counts stay
// pointers: nil means "the runtime did not report this number".
type UsageDTO struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
	CacheRead  *int64 `json:"cacheRead,omitempty"`
	CacheWrite *int64 `json:"cacheWrite,omitempty"`
}

// ContextDTO is the latest reported context fullness.
type ContextDTO struct {
	Tokens *int64 `json:"tokens,omitempty"`
	Window *int64 `json:"window,omitempty"`
}

func modelDTOs(models []oarlock.Model) []ModelDTO {
	out := make([]ModelDTO, 0, len(models))
	for _, m := range models {
		out = append(out, ModelDTO{
			Provider:     m.Provider,
			ID:           m.ID,
			Name:         m.Name,
			EffortLevels: append([]string(nil), m.EffortLevels...),
			ServiceTiers: append([]string(nil), m.ServiceTiers...),
		})
	}
	return out
}

// runDTO folds the retained records into the stable run view. It runs before
// dispose so a completed turn reads as idle rather than exited; the voyage
// record still keeps every record, dispose included.
func runDTO(runtimeID string, records []oarlock.Record, res oarlock.RunResult, requestedModel, requestedEffort, requestedServiceTier string) RunDTO {
	dto := RunDTO{
		Schema:      Schema,
		Command:     "run",
		Runtime:     runtimeID,
		SessionID:   rootSessionID(records),
		Outcome:     string(res.Outcome),
		Text:        res.Text,
		Model:       requestedModel,
		Effort:      requestedEffort,
		ServiceTier: requestedServiceTier,
		RequestID:   res.RequestID,
		Seq:         res.Seq,
	}
	if res.Turn != nil {
		dto.Turn = &TurnDTO{Kind: string(res.Turn.Kind), Failure: string(res.Turn.Failure), Reason: res.Turn.Reason}
	}
	if res.Code != "" {
		dto.Code = string(res.Code)
	}
	dto.Reason = res.Reason

	status := observe.StatusOf(records)
	dto.Status = &StatusDTO{Kind: string(status.Value.Kind), Phase: string(status.Value.Phase.Kind)}

	if model := observe.ModelOf(records).Value; model != "" {
		dto.Model = model
	}
	if effort := observe.EffortOf(records).Value; effort != "" {
		dto.Effort = effort
	}
	if tier := observe.ServiceTierOf(records).Value; tier != "" {
		dto.ServiceTier = tier
	}
	if usage := observe.UsageOf(records).Value; usage.Total != nil {
		dto.Usage = &UsageDTO{
			Input:      usage.Total.Input,
			Output:     usage.Total.Output,
			Reasoning:  usage.Total.Reasoning,
			CacheRead:  usage.Total.CacheRead,
			CacheWrite: usage.Total.CacheWrite,
		}
	}
	if report := observe.ContextUsageOf(records).Value; report != nil {
		dto.Context = &ContextDTO{Tokens: report.Usage.Tokens, Window: report.Usage.ContextWindow}
	}
	return dto
}

// rootSessionID is the stream's session id: the first root record that carries
// one. A child session's id never names the root run.
func rootSessionID(records []oarlock.Record) string {
	for _, rec := range records {
		if rec.SessionID != "" && len(rec.AgentPath) == 0 {
			return rec.SessionID
		}
	}
	return ""
}
