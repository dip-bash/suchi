<p align="center">
  <img src="https://github.com/dip-bash/img/blob/cf845804fa50814d2edc25f7445543fca4f9c7fb/suchi/suchi.png" alt="banner"/>
</p>


# suchi

A minimalist CLI clipboard manager designed for GNOME/Wayland environments. It currently interfaces with the GNOME Clipboard Indicator extension to provide terminal-based access to your clipboard history.

## Prerequisites

Before installing, ensure your system meets these requirements:
*   **OS:** Linux.
*   **Display Server:** xWayland or x11.
*   **Dependencies:** `xclip`, `libx11-6`, and `libxfixes3`. 
    * *Note: The installation script automatically detects Debian 12 and other Debian-based distributions to install these dependencies via `apt`.*

## Installation

Run the following command to download the latest binary and move it to `/usr/local/bin`:

```bash
curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash
```

## Uninstallation

To remove the binary from your system:

```bash
curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash
```

## Usage

Standard execution:
```bash
suchi
```
**TUI Shortcuts:**
* **`Enter`**: Copy the selected item and exit.
* **`Esc`**: Exit the UI.
* **`Alt+f`**: Toggle Fuzzy Searching.
* **`Alt+j` / `Alt+k**`: Navigate down/up.
* **`Alt+h`**: Toggle the shortcut help menu.

### Background Daemon

The systemd service automatically starts the daemon in the background on boot. You can check its status using:
```bash
systemctl --user status suchi-daemon.service
```
If you ever need to manually run the daemon independent of systemd, use:
```bash
suchi --demon
```

### Terminal Popup Examples
If you want to launch `suchi` in a floating or sized popup window using a keyboard shortcut, use these commands:

**For Kitty:**
```bash
kitty -o initial_window_width=80c -o initial_window_height=20c -o remember_window_size=no -- suchi
```

**For GNOME Terminal:**
```bash
gnome-terminal --geometry=80x20 -- suchi
```

## Future Roadmap

*   Add ability to delete specific entries directly from the CLI.
*   Implement pinning functionality for important snippets or other tab for snippets.
*   Add commands shortcuts
*   URL checker
*   Text extender-> type :mail in any place gives full email address

## Contributing

* Adding an interactive keybind (e.g., `Ctrl+p`) in the TUI for pinning and unpinning items.
* Adding a keybind (e.g., `Delete` or `d`) for removing specific items.

---

**Development Note:** The application data is stored in `~/.cache/suchi/history.json`.
