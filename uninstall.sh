#!/usr/bin/env bash
# suchi uninstaller - removes the binary. Your clipboard history and snippets
# are kept.
#
#   curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash
#
set -euo pipefail

BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"

SUDO=""
if [ "$EUID" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
    fi
fi

TARGET="${INSTALL_DIR}/${BINARY_NAME}"
if [ ! -f "$TARGET" ]; then
    if command -v suchi >/dev/null 2>&1; then
        TARGET="$(command -v suchi)"
    else
        echo "no suchi binary found in ${INSTALL_DIR} or PATH"
        exit 0
    fi
fi

# stop the daemon if it's running
if command -v suchi >/dev/null 2>&1; then
    suchi stop >/dev/null 2>&1 || true
fi

echo "==> Removing $TARGET..."
if [ -n "$SUDO" ]; then
    $SUDO rm -f "$TARGET"
else
    rm -f "$TARGET"
fi

echo "==> suchi is uninstalled (your clipboard history and snippets are kept)"
