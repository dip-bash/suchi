# OpenCode Agent Instructions: Suchi

This file contains high-signal, repo-specific context for OpenCode agents.

## Architecture & Environment
- **Platform Constraint**: This is a Wayland-specific terminal UI. It strictly requires the `wl-copy` system binary to execute clipboard operations. 
- **Data Source**: It acts as a frontend for the `gnome-clipboard@b00f.github.io` GNOME extension, reading from `~/.cache/gnome-clipboard@b00f.github.io/history.json` by default.
- **Dependencies**: It is a zero-dependency script relying only on the Python standard library (notably `curses` and `subprocess`). 

## Quirks & Gotchas
- **Process Detachment**: The `copy_to_wayland` function intentionally spawns `wl-copy` using `subprocess.Popen(..., start_new_session=True)`. This detaches the clipboard process so the GNOME terminal can close immediately without killing the copy operation. **Do not refactor this to `subprocess.run`** or remove the session detachment.
- **TUI Updates**: The curses UI dynamically polls the file modification time (`os.path.getmtime`) in the main loop to handle updates from the background GNOME extension.

## Execution
- Run directly via `python suchi.py [optional_path_to_json]`.