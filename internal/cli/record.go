package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/voyage"
)

// checkRecordPath refuses an existing --record target before a session opens.
// voyage.Create still uses O_EXCL as the authoritative guard; this check makes
// the common case fail early (and, unlike a race, without starting a runtime).
func checkRecordPath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("record file already exists: %s (refusing to overwrite)", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking record path %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("record directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("record directory %s is not a directory", dir)
	}
	return nil
}

// writeRecord writes header + every record + end with internal/voyage.Create.
// Records are the session's whole retained stream, dispose included, and are
// already in dense Seq order; voyage.Append refuses anything else, and End
// refuses to mark a log complete when a record was lost.
//
// The header names what was requested (model/effort/serviceTier), not what the
// runtime later reported; the stream itself carries the reported values.
func (inv *runInvocation) writeRecord(records []oarlock.Record, sessionID, reason string) error {
	header := voyage.Header{
		Runtime:     inv.runtimeID,
		Model:       inv.model,
		Effort:      inv.effort,
		ServiceTier: inv.serviceTier,
		Cwd:         inv.cwd,
		SessionID:   sessionID,
		StartedAt:   inv.startedAt.UnixMilli(),
		Recorder:    "oarlock-cli/" + version(),
	}
	w, err := voyage.Create(inv.recordPath, header, nil)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if err := w.Append(rec); err != nil {
			_ = w.End("error")
			return err
		}
	}
	return w.End(reason)
}
