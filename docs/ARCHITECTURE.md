# Architecture

This document is for developers. It explains how CNM is put together and
why.

## Overview

```
Browser ──HTTP/WebSocket──▶ CNM (Go)
                             ├─ auth       sessions, kiosk identities, CSRF, login rate limit
                             ├─ server     REST API, /ws (hub), /media/ (proxy), web UI
                             ├─ device     one supervisor per device (login, health, backoff, auth lock)
                             ├─ provider   protocol interface (detection of variant and channels)
                             │   ├─ dahua      RPC2 (login variants) or native protocol, RTSP
                             │   ├─ sofia      DVRIP, frame parser
                             │   ├─ hikvision, axis, uniview   ISAPI, VAPIX, RTSP
                             │   └─ generic    ONVIF, RTSP, RTMP, HTTP, raw TCP
                             ├─ media      desired state, reconciler, preload, health, go2rtc process
                             ├─ bridge     loopback endpoint for streams go2rtc cannot read itself (MPEG-TS)
                             ├─ playback   search, recording index, FFmpeg bridge → fragmented MP4
                             ├─ display    graphics outputs/EDID (sysfs), socket for the display agent
                             └─ config     data model (devices/cameras/groups), tolerant loader, atomic store
                                       │
                            go2rtc ◀───┘ (API local only) ──RTSP/DVRIP/HTTP──▶ devices
```

Source layout:

| Path | Content |
|---|---|
| `cmd/cnm` | the server (`cnm serve`, `cnm display`, `cnm check-config`, `cnm reset-user`) |
| `cmd/cnm-sim` | device simulator for tests and demos |
| `internal/…` | the packages shown above, plus `rtsp` (own RTSP client), `tsmux` (MPEG-TS), `secret` (encryption, password hashes), `sim` (protocol simulators) |
| `internal/webui/static` | the web interface (plain JavaScript modules, no build step), embedded into the binary |
| `deploy/` | setup, installer, systemd units |
| `scripts/` | release build and Linux integration tests |
| `web-tests/` | tests for browser code (Node.js) |

## Main decisions

**Go, one static binary.** No runtime needed on the server, small memory
footprint, one goroutine and context per device and stream for fault
isolation, and binary protocols (DVRIP, DHAV) are easy to implement.

**Protocol knowledge only in providers.** A provider describes its protocol
(ports, options, capabilities); the device dialog is generated from that.
There are no device models: the administrator chooses the protocol, and the
driver detects the variant (e.g. a Dahua RPC2 login variant or the native
protocol) and the number of channels on the first `Connect`. The supervisor
stores the result (`config.Detection`), which creates the cameras, and hands
the variant back as `DeviceConfig.Variant` on later connections. A driver
encapsulates login, health, live sources, search and playback. Capabilities
carry a verification status per detected variant; functions that are not
released stay off.

**Device control and media path are separate.** The supervisor talks to the
device (login, health, time, disk). go2rtc carries live video. CNM only
registers stable stream names (`cnm_<device-key>_chNN_<sub|main>`, where the
key is the first 8 hex characters of the device UUID); source URLs with
credentials never leave the backend.

**Authentication lock.** Every login – control connection, bridge,
playback, RTSP probe – asks `Env.AllowLogin()` first. After an
authentication failure the device is locked until its credentials change or
an administrator reconnects it manually. The go2rtc streams of a locked
device are removed as well, so go2rtc does not keep logging in with wrong
credentials either. This protects recorder accounts from lockouts.

**Devices provide cameras; cameras are objects of their own.** A device
(UUID, label, protocol, credentials, network, detected variant) is only a
provider. Cameras (`config.Camera`: UUID, device, channel, name, streams,
presentation) are separate objects; `Normalize` keeps them in step with the
device's channel count (new channels → new cameras, dropped channels →
camera removed together with its group and slot references). Groups are
lists of camera IDs; a camera can be in several groups. Monitor slots only
reference camera IDs, so renaming or regrouping never touches layouts or
device sessions. Older formats are converted on load.

**A recording index instead of a manual storage setting.**
`playback.RunIndex` checks per camera whether the device holds recordings
(disk status from the supervisor plus a recording search or probe, 30-day
window, every 10 minutes and right after each reconnect; at most two
devices in parallel, the cameras of one device one after another). Playback
is only offered for cameras with recordings; kiosk identities never get
playback.

**Configuration as an immutable snapshot.** `config.Store` clones, changes,
normalises, saves atomically (temp file, fsync, rename, `.bak`) and notifies
subscribers. Each subscriber computes its own difference: devices only
reconnect on connection-relevant changes (a fingerprint of protocol, host,
ports and credentials – not of the stored detection result, so storing it
does not reconnect). Layout, label and user changes never touch device
sessions.

## Media reconciler

- A single loop runs reconcile passes – never two at once. Triggers are
  coalesced (150 ms); operations run with limited parallelism.
- A light watch (every 3 s) notices a go2rtc restart (registered streams
  missing) and triggers a pass at once; if go2rtc is unreachable, it retries
  quickly (0.5 / 1 / 2 / 4 s).
- go2rtc's API is not safe for parallel calls, so the client serialises all
  requests; the operation timeout only starts when a request is actually
  sent.
- `PUT /api/streams` replaces the stream *object*; an existing preload stays
  attached to the old object. Therefore: delete preload → set stream → set
  preload. A preload without a consumer on the current object, or without an
  active producer, counts as stale and is repaired.
- Errors are only counted after a retry **and** after checking go2rtc's
  actual state (a timeout does not mean "not executed").
- If go2rtc has no preload API (older versions), live video still works.
- The managed go2rtc receives its configuration inline at start; it has no
  configuration file, so stream sources with credentials are never written
  to disk.

## Media proxy

`/media/api/ws?src=<name>` is the only go2rtc path reachable from outside.
CNM builds the upstream request itself (only a validated `src`, no cookies,
no origin). Background: for an unknown `src`, go2rtc would create a stream
from an arbitrary source URL – including `exec:`. Client frames are
inspected; WebRTC signalling is never allowed for kiosks and otherwise only
when enabled, because WebRTC media bypass CNM and cannot be cut off on the
server side.

## DVRIP bridge

For devices whose live video needs two connections, CNM runs the protocol
itself (login/control connection → media connection → claim on media →
start on control → data on media, keepalive on control). The frame parser
reassembles frames across packet boundaries, resynchronises after garbage
and only starts at a key frame with parameter sets. The output is
**MPEG-TS** (one PES per frame), because go2rtc's raw-stream reader would
cut frames with several slices apart. go2rtc fetches the stream from a
loopback listener; the per-process token in the URL is redacted in logs and
never stored or sent to a browser.

## Browser

- Own player for go2rtc's WebSocket protocol (MSE for H.264/H.265, automatic
  MJPEG fallback, optional WebRTC without external STUN servers). No
  iframes, no go2rtc UI.
- `StreamSlot` = player + watchdog: source offline, no frame progress for
  20 s (`requestVideoFrameCallback`) or an error → only this instance is
  recreated, with backoff 1–30 s.
- Health poll every 5 s, never overlapping, paused in background tabs.
- Multiviewer tiles are keyed by slot ID; players are only rebuilt when the
  stream or presentation changes. Status and label updates are incremental.
- Aspect ratio and fit use CSS container queries – identical in the
  multiviewer, the full-size view and playback.
- Playback uses `fetch` + MSE: the server parses the fragmented MP4 init
  segment and sends the exact codec string; structured errors come as JSON
  before the first byte. A seek reopens the device source at the absolute
  time; within buffered data the player jumps locally.

## Wall displays

```
camera-network-monitor.service            camera-network-monitor-display.service
 cnm serve ── /run/camera-network-monitor/display.sock ── cnm display (user cnm-display)
  │  desired: output → monitor, mode,         │   ├─ sway (DRM, no input devices needed)
  │           rotation, kiosk URL             │   └─ one browser per output (kiosk mode)
  └─ reads /sys/class/drm (EDID)              └─ status: outputs, modes, browser state
```

- **Fault isolation:** the output runs in its own service, user and login
  session. A browser or compositor crash does not affect CNM; a CNM restart
  leaves the displays running.
- **Protocol:** HTTP over a Unix socket; `GET /v1/desired` as a long poll
  (revision + instance epoch), `POST /v1/status` every 3 s. Only the display
  user's UID may connect (`SO_PEERCRED`), because the desired state contains
  kiosk identities.
- **Agent:** a reconciler sets outputs via sway IPC (the exact mode from the
  compositor's mode list, transform, non-overlapping positions), switches
  unassigned outputs off, starts one browser per output and places its
  window full screen on its output.
- **Watchdogs:** browser exit (backoff 2–60 s), missing window (60 s),
  end-to-end (CNM reports how long the output's viewer has not been
  connected; more than 60 s → restart), compositor exit.
- **Credentials:** each display has its own kiosk token (hashed and
  encrypted in `config.json`); the browser receives it through a start file
  readable only by the display user, never on the command line.

## Device time

Recorders store local wall-clock time. CNM measures
`offset = device time − UTC` (time zone plus drift) and converts search
ranges and playback URLs with it. Without a device time (e.g. the native
Dahua protocol), the server's time zone applies.

## Shutdown

In parallel: the HTTP server stops accepting, the hub closes WebSockets
with a code, after 300 ms all media and playback connections are closed
hard, devices close without waiting for a logout, and the managed go2rtc is
stopped. A hard guard ends the process after 5 seconds at the latest.
