package pi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

func TestLivePromptAndWait(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to run PromptAndWait against real pi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Skipf("pi could not be opened here: %v", err)
	}
	defer func() { _ = sess.Dispose(context.Background()) }()
	run, err := oarlock.PromptAndWait(ctx, sess, "Reply with the single word: ok", oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text == "" || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v", run)
	}
	if st := sess.(*Session).Status(); st.Value.Kind != "idle" {
		t.Fatalf("status after the run = %+v, want idle", st)
	}
}
