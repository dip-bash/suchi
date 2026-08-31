# Contributing to suchi

Thank you for contributing to **suchi**!

## Quick Setup

```bash
# Install dependencies (Debian/Ubuntu)
sudo apt update && sudo apt install build-essential xclip libx11-dev libxfixes-dev

# Clone repository
git clone https://github.com/dip-bash/suchi.git
cd suchi
```

## Guidelines

* **Storage:** Data persists at `~/.cache/suchi/history.json`.

## How to Contribute

1. **Issues:** Search existing issues before creating a bug report or feature request.
2. **Pull Requests:**
   - Fork & create a branch (`git checkout -b feature/name`).
   - Write clean code and verify zero build warnings (`-Wall -Wextra`).
   - Test both TUI (`suchi`) and background daemon (`suchi --demon`).
   - Push and submit a PR against `main`.
