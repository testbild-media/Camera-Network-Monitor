#!/usr/bin/env bash
# Camera Network Monitor @VERSION@ – Installer (eine Datei, alle Architekturen)
#
#   bash camera-network-monitor-@VERSION@-installer.run [Optionen]   (als root:
#   Debian "su -", Ubuntu "sudo bash …")
#
# Prüft die Datei, entpackt das passende Paket (amd64, arm64 oder armv7) und
# startet die geführte Installation (setup.sh). Optionen werden an setup.sh
# weitergegeben (--help zeigt sie). Nur entpacken: --extract [ZIEL]
set -euo pipefail
VERSION="@VERSION@"
PAYLOAD_SHA256="@PAYLOAD_SHA256@"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7*|armhf) ARCH=arm ;;
  *) echo "Nicht unterstützte Architektur: $(uname -m)" >&2; exit 1 ;;
esac
NAME="camera-network-monitor-$VERSION-linux-$ARCH"
SKIP="$(awk '/^__CNM_PAYLOAD__$/ { print NR + 1; exit }' "$0")"
payload() { tail -n +"$SKIP" "$0"; }

printf 'Camera Network Monitor %s – prüfe Installationsdatei … ' "$VERSION"
if command -v sha256sum >/dev/null; then
  [ "$(payload | sha256sum | cut -d' ' -f1)" = "$PAYLOAD_SHA256" ] || { echo "FEHLER: Datei beschädigt oder unvollständig – bitte erneut herunterladen." >&2; exit 1; }
fi
echo "ok"

if [ "${1:-}" = "--extract" ]; then
  DEST="${2:-.}"
  mkdir -p "$DEST"
  payload | tar xz -C "$DEST" "$NAME"
  echo "Entpackt: $DEST/$NAME (Installation als root: $DEST/$NAME/setup.sh)"
  exit 0
fi

# /tmp is often mounted noexec – extract below /opt when running as root
if [ "$(id -u)" -eq 0 ]; then
  mkdir -p /opt
  WORK="$(mktemp -d /opt/.cnm-installer.XXXXXX)"
else
  WORK="$(mktemp -d)"
fi
trap 'rm -rf "$WORK"' EXIT
payload | tar xz -C "$WORK" "$NAME"
set +e
"$WORK/$NAME/setup.sh" "$@"
rc=$?
exit $rc
__CNM_PAYLOAD__
