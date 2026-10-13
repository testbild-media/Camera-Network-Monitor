#!/usr/bin/env bash
# Camera Network Monitor – uninstaller.
#
#   sudo ./uninstall.sh            remove program + service, KEEP data
#   sudo ./uninstall.sh --purge    also delete /var/lib/camera-network-monitor
set -euo pipefail
# Debian: "su" without "-" keeps the user's PATH (no /usr/sbin) – useradd,
# update-initramfs etc. live there
for _d in /usr/local/sbin /usr/sbin /sbin /usr/local/bin /usr/bin /bin; do
  case ":${PATH:-}:" in *":$_d:"*) ;; *) PATH="${PATH:+$PATH:}$_d" ;; esac
done
export PATH
SERVICE="camera-network-monitor.service"
DISPLAY_SERVICE="camera-network-monitor-display.service"
APP_DIR="/opt/camera-network-monitor"
DATA_DIR="/var/lib/camera-network-monitor"
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1
[ "$(id -u)" -eq 0 ] || { echo "Bitte als root ausführen." >&2; exit 1; }

systemctl disable --now "$DISPLAY_SERVICE" 2>/dev/null || true
systemctl disable --now "$SERVICE" 2>/dev/null || true
rm -f "/etc/systemd/system/$SERVICE" "/etc/systemd/system/$DISPLAY_SERVICE" /etc/pam.d/camera-network-monitor-display
rm -f /etc/systemd/system/camera-network-monitor.service.d/10-display-gpu.conf
rmdir /etc/systemd/system/camera-network-monitor.service.d 2>/dev/null || true
if [ -f /etc/systemd/system/camera-network-monitor-display.service.d/10-seat.conf ]; then
  # the display card was moved to seat1 by setup.sh – give it back to seat0
  rm -f /etc/systemd/system/camera-network-monitor-display.service.d/10-seat.conf
  loginctl flush-devices || true
fi
rmdir /etc/systemd/system/camera-network-monitor-display.service.d 2>/dev/null || true
systemctl daemon-reload || true
rm -rf "$APP_DIR" "$APP_DIR.prev" "$APP_DIR.new"
if [ "$PURGE" -eq 1 ]; then
  read -r -p "Daten in $DATA_DIR inkl. master.key endgültig löschen? (ja/nein) " ans
  if [ "$ans" = "ja" ]; then
    rm -rf "$DATA_DIR"
    echo "Daten gelöscht."
  fi
else
  echo "Programm entfernt. Daten bleiben erhalten: $DATA_DIR"
fi
echo "Die Systembenutzer 'cnm' und ggf. 'cnm-display' bleiben bestehen (userdel zum Entfernen)."
