#!/bin/bash

REPO="dip-bash/suchi"
BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"
CLIPBOARD_FILE="$HOME/.cache/gnome-clipboard@b00f.github.io/history.json"

if [ ! -f /etc/debian_version ]; then
    echo "Error: This script only supports Debian-based distributions."
    exit 1
fi

if [ "$XDG_SESSION_TYPE" != "wayland" ]; then
    echo "Error: 'suchi' requires a Wayland session to function."
    exit 1
fi

if [ "$XDG_CURRENT_DESKTOP" != "GNOME" ]; then
    echo "Error: This tool is designed specifically for the GNOME Desktop Environment."
    exit 1
fi

if [ ! -f "$CLIPBOARD_FILE" ]; then
    echo "Error: Clipboard history file not found."
    echo "Please install the 'Clipboard Indicator' extension first: https://extensions.gnome.org/extension/4422/gnome-clipboard/"
    exit 1
fi

DOWNLOAD_URL=$(curl -s https://api.github.com/repos/$REPO/releases/latest | grep "browser_download_url" | grep -m 1 "$BINARY_NAME" | cut -d '"' -f 4)

if [ -z "$DOWNLOAD_URL" ]; then
    echo "Error: Could not retrieve the latest binary from GitHub."
    exit 1
fi

echo "Downloading $BINARY_NAME..."
curl -L -o $BINARY_NAME "$DOWNLOAD_URL"
chmod +x $BINARY_NAME

echo "Installing to $INSTALL_DIR (sudo required)..."
sudo mv $BINARY_NAME "$INSTALL_DIR/$BINARY_NAME"

echo "Done. You can now run '$BINARY_NAME'."
