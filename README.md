# Auto Image Converter

A lightweight Windows background utility that watches a folder for new **PNG**
screenshots and automatically converts them to **JPEG** or **HEIF** to save disk
space. It runs quietly in the background (no console window), with near-zero idle
CPU, and never touches an original file unless its conversion fully succeeded.

## Features

- **Background watcher** — reacts to new PNGs the moment they are written, using
  `fsnotify` (event-driven, no polling).
- **Recursive** — watches the whole directory tree, with a configurable depth
  limit; new subfolders are picked up automatically at runtime.
- **Startup batch** — optionally converts any PNGs already present when it starts.
- **JPEG or HEIF output** — JPEG uses Go's native encoder (no external tools);
  HEIF uses a bundled Python script backed by `pillow-heif`.
- **Transparency-safe** — transparent PNGs are composited onto a solid white
  background before JPEG encoding, so there are no black artifacts.
- **Best-effort metadata** — EXIF from the source PNG is carried into the JPEG
  output; HEIF relies on `pillow-heif`'s own EXIF/ICC passthrough.
- **Safe post-actions** — after a verified conversion, either delete the original
  (`replace`) or write the output into a separate folder and keep the original
  (`output_folder`). On any failure the original is left untouched.

## Requirements

- Windows 10 / 11
- [Go](https://go.dev/dl/) 1.26+ to build
- For HEIF output only: Python 3.9+ on `PATH` and `pillow-heif`
  (`pip install pillow-heif`); the encoder script `tools/heif_convert.py` ships
  with the app (see [`tools/README.md`](tools/README.md))

## Build

```powershell
pwsh -File build.ps1
# or:
go build -ldflags="-H=windowsgui" -o auto-image-converter.exe .
```

The `-H=windowsgui` flag hides the console window so the program runs purely in
the background. All diagnostics go to `auto-image-converter.log` next to the exe.

## Run

Place `auto-image-converter.exe` wherever you like and run it. On **first launch**
it generates a `config.yml` next to the executable and then exits immediately,
because `watch_directory` is empty by default — the program will not guess which
folder to monitor on your behalf.

Open the generated `config.yml`, set `watch_directory` to the folder you want to
watch, and run the exe again. From then on it starts working normally. To run it
automatically at login, put a shortcut to the exe in your Startup folder
(`shell:startup`).

> All application files (`config.yml`, `tools/`, the log) are resolved relative
> to the executable, not the current working directory — so launching from a
> Startup shortcut works correctly.

### Stopping the program

Because the program runs with no console window, tray icon, or window, there is
no button to close it. To stop it:

- Run the bundled helper script: `pwsh -File stop.ps1`, or
- **Task Manager** (`Ctrl+Shift+Esc`) → Details → `auto-image-converter.exe` →
  End task, or
- PowerShell: `Stop-Process -Name auto-image-converter`

## Configuration (`config.yml`)

```yaml
watcher:
  # REQUIRED — empty by default; the program will not run until you set this.
  # Example: "C:\\Users\\YourUsername\\Pictures\\Screenshots"
  watch_directory: ""
  enabled: true          # enable real-time background monitoring
  batch_on_startup: true # convert existing PNGs once at startup
  recursive: true        # also watch subfolders
  max_depth: 0           # 0 = unlimited; N = at most N levels below the watch root

converter:
  target_format: "JPEG"  # JPEG or HEIF
  quality: 90            # 1-100
  max_workers: 4         # concurrency cap for the startup batch

file_management:
  post_action: "replace" # replace | output_folder
  output_directory: ""   # required only when post_action is "output_folder"
```

`watch_directory` is the only field you must set. If it is empty (or blank), the
program logs an error and exits without doing anything.

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
missing files are generated, unreadable ones fall back to safe defaults, and
individual invalid fields are corrected and logged. The one field with no safe
default is `watch_directory`: if it is empty, the program logs an error and exits
rather than guessing a folder to monitor. If HEIF is selected but its runtime
(Python + `pillow-heif`) is unavailable, the startup environment check reports the
cause, conversions fail safely, and originals are kept.

## Development

```powershell
go test ./...   # run unit tests
go vet ./...    # static checks
```
