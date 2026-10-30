package cli

import (
	"time"

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

// StatusDTO is `status --json`: the login state each runtime reports. A
// signed-out machine is data, not an error — the reader answered.
type StatusDTO struct {
	Schema  string            `json:"schema"`
	Command string            `json:"command"`
	Results []StatusResultDTO `json:"results"`
}

// StatusResultDTO is one runtime's login answer. Auth is present only when the
// runtime answered AuthStatus; Unsupported and Error are the two ways it did
// not.
type StatusResultDTO struct {
	Runtime     string   `json:"runtime"`
	Auth        *AuthDTO `json:"auth,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// AuthDTO is the vendor's answer to "is this machine signed in?". Only what
// the vendor is willing to name is mirrored: tokens never pass through oarlock.
type AuthDTO struct {
	Authenticated bool   `json:"authenticated"`
	Account       string `json:"account,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// UsageDTO is `usage --json`: the account-level quota each runtime reports,
// never the session token counts of `run --json`.
type UsageDTO struct {
	Schema  string           `json:"schema"`
	Command string           `json:"command"`
	Results []UsageResultDTO `json:"results"`
}

// UsageResultDTO is one runtime's account usage. Usage is present only when
// the reader answered; a failed read leaves it nil rather than echoing a
// plausible quota of zero.
type UsageResultDTO struct {
	Runtime     string           `json:"runtime"`
	Usage       *AccountUsageDTO `json:"usage,omitempty"`
	Unsupported string           `json:"unsupported,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// AccountUsageDTO is the plan and the windows the vendor reported. Both stay
// empty when the vendor reported nothing: no window, limit or reset is ever
// synthesized.
type AccountUsageDTO struct {
	Plan    string           `json:"plan,omitempty"`
	Windows []UsageWindowDTO `json:"windows,omitempty"`
}

// UsageWindowDTO is one quota window. ResetsAt is RFC3339 and absent when the
// vendor reported no reset instant.
type UsageWindowDTO struct {
	Name     string  `json:"name"`
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit"`
	Unit     string  `json:"unit,omitempty"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

// UpdateDTO is `update --check --json`. The command is check-only, so this
// document never reports an upgrade that happened.
type UpdateDTO struct {
	Schema  string            `json:"schema"`
	Command string            `json:"command"`
	Results []UpdateResultDTO `json:"results"`
}

// UpdateResultDTO is one runtime's update check. Update is present only when
// the check answered; "no update available" is data, not an error.
type UpdateResultDTO struct {
	Runtime     string         `json:"runtime"`
	Update      *UpdateInfoDTO `json:"update,omitempty"`
	Unsupported string         `json:"unsupported,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// UpdateInfoDTO is the read-only comparison of the installed and published
// version. A runtime that cannot name either leaves the field empty.
type UpdateInfoDTO struct {
	Available bool   `json:"available"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
}

// InventoryDTO is `inventory --json`: what each runtime can offer this machine
// right now. Sections carry their own status, so a partial success stays
// readable instead of collapsing into one error.
type InventoryDTO struct {
	Schema  string               `json:"schema"`
	Command string               `json:"command"`
	Results []InventoryResultDTO `json:"results"`
}

// InventoryResultDTO is one runtime's inventory. Sections keep the runtime's
// own order; Error is set only when the call itself failed, and the sections
// read before the failure are kept.
type InventoryResultDTO struct {
	Runtime     string                `json:"runtime"`
	Sections    []InventorySectionDTO `json:"sections,omitempty"`
	Unsupported string                `json:"unsupported,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// InventorySectionDTO is one section verbatim: kind and status are the runtime's
// words, and an empty-but-ok section stays empty.
type InventorySectionDTO struct {
	Kind   string             `json:"kind"`
	Status string             `json:"status"`
	Reason string             `json:"reason,omitempty"`
	Items  []InventoryItemDTO `json:"items,omitempty"`
}

// InventoryItemDTO is one native entry. An entry the runtime did not describe
// keeps its fields empty rather than inheriting a description.
type InventoryItemDTO struct {
	Name        string `json:"name"`
	Source      string `json:"source,omitempty"`
	Description string `json:"description,omitempty"`
}

// RunDTO is `run --json`: the one final document, emitted after the turn and
// dispose. Unknown numbers stay absent rather than becoming zero.
type RunDTO struct {
	Schema      string        `json:"schema"`
	Command     string        `json:"command"`
	Runtime     string        `json:"runtime"`
	SessionID   string        `json:"sessionId,omitempty"`
	Outcome     string        `json:"outcome,omitempty"`
	Text        string        `json:"text,omitempty"`
	Turn        *TurnDTO      `json:"turn,omitempty"`
	Status      *RunStatusDTO `json:"status,omitempty"`
	Usage       *RunUsageDTO  `json:"usage,omitempty"`
	Context     *ContextDTO   `json:"context,omitempty"`
	Model       string        `json:"model,omitempty"`
	Effort      string        `json:"effort,omitempty"`
	ServiceTier string        `json:"serviceTier,omitempty"`
	RequestID   string        `json:"requestId,omitempty"`
	Seq         uint64        `json:"seq,omitempty"`
	Code        string        `json:"code,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// TurnDTO is the turn's end: the runtime's own report, or oarlock's aborted
// summary when the runtime never reported one inside the abort grace (see
// PromptAndWait). It is never an invented success.
type TurnDTO struct {
	Kind    string `json:"kind"`
	Failure string `json:"failure,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// RunStatusDTO is the folded root-agent status of one run.
type RunStatusDTO struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase,omitempty"`
}

// RunUsageDTO is the root session's token accounting. The optional counts stay
// pointers: nil means "the runtime did not report this number".
type RunUsageDTO struct {
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

// accountUsageDTO mirrors an account read verbatim. An empty AccountUsage
// stays an empty object: the command reports what the vendor said, and a
// missing window is never replaced by a guess.
func accountUsageDTO(usage oarlock.AccountUsage) *AccountUsageDTO {
	dto := &AccountUsageDTO{Plan: usage.Plan}
	if len(usage.Windows) == 0 {
		return dto
	}
	dto.Windows = make([]UsageWindowDTO, 0, len(usage.Windows))
	for _, window := range usage.Windows {
		dto.Windows = append(dto.Windows, UsageWindowDTO{
			Name:     window.Name,
			Used:     window.Used,
			Limit:    window.Limit,
			Unit:     window.Unit,
			ResetsAt: resetTime(window.ResetsAt),
		})
	}
	return dto
}

// resetTime renders a reset instant as RFC3339 — second precision, so a
// sub-second part the vendor reported is dropped rather than rounded. A nil
// instant stays empty rather than becoming "now".
func resetTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.Format(time.RFC3339)
}

// updateInfoDTO mirrors a check result. available:false with versions is the
// normal answer of an up-to-date runtime, not an error.
func updateInfoDTO(info oarlock.UpdateInfo) *UpdateInfoDTO {
	return &UpdateInfoDTO{Available: info.Available, Current: info.Current, Latest: info.Latest}
}

// inventorySectionDTOs keeps the runtime's section order and each section's own
// status, so one failed reader does not hide the sections that answered.
func inventorySectionDTOs(sections []oarlock.InventorySection) []InventorySectionDTO {
	if len(sections) == 0 {
		return nil
	}
	out := make([]InventorySectionDTO, 0, len(sections))
	for _, section := range sections {
		dto := InventorySectionDTO{
			Kind:   string(section.Kind),
			Status: string(section.Status),
			Reason: section.Reason,
		}
		if len(section.Items) != 0 {
			dto.Items = make([]InventoryItemDTO, 0, len(section.Items))
			for _, item := range section.Items {
				dto.Items = append(dto.Items, InventoryItemDTO{
					Name:        item.Name,
					Source:      item.Source,
					Description: item.Description,
				})
			}
		}
		out = append(out, dto)
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
	dto.Status = &RunStatusDTO{Kind: string(status.Value.Kind), Phase: string(status.Value.Phase.Kind)}

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
		dto.Usage = &RunUsageDTO{
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
