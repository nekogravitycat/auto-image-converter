# Auto Image Converter

A Windows utility that watches folders for new **PNG** screenshots and
automatically converts them to **JPEG** or **HEIF** to save disk space. It runs
from the **system tray** with near-zero idle CPU, and never touches an original
file unless its conversion fully succeeded.

You can monitor **multiple folders at once**, each with its own format, quality,
and post-action, and either watch a folder continuously or convert it just once.

## Features

- **System tray + settings window** — the app lives in the tray; open the window
  to manage folders, or right-click the tray for quick actions (pause/resume,
  convert everything now, exit).
- **Multiple folders, independent settings** — each monitored folder ("job") has
  its own target format, quality, recursion, and post-action.
- **Monitor or one-time** — a job can *watch continuously* (converting new files
  as they appear) or run *once* (convert the files already there, then stop).
- **One shared worker pool** — a single global "parallel workers" setting bounds
  total concurrent conversions across every folder, so many folders never
  oversubscribe the CPU. HEIF workers share one warm pool too.
- **Drag-and-drop** — drop files or folders onto the window to convert them once,
  without creating a permanent job.
- **Stats & notifications** — the window shows running totals (files converted,
  disk space saved) and tray balloons announce batch completion and errors; the
  **Open log** button opens a terminal that follows the log file live.
- **Launch at login** — a checkbox toggles a per-user startup entry (no manual
  shortcut needed).
- **Background watcher** — reacts to new PNGs the moment they are written, using
  `fsnotify` (event-driven, no polling); new subfolders are picked up at runtime.
- **JPEG or HEIF output** — JPEG uses Go's native encoder (no external tools);
  HEIF uses a bundled Python script backed by `pillow-heif`.
- **Transparency-safe** — transparent PNGs are composited onto white before JPEG
  encoding, so there are no black artifacts.
- **Best-effort metadata** — EXIF from the source PNG is carried into the JPEG
  output; HEIF relies on `pillow-heif`'s own EXIF/ICC passthrough.
- **Safe post-actions** — after a verified conversion, either delete the original
  (`replace`) or write the output into a separate folder and keep the original
  (`output_folder`). On any failure the original is left untouched.

## Requirements

- Windows 10 / 11
- [Go](https://go.dev/dl/) 1.26+ to build (pure Go — no C compiler / CGO)
- For HEIF output only: Python 3.9+ on `PATH` and `pillow-heif`
  (`pip install pillow-heif`); the encoder script `tools/heif_convert.py` ships
  with the app

## Build

```powershell
pwsh -File build.ps1
# or:
$env:CGO_ENABLED = "0"
go build -ldflags="-H=windowsgui" -o auto-image-converter.exe .
```

The `-H=windowsgui` flag hides the console window. The GUI toolkit
([tailscale/walk](https://github.com/tailscale/walk)) needs Common Controls 6,
which is supplied by the external manifest **`auto-image-converter.exe.manifest`**
that ships next to the executable — keep the two together.

## Run

Place `auto-image-converter.exe` (with its `.manifest` and the `tools/` folder)
wherever you like and run it. It starts in the system tray. On **first launch**
there are no folders configured, so the settings window opens automatically:
click **Add folder…**, pick a folder and settings, and it starts working.

- **Open the window:** click the tray icon (or right-click → *Open*).
- **Add / edit / remove folders:** the buttons above the list. Double-click a row
  to edit it.
- **Convert now:** run a one-off pass over the selected folder (or *Convert all
  now* from the tray).
- **Drag-and-drop:** drop files/folders on the window to convert them once.
- **Pause all / Resume all:** temporarily stop all watching without changing
  which folders are enabled.
- **Launch at login:** toggle the checkbox in the window.

> All application files (`config.yml`, `stats.json`, `tools/`, the log, the
> `.manifest`) are resolved relative to the executable, not the current working
> directory — so launching from a Startup entry works correctly.

> **Single instance:** only one copy runs per user session. A second launch
> detects the running instance, logs `another instance is already running;
> exiting`, and exits, so two watchers can never race on the same files. A
> crashed instance releases the guard automatically.

### Stopping the program

Right-click the tray icon → **Exit**. This performs a **graceful shutdown**: any
in-flight conversions are given time to finish (so no partial `.converting.tmp`
files are left behind) and the HEIF worker processes are shut down cleanly.
Closing the window only hides it to the tray; it keeps running in the background.

You can still force-stop via **Task Manager** → `auto-image-converter.exe` → End
task. That skips the graceful drain, but nothing is lost: any leftover
`.converting.tmp` is cleaned up on the next startup, and the HEIF workers are
terminated with the parent, so no orphan processes remain.

## Configuration (`config.yml`)

The config file is normally managed through the UI, but hand-edits are valid and
re-read (and cleaned) on the next start.

```yaml
version: 1

app:
  max_workers: 8         # shared worker pool size across ALL folders
  start_minimized: true  # start to the tray without opening the window
  autostart: false       # launch at login (managed by the UI checkbox)

jobs:
  - name: "VRChat"
    watch_directory: "C:\\Users\\YourUsername\\Pictures\\VRChat"
    enabled: true
    mode: "monitor"          # monitor = watch continuously | once = convert existing, then stop
    batch_on_startup: true   # convert files already present when the app starts
    recursive: true          # also watch subfolders
    max_depth: 0             # 0 = unlimited; N = at most N levels below the watch root
    target_format: "HEIF"    # JPEG | HEIF (per folder)
    quality: 90              # 1-100 (per folder)
    post_action: "replace"   # replace | output_folder (per folder)
    output_directory: ""     # required only when post_action is "output_folder"
```

A fresh install generates this file with an empty `jobs:` list; you add folders
from the window.

### Post-action modes

| Mode            | Converted file goes to                           | Original PNG |
| --------------- | ------------------------------------------------ | ------------ |
| `replace`       | the same folder as the source                    | deleted      |
| `output_folder` | `output_directory`, mirroring the subfolder path | kept         |

In `output_folder` mode, if the output directory lives inside the watched tree,
it is automatically excluded from watching and scanning to avoid conversion
loops. Existing output names are never overwritten — a numeric suffix is added
(e.g. `shot-1.jpg`).

## Resilience

A missing, corrupt, or partially invalid `config.yml` never crashes the program:

- **Missing file** — generated with defaults (no jobs).
- **Unreadable / not valid YAML** — safe defaults are used for this run and the
  file is left untouched (so a hand-fixable mistake is never destroyed).
- **Partially invalid** — every value that can be read is kept; any setting that
  is missing, unrecognized, the wrong type, or out of range falls back to its
  default. The file is then rewritten in clean, complete form (valid values
  preserved, defaults for the rest) and each correction is logged. This is
  idempotent: once repaired, a later start finds nothing to fix.

A job whose `watch_directory` is empty or points to a non-existent folder is not
run; the window shows its status so you can fix it. If HEIF is selected but its
runtime (Python + `pillow-heif`) is unavailable, the startup environment check
reports the cause, conversions fail safely, and originals are kept.

## Development

```powershell
go test ./...   # run unit tests
go vet ./...    # static checks
```
