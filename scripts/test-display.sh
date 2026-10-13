#!/usr/bin/env bash
# Integration test of the local display output against a REAL sway
# (headless backend, two virtual outputs) without root:
#
#   scripts/test-display.sh path/to/linux/cnm
#
# sway and foot are fetched with `apt-get download` from the distribution's
# archive and only unpacked into ~/.cache/cnm-sway (nothing is installed).
# The "browser" is a wrapper that opens a foot terminal window titled like
# the CNM viewer ("CNM-DISPLAY <port>"), so window placement, fullscreen,
# modes, rotation, crash recovery and compositor restarts are exercised for
# real; Chromium itself is not part of this test.
set -euo pipefail
CNM_BIN="$(realpath "$1")"
ROOT="$HOME/.cache/cnm-sway"
PORT=28290
PASS=0; FAIL=0
ok()  { echo "  PASS $*"; PASS=$((PASS+1)); }
bad() { echo "  FAIL $*"; FAIL=$((FAIL+1)); }

if [ ! -x "$ROOT/usr/bin/sway" ]; then
  echo "== fetching sway/foot (apt-get download, no root)"
  mkdir -p "$ROOT/debs"
  (cd "$ROOT/debs" && apt-get download sway libwlroots12t64 libseat1 libliftoff0 libdisplay-info1 \
     libxcb-composite0 libxcb-ewmh2 libxcb-res0 foot libfcft4t64 libutf8proc3 >/dev/null)
  for d in "$ROOT"/debs/*.deb; do dpkg -x "$d" "$ROOT"; done
fi
export LD_LIBRARY_PATH="$ROOT/usr/lib/x86_64-linux-gnu"
export PATH="$ROOT/usr/bin:$PATH"
echo "== $(sway --version)"

W="$(mktemp -d /tmp/cnm-display-test.XXXX)"
export XDG_RUNTIME_DIR="$W/run"     # private runtime dir (never touch the user's session)
mkdir -m 0700 "$XDG_RUNTIME_DIR"
cleanup() {
  kill $(jobs -p) 2>/dev/null || true
  sleep 0.5
  pkill -f "$W" 2>/dev/null || true
}
trap cleanup EXIT

cat > "$W/fake-browser" <<'EOF'
#!/bin/sh
[ "$1" = "--version" ] && { echo "FakeBrowser 1.0"; exit 0; }
for a in "$@"; do case "$a" in file://*) f="${a#file://}" ;; esac; done
disp=$(grep -o 'display=[A-Za-z0-9._-]*' "$f" | head -1 | cut -d= -f2)
exec foot --title "CNM-DISPLAY $disp" sh -c 'exec sleep 1000000'
EOF
chmod +x "$W/fake-browser"

echo "== start CNM"
SOCK="$XDG_RUNTIME_DIR/cnm-display.sock"
"$CNM_BIN" serve --data-dir "$W/data" --listen 127.0.0.1:$PORT --display-socket "$SOCK" >"$W/cnm.log" 2>&1 &
for i in $(seq 1 40); do curl -fsS http://127.0.0.1:$PORT/api/health >/dev/null 2>&1 && break; sleep 0.25; done
CJ="$W/cj"
curl -fsS -c "$CJ" -X POST -d '{"username":"admin","password":"test-admin-pw"}' http://127.0.0.1:$PORT/api/auth/bootstrap >/dev/null
CSRF=$(curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/auth/me | python3 -c 'import sys,json;print(json.load(sys.stdin)["csrf"])')
api() { curl -fsS -b "$CJ" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -X "$1" -d "${3:-{\}}" "http://127.0.0.1:$PORT$2"; }
MON=$(curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/settings | python3 -c 'import sys,json;print(json.load(sys.stdin)["monitors"][0]["id"])')
api POST /api/settings/displays "{\"connector\":\"HEADLESS-1\",\"enabled\":true,\"monitorId\":\"$MON\",\"mode\":\"1280x720\"}" >/dev/null
api POST /api/settings/displays "{\"connector\":\"HEADLESS-2\",\"enabled\":true,\"monitorId\":\"$MON\",\"rotation\":90}" >/dev/null

echo "== start display agent (sway headless, 2 outputs)"
WLR_BACKENDS=headless WLR_HEADLESS_OUTPUTS=2 WLR_RENDERER=pixman CNM_DISPLAY_BROWSER="$W/fake-browser" \
  "$CNM_BIN" display --socket "$SOCK" --state-dir "$W/state" >"$W/agent.log" 2>&1 &
AGENT=$!

state() { # state <connector> → display state reported to CNM
  curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/settings/displays | python3 -c '
import sys, json
d = json.load(sys.stdin)
for p in d["ports"]:
    if p["connector"] == sys.argv[1]:
        st = p.get("status") or {}
        print(st.get("state", "-"), st.get("restarts", 0))
' "$1"
}
wait_state() { # wait_state <connector> <state> [min restarts]
  for i in $(seq 1 80); do
    read -r s r <<<"$(state "$1")"
    [ "$s" = "$2" ] && [ "${r:-0}" -ge "${3:-0}" ] && return 0
    sleep 0.25
  done
  return 1
}
sw() { swaymsg -s "$XDG_RUNTIME_DIR/cnm-sway.sock" "$@"; }

if wait_state HEADLESS-1 running && wait_state HEADLESS-2 running; then ok "both outputs show their kiosk window"; else bad "outputs not running: $(state HEADLESS-1) / $(state HEADLESS-2)"; tail -20 "$W/agent.log"; fi
OUTS=$(sw -t get_outputs -r)
TREE=$(sw -t get_tree -r)
python3 - "$OUTS" "$TREE" <<'EOF' && ok "modes, rotation and fullscreen placement as configured" || bad "compositor state differs"
import json, sys
outs = {o["name"]: o for o in json.loads(sys.argv[1])}
tree = json.loads(sys.argv[2])
h1, h2 = outs["HEADLESS-1"], outs["HEADLESS-2"]
assert h1["current_mode"]["width"] == 1280 and h1["current_mode"]["height"] == 720, h1["current_mode"]
assert h2["transform"] == "90", h2["transform"]
assert h1["rect"]["x"] != h2["rect"]["x"], "outputs overlap"
wins = {}
def walk(n, out=None):
    if n.get("type") == "output":
        out = n["name"]
    if n.get("pid") and n.get("type") in ("con", "floating_con"):
        wins[n["name"]] = (out, n.get("fullscreen_mode"))
    for c in n.get("nodes", []) + n.get("floating_nodes", []):
        walk(c, out)
walk(tree)
assert wins.get("CNM-DISPLAY HEADLESS-1") == ("HEADLESS-1", 1), wins
assert wins.get("CNM-DISPLAY HEADLESS-2") == ("HEADLESS-2", 1), wins
EOF

echo "== change resolution (no browser restart)"
PID1=$(pgrep -f "CNM-DISPLAY HEADLESS-1" | head -1 || true)
api POST /api/settings/displays "{\"connector\":\"HEADLESS-1\",\"enabled\":true,\"monitorId\":\"$MON\",\"mode\":\"1024x768\"}" >/dev/null
okmode=0
for i in $(seq 1 40); do
  [ "$(sw -t get_outputs -r | python3 -c 'import sys,json;print([o for o in json.load(sys.stdin) if o["name"]=="HEADLESS-1"][0]["current_mode"]["width"])')" = 1024 ] && { okmode=1; break; }
  sleep 0.25
done
if [ $okmode = 1 ]; then ok "mode change applied live"; else
  bad "mode change not applied"
  sw -t get_outputs -r > "$W/outs.json" || true
  python3 -c 'import json,sys;[print(" ", o["name"], o.get("current_mode"), "modes:", o.get("modes")) for o in json.load(open(sys.argv[1]))]' "$W/outs.json" || true
  curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/settings/displays > "$W/disp.json" || true
  python3 -c 'import json,sys;[print(" ", p["connector"], (p.get("display") or {}).get("mode"), p.get("status")) for p in json.load(open(sys.argv[1]))["ports"]]' "$W/disp.json" || true
  tail -15 "$W/agent.log" || true
fi
[ -n "$PID1" ] && [ "$(pgrep -f "CNM-DISPLAY HEADLESS-1" | head -1 || true)" = "$PID1" ] && ok "browser kept running during the mode change" || bad "browser restarted on mode change"

echo "== browser crash → restart"
pkill -f "CNM-DISPLAY HEADLESS-2" || true
if wait_state HEADLESS-2 running 1; then ok "crashed browser restarted and placed again"; else bad "no recovery after browser crash: $(state HEADLESS-2)"; fi

echo "== switch an output off"
api POST /api/settings/displays "{\"connector\":\"HEADLESS-2\",\"enabled\":false}" >/dev/null
if wait_state HEADLESS-2 off; then ok "display switched off"; else bad "display not off: $(state HEADLESS-2)"; fi
sleep 0.5
active=$(sw -t get_outputs -r | python3 -c 'import sys,json;print([o for o in json.load(sys.stdin) if o["name"]=="HEADLESS-2"][0]["active"])')
[ "$active" = False ] && ok "output disabled in the compositor" || bad "output still active"
pgrep -f "CNM-DISPLAY HEADLESS-2" >/dev/null && bad "browser of a disabled display still running" || ok "browser of the disabled display stopped"

echo "== nothing assigned → compositor stops, console is released"
compstate() { curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/settings/displays | python3 -c 'import sys,json;print((json.load(sys.stdin)["agent"].get("compositor") or {}).get("state","-"))'; }
api POST /api/settings/displays "{\"connector\":\"HEADLESS-1\",\"enabled\":false}" >/dev/null
idle=0
for i in $(seq 1 60); do
  if [ "$(compstate)" = idle ] && ! pgrep -f "$ROOT/usr/bin/sway" >/dev/null; then idle=1; break; fi
  sleep 0.25
done
[ $idle = 1 ] && ok "compositor stopped while nothing is assigned" || bad "compositor still running without assignment ($(compstate))"
api POST /api/settings/displays "{\"connector\":\"HEADLESS-1\",\"enabled\":true,\"monitorId\":\"$MON\",\"mode\":\"1024x768\"}" >/dev/null
if wait_state HEADLESS-1 running; then ok "compositor back as soon as a multiviewer is assigned"; else bad "no restart after re-assignment: $(state HEADLESS-1)"; fi

echo "== compositor crash → restart"
pkill -KILL -f "$ROOT/usr/bin/sway" || pkill -KILL -x sway || true
# the agent must notice the crash first (state leaves "running") …
for i in $(seq 1 40); do read -r s r <<<"$(state HEADLESS-1)"; [ "$s" != running ] && break; sleep 0.25; done
[ "$s" != running ] && ok "compositor crash detected" || bad "compositor crash not detected"
# … and then bring everything back
if wait_state HEADLESS-1 running 0 && [ -S "$XDG_RUNTIME_DIR/cnm-sway.sock" ]; then ok "compositor restarted, display running again"; else bad "no recovery after compositor crash: $(state HEADLESS-1)"; tail -20 "$W/agent.log"; fi

echo "== CNM restart: agent keeps the displays"
PID1=$(pgrep -f "CNM-DISPLAY HEADLESS-1" | head -1 || true)
pkill -TERM -f "$CNM_BIN serve" || true
sleep 1
"$CNM_BIN" serve --data-dir "$W/data" --listen 127.0.0.1:$PORT --display-socket "$SOCK" >>"$W/cnm.log" 2>&1 &
for i in $(seq 1 40); do curl -fsS http://127.0.0.1:$PORT/api/health >/dev/null 2>&1 && break; sleep 0.25; done
sleep 4
[ -n "$PID1" ] && [ "$(pgrep -f "CNM-DISPLAY HEADLESS-1" | head -1 || true)" = "$PID1" ] && ok "kiosk window survived the CNM restart" || bad "kiosk window restarted with CNM"
if wait_state HEADLESS-1 running; then ok "agent reconnected to CNM"; else bad "agent did not reconnect (state: $(state HEADLESS-1))"; curl -fsS -b "$CJ" http://127.0.0.1:$PORT/api/settings/displays | python3 -c 'import sys,json;d=json.load(sys.stdin);print(" agent:", d["agent"]);[print(" ", p["connector"], p.get("display"), p.get("status")) for p in d["ports"]]' || true; ls -la "$XDG_RUNTIME_DIR"; tail -3 "$W/agent.log"; fi

echo "== agent stop leaves no processes"
kill -TERM $AGENT; wait $AGENT 2>/dev/null || true
sleep 0.5
if pgrep -f "$ROOT/usr/bin/sway" >/dev/null || pgrep -f "CNM-DISPLAY" >/dev/null; then bad "processes left behind"; else ok "sway and browsers stopped with the agent"; fi
grep -q "key=" "$W/agent.log" && bad "agent log contains a kiosk credential" || ok "no credentials in the agent log"

echo
echo "Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
