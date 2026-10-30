package cli

import (
	"fmt"
	"io"
	"sync"
	"text/tabwriter"

	"github.com/zzjcool/oarlock"
)

// The human views are deliberately plain text: one table per command, then a
// short outcome block for run. Scripts should use --json.

func renderList(w io.Writer, runtimes []RuntimeDTO) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tSESSION\tPROBE\tMODELS\tINSTALL\tLOGIN\tUSAGE\tUPGRADE\tINVENTORY")
	for _, rt := range runtimes {
		c := rt.Capabilities
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rt.ID, yesNo(c.Session), yesNo(c.Probe), yesNo(c.Models),
			yesNo(c.Install), yesNo(c.Login), yesNo(c.Usage), yesNo(c.Upgrade), yesNo(c.Inventory))
	}
	_ = tw.Flush()
}

func renderProbe(w io.Writer, results []ProbeResultDTO) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tAVAILABLE\tVERSION\tPATH\tSOURCE\tERROR")
	for _, r := range results {
		if r.Installation == nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Runtime, "-", "-", "-", "-", r.Error)
			continue
		}
		inst := r.Installation
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Runtime, yesNo(inst.Available), orDash(inst.Version), orDash(inst.Path), orDash(inst.Source), orDash(r.Error))
	}
	_ = tw.Flush()
}

func renderModels(w io.Writer, results []ModelsResultDTO) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tMODEL\tNAME\tEFFORT\tSERVICE TIERS")
	for _, r := range results {
		switch {
		case r.Error != "":
			fmt.Fprintf(tw, "%s\t(error: %s)\n", r.Runtime, r.Error)
		case r.Unsupported != "":
			fmt.Fprintf(tw, "%s\t(unsupported: %s)\n", r.Runtime, r.Unsupported)
		case len(r.Models) == 0:
			fmt.Fprintf(tw, "%s\t(no models)\n", r.Runtime)
		default:
			for _, m := range r.Models {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					r.Runtime, orDash(m.ID), orDash(m.Name), joinOrDash(m.EffortLevels), joinOrDash(m.ServiceTiers))
			}
		}
	}
	_ = tw.Flush()
}

// progress renders Session.Events as a readable transcript. Every method takes
// the same mutex so a live callback and the final summary cannot interleave
// mid-line.
type progress struct {
	mu        sync.Mutex
	w         io.Writer
	tools     map[string]string
	lineStart bool
}

func newProgress(w io.Writer) *progress {
	return &progress{w: w, tools: map[string]string{}, lineStart: true}
}

// record is the Session.Events callback. It never blocks the run for long: it
// writes one chunk to stdout under a mutex.
func (p *progress) record(rec oarlock.Record) {
	if len(rec.AgentPath) != 0 {
		return // a child agent's text is not the root run's transcript
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ev := range rec.Events {
		switch ev.Kind {
		case oarlock.EventTextDelta:
			p.text(ev.Text)
		case oarlock.EventReasoning:
			if ev.Redacted {
				p.line("[thinking] (redacted)")
			} else {
				p.line("[thinking] " + ev.Text)
			}
		case oarlock.EventToolCallStarted:
			p.tools[ev.CallID] = ev.Tool
			p.line(fmt.Sprintf("[tool] %s started", orDash(ev.Tool)))
		case oarlock.EventToolCallEnded:
			name := p.tools[ev.CallID]
			if name == "" {
				name = ev.CallID
			}
			result := string(ev.Result)
			if result == "" {
				result = "ended"
			}
			p.line(fmt.Sprintf("[tool] %s %s", orDash(name), result))
		case oarlock.EventTurnEnded:
			if ev.Outcome != nil {
				p.line("[turn] " + turnLine(ev.Outcome))
			}
		case oarlock.EventCompactionStarted:
			p.line(fmt.Sprintf("[compaction] started (%s)", orDash(ev.Trigger)))
		case oarlock.EventCompactionEnded:
			p.line("[compaction] " + orDash(string(ev.CompactionResult)))
		case oarlock.EventRetry:
			p.line(fmt.Sprintf("[retry] attempt %d/%d%s", ev.Attempt, ev.MaxAttempts, reasonSuffix(ev.Reason)))
		}
	}
}

func (p *progress) text(s string) {
	if s == "" {
		return
	}
	if p.lineStart {
		fmt.Fprint(p.w, "[text] ")
		p.lineStart = false
	}
	fmt.Fprint(p.w, s)
}

func (p *progress) line(s string) {
	if !p.lineStart {
		fmt.Fprint(p.w, "\n")
	}
	fmt.Fprintln(p.w, s)
	p.lineStart = true
}

// summary prints the final block under the same lock as the live progress.
func (p *progress) summary(dto RunDTO) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lineStart {
		fmt.Fprint(p.w, "\n")
		p.lineStart = true
	}
	fmt.Fprintf(p.w, "outcome: %s\n", dto.Outcome)
	if dto.Turn != nil {
		fmt.Fprintf(p.w, "turn: %s\n", turnDTOLine(dto.Turn))
	}
	if dto.Code != "" {
		fmt.Fprintf(p.w, "rejected: %s%s\n", dto.Code, reasonSuffix(dto.Reason))
	}
	if dto.Text != "" {
		fmt.Fprintf(p.w, "text: %s\n", dto.Text)
	}
	if dto.SessionID != "" {
		fmt.Fprintf(p.w, "session: %s\n", dto.SessionID)
	}
	if dto.Model != "" {
		fmt.Fprintf(p.w, "model: %s\n", dto.Model)
	}
	if dto.Effort != "" {
		fmt.Fprintf(p.w, "effort: %s\n", dto.Effort)
	}
	if dto.Usage != nil {
		fmt.Fprintf(p.w, "usage: input=%d output=%d\n", dto.Usage.Input, dto.Usage.Output)
	}
	if dto.Status != nil {
		fmt.Fprintf(p.w, "status: %s\n", dto.Status.Kind)
	}
	if dto.Error != "" {
		fmt.Fprintf(p.w, "error: %s\n", dto.Error)
	}
}

func turnLine(t *oarlock.TurnOutcome) string {
	line := string(t.Kind)
	if t.Failure != "" {
		line += " (" + string(t.Failure) + ")"
	}
	return line + reasonSuffix(t.Reason)
}

func turnDTOLine(t *TurnDTO) string {
	line := t.Kind
	if t.Failure != "" {
		line += " (" + t.Failure + ")"
	}
	return line + reasonSuffix(t.Reason)
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
