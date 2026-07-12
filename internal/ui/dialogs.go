//go:build windows

package ui

import (
	"strconv"
	"strings"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
)

// Combo option orders shared by the dialogs. Index 0 is the default.
var (
	formatValues = []string{config.FormatJPEG, config.FormatHEIF}
	formatLabels = []string{"JPEG (.jpg)", "HEIF (.heic, needs Python + pillow-heif)"}

	modeValues = []string{config.ModeMonitor, config.ModeOnce}
	modeLabels = []string{"Monitor (watch continuously)", "One-time (convert existing, then stop)"}

	actionValues = []string{config.ActionReplace, config.ActionOutputFolder}
	actionLabels = []string{"Replace original", "Keep original, write to output folder"}
)

func indexOf(values []string, v string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	for i, x := range values {
		if strings.ToLower(x) == v {
			return i
		}
	}
	return 0
}

// modeLabel and postActionLabel render enum values for the table.
func modeLabel(v string) string {
	switch strings.ToLower(v) {
	case config.ModeOnce:
		return "One-time"
	default:
		return "Monitor"
	}
}

func postActionLabel(v string) string {
	switch strings.ToLower(v) {
	case config.ActionOutputFolder:
		return "Output folder"
	default:
		return "Replace"
	}
}

// editJobDialog shows the job editor initialized from jc and returns the edited
// job plus true when the user accepts, or (jc, false) when cancelled.
func editJobDialog(owner walk.Form, title string, jc config.JobConfig) (config.JobConfig, bool) {
	var (
		dlg         *walk.Dialog
		okPB, canPB *walk.PushButton

		nameEdit    *walk.LineEdit
		folderEdit  *walk.LineEdit
		modeCombo   *walk.ComboBox
		formatCombo *walk.ComboBox
		qualityEdit *walk.NumberEdit
		actionCombo *walk.ComboBox
		outputEdit  *walk.LineEdit
		recursiveCB *walk.CheckBox
		depthEdit   *walk.NumberEdit
		batchCB     *walk.CheckBox
		enabledCB   *walk.CheckBox
	)

	accepted := false

	err := (Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &okPB,
		CancelButton:  &canPB,
		MinSize:       Size{Width: 520, Height: 420},
		Layout:        VBox{},
		Children: []Widget{
			Composite{
				Layout: Grid{Columns: 3},
				Children: []Widget{
					Label{Text: "Name:"},
					LineEdit{AssignTo: &nameEdit, Text: jc.Name, ColumnSpan: 2},

					Label{Text: "Folder:"},
					LineEdit{AssignTo: &folderEdit, Text: jc.WatchDirectory},
					PushButton{Text: "Browse…", OnClicked: func() {
						if p, ok := browseFolder(dlg, folderEdit.Text()); ok {
							folderEdit.SetText(p)
						}
					}},

					Label{Text: "Mode:"},
					ComboBox{AssignTo: &modeCombo, Model: modeLabels, CurrentIndex: indexOf(modeValues, jc.Mode), ColumnSpan: 2},

					Label{Text: "Format:"},
					ComboBox{AssignTo: &formatCombo, Model: formatLabels, CurrentIndex: indexOf(formatValues, jc.TargetFormat), ColumnSpan: 2},

					Label{Text: "Quality (1-100):"},
					NumberEdit{AssignTo: &qualityEdit, MinValue: 1, MaxValue: 100, Value: float64(jc.Quality), ColumnSpan: 2},

					Label{Text: "After converting:"},
					ComboBox{AssignTo: &actionCombo, Model: actionLabels, CurrentIndex: indexOf(actionValues, jc.PostAction), ColumnSpan: 2},

					Label{Text: "Output folder:"},
					LineEdit{AssignTo: &outputEdit, Text: jc.OutputDirectory},
					PushButton{Text: "Browse…", OnClicked: func() {
						if p, ok := browseFolder(dlg, outputEdit.Text()); ok {
							outputEdit.SetText(p)
						}
					}},

					Label{Text: "Subfolders:"},
					CheckBox{AssignTo: &recursiveCB, Text: "Include subfolders", Checked: jc.Recursive, ColumnSpan: 2},

					Label{Text: "Max depth (0 = ∞):"},
					NumberEdit{AssignTo: &depthEdit, MinValue: 0, MaxValue: 64, Value: float64(jc.MaxDepth), ColumnSpan: 2},

					Label{Text: "On startup:"},
					CheckBox{AssignTo: &batchCB, Text: "Convert existing files when the app starts", Checked: jc.BatchOnStartup, ColumnSpan: 2},

					Label{Text: "Enabled:"},
					CheckBox{AssignTo: &enabledCB, Text: "This folder is active", Checked: jc.Enabled, ColumnSpan: 2},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{
						AssignTo: &okPB,
						Text:     "OK",
						OnClicked: func() {
							if actionValues[actionCombo.CurrentIndex()] == config.ActionOutputFolder &&
								strings.TrimSpace(outputEdit.Text()) == "" {
								walk.MsgBox(dlg, "Output folder required",
									"Choose an output folder, or switch \"After converting\" back to Replace original.",
									walk.MsgBoxIconWarning)
								return
							}
							accepted = true
							dlg.Accept()
						},
					},
					PushButton{AssignTo: &canPB, Text: "Cancel", OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}).Create(owner)
	if err != nil {
		return jc, false
	}
	applyWindowChrome(dlg)

	if dlg.Run() != walk.DlgCmdOK || !accepted {
		return jc, false
	}

	jc.Name = strings.TrimSpace(nameEdit.Text())
	jc.WatchDirectory = strings.TrimSpace(folderEdit.Text())
	jc.Mode = modeValues[modeCombo.CurrentIndex()]
	jc.TargetFormat = formatValues[formatCombo.CurrentIndex()]
	jc.Quality = int(qualityEdit.Value())
	jc.PostAction = actionValues[actionCombo.CurrentIndex()]
	jc.OutputDirectory = strings.TrimSpace(outputEdit.Text())
	jc.Recursive = recursiveCB.Checked()
	jc.MaxDepth = int(depthEdit.Value())
	jc.BatchOnStartup = batchCB.Checked()
	jc.Enabled = enabledCB.Checked()
	return jc, true
}

// dropOnceDialog asks how to convert dropped files once and returns the spec
// plus true on accept. label is filled in for notifications.
func dropOnceDialog(owner walk.Form, fileCount int) (convert.JobSpec, bool) {
	var (
		dlg         *walk.Dialog
		okPB, canPB *walk.PushButton
		formatCombo *walk.ComboBox
		qualityEdit *walk.NumberEdit
		actionCombo *walk.ComboBox
		outputEdit  *walk.LineEdit
	)
	accepted := false

	err := (Dialog{
		AssignTo:      &dlg,
		Title:         "Convert dropped files once",
		DefaultButton: &okPB,
		CancelButton:  &canPB,
		MinSize:       Size{Width: 480, Height: 240},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: pluralFiles(fileCount) + " will be converted once, with no ongoing monitoring."},
			Composite{
				Layout: Grid{Columns: 3},
				Children: []Widget{
					Label{Text: "Format:"},
					ComboBox{AssignTo: &formatCombo, Model: formatLabels, CurrentIndex: 0, ColumnSpan: 2},

					Label{Text: "Quality (1-100):"},
					NumberEdit{AssignTo: &qualityEdit, MinValue: 1, MaxValue: 100, Value: 90, ColumnSpan: 2},

					Label{Text: "After converting:"},
					ComboBox{AssignTo: &actionCombo, Model: actionLabels, CurrentIndex: 0, ColumnSpan: 2},

					Label{Text: "Output folder:"},
					LineEdit{AssignTo: &outputEdit},
					PushButton{Text: "Browse…", OnClicked: func() {
						if p, ok := browseFolder(dlg, outputEdit.Text()); ok {
							outputEdit.SetText(p)
						}
					}},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &okPB, Text: "Convert", OnClicked: func() {
						if actionValues[actionCombo.CurrentIndex()] == config.ActionOutputFolder &&
							strings.TrimSpace(outputEdit.Text()) == "" {
							walk.MsgBox(dlg, "Output folder required",
								"Choose an output folder, or switch to Replace original.",
								walk.MsgBoxIconWarning)
							return
						}
						accepted = true
						dlg.Accept()
					}},
					PushButton{AssignTo: &canPB, Text: "Cancel", OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}).Create(owner)
	if err != nil {
		return convert.JobSpec{}, false
	}
	applyWindowChrome(dlg)
	if dlg.Run() != walk.DlgCmdOK || !accepted {
		return convert.JobSpec{}, false
	}

	// Go through a JobConfig rather than hand-building the spec, so an ad-hoc
	// conversion gets exactly the same validation and path normalization as a
	// configured folder. The watch directory stays empty: the dropped files are
	// an explicit list with no common root, so in output_folder mode they all
	// land directly in the output folder.
	jc, _ := config.NormalizeJob(config.JobConfig{
		Name:            "Drag & drop",
		TargetFormat:    formatValues[formatCombo.CurrentIndex()],
		Quality:         int(qualityEdit.Value()),
		PostAction:      actionValues[actionCombo.CurrentIndex()],
		OutputDirectory: strings.TrimSpace(outputEdit.Text()),
	})
	return convert.SpecFromJob(jc), true
}

// AlreadyRunningAlert tells the user that another copy owns the single-instance
// guard, so this launch is about to exit. It is called before any window or tray
// icon exists: MsgBox with a nil owner goes straight to the Win32 MessageBox, so
// it needs no message loop. Topmost/foreground keep it visible when the launch
// came from a shortcut and no window of ours has focus.
func AlreadyRunningAlert() {
	walk.MsgBox(nil, "Auto Image Converter",
		"Auto Image Converter is already running.\n\nLook for its icon in the notification area (system tray) to open the settings window.",
		walk.MsgBoxIconInformation|walk.MsgBoxOK|walk.MsgBoxTopMost|walk.MsgBoxSetForeground)
}

// browseFolder shows the folder picker seeded with current and returns the
// chosen path.
func browseFolder(owner walk.Form, current string) (string, bool) {
	dlg := new(walk.FileDialog)
	dlg.Title = "Select a folder"
	dlg.FilePath = current
	ok, err := dlg.ShowBrowseFolder(owner)
	if err != nil || !ok {
		return "", false
	}
	return dlg.FilePath, true
}

func pluralFiles(n int) string {
	if n == 1 {
		return "1 PNG file"
	}
	return strconv.Itoa(n) + " PNG files"
}
