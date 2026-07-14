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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"

	"github.com/nekogravitycat/auto-image-converter/internal/autostart"
	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/humanize"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
	"github.com/nekogravitycat/auto-image-converter/internal/manager"
)

// UI holds the widgets and wiring for the application window and tray icon.
type UI struct {
	mgr     *manager.Manager
	log     *logx.Logger
	logPath string

	mw          *walk.MainWindow
	ni          *walk.NotifyIcon
	model       *jobModel
	tv          *walk.TableView
	statsLabel  *walk.Label
	workersEdit *walk.NumberEdit
	autostartCB *walk.CheckBox
	startMinCB  *walk.CheckBox
	pauseAction *walk.Action

	loading bool // suppresses change handlers while initializing controls

	// workersTimer debounces the parallel-workers field (see onWorkersChanged).
	workersTimer *time.Timer
}

// workersApplyDelay is how long the parallel-workers field must sit still before
// the change is applied. A NumberEdit reports every intermediate value while the
// user types or holds the spinner, and each distinct value rebuilds the shared
// worker pool, so "8" on the way to "16" should not cost a rebuild.
const workersApplyDelay = 600 * time.Millisecond

// Run builds the window and tray, wires up and starts the manager, and runs the
// message loop until the user chooses Exit or ctx is cancelled (an OS
// interrupt/logoff). It must be called on the main goroutine. Shutdown of the
// manager is the caller's responsibility once Run returns.
func Run(ctx context.Context, mgr *manager.Manager, log *logx.Logger, logPath string, startMinimized bool) error {
	app, err := walk.InitApp()
	if err != nil {
		return err
	}

	u := &UI{mgr: mgr, log: log, logPath: logPath, model: &jobModel{}}
	if err := u.build(); err != nil {
		return err
	}
	defer u.ni.Dispose()

	mgr.SetCallbacks(u.onChange, u.onNotify)

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

// applyWindowChrome gives a top-level window the Windows 11 Mica backdrop
// material on its title bar. The window stays in light mode regardless of the
// system theme: walk's client-area controls have no dark theme, and a dark
// title bar over a light client area looks mismatched. Older Windows versions
// reject the call with ErrUnsupportedOnThisWindowsVersion, which is fine to
// ignore.
func applyWindowChrome(w walk.Win32Window) {
	_ = w.SetSystemBackdrop(win.DWMSBT_MAINWINDOW)
}

// build constructs the main window and the tray icon.
func (u *UI) build() error {
	appIcon := walk.IconApplication()

	if err := (MainWindow{
		AssignTo: &u.mw,
		Title:    "Auto Image Converter",
		Icon:     appIcon,
		Size:     Size{Width: 760, Height: 480},
		MinSize:  Size{Width: 640, Height: 360},
		// Create the window hidden; Run decides whether to show it, so that
		// "start minimized to tray" is honored. Leaving Visible unset is not
		// the same thing: the declarative builder shows the window unless the
		// property is explicitly false.
		Visible:     false,
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
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &u.statsLabel, Text: ""},
					HSpacer{},
					PushButton{Text: "Open log", ToolTipText: "Open a terminal window that follows the log live", OnClicked: u.onOpenLog},
				},
			},
		},
	}).Create(); err != nil {
		return err
	}

	applyWindowChrome(u.mw)

	// MainWindow always creates a built-in menu bar, tool bar, and status bar.
	// The empty tool bar is nonetheless a visible window parked across the top
	// of the client area — the layout only reserves room for it once it has
	// actions — so it covers the top of the first row of controls. This app
	// uses none of them; hide the ones that paint.
	u.mw.ToolBar().SetVisible(false)
	u.mw.StatusBar().SetVisible(false)

	// The close button hides to the tray; the app keeps running in the
	// background. Exiting is done from the tray menu.
	//
	// Cancelling Closing only keeps the window alive: MainWindow's WM_CLOSE
	// handler ends the message loop regardless of whether the event was
	// cancelled, so exit-on-close has to be disabled as well.
	u.mw.SetExitOnClose(false)
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

// tick periodically refreshes the stats line so a running batch shows progress
// without flooding the UI with per-file updates.
func (u *UI) tick(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			walk.App().Synchronize(u.refreshStats)
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
		ss.Converted, ss.Failed, humanize.Bytes(ss.BytesSaved),
		ls.Converted, humanize.Bytes(ls.BytesSaved)))
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

// onWorkersChanged applies the new worker count once the field settles. It runs
// on the GUI thread, so the timer needs no lock; SetMaxWorkers is safe to call
// from the timer's goroutine.
func (u *UI) onWorkersChanged() {
	if u.loading {
		return
	}
	n := int(u.workersEdit.Value())
	if u.workersTimer != nil {
		u.workersTimer.Stop()
	}
	u.workersTimer = time.AfterFunc(workersApplyDelay, func() { u.mgr.SetMaxWorkers(n) })
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

// onOpenLog opens a new terminal window that follows the log file, giving the
// full history plus live updates without keeping a copy in the window.
func (u *UI) onOpenLog() {
	// Single-quoted PowerShell literal: only ' needs escaping (by doubling).
	quoted := strings.ReplaceAll(u.logPath, "'", "''")
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-Command",
		"$Host.UI.RawUI.WindowTitle = 'Auto Image Converter log'; Get-Content -LiteralPath '"+quoted+"' -Tail 200 -Wait")
	// The app itself is a windowsgui binary with no console, so the viewer
	// must be given its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	if err := cmd.Start(); err != nil {
		u.log.Errorf("could not open log viewer: %v", err)
		walk.MsgBox(u.mw, "Open log", "Could not open a log window:\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	// The viewer outlives or predeceases us independently; just release the
	// process handle when it exits.
	go func() { _ = cmd.Wait() }()
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

