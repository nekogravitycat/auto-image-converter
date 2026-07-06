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

Exit codes:
    0  success
    2  environment not ready (Python deps missing or HEIF encoder unavailable)
    3  conversion failed
    4  bad invocation / arguments
"""

from __future__ import annotations

import argparse
import io
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


def cmd_convert(src: str, dst: str, quality: int) -> int:
    """Convert the image at ``src`` to HEIF at ``dst``."""
    Image, pillow_heif = _load()
    pillow_heif.register_heif_opener()
    try:
        with Image.open(src) as img:
            img.load()
            out = _normalize(img)
            save_kwargs = {"quality": quality}
            # Best-effort metadata passthrough; never fail the encode over it.
            exif = img.info.get("exif")
            if exif:
                save_kwargs["exif"] = exif
            icc = img.info.get("icc_profile")
            if icc:
                save_kwargs["icc_profile"] = icc
            out.save(dst, format="HEIF", **save_kwargs)
    except FileNotFoundError:
        return _fail(3, f"input file not found: {src}")
    except Exception as exc:  # noqa: BLE001 - surface any decode/encode error
        return _fail(3, f"HEIF conversion failed: {exc}")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="heif_convert.py",
        description="Convert an image to HEIF using pillow-heif.",
    )
    parser.add_argument("--check", action="store_true",
                        help="verify the environment and exit")
    parser.add_argument("-q", "--quality", type=int, default=90,
                        help="encoder quality, 1-100 (default: 90)")
    parser.add_argument("-o", "--output", help="output HEIF path")
    parser.add_argument("input", nargs="?", help="input image path")
    args = parser.parse_args(argv)

    if args.check:
        return cmd_check()

    if not args.output or not args.input:
        parser.print_usage(sys.stderr)
        return _fail(4, "both an input file and -o <output> are required")

    # Clamp defensively; the Go caller already validates, but never trust input.
    quality = max(1, min(100, args.quality))

    return cmd_convert(args.input, args.output, quality)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
