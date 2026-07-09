#!/bin/bash

# Configuration
REPO="dip-bash/suchi"
BINARY_NAME="suchi"
SYSTEMD_USER_DIR="$HOME/.config/systemd/user"
SERVICE_NAME="suchi-daemon.service"

echo "Checking system requirements..."

# 1. Ensure an active graphical session (X11 or XWayland)
if [ -z "$DISPLAY" ] && [ -z "$WAYLAND_DISPLAY" ]; then
    echo "Error: 'suchi' requires an active X11 or Wayland session."
    exit 1
fi
# 2. Automatically install required dependencies for Debian-based systems
if [ -f /etc/debian_version ]; then
    echo "Debian-based system detected. Checking dependencies..."
    MISSING_PKGS=""
    for pkg in xclip libx11-6 libxfixes3; do
            MISSING_PKGS="$MISSING_PKGS $pkg"
        fi
    done
    if [ -n "$MISSING_PKGS" ]; then
        echo "Missing dependencies detected: $MISSING_PKGS"
        sudo apt-get update
        sudo apt-get install -y $MISSING_PKGS
    fi
else
    echo "Warning: Not a Debian-based system. Please ensure 'xclip', 'libx11', and 'libxfixes' are installed manually."
fi

# 3. Fetch the latest release from GitHub
echo "Fetching latest release from GitHub..."
DOWNLOAD_URL=$(curl -s https://api.github.com/repos/$REPO/releases/latest | grep "browser_download_url" | grep -m 1 "$BINARY_NAME" | cut -d '"' -f 4)

if [ -z "$DOWNLOAD_URL" ]; then
    echo "Error: Could not retrieve the latest binary from GitHub."
    echo "Please ensure a release asset matching '$BINARY_NAME' exists."
    exit 1
fi

echo "Downloading $BINARY_NAME..."
curl -L -o $BINARY_NAME "$DOWNLOAD_URL"
chmod +x $BINARY_NAME

# 4. Install the binary system-wide
echo "Installing to $INSTALL_DIR (sudo required)..."
sudo mv $BINARY_NAME "$INSTALL_DIR/$BINARY_NAME"

# 5. Create and enable the systemd user service for the background daemon
echo "Configuring the background clipboard watcher daemon..."
mkdir -p "$SYSTEMD_USER_DIR"

cat <<EOF > "$SYSTEMD_USER_DIR/$SERVICE_NAME"
[Unit]
Description=Suchi Clipboard Watcher Daemon
After=graphical-session.target

[Service]
Type=simple
# Using --foreground because systemd handles backgrounding and tracking automatically
ExecStart=$INSTALL_DIR/$BINARY_NAME --demon --foreground
Restart=always
RestartSec=3
[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now "$SERVICE_NAME"

echo "=================================================================="
echo "Installation Successful!"
echo ""
echo "• Binary Location:  $INSTALL_DIR/$BINARY_NAME"
echo "• History Store:    ~/.cache/suchi/history.json"
echo "• Daemon Status:    RUNNING (managed by systemd)"
echo "=================================================================="// Random modification at 1787029216.70715
// Random modification at 1787029216.781842
// Random modification at 1787029216.804693
// Random modification at 1787029216.83508
// Random modification at 1787029216.948715
// Random modification at 1787029217.038221
// Random modification at 1787029217.104975
