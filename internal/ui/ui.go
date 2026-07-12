//go:build windows

// Package ui is the native Windows front-end (system tray + settings window)
// built on tailscale/walk. It is a thin layer over the manager: every action
// calls a manager method, and the manager pushes refreshes back through
// callbacks that this package marshals onto the GUI thread.
package ui

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"

	"github.com/nekogravitycat/auto-image-converter/internal/autostart"
	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
	"github.com/nekogravitycat/auto-image-converter/internal/manager"
)

// maxLogLines bounds the in-memory activity buffer shown in the window.
const maxLogLines = 500

// UI holds the widgets and wiring for the application window and tray icon.
type UI struct {
	mgr *manager.Manager
	log *logx.Logger

	mw          *walk.MainWindow
	ni          *walk.NotifyIcon
	model       *jobModel
	tv          *walk.TableView
	logBox      *walk.TextEdit
	statsLabel  *walk.Label
	workersEdit *walk.NumberEdit
	autostartCB *walk.CheckBox
	startMinCB  *walk.CheckBox
	pauseAction *walk.Action

	loading bool // suppresses change handlers while initializing controls

	logMu    sync.Mutex
	logLines []string
	logDirty bool
}

// Run builds the window and tray, wires up and starts the manager, and runs the
// message loop until the user chooses Exit or ctx is cancelled (an OS
// interrupt/logoff). It must be called on the main goroutine. Shutdown of the
// manager is the caller's responsibility once Run returns.
func Run(ctx context.Context, mgr *manager.Manager, log *logx.Logger, startMinimized bool) error {
	app, err := walk.InitApp()
	if err != nil {
		return err
	}

	u := &UI{mgr: mgr, log: log, model: &jobModel{}}
	if err := u.build(); err != nil {
		return err
	}
	defer u.ni.Dispose()

	mgr.SetCallbacks(u.onChange, u.onNotify)
	log.AddSink(u.onLogLine)

	u.refreshTable()
	u.refreshStats()
	u.refreshPauseLabel()

	// Now that the UI can receive updates, start the manager's jobs.
	mgr.Start()

	stopTicker := make(chan struct{})
	go u.tick(stopTicker)

	// Cancelling ctx (an OS signal) ends the message loop just like tray Exit.
	go func() {
		select {
		case <-ctx.Done():
			walk.App().Synchronize(func() { walk.App().Exit(0) })
		case <-stopTicker:
		}
	}()

	if !startMinimized {
		u.showWindow()
	}

	app.Run()
	close(stopTicker)
	return nil
}

// build constructs the main window and the tray icon.
func (u *UI) build() error {
	appIcon := walk.IconApplication()

	if err := (MainWindow{
		AssignTo:    &u.mw,
		Title:       "Auto Image Converter",
		Icon:        appIcon,
		Size:        Size{Width: 760, Height: 560},
		Layout:      VBox{},
		OnDropFiles: u.onDropFiles,
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					PushButton{Text: "Add folder…", OnClicked: u.onAdd},
					PushButton{Text: "Edit…", OnClicked: u.onEdit},
					PushButton{Text: "Convert now", OnClicked: u.onConvertNow},
					PushButton{Text: "Enable / Disable", OnClicked: u.onToggleEnabled},
					PushButton{Text: "Remove", OnClicked: u.onRemove},
					HSpacer{},
				},
			},
			TableView{
				AssignTo: &u.tv,
				Model:    u.model,
				Columns: []TableViewColumn{
					{Title: "Name", Width: 120},
					{Title: "Folder", Width: 250},
					{Title: "Format", Width: 60},
					{Title: "Quality", Width: 60},
					{Title: "Mode", Width: 80},
					{Title: "After", Width: 100},
					{Title: "Status", Width: 130},
				},
				OnItemActivated: u.onEdit,
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{Text: "Parallel workers:"},
					NumberEdit{AssignTo: &u.workersEdit, MinValue: 1, MaxValue: 64, OnValueChanged: u.onWorkersChanged},
					HSpacer{},
					CheckBox{AssignTo: &u.startMinCB, Text: "Start minimized to tray", OnCheckedChanged: u.onStartMinChanged},
					CheckBox{AssignTo: &u.autostartCB, Text: "Launch at login", OnCheckedChanged: u.onAutostartChanged},
				},
			},
			Label{Text: "Activity"},
			TextEdit{AssignTo: &u.logBox, ReadOnly: true, VScroll: true, MinSize: Size{Height: 150}},
			Label{AssignTo: &u.statsLabel, Text: ""},
		},
	}).Create(); err != nil {
		return err
	}

	// The close button hides to the tray; the app keeps running in the
	// background. Exiting is done from the tray menu.
	u.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		*canceled = true
		u.mw.Hide()
	})

	// Initialize control values without triggering their change handlers.
	u.loading = true
	app := u.mgr.App()
	_ = u.workersEdit.SetValue(float64(app.MaxWorkers))
	u.startMinCB.SetChecked(app.StartMinimized)
	if en, err := autostart.Enabled(); err == nil {
		u.autostartCB.SetChecked(en)
	} else {
		u.autostartCB.SetChecked(app.Autostart)
	}
	u.loading = false

	return u.buildTray(appIcon)
}

// buildTray creates the notification-area icon and its context menu.
func (u *UI) buildTray(icon *walk.Icon) error {
	ni, err := walk.NewNotifyIcon()
	if err != nil {
		return err
	}
	u.ni = ni
	_ = ni.SetIcon(icon)
	_ = ni.SetToolTip("Auto Image Converter")

	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			u.showWindow()
		}
	})

	add := func(text string, handler func()) {
		a := walk.NewAction()
		_ = a.SetText(text)
		a.Triggered().Attach(handler)
		_ = ni.ContextMenu().Actions().Add(a)
	}

	add("Open", u.showWindow)

	u.pauseAction = walk.NewAction()
	_ = u.pauseAction.SetText("Pause all")
	u.pauseAction.Triggered().Attach(u.onTogglePause)
	_ = ni.ContextMenu().Actions().Add(u.pauseAction)

	add("Convert all now", func() { u.mgr.ConvertAllNow() })
	_ = ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	add("Exit", func() { walk.App().Exit(0) })

	return ni.SetVisible(true)
}

// --- manager callbacks (called from background goroutines) ---

func (u *UI) onChange() {
	walk.App().Synchronize(func() {
		u.refreshTable()
		u.refreshStats()
		u.refreshPauseLabel()
	})
}

func (u *UI) onNotify(title, body string, isError bool) {
	walk.App().Synchronize(func() {
		if u.ni != nil {
			_ = u.ni.ShowInfo(title, body)
		}
	})
}

func (u *UI) onLogLine(line string) {
	u.logMu.Lock()
	u.logLines = append(u.logLines, strings.TrimRight(line, "\r\n"))
	if len(u.logLines) > maxLogLines {
		u.logLines = u.logLines[len(u.logLines)-maxLogLines:]
	}
	u.logDirty = true
	u.logMu.Unlock()
}

// tick periodically refreshes the live-updating parts of the window (stats and
// the activity log) so a running batch shows progress without flooding the UI
// with per-file updates.
func (u *UI) tick(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			walk.App().Synchronize(func() {
				u.refreshStats()
				u.refreshLog()
			})
		}
	}
}

// --- refresh helpers (run on the GUI thread) ---

func (u *UI) refreshTable() {
	sel := u.tv.CurrentIndex()
	u.model.setJobs(u.mgr.Jobs())
	if sel >= 0 && sel < u.model.RowCount() {
		_ = u.tv.SetCurrentIndex(sel)
	}
}

func (u *UI) refreshStats() {
	ss, ls := u.mgr.Stats()
	u.statsLabel.SetText(fmt.Sprintf(
		"Session: %d converted, %d failed, %s saved     |     Total: %d converted, %s saved",
		ss.Converted, ss.Failed, humanBytes(ss.BytesSaved),
		ls.Converted, humanBytes(ls.BytesSaved)))
}

func (u *UI) refreshLog() {
	u.logMu.Lock()
	if !u.logDirty {
		u.logMu.Unlock()
		return
	}
	text := strings.Join(u.logLines, "\r\n")
	u.logDirty = false
	u.logMu.Unlock()
	u.logBox.SetText(text)
}

func (u *UI) refreshPauseLabel() {
	if u.pauseAction == nil {
		return
	}
	if u.mgr.Paused() {
		_ = u.pauseAction.SetText("Resume all")
	} else {
		_ = u.pauseAction.SetText("Pause all")
	}
}

// --- actions (GUI thread) ---

func (u *UI) showWindow() {
	u.mw.Show()
	_ = u.mw.SetFocus()
}

func (u *UI) onAdd() {
	jc, ok := editJobDialog(u.mw, "Add folder", config.DefaultJob())
	if !ok {
		return
	}
	u.mgr.AddJob(jc)
	u.refreshTable()
}

func (u *UI) onEdit() {
	j, ok := u.selectedJob()
	if !ok {
		return
	}
	jc, accepted := editJobDialog(u.mw, "Edit folder", j.Cfg)
	if !accepted {
		return
	}
	u.mgr.UpdateJob(j.ID, jc)
	u.refreshTable()
}

func (u *UI) onConvertNow() {
	if j, ok := u.selectedJob(); ok {
		u.mgr.ConvertNow(j.ID)
	}
}

func (u *UI) onToggleEnabled() {
	if j, ok := u.selectedJob(); ok {
		u.mgr.SetJobEnabled(j.ID, !j.Cfg.Enabled)
		u.refreshTable()
	}
}

func (u *UI) onRemove() {
	j, ok := u.selectedJob()
	if !ok {
		return
	}
	if walk.MsgBox(u.mw, "Remove folder",
		fmt.Sprintf("Stop monitoring and remove %q?", j.Cfg.Name),
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	u.mgr.RemoveJob(j.ID)
	u.refreshTable()
}

func (u *UI) onWorkersChanged() {
	if u.loading {
		return
	}
	u.mgr.SetMaxWorkers(int(u.workersEdit.Value()))
}

func (u *UI) onStartMinChanged() {
	if u.loading {
		return
	}
	u.mgr.SetStartMinimized(u.startMinCB.Checked())
}

func (u *UI) onAutostartChanged() {
	if u.loading {
		return
	}
	if err := u.mgr.SetAutostart(u.autostartCB.Checked()); err != nil {
		walk.MsgBox(u.mw, "Autostart", "Could not update launch-at-login setting:\n"+err.Error(), walk.MsgBoxIconError)
	}
}

func (u *UI) onTogglePause() {
	if u.mgr.Paused() {
		u.mgr.ResumeAll()
	} else {
		u.mgr.PauseAll()
	}
	walk.App().Synchronize(u.refreshPauseLabel)
}

func (u *UI) onDropFiles(files []string) {
	pngs := expandPNGs(files)
	if len(pngs) == 0 {
		walk.MsgBox(u.mw, "Nothing to convert", "No PNG files were found in what you dropped.", walk.MsgBoxIconInformation)
		return
	}
	spec, ok := dropOnceDialog(u.mw, len(pngs))
	if !ok {
		return
	}
	u.mgr.ConvertOnce(pngs, spec, "Drag & drop")
}

// --- helpers ---

func (u *UI) selectedJob() (manager.JobState, bool) {
	idx := u.tv.CurrentIndex()
	if idx < 0 {
		walk.MsgBox(u.mw, "Select a folder", "Select a folder in the list first.", walk.MsgBoxIconInformation)
		return manager.JobState{}, false
	}
	return u.model.jobAt(idx)
}

// expandPNGs turns a mix of dropped files and folders into a flat list of PNG
// file paths.
func expandPNGs(paths []string) []string {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					if d != nil && d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.IsDir() && convert.IsPNG(path) {
					out = append(out, path)
				}
				return nil
			})
			continue
		}
		if convert.IsPNG(p) {
			out = append(out, p)
		}
	}
	return out
}

// humanBytes formats a byte count as a short human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
