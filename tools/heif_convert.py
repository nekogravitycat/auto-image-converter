#!/usr/bin/env python3
"""HEIF encoder sidecar for auto-image-converter.

The Go application invokes this script to convert an image to HEIF when
``converter.target_format`` is set to HEIF. It is a thin, self-checking wrapper
around pillow-heif, which lets the main program stay pure Go while HEIF support
remains an opt-in feature for users who choose to install the Python runtime.

Requirements (installed by the user, intentionally not bundled):
    Python 3.9+
    pip install pillow-heif        # also pulls in Pillow

Usage:
    python heif_convert.py -q <quality> -o <output.heic> <input.png>
    python heif_convert.py --check          # self-test the environment
    python heif_convert.py --serve          # persistent worker mode (see below)

Persistent worker mode (--serve):
    Imports the (expensive) imaging backend once, then serves an unbounded
    stream of conversion jobs so the Go caller never pays repeated interpreter
    startup + import costs. Protocol is newline-delimited JSON over stdin/stdout:

        stdin  (one request per line):  {"src": "in.png", "dst": "out.heic", "quality": 90}
        stdout (one response per line): {"ok": true}
                                        {"ok": false, "error": "..."}

    One response is written per request, in order. stdout carries responses
    only; all diagnostics go to stderr. The worker exits 0 on stdin EOF.

Exit codes:
    0  success
    2  environment not ready (Python deps missing or HEIF encoder unavailable)
    3  conversion failed
    4  bad invocation / arguments
"""

from __future__ import annotations

import argparse
import io
import json
import sys


def _fail(code: int, message: str) -> int:
    """Print a diagnostic to stderr and return the given exit code."""
    print(message, file=sys.stderr)
    return code


def _load():
    """Import the imaging backend, or exit(2) with a clear, actionable message."""
    try:
        from PIL import Image
        import pillow_heif
    except ImportError as exc:
        raise SystemExit(_fail(
            2,
            f"HEIF support unavailable: {exc}. "
            "Install it with: pip install pillow-heif",
        ))
    return Image, pillow_heif


def _normalize(img):
    """Coerce the image into a colour mode the HEIF encoder accepts.

    HEIF handles L / RGB / RGBA natively; palette, CMYK, etc. are converted,
    preserving an alpha channel when one is present.
    """
    if img.mode in ("RGB", "RGBA", "L", "LA"):
        return img
    if "A" in img.getbands() or (img.mode == "P" and "transparency" in img.info):
        return img.convert("RGBA")
    return img.convert("RGB")


def cmd_check() -> int:
    """Verify the environment can actually produce HEIF output."""
    Image, pillow_heif = _load()
    pillow_heif.register_heif_opener()
    # A trial encode is the only reliable proof that this pillow-heif build
    # includes an HEIF *encoder* (some builds are decode-only).
    try:
        probe = Image.new("RGB", (2, 2), (127, 127, 127))
        buf = io.BytesIO()
        probe.save(buf, format="HEIF", quality=50)
    except Exception as exc:  # noqa: BLE001 - report any encoder failure verbatim
        return _fail(2, f"pillow-heif is installed but HEIF encoding failed: {exc}")
    if buf.tell() == 0:
        return _fail(2, "pillow-heif produced no HEIF output during self-test")
    try:
        version = pillow_heif.libheif_version()
    except Exception:  # noqa: BLE001
        version = "unknown"
    print(f"ok: pillow-heif ready (libheif {version})")
    return 0


def _convert_one(Image, src: str, dst: str, quality: int) -> None:
    """Convert ``src`` to HEIF at ``dst``, raising on any failure.

    The HEIF opener must already be registered by the caller. Metadata is
    carried over best-effort and never fails the encode.
    """
    with Image.open(src) as img:
        img.load()
        out = _normalize(img)
        save_kwargs = {"quality": quality}
        exif = img.info.get("exif")
        if exif:
            save_kwargs["exif"] = exif
        icc = img.info.get("icc_profile")
        if icc:
            save_kwargs["icc_profile"] = icc
        out.save(dst, format="HEIF", **save_kwargs)


def cmd_convert(src: str, dst: str, quality: int) -> int:
    """Convert the image at ``src`` to HEIF at ``dst`` (one-shot mode)."""
    Image, pillow_heif = _load()
    pillow_heif.register_heif_opener()
    try:
        _convert_one(Image, src, dst, quality)
    except FileNotFoundError:
        return _fail(3, f"input file not found: {src}")
    except Exception as exc:  # noqa: BLE001 - surface any decode/encode error
        return _fail(3, f"HEIF conversion failed: {exc}")
    return 0


def cmd_serve() -> int:
    """Persistent worker: convert an unbounded stream of jobs from stdin.

    See the module docstring for the newline-delimited JSON protocol. The
    expensive imports happen once, up front; each subsequent job reuses the
    already-warm interpreter, which is the whole point of this mode.
    """
    Image, pillow_heif = _load()
    pillow_heif.register_heif_opener()

    # Force UTF-8 with '\n' line endings in both directions so non-ASCII paths
    # (e.g. Chinese folder names) survive on Windows, where the console default
    # is a legacy code page and text mode would otherwise translate newlines.
    try:
        sys.stdin.reconfigure(encoding="utf-8")
        sys.stdout.reconfigure(encoding="utf-8", newline="\n")
    except AttributeError:
        pass  # very old Python; fall back to whatever the defaults are

    def respond(ok: bool, error: str | None = None) -> None:
        msg = {"ok": ok}
        if error:
            msg["error"] = error
        sys.stdout.write(json.dumps(msg) + "\n")
        sys.stdout.flush()

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            src = req["src"]
            dst = req["dst"]
            quality = max(1, min(100, int(req.get("quality", 90))))
        except (ValueError, KeyError, TypeError) as exc:
            respond(False, f"bad request: {exc}")
            continue
        try:
            _convert_one(Image, src, dst, quality)
            respond(True)
        except Exception as exc:  # noqa: BLE001 - report, but keep the worker alive
            respond(False, str(exc))

    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="heif_convert.py",
        description="Convert an image to HEIF using pillow-heif.",
    )
    parser.add_argument("--check", action="store_true",
                        help="verify the environment and exit")
    parser.add_argument("--serve", action="store_true",
                        help="persistent worker mode: stream jobs as JSON over stdin/stdout")
    parser.add_argument("-q", "--quality", type=int, default=90,
                        help="encoder quality, 1-100 (default: 90)")
    parser.add_argument("-o", "--output", help="output HEIF path")
    parser.add_argument("input", nargs="?", help="input image path")
    args = parser.parse_args(argv)

    if args.check:
        return cmd_check()

    if args.serve:
        return cmd_serve()

    if not args.output or not args.input:
        parser.print_usage(sys.stderr)
        return _fail(4, "both an input file and -o <output> are required")

    # Clamp defensively; the Go caller already validates, but never trust input.
    quality = max(1, min(100, args.quality))

    return cmd_convert(args.input, args.output, quality)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
