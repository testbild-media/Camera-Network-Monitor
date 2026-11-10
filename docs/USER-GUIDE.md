# User guide

This guide walks through the web interface of Camera Network Monitor (CNM).
The interface itself is in German; the German labels are given in *italics*,
with an English explanation where it helps.

Installation and server administration are covered in
[OPERATIONS.md](OPERATIONS.md).

## Contents

- [Basic ideas](#basic-ideas)
- [Logging in](#logging-in)
- [The multiviewer](#the-multiviewer)
- [Full-size view](#full-size-view)
  - [Steering a camera (PTZ)](#steering-a-camera-ptz)
- [Playback and download](#playback-and-download)
- [My account and start screen](#my-account-and-start-screen)
- [Settings](#settings)
  - [Monitors / layouts](#monitors--layouts)
  - [Cameras](#cameras)
  - [Camera groups](#camera-groups)
  - [Devices](#devices)
  - [Displays](#displays)
  - [Users and kiosk links](#users-and-kiosk-links)
  - [System](#system)
- [Status messages](#status-messages)

## Basic ideas

| Term | Meaning |
|---|---|
| **Device** (*Gerät*) | A recorder (DVR/NVR), an IP camera or another video source that CNM connects to. |
| **Camera** (*Kamera*) | One video channel of a device. A 16-channel recorder provides 16 cameras. CNM creates them automatically. |
| **Group** (*Kamera-Gruppe*) | A freely named collection of cameras, e.g. "Outdoor" or "Entrance". A camera can be in several groups. |
| **Monitor / multiviewer** (*Monitor*) | A screen layout: a grid of tiles (*slots*), each showing one camera. |
| **Main stream / sub stream** | Most cameras send two versions of their picture: a sharp one (main) and a light one (sub). CNM uses the sub stream for tiles and the main stream for the full-size view – automatically. |
| **Display** | A monitor connected directly to the CNM server. |
| **Kiosk link** | A web address that shows exactly one multiviewer, live only, without a login form – for wall screens and smart TVs. |

## Logging in

Open `http://<server>:8090/` in Chrome, Edge or Firefox and log in with your
user name and password. After five wrong attempts, logging in is blocked for
ten minutes.

There are two roles:

- **Viewer** – watch multiviewers, open cameras full size, playback.
- **Admin** – everything a viewer can do, plus all settings.

The very first administrator is created during installation (or on the
server itself, see [OPERATIONS.md](OPERATIONS.md#first-administrator)).

## The multiviewer

After logging in you see a multiviewer. The top bar contains:

- the **monitor selection** (only if there is more than one multiviewer),
- the **media status** chip – green when the video service runs normally; a
  click shows details if something is wrong,
- *Einstellungen* (settings, administrators only),
- your **user name** – opens *Mein Konto* (my account),
- *Abmelden* (log out).

Each tile shows the camera name and the live picture. Tiles work
independently: if one camera fails, only its tile shows *STREAM OFFLINE*
and recovers by itself as soon as the camera delivers again.

If a camera's sub stream does not deliver a picture, the tile switches to the
main stream on its own and shows a small *Main* badge.

**Click a tile** to open the camera in the full-size view.

## Full-size view

The full-size view shows the main stream of the camera in the best available
quality. If the main stream does not work, CNM shows the sub stream and says
so (*Substream*).

- Click the picture or press *Schließen* (close) to return to the
  multiviewer.
- Press *Playback* to open the recordings of this camera. The button only
  appears if the device has recordings for this camera.

### Steering a camera (PTZ)

Cameras that can pan, tilt and zoom (currently with the protocol *Reolink*,
*experimental*) show a control panel in the full-size view; *PTZ* in the
header shows or hides it. Both lenses of a dual-lens camera (wide angle and
tele) move together, so they share one control.

Such cameras usually work by themselves: they rest in a **fixed position**
(the camera calls it its guard position), follow people or vehicles
(tracking) and then return to the fixed position. Manual control works
alongside that:

1. Press **Steuerung übernehmen** (take control). CNM switches the camera's
   tracking off and postpones its own return to the fixed position, so it
   does not pull away while you steer. Only you can steer now; other users
   see who is steering and may take over after a confirmation.
2. Hold a direction button (or an arrow key), use **+** / **−** to zoom,
   pick a **stored position**, or press **⌂** to go to the fixed position.
   The slider sets the speed. A movement stops when you let go – and also by
   itself after a few seconds if the connection to the browser is lost.
3. Press **Steuerung beenden** (end control): the camera moves back to its
   fixed position, and tracking and the return are switched on again
   exactly as they were before.

If nobody touches the controls, CNM ends the control by itself after the
**return time** (default 5 minutes, set per device under *PTZ: Rückkehr
nach (Minuten)*); the panel shows the remaining time. Closing the full-size
view keeps the control until then. If the camera is offline at that moment
or CNM restarts, CNM restores the camera as soon as it can reach it again.
As a second safety net, the camera's own return time is set to the same
value while you steer (if the camera does not accept that, its return is
switched off and CNM switches it on again afterwards).

Administrators can store the current position as the new fixed position
(*Als feste Position speichern*). Stored positions themselves are created
in the camera's app.

## Playback and download

Playback starts with a **time range picker**:

1. Choose a window of 1 h, 6 h or 24 h and move it with the arrows or the
   date fields. The **timeline** shows where recordings exist.
2. Drag across the timeline to select a range, or click once to select
   15 minutes from that point.
3. Press *Abspielen* (play).

In the player:

| Action | How |
|---|---|
| Play / pause | button or **space bar** |
| Jump | click on the timeline; a click into a gap jumps to the next recording |
| 10 seconds back / forward | buttons or **← / →** |
| Back to live | *Live* |

**Download:** *Als MP4 herunterladen* saves the selected range (up to two
hours) as an MP4 file. Sofia/DVRIP and Dahua recorders with the native
protocol send the recording as fast as they can; with other protocols the
download takes about as long as the selected range.

Kiosk links and wall displays never offer playback.

## My account and start screen

Click your user name in the top bar. Under *Startbildschirm* (start screen)
you choose what appears after logging in:

- *Standard* – the multiviewer you used last,
- a specific multiviewer,
- for administrators: a settings page.

The choice applies to your user only. If the chosen multiviewer is deleted
or deactivated, the standard applies again.

## Settings

*Einstellungen* is only available to administrators. While you are in the
settings, no live video runs in the background; the multiviewer reconnects
when you return.

### Monitors / layouts

*Monitore / Layouts* lists all multiviewers.

- *Monitor hinzufügen* creates a new one. *Monitor aktiv* switches it on or
  off; inactive multiviewers are hidden from viewers.
- *Layout bearbeiten* opens the **layout editor**:
  - *Vorlage* – choose a template (2 × 2 up to 6 × 6, or mixed layouts such
    as 4 + 4 + 2 with larger tiles in the last row) and press *Anwenden*.
    Cameras already assigned are carried over in order.
  - Select a slot to change its width, its row height, its label or to
    clear it. *Abstand (Gap)* sets the space between tiles.
  - The **camera list** on the side can be sorted by *Geräte* (devices) or
    *Gruppen* (groups) and searched. All entries start collapsed; click a
    header to open it.
  - **Drag a camera** onto a slot to assign it. Drag a slot onto another slot
    to swap them, or back into the list to clear it.
  - **Drag a whole device or group** (its header) onto a slot: its cameras
    fill this slot and the following ones, left to right, row by row. If not
    all of them fit, the editor tells you how many were left out.
- *Speichern* (save) – the layout is active immediately on all screens
  showing this multiviewer.

### Cameras

*Kameras* lists every camera of every device.

- The switch in each row **enables or disables** the camera. A disabled
  camera produces no video traffic at all: CNM ends all its streams at once
  – towards the recorder and towards every browser, kiosk and display – and
  starts no new ones. Its slots in layouts are kept and show *Kamera
  deaktiviert*.
- *Bearbeiten* (edit) opens the camera: name (empty = the name supplied by
  the device), aspect ratio (*Bildformat*: automatic, contain, cover/crop,
  stretch), groups, and a small preview (sub stream only).
- The column *Aufnahmen* shows whether recordings exist on the device. CNM
  checks this by itself every 10 minutes and after each reconnect.
- Select several cameras with the checkboxes to enable, disable, set the
  aspect ratio or assign groups for all of them at once.

### Camera groups

*Kamera-Gruppen*: *Gruppe anlegen* creates a group. Select a group on the
left to add or remove cameras – one by one or all currently shown
(*Alle angezeigten hinzufügen / entfernen*). Groups can be renamed and
deleted at any time; this never affects running video.

### Devices

*Geräte* lists all devices with their status (*Verbunden* = connected,
*Getrennt* = disconnected, *Fehler* = error) and what CNM detected.

**Adding a device** – *Gerät hinzufügen*:

1. *Protokoll* – choose the protocol the device speaks, for example *Dahua*
   or *Sofia (DVRIP)*. You do not need to know the model; many devices sold
   under other brand names speak one of these protocols. See
   [PROTOCOLS.md](PROTOCOLS.md) if you are unsure.
2. *IP / Hostname* – the address of the device.
3. *Benutzername / Passwort* – the login of the device. The password is
   stored encrypted and never sent to a browser.
4. *Speichern* (save).

CNM connects, finds out the details itself (sub-protocol, login variant,
number of channels) and creates the cameras. The device list then shows what
was detected, for example "RPC2 über HTTP 80 · Live über RTSP 554".

Optional fields:

- *Label* – a name for the device (otherwise the address is used).
- *Erweitert (Ports, Kanäle)* – only needed if the device does not use its
  factory ports, or to override a wrongly detected number of channels.
- For plain stream sources (RTSP, RTMP, HTTP, TCP) you enter the stream
  path; with several channels, `{ch}` in the path is replaced by the channel
  number (`{ch0}` counts from 0, `{ch2}` uses two digits).

*Verbindung testen* (test connection) performs a real login and shows what
would be detected – without saving anything.

*Funktionen dieses Protokolls* shows what CNM can do with this protocol
(live, recording search, playback, download, device time …). *experimentell*
means: built from the protocol documentation and tested, but not yet
confirmed on real hardware.

**Wrong password:** CNM tries a wrong login at most once (during detection
at most twice) and then stops, shown as *Login gesperrt*. This protects the
recorder account from being locked. Correct the password, or press
*Reconnect* after fixing it on the device.

**Device settings** – *Geräteeinstellungen* (Dahua with RPC2 and Sofia/DVRIP)
changes settings inside the recorder itself, with the login stored in CNM –
you do not need the recorder's own menu or software for this:

- **Encoding** per channel, for the main stream (recording, full-size view)
  and the sub stream (multiviewer tiles): codec, resolution, frames per
  second, bitrate type (VBR/CBR), bitrate, quality and keyframe interval.
  *Encode von Kanal 1 auf alle übertragen* copies channel 1's values to all
  channels.
- **Channel names** as the recorder shows them.
- **Recording**: continuous, only on motion, or off – optionally only within a
  daily time window (e.g. 18:00–06:00). A more complex schedule set up on the
  recorder is shown as *Eigener Zeitplan* and kept unless you choose another
  mode.
- **Motion detection** on/off, its sensitivity and – where offered – the
  recorder's buzzer on motion.
- **Alarms** per channel: *Videoverlust* (the camera signal is lost) and
  *Sabotage* (the camera is covered) – on/off, the recorder's buzzer
  (*Summer*) and recording the channel on the event (*Aufnahme*).
- **Disk alarms**: how the recorder reacts when there is no disk, a disk
  fault or little free space (on/off, buzzer, limit in percent). With
  *overwriting* the disk is always full – leave *Wenig Speicherplatz* off
  then.
- **Time**: the recorder's clock compared with the server, *Uhr jetzt auf
  Serverzeit stellen*, and the time server (NTP).
- **Disk**: size, free space, the period the recordings cover (where the
  recorder reports it), whether the oldest recordings are overwritten when
  the disk is full, and an estimate of how many days of recordings fit with
  the chosen bitrates. If the newest recording is older than a few hours
  although recording is switched on, CNM warns that the recorder is not
  recording – usually a faulty or unformatted disk. Recorders that do not
  report their disk: CNM adds up their recording files instead – once the
  disk is full and overwritten, that is about its size.
- **System**: the device name, the video standard (PAL/NTSC – it must
  match the cameras, a change restarts the recorder) and, on Sofia/DVRIP,
  the automatic maintenance restart (day and hour, or never). A recorder
  with an empty clock battery may lose its time at every restart.
- **Network** – shown only (address, mask, gateway, DNS, DHCP, MAC, ports):
  a wrong value changed from afar could make the recorder unreachable.
- **Users** (*Benutzer des Rekorders*): the recorder's user accounts and
  *Passwort ändern …*. For the account CNM logs in with, CNM knows the old
  password; for others it is asked for. CNM checks a new password with a
  second, separate login and only then relies on it – if that login fails,
  CNM restores the old password at once. A new password of CNM's own
  account is stored in CNM, and the recorder connects again with it.
  *Passwort mehrerer Rekorder ändern* in the device list sets one new
  password for CNM's account on several recorders, one after another, each
  checked the same way. Apps and programs that log in with the old password
  have to be changed afterwards. Passwords are never shown or logged.
- *Neu starten* restarts the recorder.
- **Recorder log** (*Protokoll des Geräts*, Sofia/DVRIP): the recorder's
  own event log – restarts, clock changes, changed settings, logins –
  filtered by kind and period. Useful to find out from afar why a recorder
  stopped recording or loses its time.
- **Format the disk** (*Formatieren …*, Sofia/DVRIP): deletes all
  recordings of the partition. To confirm, type the device name. It takes a
  few minutes; some recorders restart afterwards. If the disk itself is
  defective, the recorder refuses or the error remains – then it has to be
  replaced.

Before saving, CNM lists what will be changed. Values outside what CNM knows
for the recorder are shown as a hint – the recorder decides; *Trotzdem
senden* sends them anyway. If the recorder refuses an encoding, CNM explains
per channel which resolutions the recorder allows. CNM does not judge the
encoder load: the capacity older recorders report is no real limit.

Several channels are saved **one after another**: many recorders refuse a
whole block when a single channel in it has a value they do not accept. A
channel the recorder refuses is listed under *Nicht alles gespeichert*; the
other channels are saved. Older DVRIP recorders allow only certain sub
stream resolutions with each main resolution (with a QCIF main stream, for
example, only a QCIF sub stream) – even for a switched-off sub stream. CNM
then sets the largest allowed sub stream resolution itself and says so.

After saving, CNM reads the settings back from the recorder; values the
recorder did not take over as requested (for example a bitrate it limits)
are listed. Older recorders do not report which values they accept – CNM
then offers common values, and the recorder may refuse some. Live video and
recording of the changed channels can be interrupted briefly while the
recorder applies the change.

**Recorder monitoring** – CNM checks every connected recorder with device
settings once an hour: whether a disk is present and reports no fault,
whether the newest recording is recent, whether every channel that records
around the clock has recorded in the last three hours, whether a full disk
is not overwritten, and whether the recorder's clock is right. Problems are
shown in the device list, at the top of *Geräteeinstellungen* (with *Jetzt
prüfen* for an immediate check) and – for admins – as *Rekorder mit
Problemen* in the top bar. Channels set to motion recording, to a time
window or to no recording are not expected to have recent recordings.

**Network search** – *Netzwerk durchsuchen* looks for devices in the local
network and lists only those that are not added yet. *Hinzufügen* opens the
device dialog with the protocol and address filled in. For devices behind a
router or firewall, enter an **address range** – a single address or a
network in CIDR notation, at most /22 (1024 addresses).

### Displays

*Displays* is only relevant if monitors are connected directly to the CNM
server (see [OPERATIONS.md](OPERATIONS.md#wall-displays)). Each graphics
output (e.g. `HDMI-A-1`, `DP-1`) appears with the name of the connected
screen.

| Field | Effect |
|---|---|
| *Multiviewer* | what this output shows; *– Aus –* switches the output off |
| *Auflösung* | resolution; *Automatisch* uses the screen's preferred mode |
| *Drehung* | rotation – normal, 90°, 180°, 270° for portrait screens |
| *Bezeichnung* | an optional name, shown when identifying |

- *Identifizieren* shows the output name in large letters on that screen for
  10 seconds.
- *Neu laden* restarts the display on this output.
- *Browser* – *Automatisch* uses Firefox, otherwise Chromium. Browsers that
  are not installed are marked and cannot be chosen.
- *Tägliche Auffrischung* – a time of day at which all displays reload
  briefly (empty = off).

### Users and kiosk links

*Benutzer*:

- *Benutzer hinzufügen* – user name, password (at least 8 characters) and
  role (*Admin* or *Viewer*). Users can be deactivated (*Account aktiv*) or
  deleted. The last administrator cannot be removed.
- **Kiosk keys** – *Key erzeugen*: choose a multiviewer and a label. CNM
  shows a link of the form `http://<server>:8090/viewer?mon=…&key=…`. Open it
  in the browser of the wall screen. The kiosk shows only this multiviewer,
  full screen, live only, and logs in again by itself after a server restart.
  *URL kopieren* copies the link again later. *Widerrufen* (revoke) blocks
  the link immediately – the screen then shows *ZUGRIFF GESPERRT*.

### System

*System* contains the system name (shown in the top bar), the web port,
reconnect timing and the video service options. The defaults suit almost
every installation; see
[OPERATIONS.md](OPERATIONS.md#system-settings) for what each option does.

## Status messages

| Message | Meaning |
|---|---|
| *STREAM OFFLINE* | The camera does not deliver a picture at the moment. The tile retries by itself. |
| *STREAM OFFLINE* with "Browser kann H.264/H.265 nicht abspielen" | The browser lacks the video codecs. Use Chrome, Edge or Firefox. |
| *SERVER OFFLINE* | The browser lost the connection to CNM, e.g. during an update. It reconnects by itself. |
| *Media offline* | CNM runs, but its video service does not. See [OPERATIONS.md](OPERATIONS.md#troubleshooting). |
| *Kamera deaktiviert* | The camera was disabled under *Kameras*. |
| *ZUGRIFF GESPERRT* | The kiosk link was revoked. |
| *Login gesperrt* | Wrong login for this device; CNM does not retry until the login is corrected. |
| The first picture takes a few seconds | The camera sends a full frame only every few seconds. This is normal. |
