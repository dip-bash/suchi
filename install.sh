#!/usr/bin/env bash
# suchi installer - downloads the latest release binary for your CPU and
# installs it to /usr/local/bin (no root needed if you own that dir).
#
#   curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash
#
set -euo pipefail

REPO="dip-bash/suchi"
BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"

ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)
        BIN_FILE="suchi-linux-x86_64"
        ;;
    aarch64|arm64)
        BIN_FILE="suchi-linux-aarch64"
        ;;
    *)
        echo "Error: Unsupported Linux architecture ($ARCH). Suchi supports x86_64 and aarch64 only."
        exit 1
        ;;
esac

SUDO=""
if [ "$EUID" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
    else
        echo "Error: Root privileges or sudo are required to install into ${INSTALL_DIR}."
        exit 1
    fi
fi

URL="https://github.com/${REPO}/releases/latest/download/${BIN_FILE}"
echo "==> Downloading Suchi ($BIN_FILE)..."
TMP_FILE=$(mktemp)
trap 'rm -f "$TMP_FILE"' EXIT

if ! curl --fail --location --progress-bar "$URL" --output "$TMP_FILE"; then
    echo "Error: Failed to download binary from $URL"
    exit 1
fi

if [ ! -s "$TMP_FILE" ]; then
    echo "Error: Downloaded file is empty."
    exit 1
fi

echo "==> Installing to ${INSTALL_DIR}/${BINARY_NAME}..."
if [ -n "$SUDO" ]; then
    $SUDO install -m 755 "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
else
    install -m 755 "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
fi

echo "==> Suchi installed successfully!"
echo "Run 'suchi daemon' to start the clipboard watcher."
