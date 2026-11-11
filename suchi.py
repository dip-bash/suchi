#!/usr/bin/env python3
"""
suchi - clipboard history for the terminal (Wayland, X11 and TTY).
Dev - Saumyadip Jana, contact: saumyadip.social@gmail.com

Quick start:
    suchi --install-service  # record in the background at every login (systemd user service)
    suchi --demon            # ...or start the recorder by hand (suchi --stop to stop it)
    suchi                    # browse / search the history; Enter copies the entry

More commands:
    suchi -l [N]             # print the N newest entries (default 20); add --json for scripts
    suchi --get N            # print entry #N raw to stdout (numbers match the TUI)
    suchi -q WORDS           # open the TUI with a search already typed
    suchi -p                 # TUI, but print the chosen entry to stdout instead of copying
    echo text | suchi --add  # push text into the history by hand (works in a TTY)
    suchi --pause / --resume # stop / restart recording, e.g. while you copy passwords
    suchi --clear            # delete everything that is not pinned (-y skips the prompt)
    suchi --status           # daemon, backend and database info

Only one suchi window is open at a time: starting a new one closes the previous one
(--allow-multiple turns that off). Handy with a terminal launcher, e.g. for foot:
    footclient -W 80x30 suchi

The TUI starts in search mode: just type to filter (all words must match, in any
order), Enter copies. Esc switches to vim-style normal mode, ? shows every key.
Use --normal to start in normal mode.

Backends (auto-detected, override with --backend):
    wayland  native Wayland protocol (ext-data-control / wlr-data-control),
             pure Python, no wl-clipboard needed. Works on sway, Hyprland,
             KDE Plasma, river, labwc, COSMIC ...  NOT on GNOME/Mutter, which
             does not offer a background clipboard protocol; there suchi falls
             back to the X11 backend through XWayland.
    x11      libX11 + XFixes through ctypes to watch, xclip (or xsel) to
             read/write.
    tty      no display server (Linux console, SSH). There is no clipboard to
             watch, so history comes from --add (or from a GUI session sharing
             the same history file). Choosing an entry uses OSC 52 if the
             terminal supports it, otherwise types it into the prompt
             (TIOCSTI, single-line only) or prints it.

Entries flagged by password managers (x-kde-passwordManagerHint) are never recorded.

Paste into the shell prompt in any terminal, including the Linux console
(bash, put in ~/.bashrc):
    bind -x '"\\C-x\\C-v": __s=$(suchi -p); READLINE_LINE="${READLINE_LINE:0:READLINE_POINT}${__s}${READLINE_LINE:READLINE_POINT}"; READLINE_POINT=$((READLINE_POINT+${#__s}))'

Autostart: `suchi --install-service` writes and enables this user unit for you
(~/.config/systemd/user/suchi.service); `suchi --uninstall-service` removes it:
    [Service]
    ExecStart=/path/to/suchi --demon --foreground
    Restart=on-failure
    [Install]
    WantedBy=graphical-session.target
"""

import argparse
import array
import base64
import contextlib
import ctypes
import curses
import fcntl
import json
import locale
import os
import re
import select
import shutil
import signal
import socket
import sqlite3
import struct
import subprocess
import sys
import time
import unicodedata
from ctypes.util import find_library

__version__ = "0.4.1"

# Apply UI configurations
os.environ.setdefault("ESCDELAY", "25")

_CACHE_HOME = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
DEFAULT_HISTORY_PATH = os.path.join(_CACHE_HOME, "suchi", "history.db")
DEFAULT_LIMIT = 500
DEFAULT_MAX_BYTES = 1024 * 1024  # entries larger than this are not recorded
PREVIEW_CHARS = 4000  # how much of an entry the TUI preview pane loads


class BackendError(Exception):
    """A clipboard backend is missing or failed; the message is user-facing."""


def eprint(*a):
    print(*a, file=sys.stderr, flush=True)


# ════════════════════════════════════════════════════════════════════════════
# HISTORY STORAGE  (SQLite, WAL mode)
#
# SQLite does the locking between the daemon and the TUI, so there are no lock
# files or temp-file renames any more. The database is created 0600 (clipboard
# contents routinely include passwords and tokens) and secure_delete zeroes the
# bytes of deleted entries instead of leaving them in the file.
# ════════════════════════════════════════════════════════════════════════════

SCHEMA = """
CREATE TABLE IF NOT EXISTS clips (
    id     INTEGER PRIMARY KEY,
    text   TEXT    NOT NULL UNIQUE,
    pinned INTEGER NOT NULL DEFAULT 0,
    ts     INTEGER NOT NULL
)"""  # ts = last time the entry was copied or used, in ms since the epoch


def _num(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else 0


def _now_ms():
    return int(time.time() * 1000)


def _clean(text):
    """Make sure the text is valid UTF-8 (lone surrogates would break SQLite)."""
    return text.encode("utf-8", "replace").decode("utf-8")


def _ensure_dir(path):
    d = os.path.dirname(path) or "."
    os.makedirs(d, mode=0o700, exist_ok=True)
    return d


def _quarantine(path):
    """Move an unreadable database aside instead of silently overwriting it."""
    dest = f"{path}.corrupt-{int(time.time())}"
    with contextlib.suppress(OSError):
        os.replace(path, dest)
        eprint(f"suchi: history database was corrupt, moved to {dest}")
    for ext in ("-wal", "-shm"):
        with contextlib.suppress(OSError):
            os.unlink(path + ext)


def _migrate_json(db, json_path):
    """One-time import of the old history.json (suchi < 0.3)."""
    if not os.path.exists(json_path):
        return
    with contextlib.suppress(OSError, ValueError, TypeError, sqlite3.Error):
        with open(json_path, "r", encoding="utf-8") as f:
            data = json.load(f)
        rows = [(_clean(x["text"]), int(bool(x.get("pinned"))),
                 int(max(_num(x.get("usedAt")), _num(x.get("copiedAt")))))
                for x in data
                if isinstance(x, dict) and isinstance(x.get("text"), str) and x["text"].strip()]
        with db:
            db.executemany("INSERT OR IGNORE INTO clips(text, pinned, ts) VALUES (?, ?, ?)", rows)
        os.replace(json_path, json_path + ".migrated")


def open_db(path):
    """Open (creating if needed) the history database. The daemon and the TUI
    can both have it open at once. Callers close it (contextlib.closing)."""
    _ensure_dir(path)
    for attempt in (1, 2):
        # Pre-create as 0600: SQLite itself would honour the umask (usually 0644),
        # and it gives the -wal / -shm files the same permissions as this file.
        os.close(os.open(path, os.O_RDWR | os.O_CREAT, 0o600))
        db = sqlite3.connect(path, timeout=5)
        try:
            db.execute("PRAGMA journal_mode=WAL")
            db.execute("PRAGMA synchronous=NORMAL")
            db.execute("PRAGMA secure_delete=ON")
            db.execute(SCHEMA)
            break
        except sqlite3.OperationalError:  # locked / read-only dir: not corruption
            db.close()
            raise
        except sqlite3.DatabaseError:  # "file is not a database", malformed image
            db.close()
            if attempt == 2:
                raise
            _quarantine(path)
    if path == DEFAULT_HISTORY_PATH:
        _migrate_json(db, os.path.join(os.path.dirname(path), "history.json"))
    return db


def add_clip(db, text, limit, max_bytes=DEFAULT_MAX_BYTES):
    """Adds a text snippet to the history, bumping instead of duplicating.
    The text is stored exactly as copied (no trailing-newline trimming).
    Returns True if something was recorded."""
    if text is None:
        return False
    text = _clean(text)
    if not text.strip() or len(text.encode("utf-8")) > max_bytes:
        return False
    with db:  # one transaction: commit on success, roll back on error
        db.execute("INSERT INTO clips(text, ts) VALUES (?, ?) "
                   "ON CONFLICT(text) DO UPDATE SET ts = excluded.ts", (text, _now_ms()))
        pinned = db.execute("SELECT COUNT(*) FROM clips WHERE pinned = 1").fetchone()[0]
        # keep every pinned entry plus the newest unpinned ones that fit the limit
        db.execute("DELETE FROM clips WHERE pinned = 0 AND id NOT IN "
                   "(SELECT id FROM clips WHERE pinned = 0 ORDER BY ts DESC, id DESC LIMIT ?)",
                   (max(0, limit - pinned),))
    return True


def load_rows(db):
    """(id, first DISPLAY_CAP chars, pinned, ts) for the TUI: pinned first, then newest.
    Only the head of each entry is loaded; the full text is fetched when one is chosen."""
    return db.execute("SELECT id, substr(text, 1, ?), pinned, ts FROM clips "
                      "ORDER BY pinned DESC, ts DESC, id DESC", (DISPLAY_CAP,)).fetchall()


def use_clip(db, clip_id):
    """Bump an entry to the top and return its full text (None if it vanished)."""
    with db:
        db.execute("UPDATE clips SET ts = ? WHERE id = ?", (_now_ms(), clip_id))
        row = db.execute("SELECT text FROM clips WHERE id = ?", (clip_id,)).fetchone()
    return row[0] if row else None


def delete_clip(db, clip_id):
    """Delete an entry. Returns (text, pinned, ts) of what was removed (for undo)."""
    with db:
        row = db.execute("SELECT text, pinned, ts FROM clips WHERE id = ?", (clip_id,)).fetchone()
        db.execute("DELETE FROM clips WHERE id = ?", (clip_id,))
    return row


def restore_clip(db, text, pinned, ts):
    """Put a deleted entry back (merging if the same text was copied again). Returns its id."""
    with db:
        db.execute("INSERT INTO clips(text, pinned, ts) VALUES (?, ?, ?) "
                   "ON CONFLICT(text) DO UPDATE SET pinned = MAX(pinned, excluded.pinned), "
                   "ts = MAX(ts, excluded.ts)", (text, pinned, ts))
        return db.execute("SELECT id FROM clips WHERE text = ?", (text,)).fetchone()[0]


def toggle_pin(db, clip_id):
    with db:
        db.execute("UPDATE clips SET pinned = 1 - pinned WHERE id = ?", (clip_id,))


def get_preview(db, clip_id):
    """(first PREVIEW_CHARS chars, total length) of one entry."""
    row = db.execute("SELECT substr(text, 1, ?), length(text) FROM clips WHERE id = ?",
                     (PREVIEW_CHARS, clip_id)).fetchone()
    return row or ("", 0)


def get_text(db, clip_id):
    row = db.execute("SELECT text FROM clips WHERE id = ?", (clip_id,)).fetchone()
    return row[0] if row else None


def count_clips(db):
    """(total entries, pinned entries)"""
    return db.execute("SELECT COUNT(*), COALESCE(SUM(pinned), 0) FROM clips").fetchone()


def clear_clips(db, include_pinned=False):
    """Delete all unpinned (or all) entries, then shrink the file and the WAL so
    the deleted text is really gone from disk. Returns the number deleted."""
    with db:
        n = db.execute("DELETE FROM clips" + ("" if include_pinned else " WHERE pinned = 0")).rowcount
    db.execute("VACUUM")
    db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    return n


# ════════════════════════════════════════════════════════════════════════════
# X11 BACKEND  (libX11/XFixes via ctypes to watch, xclip/xsel to read/write)
# ════════════════════════════════════════════════════════════════════════════

Display_p = ctypes.c_void_p
Window = ctypes.c_ulong
Atom = ctypes.c_ulong
Bool = ctypes.c_int

XA_PRIMARY = 1
False_ = 0
XFixesSetSelectionOwnerNotifyMask = 1 << 0


class XEvent(ctypes.Structure):
    _fields_ = [("pad", ctypes.c_long * 24)]  # padding, big enough for the union


def _load_lib(name, sonames):
    path = find_library(name)
    for cand in ([path] if path else []) + list(sonames):
        try:
            return ctypes.cdll.LoadLibrary(cand)
        except OSError:
            continue
    return None


def get_x11_libs():
    """Initializes and returns libX11 and libXfixes using ctypes."""
    libX11 = _load_lib("X11", ["libX11.so.6"])
    libXfixes = _load_lib("Xfixes", ["libXfixes.so.3"])
    if not libX11 or not libXfixes:
        raise BackendError("Could not find libX11 or libXfixes. Install libx11-6 / libxfixes3.")

    libX11.XOpenDisplay.argtypes = [ctypes.c_char_p]
    libX11.XOpenDisplay.restype = Display_p
    libX11.XDefaultRootWindow.argtypes = [Display_p]
    libX11.XDefaultRootWindow.restype = Window
    libX11.XInternAtom.argtypes = [Display_p, ctypes.c_char_p, Bool]
    libX11.XInternAtom.restype = Atom
    libX11.XConnectionNumber.argtypes = [Display_p]
    libX11.XConnectionNumber.restype = ctypes.c_int
    libX11.XPending.argtypes = [Display_p]
    libX11.XPending.restype = ctypes.c_int
    libX11.XNextEvent.argtypes = [Display_p, ctypes.POINTER(XEvent)]
    libX11.XNextEvent.restype = ctypes.c_int
    libX11.XCloseDisplay.argtypes = [Display_p]
    libX11.XCloseDisplay.restype = ctypes.c_int

    libXfixes.XFixesSelectSelectionInput.argtypes = [Display_p, Window, Atom, ctypes.c_ulong]
    libXfixes.XFixesSelectSelectionInput.restype = None
    return libX11, libXfixes


def x11_tool():
    for tool in ("xclip", "xsel"):
        if shutil.which(tool):
            return tool
    return None


X11_TEXT_TARGETS = ("UTF8_STRING", "text/plain;charset=utf-8", "text/plain", "STRING")


def _xclip_targets(selection):
    """The list of formats the current selection owner offers (None if unknown)."""
    try:
        r = subprocess.run(["xclip", "-selection", selection, "-t", "TARGETS", "-o"],
                           stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=2)
    except (subprocess.TimeoutExpired, OSError):
        return None
    if r.returncode != 0:
        return None
    return r.stdout.decode("utf-8", errors="replace").split()


def x11_read(selection):
    """Grabs current selection text via xclip/xsel. None if empty, not text, or a
    password manager flagged it as secret."""
    tool = x11_tool()
    if tool == "xclip":
        cmd = ["xclip", "-selection", selection, "-o"]
        targets = _xclip_targets(selection)
        if targets is not None:
            if any(t in WL_SECRET_MIMES for t in targets):
                return None
            fmt = next((t for t in X11_TEXT_TARGETS if t in targets), None)
            if fmt is None:
                return None  # image / files etc. are not recorded
            cmd += ["-t", fmt]
    elif tool == "xsel":
        cmd = ["xsel", "--" + selection, "--output"]
    else:
        return None
    try:
        result = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=2)
    except (subprocess.TimeoutExpired, OSError):
        return None
    if result.returncode != 0:
        return None
    return result.stdout.decode("utf-8", errors="replace")


def x11_write(text, selection="clipboard"):
    """Pushes a string to the X11 selection using xclip/xsel (they fork to keep serving it)."""
    tool = x11_tool()
    if tool == "xclip":
        cmd = ["xclip", "-selection", selection, "-i"]
    elif tool == "xsel":
        cmd = ["xsel", "--" + selection, "--input"]
    else:
        raise BackendError("xclip (or xsel) not found. Install it: sudo apt install xclip")
    try:
        proc = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, start_new_session=True)
        proc.communicate(text.encode("utf-8"), timeout=3)
    except subprocess.TimeoutExpired:
        return  # still serving the selection; that is fine
    except OSError as e:
        raise BackendError(f"could not run {tool}: {e}")
    if proc.returncode != 0:
        raise BackendError(f"{tool} failed (exit {proc.returncode}); is DISPLAY correct?")


def watch_x11(selection, callback, stop):
    """Block, calling callback(text) whenever the X selection changes."""
    libX11, libXfixes = get_x11_libs()
    disp = libX11.XOpenDisplay(None)
    if not disp:
        raise BackendError("Can't open X display")
    try:
        root = libX11.XDefaultRootWindow(disp)
        atom = libX11.XInternAtom(disp, b"CLIPBOARD", False_) if selection == "clipboard" else XA_PRIMARY
        libXfixes.XFixesSelectSelectionInput(disp, root, atom, XFixesSetSelectionOwnerNotifyMask)
        fd = libX11.XConnectionNumber(disp)
        evt = XEvent()
        last = None
        changed = True  # record whatever is on the clipboard at startup
        while not stop():
            # select() on the connection (instead of a blocking XNextEvent) so
            # SIGTERM / --stop are honoured promptly.
            while libX11.XPending(disp):
                libX11.XNextEvent(disp, ctypes.byref(evt))
                changed = True
            if changed:
                changed = False
                time.sleep(0.05)  # let the new owner finish setting up
                text = x11_read(selection)
                if text is not None and text != last:
                    last = text
                    callback(text)
                continue
            select.select([fd], [], [], 1.0)
    finally:
        libX11.XCloseDisplay(disp)


# ════════════════════════════════════════════════════════════════════════════
# WAYLAND BACKEND  (native wire protocol, ext-data-control / wlr-data-control)
# ════════════════════════════════════════════════════════════════════════════

WL_TEXT_MIMES = ("text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "TEXT", "STRING")
WL_SECRET_MIMES = ("x-kde-passwordManagerHint",)  # password managers flag secrets with this
WL_MANAGERS = ("ext_data_control_manager_v1", "zwlr_data_control_manager_v1")


def _wl_str(s):
    b = s.encode("utf-8") + b"\0"
    return struct.pack("=I", len(b)) + b + b"\0" * (-len(b) % 4)


def _wl_read_str(buf, off):
    (n,) = struct.unpack_from("=I", buf, off)
    s = bytes(buf[off + 4: off + 4 + max(0, n - 1)]).decode("utf-8", "replace")
    return s, off + 4 + ((n + 3) & ~3)


def _write_all(fd, data, timeout=5.0):
    view = memoryview(data)
    deadline = time.monotonic() + timeout
    while len(view):
        left = deadline - time.monotonic()
        if left <= 0:
            return
        _, w, _ = select.select([], [fd], [], left)
        if not w:
            return
        try:
            n = os.write(fd, view[:65536])
        except (BlockingIOError, InterruptedError):
            continue
        except OSError:  # reader went away
            return
        view = view[n:]


class WaylandClipboard:
    """Minimal Wayland client speaking only what is needed for clipboard access."""

    def __init__(self):
        name = os.environ.get("WAYLAND_DISPLAY") or "wayland-0"
        if os.path.isabs(name):
            path = name
        else:
            rt = os.environ.get("XDG_RUNTIME_DIR")
            if not rt:
                raise BackendError("XDG_RUNTIME_DIR is not set; cannot find the Wayland socket")
            path = os.path.join(rt, name)
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(3)
        try:
            self.sock.connect(path)
        except OSError as e:
            self.sock.close()
            raise BackendError(f"cannot connect to Wayland socket {path}: {e}")
        self.sock.settimeout(None)

        self.rbuf = bytearray()
        self.fds = []
        self._next_id = 2
        self.globals = {}
        self.offers = {}
        self.current = {"clipboard": 0, "primary": 0}
        self.events = []
        self.sends = []
        self._callbacks = set()
        self.registry = self.mgr = self.dev = self.source = 0
        self.has_primary = False
        self.cancelled = False
        self.finished = False
        try:
            self._setup()
        except BaseException:
            self.close()
            raise

    # ---- low level -------------------------------------------------------
    def close(self):
        with contextlib.suppress(OSError):
            self.sock.close()

    def _new_id(self):
        i = self._next_id
        self._next_id += 1
        return i

    def send(self, obj, op, payload=b"", fds=()):
        msg = struct.pack("=II", obj, ((8 + len(payload)) << 16) | op) + payload
        try:
            if fds:
                self.sock.sendmsg([msg], [(socket.SOL_SOCKET, socket.SCM_RIGHTS,
                                           array.array("i", fds).tobytes())])
            else:
                self.sock.sendall(msg)
        except OSError as e:
            raise BackendError(f"Wayland connection lost: {e}")

    def _fill(self, timeout):
        """Wait up to timeout for data, parse whatever arrived. True if data came in."""
        r, _, _ = select.select([self.sock], [], [], timeout)
        if not r:
            return False
        try:
            data, anc, _flags, _ = self.sock.recvmsg(65536, socket.CMSG_SPACE(32 * 4))
        except OSError as e:
            raise BackendError(f"Wayland connection lost: {e}")
        if not data:
            raise BackendError("Wayland compositor closed the connection")
        for level, typ, cdata in anc:
            if level == socket.SOL_SOCKET and typ == socket.SCM_RIGHTS:
                arr = array.array("i")
                arr.frombytes(cdata[: len(cdata) - (len(cdata) % arr.itemsize)])
                self.fds.extend(arr)
        self.rbuf += data
        while len(self.rbuf) >= 8:
            obj, so = struct.unpack_from("=II", self.rbuf, 0)
            size, op = so >> 16, so & 0xFFFF
            if size < 8:
                raise BackendError("Wayland protocol error: malformed message")
            if len(self.rbuf) < size:
                break
            payload = bytes(self.rbuf[8:size])
            del self.rbuf[:size]
            self._dispatch(obj, op, payload)
        return True

    def roundtrip(self):
        cb = self._new_id()
        self._callbacks.add(cb)
        self.send(1, 0, struct.pack("=I", cb))  # wl_display.sync
        deadline = time.monotonic() + 3
        while cb in self._callbacks:
            if time.monotonic() > deadline:
                raise BackendError("Wayland compositor is not responding")
            self._fill(0.5)

    # ---- events ----------------------------------------------------------
    def _dispatch(self, obj, op, payload):
        if obj == 1:  # wl_display
            if op == 0:  # error
                oid, code = struct.unpack_from("=II", payload, 0)
                msg, _ = _wl_read_str(payload, 8)
                raise BackendError(f"Wayland protocol error {code} on object {oid}: {msg}")
            return  # delete_id
        if obj in self._callbacks:
            self._callbacks.discard(obj)
        elif obj == self.registry:
            if op == 0:  # global
                name = struct.unpack_from("=I", payload, 0)[0]
                iface, off = _wl_read_str(payload, 4)
                ver = struct.unpack_from("=I", payload, off)[0]
                self.globals.setdefault(iface, (name, ver))
        elif obj == self.dev:
            if op == 0:  # data_offer(new_id)
                self.offers[struct.unpack_from("=I", payload, 0)[0]] = []
            elif op in (1, 3):  # selection / primary_selection
                self._on_selection("clipboard" if op == 1 else "primary",
                                   struct.unpack_from("=I", payload, 0)[0])
            elif op == 2:  # finished
                self.finished = True
        elif obj in self.offers:
            if op == 0:  # offer(mime)
                self.offers[obj].append(_wl_read_str(payload, 0)[0])
        elif obj == self.source:
            if op == 0:  # send(mime, fd)
                mime, _ = _wl_read_str(payload, 0)
                if self.fds:
                    self.sends.append((mime, self.fds.pop(0)))
            elif op == 1:  # cancelled
                self.cancelled = True
        # events for unknown / already destroyed objects are ignored

    def _on_selection(self, kind, oid):
        old = self.current[kind]
        self.current[kind] = oid
        if old and old != oid and old in self.offers:
            self.send(old, 1)  # offer.destroy
            del self.offers[old]
        self.events.append((kind, oid))

    # ---- setup -----------------------------------------------------------
    def _bind(self, iface, version):
        name, srv_ver = self.globals[iface]
        v = min(version, srv_ver)
        nid = self._new_id()
        self.send(self.registry, 0, struct.pack("=I", name) + _wl_str(iface) + struct.pack("=II", v, nid))
        return nid, v

    def _setup(self):
        self.registry = self._new_id()
        self.send(1, 1, struct.pack("=I", self.registry))  # wl_display.get_registry
        self.roundtrip()
        iface = next((m for m in WL_MANAGERS if m in self.globals), None)
        if not iface or "wl_seat" not in self.globals:
            raise BackendError(
                "this Wayland compositor has no background clipboard protocol "
                "(ext-data-control / wlr-data-control). GNOME/Mutter is like this; "
                "suchi will use XWayland (X11 backend) instead if available.")
        seat, _ = self._bind("wl_seat", 1)
        self.mgr, ver = self._bind(iface, 2)
        self.has_primary = iface.startswith("ext_") or ver >= 2
        self.dev = self._new_id()
        self.send(self.mgr, 1, struct.pack("=II", self.dev, seat))  # get_data_device
        self.roundtrip()  # delivers the current selection events

    # ---- reading ---------------------------------------------------------
    def read_offer(self, oid, max_bytes, timeout=2.0):
        mimes = self.offers.get(oid)
        if not mimes or any(m in WL_SECRET_MIMES for m in mimes):
            return None
        mime = next((m for m in WL_TEXT_MIMES if m in mimes), None)
        if mime is None:
            mime = next((m for m in mimes if m.startswith("text/plain")), None)
        if mime is None:
            return None  # image / files etc. are not recorded
        r, w = os.pipe()
        try:
            self.send(oid, 0, _wl_str(mime), fds=[w])  # offer.receive
        finally:
            os.close(w)
        chunks, total = [], 0
        deadline = time.monotonic() + timeout
        try:
            while True:
                left = deadline - time.monotonic()
                if left <= 0:
                    return None
                if not select.select([r], [], [], left)[0]:
                    return None
                chunk = os.read(r, 65536)
                if not chunk:
                    break
                total += len(chunk)
                if total > max_bytes:
                    return None
                chunks.append(chunk)
        finally:
            os.close(r)
        return b"".join(chunks).decode("utf-8", "replace")

    def _deliver(self, kind, callback, max_bytes):
        mine = [e for e in self.events if e[0] == kind]
        self.events.clear()
        if mine:
            oid = mine[-1][1]
            if oid and oid in self.offers:
                text = self.read_offer(oid, max_bytes)
                if text is not None:
                    callback(text)

    def watch(self, kind, callback, stop, max_bytes=DEFAULT_MAX_BYTES):
        """Block, calling callback(text) whenever the selection changes."""
        if kind == "primary" and not self.has_primary:
            raise BackendError("this compositor does not support the primary selection")
        self._deliver(kind, callback, max_bytes)
        while not stop():
            if self.finished:
                raise BackendError("Wayland seat went away")
            self._fill(1.0)
            self._deliver(kind, callback, max_bytes)

    # ---- writing ---------------------------------------------------------
    def set_selection(self, text, kind="clipboard"):
        if kind == "primary" and not self.has_primary:
            raise BackendError("this compositor does not support the primary selection")
        self.source = self._new_id()
        self.send(self.mgr, 0, struct.pack("=I", self.source))  # create_data_source
        for m in WL_TEXT_MIMES:
            self.send(self.source, 0, _wl_str(m))  # source.offer
        self.send(self.dev, 0 if kind == "clipboard" else 2, struct.pack("=I", self.source))
        self._payload = text.encode("utf-8")
        self.roundtrip()  # surfaces protocol errors synchronously
        if self.cancelled:
            raise BackendError("compositor refused the selection")

    def serve(self):
        """Answer paste requests until another client takes over the selection."""
        while not self.cancelled and not self.finished:
            self._fill(1.0)
            while self.sends:
                _mime, fd = self.sends.pop(0)
                try:
                    _write_all(fd, self._payload)
                finally:
                    os.close(fd)


def wayland_copy(text, kind="clipboard"):
    """Set the Wayland selection. A forked child keeps serving it (like wl-copy)."""
    wl = WaylandClipboard()
    try:
        wl.set_selection(text, kind)
    except BaseException:
        wl.close()
        raise
    sys.stdout.flush()
    sys.stderr.flush()
    pid = os.fork()
    if pid == 0:
        code = 0
        try:
            os.setsid()
            dn = os.open(os.devnull, os.O_RDWR)
            for fd in (0, 1, 2):
                os.dup2(dn, fd)
            wl.serve()
        except BaseException:
            code = 1
        finally:
            os._exit(code)
    wl.close()  # parent's copy only; the child keeps the connection alive


# ════════════════════════════════════════════════════════════════════════════
# BACKEND SELECTION + TTY FALLBACKS
# ════════════════════════════════════════════════════════════════════════════

def detect_backend(preferred="auto"):
    """Returns (backend, problem). backend is 'wayland' | 'x11' | 'tty'.
    problem explains why a graphical session could not be used (or None)."""
    if preferred == "tty":
        return "tty", None
    if preferred == "wayland":
        WaylandClipboard().close()  # raises BackendError with the reason
        return "wayland", None
    if preferred == "x11":
        if not os.environ.get("DISPLAY"):
            raise BackendError("DISPLAY is not set; no X11 server to talk to")
        if not x11_tool():
            raise BackendError("xclip (or xsel) not found. Install it: sudo apt install xclip")
        return "x11", None

    problem = None
    if os.environ.get("WAYLAND_DISPLAY"):
        try:
            WaylandClipboard().close()
            return "wayland", None
        except BackendError as e:
            problem = str(e)
    if os.environ.get("DISPLAY"):
        if x11_tool():
            return "x11", None
        problem = (problem + " | " if problem else "") + \
            "xclip (or xsel) not found. Install it: sudo apt install xclip"
    return "tty", problem


def _osc52_capable():
    term = os.environ.get("TERM", "")
    return term not in ("", "linux", "dumb") and os.path.exists("/dev/tty")


def _tty_osc52(text):
    seq = "\033]52;c;" + base64.b64encode(text.encode("utf-8")).decode("ascii") + "\a"
    if os.environ.get("TMUX"):
        with contextlib.suppress(Exception):
            subprocess.run(["tmux", "load-buffer", "-"], input=text.encode("utf-8"),
                           timeout=2, stderr=subprocess.DEVNULL)
        seq = "\033Ptmux;" + seq.replace("\033", "\033\033") + "\033\\"
    with open("/dev/tty", "w") as tty:
        tty.write(seq)
        tty.flush()


def _tty_inject(text):
    """Type text into the controlling terminal's input queue (TIOCSTI).
    Only for a single clean line: injecting newlines/control chars would run commands."""
    import termios
    if not text or any(unicodedata.category(c) in ("Cc", "Cf", "Zl", "Zp") for c in text):
        return False
    try:
        fd = os.open("/dev/tty", os.O_RDWR)
    except OSError:
        return False
    try:
        for b in text.encode("utf-8"):
            fcntl.ioctl(fd, termios.TIOCSTI, bytes([b]))
        return True
    except OSError:  # kernel >= 6.2 may disable it (dev.tty.legacy_tiocsti)
        return False
    finally:
        os.close(fd)


def tty_deliver(text):
    if _osc52_capable() and len(text) <= 70000:
        try:
            _tty_osc52(text)
            eprint("suchi: sent to the terminal clipboard (OSC 52; needs terminal support)")
            return
        except OSError:
            pass
    if os.environ.get("TERM") == "linux" and _tty_inject(text):
        return
    if sys.stdout.isatty():
        eprint("suchi: no clipboard available here; entry printed below "
               "(use `suchi -p` with a shell binding to paste it, see --help)")
        print(display_text(text))
    else:
        sys.stdout.write(text)
        sys.stdout.flush()


def deliver(text, args):
    """Put text on the best available clipboard."""
    backend, problem = detect_backend(args.backend)
    try:
        if backend == "wayland":
            return wayland_copy(text, args.selection)
        if backend == "x11":
            return x11_write(text, args.selection)
    except BackendError as e:
        eprint(f"suchi: {e}")
    if backend == "tty" and problem:
        eprint(f"suchi: {problem}")
    tty_deliver(text)


# ════════════════════════════════════════════════════════════════════════════
# TUI COMPONENTS
# ════════════════════════════════════════════════════════════════════════════

GLYPHS_UNICODE = {"arrow": "➜", "pin": "★", "rule": "━", "ml": " ↵ ", "dot": "·"}
GLYPHS_ASCII = {"arrow": ">", "pin": "*", "rule": "-", "ml": " ~ ", "dot": "-"}

DISPLAY_CAP = 20000  # only the first chars of an entry are shown / searched

_WS_RE = re.compile(r"\s+")
# Control chars (incl. ESC), line/paragraph separators -> whitespace
_CTRL_RE = re.compile("[\x00-\x1f\x7f-\x9f\u2028\u2029]")
# Zero-width / bidi / format characters (unicode category Cf) -> dropped
_INVIS_RE = re.compile("[\u00ad\u061c\u180e\u200b-\u200f\u202a-\u202e\u2060-\u2064"
                       "\u2066-\u206f\ufeff\ufff9-\ufffb\U000e0001\U000e0020-\U000e007f]")


def char_width(ch):
    if unicodedata.combining(ch):
        return 0
    return 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1


def swidth(s):
    return sum(char_width(c) for c in s)


def display_text(text):
    """One terminal-safe line for an entry. Control characters (incl. ESC) can
    never reach the terminal. Used for BOTH searching and drawing so match
    indices always line up."""
    text = _INVIS_RE.sub("", text[:DISPLAY_CAP])
    return _WS_RE.sub(" ", _CTRL_RE.sub(" ", text)).strip()


def _lower(s):
    low = s.lower()
    if len(low) == len(s):
        return low
    # str.lower() can change length ('İ'); keep 1:1 so match indices stay valid
    return "".join(c.lower() if len(c.lower()) == 1 else c for c in s)


class Entry:
    __slots__ = ("id", "pinned", "ts", "multiline", "disp", "low")

    def __init__(self, row):
        self.id, head, self.pinned, self.ts = row
        self.multiline = "\n" in head
        self.disp = display_text(head)
        self.low = _lower(self.disp)


class AppState:
    def __init__(self):
        self.query = ""
        self.sel_idx = 0
        self.start_idx = 0
        self.list_h = 10          # rows in the list; set by draw_ui, used for PgUp/PgDn
        self.running = True
        self.copy_item = None
        self.fuzzy_search = False
        self.show_shortcuts = False
        self.show_preview = True
        self.mode = "insert"      # "insert" (type to search) or "normal" (vim-style keys)
        self.delete_item = None
        self.undo_item = None
        self.pin_item = None
        self.toggle_rec = None
        self.flash_msg = ""       # short transient status message (e.g. "Deleted")


def get_filtered_data(query, entries, fuzzy=False):
    """Returns [(entry, set_of_matching_indices)].
    Plain search is word based: every space-separated word has to appear
    somewhere in the entry, in any order. Fuzzy search matches the letters in order."""
    if not query.strip():
        return [(e, frozenset()) for e in entries]
    result = []
    if fuzzy:
        q = _lower(query).replace(" ", "")
        for e in entries:
            pos, idxs = 0, []
            for ch in q:
                pos = e.low.find(ch, pos)
                if pos == -1:
                    break
                idxs.append(pos)
                pos += 1
            else:
                result.append((e, frozenset(idxs)))
    else:
        words = _lower(query).split()
        for e in entries:
            idxs = set()
            for w in words:
                i = e.low.find(w)
                if i == -1:
                    break
                idxs.update(range(i, i + len(w)))
            else:
                result.append((e, frozenset(idxs)))
    return result


def get_relative_time(ts_ms):
    if not ts_ms:
        return ""
    diff = time.time() - (ts_ms / 1000.0)
    if diff < 60: return "just now"
    if diff < 3600: return f"{int(diff / 60)}m ago"
    if diff < 86400: return f"{int(diff / 3600)}h ago"
    return f"{int(diff / 86400)}d ago"


def safe_addstr(stdscr, y, x, text, attr=0):
    try:
        stdscr.addstr(y, x, text, attr)
    except curses.error:
        pass


def init_theme():
    """Curses attributes by role; degrades to plain attributes without colour."""
    T = {
        "sel": curses.A_REVERSE, "head": curses.A_BOLD, "pin": curses.A_BOLD,
        "match": curses.A_UNDERLINE | curses.A_BOLD,
        "sel_match": curses.A_REVERSE | curses.A_BOLD | curses.A_UNDERLINE,
        "mode_ins": curses.A_REVERSE | curses.A_BOLD,
        "mode_norm": curses.A_REVERSE | curses.A_BOLD,
        "ok": curses.A_BOLD, "warn": curses.A_BOLD | curses.A_REVERSE,
    }
    try:
        if curses.has_colors():
            curses.use_default_colors()
            curses.init_pair(1, curses.COLOR_BLACK, curses.COLOR_CYAN)
            curses.init_pair(2, curses.COLOR_CYAN, -1)
            curses.init_pair(3, curses.COLOR_YELLOW, -1)
            curses.init_pair(4, curses.COLOR_GREEN, -1)
            curses.init_pair(5, curses.COLOR_YELLOW, curses.COLOR_CYAN)
            T.update({
                "sel": curses.color_pair(1),
                "head": curses.color_pair(2) | curses.A_BOLD,
                "pin": curses.color_pair(3),
                "match": curses.color_pair(4) | curses.A_BOLD,
                "sel_match": curses.color_pair(5) | curses.A_BOLD,
                "mode_ins": curses.color_pair(1) | curses.A_BOLD,
                "mode_norm": curses.color_pair(3) | curses.A_BOLD | curses.A_REVERSE,
                "ok": curses.color_pair(4) | curses.A_BOLD,
                "warn": curses.color_pair(3) | curses.A_BOLD | curses.A_REVERSE,
            })
    except curses.error:
        pass
    return T


def read_key(stdscr):
    """Returns a printable str, an int key code, or None on timeout."""
    try:
        k = stdscr.get_wch()
    except curses.error:
        return None
    if isinstance(k, str):
        if len(k) == 1 and k.isprintable():
            return k
        return ord(k) if len(k) == 1 else None
    return k


ENTER_KEYS = (curses.KEY_ENTER, 10, 13)
BACKSPACE_KEYS = (curses.KEY_BACKSPACE, 127, 8)


def _move(state, delta, filtered_len):
    if filtered_len:
        state.sel_idx = max(0, min(filtered_len - 1, state.sel_idx + delta))


def handle_input(key, stdscr, state, filtered_len):
    state.flash_msg = ""  # any keypress clears the previous transient status message

    if key == curses.KEY_RESIZE:
        if hasattr(curses, "update_lines_cols"):
            curses.update_lines_cols()
        return

    if state.show_shortcuts:  # any key just closes the help overlay
        state.show_shortcuts = False
        return

    # keys that work the same in both modes
    page = max(1, state.list_h)
    if key == 9:  # Tab
        state.show_preview = not state.show_preview
        return
    if key == curses.KEY_F1:
        state.show_shortcuts = True
        return
    if key in (curses.KEY_DOWN, 14):  # Down / Ctrl-N
        _move(state, 1, filtered_len)
        return
    if key in (curses.KEY_UP, 16):  # Up / Ctrl-P
        _move(state, -1, filtered_len)
        return
    if key == curses.KEY_NPAGE:
        _move(state, page, filtered_len)
        return
    if key == curses.KEY_PPAGE:
        _move(state, -page, filtered_len)
        return
    if key == curses.KEY_HOME:
        state.sel_idx = 0
        return
    if key == curses.KEY_END:
        if filtered_len > 0:
            state.sel_idx = filtered_len - 1
        return

    # INSERT MODE: typing edits the search query. ESC returns to normal mode.
    if state.mode == "insert":
        if key == 27:
            state.mode = "normal"
        elif key in ENTER_KEYS:
            if filtered_len > 0:
                state.copy_item = True
                state.running = False
        elif key in BACKSPACE_KEYS:
            state.query = state.query[:-1]
            state.sel_idx = state.start_idx = 0
        elif key == 21:  # Ctrl-U clears the query
            state.query = ""
            state.sel_idx = state.start_idx = 0
        elif key == 23:  # Ctrl-W deletes the last word
            state.query = state.query.rstrip().rsplit(" ", 1)[0] if " " in state.query.rstrip() else ""
            state.sel_idx = state.start_idx = 0
        elif key == 6:  # Ctrl-F toggles fuzzy matching
            state.fuzzy_search = not state.fuzzy_search
            state.sel_idx = state.start_idx = 0
        elif isinstance(key, str):
            state.query += key
            state.sel_idx = state.start_idx = 0
        return

    # NORMAL MODE: vim-style single-key commands.
    if key in (27, "q"):
        state.running = False
    elif key in ("i", "/"):
        state.mode = "insert"
    elif key in ("j", "k"):
        _move(state, 1 if key == "j" else -1, filtered_len)
    elif key == "f":
        state.fuzzy_search = not state.fuzzy_search
        state.sel_idx = state.start_idx = 0
    elif key == "y" or key in ENTER_KEYS:
        if filtered_len > 0:
            state.copy_item = True
            state.running = False
    elif isinstance(key, str) and key in "123456789":  # copy the numbered entry right away
        if int(key) <= filtered_len:
            state.sel_idx = int(key) - 1
            state.copy_item = True
            state.running = False
    elif key == "d":
        if filtered_len > 0:
            state.delete_item = True
    elif key == "u":
        state.undo_item = True
    elif key == "p":
        if filtered_len > 0:
            state.pin_item = True
    elif key == "r":
        state.toggle_rec = True
    elif key == "?":
        state.show_shortcuts = True
    elif key == "g":
        state.sel_idx = 0
    elif key == "G":
        if filtered_len > 0:
            state.sel_idx = filtered_len - 1


def build_cells(disp, matches, max_cols):
    """Split text into drawable cells that fit max_cols terminal columns."""
    cells, used, i = [], 0, 0
    n = min(len(disp), max_cols * 2 + 8)  # never walk a huge entry
    while i < n:
        ch = disp[i]
        w = char_width(ch)
        if w == 0:
            if cells:  # combining mark: attach to previous cell
                cells[-1][0] += ch
                cells[-1][2] = cells[-1][2] or (i in matches)
            i += 1
            continue
        if used + w > max_cols:
            break
        cells.append([ch, w, i in matches])
        used += w
        i += 1
    if i < len(disp) and max_cols > 3:  # truncated: make room for "..."
        while cells and used + 3 > max_cols:
            used -= cells.pop()[1]
        cells.append(["...", 3, False])
    return cells


def _clip(s, cols):
    """Cut s to at most cols terminal columns."""
    out, w = [], 0
    for ch in s:
        cw = char_width(ch)
        if w + cw > cols:
            break
        out.append(ch)
        w += cw
    return "".join(out)


def wrap_rows(text, width, max_rows):
    """Hard-wrap text into at most max_rows rows of <= width columns, with control
    characters neutralised. Returns (rows, truncated)."""
    rows = []
    lines = text.split("\n")
    for li, raw in enumerate(lines):
        line = _INVIS_RE.sub("", _CTRL_RE.sub(" ", raw.replace("\t", "    ")))
        cur, w = "", 0
        for ch in line:
            cw = char_width(ch)
            if w + cw > width:
                rows.append(cur)
                cur, w = "", 0
                if len(rows) >= max_rows:
                    return rows, True
            cur += ch
            w += cw
        rows.append(cur)
        if len(rows) >= max_rows:
            return rows, li < len(lines) - 1
    return rows, False


def preview_height(state, height):
    """Rows for the preview pane (0 = hidden, also on short terminals)."""
    if not state.show_preview or height < 14:
        return 0
    return min(8, (height - 4) // 3)


def draw_row(stdscr, y, width, entry, matches, selected, rank, G, T):
    pinned = bool(entry.pinned)
    ml = G["ml"] if entry.multiline else ""
    rel = get_relative_time(entry.ts)
    time_w = len(rel) + 2 if (rel and width > 45) else 0
    r = str(rank) if rank <= 9 else " "  # 1-9 copy that entry in normal mode

    if selected:
        safe_addstr(stdscr, y, 0, " " * (width - 1), T["sel"])
        safe_addstr(stdscr, y, 0, f" {r} {G['arrow']} {G['pin'] if pinned else ' '} ", T["sel"])
    else:
        safe_addstr(stdscr, y, 1, r, curses.A_DIM)
        safe_addstr(stdscr, y, 5, G["pin"] if pinned else " ", T["pin"])
    x = 7

    max_cols = max(0, width - x - time_w - swidth(ml) - 1)
    runs = []  # merge neighbouring cells that share an attribute
    for s, w, m in build_cells(entry.disp, matches, max_cols):
        attr = (T["sel_match"] if selected else T["match"]) if m else (T["sel"] if selected else 0)
        if runs and runs[-1][1] == attr:
            runs[-1][0] += s
        else:
            runs.append([s, attr])
    for s, attr in runs:
        safe_addstr(stdscr, y, x, s, attr)
        x += swidth(s)
    if ml:
        safe_addstr(stdscr, y, x, ml, T["sel"] if selected else curses.A_DIM)
    if time_w:
        safe_addstr(stdscr, y, width - time_w - 1, rel, T["sel"] if selected else curses.A_DIM)


def draw_preview(stdscr, y, rows_avail, width, preview, G, T):
    """Separator line (with size info) at y, then the wrapped entry text below it."""
    text, nchars = preview if preview else ("", 0)
    if preview:
        lines = text.count("\n") + 1
        more = nchars > len(text)
        info = f" {lines}{'+' if more else ''} line{'s' if lines != 1 else ''} {G['dot']} {nchars} chars "
        line = G["rule"] * 3 + info
    else:
        line = ""
    line += G["rule"] * max(0, width - 1 - swidth(line))
    safe_addstr(stdscr, y, 0, _clip(line, width - 1), T["head"])
    if not preview:
        return
    cols = max(1, width - 5)
    rows, cut = wrap_rows(text, cols, rows_avail)
    if (cut or nchars > len(text)) and rows:
        rows[-1] = _clip(rows[-1], cols - 4) + " ..."
    for i, row in enumerate(rows[:rows_avail]):
        safe_addstr(stdscr, y + 1 + i, 3, row)


def draw_ui(stdscr, state, filtered, height, width, G, T, preview=None, daemon_state="off"):
    if height < 4 or width < 15:
        safe_addstr(stdscr, 0, 0, "Term too small")
        stdscr.refresh()
        return

    ph = preview_height(state, height)
    list_h = height - 3 - ((ph + 1) if ph else 0)
    state.list_h = list_h

    if state.sel_idx >= len(filtered) and len(filtered) > 0:
        state.sel_idx = len(filtered) - 1
    elif len(filtered) == 0:
        state.sel_idx = 0

    if state.sel_idx < state.start_idx:
        state.start_idx = state.sel_idx
    elif state.sel_idx >= state.start_idx + list_h:
        state.start_idx = state.sel_idx - list_h + 1

    # ---- header: mode tag, search text, recorder status ----
    if state.mode == "insert":
        mode_tag, mode_attr = " INSERT ", T["mode_ins"]
    else:
        mode_tag, mode_attr = " NORMAL ", T["mode_norm"]
    safe_addstr(stdscr, 0, 0, mode_tag, mode_attr)

    tag_w = len(mode_tag)
    status, status_attr = {"rec": (" REC ", T["ok"]), "paused": (" PAUSED ", T["warn"])}.get(
        daemon_state, (" NO DAEMON ", curses.A_DIM))
    if width < 50:
        status = ""
    status_w = len(status)
    header_text = f" SEARCH: {state.query}" + ("_" if state.mode == "insert" else "")
    if state.flash_msg:
        header_text = f" {state.flash_msg}"
    avail = max(0, width - 1 - tag_w - status_w)
    # keep the tail of a long query visible, count columns not characters
    while swidth(header_text) > avail and len(header_text) > 1:
        header_text = header_text[1:] if not state.flash_msg else header_text[:-1]
    safe_addstr(stdscr, 0, tag_w, header_text.ljust(avail), T["head"])
    if status:
        safe_addstr(stdscr, 0, width - 1 - status_w, status, status_attr)
    safe_addstr(stdscr, 1, 0, G["rule"] * (width - 1), T["head"])

    # ---- list ----
    if not filtered:
        if state.query:
            msg = "No matches"
        elif daemon_state == "off":
            msg = "History is empty - start recording with `suchi --demon` (or `suchi --install-service`)"
        else:
            msg = "History is empty - copy something and it will show up here"
        safe_addstr(stdscr, 2, 3, msg[: width - 4], curses.A_DIM)

    for i, (entry, matches) in enumerate(filtered[state.start_idx: state.start_idx + list_h]):
        pos = state.start_idx + i
        draw_row(stdscr, i + 2, width, entry, matches, pos == state.sel_idx, pos + 1, G, T)

    # ---- preview pane ----
    if ph:
        draw_preview(stdscr, 2 + list_h, ph, width, preview if filtered else None, G, T)

    # ---- footer ----
    fuzzy_status = "ON" if state.fuzzy_search else "OFF"
    if state.mode == "insert":
        footer = (f" {len(filtered)} items | Fuzzy: {fuzzy_status} | Enter: Copy  Esc: Normal mode  "
                  "Ctrl-F: Fuzzy  Tab: Preview  F1: Help ")
    else:
        footer = (f" {len(filtered)} items | Fuzzy: {fuzzy_status} | i: Search  j/k: Move  y: Copy  "
                  "1-9: Quick copy  d: Delete  u: Undo  p: Pin  Tab: Preview  ?: Help ")
    safe_addstr(stdscr, height - 1, 0, footer.center(width - 1)[: width - 1], curses.A_DIM)

    if state.show_shortcuts:
        draw_shortcuts_overlay(stdscr, height, width, T)

    stdscr.refresh()


def draw_shortcuts_overlay(stdscr, height, width, T):
    """Draws a centered popup listing every keybinding."""
    lines = [
        ("Search mode (default)", True),
        ("  type          filter: every word must match, any order", False),
        ("  Enter         copy highlighted entry", False),
        ("  Up/Down       move (also Ctrl-N / Ctrl-P)", False),
        ("  Ctrl-F        toggle fuzzy matching", False),
        ("  Ctrl-U / W    clear query / delete last word", False),
        ("  Esc           switch to normal mode", False),
        ("Normal mode", True),
        ("  i or /        back to search", False),
        ("  j / k  g / G  move down / up, first / last", False),
        ("  y / Enter     copy highlighted entry", False),
        ("  1-9           copy the numbered entry at once", False),
        ("  d / u         delete / undo delete", False),
        ("  p             pin / unpin (pinned stay on top)", False),
        ("  r             pause / resume recording", False),
        ("  f  q / Esc    toggle fuzzy  /  quit", False),
        ("Both modes", True),
        ("  Tab           show / hide the preview pane", False),
        ("  PgUp PgDn Home End   jump around", False),
        ("  ? or F1       this help (any key closes it)", False),
    ]
    box_w = min(width - 4, max(len(t) for t, _ in lines) + 6)
    box_h = min(height - 2, len(lines) + 2)
    if box_w < 10 or box_h < 3:
        return
    start_y = max(0, (height - box_h) // 2)
    start_x = max(0, (width - box_w) // 2)

    for row in range(box_h):
        safe_addstr(stdscr, start_y + row, start_x, " " * box_w, T["sel"])
    for i, (text, emphasize) in enumerate(lines):
        if i >= box_h - 2:
            break
        attr = T["sel"] | curses.A_BOLD if emphasize else T["sel"]
        safe_addstr(stdscr, start_y + 1 + i, start_x, ("  " + text).ljust(box_w)[:box_w], attr)


def _daemon_state(path):
    if daemon_status(path) is None:
        return "off"
    return "paused" if is_paused(path) else "rec"


def tui_main(stdscr, db, path, ascii_mode=False, start_normal=False, initial_query=""):
    """Runs the TUI; returns the id of the chosen entry, or None."""
    T = init_theme()
    G = GLYPHS_ASCII if ascii_mode else GLYPHS_UNICODE
    with contextlib.suppress(curses.error):
        curses.curs_set(0)

    state = AppState()
    state.query = initial_query
    if start_normal:
        state.mode = "normal"
    last_ver = None
    last_minute = int(time.time() // 60)
    last_probe = time.monotonic()
    daemon_state = _daemon_state(path)
    entries, filtered = [], []
    undo_stack = []   # (text, pinned, ts) of deleted entries, newest last
    previews = {}     # id -> (head, length), cleared whenever the data reloads
    needs_redraw = True

    def select_id(clip_id):
        for i, (e, _) in enumerate(filtered):
            if e.id == clip_id:
                state.sel_idx = i
                return True
        return False

    def reload(track=True):
        """Re-read the history; with track=True the highlighted entry stays highlighted
        even when new entries arrive above it."""
        nonlocal entries, filtered
        keep = filtered[state.sel_idx][0].id if track and 0 <= state.sel_idx < len(filtered) else None
        entries = [Entry(r) for r in load_rows(db)]
        filtered = get_filtered_data(state.query, entries, state.fuzzy_search)
        previews.clear()
        if keep is not None:
            select_id(keep)

    def refilter():
        nonlocal filtered
        filtered = get_filtered_data(state.query, entries, state.fuzzy_search)

    while state.running and not _stop_requested:
        try:
            # data_version changes whenever ANOTHER connection (the daemon) commits
            ver = db.execute("PRAGMA data_version").fetchone()[0]
            if ver != last_ver:
                reload()
                last_ver = ver
                needs_redraw = True
        except sqlite3.Error:
            pass  # briefly locked; try again on the next tick
        minute = int(time.time() // 60)
        if minute != last_minute:  # keep "5m ago" labels fresh
            last_minute = minute
            needs_redraw = True
        if time.monotonic() - last_probe > 2:  # is the recorder still alive?
            last_probe = time.monotonic()
            new_state = _daemon_state(path)
            if new_state != daemon_state:
                daemon_state = new_state
                needs_redraw = True

        if needs_redraw:
            stdscr.erase()
            height, width = stdscr.getmaxyx()
            preview = None
            if state.show_preview and filtered and height >= 14:
                entry = filtered[min(state.sel_idx, len(filtered) - 1)][0]
                if entry.id not in previews:
                    previews.clear()
                    try:
                        previews[entry.id] = get_preview(db, entry.id)
                    except sqlite3.Error:
                        pass
                preview = previews.get(entry.id)
            draw_ui(stdscr, state, filtered, height, width, G, T, preview, daemon_state)
            needs_redraw = False

        stdscr.timeout(200)
        key = read_key(stdscr)
        if key is None:
            continue

        needs_redraw = True
        old = (state.query, state.fuzzy_search)
        handle_input(key, stdscr, state, len(filtered))
        if state.running and (state.query, state.fuzzy_search) != old:
            refilter()

        if state.delete_item and filtered:
            target = filtered[state.sel_idx][0].id
            state.delete_item = None
            try:
                row = delete_clip(db, target)
                if row:
                    undo_stack.append(row)
                    del undo_stack[:-20]
                state.flash_msg = "Entry deleted (u: undo)"
            except sqlite3.Error:
                state.flash_msg = "Delete failed"
            reload(track=False)
            state.sel_idx = min(state.sel_idx, max(0, len(filtered) - 1))

        if state.undo_item:
            state.undo_item = None
            if not undo_stack:
                state.flash_msg = "Nothing to undo"
            else:
                try:
                    new_id = restore_clip(db, *undo_stack.pop())
                    reload(track=False)
                    select_id(new_id)
                    state.flash_msg = "Entry restored"
                except sqlite3.Error:
                    state.flash_msg = "Undo failed"

        if state.pin_item and filtered:
            target = filtered[state.sel_idx][0].id
            state.pin_item = None
            try:
                toggle_pin(db, target)
            except sqlite3.Error:
                state.flash_msg = "Pin failed"
            reload(track=False)
            for i, (e, _) in enumerate(filtered):
                if e.id == target:
                    state.sel_idx = i
                    if not state.flash_msg:
                        state.flash_msg = "Pinned" if e.pinned else "Unpinned"
                    break

        if state.toggle_rec:
            state.toggle_rec = None
            try:
                paused = not is_paused(path)
                set_paused(path, paused)
                state.flash_msg = "Recording paused" if paused else "Recording resumed"
            except OSError:
                state.flash_msg = "Could not change the recording state"
            daemon_state = _daemon_state(path)

        if state.copy_item and filtered:
            return filtered[state.sel_idx][0].id
        state.copy_item = None
    return None


@contextlib.contextmanager
def _tty_stdio():
    """Let the TUI use the real terminal even when stdin/stdout are pipes, so
    `$(suchi -p)` and `suchi < file` keep working."""
    saved = {}
    tty = None
    try:
        if not (os.isatty(0) and os.isatty(1)):
            try:
                tty = os.open("/dev/tty", os.O_RDWR)
            except OSError:
                raise BackendError("suchi's TUI needs a terminal (no /dev/tty available)")
            for fd in (0, 1):
                if not os.isatty(fd):
                    saved[fd] = os.dup(fd)
                    os.dup2(tty, fd)
        yield
    finally:
        sys.stdout.flush()
        for fd, old in saved.items():
            os.dup2(old, fd)
            os.close(old)
        if tty is not None:
            os.close(tty)


_stop_requested = []  # filled by the SIGTERM handler: "quit, a newer suchi wants the screen"


def _request_stop(*_):
    _stop_requested.append(1)


def _tui_pid_path(history_path):
    return history_path + ".tui.pid"


def take_over_tui_slot(history_path, timeout=3.0):
    """Become the only running TUI. If another one holds the slot it is asked to quit
    (SIGTERM, so it restores its terminal and its window closes) and we wait for it;
    one that ignores that for `timeout` seconds gets SIGKILL. Returns the lock fd, which
    the caller keeps open for as long as the TUI runs.
    Only a process that really holds the lock is ever signalled, so a stale pid file
    can never make us kill an unrelated process."""
    _ensure_dir(history_path)
    fd = os.open(_tui_pid_path(history_path), os.O_RDWR | os.O_CREAT, 0o600)
    deadline = time.monotonic() + timeout
    signalled, killed = set(), False
    try:
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except OSError:
                pass
            raw = os.pread(fd, 32, 0).decode(errors="ignore").strip()
            pid = int(raw) if raw.isdigit() else 0
            if pid > 0 and pid != os.getpid() and pid not in signalled:
                signalled.add(pid)
                with contextlib.suppress(OSError):
                    os.kill(pid, signal.SIGTERM)
            if time.monotonic() > deadline:
                if killed or pid <= 0:
                    raise BackendError("another suchi window does not want to close "
                                       f"(pid {pid or '?'}); use --allow-multiple to open anyway")
                killed = True
                deadline = time.monotonic() + 1.0
                with contextlib.suppress(OSError):
                    os.kill(pid, signal.SIGKILL)
            time.sleep(0.05)
        os.ftruncate(fd, 0)
        os.pwrite(fd, str(os.getpid()).encode(), 0)
    except BaseException:
        os.close(fd)
        raise
    return fd


def run_tui(args):
    with contextlib.suppress(locale.Error):
        locale.setlocale(locale.LC_ALL, "")
    enc = (locale.getpreferredencoding(False) or "").lower()
    ascii_mode = args.ascii or os.environ.get("TERM") == "linux" or "utf" not in enc

    # SIGTERM = "quit politely" (restores the terminal). Installed before taking the
    # slot, so a window that is replaced while it is still starting up exits cleanly too.
    old_handler = signal.signal(signal.SIGTERM, _request_stop)
    slot = None
    try:
        if not args.allow_multiple:
            slot = take_over_tui_slot(args.file)
        with _tty_stdio(), contextlib.closing(open_db(args.file)) as db:
            return curses.wrapper(tui_main, db, args.file, ascii_mode, args.normal, args.query or "")
    finally:
        if slot is not None:
            os.close(slot)  # released BEFORE deliver() forks, so a copy helper never keeps the slot
        signal.signal(signal.SIGTERM, old_handler)


# ════════════════════════════════════════════════════════════════════════════
# DAEMON LOGIC
# ════════════════════════════════════════════════════════════════════════════

def daemonize(log_path):
    """Double-fork daemonization process to safely detach."""
    sys.stdout.flush()
    sys.stderr.flush()
    try:
        if os.fork() > 0:
            os._exit(0)  # _exit: don't flush the inherited stdio buffers twice
    except OSError as e:
        raise BackendError(f"Fork #1 failed: {e}")

    os.chdir("/")
    os.setsid()
    os.umask(0o077)  # history holds secrets: never world-readable

    try:
        if os.fork() > 0:
            os._exit(0)
    except OSError as e:
        raise BackendError(f"Fork #2 failed: {e}")

    dn = os.open(os.devnull, os.O_RDWR)
    os.dup2(dn, 0)
    os.dup2(dn, 1)
    log_fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    os.dup2(log_fd, 2)  # errors end up in the log instead of vanishing
    sys.stderr = os.fdopen(2, "w", buffering=1, closefd=False)


def _pid_path(history_path):
    return history_path + ".pid"


def acquire_daemon_lock(history_path):
    """Single-instance guard: an flock held for the daemon's whole life."""
    _ensure_dir(history_path)
    fd = os.open(_pid_path(history_path), os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        pid = os.read(fd, 32).decode(errors="ignore").strip()
        os.close(fd)
        raise BackendError(f"daemon is already running (pid {pid or '?'}). Stop it with: suchi --stop")
    return fd


def stop_daemon(history_path):
    pidfile = _pid_path(history_path)
    try:
        fd = os.open(pidfile, os.O_RDWR)
    except OSError:
        print("suchi daemon is not running")
        return 0
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            print("suchi daemon is not running")  # we got the lock, nobody holds it
            return 0
        except OSError:
            pass
        try:
            pid = int(os.read(fd, 32).decode().strip())
        except ValueError:
            raise BackendError("pid file is unreadable")
        os.kill(pid, signal.SIGTERM)
        for _ in range(40):
            time.sleep(0.1)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                print(f"suchi daemon stopped (pid {pid})")
                return 0
            except OSError:
                continue
        raise BackendError(f"sent SIGTERM to {pid} but it is still running")
    finally:
        os.close(fd)


def _paused_path(history_path):
    return history_path + ".paused"


def is_paused(history_path):
    return os.path.exists(_paused_path(history_path))


def set_paused(history_path, paused):
    """Pausing is just a flag file the daemon checks, so it works from anywhere."""
    p = _paused_path(history_path)
    if paused:
        _ensure_dir(history_path)
        os.close(os.open(p, os.O_WRONLY | os.O_CREAT, 0o600))
    else:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(p)


def daemon_status(history_path):
    """The daemon's pid if it is running (-1 if running but pid unknown), else None.
    Uses the same flock the daemon holds for its whole life."""
    try:
        fd = os.open(_pid_path(history_path), os.O_RDONLY)
    except OSError:
        return None
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except OSError:  # the daemon holds the exclusive lock
            pid = os.read(fd, 32).decode(errors="ignore").strip()
            return int(pid) if pid.isdigit() else -1
        return None  # lock was free: nobody is running
    finally:
        os.close(fd)


def run_daemon(args, backend, lock_fd):
    """Persistently watch the clipboard and record changes."""
    os.ftruncate(lock_fd, 0)
    os.write(lock_fd, str(os.getpid()).encode())

    stopping = []
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, lambda *_: stopping.append(1))
    stop = lambda: bool(stopping)

    db = open_db(args.file)  # opened here, after daemonize(), never across a fork

    def on_text(text):
        if is_paused(args.file):
            return
        try:
            add_clip(db, text, args.limit, args.max_size)
        except Exception as e:  # keep watching even if one write fails
            eprint(f"suchi: could not save entry: {e}")

    try:
        if backend == "wayland":
            wl = WaylandClipboard()
            try:
                wl.watch(args.selection, on_text, stop, args.max_size)
            finally:
                wl.close()
        else:
            watch_x11(args.selection, on_text, stop)
    finally:
        db.close()


# ════════════════════════════════════════════════════════════════════════════
# COMMANDS (non-interactive)
# ════════════════════════════════════════════════════════════════════════════

SERVICE_NAME = "suchi.service"


def _service_path():
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~/.config")
    return os.path.join(base, "systemd", "user", SERVICE_NAME)


def _unit_quote(s):
    """Quote one ExecStart word for a systemd unit file."""
    s = s.replace("%", "%%")
    if not s or any(c.isspace() or c in "\"'\\" for c in s):
        s = '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'
    return s


def _systemctl(*a):
    r = subprocess.run(["systemctl", "--user", *a], stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, universal_newlines=True)
    return r.returncode, (r.stdout + r.stderr).strip()


def _fmt_size(n):
    for unit in ("B", "KB", "MB"):
        if n < 1024:
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} GB"


def cmd_status(args):
    pid = daemon_status(args.file)
    with contextlib.closing(open_db(args.file)) as db:
        total, pinned = count_clips(db)
    try:
        backend, problem = detect_backend(args.backend)
    except BackendError as e:
        backend, problem = "unavailable", str(e)
    try:
        size = _fmt_size(os.path.getsize(args.file))
    except OSError:
        size = "?"
    if pid is None:
        daemon = "not running  (start it: suchi --demon, or suchi --install-service)"
    else:
        daemon = "running" + (f" (pid {pid})" if pid > 0 else "")
    print(f"suchi {__version__}")
    print(f"  database : {args.file}  ({total} entries, {pinned} pinned, {size})")
    print(f"  daemon   : {daemon}")
    print(f"  recording: {'PAUSED  (suchi --resume to continue)' if is_paused(args.file) else 'on'}")
    print(f"  backend  : {backend}")
    if problem:
        print(f"             note: {problem}")
    print(f"  service  : {'installed' if os.path.exists(_service_path()) else 'not installed  (suchi --install-service)'}")
    return 0


def cmd_pause(args, paused):
    set_paused(args.file, paused)
    print("recording paused - resume with: suchi --resume" if paused else "recording resumed")
    if daemon_status(args.file) is None:
        eprint("suchi: note: the daemon is not running, so nothing is being recorded anyway")
    return 0


def _filtered_entries(db, query):
    entries = [Entry(r) for r in load_rows(db)]
    return get_filtered_data(query or "", entries)


def cmd_list(args):
    with contextlib.closing(open_db(args.file)) as db:
        rows = _filtered_entries(db, args.query)
        shown = rows[: args.list] if args.list else rows  # 0 means all
        if args.json:
            out = []
            for n, (e, _) in enumerate(shown, 1):
                out.append({"n": n, "id": e.id, "pinned": bool(e.pinned), "time_ms": e.ts,
                            "text": get_text(db, e.id) or ""})
            json.dump(out, sys.stdout, ensure_ascii=False, indent=2)
            sys.stdout.write("\n")
            return 0
    if not shown:
        eprint("suchi: no matching entries" if args.query else "suchi: history is empty")
        return 0
    cols = shutil.get_terminal_size((100, 24)).columns if sys.stdout.isatty() else None
    for n, (e, _) in enumerate(shown, 1):
        prefix = f"{n:>4}  {get_relative_time(e.ts):>8}  {'*' if e.pinned else ' '}  "
        text = e.disp
        if cols and swidth(prefix + text) > cols - 1:
            text = "".join(c[0] for c in build_cells(text, frozenset(), max(4, cols - 1 - len(prefix))))
        print(prefix + text)
    if len(shown) < len(rows):
        eprint(f"suchi: showing {len(shown)} of {len(rows)} (use -l 0 for all)")
    return 0


def cmd_get(args):
    with contextlib.closing(open_db(args.file)) as db:
        rows = _filtered_entries(db, args.query)
        if not 1 <= args.get <= len(rows):
            eprint(f"suchi: no entry #{args.get} (there are {len(rows)})")
            return 1
        text = get_text(db, rows[args.get - 1][0].id)
    if text is None:
        eprint("suchi: that entry no longer exists")
        return 1
    sys.stdout.write(text)
    if sys.stdout.isatty() and not text.endswith("\n"):
        sys.stdout.write("\n")
    sys.stdout.flush()
    return 0


def cmd_clear(args):
    with contextlib.closing(open_db(args.file)) as db:
        total, pinned = count_clips(db)
        n = total if args.include_pinned else total - pinned
        if n == 0:
            print("nothing to delete")
            return 0
        if not args.yes:
            if not sys.stdin.isatty():
                raise BackendError("refusing to delete without a terminal to confirm; pass --yes")
            keep = "" if args.include_pinned else f" (keeping {pinned} pinned)"
            try:
                ans = input(f"Delete {n} entr{'y' if n == 1 else 'ies'}{keep}? [y/N] ")
            except EOFError:
                ans = ""
            if ans.strip().lower() not in ("y", "yes"):
                print("aborted")
                return 1
        clear_clips(db, args.include_pinned)
    print(f"deleted {n} entr{'y' if n == 1 else 'ies'}")
    return 0


def cmd_install_service(args):
    cmd = [sys.executable, os.path.abspath(__file__), "--demon", "--foreground"]
    if args.file != DEFAULT_HISTORY_PATH:
        cmd += ["--file", args.file]
    if args.limit != DEFAULT_LIMIT:
        cmd += ["--limit", str(args.limit)]
    if args.max_size != DEFAULT_MAX_BYTES:
        cmd += ["--max-size", str(args.max_size)]
    if args.selection != "clipboard":
        cmd += ["--selection", args.selection]
    if args.backend != "auto":
        cmd += ["--backend", args.backend]
    unit = ("[Unit]\n"
            "Description=suchi clipboard history recorder\n"
            "PartOf=graphical-session.target\n"
            "After=graphical-session.target\n\n"
            "[Service]\n"
            f"ExecStart={' '.join(_unit_quote(c) for c in cmd)}\n"
            "Restart=on-failure\n"
            "RestartSec=3\n\n"
            "[Install]\n"
            "WantedBy=graphical-session.target\n")
    path = _service_path()
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(unit)
    print(f"wrote {path}")
    if not shutil.which("systemctl"):
        print("systemctl not found: start the recorder yourself with `suchi --demon`")
        return 0
    if daemon_status(args.file) is not None:
        stop_daemon(args.file)  # the service would otherwise fail with "already running"
    steps = [("daemon-reload",)]
    env = [v for v in ("WAYLAND_DISPLAY", "DISPLAY", "XAUTHORITY") if os.environ.get(v)]
    if env:  # the user manager needs to know which display to talk to
        steps.append(("import-environment", *env))
    steps.append(("enable", "--now", SERVICE_NAME))
    for step in steps:
        rc, out = _systemctl(*step)
        if rc != 0:
            eprint(f"suchi: `systemctl --user {' '.join(step)}` failed: {out}")
            eprint("suchi: the unit file is in place; try again from a normal desktop session")
            return 1
    time.sleep(1.0)
    if _systemctl("is-active", "--quiet", SERVICE_NAME)[0] == 0:
        print("enabled and running - suchi now records at every login (check: suchi --status)")
        return 0
    eprint("suchi: the service was enabled but is not running; see: journalctl --user -u suchi -e")
    eprint("suchi: on a desktop it needs WAYLAND_DISPLAY / DISPLAY in the user manager's environment")
    return 1


def cmd_uninstall_service(args):
    path = _service_path()
    if shutil.which("systemctl"):
        _systemctl("disable", "--now", SERVICE_NAME)
    existed = os.path.exists(path)
    with contextlib.suppress(FileNotFoundError):
        os.unlink(path)
    if shutil.which("systemctl"):
        _systemctl("daemon-reload")
    print("service removed" if existed else "service was not installed")
    return 0


# ════════════════════════════════════════════════════════════════════════════
# MAIN ENTRYPOINT
# ════════════════════════════════════════════════════════════════════════════

def _int_min(minimum):
    def conv(s):
        try:
            v = int(s)
        except ValueError:
            raise argparse.ArgumentTypeError(f"invalid integer: {s!r}")
        if v < minimum:
            raise argparse.ArgumentTypeError(f"must be at least {minimum}")
        return v
    return conv


def build_parser():
    parser = argparse.ArgumentParser(
        prog="suchi",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        description="suchi - clipboard history for Wayland, X11 and TTY terminals",
        epilog="""examples:
  suchi --install-service      record in the background at every login
  suchi                        browse and search (Enter copies, ? shows all keys)
  suchi -q ssh                 open with "ssh" already typed into the search
  suchi -l 10                  print the 10 newest entries
  suchi --get 1 | wc -c        raw text of entry #1 (numbers match the TUI)
  echo hi | suchi --add        add text by hand
  suchi --pause                stop recording (e.g. around passwords); --resume to continue

See the top of this file for the shell binding that pastes in a TTY.""")

    g = parser.add_argument_group("recorder (daemon)")
    g.add_argument("--demon", "--daemon", action="store_true", dest="demon",
                   help="run the clipboard watcher as a background daemon")
    g.add_argument("--foreground", action="store_true",
                   help="with --demon: stay in the foreground (for systemd)")
    g.add_argument("--stop", action="store_true", help="stop the background daemon")
    g.add_argument("--pause", action="store_true", help="stop recording until --resume")
    g.add_argument("--resume", action="store_true", help="continue recording")
    g.add_argument("--status", action="store_true", help="show daemon, backend and database info")
    g.add_argument("--install-service", action="store_true",
                   help="install and start a systemd user service (autostart at login)")
    g.add_argument("--uninstall-service", action="store_true", help="remove that service again")

    h = parser.add_argument_group("history")
    h.add_argument("--add", nargs="?", const="-", metavar="TEXT",
                   help="add TEXT (or stdin when omitted / '-') to the history and exit")
    h.add_argument("-l", "--list", nargs="?", const=20, type=_int_min(0), metavar="N",
                   help="print the N newest entries (default 20, 0 = all)")
    h.add_argument("--get", type=_int_min(1), metavar="N",
                   help="print entry number N to stdout, exactly as copied")
    h.add_argument("--json", action="store_true", help="with --list: machine-readable output")
    h.add_argument("-q", "--query", metavar="WORDS",
                   help="search words: pre-fills the TUI, filters --list / --get")
    h.add_argument("--clear", action="store_true", help="delete all entries that are not pinned")
    h.add_argument("--include-pinned", action="store_true", help="with --clear: pinned ones too")
    h.add_argument("-y", "--yes", action="store_true", help="with --clear: do not ask")

    t = parser.add_argument_group("interface")
    t.add_argument("-p", "--print", action="store_true",
                   help="TUI: print the chosen entry to stdout instead of copying")
    t.add_argument("--normal", action="store_true",
                   help="TUI: start in vim-style normal mode instead of search mode")
    t.add_argument("--allow-multiple", action="store_true",
                   help="do not close an already open suchi window (default: a new window replaces it)")
    t.add_argument("--ascii", action="store_true",
                   help="plain ASCII UI (automatic on the Linux console)")

    o = parser.add_argument_group("options")
    o.add_argument("-f", "--file", default=DEFAULT_HISTORY_PATH,
                   help=f"history database path (default: {DEFAULT_HISTORY_PATH})")
    o.add_argument("--limit", type=_int_min(1), default=DEFAULT_LIMIT,
                   help=f"max stored entries, pinned ones always stay (default: {DEFAULT_LIMIT})")
    o.add_argument("--max-size", type=_int_min(1), default=DEFAULT_MAX_BYTES, metavar="BYTES",
                   help=f"skip clipboard entries bigger than this (default: {DEFAULT_MAX_BYTES})")
    o.add_argument("-s", "--selection", default="clipboard", choices=["clipboard", "primary"],
                   help="which selection to watch / copy to (default: clipboard)")
    o.add_argument("--backend", default="auto", choices=["auto", "wayland", "x11", "tty"],
                   help="force a clipboard backend (default: auto-detect)")
    o.add_argument("--version", action="version", version=f"suchi {__version__}")
    return parser


def _run(args):
    args.file = os.path.expanduser(args.file)

    if args.stop:
        return stop_daemon(args.file)
    if args.status:
        return cmd_status(args)
    if args.pause or args.resume:
        return cmd_pause(args, args.pause)
    if args.install_service:
        return cmd_install_service(args)
    if args.uninstall_service:
        return cmd_uninstall_service(args)
    if args.list is not None:
        return cmd_list(args)
    if args.get is not None:
        return cmd_get(args)
    if args.clear:
        return cmd_clear(args)

    if args.add is not None:
        text = sys.stdin.read() if args.add == "-" else args.add
        if text.endswith("\n"):  # `echo foo | suchi --add` should store "foo"
            text = text[:-1]
        with contextlib.closing(open_db(args.file)) as db:
            added = add_clip(db, text, args.limit, args.max_size)
        if added:
            return 0
        eprint("suchi: nothing added (empty or larger than --max-size)")
        return 1

    if args.demon:
        backend, problem = detect_backend(args.backend)
        if backend == "tty":
            raise BackendError(
                (problem + "\n" if problem else "") +
                "no display server to watch (Linux console / SSH). A TTY has no clipboard; "
                "feed the history with `echo text | suchi --add` instead.")
        if backend == "x11":
            get_x11_libs()  # fail now, in the terminal, not silently after detaching
        lock_fd = acquire_daemon_lock(args.file)
        if not args.foreground:
            print(f"Starting suchi daemon in background "
                  f"(watching '{args.selection}' via {backend} -> {args.file})")
            if problem:
                eprint(f"suchi: note: {problem}")
            log_path = os.path.join(os.path.dirname(args.file) or ".", "suchi.log")
            with contextlib.suppress(OSError):
                if os.path.getsize(log_path) > 256 * 1024:
                    os.unlink(log_path)
            daemonize(log_path)
        try:
            run_daemon(args, backend, lock_fd)
        except BackendError as e:
            eprint(f"suchi: {e}")
            return 1
        return 0

    clip_id = run_tui(args)
    if clip_id is None:
        return 0
    # db is closed again before deliver(), which may fork
    with contextlib.closing(open_db(args.file)) as db:
        text = use_clip(db, clip_id)
    if text is None:
        eprint("suchi: that entry no longer exists")
        return 1
    if args.print:
        sys.stdout.write(text)
        sys.stdout.flush()
        return 0
    deliver(text, args)
    return 0


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.json and args.list is None and args.get is None:
        args.list = 20  # --json on its own means "list as JSON"
    actions = [args.demon, args.stop, args.status, args.pause, args.resume, args.install_service,
               args.uninstall_service, args.add is not None, args.list is not None,
               args.get is not None, args.clear]
    if sum(bool(a) for a in actions) > 1:
        parser.error("choose one action at a time (e.g. --list OR --clear)")
    try:
        return _run(args)
    except BackendError as e:
        eprint(f"suchi: {e}")
        return 1
    except KeyboardInterrupt:
        return 130
    except BrokenPipeError:
        return 0
    except sqlite3.Error as e:
        eprint(f"suchi: database error: {e}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
