#!/usr/bin/env python3
"""
suchi - Combined X11 Clipboard Daemon and TUI.

Compiling with Nuitka:
    python3 -m nuitka --onefile suchi.py

Usage:
    ./suchi.bin --demon       # Creates a persistent background daemon watching the clipboard
    ./suchi.bin               # Opens the TUI to browse and use clipboard history
"""

import sys
import os
import json
import time
import ctypes
import subprocess
import argparse
import curses
from ctypes.util import find_library

# Apply UI configurations
os.environ.setdefault('ESCDELAY', '25')

DEFAULT_HISTORY_PATH = os.path.expanduser("~/.cache/suchi/history.json")
DEFAULT_LIMIT = 500

# =============================================================================
# X11 CTYPES DEFINITIONS (Lazy-loaded to avoid crashing TUI on non-X11 envs)
# =============================================================================

Display_p = ctypes.c_void_p
Window = ctypes.c_ulong
Atom = ctypes.c_ulong
Bool = ctypes.c_int

XA_PRIMARY = 1
False_ = 0
XFixesSetSelectionOwnerNotifyMask = 1 << 0

class XEvent(ctypes.Structure):
    _fields_ = [("pad", ctypes.c_long * 24)]  # padding, big enough for the union

def get_x11_libs():
    """Initializes and returns libX11 and libXfixes using ctypes."""
    x11_path = find_library("X11")
    xfixes_path = find_library("Xfixes")

    if not x11_path or not xfixes_path:
        sys.stderr.write("Could not find libX11 or libXfixes. Install libx11-6 / libxfixes3.\n")
        sys.exit(1)

    libX11 = ctypes.cdll.LoadLibrary(x11_path)
    libXfixes = ctypes.cdll.LoadLibrary(xfixes_path)

    libX11.XOpenDisplay.argtypes = [ctypes.c_char_p]
    libX11.XOpenDisplay.restype = Display_p
    libX11.XDefaultRootWindow.argtypes = [Display_p]
    libX11.XDefaultRootWindow.restype = Window
    libX11.XInternAtom.argtypes = [Display_p, ctypes.c_char_p, Bool]
    libX11.XInternAtom.restype = Atom

    libX11.XNextEvent.argtypes = [Display_p, ctypes.POINTER(XEvent)]
    libX11.XNextEvent.restype = ctypes.c_int
    libX11.XCloseDisplay.argtypes = [Display_p]
    libX11.XCloseDisplay.restype = ctypes.c_int
    
    libXfixes.XFixesSelectSelectionInput.argtypes = [Display_p, Window, Atom, ctypes.c_ulong]
    libXfixes.XFixesSelectSelectionInput.restype = None

    return libX11, libXfixes

# =============================================================================
# HISTORY MANAGEMENT
# =============================================================================

def load_history(path):
    """Loads the full history file."""
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

def validate_and_load(path, limit=100):
    """Loads, filters, sorts, and limits the history specifically for the TUI."""
    data = load_history(path)
        key=lambda x: (
            bool(x.get('pinned')), 
            max(x.get('usedAt') or 0, x.get('copiedAt') or 0)
        ), 
        reverse=True
    )
    return data[:limit]

def save_history(path, data, limit):
    """Trims history to the limit (exempting pinned items) and saves atomically."""
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
    os.replace(tmp_path, path)

def add_clip(path, text, limit):
    """Adds a new text snippet to the history, managing duplicates."""
    if text is None:
        return
    # xclip adds one trailing newline for plain single-line copies; drop just that one
    if text.endswith("\n"):
        text = text[:-1]
    if not text.strip():
        return

    data = load_history(path)
    now_ms = int(time.time() * 1000)

    # Bump instead of duplicating
    for item in data:
        if item.get("text") == text:
            item["copiedAt"] = now_ms
            save_history(path, data, limit)
            return

    data.insert(0, {"text": text, "pinned": False, "copiedAt": now_ms, "usedAt": 0})
    save_history(path, data, limit)

def touch_used(path, text):
    """Marks an item as recently used to bump it to the top of the TUI."""
    data = load_history(path)
    now_ms = int(time.time() * 1000)
    found = False
    for item in data:
        if item.get('text') == text:
            item['usedAt'] = now_ms
            found = True
            break
    if not found:
        return
    try:
        tmp_path = path + '.tmp'
        with open(tmp_path, 'w', encoding='utf-8') as f:
            json.dump(data, f, ensure_ascii=False)
        os.replace(tmp_path, path)
    except OSError:
        pass

# =============================================================================
# XCLIP UTILITIES
# =============================================================================

def get_clipboard_text(selection):
    """Grabs current selection text via xclip."""
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

def copy_to_x11(text, selection='clipboard'):
    """Pushes a string to the X11 clipboard using xclip."""
    try:
        process = subprocess.Popen(
            ['xclip', '-selection', selection, '-i'],
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True
        )
        process.stdin.write(text.encode('utf-8'))
        process.stdin.close()
    except Exception:
        pass

# =============================================================================
# TUI COMPONENTS
# =============================================================================

class AppState:
    def __init__(self):
        self.query = ""
        self.sel_idx = 0
        self.start_idx = 0
        self.running = True
        self.copy_item = None
        self.fuzzy_search = False
        self.show_shortcuts = False

def get_filtered_data(query, data, fuzzy=False):
    if not query:
        return [(item, []) for item in data]
    
    result = []
    
    if fuzzy:
        q_chars = query.lower().replace(" ", "")
        for item in data:
            raw = (item.get('text') or '').replace('\n', ' ').strip()
            raw_low = raw.lower()
            txt_idx = 0
            match = True
            indices = []
            for char in q_chars:
                txt_idx = raw_low.find(char, txt_idx)
                if txt_idx == -1:
                    match = False
                    break
                indices.append(txt_idx)
                txt_idx += 1
            if match:
                result.append((item, indices))
    else:
        q_chars = query.lower()
        for item in data:
            raw = (item.get('text') or '').replace('\n', ' ').strip()
            raw_low = raw.lower()
            idx = raw_low.find(q_chars)
            if idx != -1:
                indices = list(range(idx, idx + len(q_chars)))
                result.append((item, indices))
                
    return result

def get_relative_time(ts_ms):
    if not ts_ms:
        return ""
    diff = time.time() - (ts_ms / 1000.0)
    if diff < 60: return "just now"
    if diff < 3600: return f"{int(diff/60)}m ago"
    if diff < 86400: return f"{int(diff/3600)}h ago"
    return f"{int(diff/86400)}d ago"

def safe_addstr(stdscr, y, x, text, attr=0):
    try:
        stdscr.addstr(y, x, text, attr)
    except curses.error:
        pass

def handle_input(key, stdscr, state, filtered_len):
    list_h = stdscr.getmaxyx()[0] - 3

    if key == 27: # ESC
        stdscr.nodelay(True)
        next_key = stdscr.getch()
        stdscr.timeout(200)
        if next_key == -1:
            state.running = False
        elif next_key == ord('j'):
            if state.sel_idx < filtered_len - 1:
                state.sel_idx += 1
        elif next_key == ord('k'):
            if state.sel_idx > 0:
                state.sel_idx -= 1
        elif next_key == ord('f'):
            state.fuzzy_search = not state.fuzzy_search
            state.sel_idx = 0
            state.start_idx = 0
        elif next_key == ord('h'):
            state.show_shortcuts = not state.show_shortcuts
    elif key in (curses.KEY_ENTER, 10, 13):
        if filtered_len > 0:
            state.copy_item = True
            state.running = False
    elif key in (curses.KEY_BACKSPACE, 127, 8):
        state.query = state.query[:-1]
        state.sel_idx = 0
        state.start_idx = 0
    elif key == curses.KEY_DOWN:
        if state.sel_idx < filtered_len - 1:
            state.sel_idx += 1
    elif key == curses.KEY_UP:
        if state.sel_idx > 0:
            state.sel_idx -= 1
    elif key == curses.KEY_NPAGE:
        if filtered_len > 0:
            state.sel_idx = max(0, min(filtered_len - 1, state.sel_idx + list_h))
    elif key == curses.KEY_PPAGE:
        if filtered_len > 0:
            state.sel_idx = max(0, state.sel_idx - list_h)
    elif key == curses.KEY_HOME:
        state.sel_idx = 0
    elif key == curses.KEY_END:
        if filtered_len > 0:
            state.sel_idx = max(0, filtered_len - 1)
    elif key == curses.KEY_RESIZE:
        if hasattr(curses, 'update_lines_cols'):
            curses.update_lines_cols()
    elif 32 <= key <= 126:
        state.query += chr(key)
        state.sel_idx = 0
        state.start_idx = 0

def draw_ui(stdscr, state, filtered, height, width):
    if height < 4 or width < 15:
        safe_addstr(stdscr, 0, 0, "Term too small")
        stdscr.refresh()
        return

    list_h = height - 3
    
    if state.sel_idx >= len(filtered) and len(filtered) > 0:
    elif len(filtered) == 0:
        state.sel_idx = 0
    
    if state.sel_idx < state.start_idx:
        state.start_idx = state.sel_idx
    elif state.sel_idx >= state.start_idx + list_h:
        state.start_idx = state.sel_idx - list_h + 1

    header_text = f" SEARCH: {state.query}"
    safe_addstr(stdscr, 0, 0, header_text.ljust(width - 1)[:width - 1], curses.color_pair(2) | curses.A_BOLD)
    safe_addstr(stdscr, 1, 0, ("━" * (width - 1)), curses.color_pair(2) | curses.A_BOLD)

    visible_items = filtered[state.start_idx : state.start_idx + list_h]
    for i, (item, match_indices) in enumerate(visible_items):
        y = i + 2
        current_idx = state.start_idx + i
        has_newlines = '\n' in (item.get('text') or '')
        raw = (item.get('text') or '').replace('\n', ' ').strip()
        
        ml_indicator = " ↵ " if has_newlines else ""
        icon = "󰤱" if is_pinned else " "
        
        rel_time = get_relative_time(ts_ms)
        time_padding = len(rel_time) + 2 if (rel_time and width > 45) else 0

        try:
            stdscr.move(y, 0)
        except curses.error:
            continue

        if current_idx == state.sel_idx:
            try:
                stdscr.addstr(" ➜ ", curses.color_pair(1))
                stdscr.addstr(icon + " ", curses.color_pair(1))
            except curses.error: pass
        else:
            try:
                stdscr.addstr("   ")
                if is_pinned:
                    stdscr.addstr("󰤱", curses.color_pair(3))
                else:
                    stdscr.addstr(" ")
                stdscr.addstr(" ")
            except curses.error: pass

        _, curr_x = stdscr.getyx()
        max_len = width - curr_x - time_padding - len(ml_indicator) - 1
        if max_len < 0: max_len = 0
        
        display_text = raw[:max_len]
        if len(raw) > max_len and max_len > 3:
            display_text = raw[:max_len-3] + "..."

        for c_idx, char in enumerate(display_text):
            is_match = c_idx in match_indices
            if current_idx == state.sel_idx:
                attr = curses.color_pair(5) | curses.A_BOLD if is_match else curses.color_pair(1)
            else:
                attr = curses.color_pair(4) | curses.A_BOLD if is_match else curses.A_NORMAL
            try:
                stdscr.addstr(char, attr)
            except curses.error: pass

        _, curr_x = stdscr.getyx()

        if ml_indicator:
            attr = curses.color_pair(1) if current_idx == state.sel_idx else curses.A_DIM
            try:
                stdscr.addstr(ml_indicator, attr)
            except curses.error: pass
            _, curr_x = stdscr.getyx()

        if current_idx == state.sel_idx:
            pad_len = width - curr_x - time_padding - 1
            if pad_len > 0:
                try:
                    stdscr.addstr(" " * pad_len, curses.color_pair(1))
                except curses.error: pass

        if time_padding > 0:
            attr = curses.color_pair(1) if current_idx == state.sel_idx else curses.A_DIM
            safe_addstr(stdscr, y, width - time_padding - 1, rel_time, attr)

    fuzzy_status = "ON" if state.fuzzy_search else "OFF"
    if state.show_shortcuts:
        footer = f" Alt+f: Fuzzy ({fuzzy_status}) | Alt+j/k: Down/Up | Alt+h: Hide "
    else:
        footer = f" {len(filtered)} items | Fuzzy: {fuzzy_status} | Alt+h: Shortcuts | ENTER: Copy | ESC: Exit "
    
    safe_addstr(stdscr, height - 1, 0, footer.center(width - 1)[:width - 1], curses.A_DIM)
    stdscr.refresh()

def tui_main(stdscr, file_path):
    curses.use_default_colors()
    curses.init_pair(1, curses.COLOR_BLACK, curses.COLOR_CYAN)
    curses.init_pair(2, curses.COLOR_CYAN, -1)
    curses.init_pair(3, curses.COLOR_YELLOW, -1)
    curses.init_pair(4, curses.COLOR_GREEN, -1)
    curses.init_pair(5, curses.COLOR_YELLOW, curses.COLOR_CYAN)
    curses.curs_set(0)
    
    state = AppState()
    last_mtime = 0
    data = []
    filtered = []
    needs_redraw = True

    while state.running:
        try:
            current_mtime = os.path.getmtime(file_path)
            if current_mtime > last_mtime:
                data = validate_and_load(file_path, 100)
                filtered = get_filtered_data(state.query, data, state.fuzzy_search)
                last_mtime = current_mtime
                needs_redraw = True
        except OSError:
            pass

        if needs_redraw:
            stdscr.clear()
            height, width = stdscr.getmaxyx()
            draw_ui(stdscr, state, filtered, height, width)
            needs_redraw = False
        
        stdscr.timeout(200) 
        key = stdscr.getch()

        if key != -1:
            needs_redraw = True
            old_query = state.query
            handle_input(key, stdscr, state, len(filtered))
            
            if state.running and state.query != old_query:
                filtered = get_filtered_data(state.query, data, state.fuzzy_search)
                
            if state.copy_item:
                state.copy_item = filtered[state.sel_idx][0]

    return state.copy_item

# =============================================================================
# DAEMON LOGIC
# =============================================================================

def daemonize():
    """Double-fork daemonization process to safely detach."""
    try:
        pid = os.fork()
        if pid > 0:
            sys.exit(0)
    except OSError as e:
        sys.stderr.write(f"Fork #1 failed: {e}\n")
        sys.exit(1)

    os.chdir("/")
    os.setsid()
    os.umask(0)

    try:
        pid = os.fork()
        if pid > 0:
            sys.exit(0)
    except OSError as e:
        sys.stderr.write(f"Fork #2 failed: {e}\n")
        sys.exit(1)

    sys.stdout.flush()
    sys.stderr.flush()

    # Redirect standard file descriptors
    with open(os.devnull, 'r') as f:
        os.dup2(f.fileno(), sys.stdin.fileno())
    with open(os.devnull, 'a+') as f:
        os.dup2(f.fileno(), sys.stdout.fileno())
        os.dup2(f.fileno(), sys.stderr.fileno())

def run_daemon(args):
    """X11 connection and while loop to persistently watch the clipboard."""
    libX11, libXfixes = get_x11_libs()

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

    libXfixes.XFixesSelectSelectionInput(
        disp, root, target_atom, XFixesSetSelectionOwnerNotifyMask
    )

    evt = XEvent()
    last_text = None

    try:
        while True:
            libX11.XNextEvent(disp, ctypes.byref(evt))
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

# =============================================================================
# MAIN ENTRYPOINT
# =============================================================================

def main():
    parser = argparse.ArgumentParser(description="suchi - X11 Clipboard Manager & TUI")
    
    parser.add_argument(
        "--demon", "--daemon",
        action="store_true",
        help="Run the clipboard watcher as a persistent background daemon"
    )
    parser.add_argument(
        "--foreground",
        action="store_true",
        help="Run daemon in foreground without detaching (used with --demon)"
    )
    parser.add_argument(
        "-f", "--file",
        default=DEFAULT_HISTORY_PATH,
        help=f"History JSON path (default: {DEFAULT_HISTORY_PATH})"
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=DEFAULT_LIMIT,
        help=f"Max stored entries for the daemon (default: {DEFAULT_LIMIT})"
    )
    parser.add_argument(
        "-s", "--selection",
        default="clipboard",
        choices=["clipboard", "primary"],
        help="Which X selection to watch in daemon mode (default: clipboard)"
    )
    
    args = parser.parse_args()

    if args.demon:
        if not args.foreground:
            print(f"Starting suchi daemon in background (watching '{args.selection}' -> {args.file})")
            daemonize()
        run_daemon(args)
    else:
        # Standard execution runs the TUI
        copy_item = curses.wrapper(tui_main, args.file)
        if copy_item:
            text_to_copy = copy_item.get('text') or ''
            touch_used(args.file, text_to_copy)
            copy_to_x11(text_to_copy)

if __name__ == "__main__":
    main()// Random modification at 1787029216.691921
// Random modification at 1787029216.766229
// Random modification at 1787029216.789103
// Random modification at 1787029216.827052
// Random modification at 1787029216.856697
// Random modification at 1787029216.908681
// Random modification at 1787029216.916613
// Random modification at 1787029216.924742
// Random modification at 1787029216.970522
// Random modification at 1787029216.978286
// Random modification at 1787029217.007868
// Random modification at 1787029217.01595
