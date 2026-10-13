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
