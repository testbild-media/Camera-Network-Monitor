# Changelog

## 1.2.0

### New

- **Device settings** (*Geräteeinstellungen*) for Dahua recorders with RPC2
  and Sofia/DVRIP recorders: CNM changes settings inside the recorder with
  its stored login – encoding per channel (codec, resolution, frame rate,
  VBR/CBR, bitrate, quality, keyframe interval) for main and sub stream,
  channel names, recording mode (continuous, motion, off, optionally within
  a daily time window), motion detection and sensitivity, recorder clock and
  time server, overwriting when the disk is full, and a restart. CNM shows
  the disk with the period its recordings cover, warns when a recorder has
  stopped recording, and estimates the recording time for the chosen
  bitrates (for recorders that do not report their disk, from the size of
  their recordings), reads the recorder's own event log and formats its
  disk from afar (Sofia/DVRIP, confirmed with the device name),
  lists what will be changed before saving and reports values the recorder
  did not take over. Values outside what CNM knows for a recorder are only
  a hint (*Trotzdem senden*); when the
  recorder refuses an encoding, CNM explains it per channel. Several
  channels are saved one after another, so a channel the recorder refuses
  does not stop the others. On older DVRIP recorders CNM adapts the sub
  stream resolution to what the recorder allows with the chosen main
  resolution. Experimental: verified against simulators shaped after real
  recorders and on real recorders.

- **Alarms** in the device settings: video loss and tamper (camera covered)
  per channel with buzzer and recording, the buzzer on motion, and the
  recorder's reactions to a missing disk, a disk fault and little free
  space.
- **System and network** in the device settings: device name, video
  standard (PAL/NTSC) and – Sofia/DVRIP – the automatic maintenance
  restart; the network settings are shown (not changed).
- **Users and passwords** of the recorders: list the accounts, change a
  password – checked with a second login, the old password restored when
  that fails – and set one new password for CNM's account on several
  recorders at once; CNM takes the new password over.
- **Recorder monitoring**: CNM checks every recorder with device settings
  hourly – disk present and without fault, recording not stopped, every
  channel recording around the clock has recent recordings, full disk
  overwritten, clock right – and shows problems in the device list, in the
  device settings and for admins in the top bar.
- **Reolink cameras and recorders** (new protocol *Reolink*, experimental):
  CNM finds HTTPS or HTTP itself, switches RTSP and RTMP on when they are
  off, offers both lenses of dual-lens cameras (wide angle and tele) as
  cameras, shows live video and plays back the recordings on the SD card.
  The network search suggests the protocol.
- **PTZ control** in the full-size view (Reolink cameras): pan, tilt, zoom,
  stored positions and the fixed (guard) position. While a user steers,
  CNM pauses the camera's own tracking and its return to the fixed
  position; when the user ends the control – or after the return time
  without input (default 5 minutes, per device) – the camera returns to its
  fixed position and tracking and the return are restored exactly as they
  were, also after a restart of CNM or when the camera was offline. One
  user steers at a time; others see who and may take over. Administrators
  can store the current position as the fixed position.

### Improvements

- The action buttons in the device list wrap on narrow screens instead of
  widening the table.

### Fixes

- Dahua (RPC2): some recorders silently drop idle connections. The next
  health check then failed and CNM reconnected the device – on affected
  recorders up to a few hundred times a day. CNM now reuses connections only
  briefly and repeats a request once on a fresh connection when a reused one
  turns out to be dead (never for logins).

## 1.1.2

### Fixes

- Dahua (RPC2): some firmware counts playback channels from 0 instead of 1
  (seen on 4-channel recorders): playing camera 3 showed camera 4, camera 4
  reported "no recordings", and the firmware refuses camera 1 over RTSP. CNM
  now detects this numbering by itself. Cameras 2 and up play over RTSP with
  the device's numbering, camera 1 from the recorder's download interface.
  Because such recorders only deliver whole recording files there, starting
  camera 1 can take up to about half a minute, depending on the position
  within the recording file.

## 1.1.1

### Fixes

- Dahua (RPC2): some recorders list their recordings with times padded by
  spaces instead of zeros (e.g. `2026-11- 4 15: 0:45`). CNM could not read
  them, so the playback timeline stayed empty although the recorder had
  recordings. Both formats are read now; a time format CNM cannot read is
  reported as an error instead of an empty timeline.
- Dahua (RPC2): the check whether a camera has recordings no longer counts
  an empty answer from the recorder as "recordings available".

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
