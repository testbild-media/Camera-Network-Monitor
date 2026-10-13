#!/usr/bin/env bash
# Camera Network Monitor – installer / updater (Debian/Ubuntu, systemd).
#
#   sudo ./install.sh [--no-apt] [--force] [--no-start] [--with-display]
#
#   --with-display  additionally set up the local display output (the
#                   server's own monitors show multiviewers as kiosks):
#                   sway + browser, user cnm-display, PAM file, display unit
#
# Upgrade contract:
#   1. pre-flight: the NEW binary verifies that the existing config.json and
#      master.key are readable (before anything is stopped or replaced)
#   2. stop the service
#   3. replace program files in /opt/camera-network-monitor (previous version
#      kept as /opt/camera-network-monitor.prev for rollback)
#   4. NEVER touch config.json / master.key in /var/lib/camera-network-monitor
#      (a local data directory from the package or the old installation is
#      imported once, only when no service data exists yet)
#   5. install / update the systemd unit, enable and start, health check
#
# Test mode: CNM_ROOT=/some/dir ./install.sh --no-apt
#   installs below $CNM_ROOT without root, without user creation and without
#   systemctl (used by the automated installer test).
set -euo pipefail
# Debian: "su" without "-" keeps the user's PATH (no /usr/sbin) – useradd,
# update-initramfs etc. live there
for _d in /usr/local/sbin /usr/sbin /sbin /usr/local/bin /usr/bin /bin; do
  case ":${PATH:-}:" in *":$_d:"*) ;; *) PATH="${PATH:+$PATH:}$_d" ;; esac
done
export PATH

ROOT="${CNM_ROOT:-}"
APP_DIR="$ROOT/opt/camera-network-monitor"
DATA_DIR="$ROOT/var/lib/camera-network-monitor"
UNIT_DIR="$ROOT/etc/systemd/system"
SERVICE="camera-network-monitor.service"
DISPLAY_SERVICE="camera-network-monitor-display.service"
DISPLAY_USER="cnm-display"
DISPLAY_HOME="/var/lib/camera-network-monitor-display"
PAM_DIR="$ROOT/etc/pam.d"
SVC_USER="cnm"
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

APT=1
FORCE=0
START=1
WITH_DISPLAY=0
for a in "$@"; do
  case "$a" in
    --no-apt) APT=0 ;;
    --force) FORCE=1 ;;
    --no-start) START=0 ;;
    --with-display) WITH_DISPLAY=1 ;;
    -h|--help) awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; exit 0 ;;
    *) echo "unbekannte Option: $a" >&2; exit 2 ;;
  esac
done

info() { printf '\033[1;33m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; }
die()  { warn "$*"; exit 1; }

TEST_MODE=0
if [ -n "$ROOT" ]; then
  TEST_MODE=1
  info "Testmodus: Installation unter $ROOT (ohne systemd, ohne Benutzeranlage)"
elif [ "$(id -u)" -ne 0 ]; then
  die "Bitte als root ausführen (als root anmelden bzw. \"su -\" oder \"sudo $0\")."
fi

[ -x "$PKG_DIR/bin/cnm" ] || die "Paket unvollständig: $PKG_DIR/bin/cnm fehlt."
"$PKG_DIR/bin/cnm" version >/dev/null 2>&1 || die "bin/cnm ist nicht für diese Architektur ($(uname -m)) gebaut."
NEW_VERSION="$("$PKG_DIR/bin/cnm" version)"
info "Installiere $NEW_VERSION"

# ---- 1. pre-flight compatibility check (before anything changes) -----------------
find_import_source() {
  for cand in "$PKG_DIR/data" "$APP_DIR/data" "$APP_DIR/.data"; do
    if [ -f "$cand/config.json" ]; then echo "$cand"; return 0; fi
  done
  return 1
}

if [ -f "$DATA_DIR/config.json" ]; then
  info "Prüfe bestehende Konfiguration in $DATA_DIR"
  if ! "$PKG_DIR/bin/cnm" check-config --data-dir "$DATA_DIR"; then
    if [ "$FORCE" -eq 1 ]; then
      warn "Kompatibilitätsprüfung fehlgeschlagen – wird wegen --force fortgesetzt."
    else
      die "Kompatibilitätsprüfung fehlgeschlagen – nichts wurde verändert. (Mit --force trotzdem installieren.)"
    fi
  fi
elif SRC="$(find_import_source)"; then
  info "Prüfe zu importierende lokale Daten in $SRC"
  "$PKG_DIR/bin/cnm" check-config --data-dir "$SRC" || { [ "$FORCE" -eq 1 ] || die "Lokale Daten in $SRC sind nicht kompatibel – nichts wurde verändert."; }
fi

# ---- runtime dependencies -------------------------------------------------------------
if [ "$TEST_MODE" -eq 0 ]; then
  command -v systemctl >/dev/null || die "systemd wird benötigt."
  if ! id "$SVC_USER" >/dev/null 2>&1; then
    info "Lege Systembenutzer $SVC_USER an"
    useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SVC_USER"
  fi
  if ! command -v ffmpeg >/dev/null 2>&1; then
    if [ "$APT" -eq 1 ] && command -v apt-get >/dev/null; then
      info "Installiere FFmpeg (für Playback)"
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ffmpeg || warn "FFmpeg konnte nicht installiert werden – Playback ist ohne FFmpeg nicht verfügbar."
    else
      warn "FFmpeg fehlt – Playback ist ohne FFmpeg nicht verfügbar (apt-get install ffmpeg)."
    fi
  fi
fi

# ---- 2. stop service ------------------------------------------------------------------
WAS_ACTIVE=0
if [ "$TEST_MODE" -eq 0 ] && systemctl is-active --quiet "$SERVICE"; then
  WAS_ACTIVE=1
  info "Stoppe $SERVICE"
  systemctl stop "$SERVICE" || warn "Stoppen meldete einen Fehler – fahre fort."
fi

# ---- 3. program files -------------------------------------------------------------------
info "Installiere Programmdateien nach $APP_DIR"
mkdir -p "$(dirname "$APP_DIR")"
STAGE="$APP_DIR.new"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/deploy"
install -m 0755 "$PKG_DIR/bin/cnm" "$STAGE/bin/cnm"
[ -f "$PKG_DIR/bin/go2rtc" ] && install -m 0755 "$PKG_DIR/bin/go2rtc" "$STAGE/bin/go2rtc"
[ -f "$PKG_DIR/bin/cnm-sim" ] && install -m 0755 "$PKG_DIR/bin/cnm-sim" "$STAGE/bin/cnm-sim"
install -m 0644 "$PKG_DIR/deploy/$SERVICE" "$STAGE/deploy/$SERVICE"
for f in "$DISPLAY_SERVICE" camera-network-monitor-display.pam; do
  [ -f "$PKG_DIR/deploy/$f" ] && install -m 0644 "$PKG_DIR/deploy/$f" "$STAGE/deploy/$f"
done
install -m 0755 "$PKG_DIR/install.sh" "$STAGE/install.sh"
[ -f "$PKG_DIR/setup.sh" ] && install -m 0755 "$PKG_DIR/setup.sh" "$STAGE/setup.sh"
[ -f "$PKG_DIR/uninstall.sh" ] && install -m 0755 "$PKG_DIR/uninstall.sh" "$STAGE/uninstall.sh"
[ -d "$PKG_DIR/docs" ] && cp -r "$PKG_DIR/docs" "$STAGE/docs"
for f in README.md CHANGELOG.md LICENSE THIRD_PARTY_NOTICES.md VERSION; do
  [ -f "$PKG_DIR/$f" ] && install -m 0644 "$PKG_DIR/$f" "$STAGE/$f"
done
if [ -d "$APP_DIR" ]; then
  rm -rf "$APP_DIR.prev"
  mv "$APP_DIR" "$APP_DIR.prev"
  info "Vorherige Version gesichert in $APP_DIR.prev"
fi
mv "$STAGE" "$APP_DIR"

# ---- 4. data directory (never overwrite config.json / master.key) ------------------------
mkdir -p "$DATA_DIR"
if [ ! -f "$DATA_DIR/config.json" ]; then
  SRC=""
  for cand in "$PKG_DIR/data" "$APP_DIR.prev/data" "$APP_DIR.prev/.data"; do
    if [ -f "$cand/config.json" ]; then SRC="$cand"; break; fi
  done
  if [ -n "$SRC" ]; then
    info "Importiere vorhandene lokale Daten einmalig aus $SRC"
    for f in config.json config.json.bak master.key; do
      [ -f "$SRC/$f" ] && [ ! -e "$DATA_DIR/$f" ] && cp -p "$SRC/$f" "$DATA_DIR/$f"
    done
  fi
else
  info "Bestehende Daten in $DATA_DIR bleiben unverändert"
fi
chmod 0750 "$DATA_DIR"
for f in "$DATA_DIR"/config.json* "$DATA_DIR"/master.key "$DATA_DIR"/go2rtc.yaml; do
  [ -f "$f" ] && chmod 0600 "$f"
done
if [ "$TEST_MODE" -eq 0 ]; then
  chown -R "$SVC_USER:$SVC_USER" "$DATA_DIR"
fi

# ---- 5. systemd unit ------------------------------------------------------------------------
mkdir -p "$UNIT_DIR"
install -m 0644 "$PKG_DIR/deploy/$SERVICE" "$UNIT_DIR/$SERVICE"
# the display unit is (re)installed on request and kept up to date afterwards
DISPLAY_SETUP=0
if [ "$WITH_DISPLAY" -eq 1 ] || [ -f "$UNIT_DIR/$DISPLAY_SERVICE" ]; then
  DISPLAY_SETUP=1
  mkdir -p "$PAM_DIR"
  install -m 0644 "$PKG_DIR/deploy/$DISPLAY_SERVICE" "$UNIT_DIR/$DISPLAY_SERVICE"
  install -m 0644 "$PKG_DIR/deploy/camera-network-monitor-display.pam" "$PAM_DIR/camera-network-monitor-display"
fi
if [ "$TEST_MODE" -eq 1 ]; then
  info "Testmodus abgeschlossen."
  exit 0
fi

# ---- 5b. local display output (optional) -----------------------------------------------------
is_snap() {
  local real
  real="$(readlink -f "$1" 2>/dev/null || echo "$1")"
  case "$real" in /snap/*) return 0 ;; esac
  [ "$(stat -c %s "$real" 2>/dev/null || echo 99999)" -lt 16384 ] && grep -q "/snap/bin/" "$real" 2>/dev/null
}
have_browser() {
  for b in chromium chromium-browser google-chrome-stable google-chrome firefox-esr firefox; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
have_chromium() {
  for b in chromium chromium-browser google-chrome-stable google-chrome; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
have_firefox() {
  for b in firefox-esr firefox; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
apt_has() { apt-cache policy "$1" 2>/dev/null | grep -q 'Candidate: [0-9]'; }
if [ "$DISPLAY_SETUP" -eq 1 ]; then
  info "Richte lokale Display-Ausgabe ein"
  if [ "$APT" -eq 1 ] && command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends sway kbd fonts-dejavu-core \
      || warn "sway konnte nicht installiert werden – die Display-Ausgabe startet ohne sway nicht."
    # kiosk browsers: Firefox (default) and Chromium (alternative) where .deb packages exist
    if ! have_firefox && apt_has firefox-esr; then
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends firefox-esr \
        || warn "firefox-esr konnte nicht installiert werden."
    fi
    if ! have_chromium && apt_has chromium; then
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends chromium \
        || warn "chromium konnte nicht installiert werden."
    fi
  fi
  if ! have_browser; then
    warn "Kein geeigneter Browser (Firefox oder Chromium als .deb, kein Snap) gefunden."
    warn "Ubuntu: Firefox aus dem Mozilla-APT-Repository installieren – Anleitung in docs/OPERATIONS.md (Abschnitt „Wall displays“)."
  fi
  if ! id "$DISPLAY_USER" >/dev/null 2>&1; then
    info "Lege Systembenutzer $DISPLAY_USER an"
    useradd --system --user-group --create-home --home-dir "$DISPLAY_HOME" --shell /usr/sbin/nologin "$DISPLAY_USER"
  fi
  for g in video render input; do
    getent group "$g" >/dev/null && usermod -aG "$g" "$DISPLAY_USER"
  done
fi
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null
if [ "$START" -eq 0 ]; then
  info "Installiert (nicht gestartet)."
  exit 0
fi
info "Starte $SERVICE"
systemctl start "$SERVICE"
if [ "$DISPLAY_SETUP" -eq 1 ]; then
  # only --with-display enables the service; an update just refreshes the unit
  # and never re-enables a display service the admin switched off
  [ "$WITH_DISPLAY" -eq 1 ] && systemctl enable "$DISPLAY_SERVICE" >/dev/null
  # setup.sh starts the kiosk output itself at the very end
  if [ "${CNM_NO_DISPLAY_START:-0}" != 1 ] && systemctl is-enabled --quiet "$DISPLAY_SERVICE"; then
    info "Starte $DISPLAY_SERVICE"
    systemctl restart "$DISPLAY_SERVICE" || warn "Display-Dienst startet nicht: journalctl -u $DISPLAY_SERVICE -n 50"
  fi
fi

PORT="$(grep -o '"listenPort": *[0-9]*' "$DATA_DIR/config.json" 2>/dev/null | grep -o '[0-9]*$' || true)"
PORT="${PORT:-8090}"
for i in $(seq 1 30); do
  if command -v curl >/dev/null && curl -fsS "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; then
    info "CNM läuft: http://$(hostname -I 2>/dev/null | awk '{print $1}'):$PORT/"
    break
  fi
  if ! systemctl is-active --quiet "$SERVICE"; then
    sleep 1
  fi
  sleep 1
  if [ "$i" -eq 30 ]; then
    warn "CNM antwortet nicht. Log: journalctl -u $SERVICE -n 100"
    exit 1
  fi
done

cat <<EOT

  Fertig. Wichtige Hinweise:
  • Daten:   $DATA_DIR (config.json + master.key immer GEMEINSAM sichern)
  • Logs:    journalctl -u $SERVICE -f
  • Rollback: $APP_DIR.prev enthält die vorherige Programmversion
  • Displays: Einstellungen → Displays (Einrichtung mit --with-display)
  • Erstes Login: Ist noch kein Benutzer angelegt, die Ersteinrichtung direkt
    am Server über http://127.0.0.1:$PORT/ aufrufen (nur localhost).
EOT
