// Device settings: encoding, names, time, recording and motion detection of
// a connected device – read and written by CNM with the stored credentials.
import api from '../api.js';
import store from '../store.js';
import { h, clear, select, field, checkbox, loadingBox, busy } from '../dom.js';
import { openDialog, confirmDialog, alertDialog, setMessage, toast } from '../dialog.js';

const MODE_LABEL = {
  continuous: 'Daueraufnahme',
  motion: 'Nur bei Bewegung',
  off: 'Keine Aufnahme',
  custom: 'Eigener Zeitplan (am Gerät eingestellt)',
};
const MODE_SHORT = { continuous: 'Dauer', motion: 'Bewegung', off: 'aus', custom: 'eigener Plan' };
const REBOOT_DAYS = [['never', 'nie'], ['daily', 'täglich'], ['sun', 'sonntags'], ['mon', 'montags'], ['tue', 'dienstags'],
  ['wed', 'mittwochs'], ['thu', 'donnerstags'], ['fri', 'freitags'], ['sat', 'samstags']];
const QUALITY = [[1, '1 – niedrigste'], [2, '2'], [3, '3'], [4, '4 – mittel'], [5, '5'], [6, '6 – beste']];

const clone = (o) => JSON.parse(JSON.stringify(o));
const same = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
const fmtGB = (bytes) => `${(bytes / 1e9).toLocaleString('de-DE', { maximumFractionDigits: bytes < 1e11 ? 1 : 0 })} GB`;

export function supportsDeviceConfig(proto) {
  const v = proto && proto.capabilities && proto.capabilities.config;
  return v === 'verified' || v === 'experimental';
}

function streamOf(s) {
  if (!s) return null;
  const { options, ...rest } = s;
  return rest;
}

function resLabel(st) {
  const o = st && st.options && st.options.resolutions.find((r) => r.value === st.resolution);
  return o ? o.label.replace(/ \(aktuell\)$/, '') : (st ? st.resolution : '');
}

export function openDeviceConfig(dev) {
  const msg = h('div', { hidden: true });
  const content = h('div', { class: 'devcfg' }, loadingBox('Einstellungen werden vom Gerät gelesen …'));
  let orig = null;
  let edit = null;
  const dlg = openDialog({
    title: `Geräteeinstellungen – ${dev.label}`,
    size: 'large',
    enterSubmits: false,
    body: h('div', {}, msg, content),
    actions: [
      { label: 'Neu starten …', kind: 'danger', left: true, onClick: reboot },
      { label: 'Schließen' },
      { label: 'Speichern', kind: 'primary', onClick: save },
    ],
  });
  load();

  async function load() {
    try {
      const r = await api.get(`/api/settings/devices/${encodeURIComponent(dev.id)}/config`);
      orig = r.settings;
      edit = clone(orig);
      render();
    } catch (e) {
      clear(content);
      setMessage(msg, (e && e.message) || 'Die Einstellungen konnten nicht gelesen werden.');
    }
  }

  // ---- rendering --------------------------------------------------------------------------------

  let estimateEl = null;

  function updateEstimate() {
    if (!estimateEl) return;
    const total = (edit.disks || []).reduce((s, d) => s + (d.totalBytes || 0), 0);
    const mbps = edit.channels.reduce((s, c) => s + (c.main && c.main.enabled && (!c.record || c.record.mode !== 'off') ? c.main.bitrate / 1000 : 0), 0);
    if (!total || !mbps) { estimateEl.textContent = ''; return; }
    const days = total / (mbps * 1e6 / 8 * 86400);
    const est = (edit.disks || []).some((d) => d.estimated);
    estimateEl.textContent = `Geschätzte Speicherdauer: ≈ ${days.toLocaleString('de-DE', { maximumFractionDigits: days < 10 ? 1 : 0 })} Tage `
      + `(Summe der Hauptstreams ${mbps.toLocaleString('de-DE', { maximumFractionDigits: 1 })} Mbit/s, bei voller Ausnutzung; mit VBR und Bewegungsaufnahme meist länger`
      + `${est ? '; angenommen, die Festplatte ist voll' : ''})`;
  }

  // last recorder check (hourly) and a check on demand
  function monitorBox() {
    const box = h('div', { class: 'cfg-monitor' });
    const show = (mon) => {
      clear(box);
      const btn = h('button', { class: 'btn small', type: 'button' }, 'Jetzt prüfen');
      btn.addEventListener('click', () => busy(btn, async () => {
        try {
          const r = await api.post(`/api/settings/devices/${encodeURIComponent(dev.id)}/check`, {});
          show(r);
        } catch (e) { setMessage(msg, e.message); }
      }));
      const when = mon && mon.checkedAt ? `zuletzt geprüft ${new Date(mon.checkedAt).toLocaleString('de-DE')}` : 'noch nicht geprüft';
      const problems = (mon && mon.problems) || [];
      box.append(h('div', { class: problems.length ? 'msg error' : 'msg' },
        h('div', { class: 'row' },
          h('strong', {}, problems.length ? 'Überwachung: Probleme gefunden' : (mon ? 'Überwachung: keine Probleme' : 'Überwachung')),
          h('span', { class: 'faint' }, ` · ${when}`), h('span', { class: 'spacer' }), btn),
        problems.length ? h('ul', {}, problems.map((p) => h('li', {}, p))) : null));
    };
    const st = store.devices.get(dev.id) || dev.status;
    show(st && st.monitor);
    return box;
  }

  function render() {
    clear(content);
    content.appendChild(monitorBox());
    for (const n of edit.notes || []) content.appendChild(h('div', { class: 'msg', style: { marginBottom: '8px' } }, n));
    content.append(storageSection(), timeSection());
    if (edit.system || edit.network) content.append(systemSection());
    content.append(channelsSection());
    if (edit.users) content.append(usersSection());
    if (edit.log) content.append(logSection());
    updateEstimate();
  }

  // formatting deletes all recordings of the partition: the admin types the
  // device name to confirm (checked by the server as well)
  function formatDisk(d) {
    const name = dev.label || '';
    const input = h('input', { type: 'text', autocomplete: 'off', spellcheck: 'false' });
    const m = h('div', { class: 'msg', hidden: true });
    const body = h('div', {},
      h('div', { class: 'msg error' }, `Alle Aufnahmen auf „${d.name || 'Festplatte'}“ werden unwiderruflich gelöscht.`),
      h('p', {}, 'Das Formatieren dauert einige Minuten. Aufnahme und Live-Bild können so lange ausfallen; manche Rekorder starten danach neu. '
        + 'Ist die Festplatte selbst defekt, lehnt der Rekorder das Formatieren ab oder der Fehler bleibt – dann muss sie getauscht werden.'),
      field(`Zum Bestätigen den Gerätenamen eintippen: ${name}`, input), m);
    const dlg = openDialog({
      title: 'Festplatte formatieren',
      body,
      enterSubmits: false,
      actions: [
        { label: 'Abbrechen' },
        {
          label: 'Formatieren',
          kind: 'danger',
          onClick: async () => {
            if (input.value.trim() !== name.trim()) { setMessage(m, 'Der Name stimmt nicht.'); return false; }
            setMessage(m, 'Formatiert … das kann einige Minuten dauern.', 'warn');
            try {
              await api.post(`/api/settings/devices/${encodeURIComponent(dev.id)}/format`, { disk: d.disk, part: d.part, confirm: input.value });
            } catch (e) { setMessage(m, e.message); return false; }
            toast('Festplatte formatiert');
            await load();
            return true;
          },
        },
      ],
    });
    const go = dlg.buttons[1];
    go.disabled = true;
    input.addEventListener('input', () => { go.disabled = input.value.trim() !== name.trim(); });
  }

  // device name, video standard, maintenance restart; the network is shown only
  function systemSection() {
    const box = h('div', {}, h('div', { class: 'section-title' }, 'System'));
    const sy = edit.system;
    if (sy) {
      const grid = h('div', { class: 'form-grid' });
      if (typeof sy.name === 'string') {
        const name = h('input', { type: 'text', value: sy.name, maxlength: '63' });
        name.addEventListener('input', () => { sy.name = name.value; });
        grid.appendChild(field('Gerätename im Rekorder', name));
      }
      if (sy.videoStandard) {
        const vs = select([{ value: 'PAL', label: 'PAL (Europa, 25 Bilder/s)' }, { value: 'NTSC', label: 'NTSC (30 Bilder/s)' }], sy.videoStandard);
        vs.addEventListener('change', () => { sy.videoStandard = vs.value; });
        grid.appendChild(field('Videonorm', vs, 'Muss zu den Kameras passen. Eine Änderung startet den Rekorder neu.'));
      }
      if (sy.autoReboot) {
        const r = sy.autoReboot;
        const day = select(REBOOT_DAYS.map(([value, label]) => ({ value, label })), r.day);
        const hour = select([...Array(24).keys()].map((x) => ({ value: String(x), label: `${String(x).padStart(2, '0')}:00 Uhr` })), String(r.hour));
        hour.disabled = r.day === 'never';
        day.addEventListener('change', () => { r.day = day.value; hour.disabled = r.day === 'never'; });
        hour.addEventListener('change', () => { r.hour = parseInt(hour.value, 10); });
        grid.appendChild(field('Automatischer Wartungs-Neustart', h('div', { class: 'row' }, day, hour),
          'Bei leerer Uhrenbatterie kann der Rekorder nach einem Neustart die Uhrzeit verlieren.'));
      }
      box.appendChild(grid);
    }
    const n = edit.network;
    if (n) {
      const rows = [['IP-Adresse', n.ip], ['Subnetzmaske', n.mask], ['Gateway', n.gateway], ['DNS', (n.dns || []).join(', ')],
        ['DHCP', typeof n.dhcp === 'boolean' ? (n.dhcp ? 'an' : 'aus') : ''], ['MAC-Adresse', n.mac], ['Hostname', n.hostname],
        ['Ports', Object.entries(n.ports || {}).map(([k, v]) => `${k} ${v}`).join(' · ')]].filter(([, v]) => v);
      box.append(h('div', { class: 'cfg-subtitle' }, 'Netzwerk – nur Anzeige: Änderungen aus der Ferne könnten den Rekorder unerreichbar machen'),
        h('table', { class: 'cfg-net' }, h('tbody', {}, rows.map(([k, v]) => h('tr', {}, h('td', { class: 'faint' }, k), h('td', { class: 'mono' }, v))))));
    }
    return box;
  }

  // the recorder's user accounts; passwords are only ever set, never shown
  function usersSection() {
    const box = h('div', {}, h('div', { class: 'section-title' }, 'Benutzer des Rekorders'));
    const list = h('div', { class: 'cfg-users' });
    const btn = h('button', { class: 'btn small', type: 'button' }, 'Benutzer anzeigen');
    btn.addEventListener('click', () => busy(btn, async () => {
      clear(list);
      try {
        const r = await api.get(`/api/settings/devices/${encodeURIComponent(dev.id)}/users`);
        list.appendChild(h('table', { class: 'cfg-net' }, h('tbody', {}, (r.users || []).map((u) => h('tr', {},
          h('td', {}, h('strong', {}, u.name)),
          h('td', { class: 'faint' }, u.group || ''),
          h('td', {}, u.self ? h('span', { class: 'chip ok' }, 'Zugang von CNM') : ''),
          h('td', {}, h('button', { class: 'btn small', type: 'button', onclick: () => passwordDialog(u) }, 'Passwort ändern …')))))));
      } catch (e) { list.appendChild(h('div', { class: 'msg error' }, e.message)); }
    }));
    box.append(btn, list);
    return box;
  }

  function passwordDialog(user) {
    const oldIn = h('input', { type: 'password', autocomplete: 'off' });
    const newIn = h('input', { type: 'password', autocomplete: 'new-password', maxlength: '32' });
    const rep = h('input', { type: 'password', autocomplete: 'new-password', maxlength: '32' });
    const m = h('div', { class: 'msg', hidden: true });
    const body = h('div', {},
      h('div', { class: 'msg' }, user.self
        ? 'Mit diesem Zugang meldet sich CNM am Rekorder an. CNM prüft das neue Passwort mit einer zweiten Anmeldung und übernimmt es danach automatisch. Klappt diese Anmeldung nicht, stellt CNM das alte Passwort sofort wieder her.'
        : 'CNM prüft das neue Passwort mit einer zweiten Anmeldung. Klappt sie nicht, stellt CNM das alte Passwort sofort wieder her.'),
      user.self ? null : field('Bisheriges Passwort', oldIn),
      field('Neues Passwort', newIn, '1–32 Zeichen; manche Rekorder verlangen Buchstaben und Ziffern.'),
      field('Neues Passwort wiederholen', rep),
      h('p', { class: 'faint' }, 'Apps und Programme, die sich mit dem alten Passwort anmelden, müssen danach angepasst werden.'),
      m);
    openDialog({
      title: `Passwort ändern – ${user.name}`,
      body,
      enterSubmits: false,
      actions: [
        { label: 'Abbrechen' },
        {
          label: 'Passwort ändern',
          kind: 'primary',
          onClick: async () => {
            if (!newIn.value || newIn.value !== rep.value) { setMessage(m, 'Die beiden neuen Passwörter stimmen nicht überein.'); return false; }
            if (!user.self && !oldIn.value) { setMessage(m, 'Bitte das bisherige Passwort angeben.'); return false; }
            setMessage(m, 'Ändert und prüft …', 'warn');
            try {
              await api.post(`/api/settings/devices/${encodeURIComponent(dev.id)}/password`, { user: user.name, oldPassword: oldIn.value, newPassword: newIn.value });
            } catch (e) { setMessage(m, e.message); return false; }
            toast(user.self ? 'Passwort geändert – CNM meldet sich ab jetzt mit dem neuen Passwort an' : 'Passwort geändert');
            return true;
          },
        },
      ],
    });
  }

  // the device's own event log (read only)
  function logSection() {
    const box = h('div', {}, h('div', { class: 'section-title' }, 'Protokoll des Geräts'));
    const range = select([{ value: '30', label: 'letzte 30 Tage' }, { value: '365', label: 'letztes Jahr' }, { value: '3650', label: 'alles' }], '30');
    const kind = select([{ value: 'all', label: 'alle Ereignisse' }, { value: 'storage', label: 'Festplatte' }, { value: 'system', label: 'System (Neustart, Uhrzeit)' },
      { value: 'record', label: 'Aufnahme' }, { value: 'config', label: 'Einstellungen' }, { value: 'alarm', label: 'Alarme' }, { value: 'account', label: 'Benutzer' }], 'all');
    const btn = h('button', { class: 'btn small', type: 'button' }, 'Protokoll laden');
    const list = h('div', { class: 'cfg-log' });
    btn.addEventListener('click', () => busy(btn, async () => {
      clear(list);
      try {
        const r = await api.get(`/api/settings/devices/${encodeURIComponent(dev.id)}/log?days=${range.value}&kind=${kind.value}`);
        const entries = r.entries || [];
        if (!entries.length) { list.appendChild(h('div', { class: 'faint' }, 'Keine Einträge in diesem Zeitraum.')); return; }
        list.appendChild(h('div', { class: 'faint' }, `${entries.length} Einträge, neueste zuerst (Uhrzeit des Geräts)`));
        list.appendChild(h('table', { class: 'table cfg-log-table' },
          h('thead', {}, h('tr', {}, h('th', {}, 'Zeit'), h('th', {}, 'Ereignis'), h('th', {}, 'Daten'), h('th', {}, 'Benutzer'))),
          h('tbody', {}, entries.map((e) => h('tr', { class: /storage|disk|hdd/i.test(e.type) ? 'warn-row' : '' },
            h('td', {}, e.time), h('td', {}, e.type), h('td', {}, e.data || ''), h('td', {}, e.user || ''))))));
      } catch (e) { list.appendChild(h('div', { class: 'msg error' }, e.message)); }
    }));
    box.append(h('div', { class: 'row' }, kind, range, btn), list);
    return box;
  }

  function storageSection() {
    const box = h('div', {}, h('div', { class: 'section-title' }, 'Festplatte'));
    if (edit.disks === null || edit.disks === undefined) {
      box.appendChild(h('div', { class: 'faint' }, 'Das Gerät meldet keine Angaben zu seiner Festplatte.'));
    } else if (!edit.disks.length) {
      box.appendChild(h('div', { class: 'msg warn' }, 'Keine Festplatte erkannt – das Gerät nimmt nichts auf.'));
    } else {
      for (const d of edit.disks) {
        if (d.estimated) {
          box.appendChild(h('div', { class: 'cfg-disk' },
            h('strong', {}, 'Aufnahmen'), ' ',
            h('span', {}, `≈ ${fmtGB(d.totalBytes)} belegt`),
            d.oldest && d.newest ? h('span', { class: 'faint' }, ` · ${d.oldest} bis ${d.newest}`) : null,
            h('div', { class: 'faint' }, 'Die Größe der Festplatte meldet das Gerät nicht. CNM zählt die Aufnahmen zusammen – bei voller Festplatte, die überschrieben wird, entspricht das etwa ihrer Größe.')));
          continue;
        }
        const fmtBtn = edit.canFormat ? h('button', { class: 'btn small danger', type: 'button', onclick: () => formatDisk(d) }, 'Formatieren …') : null;
        box.appendChild(h('div', { class: 'cfg-disk' },
          fmtBtn, fmtBtn ? ' ' : null,
          h('strong', {}, d.name || 'Festplatte'), ' ',
          h('span', {}, `${fmtGB(d.totalBytes)}, frei ${fmtGB(d.freeBytes)}`),
          d.oldest && d.newest ? h('span', { class: 'faint' }, ` · Aufnahmen ${d.oldest} bis ${d.newest}`)
            : d.oldest ? h('span', { class: 'faint' }, ` · älteste Aufnahme ${d.oldest}`) : null,
          d.state ? h('span', { class: 'faint' }, ` · ${d.state}`) : null));
      }
      // the newest recording on any disk, against the device clock
      const parse = (s) => Date.parse(String(s).replace(' ', 'T'));
      const newest = Math.max(...edit.disks.map((d) => (d.newest ? parse(d.newest) : NaN)).filter((t) => !Number.isNaN(t)));
      const devNow = edit.time && edit.time.deviceTime ? parse(edit.time.deviceTime) : Date.now();
      const records = edit.channels.some((c) => c.record && c.record.mode !== 'off');
      if (records && Number.isFinite(newest) && devNow - newest > 6 * 3600 * 1000) {
        const days = Math.floor((devNow - newest) / 86400000);
        const last = edit.disks.find((d) => d.newest && parse(d.newest) === newest).newest;
        box.appendChild(h('div', { class: 'msg error' },
          `Die neueste Aufnahme ist vom ${last}${days >= 1 ? ` – das Gerät nimmt seit ${days} ${days === 1 ? 'Tag' : 'Tagen'} nicht mehr auf` : ' – das Gerät nimmt gerade nicht auf'}. `
          + 'Festplatte im Menü des Rekorders prüfen (Zustand, ggf. formatieren oder tauschen).'));
      }
    }
    if (typeof edit.overwrite === 'boolean') {
      const cb = checkbox('Bei voller Festplatte die ältesten Aufnahmen überschreiben', edit.overwrite);
      cb.input.addEventListener('change', () => { edit.overwrite = cb.input.checked; });
      box.appendChild(cb);
    }
    estimateEl = h('div', { class: 'faint cfg-estimate' });
    box.appendChild(estimateEl);
    const a = edit.diskAlarms;
    if (a) {
      const alarms = h('div', { class: 'cfg-alarms' }, h('div', { class: 'cfg-subtitle' }, 'Alarm des Rekorders bei Festplattenproblemen'));
      if (a.noDisk) alarms.appendChild(eventEditor('Keine Festplatte', a.noDisk));
      if (a.failure) alarms.appendChild(eventEditor('Festplattenfehler', a.failure));
      if (a.lowSpace) {
        const row = eventEditor('Wenig Speicherplatz', a.lowSpace);
        const pct = h('input', { type: 'number', min: '1', max: '99', value: String(a.lowSpacePercent || 10), style: { width: '70px' } });
        pct.addEventListener('input', () => { a.lowSpacePercent = parseInt(pct.value, 10) || 0; });
        row.append(h('span', { class: 'faint' }, 'unter'), pct, h('span', { class: 'faint' }, '% frei'));
        alarms.appendChild(row);
        alarms.appendChild(h('div', { class: 'hint' }, 'Mit „Überschreiben“ ist die Festplatte immer voll – dann „Wenig Speicherplatz“ ausgeschaltet lassen, sonst meldet der Rekorder dauernd Alarm.'));
      }
      box.appendChild(alarms);
    }
    return box;
  }

  // an event of the recorder: on/off, buzzer, recording (where offered)
  function eventEditor(label, ev) {
    const row = h('div', { class: 'cfg-event' });
    const en = checkbox(label, ev.enabled);
    en.input.addEventListener('change', () => { ev.enabled = en.input.checked; });
    row.appendChild(en);
    if (typeof ev.beep === 'boolean') {
      const b = checkbox('Summer', ev.beep);
      b.input.addEventListener('change', () => { ev.beep = b.input.checked; });
      row.appendChild(b);
    }
    if (typeof ev.record === 'boolean') {
      const r = checkbox('Aufnahme', ev.record);
      r.input.addEventListener('change', () => { ev.record = r.input.checked; });
      row.appendChild(r);
    }
    return row;
  }

  function alarmEditor(c) {
    const box = h('div', { class: 'cfg-stream' }, h('h4', {}, 'Alarme'));
    if (c.videoLoss) box.appendChild(eventEditor('Videoverlust (Kamerasignal fehlt)', c.videoLoss));
    if (c.tamper) box.appendChild(eventEditor('Sabotage (Kamera abgedeckt)', c.tamper));
    box.appendChild(h('div', { class: 'hint' }, '„Aufnahme“ nimmt den Kanal beim Ereignis auf, „Summer“ piept am Rekorder.'));
    return box;
  }

  function timeSection() {
    const box = h('div', {}, h('div', { class: 'section-title' }, 'Uhrzeit'));
    const t = edit.time;
    if (!t) {
      box.appendChild(h('div', { class: 'faint' }, 'Das Gerät meldet seine Uhrzeit nicht.'));
      return box;
    }
    const off = t.offsetSeconds;
    const diff = Math.abs(off) <= 2 ? 'stimmt mit dem Server überein'
      : `${Math.abs(off) >= 120 ? `${Math.round(Math.abs(off) / 60)} min` : `${Math.abs(off)} s`} ${off > 0 ? 'vor' : 'nach'}`;
    const syncBtn = h('button', { class: 'btn small', type: 'button' }, 'Uhr jetzt auf Serverzeit stellen');
    syncBtn.addEventListener('click', () => busy(syncBtn, async () => {
      try {
        await api.post(`/api/settings/devices/${encodeURIComponent(dev.id)}/time`, {});
        toast('Uhr des Geräts gestellt');
        await load();
      } catch (e) { setMessage(msg, e.message); }
    }));
    box.appendChild(h('div', { class: 'row cfg-time' },
      h('span', {}, `Gerätezeit ${t.deviceTime} – `, h('span', { class: Math.abs(off) > 60 ? 'warn-text' : '' }, diff)),
      h('span', { class: 'spacer' }), syncBtn));
    if (t.ntp) {
      const n = t.ntp;
      const en = checkbox('Uhr automatisch über einen Zeitserver (NTP) stellen', n.enabled);
      const srv = h('input', { type: 'text', value: n.server || '', maxlength: '128' });
      const port = h('input', { type: 'number', min: '1', max: '65535', value: String(n.port || 123) });
      const iv = h('input', { type: 'number', min: '1', max: '100000', value: String(n.intervalMin || 60) });
      en.input.addEventListener('change', () => { n.enabled = en.input.checked; });
      srv.addEventListener('input', () => { n.server = srv.value.trim(); });
      port.addEventListener('input', () => { n.port = parseInt(port.value, 10) || 0; });
      iv.addEventListener('input', () => { n.intervalMin = parseInt(iv.value, 10) || 0; });
      box.append(en, h('div', { class: 'form-grid' }, field('Zeitserver', srv), field('Port', port), field('Abgleich alle (Minuten)', iv)));
    }
    return box;
  }

  function channelsSection() {
    const box = h('div', {}, h('div', { class: 'row', style: { alignItems: 'center' } },
      h('div', { class: 'section-title', style: { flex: '1' } }, `Kanäle (${edit.channels.length})`),
      edit.channels.length > 1 && edit.channels[0].main ? h('button', { class: 'btn small', type: 'button', onclick: copyFirst }, 'Encode von Kanal 1 auf alle übertragen') : null));
    for (const c of edit.channels) box.appendChild(channelCard(c));
    return box;
  }

  function copyFirst() {
    const first = edit.channels[0];
    for (const c of edit.channels.slice(1)) {
      for (const k of ['main', 'sub']) {
        if (!first[k] || !c[k]) continue;
        const src = streamOf(first[k]);
        const ok = c[k].options.resolutions.some((r) => r.value === src.resolution);
        Object.assign(c[k], { ...src, resolution: ok ? src.resolution : c[k].resolution, codec: c[k].options.codecs.includes(src.codec) ? src.codec : c[k].codec });
      }
    }
    render();
    toast('Encode-Einstellungen von Kanal 1 übernommen – noch nicht gespeichert');
  }

  function channelCard(c) {
    const summary = h('span', { class: 'faint' });
    const refresh = () => {
      const parts = [];
      if (c.main) parts.push(`${resLabel(c.main)}, ${c.main.fps} B/s, ${c.main.control} ${c.main.bitrate} kbit/s${c.main.enabled ? '' : ' (aus)'}`);
      if (c.record) parts.push(`Aufnahme: ${MODE_SHORT[c.record.mode] || c.record.mode}${c.record.from ? ` ${c.record.from}–${c.record.to}` : ''}`);
      summary.textContent = parts.join(' · ');
      updateEstimate();
    };
    const title = h('strong', {}, `Kanal ${c.channel + 1}${typeof c.name === 'string' && c.name ? ` – ${c.name}` : ''}`);
    const body = h('div', { class: 'cfg-ch-body' });
    if (typeof c.name === 'string') {
      const name = h('input', { type: 'text', value: c.name, maxlength: '63' });
      name.addEventListener('input', () => { c.name = name.value; title.textContent = `Kanal ${c.channel + 1}${c.name ? ` – ${c.name}` : ''}`; });
      body.appendChild(h('div', { class: 'form-grid' }, field('Name im Rekorder', name)));
    }
    const streams = h('div', { class: 'cfg-streams' });
    if (c.main) streams.appendChild(streamEditor(c.main, 'Hauptstream (Aufnahme, Großansicht)', refresh));
    if (c.sub) streams.appendChild(streamEditor(c.sub, 'Substream (Multiviewer-Kacheln)', refresh));
    if (streams.childNodes.length) body.appendChild(streams);
    if (c.record || c.motion) body.appendChild(h('div', { class: 'cfg-streams' }, recordEditor(c, refresh), motionEditor(c)));
    if (c.videoLoss || c.tamper) body.appendChild(h('div', { class: 'cfg-streams' }, alarmEditor(c)));
    refresh();
    return h('details', { class: 'cfg-ch' }, h('summary', {}, title, summary), body);
  }

  function streamEditor(st, label, onChange) {
    const o = st.options || { codecs: [st.codec], resolutions: [{ value: st.resolution, label: st.resolution }], fpsMax: 30, bitrateMin: 16, bitrateMax: 16384, gopUnit: 'frames', gopMax: 250 };
    const num = (v, min, max) => h('input', { type: 'number', min: String(min), max: String(max), value: String(v) });
    const en = checkbox('aktiv', st.enabled);
    const codec = select(o.codecs.map((x) => ({ value: x, label: x })), st.codec);
    const res = select(o.resolutions, st.resolution);
    const fps = num(st.fps, 1, o.fpsMax);
    const ctl = select([{ value: 'VBR', label: 'VBR – variabel' }, { value: 'CBR', label: 'CBR – fest' }], st.control);
    const br = num(st.bitrate, o.bitrateMin, o.bitrateMax);
    const q = select(QUALITY.map(([v, l]) => ({ value: String(v), label: l })), String(st.quality));
    const gop = num(st.gop, 1, o.gopMax);
    const sync = () => { q.disabled = st.control !== 'VBR'; };
    const bind = (el, fn) => el.addEventListener(el.tagName === 'SELECT' ? 'change' : 'input', () => { fn(); sync(); onChange(); });
    bind(en.input, () => { st.enabled = en.input.checked; });
    bind(codec, () => { st.codec = codec.value; });
    bind(res, () => { st.resolution = res.value; });
    bind(fps, () => { st.fps = parseInt(fps.value, 10) || 0; });
    bind(ctl, () => { st.control = ctl.value; });
    bind(br, () => { st.bitrate = parseInt(br.value, 10) || 0; });
    bind(q, () => { st.quality = parseInt(q.value, 10); });
    bind(gop, () => { st.gop = parseInt(gop.value, 10) || 0; });
    sync();
    return h('div', { class: 'cfg-stream' },
      h('div', { class: 'row', style: { alignItems: 'center' } }, h('h4', { style: { flex: '1' } }, label), en),
      h('div', { class: 'form-grid' },
        field('Codec', codec),
        field('Auflösung', res),
        field('Bilder pro Sekunde', fps, `max. ${o.fpsMax}`),
        field('Bitratentyp', ctl),
        field('Bitrate (kbit/s)', br, `${o.bitrateMin}–${o.bitrateMax}, bei VBR die Obergrenze`),
        field('Qualität (VBR)', q),
        field(o.gopUnit === 'seconds' ? 'Schlüsselbild alle … Sekunden' : 'Schlüsselbild alle … Bilder', gop,
          o.gopUnit === 'seconds' ? 'empfohlen 1–2' : 'empfohlen 1–2 × Bildrate')));
  }

  function recordEditor(c, onChange) {
    const box = h('div', { class: 'cfg-stream' }, h('h4', {}, 'Aufnahme'));
    if (!c.record) { box.appendChild(h('div', { class: 'faint' }, 'Nicht einstellbar.')); return box; }
    const r = c.record;
    const modes = ['continuous', 'motion', 'off'].map((m) => ({ value: m, label: MODE_LABEL[m] }));
    if (r.mode === 'custom') modes.unshift({ value: 'custom', label: MODE_LABEL.custom });
    const mode = select(modes, r.mode);
    const win = checkbox('nur in diesem Zeitfenster (täglich)', !!r.from);
    const from = h('input', { type: 'time', value: r.from || '18:00' });
    const to = h('input', { type: 'time', value: r.to || '06:00' });
    const winRow = h('div', { class: 'form-grid' }, field('von', from), field('bis', to, 'über Mitternacht möglich, z. B. 18:00–06:00'));
    const sync = () => {
      const active = r.mode === 'continuous' || r.mode === 'motion';
      win.hidden = !active;
      winRow.hidden = !active || !win.input.checked;
      if (active && win.input.checked) { r.from = from.value; r.to = to.value; } else if (r.mode !== 'custom') { r.from = ''; r.to = ''; }
      onChange();
    };
    mode.addEventListener('change', () => {
      r.mode = mode.value;
      if (r.mode === 'motion' && c.motion) c.motion.enabled = true;
      sync();
    });
    win.input.addEventListener('change', sync);
    from.addEventListener('change', sync);
    to.addEventListener('change', sync);
    box.append(field('Modus', mode), win, winRow);
    if (r.mode === 'custom') box.appendChild(h('div', { class: 'hint' }, 'Am Gerät ist ein Zeitplan eingestellt, den CNM nicht vereinfacht darstellen kann. Er bleibt, solange hier nichts anderes gewählt wird.'));
    win.hidden = !(r.mode === 'continuous' || r.mode === 'motion');
    winRow.hidden = win.hidden || !win.input.checked;
    return box;
  }

  function motionEditor(c) {
    const box = h('div', { class: 'cfg-stream' }, h('h4', {}, 'Bewegungserkennung'));
    if (!c.motion) { box.appendChild(h('div', { class: 'faint' }, 'Nicht einstellbar.')); return box; }
    const m = c.motion;
    const en = checkbox('Bewegungserkennung aktiv', m.enabled);
    const min = m.sensitivityMax === 100 ? 0 : 1;
    const range = h('input', { type: 'range', min: String(min), max: String(m.sensitivityMax), step: '1', value: String(m.sensitivity) });
    const val = h('span', { class: 'mono' }, `${m.sensitivity} / ${m.sensitivityMax}`);
    en.input.addEventListener('change', () => { m.enabled = en.input.checked; });
    range.addEventListener('input', () => { m.sensitivity = parseInt(range.value, 10); val.textContent = `${m.sensitivity} / ${m.sensitivityMax}`; });
    box.append(en, field('Empfindlichkeit', h('div', { class: 'row', style: { alignItems: 'center', gap: '10px' } }, range, val)));
    if (typeof m.beep === 'boolean') {
      const b = checkbox('Summer bei Bewegung', m.beep);
      b.input.addEventListener('change', () => { m.beep = b.input.checked; });
      box.append(b);
    }
    if (c.record && c.record.mode === 'motion') box.appendChild(h('div', { class: 'hint' }, 'Für „Nur bei Bewegung“ schaltet CNM die Bewegungserkennung und ihre Aufnahme-Verknüpfung automatisch ein.'));
    return box;
  }

  // ---- actions ----------------------------------------------------------------------------------

  // values outside what the device offers (only changed streams)
  function optionProblems() {
    const out = [];
    edit.channels.forEach((c, i) => {
      for (const [k, label] of [['main', 'Hauptstream'], ['sub', 'Substream']]) {
        const s = c[k];
        if (!s || !s.options || same(streamOf(s), streamOf(orig.channels[i][k]))) continue;
        const o = s.options;
        const where = `Kanal ${c.channel + 1} ${label}`;
        const unit = o.gopUnit === 'seconds' ? 'Sekunden' : 'Bilder';
        if (!(s.fps >= 1 && s.fps <= o.fpsMax)) out.push(`${where}: Bilder pro Sekunde 1–${o.fpsMax} (eingetragen: ${s.fps})`);
        if (!(s.gop >= 1 && s.gop <= o.gopMax)) out.push(`${where}: Schlüsselbild alle 1–${o.gopMax} ${unit} (eingetragen: ${s.gop})`);
        if (!(s.bitrate >= o.bitrateMin && s.bitrate <= o.bitrateMax)) out.push(`${where}: Bitrate ${o.bitrateMin}–${o.bitrateMax} kbit/s (eingetragen: ${s.bitrate})`);
      }
    });
    return out;
  }

  function buildChange() {
    const change = { channels: [] };
    const what = [];
    edit.channels.forEach((c, i) => {
      const o = orig.channels[i];
      const out = { channel: c.channel };
      const parts = [];
      if (typeof c.name === 'string' && c.name !== o.name) { out.name = c.name.trim(); parts.push('Name'); }
      if (c.main && !same(streamOf(c.main), streamOf(o.main))) { out.main = streamOf(c.main); parts.push('Hauptstream'); }
      if (c.sub && !same(streamOf(c.sub), streamOf(o.sub))) { out.sub = streamOf(c.sub); parts.push('Substream'); }
      const rec = (r) => r && { mode: r.mode, from: r.from || '', to: r.to || '' };
      if (c.record && c.record.mode !== 'custom' && !same(rec(c.record), rec(o.record))) { out.record = { mode: c.record.mode, from: c.record.from || '', to: c.record.to || '' }; parts.push('Aufnahme'); }
      if (c.motion && !same(c.motion, o.motion)) { out.motion = c.motion; parts.push('Bewegung'); }
      if (c.videoLoss && !same(c.videoLoss, o.videoLoss)) { out.videoLoss = c.videoLoss; parts.push('Videoverlust'); }
      if (c.tamper && !same(c.tamper, o.tamper)) { out.tamper = c.tamper; parts.push('Sabotage'); }
      if (parts.length) { change.channels.push(out); what.push(`Kanal ${c.channel + 1}: ${parts.join(', ')}`); }
    });
    if (edit.time && edit.time.ntp && !same(edit.time.ntp, orig.time && orig.time.ntp)) { change.ntp = edit.time.ntp; what.push('Zeitserver'); }
    if (typeof edit.overwrite === 'boolean' && edit.overwrite !== orig.overwrite) { change.overwrite = edit.overwrite; what.push('Überschreiben'); }
    if (edit.diskAlarms && !same(edit.diskAlarms, orig.diskAlarms)) { change.diskAlarms = edit.diskAlarms; what.push('Festplatten-Alarme'); }
    if (edit.system && orig.system) {
      const s = {};
      const e = edit.system;
      const o = orig.system;
      if (typeof e.name === 'string' && e.name !== o.name) { s.name = e.name.trim(); what.push('Gerätename'); }
      if (e.autoReboot && !same(e.autoReboot, o.autoReboot)) { s.autoReboot = e.autoReboot; what.push('Wartungs-Neustart'); }
      if (e.videoStandard && e.videoStandard !== o.videoStandard) { s.videoStandard = e.videoStandard; what.push(`Videonorm ${e.videoStandard} – der Rekorder startet dafür neu`); }
      if (Object.keys(s).length) change.system = s;
    }
    return { change, what };
  }

  async function save() {
    if (!edit) return false;
    setMessage(msg, '');
    const { change, what } = buildChange();
    if (!what.length) { toast('Keine Änderungen'); return false; }
    // hints only: the device decides what it accepts
    const hints = optionProblems();
    if (!await confirmDialog('Am Gerät speichern', h('div', {},
      h('p', {}, 'Folgendes wird im Rekorder geändert:'),
      h('ul', {}, what.map((w) => h('li', {}, w))),
      hints.length ? h('div', { class: 'msg warn' }, h('div', {}, 'Hinweis – außerhalb dessen, was CNM für dieses Gerät kennt:'), h('ul', {}, hints.map((x) => h('li', {}, x)))) : null,
      h('p', { class: 'faint' }, 'Live-Bild und Aufnahme der betroffenen Kanäle können kurz unterbrochen werden.')),
    { confirmLabel: hints.length ? 'Trotzdem senden' : 'Speichern' })) return false;
    try {
      const r = await api.put(`/api/settings/devices/${encodeURIComponent(dev.id)}/config`, change);
      orig = r.settings;
      edit = clone(orig);
      render();
      if (r.restart) setMessage(msg, 'Gespeichert. Das Gerät verlangt einen Neustart, damit alle Änderungen wirksam werden.', 'warn');
      if (r.failures && r.failures.length) {
        setMessage(msg, `Nicht gespeichert: ${r.failures.join(' · ')}`);
        await alertDialog('Nicht alles gespeichert', h('div', {},
          h('p', {}, 'Diese Kanäle hat das Gerät abgelehnt – die übrigen wurden gespeichert:'),
          h('ul', {}, r.failures.map((w) => h('li', {}, w))),
          r.warnings && r.warnings.length ? h('div', {}, h('p', {}, 'Außerdem:'), h('ul', {}, r.warnings.map((w) => h('li', {}, w)))) : null), 'warn');
      } else if (r.warnings && r.warnings.length) {
        await alertDialog('Nicht alles übernommen', h('div', {},
          h('p', {}, 'Das Gerät hat einige Werte anders übernommen:'),
          h('ul', {}, r.warnings.map((w) => h('li', {}, w)))), 'warn');
      } else {
        toast('Einstellungen am Gerät gespeichert');
      }
    } catch (e) {
      setMessage(msg, (e && e.message) || 'Speichern fehlgeschlagen');
    }
    return false;
  }

  async function reboot() {
    if (!await confirmDialog('Gerät neu starten', `„${dev.label}“ wirklich neu starten? Alle Kameras des Geräts sind dann für ein bis zwei Minuten offline, laufende Aufnahmen werden unterbrochen.`, { confirmLabel: 'Neu starten', danger: true })) return false;
    try {
      await api.post(`/api/settings/devices/${encodeURIComponent(dev.id)}/reboot`, {});
      toast(`${dev.label} startet neu`);
      return undefined; // close the dialog
    } catch (e) {
      setMessage(msg, (e && e.message) || 'Neustart fehlgeschlagen');
      return false;
    }
  }
  return dlg;
}
