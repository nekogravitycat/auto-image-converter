# tools

This directory holds bundled helpers that the application invokes as sidecar
processes. It is resolved relative to the executable's own location, so it must
sit next to `auto-image-converter.exe` at runtime.

## heif_convert.py (required only for HEIF output)

When `converter.target_format` is set to `HEIF`, the application shells out to
`heif_convert.py`, a small script that encodes HEIF via
[pillow-heif](https://pypi.org/project/pillow-heif/). The script is committed to
the repository and ships with the app — you do **not** need to supply it.

JPEG output works out of the box with no external tools, so HEIF is treated as
an opt-in choice for advanced users: enabling it means providing a Python
runtime with `pillow-heif` installed.

### Enable HEIF

1. Install **Python 3.9+** and make sure it is on `PATH` (the app tries
   `python`, `python3`, then the `py` launcher, in that order).
2. Install the encoder library:

   ```powershell
   pip install pillow-heif
   ```

   This also pulls in Pillow. The heavy native code (libheif + codecs, including
   the HEVC encoder) lives inside this wheel — nothing else needs to be placed
   in this folder.
3. Set `converter.target_format: "HEIF"` in `config.yml`.

### Verify

The application self-checks the environment at startup by running
`python heif_convert.py --check`, which imports pillow-heif and performs a trial
encode. You can run the same check yourself:

```powershell
python tools\heif_convert.py --check
# -> ok: pillow-heif ready (libheif <version>)
```

If the check fails, a clear, actionable error is written to
`auto-image-converter.log` and HEIF conversions fail safely — the original PNG
files are always left untouched.

### Direct usage

The application calls the script like this (arguments are passed directly, with
no shell, so paths need no escaping):

```powershell
python tools\heif_convert.py -q <quality 1-100> -o <output.heic> <input.png>
```

Exit codes: `0` success · `2` environment not ready · `3` conversion failed ·
`4` bad arguments.

### Troubleshooting

| Log symptom                                   | Cause                                   | Fix                                        |
| --------------------------------------------- | --------------------------------------- | ------------------------------------------ |
| `no Python interpreter found on PATH`         | Python not installed or not on `PATH`   | Install Python 3.9+; reopen the shell      |
| `HEIF support unavailable: No module named …` | `pillow-heif` (or Pillow) not installed | `pip install pillow-heif`                  |
| `pillow-heif ... HEIF encoding failed`        | Decode-only build / broken install      | Reinstall: `pip install -U pillow-heif`    |
| `HEIF conversion timed out`                   | Interpreter hung (2-minute limit hit)   | Check the machine load / the input file    |

The JPEG output path has no external dependency and needs nothing here.

> Unlike a native encoder binary, this script is plain text with no bundled
> DLLs, so it is safe to commit and carries no antivirus-heuristic risk.
