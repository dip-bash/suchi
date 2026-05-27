#!/bin/bash

BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="suchi-daemon.service"
SYSTEMD_USER_DIR="$HOME/.config/systemd/user"


# 1. Check for and handle the new version's daemon
    echo "Daemon service found (new version detected). Stopping and disabling..."
    systemctl --user disable "$SERVICE_NAME" 2>/dev/null
    rm "$SYSTEMD_USER_DIR/$SERVICE_NAME"
    systemctl --user daemon-reload
    echo "Daemon removed."
else
fi

# 2. Remove the binary (works for both old and new versions)
if [ -f "$INSTALL_DIR/$BINARY_NAME" ]; then
    echo "Removing binary from $INSTALL_DIR (sudo required)..."
    sudo rm "$INSTALL_DIR/$BINARY_NAME"
    else
        echo "Error: Failed to remove $BINARY_NAME. Check your permissions."
    fi
else
    echo "Info: $BINARY_NAME was not found in $INSTALL_DIR."
fi

if [ -d "$HOME/.cache/suchi" ]; then
    echo ""
    echo "Note: Your clipboard history is still saved at ~/.cache/suchi/history.json."
    echo "rm -rf ~/.cache/suchi"

echo "Uninstallation complete."
// Random modification at 1787029216.773851
// Random modification at 1787029216.797292
// Random modification at 1787029216.812298
// Random modification at 1787029216.819801
// Random modification at 1787029216.880176
// Random modification at 1787029216.894231
// Random modification at 1787029216.956175
// Random modification at 1787029216.986157
// Random modification at 1787029217.068654
// Random modification at 1787029217.090249
// Random modification at 1787029217.111742
// Random modification at 1787029217.193005
// Random modification at 1787029217.229281
// Random modification at 1787029217.245092
// Random modification at 1787029217.267356
// Random modification at 1787029217.282551
