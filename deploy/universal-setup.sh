#!/usr/bin/env bash
# Camera Network Monitor – Installation (alle Architekturen)
#
#   ./setup.sh [Optionen]      geführte Installation als root (Optionen: --help)
#                              (Debian: "su -", Ubuntu: "sudo ./setup.sh")
#
# Wählt das Paket passend zur Architektur dieses Servers (amd64, arm64 oder
# armv7) und startet dessen setup.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7*|armhf) ARCH=arm ;;
  *) echo "Nicht unterstützte Architektur: $(uname -m) (unterstützt: x86_64, aarch64, armv7)" >&2; exit 1 ;;
esac
PKG="$DIR/linux-$ARCH"
[ -x "$PKG/setup.sh" ] || { echo "Paket für $ARCH fehlt in $DIR – Archiv vollständig entpacken." >&2; exit 1; }
exec "$PKG/setup.sh" "$@"
