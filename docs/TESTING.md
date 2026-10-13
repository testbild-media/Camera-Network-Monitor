# Testing

CNM is tested against **protocol simulators** (`internal/sim`, also available
as the `cnm-sim` program) that send real H.264 video and hold recordings, and
against the real go2rtc and FFmpeg binaries. No hardware is needed for any
automated test.

## Running the tests

```bash
go test ./...                               # all platforms; playback tests use FFmpeg if present
go test -race ./...                         # Linux, race detector
node --test web-tests/timeline.test.mjs     # timeline logic of the browser code
CNM_GO2RTC_BIN=/path/to/go2rtc go test -run TestRealGo2rtc ./internal/media/   # against a real go2rtc
```

Linux integration tests (no root needed), run against a built release:

```bash
scripts/build-release.sh 1.1.0 --arch amd64
scripts/test-installer.sh dist/camera-network-monitor-1.1.0-linux-amd64.tar.gz
scripts/test-setup.sh dist/camera-network-monitor-1.1.0-installer.run dist/camera-network-monitor-1.1.0.tar.gz
scripts/test-display.sh path/to/linux/cnm   # wall displays against a real (headless) sway
```

| Script | What it checks |
|---|---|
| `test-installer.sh` | fresh install and upgrade over an old installation; `config.json` and `master.key` stay byte-identical; a missing `master.key` aborts without changes; one-time import of local data; old logins, recorder passwords and kiosk keys still work; access matrix; service runs without an interactive environment; shutdown in under 5 s with open connections; no orphaned child processes |
| `test-setup.sh` | single-file installer and complete archive (checksums, architecture, damaged download rejected); graphics card detection and driver packages per distribution (dry run with simulated hardware); questions first, abort changes nothing, passwords never printed; administrator and monitor assignment through the real API; several graphics cards; kiosk browsers |
| `test-display.sh` | outputs, exact modes, rotation and placement with a real sway; resolution change without reload; browser and compositor crashes; the display survives a CNM restart |

## What is covered

### Devices and protocols

- Detection from protocol + address only: Dahua RPC2 login variants (the
  verified ones first, at most two rejected logins), the native protocol,
  channel count from the device; nothing reachable = network error
  (`dahua.TestDetect*`).
- The detection result is stored, cameras are created automatically, storing
  does not reconnect, later connections use the stored variant
  (`device.TestDetectionStoredAndCamerasCreated`).
- Capabilities per detected variant (`dahua.TestCapabilitiesPerVariant`).
- Migration of older configurations to protocols, keeping variant and
  channel count (`config.TestMigrateManufacturerModelToProtocol`,
  `config.TestParseLegacyConfig`).
- Sofia/DVRIP search with paging, two-connection playback across file
  boundaries, jump to the start time, recorders without a disk
  (`sofia.*`, `playback.TestDVRIPPlaybackPipeline`).
- Native Dahua protocol: challenge login, recording clusters, download as
  DHAV, cleanup (`dahua.TestNative*`, `playback.TestDHAVPipeline`).
- Hikvision ISAPI, Axis VAPIX, Uniview, ONVIF (stream addresses, metadata
  profiles, Profile G replay), generic RTSP/RTMP/HTTP/TCP sources
  (`hikvision.*`, `axis.*`, `uniview.*`, `generic.*`, `rtsp.*`).
- A wrong password is tried once and never again
  (`device.TestAuthFailureIsNotRetried` and per-protocol tests).
- Network search: parsers for ONVIF, DVRIP, Dahua, SADP and SSDP replies,
  merging per address, suggestions resolve to existing protocols, address
  ranges (`discovery.*`).

### Live video and media

- Desired-state computation, sub/main fallback in both directions, preview
  uses the sub stream only (`media.TestCamerasPolicies`,
  `TestBidirectionalFallbackSources`, `TestPreviewIsSubOnly`).
- Reconciler: registration, stale preload repair, go2rtc restart, verified
  timeouts, missing preload API, no duplicate registrations
  (`media.TestReconcile*`).
- go2rtc API calls are serialised; with a real go2rtc: 64 parallel
  registrations, nothing written to disk, ONVIF sources
  (`TestClientSerialisesChanges`, `TestRealGo2rtc*`).
- Disabling a camera ends all its live sessions (administrator and kiosk at
  the same time, upstream too), other cameras are not disturbed, no new
  sessions, the layout is kept (`server.TestDisablingCameraEndsAllStreams`,
  `TestDisabledCameraCannotBeAssignedAnew`).

### Playback

- Search and fragmented-MP4 playback, structured errors, FFmpeg ends with
  the client, download file names, error classification
  (`playback.*`).
- Recording availability index and automatic storage detection
  (`playback.TestRecordingAvailabilityIndex`,
  `TestStorageDetectedAutomatically`).
- Timeline segments, snapping into recordings (`web-tests/timeline.test.mjs`).

### Security

- Access matrix: anonymous (also from the server itself) sees nothing,
  viewers have no settings, the first administrator only via the server
  itself, CSRF, login rate limit (`server.TestAccessMatrix`,
  `TestLoginRateLimit`, `auth.*`).
- Kiosks: limited to one monitor on every endpoint, live only, revoking
  ends sessions and media at once (`TestKioskScopeAndParallelIdentity`,
  `TestKioskRevokeTerminatesEverything`).
- Recorder passwords never reach a browser; credentials are redacted in
  logs, also URL-encoded (`TestSecretsNeverReachBrowser`,
  `provider.TestRedactPlainAndEncodedUserinfo`).
- The last administrator is protected (`TestLastAdminProtection`).
- Legacy password hashes and encrypted values from older versions are
  readable (`secret.*`).

### Wall displays

- EDID parsing, outputs per graphics card (`display.*`).
- Assignment API, kiosk identity per output, credential never on the
  command line (`server.TestLocalDisplaysAPI`,
  `agent.TestStartPageHidesCredential`).
- Modes, rotation, placement, crash handling and watchdogs
  (`agent.TestReconcile*`, `TestBrowserCrashBackoffAndRestarts`,
  `TestWindowNeverAppears`).
- Browser preference and command line (`agent.TestBrowserPreference`,
  `TestBrowserCommandLine`).

### Continuous operation

- No growing listeners or goroutines over keep-alive connections
  (`server.TestKeepAliveConnectionDoesNotLeak`).
- Sessions and rate-limit entries expire; sessions survive restarts, a
  logout stays effective (`auth.TestSessionExpiryAndSweep`,
  `TestSessionsSurviveRestart`).
- `go test -race ./...` is clean on Linux.

## Manual checks

Some behaviour is checked by hand in a browser against the simulators and
go2rtc: tile recovery after stream failures, the server-offline banner and
reconnect, kiosk re-login after a server restart, the layout editor
(drag and drop, group drop, collapsed lists), playback controls, and that
no live players keep running in the settings.

Local multicast search can only be tested on a real network: Go disables
multicast loopback, so a simulator and the scanner on the same machine do
not see each other.

For checking real devices, see
[PROTOCOLS.md](PROTOCOLS.md#checking-a-device).
