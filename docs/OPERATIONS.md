# Operations manual

This manual covers installing, updating and running Camera Network Monitor
(CNM) on a Linux server. Using the web interface is described in the
[user guide](USER-GUIDE.md).

## Contents

- [Requirements](#requirements)
- [Directories and files](#directories-and-files)
- [Installation](#installation)
- [First administrator](#first-administrator)
- [Updates](#updates)
- [Rollback](#rollback)
- [Backups](#backups)
- [The service](#the-service)
- [Wall displays](#wall-displays)
- [Access and roles](#access-and-roles)
- [Kiosk links](#kiosk-links)
- [Lost administrator access](#lost-administrator-access)
- [System settings](#system-settings)
- [Network search and firewalls](#network-search-and-firewalls)
- [Troubleshooting](#troubleshooting)
- [Uninstalling](#uninstalling)
- [Trying CNM without hardware](#trying-cnm-without-hardware)

## Requirements

- Linux with systemd – **Debian 11 or newer** or **Ubuntu 20.04 or newer** –
  on amd64 (x86_64), arm64 or armv7.
- Network access from the server to the recorders and cameras.
- root access and access to the distribution's package sources (internet or
  a local mirror). FFmpeg, graphics drivers, the compositor and the kiosk
  browsers are installed from there.
- Everything else is included in the package, in particular the
  [go2rtc](https://github.com/AlexxIT/go2rtc) media server.

## Directories and files

| Path | Content |
|---|---|
| `/opt/camera-network-monitor` | program files (replaced on every update) |
| `/opt/camera-network-monitor.prev` | the previous program version (for a rollback) |
| `/var/lib/camera-network-monitor` | **all data – never overwritten by an update** |
| `/etc/systemd/system/camera-network-monitor.service` | the service definition |
| `/var/log/camera-network-monitor-setup.log` | log of the last setup run (passwords are never logged) |

The data directory contains:

| File | Meaning |
|---|---|
| `config.json` | devices, cameras, groups, monitors, users, displays, system settings |
| `config.json.bak` | the previous version, written automatically on every change |
| `config.json.v<N>-original` | a one-time archive of the original file before a format upgrade |
| `master.key` | the key used to encrypt recorder passwords and kiosk links |
| `sessions.json` | logged-in sessions (only hashes), so a restart does not log anyone out; no backup needed |

## Installation

Both installation files are attached to every
[release on GitHub](https://github.com/testbild-media/Camera-Network-Monitor/releases/latest).
There are two ways to install, both doing the same:

- the **complete archive** `camera-network-monitor-<version>.tar.gz`
  (contains all architectures):

  ```bash
  tar xzf camera-network-monitor-<version>.tar.gz
  cd camera-network-monitor-<version>
  ./setup.sh
  ```

- the **single-file installer** `camera-network-monitor-<version>-installer.run`:

  ```bash
  bash camera-network-monitor-<version>-installer.run
  ```

  It checks its own checksum, unpacks the package that matches the server
  and starts the same setup.

Run both as root. On Debian, become root with `su -` (with the dash); on
Ubuntu, prefix the command with `sudo`.

### What the setup does

1. It asks **all questions at the beginning** and shows a summary:
   - set up local wall displays? (suggested when a graphics card and a
     monitor are detected)
   - which **graphics driver** – the detected card is preselected: Intel
     (Mesa + VA-API), AMD (Mesa + firmware + VA-API), NVIDIA proprietary
     (Ubuntu: `ubuntu-drivers`, Debian: `nvidia-driver`; needs a reboot),
     NVIDIA nouveau, virtual machine / server graphics, or nothing
   - with several graphics cards: which card drives the wall monitors
   - Ubuntu: install Firefox from the Mozilla repository (snap browsers do
     not work for the kiosk)
   - the first **administrator** (name and password) and whether the
     connected monitors should show the default multiviewer right away
   - open the web port in the firewall (if ufw is active) and switch off a
     graphical login manager that would occupy the screens
2. It then installs **without further questions**: base packages (FFmpeg,
   curl, python3, pciutils), the graphics driver including the package
   sources it needs (Debian: `non-free-firmware`, for NVIDIA also
   `contrib non-free`; the original source files are kept as `*.cnm-bak`),
   the kiosk browsers, CNM itself, the display service, the administrator,
   the monitor assignment and the firewall rule.
3. At the end it offers a reboot if a driver needs one.

When it is done, open `http://<server>:8090/` and log in.

### Setup options

| Option | Meaning |
|---|---|
| `--yes` | no questions, use the defaults |
| `--display` / `--no-display` | set up local wall displays or not |
| `--gpu=auto\|intel\|amd\|nvidia\|nouveau\|generic\|none` | graphics driver |
| `--display-gpu=card1` | the graphics card for the wall monitors (also as PCI address) |
| `--admin-user=NAME` | create this administrator; the password is taken from the environment variable `CNM_ADMIN_PASSWORD` |
| `--no-assign` | do not assign the default multiviewer to the monitors |
| `--firefox-repo` / `--no-firefox-repo` | Ubuntu: Firefox from the Mozilla repository |
| `--open-firewall` / `--no-firewall` | ufw rule for the web port |
| `--reboot` / `--no-reboot` | reboot at the end without asking, or never |
| `--configure-only [--port=N]` | only create the administrator / assign monitors on a running CNM |
| `--dry-run` | only show what would be done |
| `--extract [DIR]` | (`.run` file only) unpack, install nothing |

Unattended example:

```bash
CNM_ADMIN_PASSWORD='…' ./setup.sh --yes --display --admin-user=admin
```

### Program only

`install.sh` in the package installs only CNM and its service – no drivers,
no questions:

```bash
./install.sh                 # options: --no-apt  --no-start  --force  --with-display
```

`--with-display` also sets up the wall display service (see below).

## First administrator

The setup creates the first administrator. If it was skipped, open
`http://127.0.0.1:8090/` **on the server itself** (for example with an SSH
tunnel). As long as no user exists, CNM offers to create the first
administrator – only to requests from the server itself, never over the
network. The password must have at least 8 characters.

## Updates

Unpack the new archive and run `./setup.sh` again (or the new `.run` file).
Settings, users, devices, passwords, kiosk links and drivers stay as they
are; the driver question then defaults to "install nothing".

What happens during an update:

1. **Check first:** the *new* program reads the existing `config.json` and
   `master.key` (read-only) and checks that all stored passwords can be
   decrypted. If not, the update stops before anything was changed.
2. The service is stopped and the program files are replaced; the old
   version is moved to `/opt/camera-network-monitor.prev`.
3. `config.json` and `master.key` are not touched.
4. The service definition is updated and the service is started and checked.

On the first start, CNM converts older configuration formats automatically
and archives the original once as `config.json.v<N>-original`. Devices from
older versions keep their login variant and number of channels and connect
exactly as before.

Check a configuration by hand at any time (changes nothing):

```bash
/opt/camera-network-monitor/bin/cnm check-config --data-dir /var/lib/camera-network-monitor
```

## Rollback

```bash
systemctl stop camera-network-monitor
mv /opt/camera-network-monitor /opt/camera-network-monitor.failed
mv /opt/camera-network-monitor.prev /opt/camera-network-monitor
systemctl start camera-network-monitor
```

If the configuration format was converted during the update, copy the
archived `config.json.v<N>-original` back to `config.json` before starting
the old version.

## Backups

Back up `config.json` and `master.key` **together**:

```bash
tar czf cnm-backup-$(date +%F).tar.gz -C /var/lib/camera-network-monitor config.json master.key
```

Without the matching `master.key`, the stored recorder passwords and kiosk
links cannot be decrypted. CNM then deliberately refuses to start and
reports the reason in the journal. Keep the backup in a safe place – it
contains everything needed to log in to your recorders.

## The service

```bash
systemctl status camera-network-monitor
journalctl -u camera-network-monitor -f
```

- CNM runs as the restricted user `cnm` and only writes to its data
  directory.
- It restarts automatically after a crash; a watchdog restarts it if the web
  server stops answering.
- Stopping or restarting takes less than 5 seconds, even with open viewers.
- go2rtc and FFmpeg run as child processes of CNM and end with it.
- Its processes are invisible to other users, because FFmpeg command lines
  contain recorder addresses.
- For detailed logs, set `CNM_LOG_LEVEL=debug` with
  `systemctl edit camera-network-monitor`.

## Wall displays

The server can show multiviewers on its own monitors (HDMI, DisplayPort) –
without a desktop, mouse or keyboard. Which multiviewer appears on which
output, in which resolution and rotation, is set in the web interface under
*Einstellungen → Displays* (see the [user guide](USER-GUIDE.md#displays)).

### Setup

The setup installs this when you answer "yes" to local displays. By hand:

```bash
./install.sh --with-display
```

This installs:

- the packages `sway` (a minimal display compositor), `kbd`,
  `fonts-dejavu-core` and the kiosk browsers if missing,
- a system user `cnm-display`,
- the service `camera-network-monitor-display` (runs on the seventh console
  in its own login session, not as root).

Later updates keep the display service up to date automatically.

### Browsers

Debian gets **Firefox ESR and Chromium**. Ubuntu gets Firefox from the
Mozilla repository (Chromium is only available as a snap on Ubuntu; Google
Chrome as a `.deb` package works instead). Snap browsers do not work for the
kiosk.

Under *Einstellungen → Displays*, *Automatisch* uses Firefox, otherwise
Chromium. Browsers that are not installed are marked there. The kiosk runs
with German language and without the translation feature, so no
"translate this page?" offer appears.

Installing Firefox on Ubuntu by hand:

```bash
install -d -m 0755 /etc/apt/keyrings
wget -q https://packages.mozilla.org/apt/repo-signing-key.gpg -O- > /etc/apt/keyrings/packages.mozilla.org.asc
echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" > /etc/apt/sources.list.d/mozilla.list
printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' > /etc/apt/preferences.d/mozilla
apt update && apt install firefox
systemctl restart camera-network-monitor-display
```

### Console and several graphics cards

- **One graphics card** (console and wall display on the same monitor): the
  wall display occupies the monitor **only while a multiviewer is assigned**
  and the display works; otherwise the normal text console stays visible.
  Switch to the console with **Ctrl+Alt+F1** and back to the wall display
  with **Ctrl+Alt+F7**. If the display fails to start three times, the
  service pauses for 15 minutes (or until the next change) instead of taking
  over the console again and again.
- **Several graphics cards** (e.g. on-board VGA as console plus a GPU with
  HDMI/DP outputs): the setup asks which card drives the wall monitors.
  That card gets a seat of its own, so the console on the other card stays
  usable at all times. The web interface only shows the outputs of the
  chosen card. Running the setup again changes the choice.

**Emergency – console not reachable:** in the boot menu (GRUB; on Ubuntu hold
Shift or press Esc while booting), press `e` on the entry, append
` systemd.mask=camera-network-monitor-display.service` to the line starting
with `linux` and boot with Ctrl+X. Then log in and run
`systemctl disable --now camera-network-monitor-display`.

### Behaviour in continuous operation

- Browser crashed → restarted after a short delay (2–60 s).
- Page not connected to CNM for more than 60 s although CNM runs (frozen or
  white page) → the browser is restarted.
- Compositor crashed → everything is set up again.
- CNM restarted or updated → the displays keep running, show *SERVER
  OFFLINE* briefly and reconnect by themselves.
- Monitor unplugged → its display is stopped and started again when the
  monitor is plugged back in.

### Display troubleshooting

```bash
systemctl status camera-network-monitor-display
journalctl -u camera-network-monitor-display -f
```

| Symptom | Cause / action |
|---|---|
| Monitor shows only a black screen with a cursor | The display console is active but cannot show anything (e.g. no browser). Ctrl+Alt+F1 leads to the console; the status is shown under *Einstellungen → Displays*. |
| *Display-Dienst nicht verbunden* | The display service is not set up or not running (`./install.sh --with-display`), or CNM itself is not running. |
| Compositor error "no DRM device" | The server has no graphics output (virtual machine, headless board), or a login manager (gdm, lightdm) occupies the console – disable it. |
| *Kein Browser gefunden* / *Nur Snap-Browser gefunden* | Install Firefox (Debian: `apt install firefox-esr`) or Chromium as a `.deb` package. |
| Browser does not start (sandbox error in the journal) | Pass extra browser options with `systemctl edit camera-network-monitor-display` → `Environment=CNM_DISPLAY_BROWSER_ARGS=...`. Use `--no-sandbox` only as a last resort. |
| Stuttering with many main streams | Enable hardware decoding, e.g. `CNM_DISPLAY_BROWSER_ARGS=--enable-features=VaapiVideoDecodeLinuxGL` (depends on the driver). |
| Chosen resolution not applied | The monitor does not offer this mode; the preferred resolution is used and the settings page shows a note. |

## Access and roles

| Access | Rights |
|---|---|
| without login (also from the server itself) | nothing but the login page; creating the very first administrator only from the server itself |
| user with role *Viewer* | multiviewers, full-size view, playback |
| user with role *Admin* | additionally all settings |
| kiosk link | exactly one multiviewer, live only (no playback, no settings) |
| wall display on the server | like a kiosk link, with its own automatic identity per output |

- Requests that pass through a reverse proxy (`X-Forwarded-For` or
  `Forwarded` header) never count as coming from the server itself.
- Sessions last 12 hours from the last activity, kiosk sessions 30 days;
  both survive restarts and updates.
- After 5 wrong logins, logging in is blocked for 10 minutes per client and
  user name.
- Recorder passwords are stored encrypted and are never sent to a browser.
  Browsers only ever talk to CNM, never directly to a recorder.

## Kiosk links

*Einstellungen → Benutzer → Kiosk-Keys → Key erzeugen* creates a link of the
form

```
http://<server>:8090/viewer?mon=<monitor-id>&key=<key>
```

- The kiosk shows only this multiviewer, full screen, without navigation.
- After a server restart, the kiosk logs in again by itself.
- Playback is blocked for kiosks (also on the server side).
- *Widerrufen* (revoke) immediately ends all sessions and video connections
  of this link; the screen shows *ZUGRIFF GESPERRT*. Other viewers of the
  same cameras are not affected.
- *URL kopieren* copies the link again later. Keys from old installations
  that were stored as a hash only keep working, but their link cannot be
  shown again.

Treat a kiosk link like a password: anyone who has it can watch that
multiviewer.

## Lost administrator access

```bash
systemctl stop camera-network-monitor
echo 'NewPassword123' | runuser -u cnm -- /opt/camera-network-monitor/bin/cnm reset-user \
     --data-dir /var/lib/camera-network-monitor --username admin
systemctl start camera-network-monitor
```

## System settings

*Einstellungen → System*. The defaults suit almost every installation.

| Setting | Default | Notes |
|---|---|---|
| *Systemname* | Camera Network Monitor | shown in the top bar and the browser tab |
| *CNM Listen Host / Port* | all interfaces, port 8090 | takes effect after a service restart |
| *Healthcheck-Intervall* | 30 s | how often each device is checked |
| *Reconnect Minimum / Maximum* | 5 s / 60 s | waiting time between reconnects, growing; **never** after a wrong password |
| *Multiview-Streams warm halten* | on | keeps the sub streams of active multiviewers connected, so tiles appear instantly |
| *Mainstreams warm halten* | off | also keeps main streams connected – faster full-size view, but more recorder connections and bandwidth |
| *WebRTC erlauben* | off | lower latency in some browsers; never used for kiosks. The video then bypasses CNM and cannot be cut off on revocation. |
| *go2rtc API URL* | `http://127.0.0.1:1984` | the media server; reachable only from the server itself |
| *go2rtc selbst verwalten* | on for new installations, unless a go2rtc already runs | CNM starts and supervises the bundled go2rtc |
| *Media Reconcile-Intervall* | 15 s | how often CNM compares the wanted and the actual streams; changes are applied immediately anyway |

## Network search and firewalls

*Netzwerk durchsuchen* (under *Geräte*) asks the local network for devices
via ONVIF, DVRIP, Dahua, Hikvision SADP and Axis SSDP. These searches use
broadcast and multicast and therefore only reach devices in the same
network segment.

- For devices behind a router or firewall, enter an **address range** (a
  single address or a CIDR network up to /22). CNM then asks every address
  directly and also checks the usual TCP ports.
- A firewall on the CNM server must allow the UDP answers to the search
  (ports 3702, 34569, 37810, 37020, 5050, 1900).
- For normal operation, CNM only needs outgoing connections to the device
  ports (typically TCP 80 and 554, plus 34567 for DVRIP or 37777 for the
  native Dahua protocol).

## Troubleshooting

| Symptom | Cause / action |
|---|---|
| Tile shows *STREAM OFFLINE* | The source does not deliver. Check the device status in the settings; the tile recovers by itself. |
| *STREAM OFFLINE* with "Browser kann H.264/H.265 nicht abspielen" | The browser lacks the licensed video codecs (e.g. plain open-source Chromium). Use Chrome, Edge or Firefox. |
| Device connected, but all its streams offline; journal shows `i/o timeout` from go2rtc | The video session is set up, but no video data arrives. Typical for a firewall between CNM and the recorder that inspects RTSP (RTSP helper/ALG, IPS). Disable the helper for this connection or change the RTSP port on the recorder. |
| "Verbindung abgelehnt (… – Port prüfen)" | The port entered for the device is not open. Correct it under *Erweitert* in the device dialog. |
| *Media offline* | CNM runs, the media server does not. Check *Einstellungen → System* and the journal (port 1984 in use?). |
| *SERVER OFFLINE* | The browser lost its connection to CNM. It reconnects by itself. |
| *Media WARN* | Problems with individual streams; a click on the chip shows details. |
| Device *Fehler – Login gesperrt* | Wrong login. CNM deliberately does not retry (to avoid locking the recorder account). Correct the password, or press *Reconnect*. |
| First picture appears with a delay | The camera sends a full frame only every few seconds. Not an error. |
| No playback button | No recordings were found for this camera (column *Aufnahmen* under *Kameras*), the device has no hard disk, or its protocol has no playback. The check runs every 10 minutes and after each reconnect. |
| CNM does not start: "master.key fehlt" | Restore `master.key` from the backup that belongs to `config.json`. |

## Uninstalling

```bash
/opt/camera-network-monitor/uninstall.sh            # keeps the data
/opt/camera-network-monitor/uninstall.sh --purge    # also deletes /var/lib/camera-network-monitor
```

## Trying CNM without hardware

The package contains a device simulator, `bin/cnm-sim`. It provides
simulated recorders for every supported protocol – with test video and
recordings – and answers network searches:

```bash
/opt/camera-network-monitor/bin/cnm-sim --password sim12345
```

The simulated devices listen on the server itself (`127.0.0.1`). Add them
under *Geräte* with the user `admin` (Axis: `root`), the password given
above and these ports:

| Simulated device | Protocol in CNM | Ports (enter under *Erweitert*) |
|---|---|---|
| Recorder, RPC2 | Dahua | HTTP 18008, RTSP 18554 |
| Recorder, RPC2 with current web login | Dahua | HTTP 18009, RTSP 18554 |
| Recorder, native protocol | Dahua | native 18777, RTSP 18554 |
| Recorder, DVRIP | Sofia (DVRIP) | 20051 |
| Recorder, DVRIP (older live transport) | Sofia (DVRIP) | 20052 |
| Recorder, ISAPI | Hikvision | HTTP 18080, RTSP 18564 |
| Recorder, ONVIF Profile S/G | ONVIF | HTTP 18081 |
| Camera, VAPIX with edge storage | Axis | HTTP 18082, RTSP 18567 |
| Recorder, RTSP | Uniview | RTSP 18566 |

`cnm-sim --help` lists all options.
