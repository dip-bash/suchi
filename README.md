<p align="center">
  <img src="https://github.com/dip-bash/img/blob/cf845804fa50814d2edc25f7445543fca4f9c7fb/suchi/suchi.png" alt="banner"/>
</p>


# suchi

A minimalist CLI clipboard manager designed for GNOME/Wayland environments. It currently interfaces with the GNOME Clipboard Indicator extension to provide terminal-based access to your clipboard history.

## Prerequisites

Before installing, ensure your system meets these requirements:
*   **OS:** Debian-based Linux.
*   **Desktop:** GNOME.
*   **Display Server:** Wayland.
*   **Extension:** [Clipboard Indicator](https://extensions.gnome.org/extension/4422/gnome-clipboard/) must be installed and active.

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
*   Implement pinning functionality for important snippets.
*   Remove strict dependencies on Debian and GNOME.
*   Remove the requirement for the Clipboard Indicator extension.
*   Support for X11 and other desktop environments.

## Contributing

The goal is to move away from specific desktop environment dependencies and make this a universal Linux tool. Contributions that help with the following are welcome via Pull Requests:

*   Refactoring the backend to handle independent clipboard state.
*   Adding support for X11/wl-clipboard integration.
*   Improving the CLI interface for pinning and deleting items.

---

**Development Note:** This project is in active development. If you encounter issues with the history file path, verify that the GNOME extension is generating `history.json` in your `~/.cache` directory.
