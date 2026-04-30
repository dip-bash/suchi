#!/bin/bash

BINARY_NAME="suchi"
INSTALL_DIR="/usr/local/bin"

echo "Starting uninstallation of $BINARY_NAME..."

if [ -f "$INSTALL_DIR/$BINARY_NAME" ]; then
    echo "Removing binary from $INSTALL_DIR (sudo required)..."
    sudo rm "$INSTALL_DIR/$BINARY_NAME"
    
    if [ $? -eq 0 ]; then
        echo "Successfully removed $BINARY_NAME."
    else
        echo "Error: Failed to remove $BINARY_NAME. Check your permissions."
        exit 1
    fi
else
    echo "Info: $BINARY_NAME was not found in $INSTALL_DIR."
fi

echo "Uninstallation complete."
