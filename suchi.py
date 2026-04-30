import json
import curses
import os
import time
import subprocess
import argparse

os.environ.setdefault('ESCDELAY', '25')

class AppState:
    def __init__(self):
        self.query = ""
        self.sel_idx = 0
        self.start_idx = 0
        self.running = True
        self.copy_item = None

def validate_and_load(path, limit=100):
    if not os.path.exists(path):
        return []
    try:
        with open(path, 'r', encoding='utf-8') as f:
            data = json.load(f)
            if not isinstance(data, list):
                return []
            data = [x for x in data if isinstance(x, dict)]
            data.sort(
                key=lambda x: (
                    bool(x.get('pinned')), 
                    max(x.get('usedAt') or 0, x.get('copiedAt') or 0)
                ), 
                reverse=True
            )
            return data[:limit]
    except (json.JSONDecodeError, ValueError, UnicodeDecodeError, OSError):
        return []

def copy_to_wayland(text):
    try:
        # Fully detach the process from the terminal so GNOME terminal can close instantly
        process = subprocess.Popen(
            ['wl-copy'], 
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True  # Create a new session, detaching from the parent terminal
        )
        process.stdin.write(text.encode('utf-8'))
        process.stdin.close() # Close stdin to signal EOF to wl-copy
    except Exception:
        pass

def get_filtered_data(query, data):
    if not query:
        return [(item, []) for item in data]
    q_chars = query.lower().replace(" ", "")
    result = []
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
        pass 
    elif 32 <= key <= 126:
        state.query += chr(key)
        state.sel_idx = 0
        state.start_idx = 0

def main(stdscr, file_path):
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
                filtered = get_filtered_data(state.query, data)
                last_mtime = current_mtime
                needs_redraw = True
        except OSError:
            pass

        if needs_redraw:
            stdscr.clear()
            height, width = stdscr.getmaxyx()
            
            if height < 4 or width < 15:
                safe_addstr(stdscr, 0, 0, "Term too small")
                stdscr.refresh()
            else:
                list_h = height - 3
                
                if state.sel_idx >= len(filtered) and len(filtered) > 0:
                    state.sel_idx = len(filtered) - 1
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
                    is_pinned = bool(item.get('pinned'))
                    has_newlines = '\n' in (item.get('text') or '')
                    raw = (item.get('text') or '').replace('\n', ' ').strip()
                    
                    ml_indicator = " ↵ " if has_newlines else ""
                    icon = "󰤱" if is_pinned else " "
                    
                    ts_ms = max(item.get('usedAt') or 0, item.get('copiedAt') or 0)
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

                footer = f" {len(filtered)} items | ENTER: Copy | ESC: Exit "
                safe_addstr(stdscr, height - 1, 0, footer.center(width - 1)[:width - 1], curses.A_DIM)

            stdscr.refresh()
            needs_redraw = False
        
        stdscr.timeout(200) 
        key = stdscr.getch()

        if key != -1:
            needs_redraw = True
            old_query = state.query
            handle_input(key, stdscr, state, len(filtered))
            
            if state.running and state.query != old_query:
                filtered = get_filtered_data(state.query, data)
                
            if state.copy_item:
                state.copy_item = filtered[state.sel_idx][0]

    # Outside the while loop: Wait for curses to shut down, then copy
    return state.copy_item

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("file", nargs="?", default=os.path.expanduser("~/.cache/gnome-clipboard@b00f.github.io/history.json"))
    args = parser.parse_args()
    
    copy_item = curses.wrapper(main, args.file)
    if copy_item:
        text_to_copy = copy_item.get('text') or ''
        copy_to_wayland(text_to_copy)
