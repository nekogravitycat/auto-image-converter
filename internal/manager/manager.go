// Package manager coordinates the whole running application: it owns the shared
// conversion Engine, the set of monitored-folder jobs, statistics, and the
// lifecycle of watchers and batch conversions. The UI drives it through a small,
// goroutine-safe API; the manager pushes refresh and notification signals back
// to the UI through callbacks.
//
// Concurrency model:
//   - m.mu guards the configuration (app settings, the job list and their
//     statuses), the callbacks, and the current engine handle.
//   - m.reconcileMu serializes reconciliation and exclusively owns the set of
//     running watchers, so starting/stopping watchers (which can block briefly
//     on a drain) never happens on the UI thread and never overlaps itself.
package manager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/autostart"
	"github.com/nekogravitycat/auto-image-converter/internal/batch"
	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/humanize"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
	"github.com/nekogravitycat/auto-image-converter/internal/stats"
	"github.com/nekogravitycat/auto-image-converter/internal/watch"
)

// JobState is a snapshot of one job's configuration and current runtime status,
// returned to the UI for display.
type JobState struct {
	ID     int
	Cfg    config.JobConfig
	Status string
}

// runner tracks a single running watcher so it can be stopped.
type runner struct {
	spec   convert.JobSpec
	cancel context.CancelFunc
	done   chan struct{}
}

// engineHandle owns one convert.Engine and reference-counts its users, so an
// engine is only closed once nothing is still converting through it.
//
// This matters because the engine is rebuilt whenever the worker count changes,
// while background batches hold on to whichever engine they started with. Closing
// an engine out from under a running batch would shut down its HEIF workers
// mid-conversion and fail every remaining file. Instead the old engine is
// retired: new work goes to the replacement, and the old one closes itself once
// its last user is done.
type engineHandle struct {
	eng *convert.Engine

	mu      sync.Mutex
	users   int
	retired bool
}

// use registers a user of the engine. The caller must invoke done when finished.
// Callers take a handle and call use while holding the manager's lock, so a
// concurrent retire can never slip in between the two.
func (h *engineHandle) use() *convert.Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users++
	return h.eng
}

// done releases a user, closing the engine if it was retired and this was the
// last one.
func (h *engineHandle) done() {
	h.mu.Lock()
	h.users--
	closeNow := h.retired && h.users == 0
	h.mu.Unlock()
	if closeNow {
		h.eng.Close()
	}
}

// retire marks the engine as superseded: it is closed as soon as its last user
// finishes, or immediately if it has none.
func (h *engineHandle) retire() {
	h.mu.Lock()
	if h.retired {
		h.mu.Unlock()
		return
	}
	h.retired = true
	closeNow := h.users == 0
	h.mu.Unlock()
	if closeNow {
		h.eng.Close()
	}
}

// Manager is the application coordinator. Construct it with New.
type Manager struct {
	mu          sync.Mutex
	reconcileMu sync.Mutex

	cfgPath        string
	statsPath      string
	heifScriptPath string
	exePath        string

	app    config.AppConfig
	jobs   []*JobState
	nextID int
	paused bool
	closed bool

	log   *logx.Logger
	stats *stats.Stats

	eng         *engineHandle
	engWorkers  int
	heifChecked bool // the HEIF environment self-test runs at most once per run

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup // background batches and watchers

	running map[int]*runner // reconcile-owned

	onChange func()
	notify   func(title, body string, isError bool)
}

// New creates a Manager from an already-loaded configuration. The engine is
// built to match the configuration; call Start to begin converting and watching.
func New(cfg config.Config, cfgPath, statsPath, heifScriptPath, exePath string, log *logx.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfgPath:        cfgPath,
		statsPath:      statsPath,
		heifScriptPath: heifScriptPath,
		exePath:        exePath,
		app:            cfg.App,
		log:            log,
		stats:          stats.Load(statsPath),
		rootCtx:        ctx,
		rootCancel:     cancel,
		running:        make(map[int]*runner),
	}
	for _, jc := range cfg.Jobs {
		m.jobs = append(m.jobs, &JobState{ID: m.nextID, Cfg: jc, Status: "idle"})
		m.nextID++
	}
	m.buildEngine(cfg.App.MaxWorkers)
	return m
}

// SetCallbacks registers the UI refresh and notification callbacks. onChange is
// invoked (from background goroutines) whenever job state changes so the UI can
// refresh; notify posts a user-facing balloon. Both may be nil.
func (m *Manager) SetCallbacks(onChange func(), notify func(title, body string, isError bool)) {
	m.mu.Lock()
	m.onChange = onChange
	m.notify = notify
	m.mu.Unlock()
}

// --- Read-only accessors for the UI ---

// App returns the current global settings.
func (m *Manager) App() config.AppConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.app
}

// Jobs returns a snapshot of all jobs and their statuses.
func (m *Manager) Jobs() []JobState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JobState, len(m.jobs))
	for i, j := range m.jobs {
		out[i] = *j
	}
	return out
}

// Paused reports whether all monitoring is currently paused.
func (m *Manager) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused
}

// Stats returns the current session and lifetime statistics.
func (m *Manager) Stats() (session, lifetime stats.Snapshot) {
	return m.stats.Session(), m.stats.Lifetime()
}

// --- Lifecycle ---

// Start sweeps orphaned temp files, runs any startup/one-time batches, and
// begins watching the enabled monitor jobs. It returns promptly; the work runs
// in the background.
func (m *Manager) Start() {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}

	for _, j := range m.Jobs() {
		spec := convert.SpecFromJob(j.Cfg)
		batch.SweepTemps(spec, m.log)
		if !j.Cfg.Enabled || !dirExists(j.Cfg.WatchDirectory) {
			continue
		}
		if j.Cfg.Mode == config.ModeOnce || j.Cfg.BatchOnStartup {
			m.runBatch(spec, nil, j.Cfg.Name, true)
		}
	}
	m.startStatsSaver()
	m.triggerReconcile()
}

// statsSaveInterval is how often lifetime totals are flushed while the app runs.
// Without it, files converted by a watcher would only be persisted when a batch
// happens to finish or at a graceful shutdown — so a force-kill would lose them.
const statsSaveInterval = 30 * time.Second

// startStatsSaver periodically persists the lifetime statistics until shutdown.
func (m *Manager) startStatsSaver() {
	// Join the wait group under the lock that Shutdown uses to set closed, so the
	// counter is never raised after Shutdown has begun waiting on it.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		t := time.NewTicker(statsSaveInterval)
		defer t.Stop()
		for {
			select {
			case <-m.rootCtx.Done():
				return
			case <-t.C:
				if err := m.stats.Save(); err != nil {
					m.log.Warnf("could not save stats: %v", err)
				}
			}
		}
	}()
}

// Shutdown cancels all work, waits for in-flight conversions to drain (bounded
// by the watch/batch drain timeouts), closes the engine, and persists stats. It
// is safe to call once; further calls are no-ops.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.rootCancel
	m.mu.Unlock()

	m.log.Infof("shutdown requested; draining in-flight conversions")
	cancel()

	m.reconcileMu.Lock()
	m.stopAllRunners()
	m.reconcileMu.Unlock()

	m.wg.Wait()

	m.mu.Lock()
	h := m.eng
	m.mu.Unlock()
	if h != nil {
		// Every batch and watcher has finished, so this closes the engine (and its
		// HEIF workers) immediately.
		h.retire()
	}
	if err := m.stats.Save(); err != nil {
		m.log.Warnf("could not save stats: %v", err)
	}
	m.log.Infof("shutdown complete")
}

// --- Mutations (called from the UI thread) ---

// AddJob validates and appends a new job, persists the config, and starts its
// watcher and/or startup batch as appropriate. It returns the new job's ID.
func (m *Manager) AddJob(jc config.JobConfig) int {
	jc, _ = config.NormalizeJob(jc)
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.jobs = append(m.jobs, &JobState{ID: id, Cfg: jc, Status: "idle"})
	m.persistLocked()
	m.mu.Unlock()

	if jc.Enabled && dirExists(jc.WatchDirectory) && (jc.Mode == config.ModeOnce || jc.BatchOnStartup) {
		m.runBatch(convert.SpecFromJob(jc), nil, jc.Name, true)
	}
	m.triggerReconcile()
	return id
}

// UpdateJob replaces the settings of the job with the given ID, persists, and
// reconciles so the change (format, folder, mode, etc.) takes effect.
func (m *Manager) UpdateJob(id int, jc config.JobConfig) {
	jc, _ = config.NormalizeJob(jc)
	m.mu.Lock()
	if j := m.findLocked(id); j != nil {
		j.Cfg = jc
		m.persistLocked()
	}
	m.mu.Unlock()
	m.triggerReconcile()
}

// RemoveJob deletes the job with the given ID.
func (m *Manager) RemoveJob(id int) {
	m.mu.Lock()
	for i, j := range m.jobs {
		if j.ID == id {
			m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
			break
		}
	}
	m.persistLocked()
	m.mu.Unlock()
	m.triggerReconcile()
}

// SetJobEnabled enables or disables a job in place.
func (m *Manager) SetJobEnabled(id int, enabled bool) {
	m.mu.Lock()
	if j := m.findLocked(id); j != nil {
		j.Cfg.Enabled = enabled
		m.persistLocked()
	}
	m.mu.Unlock()
	m.triggerReconcile()
}

// SetMaxWorkers changes the shared worker-pool size, rebuilding the engine. An
// unchanged value is ignored, so a UI that reports every keystroke does not
// rebuild the engine (and restart the HEIF workers) for nothing.
func (m *Manager) SetMaxWorkers(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	if m.app.MaxWorkers == n {
		m.mu.Unlock()
		return
	}
	m.app.MaxWorkers = n
	m.persistLocked()
	m.mu.Unlock()
	m.triggerReconcile()
}

// SetStartMinimized records whether the app should start hidden to the tray.
func (m *Manager) SetStartMinimized(v bool) {
	m.mu.Lock()
	m.app.StartMinimized = v
	m.persistLocked()
	m.mu.Unlock()
}

// SetAutostart toggles launch-at-login (updating the registry) and records the
// choice. It returns any error from updating the registry.
func (m *Manager) SetAutostart(enabled bool) error {
	if err := autostart.Set(enabled, m.exePath); err != nil {
		m.log.Errorf("could not update autostart: %v", err)
		return err
	}
	m.mu.Lock()
	m.app.Autostart = enabled
	m.persistLocked()
	m.mu.Unlock()
	return nil
}

// PauseAll stops all watchers without changing the enabled flags; ResumeAll
// restarts them.
func (m *Manager) PauseAll() { m.setPaused(true) }

// ResumeAll resumes monitoring after a PauseAll.
func (m *Manager) ResumeAll() { m.setPaused(false) }

func (m *Manager) setPaused(v bool) {
	m.mu.Lock()
	m.paused = v
	m.mu.Unlock()
	m.triggerReconcile()
}

// ConvertNow runs a one-off batch over the job's folder using its settings.
func (m *Manager) ConvertNow(id int) {
	m.mu.Lock()
	j := m.findLocked(id)
	if j == nil {
		m.mu.Unlock()
		return
	}
	cfg := j.Cfg
	m.mu.Unlock()

	if !dirExists(cfg.WatchDirectory) {
		m.postNotify("Cannot convert", cfg.Name+": folder not found", true)
		return
	}
	m.runBatch(convert.SpecFromJob(cfg), nil, cfg.Name, true)
}

// ConvertAllNow runs a one-off batch over every enabled job's folder.
func (m *Manager) ConvertAllNow() {
	for _, j := range m.Jobs() {
		if j.Cfg.Enabled && dirExists(j.Cfg.WatchDirectory) {
			m.runBatch(convert.SpecFromJob(j.Cfg), nil, j.Cfg.Name, true)
		}
	}
}

// ConvertOnce converts an explicit set of files/folders once with the given
// spec (used for drag-and-drop). Folders in files are expanded by the batch
// scan when the spec's watch dir points at them; individual files are converted
// directly.
func (m *Manager) ConvertOnce(files []string, spec convert.JobSpec, label string) {
	m.runBatch(spec, files, label, true)
}

// --- Internal helpers ---

// runBatch launches a batch conversion in the background, tracked so shutdown
// drains it. When files is nil the job's whole folder is scanned; otherwise the
// explicit files are converted.
func (m *Manager) runBatch(spec convert.JobSpec, files []string, label string, notifyDone bool) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	ctx := m.rootCtx
	// Claim the engine under the lock: a reconcile that swaps in a replacement
	// can then only retire this one, never close it while the batch is running.
	h := m.eng
	eng := h.use()
	m.wg.Add(1)
	m.mu.Unlock()

	if spec.TargetFormat == config.FormatHEIF {
		go m.checkHEIFEnvironment()
	}

	go func() {
		// Defers run last-in-first-out: release the engine before the wait group,
		// so Shutdown's Wait returning means every engine user is gone too.
		defer m.wg.Done()
		defer h.done()

		var sum batch.Summary
		if files == nil {
			sum = batch.Run(ctx, spec, eng, m.log)
		} else {
			sum = batch.RunFiles(ctx, spec, eng, m.log, files)
		}
		_ = m.stats.Save()
		if notifyDone {
			m.notifyBatch(label, sum)
		}
		m.fireChange()
	}()
}

// triggerReconcile runs reconcile on a background goroutine so the UI thread is
// never blocked by a watcher drain.
func (m *Manager) triggerReconcile() { go m.reconcile() }

// reconcile brings the set of running watchers in line with the configuration:
// it rebuilds the engine when the worker count or HEIF usage changed, then
// starts/stops watchers so exactly the enabled monitor jobs (when not paused)
// are being watched.
func (m *Manager) reconcile() {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	desiredWorkers := m.app.MaxWorkers
	desiredHEIF := m.anyHEIFLocked()
	paused := m.paused
	snap := make([]JobState, len(m.jobs))
	for i, j := range m.jobs {
		snap[i] = *j
	}
	h := m.eng
	needRebuild := desiredWorkers != m.engWorkers
	m.mu.Unlock()

	if needRebuild {
		m.stopAllRunners()
		h = m.buildEngine(desiredWorkers)
	}
	eng := h.eng

	if desiredHEIF {
		go m.checkHEIFEnvironment()
	}

	// Desired running set: enabled monitor jobs with an existing folder, unless
	// paused. A job whose folder is gone is recorded as failed so the status set
	// here survives the refresh below.
	desired := make(map[int]convert.JobSpec)
	failed := make(map[int]string)
	for i := range snap {
		j := snap[i]
		if paused || !j.Cfg.Enabled || j.Cfg.Mode != config.ModeMonitor {
			continue
		}
		if !dirExists(j.Cfg.WatchDirectory) {
			failed[j.ID] = "error: watch folder not found"
			continue
		}
		desired[j.ID] = convert.SpecFromJob(j.Cfg)
	}

	// Stop runners no longer desired or whose spec changed.
	for id, r := range m.running {
		if want, ok := desired[id]; !ok || want != r.spec {
			r.cancel()
			<-r.done
			delete(m.running, id)
		}
	}
	// Start desired runners not yet running.
	for id, spec := range desired {
		if _, ok := m.running[id]; !ok {
			m.running[id] = m.startRunner(eng, spec, id)
		}
	}

	// Refresh the status of every job in one pass: watching, failed, or one of
	// the not-watching states.
	m.mu.Lock()
	for _, j := range m.jobs {
		switch {
		case m.running[j.ID] != nil:
			j.Status = "monitoring"
		case failed[j.ID] != "":
			j.Status = failed[j.ID]
		default:
			j.Status = statusFor(j.Cfg, m.paused)
		}
	}
	m.mu.Unlock()

	m.fireChange()
}

// startRunner launches a watcher for spec against the given engine and returns a
// handle to stop it. Called only from the reconcile goroutine.
func (m *Manager) startRunner(eng *convert.Engine, spec convert.JobSpec, id int) *runner {
	ctx, cancel := context.WithCancel(m.rootCtx)
	done := make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		if err := watch.Run(ctx, spec, eng, m.log); err != nil {
			m.log.Errorf("[%s] watcher stopped with error: %v", spec.Name, err)
			m.setStatus(id, "error: "+err.Error())
		}
	}()
	return &runner{spec: spec, cancel: cancel, done: done}
}

// stopAllRunners cancels every running watcher and waits for it to drain. Called
// only while holding reconcileMu.
func (m *Manager) stopAllRunners() {
	for id, r := range m.running {
		r.cancel()
		<-r.done
		delete(m.running, id)
	}
}

// buildEngine builds an engine sized to workers, installs it as the current one,
// and retires the previous one (which closes as soon as any batch still using it
// finishes). It records the worker count so reconcile can detect future changes.
func (m *Manager) buildEngine(workers int) *engineHandle {
	eng := convert.NewEngine(workers, m.log, m.heifScriptPath)
	eng.SetResultHook(m.recordResult)
	h := &engineHandle{eng: eng}

	m.mu.Lock()
	old := m.eng
	m.eng = h
	m.engWorkers = workers
	m.mu.Unlock()

	if old != nil {
		old.retire()
	}
	return h
}

// checkHEIFEnvironment runs the HEIF self-test once per run, the first time
// anything targets HEIF, so a missing Python or pillow-heif is reported clearly
// rather than as one failure per screenshot. It is slow (it starts an
// interpreter), so it runs off the reconcile path.
func (m *Manager) checkHEIFEnvironment() {
	m.mu.Lock()
	if m.heifChecked || m.closed {
		m.mu.Unlock()
		return
	}
	m.heifChecked = true
	h := m.eng
	m.mu.Unlock()

	if err := h.eng.ValidateHEIF(); err != nil {
		m.log.Errorf("%v", err)
		m.log.Errorf("HEIF conversions will fail until Python and pillow-heif are installed (pip install pillow-heif); originals will be kept")
		return
	}
	// The environment is good: pay the interpreter startup cost now rather than
	// on the first screenshot.
	h.eng.WarmHEIF()
}

// recordResult feeds every conversion outcome into the statistics.
func (m *Manager) recordResult(res convert.Result, err error) {
	if err != nil {
		m.stats.RecordFailed()
		return
	}
	m.stats.RecordConverted(res.OriginalBytes - res.OutputBytes)
}

func (m *Manager) anyHEIFLocked() bool {
	for _, j := range m.jobs {
		if convert.SpecFromJob(j.Cfg).TargetFormat == config.FormatHEIF {
			return true
		}
	}
	return false
}

func (m *Manager) findLocked(id int) *JobState {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (m *Manager) setStatus(id int, status string) {
	m.mu.Lock()
	if j := m.findLocked(id); j != nil {
		j.Status = status
	}
	m.mu.Unlock()
	m.fireChange()
}

// persistLocked writes the current configuration to disk. The caller holds m.mu.
func (m *Manager) persistLocked() {
	jobs := make([]config.JobConfig, len(m.jobs))
	for i, j := range m.jobs {
		jobs[i] = j.Cfg
	}
	cfg := config.Config{Version: config.CurrentVersion, App: m.app, Jobs: jobs}
	if err := config.Save(m.cfgPath, cfg); err != nil {
		m.log.Errorf("could not save config to %s: %v", m.cfgPath, err)
	}
}

func (m *Manager) fireChange() {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *Manager) postNotify(title, body string, isError bool) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(title, body, isError)
	}
}

func (m *Manager) notifyBatch(label string, s batch.Summary) {
	if s.Converted == 0 && s.Failed == 0 {
		return
	}
	if s.Failed > 0 {
		m.postNotify("Conversion finished with errors",
			fmt.Sprintf("%s: %d converted, %d failed, %s saved", label, s.Converted, s.Failed, humanize.Bytes(s.BytesSaved)), true)
	} else {
		m.postNotify("Conversion complete",
			fmt.Sprintf("%s: %d converted, %s saved", label, s.Converted, humanize.Bytes(s.BytesSaved)), false)
	}
}

// statusFor returns the display status for a job that is not currently watching.
func statusFor(j config.JobConfig, paused bool) string {
	switch {
	case !j.Enabled:
		return "disabled"
	case paused:
		return "paused"
	case j.Mode == config.ModeOnce:
		return "one-time"
	default:
		return "idle"
	}
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
