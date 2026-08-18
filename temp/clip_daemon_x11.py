#!/usr/bin/env python3
"""
clip_daemon_x11.py — X11 clipboard watcher that feeds suchi's JSON history.

Replaces the GNOME "clipboard-history" extension dependency for X11 users.
Uses XFixes (same technique as wach_x11.py) to get woken up the instant the
CLIPBOARD selection owner changes, then pulls the new text with `xclip` and
appends it to the same JSON file suchi.py reads.

Requires: xclip  (sudo apt install xclip / sudo pacman -S xclip / ...)

Usage:
    python3 clip_daemon_x11.py                 # watches CLIPBOARD, default path
    python3 clip_daemon_x11.py -f ~/history.json --limit 300
    python3 clip_daemon_x11.py -s primary       # watch PRIMARY instead

Run this in the background (shell autostart, systemd --user unit, etc). It
never exits on its own — it's meant to be a long-running daemon, unlike
wach_x11.py which exits on the first event.
"""

import sys
import os
import json
import time
import ctypes
import subprocess
import argparse
from ctypes.util import find_library

DEFAULT_HISTORY_PATH = os.path.expanduser("~/.cache/suchi/history.json")
DEFAULT_LIMIT = 500

# ---------- X11 / XFixes ctypes setup (same approach as wach_x11.py) ----------
x11_path = find_library("X11")
xfixes_path = find_library("Xfixes")

if not x11_path or not xfixes_path:
    sys.stderr.write(
        "Could not find libX11 or libXfixes. Install libx11-6 / libxfixes3 (dev headers not needed).\n"
    )
    sys.exit(1)

libX11 = ctypes.cdll.LoadLibrary(x11_path)
libXfixes = ctypes.cdll.LoadLibrary(xfixes_path)

Display_p = ctypes.c_void_p
Window = ctypes.c_ulong
Atom = ctypes.c_ulong
Bool = ctypes.c_int

XA_PRIMARY = 1
False_ = 0
XFixesSetSelectionOwnerNotifyMask = 1 << 0

libX11.XOpenDisplay.argtypes = [ctypes.c_char_p]
libX11.XOpenDisplay.restype = Display_p
libX11.XDefaultRootWindow.argtypes = [Display_p]
libX11.XDefaultRootWindow.restype = Window
libX11.XInternAtom.argtypes = [Display_p, ctypes.c_char_p, Bool]
libX11.XInternAtom.restype = Atom


class XEvent(ctypes.Structure):
    _fields_ = [("pad", ctypes.c_long * 24)]  # padding, big enough for the union


libX11.XNextEvent.argtypes = [Display_p, ctypes.POINTER(XEvent)]
libX11.XNextEvent.restype = ctypes.c_int
libX11.XCloseDisplay.argtypes = [Display_p]
libX11.XCloseDisplay.restype = ctypes.c_int
libXfixes.XFixesSelectSelectionInput.argtypes = [
    Display_p,
    Window,
    Atom,
    ctypes.c_ulong,
]
libXfixes.XFixesSelectSelectionInput.restype = None


# ---------- clipboard content fetch ----------


def get_clipboard_text(selection):
    """Grab current selection text via xclip. Returns None if empty/binary/unavailable."""
    try:
        result = subprocess.run(
            ["xclip", "-selection", selection, "-o"],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=2,
        )
        if result.returncode != 0:
            return None
        return result.stdout.decode("utf-8", errors="replace")
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
        return None


# ---------- history store (same schema suchi.py expects) ----------


def load_history(path):
    if not os.path.exists(path):
        return []
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, list):
            return [x for x in data if isinstance(x, dict)]
    except (json.JSONDecodeError, ValueError, UnicodeDecodeError, OSError):
        pass
    return []


def save_history(path, data, limit):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    pinned = [x for x in data if x.get("pinned")]
    unpinned = [x for x in data if not x.get("pinned")]
    unpinned = unpinned[: max(0, limit - len(pinned))]
    trimmed = pinned + unpinned
    trimmed.sort(
        key=lambda x: (
            bool(x.get("pinned")),
            max(x.get("usedAt") or 0, x.get("copiedAt") or 0),
        ),
        reverse=True,
    )
    tmp_path = path + ".tmp"
    with open(tmp_path, "w", encoding="utf-8") as f:
        json.dump(trimmed, f, ensure_ascii=False)
    os.replace(tmp_path, path)  # atomic, so suchi.py never reads a half-written file


def add_clip(path, text, limit):
    if text is None:
        return
    # xclip adds one trailing newline for plain single-line copies; drop just that one
    if text.endswith("\n"):
        text = text[:-1]
    if not text.strip():
        return

    data = load_history(path)
    now_ms = int(time.time() * 1000)

    # Already have this text? bump it instead of creating a duplicate entry
    for item in data:
        if item.get("text") == text:
            item["copiedAt"] = now_ms
            save_history(path, data, limit)
            return

    data.insert(
        0,
        {
            "text": text,
            "pinned": False,
            "copiedAt": now_ms,
            "usedAt": 0,
        },
    )
    save_history(path, data, limit)


# ---------- main loop ----------


def main():
    parser = argparse.ArgumentParser(
        description="X11 clipboard watcher -> suchi history store"
    )
    parser.add_argument(
        "-f",
        "--file",
        default=DEFAULT_HISTORY_PATH,
        help=f"History JSON path (default: {DEFAULT_HISTORY_PATH})",
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=DEFAULT_LIMIT,
        help=f"Max stored entries, pinned items exempt (default: {DEFAULT_LIMIT})",
    )
    parser.add_argument(
        "-s",
        "--selection",
        default="clipboard",
        choices=["clipboard", "primary"],
        help="Which X selection to watch (default: clipboard). "
        "'primary' fires on every mouse-highlight, so it's noisy — "
        "clipboard (explicit copy) is what you almost always want.",
    )
    args = parser.parse_args()

    try:
        subprocess.run(
            ["xclip", "-version"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=2,
        )
    except (FileNotFoundError, OSError):
        sys.stderr.write("xclip not found. Install it first: sudo apt install xclip\n")
        sys.exit(1)

    disp = libX11.XOpenDisplay(None)
    if not disp:
        sys.stderr.write("Can't open X display\n")
        sys.exit(1)

    root = libX11.XDefaultRootWindow(disp)
    if args.selection == "clipboard":
        target_atom = libX11.XInternAtom(disp, b"CLIPBOARD", False_)
    else:
        target_atom = XA_PRIMARY

    libXfixes.XFixesSelectSelectionInput(
        disp, root, target_atom, XFixesSetSelectionOwnerNotifyMask
    )

    evt = XEvent()
    last_text = None
    print(f"[clip_daemon_x11] watching '{args.selection}' -> {args.file}", flush=True)

    try:
        while True:
            libX11.XNextEvent(disp, ctypes.byref(evt))
            # give the new selection owner a beat to actually populate the data
            time.sleep(0.05)
            text = get_clipboard_text(args.selection)
            if text is None or text == last_text:
                continue
            last_text = text
            add_clip(args.file, text, args.limit)
    except KeyboardInterrupt:
        pass
    finally:
        libX11.XCloseDisplay(disp)


if __name__ == "__main__":
    main()
