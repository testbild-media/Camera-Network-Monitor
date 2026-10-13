# Protocols

CNM does not know device models. When you add a device, you only choose the
**protocol** it speaks – for example *Dahua* (also spoken by many recorders
sold under other brand names) or *Sofia (DVRIP)* – enter its address and
login, and save. Everything else CNM finds out itself on the first
connection:

- the **protocol variant** – for Dahua, for example, whether the device is
  controlled over HTTP (RPC2) or only over the native protocol, and which
  login variant it expects; for DVRIP, whether live video uses one or two
  connections;
- the **number of channels** – the cameras are then created automatically.

The result is shown in the device list (e.g. "RPC2 über HTTP 80 · Live über
RTSP 554") and stored; later connections use the stored variant directly.
Ports and a different number of channels can be entered under *Erweitert* –
only needed if the device does not use its factory ports or the number of
channels is detected wrongly. If the address, the protocol or the ports
change, CNM detects again.

## Which protocol do I choose?

| Your device | Choose |
|---|---|
| Recorder or camera with a Dahua-based web interface, or one that uses TCP port 37777 | **Dahua** |
| Recorder that works with the XMEye or NETSurveillance app, TCP port 34567 | **Sofia (DVRIP)** |
| Hikvision recorder or camera | **Hikvision** |
| Axis camera or video encoder | **Axis** |
| Uniview recorder or camera | **Uniview** |
| Any other device with ONVIF support | **ONVIF** |
| A device that only offers a stream address | **RTSP**, **RTMP**, **HTTP** or **TCP** |

If you are unsure, use *Netzwerk durchsuchen*: the network search suggests
a protocol for every device it finds.

## Overview

| Protocol / detected variant | Live | Recording search | Playback / download | Device time | Network search |
|---|---|---|---|---|---|
| Dahua – RPC2, login "Dahua3.0-Web3.0" | yes | yes | yes | yes | experimental |
| Dahua – RPC2, login "Web3.0" (form) | yes | yes | yes | yes | experimental |
| Dahua – RPC2, current web login | yes | experimental | experimental | experimental | experimental |
| Dahua – native protocol (TCP 37777), live via RTSP | yes | experimental | yes | – | experimental |
| Sofia (DVRIP) | yes (one or two connections, automatic) | yes | yes | experimental | experimental |
| Hikvision (ISAPI) | experimental | experimental | experimental | experimental | experimental |
| Axis (VAPIX) | experimental | experimental | experimental | experimental | experimental |
| Uniview (RTSP) | experimental | – (availability check) | experimental | – | experimental |
| ONVIF (Profile S/G) | yes | experimental (time span only) | experimental | experimental | experimental |
| RTSP / RTMP / HTTP / TCP | yes | – | – | – | port check (address range) |

What the levels mean:

| Level | Meaning |
|---|---|
| **yes** | verified on real hardware and tested against the simulator |
| **experimental** | implemented from the public protocol documentation and tested against the simulator, not yet confirmed on real hardware – enabled |
| **–** | not available for this protocol |

The device dialog shows these levels under *Funktionen dieses Protokolls*.
After detection, the levels of the detected variant apply (the native Dahua
protocol, for example, reports no device time). PTZ control, device
configuration and events are not offered for any device at the moment.

If you use a device with an *experimental* protocol, feedback is very
welcome: which protocol and variant were detected, the firmware version,
and what works.

## Notes per protocol

### Dahua

- **Detection:** first RPC2 on the HTTP port, trying the login variants in
  the order in which they were verified. The first login step (the
  challenge) does not count as a login attempt; a variant whose challenge
  is not answered is skipped. **At most two rejected logins** per detection –
  recorders lock accounts after a few failures. Without RPC2, CNM checks the
  challenge of the native protocol (TCP 37777, no login) and the login via
  RTSP.
- **Channels:** with RPC2 from the channel title table; with the native
  protocol by asking RTSP channel by channel until one is refused.
- **Playback:** RPC2 devices via RTSP (`/cam/playback`); native devices over
  the native port, which delivers the recording as fast as the device can.
  An error on the native port does **not** lock the device – live video via
  RTSP keeps working.

### Sofia (DVRIP)

- **Detection:** the number of channels comes with the login answer. Whether
  live video uses one or two connections is detected on every connect.
- **Recording search** with paging through all files of the time range.
- **Playback:** file by file over a second connection; CNM jumps to the
  requested moment and joins the following files up to the end of the
  range. The recorder sends as fast as it can, so downloads are quick.
- Recorders without a hard disk are recognised (no playback button).

### Hikvision (ISAPI)

- HTTP Digest login; after a rejected login, no retry (Hikvision would
  otherwise block the address).
- **Channels:** IP channels of a recorder, or the video inputs of a camera.
- Search and playback via ISAPI and RTSP. Recorders usually store local
  time; CNM measures the device time and converts.

### Axis (VAPIX)

- HTTP Digest login; a rejected login stops automatic retries.
- **Channels:** the video sources (multi-sensor cameras, encoders).
- Recordings on edge storage (SD card or NAS) are listed and played back via
  RTSP.

### Uniview (RTSP)

- **Channels:** by asking RTSP channel by channel until one is refused.
- There is no recording list: CNM asks the recorder via RTSP whether a time
  range contains material and plays it back the same way. Devices with
  ONVIF Profile G can alternatively be added as *ONVIF*.

### ONVIF (Profile S/G)

- **Channels** are the video sources; the first video profile of a source is
  the main stream, the second the sub stream. Profiles without video
  (metadata, audio), which some devices list first, do not count.
- CNM determines the stream addresses itself; addresses announced for
  another host (NAT, port forwarding) are rewritten to the address you
  entered.
- Profile G recordings: the device only reports the total span of each
  recording (first to last, no gaps), so the timeline is marked as
  approximate.

### RTSP, RTMP, HTTP, TCP

- No detection possible: the stream path (with several channels with the
  placeholder `{ch}`) and the number of channels are entered in the device
  dialog. HTTP accepts MJPEG, JPEG snapshots, raw H.264 and MPEG-TS; TCP
  expects a raw H.264 stream.

### Devices without a protocol of their own in CNM

Devices whose manufacturers only offer their own SDKs besides ONVIF are
added via **ONVIF Profile S/G**. The network search suggests ONVIF for them.

### GB/T 28181

Not supported: GB/T 28181 is a platform protocol in which devices register
with a central server; it offers no direct device access. Such devices can
usually also be added via ONVIF or RTSP.

## Network search

*Einstellungen → Geräte → Netzwerk durchsuchen* (administrators only, runs
about 4 seconds, only when you press the button):

| Search | How |
|---|---|
| ONVIF WS-Discovery | multicast, UDP 3702 |
| DVRIP | broadcast, UDP 34569 |
| Dahua | multicast, UDP 37810; older firmware: broadcast, UDP 5050 |
| Hikvision SADP | multicast, UDP 37020 |
| Axis SSDP | multicast, UDP 1900 (answers even with ONVIF switched off) |

The search suggests a **protocol** for each device (and ports, where they
differ from the factory ports). If a device answers via ONVIF and its ONVIF
information reveals a protocol of its own in CNM, that protocol is
suggested; otherwise ONVIF.

Answers are merged per address; devices that are already added are hidden.
*Hinzufügen* opens the device dialog with protocol, address and ports
filled in.

Multicast and broadcast do not pass routers. For devices in other networks
(for example behind a firewall), enter an **address range** – a single
address or a CIDR network up to /22. CNM then asks every address directly
and also checks TCP ports 80, 554, 8000, 34567 and 37777.

## Factory ports

| Protocol | Ports |
|---|---|
| Dahua | HTTP 80, RTSP 554, native 37777 |
| Sofia (DVRIP) | 34567 |
| Hikvision | HTTP 80, RTSP 554 |
| Axis | HTTP 80, RTSP 554 |
| Uniview | RTSP 554 |
| ONVIF | HTTP 80 |

Different ports go under *Erweitert* in the device dialog.

## Checking a device

A recommended check per device:

1. *Netzwerk durchsuchen* – the device appears with a suggested protocol (or
   *Gerät hinzufügen*: protocol and address). *Verbindung testen* shows the
   detected variant and number of channels (real login).
2. Save – status *Verbunden*, cameras are created. Drag a camera onto a slot
   in the layout editor – the tile shows the picture.
3. Enter a wrong password – status *Fehler – Login gesperrt*; the device log
   shows at most **two** failed logins (detection) or **one** (stored
   variant), no further retries.
4. Playback: choose a range – the timeline shows recordings – play – jump,
   ±10 s – *Als MP4 herunterladen*.

## Adding a protocol (developers)

1. Create a package `internal/provider/<name>` and call
   `provider.Register(...)` in `init()`.
2. Return the protocol from `Info()`: ports, options, capabilities, whether
   it detects itself (`AutoDetect`).
3. Implement `provider.Driver` (`Connect`, `Check`, `LiveSources`, `Close`).
   `Connect` reports `Variant`, `Summary` and `Channels`; CNM stores them and
   hands the variant back as `DeviceConfig.Variant` on later connections.
   Optional interfaces: `RecordingSearcher`, `RecordingProber`,
   `PlaybackOpener`, `LegacyLiveOpener`, `Tester`, `VariantCapabilities`.
4. Check `Env.AllowLogin()` before every login and report authentication
   failures as `provider.KindAuth` (`Env.ReportAuthFailure`).
5. Optionally add a network search parser in `internal/discovery`.
6. Import the package in `cmd/cnm/main.go`. The web interface needs no
   changes – it is generated from the protocol description.
