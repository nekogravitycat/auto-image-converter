package convert

import "testing"

// TestKillOnCloseJob exercises the job-object lifecycle: creation applies the
// kill-on-close limit without error, and close is idempotent.
func TestKillOnCloseJob(t *testing.T) {
	j, err := newKillOnCloseJob()
	if err != nil {
		t.Fatalf("newKillOnCloseJob: %v", err)
	}
	j.close()
	j.close() // must be safe to call more than once
}
