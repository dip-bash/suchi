#!/usr/bin/env python3
"""
suchi - clipboard history for the terminal (Wayland, X11 and TTY).
Dev - Saumyadip Jana, contact: saumyadip.social@gmail.com

Usage:
    suchi --demon            # background daemon that records clipboard changes
    suchi --demon --foreground
    suchi --stop             # stop the background daemon
    suchi                    # TUI: browse / search / copy history
    suchi -p                 # TUI, but print the chosen entry to stdout
    echo text | suchi --add  # push text into the history by hand (works in a TTY)

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

Paste into the shell prompt in any terminal, including the Linux console
(bash, put in ~/.bashrc):
    bind -x '"\\C-x\\C-v": __s=$(suchi -p); READLINE_LINE="${READLINE_LINE:0:READLINE_POINT}${__s}${READLINE_LINE:READLINE_POINT}"; READLINE_POINT=$((READLINE_POINT+${#__s}))'

systemd user service (~/.config/systemd/user/suchi.service):
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
import struct
import subprocess
import sys
import tempfile
import time
import unicodedata
from ctypes.util import find_library

__version__ = "0.2.0"

# Apply UI configurations
os.environ.setdefault("ESCDELAY", "25")

_CACHE_HOME = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
DEFAULT_HISTORY_PATH = os.path.join(_CACHE_HOME, "suchi", "history.json")
DEFAULT_LIMIT = 500
DEFAULT_MAX_BYTES = 1024 * 1024  # entries larger than this are not recorded


class BackendError(Exception):
    """A clipboard backend is missing or failed; the message is user-facing."""


def eprint(*a):
    print(*a, file=sys.stderr, flush=True)


# ════════════════════════════════════════════════════════════════════════════
# HISTORY MANAGEMENT
#
# Every read-modify-write happens under an exclusive flock so the daemon and
# the TUI can never overwrite each other, and every write goes through a
# unique temp file + fsync + atomic rename. Files are created 0600 because
# clipboard contents routinely include passwords and tokens.
# ════════════════════════════════════════════════════════════════════════════

def _num(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else 0


def _sort_key(item):
    return (bool(item.get("pinned")), max(_num(item.get("usedAt")), _num(item.get("copiedAt"))))


def _ensure_dir(path):
    d = os.path.dirname(path) or "."
    os.makedirs(d, mode=0o700, exist_ok=True)
    return d


@contextlib.contextmanager
def history_lock(path):
    _ensure_dir(path)
    fd = os.open(path + ".lock", os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)  # closing the descriptor releases the lock


def _quarantine(path):
    """Move an unreadable history file aside instead of silently overwriting it."""
    dest = f"{path}.corrupt-{int(time.time())}"
    with contextlib.suppress(OSError):
        os.replace(path, dest)
        eprint(f"suchi: history file was corrupt, moved to {dest}")


def load_history(path):
    """Load the full history. Raises OSError if the file exists but can't be read."""
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except FileNotFoundError:
        return []
    except (ValueError, UnicodeDecodeError):  # JSONDecodeError is a ValueError
        _quarantine(path)
        return []
    if not isinstance(data, list):
        _quarantine(path)
        return []
    return [x for x in data if isinstance(x, dict) and isinstance(x.get("text"), str)]


def validate_and_load(path):
    """Loads and sorts the whole history for the TUI (pinned first, then recency)."""
    try:
        data = load_history(path)
    except OSError:
        return []
    data.sort(key=_sort_key, reverse=True)
    return data


def trim_history(data, limit):
    """Sort first, THEN trim, so the entries that get dropped are the oldest ones."""
    data = sorted(data, key=_sort_key, reverse=True)
    pinned = [x for x in data if x.get("pinned")]
    rest = [x for x in data if not x.get("pinned")]
    return pinned + rest[: max(0, limit - len(pinned))]


def save_history(path, data, limit=None):
    """Atomically write the history (caller must hold history_lock)."""
    d = _ensure_dir(path)
    if limit is not None:
        data = trim_history(data, limit)
    fd, tmp = tempfile.mkstemp(prefix=".history-", suffix=".tmp", dir=d)  # created 0600
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(tmp)
        raise


def add_clip(path, text, limit, max_bytes=DEFAULT_MAX_BYTES):
    """Adds a text snippet to the history, bumping instead of duplicating.
    The text is stored exactly as copied (no trailing-newline trimming).
    Returns True if something was recorded."""
    if text is None:
        return False
    text = text.encode("utf-8", "replace").decode("utf-8")
    if not text.strip() or len(text.encode("utf-8")) > max_bytes:
        return False
    now_ms = int(time.time() * 1000)
    with history_lock(path):
        data = load_history(path)
        for item in data:
            if item.get("text") == text:
                item["copiedAt"] = now_ms
                break
        else:
            data.append({"text": text, "pinned": False, "copiedAt": now_ms, "usedAt": 0})
        save_history(path, data, limit)
    return True


def _mutate(path, text, fn):
    """Apply fn(data, item) to the entry whose text matches. Returns True if found."""
    with history_lock(path):
        data = load_history(path)
        for item in data:
            if item.get("text") == text:
                fn(data, item)
                save_history(path, data)
                return True
    return False


def touch_used(path, text):
    """Marks an item as recently used to bump it to the top of the TUI."""
    return _mutate(path, text, lambda d, i: i.__setitem__("usedAt", int(time.time() * 1000)))


def delete_clip(path, text):
    """Removes a single history entry that matches text. Returns True if deleted."""
    return _mutate(path, text, lambda d, i: d.remove(i))


def toggle_pin(path, text):
    """Flips the pinned flag on the entry that matches text. Returns True if found."""
    return _mutate(path, text, lambda d, i: i.__setitem__("pinned", not i.get("pinned", False)))


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


def x11_read(selection):
    """Grabs current selection text via xclip/xsel. None if empty / not text."""
    tool = x11_tool()
    if tool == "xclip":
        cmd = ["xclip", "-selection", selection, "-o"]
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

GLYPHS_UNICODE = {"arrow": "➜", "pin": "󰤱", "rule": "━", "ml": " ↵ "}
GLYPHS_ASCII = {"arrow": ">", "pin": "*", "rule": "-", "ml": " ~ "}

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
    __slots__ = ("item", "text", "disp", "low")

    def __init__(self, item):
        self.item = item
        self.text = item.get("text") or ""
        self.disp = display_text(self.text)
        self.low = _lower(self.disp)


class AppState:
    def __init__(self):
        self.query = ""
        self.sel_idx = 0
        self.start_idx = 0
        self.running = True
        self.copy_item = None
        self.fuzzy_search = False
        self.show_shortcuts = False
        self.mode = "normal"   # "normal" or "insert" (vim-style)
        self.delete_item = None
        self.pin_item = None
        self.flash_msg = ""    # short transient status message (e.g. "Deleted")


def get_filtered_data(query, entries, fuzzy=False):
    """Returns [(entry, set_of_matching_indices)]."""
    if not query:
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
        q = _lower(query)
        for e in entries:
            i = e.low.find(q)
            if i != -1:
                result.append((e, frozenset(range(i, i + len(q)))))
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
    list_h = max(1, stdscr.getmaxyx()[0] - 3)

    # Any keypress clears the previous transient status message.
    state.flash_msg = ""

    if key == curses.KEY_RESIZE:
        if hasattr(curses, "update_lines_cols"):
            curses.update_lines_cols()
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
        elif key == curses.KEY_DOWN:
            _move(state, 1, filtered_len)
        elif key == curses.KEY_UP:
            _move(state, -1, filtered_len)
        elif isinstance(key, str):
            state.query += key
            state.sel_idx = state.start_idx = 0
        return

    # NORMAL MODE: vim-style single-key commands.
    if key in (27, "q"):
        state.running = False
    elif key in ("i", "/"):
        state.mode = "insert"
    elif key in ("j", curses.KEY_DOWN):
        _move(state, 1, filtered_len)
    elif key in ("k", curses.KEY_UP):
        _move(state, -1, filtered_len)
    elif key == "f":
        state.fuzzy_search = not state.fuzzy_search
        state.sel_idx = state.start_idx = 0
    elif key == "y" or key in ENTER_KEYS:
        if filtered_len > 0:
            state.copy_item = True
            state.running = False
    elif key == "d":
        if filtered_len > 0:
            state.delete_item = True
    elif key == "p":
        if filtered_len > 0:
            state.pin_item = True
    elif key == "?":
        state.show_shortcuts = not state.show_shortcuts
    elif key == curses.KEY_NPAGE:
        _move(state, list_h, filtered_len)
    elif key == curses.KEY_PPAGE:
        _move(state, -list_h, filtered_len)
    elif key in (curses.KEY_HOME, "g"):
        state.sel_idx = 0
    elif key in (curses.KEY_END, "G"):
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


def draw_row(stdscr, y, width, entry, matches, selected, G, T):
    item = entry.item
    pinned = bool(item.get("pinned"))
    ml = G["ml"] if "\n" in entry.text else ""
    rel = get_relative_time(max(_num(item.get("usedAt")), _num(item.get("copiedAt"))))
    time_w = len(rel) + 2 if (rel and width > 45) else 0

    if selected:
        safe_addstr(stdscr, y, 0, " " * (width - 1), T["sel"])
        safe_addstr(stdscr, y, 0, f" {G['arrow']} {G['pin'] if pinned else ' '} ", T["sel"])
    else:
        safe_addstr(stdscr, y, 0, "   ")
        safe_addstr(stdscr, y, 3, G["pin"] if pinned else " ", T["pin"])
    x = 5

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


def draw_ui(stdscr, state, filtered, height, width, G, T):
    if height < 4 or width < 15:
        safe_addstr(stdscr, 0, 0, "Term too small")
        stdscr.refresh()
        return

    list_h = height - 3

    if state.sel_idx >= len(filtered) and len(filtered) > 0:
        state.sel_idx = len(filtered) - 1
    elif len(filtered) == 0:
        state.sel_idx = 0

    if state.sel_idx < state.start_idx:
        state.start_idx = state.sel_idx
    elif state.sel_idx >= state.start_idx + list_h:
        state.start_idx = state.sel_idx - list_h + 1

    if state.mode == "insert":
        mode_tag, mode_attr = " INSERT ", T["mode_ins"]
    else:
        mode_tag, mode_attr = " NORMAL ", T["mode_norm"]
    safe_addstr(stdscr, 0, 0, mode_tag, mode_attr)

    tag_w = len(mode_tag)
    header_text = f" SEARCH: {state.query}"
    if state.flash_msg:
        header_text = f" {state.flash_msg}"
    avail = max(0, width - 1 - tag_w)
    # keep the tail of a long query visible, count columns not characters
    while swidth(header_text) > avail and len(header_text) > 1:
        header_text = header_text[1:] if not state.flash_msg else header_text[:-1]
    safe_addstr(stdscr, 0, tag_w, header_text.ljust(avail), T["head"])
    safe_addstr(stdscr, 1, 0, G["rule"] * (width - 1), T["head"])

    if not filtered:
        msg = "No matches" if state.query else "History is empty - run `suchi --demon`, or pipe text into `suchi --add`"
        safe_addstr(stdscr, 2, 3, msg[: width - 4], curses.A_DIM)

    for i, (entry, matches) in enumerate(filtered[state.start_idx: state.start_idx + list_h]):
        draw_row(stdscr, i + 2, width, entry, matches, state.start_idx + i == state.sel_idx, G, T)

    fuzzy_status = "ON" if state.fuzzy_search else "OFF"
    if state.mode == "insert":
        footer = " -- INSERT -- | Esc: Normal mode | Enter: Copy "
    else:
        footer = (f" {len(filtered)} items | Fuzzy: {fuzzy_status} | "
                  "i: Search  j/k: Move  y: Copy  d: Delete  p: Pin  ?: Help ")
    safe_addstr(stdscr, height - 1, 0, footer.center(width - 1)[: width - 1], curses.A_DIM)

    if state.show_shortcuts:
        draw_shortcuts_overlay(stdscr, height, width, T)

    stdscr.refresh()


def draw_shortcuts_overlay(stdscr, height, width, T):
    """Draws a centered popup listing every keybinding."""
    lines = [
        ("suchi - keybindings", True),
        ("", False),
        ("i or /      enter insert mode (search)", False),
        ("Esc         insert: back to normal  |  normal: quit", False),
        ("j / k       move down / up (arrows work in both modes)", False),
        ("g / G       jump to first / last", False),
        ("f           toggle fuzzy search", False),
        ("y / Enter   copy highlighted entry", False),
        ("d           delete highlighted entry", False),
        ("p           pin / unpin highlighted entry", False),
        ("Ctrl-U/W    insert: clear query / delete word", False),
        ("q           quit", False),
        ("?           toggle this help", False),
        ("", False),
        ("press ? to close", True),
    ]
    box_w = min(width - 4, max(len(t) for t, _ in lines) + 6)
    box_h = min(height - 4, len(lines) + 2)
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


def tui_main(stdscr, file_path, ascii_mode=False):
    T = init_theme()
    G = GLYPHS_ASCII if ascii_mode else GLYPHS_UNICODE
    with contextlib.suppress(curses.error):
        curses.curs_set(0)

    state = AppState()
    last_mtime = None
    last_minute = int(time.time() // 60)
    entries, filtered = [], []
    needs_redraw = True

    def reload():
        nonlocal entries, filtered, last_mtime
        entries = [Entry(i) for i in validate_and_load(file_path)]
        filtered = get_filtered_data(state.query, entries, state.fuzzy_search)
        try:
            last_mtime = os.stat(file_path).st_mtime_ns
        except OSError:
            last_mtime = None

    def refilter():
        nonlocal filtered
        filtered = get_filtered_data(state.query, entries, state.fuzzy_search)

    while state.running:
        try:
            mt = os.stat(file_path).st_mtime_ns
        except OSError:
            mt = None
        if mt != last_mtime:
            reload()
            needs_redraw = True
        minute = int(time.time() // 60)
        if minute != last_minute:  # keep "5m ago" labels fresh
            last_minute = minute
            needs_redraw = True

        if needs_redraw:
            stdscr.erase()
            height, width = stdscr.getmaxyx()
            draw_ui(stdscr, state, filtered, height, width, G, T)
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
            target = filtered[state.sel_idx][0].text
            state.delete_item = None
            try:
                delete_clip(file_path, target)
                state.flash_msg = "Entry deleted"
            except OSError:
                state.flash_msg = "Delete failed"
            reload()
            state.sel_idx = min(state.sel_idx, max(0, len(filtered) - 1))

        if state.pin_item and filtered:
            target = filtered[state.sel_idx][0].text
            state.pin_item = None
            try:
                toggle_pin(file_path, target)
            except OSError:
                state.flash_msg = "Pin failed"
            reload()
            for i, (e, _) in enumerate(filtered):
                if e.text == target:
                    state.sel_idx = i
                    if not state.flash_msg:
                        state.flash_msg = "Pinned" if e.item.get("pinned") else "Unpinned"
                    break

        if state.copy_item and filtered:
            return filtered[state.sel_idx][0].text
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


def run_tui(args):
    with contextlib.suppress(locale.Error):
        locale.setlocale(locale.LC_ALL, "")
    enc = (locale.getpreferredencoding(False) or "").lower()
    ascii_mode = args.ascii or os.environ.get("TERM") == "linux" or "utf" not in enc
    with _tty_stdio():
        return curses.wrapper(tui_main, args.file, ascii_mode)


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


def run_daemon(args, backend, lock_fd):
    """Persistently watch the clipboard and record changes."""
    os.ftruncate(lock_fd, 0)
    os.write(lock_fd, str(os.getpid()).encode())

    stopping = []
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, lambda *_: stopping.append(1))
    stop = lambda: bool(stopping)

    def on_text(text):
        try:
            add_clip(args.file, text, args.limit, args.max_size)
        except Exception as e:  # keep watching even if one write fails
            eprint(f"suchi: could not save entry: {e}")

    if backend == "wayland":
        wl = WaylandClipboard()
        try:
            wl.watch(args.selection, on_text, stop, args.max_size)
        finally:
            wl.close()
    else:
        watch_x11(args.selection, on_text, stop)


# ════════════════════════════════════════════════════════════════════════════
# MAIN ENTRYPOINT
# ════════════════════════════════════════════════════════════════════════════

def build_parser():
    parser = argparse.ArgumentParser(
        description="suchi - clipboard history for Wayland, X11 and TTY terminals",
        epilog="See the top of this file for the shell binding (paste in a TTY) and a systemd unit.")
    parser.add_argument("--demon", "--daemon", action="store_true", dest="demon",
                        help="Run the clipboard watcher as a persistent background daemon")
    parser.add_argument("--foreground", action="store_true",
                        help="Run daemon in foreground without detaching (used with --demon)")
    parser.add_argument("--stop", action="store_true", help="Stop the background daemon")
    parser.add_argument("--add", nargs="?", const="-", metavar="TEXT",
                        help="Add TEXT (or stdin when omitted / '-') to the history and exit")
    parser.add_argument("-p", "--print", action="store_true",
                        help="TUI: print the chosen entry to stdout instead of copying it")
    parser.add_argument("-f", "--file", default=DEFAULT_HISTORY_PATH,
                        help=f"History JSON path (default: {DEFAULT_HISTORY_PATH})")
    parser.add_argument("--limit", type=int, default=DEFAULT_LIMIT,
                        help=f"Max stored entries (default: {DEFAULT_LIMIT})")
    parser.add_argument("--max-size", type=int, default=DEFAULT_MAX_BYTES, metavar="BYTES",
                        help=f"Skip clipboard entries bigger than this (default: {DEFAULT_MAX_BYTES})")
    parser.add_argument("-s", "--selection", default="clipboard", choices=["clipboard", "primary"],
                        help="Which selection to watch / copy to (default: clipboard)")
    parser.add_argument("--backend", default="auto", choices=["auto", "wayland", "x11", "tty"],
                        help="Force a clipboard backend (default: auto-detect)")
    parser.add_argument("--ascii", action="store_true",
                        help="Plain ASCII UI (automatic on the Linux console)")
    parser.add_argument("--version", action="version", version=f"suchi {__version__}")
    return parser


def _run(args):
    args.file = os.path.expanduser(args.file)

    if args.stop:
        return stop_daemon(args.file)

    if args.add is not None:
        text = sys.stdin.read() if args.add == "-" else args.add
        if text.endswith("\n"):  # `echo foo | suchi --add` should store "foo"
            text = text[:-1]
        if add_clip(args.file, text, args.limit, args.max_size):
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

    text = run_tui(args)
    if text is None:
        return 0
    with contextlib.suppress(OSError):
        touch_used(args.file, text)
    if args.print:
        sys.stdout.write(text)
        sys.stdout.flush()
        return 0
    deliver(text, args)
    return 0


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return _run(args)
    except BackendError as e:
        eprint(f"suchi: {e}")
        return 1
    except KeyboardInterrupt:
        return 130
    except BrokenPipeError:
        return 0


if __name__ == "__main__":
    sys.exit(main())
