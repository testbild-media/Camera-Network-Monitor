# Changelog

## 1.1.0 – first public release

Camera Network Monitor was rewritten from scratch as a single Go program with
an embedded web interface. Existing installations of earlier versions
(including the 0.3.x series) are upgraded in place; settings, users, devices,
passwords and kiosk links are kept.

### Devices

- Devices are added by **protocol** only: choose e.g. *Dahua* or *Sofia
  (DVRIP)*, enter the address and the login, save. CNM detects the
  sub-protocol, the login variant and the number of channels on the first
  connection, stores the result and creates the cameras automatically.
- Protocols: Dahua (RPC2 over HTTP and the native protocol), Sofia/DVRIP,
  Hikvision ISAPI, Axis VAPIX, Uniview, ONVIF Profile S/G, RTSP, RTMP, HTTP
  streams and raw H.264 over TCP. Devices without a protocol of their own in
  CNM are added via ONVIF.
- Network search (ONVIF, DVRIP, Dahua, Hikvision SADP, Axis SSDP) with an
  optional address range for devices behind a router or firewall.
- A wrong password is never retried automatically, so recorder accounts are
  not locked.
- Recorders without a hard disk are recognised; the playback button is only
  shown where recordings exist.

### Cameras and multiviewers

- Cameras are objects of their own: names, aspect ratio, groups (a camera may
  be in several groups), enable/disable. Disabling a camera immediately ends
  all of its streams – towards the recorder and towards every viewer.
- Automatic stream choice: tiles use the sub stream, the full-size view the
  main stream; if one of them fails, the other is used.
- Layout editor with templates, search, devices or groups as a collapsible
  list; dragging a whole device or group onto a slot fills the following
  slots in reading order.
- Start screen per user (last multiviewer, a fixed multiviewer or – for
  administrators – a settings page).

### Playback

- Timeline with the recordings of 1 h, 6 h or 24 h, range selection, seek,
  ±10 s, MP4 download of up to 2 hours.

### Wall displays

- Multiviewers on the server's own HDMI/DisplayPort outputs, configured in
  the web interface: resolution, rotation, browser, daily refresh.
- Firefox and Chromium are both installed (Debian); the kiosk shows no
  translation offer.
- Kiosk links (live only, one multiviewer, revocable).

### Installation and operation

- One installer for amd64, arm64 and armv7 that asks all questions first,
  installs graphics drivers, browsers and CNM, creates the first
  administrator and assigns the connected monitors.
- Updates keep all data; the new version checks the existing configuration
  before anything is replaced, and the previous version is kept for a
  rollback.
- The bundled go2rtc gets its configuration directly at start, so no stream
  addresses with recorder logins are written to disk.
