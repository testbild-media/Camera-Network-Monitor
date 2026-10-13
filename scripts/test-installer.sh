#!/usr/bin/env bash
# End-to-end installer / upgrade / runtime test for Linux (no root needed).
#
#   scripts/test-installer.sh dist/camera-network-monitor-<v>-linux-amd64.tar.gz
#
# Covers the upgrade (keeps config.json + master.key, one-time import,
# runtime independence), legacy data after an upgrade and the shutdown
# (< 5 s with open WebSocket/media connections, no orphan children).
set -euo pipefail
PKG_TGZ="$(realpath "$1")"
WORK="$(mktemp -d /tmp/cnm-install-test.XXXX)"
trap 'kill $(jobs -p) 2>/dev/null || true; pkill -f "$WORK" 2>/dev/null || true' EXIT
PASS=0; FAIL=0
ok()   { echo "  PASS $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL $*"; FAIL=$((FAIL+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

cd "$WORK"
tar xzf "$PKG_TGZ"
PKG="$(ls -d camera-network-monitor-*/)"
PKG="$WORK/${PKG%/}"
BIN="$PKG/bin"

echo "== 1. fresh install (test mode)"
R1="$WORK/root1"
CNM_ROOT="$R1" "$PKG/install.sh" --no-apt >/dev/null
check "binary installed and executable" "[ -x '$R1/opt/camera-network-monitor/bin/cnm' ]"
check "go2rtc bundled" "[ -x '$R1/opt/camera-network-monitor/bin/go2rtc' ]"
check "systemd unit installed" "[ -f '$R1/etc/systemd/system/camera-network-monitor.service' ]"
check "data dir 0750" "[ \"\$(stat -c %a '$R1/var/lib/camera-network-monitor')\" = 750 ]"

check "main unit opens the display socket" "grep -q -- '--display-socket /run/camera-network-monitor/display.sock' '$R1/etc/systemd/system/camera-network-monitor.service' && grep -q '^RuntimeDirectory=camera-network-monitor' '$R1/etc/systemd/system/camera-network-monitor.service'"
check "display unit not installed without --with-display" "[ ! -e '$R1/etc/systemd/system/camera-network-monitor-display.service' ]"
CNM_ROOT="$R1" "$PKG/install.sh" --no-apt --with-display >/dev/null
check "--with-display installs display unit and PAM file" "[ -f '$R1/etc/systemd/system/camera-network-monitor-display.service' ] && grep -q pam_systemd '$R1/etc/pam.d/camera-network-monitor-display'"
CNM_ROOT="$R1" "$PKG/install.sh" --no-apt >/dev/null
check "upgrade keeps the display unit" "[ -f '$R1/etc/systemd/system/camera-network-monitor-display.service' ]"
check "display agent command available" "{ '$BIN/cnm' display -h 2>&1 || true; } | grep -q -- '-socket'"

echo "== 1b. bundled programs run inside the unit's sandbox (seccomp settings)"
# MemoryDenyWriteExecute, SystemCallFilter & co. are taken from the real unit
# and applied with a transient user unit – this catches e.g. the UPX
# compressed go2rtc crashing under MemoryDenyWriteExecute
if systemd-run --user --quiet --wait true 2>/dev/null; then
  UNIT="$R1/etc/systemd/system/camera-network-monitor.service"
  PROPS=()
  while IFS= read -r line; do PROPS+=(-p "$line"); done < <(grep -E '^(MemoryDenyWriteExecute|SystemCallFilter|SystemCallArchitectures|SystemCallErrorNumber|RestrictAddressFamilies|LockPersonality|RestrictRealtime|RestrictNamespaces|NoNewPrivileges)=' "$UNIT")
  for prog in "go2rtc -version" "cnm version"; do
    set -- $prog
    if out=$(timeout 30 systemd-run --user --quiet --wait --pipe "${PROPS[@]}" "$R1/opt/camera-network-monitor/bin/$1" "${@:2}" 2>&1) && [ -n "$out" ]; then
      ok "$1 starts under the unit's seccomp settings"
    else
      bad "$1 fails under the unit's seccomp settings: $out"
    fi
  done
else
  echo "  (skipped: no systemd user manager)"
fi

echo "== 2. upgrade over legacy data keeps config.json + master.key"
R2="$WORK/root2"
D2="$R2/var/lib/camera-network-monitor"
# the managed go2rtc gets its configuration inline (no file): match its API address
G2R='go2rtc -config [{]"api":[{]"listen":"127[.]0[.]0[.]1:21985"'
mkdir -p "$R2/opt/camera-network-monitor/node_modules"
echo "old node app" > "$R2/opt/camera-network-monitor/server.js"
"$BIN/cnm-sim" legacy-fixture "$D2" 28008 28554 >/dev/null
sha_before="$(sha256sum "$D2/config.json" "$D2/master.key")"
CNM_ROOT="$R2" "$PKG/install.sh" --no-apt >/dev/null
sha_after="$(sha256sum "$D2/config.json" "$D2/master.key")"
check "config.json and master.key byte-identical" "[ \"$sha_before\" = \"$sha_after\" ]"
check "previous program kept for rollback" "[ -f '$R2/opt/camera-network-monitor.prev/server.js' ]"
check "old program files replaced" "[ ! -e '$R2/opt/camera-network-monitor/server.js' ]"
check "secret files 0600" "[ \"\$(stat -c %a '$D2/master.key')\" = 600 ]"

echo "== 3. missing master.key aborts before changing anything"
R3="$WORK/root3"
D3="$R3/var/lib/camera-network-monitor"
"$BIN/cnm-sim" legacy-fixture "$D3" >/dev/null
rm "$D3/master.key"
mkdir -p "$R3/opt/camera-network-monitor"; echo keep > "$R3/opt/camera-network-monitor/marker"
if CNM_ROOT="$R3" "$PKG/install.sh" --no-apt >/dev/null 2>&1; then bad "installer must fail"; else ok "installer refused"; fi
check "program dir untouched" "[ -f '$R3/opt/camera-network-monitor/marker' ] && [ ! -e '$R3/opt/camera-network-monitor.prev' ]"

echo "== 4. one-time import of a local data directory"
R4="$WORK/root4"
cp -r "$PKG" "$WORK/pkg-with-data"
"$BIN/cnm-sim" legacy-fixture "$WORK/pkg-with-data/data" >/dev/null
CNM_ROOT="$R4" "$WORK/pkg-with-data/install.sh" --no-apt >/dev/null
check "data imported" "cmp -s '$WORK/pkg-with-data/data/master.key' '$R4/var/lib/camera-network-monitor/master.key'"
"$BIN/cnm-sim" legacy-fixture "$WORK/pkg-with-data/data" >/dev/null   # new key in package
CNM_ROOT="$R4" "$WORK/pkg-with-data/install.sh" --no-apt >/dev/null
check "existing service data wins over package data" "! cmp -s '$WORK/pkg-with-data/data/master.key' '$R4/var/lib/camera-network-monitor/master.key'"

echo "== 5. upgraded legacy data is usable (login, DVR password, kiosk key)"
"$BIN/cnm-sim" --password dvr-geheim --dahua-http 127.0.0.1:28008 --dahua-rtsp 127.0.0.1:28554 --dahua-web3 '' --sofia-modern '' --sofia-legacy '' --dahua-native '' --hik-http '' --onvif-http '' --axis-http '' --uniview-rtsp '' --discovery=false >"$WORK/sim.log" 2>&1 &
for i in $(seq 1 60); do grep -q "Dahua RPC2 sim:" "$WORK/sim.log" && break; sleep 0.5; done
grep -q "Dahua RPC2 sim:" "$WORK/sim.log" || { cat "$WORK/sim.log"; bad "simulator did not start"; }
APP="$R2/opt/camera-network-monitor/bin/cnm"
env -i PATH=/usr/bin:/bin CNM_LOG_LEVEL=info "$APP" serve --data-dir "$D2" --listen 127.0.0.1:28190 >"$WORK/cnm.log" 2>&1 &
CNM_PID=$!
for i in $(seq 1 40); do curl -fsS http://127.0.0.1:28190/api/health >/dev/null 2>&1 && break; sleep 0.25; done
check "service starts with a minimal environment (no PATH/HOME setup needed)" "curl -fsS http://127.0.0.1:28190/api/health >/dev/null"
check "original config archived once" "[ -f '$D2/config.json.v1-original' ]"
CJ="$WORK/cj"
code=$(curl -s -o /dev/null -w '%{http_code}' -c "$CJ" -H 'X-Forwarded-For: 192.0.2.1' -X POST -d '{"username":"chef","password":"legacy-admin-1"}' http://127.0.0.1:28190/api/auth/login)
check "legacy admin password accepted" "[ $code = 200 ]"
for i in $(seq 1 40); do
  st=$(curl -s -b "$CJ" http://127.0.0.1:28190/api/settings | grep -o '"state":"[a-z]*"' | head -1 || true)
  [ "$st" = '"state":"connected"' ] && break; sleep 0.25
done
if [ "$st" = '"state":"connected"' ]; then ok "legacy encrypted DVR password works (device connected)"; else bad "legacy DVR password (state $st)"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 192.0.2.2' -X POST -d '{"mon":"mon1","key":"legacyKioskToken_0123456789abcdefghijklmnopqrstuv"}' http://127.0.0.1:28190/api/auth/kiosk)
check "legacy kiosk key still valid" "[ $code = 200 ]"
upgraded=0
for i in $(seq 1 20); do grep -q 'scrypt\$32768' "$D2/config.json" && { upgraded=1; break; }; sleep 0.25; done
if [ $upgraded = 1 ]; then ok "legacy password hash upgraded to current scrypt format"; else bad "password hash not upgraded"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:28190/api/state)
check "no anonymous viewer access" "[ $code = 401 ]"
# schema 3: device ids become UUIDs, channels become first-class cameras
MODEL=$(curl -s -b "$CJ" -H 'X-Forwarded-For: 192.0.2.1' http://127.0.0.1:28190/api/settings | python3 -c '
import sys, json, re
s = json.load(sys.stdin)
d = s["devices"][0]
uuid = re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", d["id"]) is not None
cams = [c for c in s["cameras"] if c["deviceId"] == d["id"]]
cams.sort(key=lambda c: c["channel"])
print("%s %d %s" % ("uuid" if uuid else "legacy:" + d["id"], len(cams), cams[0]["multiview"] if cams else "-"))')
read -r IDKIND NCAMS STREAM <<<"$MODEL"
check "device id migrated to a UUID" "[ '$IDKIND' = uuid ]"
check "channels migrated to cameras ($NCAMS)" "[ '$NCAMS' -ge 1 ]"
check "legacy id kept as metadata" "grep -q '\"legacyId\": \"dvr1\"' '$D2/config.json'"

# enable the bundled go2rtc (managed by CNM) through the API
CSRF=$(curl -s -b "$CJ" -H 'X-Forwarded-For: 192.0.2.1' http://127.0.0.1:28190/api/auth/me | sed -n 's/.*"csrf":"\([^"]*\)".*/\1/p')
SYS=$(curl -s -b "$CJ" -H 'X-Forwarded-For: 192.0.2.1' http://127.0.0.1:28190/api/settings | python3 -c '
import sys, json
s = json.load(sys.stdin)["system"]
m = s["media"]
m.update({"manageGo2rtc": True, "go2rtcUrl": "http://127.0.0.1:21985", "go2rtcApiListen": "127.0.0.1:21985", "go2rtcRtspListen": "127.0.0.1:28556"})
print(json.dumps(s))')
code=$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" -H "X-CSRF-Token: $CSRF" -H 'X-Forwarded-For: 192.0.2.1' -X PUT -d "$SYS" http://127.0.0.1:28190/api/settings/system)
check "managed go2rtc enabled via API" "[ $code = 200 ]"
media=""
for i in $(seq 1 60); do
  media=$(curl -s -b "$CJ" -H 'X-Forwarded-For: 192.0.2.1' "http://127.0.0.1:28190/api/media/status?streams=$STREAM" | grep -o "\"$STREAM\":{[^}]*}" || true)
  echo "$media" | grep -q '"online":true' && break; sleep 0.5
done
if echo "$media" | grep -q '"online":true'; then ok "bundled go2rtc managed by CNM, warm stream online"; else bad "media not online: $media"; fi

echo "== 6. shutdown < 5 s with open control and media connections"
WSH=(-H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
curl -s -N --http1.1 -b "$CJ" "${WSH[@]}" http://127.0.0.1:28190/ws >/dev/null 2>&1 &
python3 - "$WORK/media.out" "$CJ" "$STREAM" <<'PYEOF' &
import socket, sys, os, base64, json, struct
out, jar, stream = sys.argv[1], sys.argv[2], sys.argv[3]
TAB = chr(9)
cookie = ""
for line in open(jar):
    f = line.rstrip().split(TAB)
    if len(f) == 7 and f[5] == "cnm_session":
        cookie = "cnm_session=" + f[6]
s = socket.create_connection(("127.0.0.1", 28190))
key = base64.b64encode(os.urandom(16)).decode()
CRLF = chr(13) + chr(10)
s.sendall(CRLF.join(["GET /media/api/ws?src=" + stream + " HTTP/1.1", "Host: 127.0.0.1:28190",
                     "Upgrade: websocket", "Connection: Upgrade", "Sec-WebSocket-Version: 13",
                     "Cookie: " + cookie, "Sec-WebSocket-Key: " + key, "", ""]).encode())
hdr = b""
while (CRLF + CRLF).encode() not in hdr:
    hdr += s.recv(1)
payload = json.dumps({"type": "mse", "value": "avc1.640029,avc1.42E01E,avc1.420029"}).encode()
mask = os.urandom(4)
frame = bytes([0x81, 0x80 | len(payload)]) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
s.sendall(frame)
total = 0
while True:
    d = s.recv(65536)
    if not d:
        break
    total += len(d)
    with open(out, "w") as f:
        f.write(str(total))
PYEOF
sleep 3
if [ -s "$WORK/media.out" ] && [ "$(cat "$WORK/media.out")" -gt 1000 ]; then ok "media websocket streaming through the proxy ($(cat "$WORK/media.out") bytes)"; else bad "media websocket produced no data"; fi
start=$(date +%s%N)
kill -TERM "$CNM_PID"
wait "$CNM_PID" || true
ms=$(( ($(date +%s%N) - start) / 1000000 ))
echo "  shutdown took ${ms} ms"
check "shutdown finished within 5 s" "[ $ms -lt 5000 ]"
sleep 0.5
if pgrep -f "$G2R" >/dev/null; then bad "go2rtc child survived"; else ok "no orphaned go2rtc child"; fi

echo "== 7. CNM killed hard (SIGKILL): children die with it (PDEATHSIG)"
env -i PATH=/usr/bin:/bin "$APP" serve --data-dir "$D2" --listen 127.0.0.1:28190 >"$WORK/cnm2.log" 2>&1 &
CNM_PID=$!
for i in $(seq 1 40); do pgrep -f "$G2R" >/dev/null && break; sleep 0.25; done
if pgrep -f "$G2R" >/dev/null; then ok "managed go2rtc running"; else bad "managed go2rtc not started"; fi
kill -KILL "$CNM_PID"; wait "$CNM_PID" 2>/dev/null || true
sleep 1
if pgrep -f "$G2R" >/dev/null; then bad "go2rtc orphaned after SIGKILL"; else ok "go2rtc terminated with CNM"; fi

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
