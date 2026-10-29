package codex

// Effort on a resumed thread (issue #54). thread/start carries effort as a
// `config.model_reasoning_effort` override, but a thread/resume must not: any
// config override on a resume rebuilds the thread's settings from config.toml,
// so the level the thread last ran with would be lost. So a resumed thread is given
// the requested level with thread/settings/update instead, and the open is
// accepted only when codex's own thread/settings/updated report names exactly
// that level. Silence or a different level refuses the open: an effort the
// runtime cannot confirm is not an effort this adapter claims to have set.
//
// Ordering, which the two halves in session.go rely on:
//
//   - the wait is armed before the request is written, so a report codex sends
//     before its reply is not lost;
//   - the request is a nil-hook Call, so its reply is a plain frame;
//   - the report itself is an ordinary notification, recorded by the pump
//     before it reaches the waiter (session.go deliverSettings), so the
//     projection's effort event (project.go settingsViews) is in the stream by
//     the time applyResumeEffort returns.
//
// Live-unverified: the installed codex-cli 0.165.0 schema names
// ThreadSettingsUpdateParams{threadId, effort} and
// ThreadSettingsUpdatedNotification{threadId, threadSettings}, but no logged-in
// codex exists here; the fake app-server is the only executor.

import (
	"context"
	"fmt"
	"time"
)

// SettingsReportTimeout bounds the wait for codex's thread/settings/updated.
const SettingsReportTimeout = 10 * time.Second

// applyResumeEffort sets effort on the loaded thread (thread/settings/update) and refuses
// unless codex reports exactly that level back.
func (s *Session) applyResumeEffort(ctx context.Context, requested string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if requested == "" {
		// No level was asked for on this resume, so there is none to confirm.
		return nil
	}
	// Armed before the request: codex may report the settings change before the
	// reply reaches us.
	s.armSettings()
	if _, err := s.conn.Call(ctx, "thread/settings/update", map[string]any{"threadId": s.thread, "effort": requested}, nil); err != nil {
		s.disarmSettings()
		// An RPCError names codex's own message; a transport failure names the
		// method and what happened to the process.
		return fmt.Errorf("codex: thread/settings/update failed: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, SettingsReportTimeout)
	defer cancel()
	report, ok := s.awaitSettings(wctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("codex: effort %q cannot be confirmed: codex reported no thread/settings/updated within %v", requested, SettingsReportTimeout)
	}
	got, _ := report.obj("threadSettings").str("effort")
	if got == requested {
		return nil
	}
	return fmt.Errorf("codex: thread/settings/updated reports effort %s although %q was requested", reportedEffort(got), requested)
}

// reportedEffort names the level a settings report carries; a report that names
// none is the model's own default rather than a level.
func reportedEffort(effort string) string {
	if effort == "" {
		return "none (the model's default)"
	}
	return fmt.Sprintf("%q", effort)
}
