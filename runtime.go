package oarlock

import (
	"context"
	"time"
)

// Session is the core capability. A runtime that cannot open a session is not
// usable. Optional whole operations are separate interfaces (Steerer,
// Withdrawer, ContextBreakdownReader); their presence is the capability.
//
// A control returns (outcome, error): outcome carries accepted/rejected and is
// the normal answer; error is reserved for operational failures (a broken
// transport, a cancelled context). Final states and landing points are always
// events, never return values.
type Session interface {
	Prompt(ctx context.Context, input string, opt PromptOptions) (ControlOutcome, error)
	Queue(ctx context.Context, input string, opt InputOptions) (ControlOutcome, error)
	Abort(ctx context.Context) (ControlOutcome, error)
	Dispose(ctx context.Context) error

	// Records returns the retained records after the cursor, then (for a live
	// cursor) the stream continues through Events. A zero cursor replays the
	// whole instance. See docs/spec/record-stream.md.
	Records(cursor Cursor) []Record
	// Events replays retained records after the cursor and then delivers live
	// records until the returned unsubscribe function is called. The callback
	// is a side tap: it is never allowed to block or fail the run.
	Events(cursor *Cursor, fn func(Record)) (unsubscribe func())

	// Capabilities declares the per-session facts a host must know before
	// acting. A whole operation is not declared here; it is an interface.
	Capabilities() Capabilities
}

// Steerer is implemented only by sessions that can add input to the active
// turn. Absence is the capability; there is no SupportsSteer flag.
type Steerer interface {
	Steer(ctx context.Context, input string, opt InputOptions) (ControlOutcome, error)
}

// Withdrawer is implemented only by sessions that hold queued input themselves
// and can take it back before it is sent.
type Withdrawer interface {
	Withdraw(ctx context.Context, inputID string) (ControlOutcome, error)
}

// ContextBreakdownReader is implemented only by sessions whose runtime can
// itemize what fills its context window. The answer is read now, not recorded.
type ContextBreakdownReader interface {
	ContextBreakdown(ctx context.Context) (*ContextBreakdown, error)
}

// ControlKind is the two answers a toRuntime control can get.
type ControlKind string

const (
	ControlAccepted ControlKind = "accepted"
	ControlRejected ControlKind = "rejected"
)

// ControlOutcome is the read face of the two records a control produced.
// Kind is accepted or rejected. Code and Reason are set only on a rejection;
// Seq is the request's position in the stream.
type ControlOutcome struct {
	Kind      ControlKind
	Seq       uint64
	RequestID string
	Code      Code
	Reason    string
	Request   Record
	Response  Record
}

// InputOptions describe one input. Empty text is refused unless at least one
// image is present. Images are native inputs, not paths in prose.
type InputOptions struct {
	InputID string
	Images  []InputImage
	Origin  string
}

// PromptOptions add the prompt-only ability to declare earlier logical inputs
// delivered by this request.
type PromptOptions struct {
	InputOptions
	Carries []string
}

// ContextBreakdown is a runtime's own itemization of its context window.
type ContextBreakdown struct {
	Categories []ContextCategory
}

// ContextCategory is one native category in a context breakdown.
type ContextCategory struct {
	Name   string
	Tokens int64
}

// AttributionTier declares how much sub-agent attribution a runtime exposes.
// It is a declaration because a host must decide before acting whether child
// usage can be attributed at all.
type AttributionTier string

const (
	// AttributionNone: the runtime has no sub-agents.
	AttributionNone AttributionTier = "none"
	// AttributionOpaque: sub-agents exist but the selected interface shows
	// only the root.
	AttributionOpaque AttributionTier = "opaque"
	// AttributionAttributed: child records self-attribute via AgentPath.
	AttributionAttributed AttributionTier = "attributed"
	// AttributionNested: children are sessions of their own, linked in the
	// session graph.
	AttributionNested AttributionTier = "nested"
)

// QueueCapability says where input held for a later turn lives.
type QueueCapability struct {
	// Durable is true when the runtime persists held input across a process
	// restart. Every runtime can at least hold input in the adapter.
	Durable bool
}

// Capabilities is the runtime-time declaration for facts a host must know
// before acting. It deliberately contains no flag for an operation: steer,
// withdraw and context breakdown are interfaces.
type Capabilities struct {
	Queue       QueueCapability
	Attribution AttributionTier
	// Images is true when prompt/steer/queue inputs may carry images.
	Images bool
}

// Installation is the result of a read-only probe.
type Installation struct {
	Available bool
	Version   string
	Path      string
	// Source names where the probe found it: "bundled", "path", "config".
	Source string
}

// InstallationProbe answers whether the runtime is installed without account
// or network I/O.
type InstallationProbe interface {
	Installation(ctx context.Context) (Installation, error)
}

// Model is one selectable model as the runtime reports it.
type Model struct {
	Provider     string
	ID           string
	Name         string
	EffortLevels []string
	ServiceTiers []string
}

// ModelLister lists what the runtime can run now. An empty list with no error
// is an honest "none"; "not signed in" must be a typed error, never silence.
type ModelLister interface {
	ListModels(ctx context.Context) ([]Model, error)
}

// InstallRequest selects a release line where the runtime ships more than one.
type InstallRequest struct {
	Line string
}

// InstallPlan is read-only: the command that would run and why.
type InstallPlan struct {
	Command        []string
	ChangesMachine bool
	Reason         string
}

// InstallResult reports what the native installer did.
type InstallResult struct {
	Installation Installation
}

// Installer is present only where an installer exists. It changes the machine
// and is called explicitly, never as part of a probe.
type Installer interface {
	InstallPlan(ctx context.Context, req InstallRequest) (InstallPlan, error)
	Install(ctx context.Context, req InstallRequest) (InstallResult, error)
}

// LoginChallenge is the non-interactive part of a vendor login flow. A caller
// can print URL/Code without waiting on stdin.
type LoginChallenge struct {
	Kind         string // "url", "device_code" or "none"
	URL          string
	Code         string
	Instructions string
}

// AuthStatus is the vendor's answer to "is this machine signed in?".
type AuthStatus struct {
	Authenticated bool
	Account       string
	Detail        string
}

// LoginProvider is present only where a native login/logout exists. Tokens
// never pass through oarlock.
type LoginProvider interface {
	Login(ctx context.Context) (LoginChallenge, error)
	Logout(ctx context.Context) error
	AuthStatus(ctx context.Context) (AuthStatus, error)
}

// UsageWindow is one quota window a runtime's account reports.
type UsageWindow struct {
	Name     string
	Used     float64
	Limit    float64
	Unit     string
	ResetsAt *time.Time
}

// AccountUsage is the account-level usage read, not session tokens.
type AccountUsage struct {
	Plan    string
	Windows []UsageWindow
}

// AccountUsageReader is present only where the vendor exposes account usage.
type AccountUsageReader interface {
	AccountUsage(ctx context.Context) (AccountUsage, error)
}

// UpdateInfo is a read-only update check.
type UpdateInfo struct {
	Available bool
	Current   string
	Latest    string
}

// Upgrader is present only where the vendor ships its own updater.
type Upgrader interface {
	CheckUpdate(ctx context.Context) (UpdateInfo, error)
	Upgrade(ctx context.Context) error
}

// InventoryKind names one inventory section.
type InventoryKind string

const (
	InventoryModels   InventoryKind = "models"
	InventorySkills   InventoryKind = "skills"
	InventoryCommands InventoryKind = "commands"
	InventoryTools    InventoryKind = "tools"
)

// InventoryStatus is per-section and preserves partial success.
type InventoryStatus string

const (
	InventoryOK          InventoryStatus = "ok"
	InventoryUnsupported InventoryStatus = "unsupported"
	InventoryError       InventoryStatus = "error"
)

// InventoryItem is one entry from a native inventory.
type InventoryItem struct {
	Name        string
	Source      string
	Description string
}

// InventorySection carries its own status, so one failed reader does not erase
// the others.
type InventorySection struct {
	Kind   InventoryKind
	Status InventoryStatus
	Reason string
	Items  []InventoryItem
}

// Inventory is the four native inventories, each with its own status.
type Inventory struct {
	Sections []InventorySection
}

// InventoryReader is present only where at least one inventory is readable.
type InventoryReader interface {
	Inventory(ctx context.Context) (Inventory, error)
}

// StartSession opens one session. It refuses unsupported options before any
// process starts, using the same table Runtime.Refused declares.
type StartSession func(ctx context.Context, inst Installation, opt Options) (Session, error)

// Runtime is one adoption unit. Session is required; every other operation is
// an optional field whose nil value means "not implemented here". Nil is the
// presence check: there is no SupportsInstall bool beside it.
type Runtime struct {
	ID      string
	Session StartSession
	// Refused is the same table the open path checks, so the declaration and
	// the behaviour cannot drift.
	Refused RefusedOptions

	Probe     InstallationProbe
	Models    ModelLister
	Install   Installer
	Login     LoginProvider
	Usage     AccountUsageReader
	Upgrade   Upgrader
	Inventory InventoryReader
}

// CheckOptions refuses every declared-and-present option before anything
// starts. A declared option that is not present in opt is not an error.
func (r Runtime) CheckOptions(opt Options) error {
	return r.Refused.Check(opt)
}

// Installation is a convenience read: a runtime without a probe is not
// verified as installed, and the zero value says so. It never performs
// account or network I/O.
func (r Runtime) Installation(ctx context.Context) (Installation, error) {
	if r.Probe == nil {
		return Installation{}, nil
	}
	return r.Probe.Installation(ctx)
}
