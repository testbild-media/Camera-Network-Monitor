#!/usr/bin/env bash
# Camera Network Monitor – Komplett-Installation (Debian/Ubuntu)
#
#   ./setup.sh                 geführte Installation als root (fragt alles am Anfang)
#   ./setup.sh --yes [...]     unbeaufsichtigt mit Standardwerten
#   (root werden: Debian "su -", Ubuntu "sudo ./setup.sh")
#
# Optionen:
#   --yes, -y              keine Rückfragen, Standardwerte verwenden
#   --display / --no-display
#                          lokale Display-Ausgabe einrichten / nicht einrichten
#   --gpu=TREIBER          auto | intel | amd | nvidia | nouveau | generic | none
#   --display-gpu=KARTE    Grafikkarte für die Displays (card1 oder PCI 0000:01:00.0),
#                          nur bei mehreren Grafikkarten
#   --admin-user=NAME      erster Administrator (Passwort aus $CNM_ADMIN_PASSWORD)
#   --no-assign            angeschlossene Monitore nicht automatisch belegen
#   --firefox-repo / --no-firefox-repo
#                          Ubuntu: Firefox aus dem Mozilla-Repository installieren
#   --open-firewall / --no-firewall
#   --reboot / --no-reboot Neustart am Ende (falls nötig) ohne Rückfrage
#   --dry-run              nur anzeigen, was getan würde (kein root nötig)
#   --configure-only       nur Administrator anlegen / Monitore belegen bei
#                          bereits laufendem CNM (keine Pakete); --port=N
#
# Was passiert:
#   1. Fragen (Displays, Grafiktreiber, Browser, Admin, Firewall) + Zusammenfassung
#   2. Abhängigkeiten (FFmpeg, curl, python3, pciutils …)
#   3. Grafiktreiber (Intel/AMD/NVIDIA/VM) inkl. benötigter Paketquellen
#   4. Browser für die Kiosk-Ausgabe: Debian Firefox ESR und Chromium; Ubuntu Firefox (.deb aus dem
#      Mozilla-Repository – Chromium gibt es dort nur als Snap)
#   5. CNM selbst über install.sh (Dienst, Daten, Updates/Rollback wie gewohnt)
#   6. erster Administrator, Monitor-Zuweisung, Firewall, ggf. Neustart
set -euo pipefail
# Debian: "su" without "-" keeps the user's PATH (no /usr/sbin) – useradd,
# update-initramfs etc. live there
for _d in /usr/local/sbin /usr/sbin /sbin /usr/local/bin /usr/bin /bin; do
  case ":${PATH:-}:" in *":$_d:"*) ;; *) PATH="${PATH:+$PATH:}$_d" ;; esac
done
export PATH

PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG="${CNM_SETUP_LOG:-/var/log/camera-network-monitor-setup.log}"
SYSFS="${CNM_SETUP_SYSFS:-/sys/class/drm}"
OS_RELEASE="${CNM_SETUP_OS_RELEASE:-/etc/os-release}"
DATA_DIR="${CNM_SETUP_DATA_DIR:-/var/lib/camera-network-monitor}"
APP_BIN="/opt/camera-network-monitor/bin/cnm"

YES=0; DRY=0; CONFIG_ONLY=0
OPT_DISPLAY=""; OPT_GPU=""; OPT_DGPU=""; OPT_ADMIN=""; OPT_ASSIGN=""; OPT_FFREPO=""; OPT_FW=""; OPT_REBOOT=""; OPT_PORT=""
for a in "$@"; do
  case "$a" in
    -y|--yes) YES=1 ;;
    --display) OPT_DISPLAY=yes ;;
    --no-display) OPT_DISPLAY=no ;;
    --gpu=*) OPT_GPU="${a#--gpu=}" ;;
    --display-gpu=*) OPT_DGPU="${a#--display-gpu=}" ;;
    --admin-user=*) OPT_ADMIN="${a#--admin-user=}" ;;
    --no-assign) OPT_ASSIGN=no ;;
    --firefox-repo) OPT_FFREPO=yes ;;
    --no-firefox-repo) OPT_FFREPO=no ;;
    --open-firewall) OPT_FW=yes ;;
    --no-firewall) OPT_FW=no ;;
    --reboot) OPT_REBOOT=yes ;;
    --no-reboot) OPT_REBOOT=no ;;
    --dry-run) DRY=1 ;;
    --configure-only) CONFIG_ONLY=1 ;;
    --port=*) OPT_PORT="${a#--port=}" ;;
    -h|--help) awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; exit 0 ;;
    *) echo "Unbekannte Option: $a (siehe --help)" >&2; exit 2 ;;
  esac
done
case "$OPT_GPU" in ""|auto|intel|amd|nvidia|nouveau|generic|none) ;; *) echo "--gpu: auto|intel|amd|nvidia|nouveau|generic|none" >&2; exit 2 ;; esac

# ---- output helpers ----------------------------------------------------------------------
if [ -t 1 ]; then B=$'\033[1m'; Y=$'\033[1;33m'; G=$'\033[1;32m'; R=$'\033[1;31m'; D=$'\033[2m'; N=$'\033[0m'; else B= Y= G= R= D= N=; fi
STEP=0
# the Linux text console has no glyphs for ✓ ← … → use plain ASCII there
if [ "${TERM:-}" = linux ]; then SYM_OK="OK"; SYM_ERR="XX"; SYM_L="<-"; SYM_R="->"; SYM_DOT="*"; ELL="..."
else SYM_OK="✓"; SYM_ERR="✗"; SYM_L="←"; SYM_R="→"; SYM_DOT="•"; ELL="…"; fi
# everything shown is also written to the log – synchronously, so the order
# on screen is always right (prompts never overtake earlier output)
logp() {
  [ "${LOGGING:-0}" -eq 1 ] || return 0
  printf '%s\n' "$*" | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' >>"$LOG" 2>/dev/null || true
}
step() { STEP=$((STEP+1)); printf '\n%s[%d] %s%s\n' "$B" "$STEP" "$*" "$N"; logp ""; logp "[$STEP] $*"; }
info() { printf '%s==>%s %s\n' "$Y" "$N" "$*"; logp "==> $*"; }
ok()   { printf '%s %s %s %s\n' "$G" "$SYM_OK" "$N" "$*"; logp " $SYM_OK $*"; }
warn() { printf '%s !! %s %s\n' "$R" "$N" "$*" >&2; WARNINGS+=("$*"); logp " !! $*"; }
die()  { printf '%s !! %s %s\n' "$R" "$N" "$*" >&2; logp " !! $*"; exit 1; }
WARNINGS=()

# run executes a short command (or only prints it in --dry-run mode)
run() {
  if [ "$DRY" -eq 1 ]; then printf '%s   $ %s%s\n' "$D" "$*" "$N"; return 0; fi
  "$@" </dev/null
}

elapsed() { local t=$((SECONDS - $1)); printf '%d:%02d' $((t / 60)) $((t % 60)); }

# run_step runs a long command in its own session: it never reads from the
# keyboard (stdin is /dev/null), its output goes to the log, and a live status
# line shows the elapsed time and its last output line (e.g. apt waiting for a
# lock). Ctrl+C stops it deliberately (see on_interrupt).
CHILD=""
run_step() { # run_step "Beschreibung" command args…
  local label="$1"; shift
  if [ "$DRY" -eq 1 ]; then printf '%s   $ %s%s\n' "$D" "$*" "$N"; return 0; fi
  local out start=$SECONDS rc=0 i=0 last spin='|/-\'
  out="$(mktemp)"
  setsid "$@" </dev/null >"$out" 2>&1 &
  CHILD=$!
  while kill -0 "$CHILD" 2>/dev/null; do
    if [ "$HAVE_TTY" -eq 1 ]; then
      last="$(tail -n 1 "$out" 2>/dev/null | tr -d '\r\033' | cut -c1-70 || true)"
      printf '\r\033[K   %s %s  %s  %s%s%s' "${spin:$((i % 4)):1}" "$label" "$(elapsed "$start")" "$D" "$last" "$N" >&3
      i=$((i + 1))
    elif [ "$SECONDS" -gt "$start" ] && [ $(((SECONDS - start) % 30)) -eq 0 ]; then
      printf '   %s %s läuft noch (%s)\n' "$ELL" "$label" "$(elapsed "$start")" >&3
    fi
    sleep 1
  done
  wait "$CHILD" || rc=$?
  CHILD=""
  [ "$HAVE_TTY" -eq 1 ] && printf '\r\033[K' >&3
  if [ "$LOGGING" -eq 1 ]; then { echo "---- $label: $*"; cat "$out"; } >>"$LOG" 2>/dev/null || true; fi
  if [ "$rc" -eq 0 ]; then
    ok "$label ($(elapsed "$start"))"
  else
    printf '%s %s %s %s – Fehler (Code %s). Letzte Ausgabe:\n' "$R" "$SYM_ERR" "$N" "$label" "$rc"
    tail -n 25 "$out" | sed 's/^/     /'
    logp " $SYM_ERR $label – Fehler (Code $rc)"
  fi
  rm -f "$out"
  return "$rc"
}

on_interrupt() {
  trap - INT TERM
  printf '\n' >&3
  if [ -n "$CHILD" ] && kill -0 "$CHILD" 2>/dev/null; then
    printf 'Abbruch – beende den laufenden Schritt %s\n' "$ELL" >&3
    kill -TERM -- "-$CHILD" 2>/dev/null || kill -TERM "$CHILD" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do kill -0 "$CHILD" 2>/dev/null || break; sleep 1; done
    kill -KILL -- "-$CHILD" 2>/dev/null || kill -KILL "$CHILD" 2>/dev/null || true
  fi
  printf 'Setup abgebrochen. Es kann gefahrlos erneut gestartet werden – unterbrochene\nPaketinstallationen werden dabei automatisch repariert.\n' >&3
  exit 130
}
trap on_interrupt INT TERM

# prompts go to the terminal even when the output is logged; without a
# terminal (pipes, CI) answers are read from stdin
# (CNM_SETUP_STDIN=1 forces stdin, e.g. for automated tests)
# fd 3 = prompt output (opened once; reopening /dev/stderr would truncate a log file)
HAVE_TTY=0; LOGGING=0
if [ "${CNM_SETUP_STDIN:-0}" != 1 ] && { : >/dev/tty; } 2>/dev/null; then TTY=/dev/tty; HAVE_TTY=1; exec 3>/dev/tty; else TTY=/dev/stdin; exec 3>&2; fi
ask_yn() { # ask_yn "Frage" y|n  → 0 = ja
  local q="$1" def="$2" ans hint="[J/n]"
  [ "$def" = n ] && hint="[j/N]"
  if [ "$YES" -eq 1 ]; then [ "$def" = y ]; return; fi
  while true; do
    printf '%s?%s %s %s ' "$B" "$N" "$q" "$hint" >&3
    read -r ans <"$TTY" || ans=""
    case "${ans,,}" in
      "") [ "$def" = y ]; return ;;
      j|ja|y|yes) return 0 ;;
      n|nein|no) return 1 ;;
    esac
  done
}
ask_text() { # ask_text "Frage" default → stdout
  local q="$1" def="$2" ans
  if [ "$YES" -eq 1 ]; then echo "$def"; return; fi
  printf '%s?%s %s [%s] ' "$B" "$N" "$q" "$def" >&3
  read -r ans <"$TTY" || ans=""
  echo "${ans:-$def}"
}
ask_secret() { # ask_secret "Frage" → stdout (not echoed); fails at end of input
  local ans rc=0
  printf '%s?%s %s ' "$B" "$N" "$1" >&3
  read -r -s ans <"$TTY" || rc=1
  printf '\n' >&3
  echo "$ans"
  return "$rc"
}

# tool output is parsed in the C locale (German: "Installationskandidat:")
apt_has() { local out; out="$(LC_ALL=C apt-cache policy "$1" 2>/dev/null || true)"; grep -q 'Candidate: [0-9]' <<<"$out"; }
# apt waits (visibly) for a lock held by e.g. automatic updates instead of failing
APT_OPTS=(-o DPkg::Lock::Timeout=1200 -o Acquire::Retries=3 -o Acquire::http::Timeout=30
  -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)
APT_UPDATED=0
apt_update() {
  [ "$APT_UPDATED" -eq 1 ] && return 0
  run_step "Paketlisten aktualisieren" apt-get "${APT_OPTS[@]}" update \
    || die "Die Paketlisten konnten nicht geladen werden – Internetverbindung bzw. Paketquellen (/etc/apt) prüfen. Details: $LOG"
  APT_UPDATED=1
}
# apt_install [--required] PAKETE… installs the available packages; with
# --required, missing or failing packages abort the setup
apt_install() {
  local required=0 want=() missing=() p label
  [ "${1:-}" = --required ] && { required=1; shift; }
  for p in "$@"; do
    if [ "$DRY" -eq 1 ] || apt_has "$p"; then want+=("$p"); else missing+=("$p"); fi
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    [ "$required" -eq 1 ] && die "Benötigte Pakete fehlen in den Paketquellen: ${missing[*]}"
    warn "Nicht in den Paketquellen verfügbar (übersprungen): ${missing[*]}"
  fi
  [ "${#want[@]}" -eq 0 ] && return 0
  label="Installiere ${want[*]}"
  [ "${#label}" -gt 48 ] && label="${label:0:45}$ELL"
  if ! run_step "$label" apt-get "${APT_OPTS[@]}" install -y --no-install-recommends "${want[@]}"; then
    [ "$required" -eq 1 ] && die "Installation fehlgeschlagen: ${want[*]} (Details oben und in $LOG)"
    warn "Installation fehlgeschlagen: ${want[*]}"
  fi
  return 0
}

# ---- system checks ------------------------------------------------------------------------
printf '\n%sCamera Network Monitor – Installation%s\n' "$B" "$N"
printf '%sPaket: %s%s\n' "$D" "$PKG_DIR" "$N"
if [ "$DRY" -eq 0 ] && [ "$CONFIG_ONLY" -eq 0 ]; then
  [ "$(id -u)" -eq 0 ] || die "Bitte als root ausführen: als root anmelden bzw. \"su -\" (Debian) oder \"sudo $0\""
fi
if [ "$DRY" -eq 0 ] && [ "$(id -u)" -eq 0 ]; then
  mkdir -p "$(dirname "$LOG")"
  LOGGING=1
  # tee ignores Ctrl+C so the abort message and cleanup still reach the screen
  echo "---- $(date -Is) setup $*" >>"$LOG"
fi
[ -x "$PKG_DIR/install.sh" ] && [ -x "$PKG_DIR/bin/cnm" ] || die "Paket unvollständig (install.sh/bin/cnm fehlen) – bitte das vollständige Paket entpacken."
"$PKG_DIR/bin/cnm" version >/dev/null 2>&1 || die "bin/cnm passt nicht zu dieser Architektur ($(uname -m))."
[ -r "$OS_RELEASE" ] || die "$OS_RELEASE fehlt – nur Debian/Ubuntu werden unterstützt."
# shellcheck disable=SC1090
. "$OS_RELEASE"
DISTRO="${ID:-}"
case " ${ID:-} ${ID_LIKE:-} " in
  *" ubuntu "*) DISTRO=ubuntu ;;
  *" debian "*) DISTRO=debian ;;
  *) die "Nicht unterstütztes System: ${PRETTY_NAME:-unbekannt} (Debian oder Ubuntu erforderlich)" ;;
esac
if [ "$DRY" -eq 0 ] && [ "$CONFIG_ONLY" -eq 0 ]; then
  command -v systemctl >/dev/null || die "systemd wird benötigt."
  command -v apt-get >/dev/null || die "apt-get wird benötigt."
fi
info "System: ${PRETTY_NAME:-$DISTRO} ($(uname -m))"
cfg_port() { grep -o '"listenPort": *[0-9]*' "$DATA_DIR/config.json" 2>/dev/null | grep -o '[0-9]*$' || true; }
PORT="${OPT_PORT:-$(cfg_port)}"
PORT="${PORT:-8090}"
FRESH=1
[ -f "$DATA_DIR/config.json" ] && FRESH=0
# an existing configuration may still have no user at all (e.g. an earlier,
# interrupted setup) – then the first administrator is asked for as well
HAS_USERS=0
if [ -f "$DATA_DIR/config.json" ] && ! grep -q '"users": \[\]' "$DATA_DIR/config.json" && grep -q '"users"' "$DATA_DIR/config.json"; then
  HAS_USERS=1
fi
if [ "$CONFIG_ONLY" -eq 1 ] && [ "$DRY" -eq 0 ]; then
  me="$(curl -sS "http://127.0.0.1:$PORT/api/auth/me" 2>/dev/null || true)"
  [ -n "$me" ] || die "CNM antwortet nicht auf Port $PORT (läuft der Dienst? Port mit --port=N angeben)"
  FRESH=0; HAS_USERS=1
  if printf '%s' "$me" | python3 -c 'import sys,json;sys.exit(0 if json.load(sys.stdin).get("needsBootstrap") else 1)'; then FRESH=1; HAS_USERS=0; fi
fi
[ "$FRESH" -eq 0 ] && [ "$CONFIG_ONLY" -eq 0 ] && info "Bestehende Installation gefunden – Update (Daten bleiben erhalten)"

# ---- hardware detection ---------------------------------------------------------------------
declare -A VENDOR_NAME=([0x8086]=Intel [0x1002]=AMD [0x1022]=AMD [0x10de]=NVIDIA [0x1af4]=virtio [0x15ad]=VMware
  [0x1234]=QEMU [0x1b36]=QXL [0x80ee]=VirtualBox [0x1414]=Hyper-V [0x1a03]=ASPEED [0x102b]=Matrox)
# per graphics card: name, label, driver family, PCI address, console flag,
# connectors and connected monitors
GPUS=(); CARD_NAME=(); CARD_KIND=(); CARD_PCI=(); CARD_BOOT=(); CARD_PORTS=(); CARD_CONN=()
for card in "$SYSFS"/card[0-9]*; do
  [ -d "$card" ] || continue
  name="$(basename "$card")"
  [[ "$name" =~ ^card[0-9]+$ ]] || continue
  v="$(cat "$card/device/vendor" 2>/dev/null || true)"
  d="$(cat "$card/device/device" 2>/dev/null || true)"
  label="Grafikkarte${v:+ ($v)}"
  if [ -n "$v" ] && [ -n "${VENDOR_NAME[$v]:-}" ]; then label="${VENDOR_NAME[$v]}"; fi
  slot="$(basename "$(readlink -f "$card/device" 2>/dev/null || echo "")")"
  if command -v lspci >/dev/null && [ -n "$slot" ]; then
    desc="$(lspci -s "$slot" 2>/dev/null | sed -n '1s/^[^:]*: //p' || true)"
    [ -n "$desc" ] && label="$label – $desc"
  fi
  case "$v" in
    0x10de) kind=nvidia ;;
    0x1002|0x1022) kind=amd ;;
    0x8086) kind=intel ;;
    *) kind=generic ;;
  esac
  ports=(); conn=()
  for c in "$SYSFS/$name"-*; do
    [ -d "$c" ] || continue
    cn="${c##*/$name-}"
    [[ "$cn" == Writeback* ]] && continue
    ports+=("$cn")
    [ "$(cat "$c/status" 2>/dev/null)" = connected ] && conn+=("$cn")
  done
  CARD_NAME+=("$name"); CARD_KIND+=("$kind"); CARD_PCI+=("$slot")
  CARD_BOOT+=("$(cat "$card/device/boot_vga" 2>/dev/null || echo 0)")
  CARD_PORTS+=("${ports[*]}"); CARD_CONN+=("${conn[*]}")
  GPUS+=("$name: $label${v:+ [${v#0x}:${d#0x}]}")
done
NCARDS=${#CARD_NAME[@]}
# a card is the console when the firmware booted from it (boot_vga); without
# that information the first card counts as console
CONSOLE_IDX=-1
for i in "${!CARD_NAME[@]}"; do [ "${CARD_BOOT[$i]}" = 1 ] && { CONSOLE_IDX=$i; break; }; done
[ "$CONSOLE_IDX" -lt 0 ] && [ "$NCARDS" -gt 0 ] && CONSOLE_IDX=0
# recommended display card: a dedicated GPU (Intel/AMD/NVIDIA) that is not the
# console, else the card with the most connected monitors
DISP_IDX=-1
for i in "${!CARD_NAME[@]}"; do
  [ "$i" -ne "$CONSOLE_IDX" ] && [ "${CARD_KIND[$i]}" != generic ] && { DISP_IDX=$i; break; }
done
if [ "$DISP_IDX" -lt 0 ] && [ "$NCARDS" -gt 0 ]; then
  best=-1; DISP_IDX=0
  for i in "${!CARD_NAME[@]}"; do
    n=$(wc -w <<<"${CARD_CONN[$i]}")
    [ "$n" -gt "$best" ] && { best=$n; DISP_IDX=$i; }
  done
fi
if [ -n "$OPT_DGPU" ]; then
  want="$(tr 'A-Z' 'a-z' <<<"${OPT_DGPU#pci-}")"
  found=-1
  for i in "${!CARD_NAME[@]}"; do
    case "$want" in "${CARD_NAME[$i]}"|"${CARD_PCI[$i]}"|"${CARD_PCI[$i]#0000:}") found=$i ;; esac
  done
  [ "$found" -ge 0 ] || die "--display-gpu=$OPT_DGPU: keine solche Grafikkarte"
  DISP_IDX=$found
fi
# kind and monitors of the chosen display card
set_display_card() {
  GPU_KIND="${CARD_KIND[$DISP_IDX]:-}"
  read -r -a CONNECTED <<<"${CARD_CONN[$DISP_IDX]:-}"
}
GPU_KIND=""; CONNECTED=()
[ "$DISP_IDX" -ge 0 ] && set_display_card
ALL_CONNECTED=()
for i in "${!CARD_NAME[@]}"; do read -r -a tmp <<<"${CARD_CONN[$i]}"; ALL_CONNECTED+=("${tmp[@]}"); done

# ---- 1. questions ---------------------------------------------------------------------------
step "Fragen zur Einrichtung"
if [ "${#GPUS[@]}" -gt 0 ]; then
  # one line per card with its monitors, so it is clear which monitor hangs where
  info "Erkannte Grafikhardware:"
  for i in "${!GPUS[@]}"; do
    mon="${CARD_CONN[$i]:-}"; mon="${mon// /, }"
    tag=""; [ "$NCARDS" -ge 2 ] && [ "$i" -eq "$CONSOLE_IDX" ] && tag="  [Konsole]"
    if [ -n "$mon" ]; then mtext="Monitor: $mon"; else mtext="kein Monitor angeschlossen"; fi
    printf '      %s – %s%s\n' "${GPUS[$i]}" "$mtext" "$tag"
    logp "      ${GPUS[$i]} – $mtext$tag"
  done
else
  info "Keine Grafikhardware erkannt (Server ohne GPU oder virtuelle Maschine ohne Grafik)"
fi

DO_DISPLAY=no
DEF_DISPLAY=n; [ "${#GPUS[@]}" -gt 0 ] && [ "${#ALL_CONNECTED[@]}" -gt 0 ] && DEF_DISPLAY=y
systemctl is-enabled --quiet camera-network-monitor-display 2>/dev/null && DEF_DISPLAY=y
if [ -n "$OPT_DISPLAY" ]; then DO_DISPLAY="$OPT_DISPLAY"
elif ask_yn "Sollen an diesem Server angeschlossene Monitore Multiviewer anzeigen (lokale Displays)?$([ "$NCARDS" -ge 2 ] && echo " (welche Grafikkarte, wird gleich gefragt)")" "$DEF_DISPLAY"; then DO_DISPLAY=yes; fi

SEAT_MODE=no
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && [ "$NCARDS" -ge 2 ]; then
  if [ -z "$OPT_DGPU" ] && [ "$YES" -eq 0 ]; then
    printf '%s?%s Welche Grafikkarte soll die Monitore mit den Multiviewern ansteuern?\n' "$B" "$N" >&3
    for i in "${!CARD_NAME[@]}"; do
      ports="${CARD_PORTS[$i]:-keine Anschlüsse}"
      [ -n "${CARD_CONN[$i]}" ] && ports="$ports – Monitor an: ${CARD_CONN[$i]}"
      tags=""
      [ "$i" -eq "$CONSOLE_IDX" ] && tags="$tags [Konsole]"
      [ "$i" -eq "$DISP_IDX" ] && tags="$tags  $SYM_L empfohlen"
      printf '      %d) %s\n         Anschlüsse: %s%s\n' "$((i+1))" "${GPUS[$i]}" "$ports" "$tags" >&3
    done
    while true; do
      printf '    Auswahl [%d]: ' "$((DISP_IDX+1))" >&3
      read -r ans <"$TTY" || ans=""
      ans="${ans:-$((DISP_IDX+1))}"
      if [[ "$ans" =~ ^[0-9]+$ ]] && [ "$ans" -ge 1 ] && [ "$ans" -le "$NCARDS" ]; then DISP_IDX=$((ans-1)); break; fi
    done
  fi
  set_display_card
  # a card other than the console gets its own seat: no console switching,
  # the console on the other card stays usable at all times
  [ "$DISP_IDX" -ne "$CONSOLE_IDX" ] && SEAT_MODE=yes
fi

GPU=none
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ]; then
  GPU="${OPT_GPU:-auto}"
  if [ "$GPU" = auto ]; then
    GPU="${GPU_KIND:-generic}"
    # updates of an existing display setup keep the installed driver
    [ -z "$OPT_GPU" ] && [ -f /etc/systemd/system/camera-network-monitor-display.service ] && GPU=none
  fi
  if [ -z "$OPT_GPU" ] && [ "$YES" -eq 0 ]; then
    opts=(intel amd nvidia nouveau generic none)
    labels=("Intel (Mesa, VA-API-Videobeschleunigung)"
            "AMD (Mesa, Firmware, VA-API)"
            "NVIDIA – proprietärer Treiber (beste Leistung, Neustart nötig)"
            "NVIDIA – freier Treiber nouveau (ohne Zusatzpakete, geringere Leistung)"
            "Virtuelle Maschine / Server-Grafik (ASPEED, Matrox …) / sonstige (Mesa)"
            "Nichts installieren (Treiber bereits eingerichtet)")
    printf '%s?%s Welcher Grafiktreiber soll installiert werden?\n' "$B" "$N" >&3
    def_i=1
    for i in "${!opts[@]}"; do
      mark=""; [ "${opts[$i]}" = "$GPU" ] && { mark="  $SYM_L erkannt"; def_i=$((i+1)); }
      printf '      %d) %s%s\n' "$((i+1))" "${labels[$i]}" "$mark" >&3
    done
    while true; do
      printf '    Auswahl [%d]: ' "$def_i" >&3
      read -r ans <"$TTY" || ans=""
      ans="${ans:-$def_i}"
      if [[ "$ans" =~ ^[1-6]$ ]]; then GPU="${opts[$((ans-1))]}"; break; fi
    done
  fi
fi

# browser for the kiosk output (snap browsers cannot be used)
is_snap() {
  local real; real="$(readlink -f "$1" 2>/dev/null || echo "$1")"
  case "$real" in /snap/*) return 0 ;; esac
  [ "$(stat -c %s "$real" 2>/dev/null || echo 99999)" -lt 16384 ] && grep -q "/snap/bin/" "$real" 2>/dev/null
}
have_browser() {
  local b p
  for b in chromium chromium-browser google-chrome-stable google-chrome firefox-esr firefox; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
# kiosk browsers: Firefox (default) and Chromium – both are offered in the
# display settings, so both are installed where .deb packages exist
have_chromium() {
  local b p
  for b in chromium chromium-browser google-chrome-stable google-chrome; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
have_firefox() {
  local b p
  for b in firefox-esr firefox; do
    p="$(command -v "$b" 2>/dev/null)" || continue
    is_snap "$p" || return 0
  done
  return 1
}
FFREPO=no
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && [ "$DISTRO" = ubuntu ] && ! have_firefox; then
  if [ -n "$OPT_FFREPO" ]; then FFREPO="$OPT_FFREPO"
  elif ask_yn "Ubuntu liefert Browser nur als Snap. Firefox aus dem offiziellen Mozilla-Repository installieren?" y; then FFREPO=yes; fi
fi

# administrator: the first one (no users yet) or – on request – create one /
# reset a forgotten password on an existing installation
ADMIN_USER=""; ADMIN_PASS="${CNM_ADMIN_PASSWORD:-}"; ADMIN_MODE=""
if [ "$HAS_USERS" -eq 0 ]; then
  ADMIN_MODE=bootstrap
  [ "$FRESH" -eq 0 ] && info "Die bestehende Installation hat noch keinen Benutzer."
  if [ -n "$OPT_ADMIN" ]; then ADMIN_USER="$OPT_ADMIN"
  elif [ "$YES" -eq 1 ]; then [ -n "$ADMIN_PASS" ] && ADMIN_USER=admin
  elif ask_yn "Jetzt den ersten Administrator anlegen? (sonst später im Browser direkt am Server)" y; then
    ADMIN_USER="$(ask_text "Benutzername" admin)"
  fi
elif [ "$CONFIG_ONLY" -eq 0 ]; then
  ADMIN_MODE=reset
  if [ -n "$OPT_ADMIN" ]; then ADMIN_USER="$OPT_ADMIN"
  elif [ "$YES" -eq 0 ] && ask_yn "Es gibt bereits Benutzer. Einen Administrator anlegen oder ein Passwort zurücksetzen (z. B. vergessen)?" n; then
    ADMIN_USER="$(ask_text "Benutzername" admin)"
  fi
fi
if [ -n "$ADMIN_USER" ]; then
  if [ -z "$ADMIN_PASS" ]; then
    if [ "$YES" -eq 1 ]; then warn "Kein Passwort in CNM_ADMIN_PASSWORD – Administrator wird später im Browser angelegt"; ADMIN_USER=""
    else
      while true; do
        if ! ADMIN_PASS="$(ask_secret "Passwort für $ADMIN_USER (mind. 8 Zeichen):")"; then
          warn "Keine Eingabe möglich – Administrator wird später im Browser angelegt"; ADMIN_USER=""; ADMIN_PASS=""; break
        fi
        [ "${#ADMIN_PASS}" -ge 8 ] || { echo "    zu kurz" >&3; continue; }
        [ "$(ask_secret "Passwort wiederholen:" || true)" = "$ADMIN_PASS" ] && break
        echo "    Passwörter stimmen nicht überein" >&3
      done
    fi
  fi
  [ -n "$ADMIN_USER" ] && [ "${#ADMIN_PASS}" -lt 8 ] && die "Das Administrator-Passwort muss mindestens 8 Zeichen haben."
fi

ASSIGN=no
if [ "$DO_DISPLAY" = yes ] && [ -n "$ADMIN_USER" ] && [ "${#CONNECTED[@]}" -gt 0 ] && [ "$OPT_ASSIGN" != no ]; then
  if ask_yn "Angeschlossene Monitore (${CONNECTED[*]}) gleich mit dem Standard-Multiviewer belegen? (später in Einstellungen $SYM_R Displays änderbar)" y; then ASSIGN=yes; fi
fi

FIREWALL=no
if [ "$CONFIG_ONLY" -eq 0 ] && command -v ufw >/dev/null && grep -q "Status: active" <<<"$(LC_ALL=C ufw status 2>/dev/null || true)"; then
  if [ -n "$OPT_FW" ]; then FIREWALL="$OPT_FW"
  elif ask_yn "Die Firewall (ufw) ist aktiv. Port $PORT/tcp für die Weboberfläche freigeben?" y; then FIREWALL=yes; fi
fi

DM=""
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ]; then
  for s in gdm3 gdm lightdm sddm lxdm xdm; do
    if systemctl is-enabled "$s" >/dev/null 2>&1; then DM="$s"; break; fi
  done
fi
DISABLE_DM=no
if [ -n "$DM" ]; then
  if ask_yn "Ein grafischer Login ($DM) ist aktiv und belegt die Bildschirme. Für die Kiosk-Ausgabe deaktivieren (wirksam nach Neustart)?" y; then DISABLE_DM=yes; fi
fi

print_summary() {
printf '\n%sZusammenfassung%s\n' "$B" "$N"
if [ "$CONFIG_ONLY" -eq 1 ]; then printf '  Modus:            nur Konfiguration (Port %s)\n' "$PORT"
else printf '  Installation:     %s\n' "$([ "$FRESH" -eq 1 ] && echo "neu" || echo "Update (Daten bleiben erhalten)")"; fi
printf '  Lokale Displays:  %s\n' "$([ "$DO_DISPLAY" = yes ] && echo ja || echo nein)"
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && [ "$NCARDS" -ge 2 ]; then
  printf '  Display-Karte:    %s (PCI %s)%s\n' "${CARD_NAME[$DISP_IDX]}" "${CARD_PCI[$DISP_IDX]}" \
    "$([ "$SEAT_MODE" = yes ] && echo " – eigener Seat, Konsole bleibt auf ${CARD_NAME[$CONSOLE_IDX]}")"
fi
[ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && printf '  Grafiktreiber:    %s\n' "$GPU"
[ "$FFREPO" = yes ] && printf '  Browser:          Firefox (Mozilla-Repository)\n'
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && [ "$DISTRO" = debian ] && { ! have_firefox || ! have_chromium; }; then
  printf '  Browser:          Firefox ESR und Chromium (Debian)\n'
fi
if [ -n "$ADMIN_USER" ] && [ "$ADMIN_MODE" = reset ]; then printf '  Administrator:    %s (anlegen bzw. Passwort setzen)\n' "$ADMIN_USER"
elif [ -n "$ADMIN_USER" ]; then printf '  Administrator:    %s (neu)\n' "$ADMIN_USER"
elif [ "$HAS_USERS" -eq 1 ]; then printf '  Administrator:    unverändert\n'
else printf '  Administrator:    später im Browser\n'; fi
[ "$ASSIGN" = yes ] && printf '  Monitore belegen: %s\n' "${CONNECTED[*]}"
[ "$FIREWALL" = yes ] && printf '  Firewall:         Port %s/tcp freigeben\n' "$PORT"
[ "$DISABLE_DM" = yes ] && printf '  Grafischer Login: %s deaktivieren\n' "$DM"
return 0
}
print_summary
[ "$LOGGING" -eq 1 ] && print_summary | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' >>"$LOG"
if [ "$YES" -eq 0 ] && ! ask_yn "Installation jetzt starten?" y; then echo "Abgebrochen – nichts wurde verändert."; exit 0; fi

REBOOT_NEEDED=0
if [ "$CONFIG_ONLY" -eq 0 ]; then

# ---- 2. dependencies --------------------------------------------------------------------------
# no package tool may ever wait for keyboard input (debconf, needrestart, ucf …)
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1 \
  APT_LISTCHANGES_FRONTEND=none UCF_FORCE_CONFFOLD=1
step "Abhängigkeiten installieren"
if pgrep -x unattended-upgr >/dev/null 2>&1 || pgrep -x apt-get >/dev/null 2>&1 || pgrep -x dpkg >/dev/null 2>&1; then
  info "Eine andere Paketinstallation läuft gerade (z. B. automatische Updates) – das Setup wartet darauf."
fi
if [ -n "$(ls -A /var/lib/dpkg/updates 2>/dev/null)" ]; then
  run_step "Unterbrochene Paketinstallation reparieren" dpkg --configure -a || warn "dpkg --configure -a meldete Fehler"
fi
apt_update
apt_install --required ca-certificates curl python3 tar
apt_install pciutils ffmpeg

# ---- 3. graphics driver -----------------------------------------------------------------------
# debian_components adds components (e.g. non-free-firmware) to the Debian sources
debian_components() {
  # exact component tokens ("non-free" is not part of "non-free-firmware");
  # deb822 (Components: lines) and classic deb/deb-src lines
  local f tmp changed=0
  for f in "${APT_ETC:-/etc/apt}/sources.list.d/debian.sources" "${APT_ETC:-/etc/apt}/sources.list"; do
    [ -f "$f" ] || continue
    tmp="$(mktemp)"
    awk -v add="$*" '
      BEGIN { n = split(add, want, " ") }
      /^Components:/ || (/^deb(-src)?[ \t]/ && /debian/) {
        line = $0
        for (k = 1; k <= n; k++) {
          have = 0
          for (i = 1; i <= NF; i++) if ($i == want[k]) have = 1
          if (!have) line = line " " want[k]
        }
        print line; next
      }
      { print }' "$f" >"$tmp"
    if ! cmp -s "$f" "$tmp"; then
      info "Paketquellen ergänzt um: $* ($f)"
      if [ "$DRY" -eq 0 ]; then [ -f "$f.cnm-bak" ] || cp -p "$f" "$f.cnm-bak"; cat "$tmp" >"$f"; fi
      changed=1
    fi
    rm -f "$tmp"
  done
  if [ "$changed" -eq 1 ]; then APT_UPDATED=0; apt_update; fi
}
ubuntu_components() {
  local c
  for c in "$@"; do
    apt_install software-properties-common
    run add-apt-repository -y "$c" >/dev/null || warn "Paketquelle $c konnte nicht aktiviert werden"
  done
  APT_UPDATED=0; apt_update
}
MESA=(libgl1-mesa-dri libegl-mesa0 libgbm1 libglx-mesa0 mesa-vulkan-drivers)
install_gpu() {
  case "$GPU" in
    none) info "Grafiktreiber: keine Änderung"; return ;;
    intel)
      [ "$DISTRO" = debian ] && debian_components non-free-firmware
      apt_install "${MESA[@]}" intel-media-va-driver i965-va-driver
      if [ "$DISTRO" = debian ]; then apt_install firmware-misc-nonfree; else apt_install linux-firmware; fi ;;
    amd)
      [ "$DISTRO" = debian ] && debian_components non-free-firmware
      apt_install "${MESA[@]}" mesa-va-drivers
      if [ "$DISTRO" = debian ]; then apt_install firmware-amd-graphics; else apt_install linux-firmware; fi
      REBOOT_NEEDED=1 ;;
    nouveau|generic)
      apt_install "${MESA[@]}" mesa-va-drivers ;;
    nvidia)
      if [ "$DISTRO" = ubuntu ]; then
        apt_has ubuntu-drivers-common || ubuntu_components restricted multiverse
        apt_install ubuntu-drivers-common
        info "Installiere den empfohlenen NVIDIA-Treiber (ubuntu-drivers)"
        run_step "NVIDIA-Treiber installieren (ubuntu-drivers)" ubuntu-drivers install || warn "ubuntu-drivers konnte keinen Treiber installieren"
      else
        debian_components contrib non-free non-free-firmware
        apt_install "linux-headers-$(uname -r)" nvidia-driver firmware-misc-nonfree
      fi
      apt_install "${MESA[@]}"
      # sway needs kernel mode setting of the NVIDIA driver
      info "Aktiviere nvidia-drm.modeset (Kernel-Modesetting)"
      if [ "$DRY" -eq 1 ]; then run tee /etc/modprobe.d/cnm-nvidia-drm.conf
      else printf 'options nvidia-drm modeset=1 fbdev=1\n' >/etc/modprobe.d/cnm-nvidia-drm.conf; fi
      if [ "$DRY" -eq 1 ] || command -v update-initramfs >/dev/null; then run_step "initramfs aktualisieren" update-initramfs -u || warn "update-initramfs meldete Fehler"; fi
      if command -v mokutil >/dev/null && grep -qi enabled <<<"$(LC_ALL=C mokutil --sb-state 2>/dev/null || true)"; then
        warn "Secure Boot ist aktiv: der NVIDIA-Treiber muss ggf. beim nächsten Start über MOK bestätigt werden (Bildschirmanweisungen folgen)."
      fi
      REBOOT_NEEDED=1 ;;
  esac
  ok "Grafiktreiber ($GPU) eingerichtet"
}
if [ "$DO_DISPLAY" = yes ]; then
  step "Grafiktreiber"
  install_gpu
fi

# ---- 4. browser ---------------------------------------------------------------------------------
if [ "$DO_DISPLAY" = yes ]; then
  step "Browser für die Kiosk-Ausgabe"
  if have_firefox; then
    ok "Firefox vorhanden"
  elif [ "$FFREPO" = yes ]; then
    info "Richte das Mozilla-Repository ein und installiere Firefox (.deb)"
    run install -d -m 0755 /etc/apt/keyrings
    if [ "$DRY" -eq 1 ]; then run curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg -o /etc/apt/keyrings/packages.mozilla.org.asc
    else
      curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg -o /etc/apt/keyrings/packages.mozilla.org.asc
      echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" >/etc/apt/sources.list.d/mozilla.list
      printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' >/etc/apt/preferences.d/mozilla
    fi
    APT_UPDATED=0; apt_update
    # Ubuntu's snap transition package carries an epoch ("1:1snap1"); the
    # pinned Mozilla package replaces it as a formal downgrade
    run_step "Installiere Firefox (Mozilla)" apt-get "${APT_OPTS[@]}" install -y --allow-downgrades --no-install-recommends firefox \
      || warn "Firefox-Installation meldete einen Fehler"
    have_firefox && ok "Firefox installiert" || [ "$DRY" -eq 1 ] || warn "Firefox konnte nicht installiert werden – siehe docs/OPERATIONS.md"
  elif [ "$DISTRO" = debian ]; then
    apt_install firefox-esr
    have_firefox && ok "Firefox ESR installiert" || [ "$DRY" -eq 1 ] || warn "Firefox ESR konnte nicht installiert werden – siehe docs/OPERATIONS.md"
  elif have_browser; then
    ok "Geeigneter Browser vorhanden (Chromium/Chrome)"
  else
    warn "Kein Browser für die Kiosk-Ausgabe – Firefox (.deb) nachinstallieren, siehe docs/OPERATIONS.md"
  fi
  # Chromium as the alternative (Settings → Displays); Ubuntu has it only as snap
  if [ "$DISTRO" = debian ]; then
    if have_chromium; then
      ok "Chromium vorhanden"
    else
      apt_install chromium
      have_chromium && ok "Chromium installiert" || [ "$DRY" -eq 1 ] || warn "Chromium konnte nicht installiert werden"
    fi
  fi
  if [ "$DISABLE_DM" = yes ]; then
    run systemctl disable "$DM" >/dev/null 2>&1 || true
    run systemctl set-default multi-user.target >/dev/null
    info "$DM wird nach dem Neustart nicht mehr gestartet"
    REBOOT_NEEDED=1
  fi
fi

# ---- 5. CNM -------------------------------------------------------------------------------------
step "Camera Network Monitor installieren"
INSTALL_ARGS=()
[ "$DO_DISPLAY" = yes ] && INSTALL_ARGS+=(--with-display)
run_step "Camera Network Monitor installieren" env CNM_NO_DISPLAY_START=1 "$PKG_DIR/install.sh" "${INSTALL_ARGS[@]}" \
  || die "Die Installation von CNM ist fehlgeschlagen (Details oben und in $LOG)."
# the kiosk output takes over the monitor – it starts only at the very end,
# after the summary, so the setup stays readable on a local console
[ "$DO_DISPLAY" = yes ] && run systemctl stop camera-network-monitor-display 2>/dev/null || true
if [ "$DO_DISPLAY" = no ] && [ -f /etc/systemd/system/camera-network-monitor-display.service ]; then
  run systemctl disable --now camera-network-monitor-display >/dev/null 2>&1 || true
  info "Lokale Displays abgeschaltet (Display-Dienst deaktiviert)"
fi

# graphics card for the displays (only with several cards)
CNM_DROPIN=/etc/systemd/system/camera-network-monitor.service.d/10-display-gpu.conf
SEAT_DROPIN=/etc/systemd/system/camera-network-monitor-display.service.d/10-seat.conf
write_file() { # write_file PATH CONTENT
  if [ "$DRY" -eq 1 ]; then printf '%s   # %s:%s\n%s%s\n' "$D" "$1" "$N" "$2" ""; return 0; fi
  mkdir -p "$(dirname "$1")"; printf '%s\n' "$2" >"$1"
}
if [ "$DO_DISPLAY" = yes ]; then
  CHANGED_UNITS=0
  if [ "$NCARDS" -ge 2 ]; then
    step "Grafikkarte für die Displays: ${CARD_NAME[$DISP_IDX]}"
    write_file "$CNM_DROPIN" "# setup.sh: Displays nur an dieser Grafikkarte (PCI-Adresse, stabil über Neustarts)
[Service]
Environment=CNM_DISPLAY_GPU=${CARD_PCI[$DISP_IDX]}"
    CHANGED_UNITS=1
    if [ "$SEAT_MODE" = yes ]; then
      dev="$(readlink -f "$SYSFS/${CARD_NAME[$DISP_IDX]}" 2>/dev/null || echo "$SYSFS/${CARD_NAME[$DISP_IDX]}")"
      run loginctl attach seat1 "$dev" || warn "Grafikkarte konnte seat1 nicht zugeordnet werden"
      write_file "$SEAT_DROPIN" "# setup.sh: Die Displays laufen auf einer eigenen Grafikkarte (seat1).
# Kein Konsolenwechsel – die Konsole an ${CARD_NAME[$CONSOLE_IDX]} bleibt jederzeit bedienbar.
[Service]
TTYPath=
StandardInput=null
TTYReset=no
TTYVHangup=no
TTYVTDisallocate=no
UtmpIdentifier=
Environment=XDG_SEAT=seat1"
      ok "${CARD_NAME[$DISP_IDX]} gehört jetzt zu seat1 – die Konsole bleibt auf ${CARD_NAME[$CONSOLE_IDX]}"
    elif [ -f "$SEAT_DROPIN" ] || [ "$DRY" -eq 1 ]; then
      # back to the console card: undo the earlier seat assignment
      run rm -f "$SEAT_DROPIN"
      run loginctl flush-devices || true
      info "Eigener Seat aufgehoben – die Displays nutzen die Konsolen-Grafikkarte"
    fi
  elif [ -f "$CNM_DROPIN" ]; then
    run rm -f "$CNM_DROPIN"; CHANGED_UNITS=1
  fi
  if [ "$CHANGED_UNITS" -eq 1 ]; then
    run systemctl daemon-reload
    run systemctl restart camera-network-monitor
    if [ "$DRY" -eq 0 ]; then
      for _ in $(seq 1 40); do curl -fsS "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break; sleep 0.5; done
    fi
  fi
fi
PORT="${OPT_PORT:-$(cfg_port)}"
PORT="${PORT:-8090}"

fi # CONFIG_ONLY
BASE="http://127.0.0.1:$PORT"

# ---- 6. administrator, displays, firewall ---------------------------------------------------------
api_json() { # api_json METHOD PATH [json-from-stdin] → body on stdout
  local m="$1" p="$2"; shift 2
  curl -fsS -b "$CJ" -c "$CJ" -H "Content-Type: application/json" ${CSRF:+-H "X-CSRF-Token: $CSRF"} -X "$m" "$@" "$BASE$p"
}
CJ="$(mktemp)"; CSRF=""
trap 'rm -f "$CJ"' EXIT
admin_json() { CNM_U="$ADMIN_USER" CNM_P="$ADMIN_PASS" python3 -c 'import json,os;print(json.dumps({"username":os.environ["CNM_U"],"password":os.environ["CNM_P"]}))'; }
if [ -n "$ADMIN_USER" ]; then
  step "Administrator $ADMIN_USER einrichten"
  if [ "$ADMIN_MODE" = bootstrap ] && [ "$DRY" -eq 0 ]; then
    needs="$(curl -sS "$BASE/api/auth/me" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("needsBootstrap"))' 2>/dev/null || echo "?")"
    if [ "$needs" != True ]; then
      if [ "$CONFIG_ONLY" -eq 1 ]; then warn "Es gibt bereits einen Benutzer – mit --configure-only wird kein Administrator angelegt"; ADMIN_USER=""
      else ADMIN_MODE=reset; fi
    fi
  fi
  if [ -n "$ADMIN_USER" ] && [ "$ADMIN_MODE" = bootstrap ]; then
    if [ "$DRY" -eq 1 ]; then run curl -X POST "$BASE/api/auth/bootstrap"
    else
      resp="$(admin_json | api_json POST /api/auth/bootstrap --data-binary @- 2>&1)" || { warn "Administrator konnte nicht angelegt werden: $resp"; ADMIN_USER=""; }
      if [ -n "$ADMIN_USER" ]; then
        CSRF="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("csrf",""))')"
        ok "Administrator $ADMIN_USER angelegt"
      fi
    fi
  elif [ -n "$ADMIN_USER" ]; then
    # existing users: create the admin / set its password offline (service
    # stopped, as the service user so file ownership stays correct)
    if [ "$DRY" -eq 1 ]; then
      run systemctl stop camera-network-monitor
      run runuser -u cnm -- "$APP_BIN" reset-user --data-dir "$DATA_DIR" --username "$ADMIN_USER"
      run systemctl start camera-network-monitor
    else
      systemctl stop camera-network-monitor
      errf="$(mktemp)"
      if printf '%s\n' "$ADMIN_PASS" | runuser -u cnm -- "$APP_BIN" reset-user --data-dir "$DATA_DIR" --username "$ADMIN_USER" >/dev/null 2>"$errf"; then
        ok "Administrator $ADMIN_USER angelegt bzw. Passwort gesetzt"
      else
        warn "Administrator konnte nicht gesetzt werden: $(tail -n 2 "$errf" | tr '\n' ' ')"; ADMIN_USER=""
      fi
      rm -f "$errf"
      systemctl start camera-network-monitor
      for _ in $(seq 1 40); do curl -fsS "$BASE/api/health" >/dev/null 2>&1 && break; sleep 0.5; done
      if [ -n "$ADMIN_USER" ]; then
        resp="$(admin_json | api_json POST /api/auth/login --data-binary @- 2>/dev/null)" \
          && CSRF="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("csrf",""))')" \
          || warn "Anmeldung als $ADMIN_USER nicht möglich – Monitore bitte später in der Oberfläche zuweisen"
      fi
    fi
  fi
fi
unset ADMIN_PASS CNM_ADMIN_PASSWORD

if [ "$ASSIGN" = yes ] && [ -n "$ADMIN_USER" ] && { [ "$DRY" -eq 1 ] || [ -n "$CSRF" ]; }; then
  step "Monitore mit dem Standard-Multiviewer belegen"
  if [ "$DRY" -eq 1 ]; then run curl -X POST "$BASE/api/settings/displays"
  else
    plan="$(api_json GET /api/settings/displays | python3 -c '
import sys, json
d = json.load(sys.stdin)
mons = [m for m in d.get("monitors", []) if m.get("enabled")] or d.get("monitors", [])
if mons:
    for p in d.get("ports", []):
        if p.get("connected") and not p.get("display"):
            print(json.dumps({"connector": p["connector"], "enabled": True, "monitorId": mons[0]["id"], "mode": "", "rotation": 0}))
')" || plan=""
    n=0
    while IFS= read -r body; do
      [ -n "$body" ] || continue
      if printf '%s' "$body" | api_json POST /api/settings/displays --data-binary @- >/dev/null; then n=$((n+1)); else warn "Zuweisung fehlgeschlagen: $body"; fi
    done <<<"$plan"
    ok "$n Monitor(e) belegt"
  fi
fi

if [ "$FIREWALL" = yes ]; then
  step "Firewall"
  run ufw allow "$PORT/tcp" >/dev/null && ok "Port $PORT/tcp freigegeben"
fi

# ---- summary ------------------------------------------------------------------------------------
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
print_final() {
printf '\n%s%sFertig.%s\n' "$G" "$B" "$N"
printf '  Weboberfläche:  http://%s:%s/\n' "${IP:-<server-ip>}" "$PORT"
if [ -n "$ADMIN_USER" ]; then printf '  Anmeldung:      %s (Passwort wie eingegeben)\n' "$ADMIN_USER"
elif [ "$HAS_USERS" -eq 0 ]; then printf '  Erster Login:   Administrator anlegen: ./setup.sh erneut ausführen oder direkt am Server http://127.0.0.1:%s/ öffnen\n' "$PORT"; fi
[ "$DO_DISPLAY" = yes ] && printf '  Displays:       Einstellungen %s Displays (Status: systemctl status camera-network-monitor-display)\n' "$SYM_R"
printf '  Protokoll:      %s\n' "$LOG"
[ "$DO_DISPLAY" = yes ] && printf '  Konsole:        an diesem Monitor jederzeit mit Strg+Alt+F1 (Kiosk-Ausgabe: Strg+Alt+F7)\n'
if [ "${#WARNINGS[@]}" -gt 0 ]; then
  printf '\n%sHinweise:%s\n' "$Y" "$N"; for w in "${WARNINGS[@]}"; do printf '  %s %s\n' "$SYM_DOT" "$w"; done
fi
return 0
}
print_final
[ "$LOGGING" -eq 1 ] && print_final | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' >>"$LOG"
if [ "$DO_DISPLAY" = yes ] && [ "$CONFIG_ONLY" -eq 0 ] && [ "$REBOOT_NEEDED" -eq 0 ]; then
  # on the server's own console: announce the switch so nobody is surprised
  if [ "$HAVE_TTY" -eq 1 ] && [[ "$(readlink /proc/$$/fd/3 2>/dev/null)" =~ ^/dev/tty[0-9]+$ ]]; then
    printf '\n%sDieser Monitor zeigt gleich die Kiosk-Ausgabe (sofern ein Multiviewer zugewiesen ist).%s\n' "$Y" "$N" >&3
    printf 'Zurück zur Konsole jederzeit mit Strg+Alt+F1.\n' >&3
    for i in 10 9 8 7 6 5 4 3 2 1; do printf '\r   Start in %2d s %s ' "$i" "$ELL" >&3; sleep 1; done
    printf '\n' >&3
  fi
  run systemctl start camera-network-monitor-display || warn "Display-Dienst startet nicht: journalctl -u camera-network-monitor-display -n 50"
fi
if [ "$REBOOT_NEEDED" -eq 1 ]; then
  printf '\n%sEin Neustart ist nötig, damit Grafiktreiber und Kiosk-Ausgabe aktiv werden.%s\n' "$Y" "$N"
  if [ "$OPT_REBOOT" = yes ] || { [ "$OPT_REBOOT" != no ] && ask_yn "Jetzt neu starten?" y; }; then
    info "Starte neu $ELL"; run systemctl reboot
  else
    info "Bitte später neu starten (als root: reboot)"
  fi
fi
