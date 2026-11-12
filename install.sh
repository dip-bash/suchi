#!/usr/bin/env bash
# suchi installer - downloads the release binary for your CPU, verifies its
# checksum and installs it. No root needed.
#
#   curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash
#
# Options (after `bash -s --` when piping):
#   --version X.Y.Z   install a specific release (default: latest)
#   --dir DIR         install into DIR (default: ~/.local/bin; /usr/local/bin as
#                     root; $PREFIX/bin on Termux)
#   --no-service      do not install the systemd user unit
#   -h, --help
#
# Environment: SUCHI_VERSION, SUCHI_INSTALL_DIR, SUCHI_BASE_URL (mirror).
#
#   curl -sSL .../install.sh | bash -s -- --version 0.5.0 --dir ~/bin

set -euo pipefail

REPO="dip-bash/suchi"
BASE_URL="${SUCHI_BASE_URL:-https://github.com}"
VERSION="${SUCHI_VERSION:-latest}"
INSTALL_DIR="${SUCHI_INSTALL_DIR:-}"
WITH_SERVICE=1

if [ -t 1 ]; then B=$'\033[1m'; C=$'\033[1;36m'; Y=$'\033[1;33m'; R=$'\033[1;31m'; Z=$'\033[0m'; else B='' C='' Y='' R='' Z=''; fi
say()  { printf '%s==>%s %s\n' "$C" "$Z" "$*"; }
warn() { printf '%swarning:%s %s\n' "$Y" "$Z" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$R" "$Z" "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
  cat <<'EOT'
suchi installer

  curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash
  curl -sSL .../install.sh | bash -s -- --version 0.5.0 --dir ~/bin

Options:
  --version X.Y.Z   install a specific release (default: latest)
  --dir DIR         install into DIR (default: ~/.local/bin; /usr/local/bin as
                    root; $PREFIX/bin on Termux)
  --no-service      do not install the systemd user unit
  -h, --help

Environment: SUCHI_VERSION, SUCHI_INSTALL_DIR, SUCHI_BASE_URL (mirror).
EOT
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version|-v) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --dir)        [ $# -ge 2 ] || die "--dir needs a value";     INSTALL_DIR="$2"; shift 2 ;;
    --no-service) WITH_SERVICE=0; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown option: $1 (try --help)" ;;
  esac
done

# ---- platform -------------------------------------------------------------------

[ "$(uname -s)" = "Linux" ] || die "suchi supports Linux only (this is $(uname -s))"

case "$(uname -m)" in
  x86_64|amd64)                    ARCH=amd64 ;;
  aarch64|arm64)                   ARCH=arm64 ;;
  armv7l|armv8l|armv6l|armv7*|arm) ARCH=arm ;;
  i386|i486|i586|i686|x86)         ARCH=386 ;;
  *) die "unsupported CPU: $(uname -m) (suchi ships amd64, arm64, arm and 386)" ;;
esac
ASSET="suchi-linux-$ARCH"

# Termux sets PREFIX to its own root; a normal Linux shell does not.
IS_TERMUX=0
case "${PREFIX:-}" in */com.termux/*) IS_TERMUX=1 ;; esac

if [ -z "$INSTALL_DIR" ]; then
  if   [ "$IS_TERMUX" = 1 ];        then INSTALL_DIR="$PREFIX/bin"
  elif [ "$(id -u)" = 0 ];          then INSTALL_DIR="/usr/local/bin"
  else                                   INSTALL_DIR="$HOME/.local/bin"
  fi
fi

# ---- download -------------------------------------------------------------------

if   have curl; then fetch() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }
elif have wget; then fetch() { wget -q --tries=3 --timeout=15 -O "$2" "$1"; }
else die "need curl or wget to download suchi"; fi

if [ "$VERSION" = "latest" ]; then
  URL_BASE="$BASE_URL/$REPO/releases/latest/download"
else
  URL_BASE="$BASE_URL/$REPO/releases/download/v${VERSION#v}"
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/suchi-install.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

sha256_of() {
  if   have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  elif have shasum;    then shasum -a 256 "$1" | cut -d' ' -f1
  elif have openssl;   then openssl dgst -sha256 "$1" | sed 's/^.*= //'
  else return 1; fi
}

say "installing suchi ${VERSION} for linux/${ARCH}"
if fetch "$URL_BASE/$ASSET" "$TMP/suchi"; then
  if fetch "$URL_BASE/checksums.txt" "$TMP/checksums.txt" 2>/dev/null; then
    want="$(awk -v f="$ASSET" '$2 == f || $2 == "*" f { print $1 }' "$TMP/checksums.txt")"
    if [ -z "$want" ]; then
      warn "checksums.txt has no entry for $ASSET; skipping verification"
    elif got="$(sha256_of "$TMP/suchi")"; then
      [ "$got" = "$want" ] || die "checksum mismatch for $ASSET (expected $want, got $got) - not installing"
      say "checksum verified"
    else
      warn "no sha256 tool found; skipping verification"
    fi
  else
    warn "could not download checksums.txt; skipping verification"
  fi
  chmod +x "$TMP/suchi"
  "$TMP/suchi" version >/dev/null 2>&1 || die "the downloaded binary does not run on this machine"
else
  # No prebuilt binary (yet): build from source if Go is available.
  have go || die "could not download $URL_BASE/$ASSET and Go is not installed to build from source.
       Check the version exists: https://github.com/$REPO/releases"
  warn "no prebuilt binary found; building from source with Go"
  ref="latest"; [ "$VERSION" = "latest" ] || ref="v${VERSION#v}"
  mkdir -p "$INSTALL_DIR"
  CGO_ENABLED=0 GOBIN="$INSTALL_DIR" go install "github.com/$REPO/cmd/suchi@$ref" \
    || die "go install failed. Check that version '$VERSION' exists: https://github.com/$REPO/releases"
  cp "$INSTALL_DIR/suchi" "$TMP/suchi"
fi

# ---- install --------------------------------------------------------------------

DEST="$INSTALL_DIR/suchi"
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR (try --dir, or run as root)"
OLD=""
[ -x "$DEST" ] && OLD="$("$DEST" version 2>/dev/null | awk '{print $2}' || true)"
if [ "$TMP/suchi" != "$DEST" ]; then
  if have install; then install -m 0755 "$TMP/suchi" "$DEST"; else cp "$TMP/suchi" "$DEST"; chmod 0755 "$DEST"; fi
fi
NEW="$("$DEST" version | awk '{print $2}')"
if [ -n "$OLD" ] && [ "$OLD" != "$NEW" ]; then say "upgraded suchi $OLD -> $NEW ($DEST)"
elif [ -n "$OLD" ]; then say "reinstalled suchi $NEW ($DEST)"
else say "installed suchi $NEW to $DEST"; fi

# ---- systemd user unit (optional) -----------------------------------------------

SERVICE_NOTE=""
if [ "$WITH_SERVICE" = 1 ] && [ "$IS_TERMUX" = 0 ] && [ "$(id -u)" != 0 ] && have systemctl \
   && systemctl --user --version >/dev/null 2>&1; then
  UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/suchi.service" <<EOF
[Unit]
Description=suchi clipboard history watcher
After=graphical-session.target
PartOf=graphical-session.target

[Service]
ExecStart=$DEST daemon --foreground
Restart=on-failure
RestartSec=2

[Install]
WantedBy=graphical-session.target
EOF
  systemctl --user daemon-reload >/dev/null 2>&1 || true
  SERVICE_NOTE="  start it at every login:  ${B}systemctl --user enable --now suchi${Z}"
fi

# ---- hints ----------------------------------------------------------------------

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) warn "$INSTALL_DIR is not in your PATH. Add this to your shell profile:
         export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac

if [ -z "${WAYLAND_DISPLAY:-}" ] && [ -n "${DISPLAY:-}" ] && ! have xclip && ! have xsel; then
  if   have apt-get; then pkg="sudo apt install xclip"
  elif have dnf;     then pkg="sudo dnf install xclip"
  elif have pacman;  then pkg="sudo pacman -S xclip"
  elif have apk;     then pkg="sudo apk add xclip"
  elif have zypper;  then pkg="sudo zypper install xclip"
  else pkg="install the xclip package"; fi
  warn "you are on X11 and xclip is missing; suchi needs it to copy. Run: $pkg"
fi

cat <<EOF

${B}Next steps${Z}
  suchi daemon     start recording your clipboard
  suchi            open the interface (Tab switches Clip / Snip, ? for help)
$SERVICE_NOTE
  uninstall:       curl -sSL https://raw.githubusercontent.com/$REPO/main/uninstall.sh | bash
EOF
