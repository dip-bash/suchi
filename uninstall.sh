#!/bin/bash

BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="suchi-daemon.service"
SYSTEMD_USER_DIR="$HOME/.config/systemd/user"


# 1. Check for and handle the new version's daemon
if [ -f "$SYSTEMD_USER_DIR/$SERVICE_NAME" ]; then
    echo "Daemon service found (new version detected). Stopping and disabling..."
    systemctl --user stop "$SERVICE_NAME" 2>/dev/null
    systemctl --user disable "$SERVICE_NAME" 2>/dev/null
    rm "$SYSTEMD_USER_DIR/$SERVICE_NAME"
    systemctl --user daemon-reload
    echo "Daemon removed."
else
    echo "Info: No daemon service found (old version detected or already removed)."
fi

# 2. Remove the binary (works for both old and new versions)
if [ -f "$INSTALL_DIR/$BINARY_NAME" ]; then
    echo "Removing binary from $INSTALL_DIR (sudo required)..."
    sudo rm "$INSTALL_DIR/$BINARY_NAME"
    
        echo "Successfully removed $BINARY_NAME."
    else
        echo "Error: Failed to remove $BINARY_NAME. Check your permissions."
        exit 1
    fi
else
    echo "Info: $BINARY_NAME was not found in $INSTALL_DIR."
fi

# 3. Optional: Mention the clipboard history directory
if [ -d "$HOME/.cache/suchi" ]; then
    echo ""
    echo "Note: Your clipboard history is still saved at ~/.cache/suchi/history.json."
    echo "If you want to completely remove your clipboard data, run:"
    echo "rm -rf ~/.cache/suchi"
fi

echo "Uninstallation complete."
// Random modification at 1787029216.700035
// Random modification at 1787029216.773851
