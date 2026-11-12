#!/usr/bin/env bash
# suchi uninstaller - stops the daemon and removes the binary and the systemd
# unit. Your clipboard history and snippets are kept unless you pass --purge.
#
#   curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash
#   curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash -s -- --purge

set -euo pipefail

PURGE=0
DIRS=()

if [ -t 1 ]; then B=$'\033[1m'; C=$'\033[1;36m'; Y=$'\033[1;33m'; Z=$'\033[0m'; else B='' C='' Y='' Z=''; fi
say()  { printf '%s==>%s %s\n' "$C" "$Z" "$*"; }
warn() { printf '%swarning:%s %s\n' "$Y" "$Z" "$*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
  cat <<'EOT'
suchi uninstaller

  curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash
  curl -sSL .../uninstall.sh | bash -s -- --purge

Options:
  --purge       also delete your clipboard history, snippets and logs
  --dir DIR     also look for the binary in DIR
  -h, --help

Environment: SUCHI_INSTALL_DIR
EOT
}

while [ $# -gt 0 ]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    --dir)   [ $# -ge 2 ] || { echo "error: --dir needs a value" >&2; exit 1; }; DIRS+=("$2"); shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown option: $1 (try --help)" >&2; exit 1 ;;
  esac
done

[ -n "${SUCHI_INSTALL_DIR:-}" ] && DIRS+=("$SUCHI_INSTALL_DIR")
DIRS+=("$HOME/.local/bin" "/usr/local/bin")
[ -n "${PREFIX:-}" ] && DIRS+=("$PREFIX/bin")
if have suchi; then DIRS+=("$(dirname "$(command -v suchi)")"); fi

# find every installed copy (no duplicates)
FOUND=()
for d in "${DIRS[@]}"; do
  f="$d/suchi"
  [ -f "$f" ] || continue
  dup=0
  for g in "${FOUND[@]+"${FOUND[@]}"}"; do [ "$g" = "$f" ] && dup=1; done
  [ "$dup" = 1 ] || FOUND+=("$f")
done

# stop the daemon first so it does not hold files open or restart itself
if have systemctl && systemctl --user --version >/dev/null 2>&1; then
  systemctl --user disable --now suchi >/dev/null 2>&1 || true
fi
if [ "${#FOUND[@]}" -gt 0 ]; then
  "${FOUND[0]}" stop >/dev/null 2>&1 || true
fi

UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/suchi.service"
if [ -f "$UNIT" ]; then
  rm -f "$UNIT"
  if have systemctl; then systemctl --user daemon-reload >/dev/null 2>&1 || true; fi
  say "removed the systemd user unit"
fi

if [ "${#FOUND[@]}" -eq 0 ]; then
  warn "no suchi binary found in: ${DIRS[*]}"
else
  for f in "${FOUND[@]}"; do
    if rm -f "$f" 2>/dev/null; then say "removed $f"
    else warn "could not remove $f (try with sudo)"; fi
  done
fi

DATA="${XDG_DATA_HOME:-$HOME/.local/share}/suchi"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/suchi"
if [ "$PURGE" = 1 ]; then
  rm -rf "$DATA" "$STATE"
  say "deleted $DATA and $STATE"
else
  if [ -d "$DATA" ]; then
    echo "${B}Kept${Z} your clipboard history and snippets in $DATA"
    echo "     delete them too with:  curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash -s -- --purge"
  fi
fi
say "suchi is uninstalled"
