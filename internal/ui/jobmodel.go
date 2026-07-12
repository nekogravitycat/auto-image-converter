//go:build windows

package ui

import (
	"github.com/tailscale/walk"

	"github.com/nekogravitycat/auto-image-converter/internal/manager"
)

// jobModel adapts the manager's job list to a walk TableView.
type jobModel struct {
	walk.TableModelBase
	jobs []manager.JobState
}

// RowCount returns the number of jobs.
func (m *jobModel) RowCount() int { return len(m.jobs) }

// Value returns the cell text for a job row.
func (m *jobModel) Value(row, col int) interface{} {
	j := m.jobs[row]
	switch col {
	case 0:
		return j.Cfg.Name
	case 1:
		return j.Cfg.WatchDirectory
	case 2:
		return j.Cfg.TargetFormat
	case 3:
		return j.Cfg.Quality
	case 4:
		return modeLabel(j.Cfg.Mode)
	case 5:
		return postActionLabel(j.Cfg.PostAction)
	case 6:
		return j.Status
	}
	return nil
}

// jobAt returns the job at the given row, or false when the row is out of range.
func (m *jobModel) jobAt(row int) (manager.JobState, bool) {
	if row < 0 || row >= len(m.jobs) {
		return manager.JobState{}, false
	}
	return m.jobs[row], true
}

// setJobs replaces the model's data and refreshes the view.
func (m *jobModel) setJobs(jobs []manager.JobState) {
	m.jobs = jobs
	m.PublishRowsReset()
}
