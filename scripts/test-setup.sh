#!/usr/bin/env bash
# Tests the one-file installer and setup.sh without root (dry run with
# simulated GPUs/monitors):
#
#   scripts/test-setup.sh dist/camera-network-monitor-<v>-installer.run
set -uo pipefail
RUN="$(realpath "$1")"
export CNM_SETUP_STDIN=1 # answers come from stdin, never from a terminal
exec </dev/null
W="$(mktemp -d /tmp/cnm-setup-test.XXXX)"
trap 'rm -rf "$W"' EXIT
PASS=0; FAIL=0
ok()  { echo "  PASS $*"; PASS=$((PASS+1)); }
bad() { echo "  FAIL $*"; FAIL=$((FAIL+1)); }
expect() { # expect <file> <regex> <description>
  if grep -Eq -- "$2" "$1"; then ok "$3"; else bad "$3"; echo "------ output:"; sed -n '1,200p' "$1"; echo "------"; fi
}
reject() {
  if grep -Eq -- "$2" "$1"; then bad "$3"; grep -E -- "$2" "$1" | head -3; else ok "$3"; fi
}

# fake DRM tree: card0 with one GPU, HDMI-A-1 connected, DP-1 empty
gpu() { # gpu <dir> <vendor> <device>
  rm -rf "$1"; mkdir -p "$1/card0/device" "$1/card0-HDMI-A-1" "$1/card0-DP-1"
  echo "$2" > "$1/card0/device/vendor"; echo "$3" > "$1/card0/device/device"
  echo connected > "$1/card0-HDMI-A-1/status"; echo disconnected > "$1/card0-DP-1/status"
}
printf 'ID=debian\nPRETTY_NAME="Debian GNU/Linux 12 (bookworm)"\n' > "$W/os-debian"
printf 'ID=ubuntu\nID_LIKE=debian\nPRETTY_NAME="Ubuntu 24.04 LTS"\n' > "$W/os-ubuntu"

echo "== installer file"
"$RUN" --extract "$W/x" >"$W/extract.out" 2>&1
expect "$W/extract.out" "prüfe Installationsdatei … ok" "payload checksum verified"
PKG="$(ls -d "$W"/x/camera-network-monitor-*)"
[ -x "$PKG/setup.sh" ] && [ -x "$PKG/install.sh" ] && [ -x "$PKG/bin/cnm" ] && ok "extracted package is complete and executable" || bad "package incomplete"
"$PKG/bin/cnm" version >/dev/null 2>&1 && ok "binary matches this architecture" || bad "wrong architecture"
head -c 2000000 "$RUN" > "$W/broken.run"
bash "$W/broken.run" --extract "$W/y" >"$W/broken.out" 2>&1 && bad "truncated installer accepted" || ok "truncated installer rejected"
expect "$W/broken.out" "beschädigt oder unvollständig" "clear message for a damaged download"

echo "== unattended: Intel GPU on Debian"
gpu "$W/drm" 0x8086 0x3e92
CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" CNM_ADMIN_PASSWORD=geheim123 \
  bash "$RUN" --dry-run --yes --display --admin-user=chef --no-reboot >"$W/intel.out" 2>&1
expect "$W/intel.out" "Intel" "Intel GPU detected"
expect "$W/intel.out" "card0: Intel .* – Monitor: HDMI-A-1" "connected monitor shown at its card"
expect "$W/intel.out" "Grafiktreiber:    intel" "Intel driver chosen automatically"
expect "$W/intel.out" "intel-media-va-driver" "VA-API driver installed"
expect "$W/intel.out" "firmware-misc-nonfree" "Debian firmware package"
expect "$W/intel.out" "apt-get .*install .*firefox-esr" "Debian: Firefox ESR as kiosk browser"
expect "$W/intel.out" "apt-get .*install .*chromium" "Debian: Chromium as alternative browser"
expect "$W/intel.out" "Browser:          Firefox ESR und Chromium \(Debian\)" "browsers shown in the summary"
expect "$W/intel.out" "install.sh --with-display" "CNM installed with display support"
expect "$W/intel.out" "Administrator:    chef" "admin from command line"
expect "$W/intel.out" "Monitore belegen: HDMI-A-1" "monitor assignment planned"
reject "$W/intel.out" "geheim123" "admin password never printed"

echo "== unattended: NVIDIA on Ubuntu"
gpu "$W/drm" 0x10de 0x2484
CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-ubuntu" bash "$RUN" --dry-run --yes --display --no-reboot >"$W/nv.out" 2>&1
expect "$W/nv.out" "Grafiktreiber:    nvidia" "NVIDIA driver chosen automatically"
expect "$W/nv.out" "ubuntu-drivers install" "Ubuntu: recommended NVIDIA driver"
expect "$W/nv.out" "cnm-nvidia-drm.conf" "nvidia-drm modeset enabled"
expect "$W/nv.out" "update-initramfs -u" "initramfs updated"
expect "$W/nv.out" "Neustart ist nötig" "reboot announced"
expect "$W/nv.out" "Administrator:    später im Browser" "no admin without password in unattended mode"

echo "== interactive: AMD detected, user picks generic + answers"
gpu "$W/drm" 0x1002 0x73bf
# answers: displays? (default) | driver 5 | firefox repo? n | admin? j | name | pw | pw | assign? n | start? j
printf '\n5\nn\nj\nzentrale\nsehrgeheim1\nsehrgeheim1\nn\nj\n' | CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-ubuntu" \
  bash "$RUN" --dry-run --no-reboot >"$W/amd.out" 2>&1
expect "$W/amd.out" "← erkannt" "detected driver marked in the menu"
expect "$W/amd.out" "Grafiktreiber:    generic" "menu choice respected"
expect "$W/amd.out" "Administrator:    zentrale" "admin name from prompt"
reject "$W/amd.out" "sehrgeheim1" "typed password not echoed"
reject "$W/amd.out" "Monitore belegen" "assignment declined"
expect "$W/amd.out" "Kein Browser für die Kiosk-Ausgabe|Geeigneter Browser vorhanden|Firefox vorhanden" "browser situation reported"

echo "== no GPU: displays default off"
rm -rf "$W/drm"; mkdir -p "$W/drm"
CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" bash "$RUN" --dry-run --yes --no-reboot >"$W/none.out" 2>&1
expect "$W/none.out" "Keine Grafikhardware erkannt" "missing GPU reported"
expect "$W/none.out" "Lokale Displays:  nein" "displays not set up without GPU"
reject "$W/none.out" "with-display" "no display unit without GPU"

echo "== abort leaves the system untouched"
gpu "$W/drm" 0x8086 0x3e92
printf 'n\nn\nn\n' | CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" bash "$RUN" --dry-run >"$W/abort.out" 2>&1
expect "$W/abort.out" "Abgebrochen – nichts wurde verändert" "abort before any change"
reject "$W/abort.out" "apt-get" "no package operation after abort"

echo "== Debian: root via \"su\" without \"-\" (no /usr/sbin in PATH)"
gpu "$W/drm" 0x8086 0x3e92
env PATH=/usr/bin:/bin CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" \
  bash "$PKG/setup.sh" --dry-run --yes --display --no-reboot >"$W/su.out" 2>&1 && ok "setup runs with a minimal PATH" || bad "setup fails with a minimal PATH"
env PATH=/usr/bin:/bin bash -c "$(sed -n '/^for _d in /,/^export PATH/p' "$PKG/install.sh"); command -v useradd >/dev/null" \
  && ok "install.sh finds useradd/sbin tools after su" || bad "sbin tools not found with a minimal PATH"
"$PKG/setup.sh" --help >"$W/help.out" 2>&1
expect "$W/help.out" "--configure-only" "help shows all options"
expect "$W/help.out" "--display-gpu=KARTE" "help shows the GPU option"

echo "== Debian 13 package sources (deb822): exact components"
mkdir -p "$W/apt/sources.list.d"
printf '%s\n' 'Types: deb' 'URIs: http://deb.debian.org/debian' 'Suites: trixie trixie-updates' 'Components: main non-free-firmware' \
  'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' '' 'Types: deb' 'URIs: http://security.debian.org/debian-security' \
  'Suites: trixie-security' 'Components: main non-free-firmware' > "$W/apt/sources.list.d/debian.sources"
printf '%s\n' 'deb http://deb.debian.org/debian trixie main non-free-firmware' '# deb cdrom:[Debian 13]/ trixie main' > "$W/apt/sources.list"
dc() { ( info() { :; }; apt_update() { :; }; DRY=0; APT_ETC="$W/apt"
         eval "$(sed -n '/^debian_components() {/,/^}/p' "$PKG/setup.sh")"; debian_components "$@" ); }
dc contrib non-free non-free-firmware
[ "$(grep -c '^Components: main non-free-firmware contrib non-free$' "$W/apt/sources.list.d/debian.sources")" = 2 ] \
  && ok "deb822: non-free added although non-free-firmware exists" || { bad "deb822 components wrong"; cat "$W/apt/sources.list.d/debian.sources"; }
grep -qx 'deb http://deb.debian.org/debian trixie main non-free-firmware contrib non-free' "$W/apt/sources.list" \
  && grep -qx '# deb cdrom:\[Debian 13\]/ trixie main' "$W/apt/sources.list" && ok "classic sources: components added, comments untouched" || bad "classic sources wrong"
[ -f "$W/apt/sources.list.d/debian.sources.cnm-bak" ] && ok "original sources kept as .cnm-bak" || bad "no backup of the sources"
cp "$W/apt/sources.list.d/debian.sources" "$W/before"; dc contrib non-free non-free-firmware
cmp -s "$W/before" "$W/apt/sources.list.d/debian.sources" && ok "adding components is idempotent" || bad "components added twice"

if unshare -r true 2>/dev/null; then
  echo "== German Debian: apt-cache prints 'Installationskandidat:'"
  mkdir -p "$W/debin"
  printf '%s\n' '#!/bin/sh' 'if [ "${LC_ALL:-}" = C ]; then printf "%s:\n  Installed: (none)\n  Candidate: 1.0\n" "$2"' \
    'else printf "%s:\n  Installiert:           (keine)\n  Installationskandidat: 1.0\n" "$2"; fi' > "$W/debin/apt-cache"
  printf '%s\n' '#!/bin/sh' 'echo "Paketlisten werden gelesen… Fertig"' > "$W/debin/apt-get"
  chmod +x "$W/debin/apt-cache" "$W/debin/apt-get"
  rm -rf "$W/drm"; mkdir -p "$W/drm"
  printf 'n\nn\nj\n' | env LANG=de_DE.UTF-8 PATH="$W/debin:$PATH" CNM_SETUP_LOG="$W/de.log" CNM_SETUP_SYSFS="$W/drm" \
    CNM_SETUP_OS_RELEASE="$W/os-debian" timeout 60 unshare -r bash "$PKG/setup.sh" --no-reboot >"$W/de.out" 2>&1
  expect "$W/de.out" "Installiere ca-certificates curl python3 tar" "essential packages found on a German system"
  reject "$W/de.out" "Benötigte Pakete fehlen" "no false 'missing packages' error"
  expect "$W/de.log" "Installiere ca-certificates" "setup log written"
fi

echo "== existing installation without any user (interrupted first setup)"
mkdir -p "$W/data-nouser" "$W/data-users"
printf '{\n  "version": 3,\n  "system": {\n    "listenPort": 8090\n  },\n  "users": []\n}\n' > "$W/data-nouser/config.json"
printf '{\n  "version": 3,\n  "users": [\n    {\n      "id": "u1",\n      "username": "chef"\n    }\n  ]\n}\n' > "$W/data-users/config.json"
rm -rf "$W/drm"; mkdir -p "$W/drm"
CNM_SETUP_DATA_DIR="$W/data-nouser" CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" CNM_ADMIN_PASSWORD=geheim123 \
  bash "$PKG/setup.sh" --dry-run --yes --admin-user=chef --no-reboot >"$W/nouser.out" 2>&1
expect "$W/nouser.out" "Bestehende Installation gefunden" "detected as an update"
expect "$W/nouser.out" "hat noch keinen Benutzer" "missing user detected"
expect "$W/nouser.out" "Administrator:    chef \(neu\)" "first administrator offered on update"
expect "$W/nouser.out" "api/auth/bootstrap" "created through the bootstrap API"
# interactive: displays (Enter = no) | admin? (Enter = yes) | name | pw | pw | start
printf '\n\nchef\nsehrgeheim1\nsehrgeheim1\nj\n' | CNM_SETUP_DATA_DIR="$W/data-nouser" CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" \
  bash "$PKG/setup.sh" --dry-run --no-reboot >"$W/nouser-i.out" 2>&1
expect "$W/nouser-i.out" "Jetzt den ersten Administrator anlegen" "admin question asked on an update without users"
expect "$W/nouser-i.out" "Administrator:    chef \(neu\)" "interactive admin on update"

echo "== existing users: create admin / reset password on request"
CNM_SETUP_DATA_DIR="$W/data-users" CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" CNM_ADMIN_PASSWORD=geheim123 \
  bash "$PKG/setup.sh" --dry-run --yes --admin-user=chef --no-reboot >"$W/users.out" 2>&1
expect "$W/users.out" "Administrator:    chef \(anlegen bzw. Passwort setzen\)" "reset mode for existing users"
expect "$W/users.out" "runuser -u cnm -- .*cnm reset-user --data-dir .* --username chef" "password set offline as service user"
reject "$W/users.out" "geheim123" "password never printed"
CNM_SETUP_DATA_DIR="$W/data-users" CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" \
  bash "$PKG/setup.sh" --dry-run --yes --no-reboot >"$W/users2.out" 2>&1
expect "$W/users2.out" "Administrator:    unverändert" "existing users untouched by default"
reject "$W/users2.out" "reset-user" "no reset without request"

echo "== reset-user really creates an admin that can log in"
mkdir -p "$W/data-reset"
printf '%s\n' 'neu-geheim-1' | "$PKG/bin/cnm" reset-user --data-dir "$W/data-reset" --username chef >/dev/null 2>&1 \
  && ok "reset-user accepted the password from stdin" || bad "reset-user failed"
"$PKG/bin/cnm" serve --data-dir "$W/data-reset" --listen 127.0.0.1:28393 >"$W/reset.log" 2>&1 &
RP=$!
for i in $(seq 1 40); do curl -fsS http://127.0.0.1:28393/api/health >/dev/null 2>&1 && break; sleep 0.25; done
curl -fsS -H 'Content-Type: application/json' -d '{"username":"chef","password":"neu-geheim-1"}' http://127.0.0.1:28393/api/auth/login | grep -q '"role":"admin"' \
  && ok "admin from reset-user can log in" || bad "login after reset-user failed"
kill $RP 2>/dev/null; wait $RP 2>/dev/null

echo "== unsupported system"
printf 'ID=fedora\nPRETTY_NAME="Fedora 40"\n' > "$W/os-fedora"
CNM_SETUP_OS_RELEASE="$W/os-fedora" bash "$RUN" --dry-run --yes >"$W/fedora.out" 2>&1 && bad "fedora accepted" || ok "unsupported distribution refused"
expect "$W/fedora.out" "Debian oder Ubuntu erforderlich" "clear message for unsupported systems"

echo "== two GPUs: ASPEED VGA console (card0) + NVIDIA with HDMI/DP (card1)"
gpu2() {
  rm -rf "$1"; mkdir -p "$1/devices/0000:03:00.0" "$1/devices/0000:01:00.0" "$1/card0" "$1/card1" \
    "$1/card0-VGA-1" "$1/card1-HDMI-A-1" "$1/card1-DP-1"
  echo 0x1a03 > "$1/devices/0000:03:00.0/vendor"; echo 0x2000 > "$1/devices/0000:03:00.0/device"; echo 1 > "$1/devices/0000:03:00.0/boot_vga"
  echo 0x10de > "$1/devices/0000:01:00.0/vendor"; echo 0x2484 > "$1/devices/0000:01:00.0/device"; echo 0 > "$1/devices/0000:01:00.0/boot_vga"
  ln -s ../devices/0000:03:00.0 "$1/card0/device"; ln -s ../devices/0000:01:00.0 "$1/card1/device"
  echo connected > "$1/card0-VGA-1/status"; echo connected > "$1/card1-HDMI-A-1/status"; echo disconnected > "$1/card1-DP-1/status"
}
gpu2 "$W/drm2"
CNM_SETUP_SYSFS="$W/drm2" CNM_SETUP_OS_RELEASE="$W/os-debian" CNM_ADMIN_PASSWORD=geheim123 \
  bash "$RUN" --dry-run --yes --display --admin-user=chef --no-reboot >"$W/two.out" 2>&1
expect "$W/two.out" "card0: ASPEED .* – Monitor: VGA-1  \[Konsole\]" "console card and its monitor listed"
expect "$W/two.out" "card1: NVIDIA .* – Monitor: HDMI-A-1$" "display card lists only its own monitors"
expect "$W/two.out" "Display-Karte:    card1 \(PCI 0000:01:00.0\) – eigener Seat, Konsole bleibt auf card0" "dedicated GPU recommended, console card kept"
expect "$W/two.out" "Grafiktreiber:    nvidia" "driver follows the display card"
expect "$W/two.out" "loginctl attach seat1 .*card1" "display card moved to its own seat"
expect "$W/two.out" "CNM_DISPLAY_GPU=0000:01:00.0" "CNM limited to the display card"
expect "$W/two.out" "Environment=XDG_SEAT=seat1" "display service runs on seat1 without a console"
expect "$W/two.out" "Monitore belegen: HDMI-A-1$" "only monitors of the display card are assigned"
# interactive: user deliberately picks the console card
# answers: displays? (Enter) | card 1 | driver (Enter) | admin? n | start? j
printf '\n1\n\nn\nj\n' | CNM_SETUP_SYSFS="$W/drm2" CNM_SETUP_OS_RELEASE="$W/os-debian" bash "$RUN" --dry-run --no-reboot >"$W/two-i.out" 2>&1
expect "$W/two-i.out" "\[Konsole\]" "console card marked in the menu"
expect "$W/two-i.out" "← empfohlen" "recommended card marked"
expect "$W/two-i.out" "Display-Karte:    card0 \(PCI 0000:03:00.0\)$" "console card can be chosen"
reject "$W/two-i.out" "loginctl attach" "no extra seat when the console card is used"
CNM_SETUP_SYSFS="$W/drm2" CNM_SETUP_OS_RELEASE="$W/os-debian" bash "$RUN" --dry-run --yes --display --display-gpu=card0 --no-reboot >"$W/two-o.out" 2>&1
expect "$W/two-o.out" "Display-Karte:    card0" "--display-gpu selects the card"

echo "== configure-only against a real CNM (admin + monitor assignment via API)"
"$PKG/bin/cnm-sim" display-fixture "$W/drm-real" >/dev/null
CNM_DRM_SYSFS="$W/drm-real" "$PKG/bin/cnm" serve --data-dir "$W/data" --listen 127.0.0.1:28391 >"$W/cnm.log" 2>&1 &
CNM_PID=$!
for i in $(seq 1 40); do curl -fsS http://127.0.0.1:28391/api/health >/dev/null 2>&1 && break; sleep 0.25; done
PW='ge"heim\12 $ok'   # quotes, backslash, dollar and space must survive
CNM_SETUP_SYSFS="$W/drm-real" CNM_SETUP_OS_RELEASE="$W/os-ubuntu" CNM_ADMIN_PASSWORD="$PW" \
  bash "$PKG/setup.sh" --configure-only --port=28391 --yes --display --admin-user=chef >"$W/cfg.out" 2>&1
expect "$W/cfg.out" "Administrator chef angelegt" "admin created through the API"
expect "$W/cfg.out" "2 Monitor\(e\) belegt" "both connected monitors assigned"
reject "$W/cfg.out" "apt-get|install.sh" "configure-only installs nothing"
CJ="$W/cj"
PW="$PW" python3 -c 'import json,os;print(json.dumps({"username":"chef","password":os.environ["PW"]}))' \
  | curl -fsS -c "$CJ" -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:28391/api/auth/login >/dev/null \
  && ok "admin can log in with a password containing quotes/backslashes" || bad "admin login failed"
curl -fsS -b "$CJ" http://127.0.0.1:28391/api/settings/displays | python3 -c '
import sys, json
d = json.load(sys.stdin)
on = sorted(p["connector"] for p in d["ports"] if p.get("display") and p["display"]["enabled"])
sys.exit(0 if on == ["DP-1", "HDMI-A-1"] else 1)' && ok "displays enabled with the default multiviewer" || bad "display assignment not stored"
CNM_SETUP_SYSFS="$W/drm-real" CNM_SETUP_OS_RELEASE="$W/os-ubuntu" bash "$PKG/setup.sh" --configure-only --port=28391 --yes --display >"$W/cfg2.out" 2>&1
reject "$W/cfg2.out" "angelegt|belegt" "second run changes nothing"
kill $CNM_PID 2>/dev/null; wait $CNM_PID 2>/dev/null

if command -v script >/dev/null && unshare -r true 2>/dev/null; then
  echo "== real terminal: hanging package step, live status, Ctrl+C returns the console"
  mkdir -p "$W/fakebin"
  # an apt-get that waits for a lock forever and ignores Ctrl+C (like dpkg)
  printf '%s\n' '#!/bin/sh' 'trap "" INT' 'echo "Waiting for cache lock: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 4711 (unattended-upgr)"' 'exec sleep 300' > "$W/fakebin/apt-get"
  chmod +x "$W/fakebin/apt-get"
  rm -rf "$W/drm"; mkdir -p "$W/drm"
  ( sleep 2; printf 'n\n'; sleep 1; printf 'n\n'; sleep 1; printf 'j\n'; sleep 6; printf '\003'; sleep 25 ) | \
    env -u CNM_SETUP_STDIN PATH="$W/fakebin:$PATH" CNM_SETUP_LOG="$W/tty.log" CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" \
    timeout 45 script -qec "unshare -r bash $PKG/setup.sh" "$W/typescript" >/dev/null 2>&1
  sed -e 's/\x1b\[[0-9;]*[a-zA-Z]//g' -e 's/\r/\n/g' "$W/typescript" > "$W/tty.out"
  expect "$W/tty.out" "Paketlisten aktualisieren .*Waiting for cache lock" "live status shows what the step waits for"
  expect "$W/tty.out" "Setup abgebrochen" "Ctrl+C ends the setup with a message"
  hw=$(grep -n "Keine Grafikhardware erkannt" "$W/tty.out" | head -1 | cut -d: -f1)
  q=$(grep -n "Sollen an diesem Server" "$W/tty.out" | head -1 | cut -d: -f1)
  sm=$(grep -n "^Zusammenfassung" "$W/tty.out" | head -1 | cut -d: -f1)
  st=$(grep -n "Installation jetzt starten" "$W/tty.out" | head -1 | cut -d: -f1)
  [ -n "$hw" ] && [ -n "$q" ] && [ "$hw" -lt "$q" ] && [ -n "$sm" ] && [ -n "$st" ] && [ "$sm" -lt "$st" ] \
    && ok "screen order: hardware before question, summary before confirmation" || bad "output order on screen ($hw/$q, $sm/$st)"
  pgrep -f "$W/fakebin" >/dev/null && bad "hanging package step survived Ctrl+C" || ok "hanging package step stopped"
else
  echo "== (skipped real terminal test: script/unshare not available)"
fi

if [ -n "${2:-}" ]; then
  echo "== complete archive (tar.gz)"
  ARCHIVE="$(realpath "$2")"
  mkdir -p "$W/arc" && tar xzf "$ARCHIVE" -C "$W/arc"
  TOP="$(ls -d "$W"/arc/camera-network-monitor-*)"
  for f in setup.sh INSTALL.txt SHA256SUMS docs/OPERATIONS.md linux-amd64/setup.sh linux-arm64/bin/cnm linux-arm/bin/go2rtc; do
    [ -e "$TOP/$f" ] || bad "archive misses $f"
  done
  [ -x "$TOP/setup.sh" ] && [ -x "$TOP/linux-amd64/bin/cnm" ] && [ -x "$TOP/linux-amd64/install.sh" ] && ok "archive layout and executable bits" || bad "archive layout/modes"
  (cd "$TOP" && sha256sum --quiet -c SHA256SUMS) && ok "all checksums match" || bad "checksum mismatch"
  grep -q "@VERSION@" "$TOP/INSTALL.txt" && bad "INSTALL.txt not versioned" || ok "INSTALL.txt carries the version"
  "$TOP/setup.sh" --help >"$W/arc-help.out" 2>&1
  expect "$W/arc-help.out" "--gpu=TREIBER" "setup --help through the entry script"
  gpu "$W/drm" 0x8086 0x3e92
  CNM_SETUP_SYSFS="$W/drm" CNM_SETUP_OS_RELEASE="$W/os-debian" "$TOP/setup.sh" --dry-run --yes --display --no-reboot >"$W/arc.out" 2>&1
  expect "$W/arc.out" "Paket: $TOP/linux-amd64" "entry script picks the amd64 package"
  expect "$W/arc.out" "linux-amd64/install.sh --with-display" "full setup runs from the archive"
fi

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
