<p align="center">
  <img src="https://github.com/dip-bash/img/blob/cf845804fa50814d2edc25f7445543fca4f9c7fb/suchi/suchi.png" alt="banner"/>
</p>


# suchi

A minimalist CLI clipboard manager designed for Linux environments. 
## Key Features

* **Runs Standalone:** No third-party desktop extensions required. Works out of the box with a built-in background watcher.
* **Vim-Style Navigation:** Seamless dual-mode interface (Normal / Insert) designed for terminal lovers.
* **Smart Search:** Easily toggle between exact text search and fuzzy matching.
* **Pin & Delete Clips:** Keep your most important snippets saved at the top or delete unwanted sensitive data directly from the UI.
* **Single Binary Distribution:** Built as a self-contained executable for low resource consumption and fast execution.

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

## Keyboard Shortcuts

The app opens in **Normal Mode** by default.

### Modes

* `i` — Switch to **Insert Mode** to type your search query.
* `Esc` (in Insert mode) — Return to **Normal Mode**.
* `Esc` (in Normal mode) — Exit `suchi`.

### Normal Mode Commands

* `j` / `k` or `Down` / `Up` — Move selection down or up.
* `f` — Toggle fuzzy search ON/OFF.
* `y` or `Enter` — Copy highlighted entry to clipboard and exit.
* `d` — Delete highlighted entry permanently.
* `p` — Pin or unpin highlighted entry.
* `?` — Toggle keybinding help overlay popup.
* `Page Up` / `Page Down` — Scroll quickly through list pages.

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

**For Foot:**
```bash
footclient -W 80x20 suchi; or foot -W 80x20 suchi
```

## Upcoming Features & Roadmap

The project is under active development. Here is what is coming next:

### 1. Tabbed Interface (`Tab` Key Navigation)

Cycle seamlessly between three dedicated workspaces using the `Tab` key:

* **Tab 1: Clipboard History** — The default clipboard manager with history search, pinning, and deletion.
* **Tab 2: Short Notes** — A minimal scratchpad UI to quickly draft and save plain text notes on the fly using Vim-style editing.
* **Tab 3: Text Expander** — Store short system-wide text triggers (e.g., typing `:mail` automatically expands into your full email address or long commands).
*   URL checker


---

**Development Note:** The application data is stored in `~/.cache/suchi/history.json`.
