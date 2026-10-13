// Devices (camera providers): DVR/NVR, IP cameras, stream sources.
import api from '../api.js';
import store from '../store.js';
import { h, clear, select, field, fmtAgo, busy } from '../dom.js';
import { openDialog, confirmDialog, setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';

const STATE_LABEL = {
  connected: 'Verbunden',
  degraded: 'Eingeschränkt',
  connecting: 'Verbinde …',
  disconnected: 'Getrennt',
  error: 'Fehler',
};

function statusCell(st) {
  const state = st ? st.state : 'disconnected';
  const el = h('div', {},
    h('span', { class: `status ${state}` }, state === 'connecting' ? h('span', { class: 'spin' }) : h('span', { class: 'dot' }), STATE_LABEL[state] || state),
  );
  const details = [];
  if (st && st.authLocked) details.push('Login gesperrt – Zugangsdaten prüfen oder neu verbinden');
  else if (st && st.error) details.push(st.error);
  if (st && st.degraded) details.push(st.degraded);
  if (st && st.storage === 'absent') details.push('keine Festplatte erkannt');
  if (st && st.nextRetry && state === 'disconnected') details.push(`nächster Versuch ${new Date(st.nextRetry).toLocaleTimeString()}`);
  if (st && st.lastOk && state !== 'connected') details.push(`zuletzt OK ${fmtAgo(st.lastOk)}`);
  if (details.length) el.appendChild(h('div', { class: 'status-detail' }, details.join(' · ')));
  return el;
}

function protocolOf(ctx, id) {
  return (ctx.protocols || []).find((p) => p.id.toLowerCase() === String(id || '').toLowerCase()) || null;
}

// What CNM found out at the first connection (or that it still has to).
function detectedText(proto, d) {
  if (!proto || !proto.autoDetect) return '';
  if (d && d.detected && d.detected.summary) return d.detected.summary;
  return 'wird beim ersten Verbinden erkannt';
}

export function renderDevices(root, ctx) {
  const tbody = h('tbody');
  const cells = new Map();
  root.append(
    h('div', { class: 'toolbar' },
      h('h2', {}, 'Geräte'),
      h('span', { class: 'faint' }, 'Geräte stellen Kameras bereit – Kameras werden im Tab „Kameras“ verwaltet.'),
      h('button', { class: 'btn', onclick: () => openDiscovery(ctx) }, 'Netzwerk durchsuchen'),
      h('button', { class: 'btn primary', onclick: () => editDevice(ctx, null) }, 'Gerät hinzufügen'),
    ),
    h('table', { class: 'list' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Status'), h('th', {}, 'Label'), h('th', {}, 'Protokoll'), h('th', {}, 'Host'), h('th', {}, 'Kameras'), h('th', {}))),
      tbody,
    ),
  );
  const devices = ctx.data.devices || [];
  // devices still to be detected: reload once they connected (variant, cameras)
  const undetected = new Set(devices.filter((d) => { const p = protocolOf(ctx, d.protocol); return p && p.autoDetect && !d.detected; }).map((d) => d.id));
  if (!devices.length) {
    tbody.appendChild(h('tr', {}, h('td', { colspan: '6', class: 'muted' }, 'Noch keine Geräte angelegt.')));
  }
  for (const d of devices) {
    const proto = protocolOf(ctx, d.protocol);
    const stCell = h('td', {}, statusCell(store.devices.get(d.id) || d.status));
    cells.set(d.id, stCell);
    const cams = (ctx.data.cameras || []).filter((c) => c.deviceId === d.id);
    tbody.appendChild(h('tr', {},
      stCell,
      h('td', {}, d.label),
      h('td', {}, h('div', {}, proto ? proto.name : (d.protocol || 'kein Protokoll gewählt')), h('div', { class: 'faint' }, detectedText(proto, d))),
      h('td', { class: 'mono' }, d.host),
      h('td', {}, `${cams.filter((c) => c.enabled).length} / ${cams.length}`),
      h('td', { class: 'actions' },
        h('button', { class: 'btn small', onclick: (e) => busy(e.currentTarget, async () => {
          if (await guarded(() => api.post('/api/devices/reconnect', { id: d.id })) !== false) toast(`${d.label}: Neuverbindung angestoßen`);
        }) }, 'Reconnect'),
        h('button', { class: 'btn small', onclick: () => editDevice(ctx, d) }, 'Bearbeiten'),
        h('button', { class: 'btn small danger', onclick: async (e) => {
          const btn = e.currentTarget;
          if (!await confirmDialog('Gerät löschen', `„${d.label}“ wirklich löschen? Die ${cams.length} bereitgestellten Kameras werden ebenfalls entfernt (auch aus Gruppen und Monitoren).`, { confirmLabel: 'Löschen', danger: true })) return;
          await busy(btn, async () => {
            if (await guarded(() => api.del(`/api/settings/devices/${encodeURIComponent(d.id)}`)) !== false) await ctx.reload();
          });
        } }, 'Löschen'),
      ),
    ));
  }
  return {
    onDevice(st) {
      const cell = cells.get(st.id);
      if (cell) { clear(cell); cell.appendChild(statusCell(st)); }
      if (undetected.has(st.id) && (st.state === 'connected' || st.state === 'degraded')) {
        undetected.delete(st.id);
        setTimeout(() => ctx.reload(), 300); // the detection is stored right before "connected"
      }
    },
  };
}

function optionInput(o, value) {
  const v = value ?? o.default ?? '';
  switch (o.type) {
    case 'text':
      return h('input', { type: 'text', value: v, placeholder: o.placeholder || '', spellcheck: 'false' });
    case 'number':
      return h('input', { type: 'number', value: v, min: String(o.min ?? 0), max: o.max ? String(o.max) : undefined, step: '1' });
    default:
      return select(o.choices || [], v);
  }
}

// Capability matrix of a protocol (one line per function, see provider.Capabilities).
const CAP_ROWS = [
  ['discovery', 'Discovery'],
  ['channelList', 'Kanalliste'],
  ['liveMain', 'Live Mainstream'],
  ['liveSub', 'Live Substream'],
  ['recordingSearch', 'Aufnahmesuche'],
  ['playback', 'Wiedergabe / Seek'],
  ['download', 'Download'],
  ['deviceTime', 'Gerätezeit'],
  ['storageInfo', 'Speicherstatus'],
  ['ptz', 'PTZ'],
  ['config', 'Konfiguration'],
  ['events', 'Events'],
];
const CAP_STATE = {
  verified: ['ok', 'ja'],
  experimental: ['warn', 'experimentell'],
  disabled: ['', 'deaktiviert'],
  unsupported: ['none', '–'],
};

export function capabilityTable(m) {
  const caps = (m && m.capabilities) || {};
  const rows = [];
  if (m && m.auth) rows.push(h('div', { class: 'cap' }, h('span', { class: 'k' }, 'Anmeldung'), h('span', { class: 'v ok' }, m.auth)));
  for (const [key, label] of CAP_ROWS) {
    const [cls, text] = CAP_STATE[caps[key]] || CAP_STATE.unsupported;
    let t = text;
    if (key === 'recordingSearch' && caps.searchKind === 'probe' && caps[key] !== 'unsupported') t += ' (Zeitprobe)';
    if (key === 'recordingSearch' && caps.searchKind === 'span' && caps[key] !== 'unsupported') t += ' (Zeitspanne)';
    rows.push(h('div', { class: 'cap' }, h('span', { class: 'k' }, label), h('span', { class: `v ${cls}` }, t)));
  }
  return h('div', { class: 'caps' }, rows);
}

// ---- network search ---------------------------------------------------------------------------

let lastScan = null; // { range, result } – kept while the settings page lives

function discoveryRows(ctx, result, showAdded, rerender) {
  const hosts = new Set((ctx.data.devices || []).map((d) => d.host));
  const devs = (result.devices || []).filter((d) => showAdded || !(d.added || hosts.has(d.ip)));
  if (!devs.length) {
    return h('p', { class: 'muted' }, (result.devices || []).length
      ? 'Alle gefundenen Geräte sind bereits angelegt.'
      : 'Keine Geräte gefunden. Liegen die Geräte in einem anderen Subnetz (über Router/Firewall), dort den Adressbereich angeben.');
  }
  const rows = devs.map((d) => {
    const added = d.added || hosts.has(d.ip);
    const s = d.suggestion;
    const proto = s ? protocolOf(ctx, s.protocol) : null;
    const ident = [d.mac ? `MAC ${d.mac}` : '', d.serial ? `SN ${d.serial}` : ''].filter(Boolean).join(' · ');
    const action = added
      ? h('span', { class: 'faint' }, `angelegt${d.deviceLabel ? `: ${d.deviceLabel}` : ''}`)
      : (proto
        ? h('button', { class: 'btn small primary', onclick: () => editDevice(ctx, null, {
          prefill: { label: s.label || d.name || d.ip, protocol: proto.id, host: d.ip, ports: s.ports || {}, options: s.options || {} },
          onSaved: () => { d.added = true; rerender(); },
        }) }, 'Hinzufügen')
        : h('span', { class: 'faint' }, 'kein passendes Protokoll'));
    return h('tr', {},
      h('td', { class: 'mono' }, d.ip),
      h('td', {},
        h('div', {}, d.name || '–', d.channels ? h('span', { class: 'faint' }, ` · ${d.channels} Kanäle`) : null),
        h('div', { class: 'faint' }, proto ? `Vorschlag: ${proto.name}` : ''),
        (d.notes || []).length ? h('div', { class: 'status-detail warn' }, d.notes.join(' · ')) : null),
      h('td', {}, (d.protocols || []).join(', ')),
      h('td', { class: 'faint' }, ident || '–'),
      h('td', { class: 'actions' }, action),
    );
  });
  return h('table', { class: 'list' },
    h('thead', {}, h('tr', {}, h('th', {}, 'Adresse'), h('th', {}, 'Gerät'), h('th', {}, 'Erkannt über'), h('th', {}, 'Kennung'), h('th', {}))),
    h('tbody', {}, rows));
}

function openDiscovery(ctx) {
  const rangeIn = h('input', { type: 'text', value: lastScan ? lastScan.range : '', placeholder: 'optional – einzelne Adresse oder Netz in CIDR-Schreibweise (bis /22)', spellcheck: 'false' });
  const showAdded = h('input', { type: 'checkbox' });
  const msg = h('div', { hidden: true });
  const out = h('div', { class: 'discovery-out' });
  const render = () => {
    clear(out);
    if (!lastScan) {
      out.appendChild(h('p', { class: 'muted' }, 'Die Suche fragt die Geräte im lokalen Netz per ONVIF, DVRIP, Dahua, Hikvision SADP und Axis SSDP ab (ca. 4 Sekunden). Für Geräte hinter einem Router kann zusätzlich ein Adressbereich gezielt abgefragt werden.'));
      return;
    }
    const r = lastScan.result;
    out.append(
      h('div', { class: 'faint', style: { margin: '6px 0' } }, `${(r.devices || []).length} Gerät(e) gefunden in ${(r.durationMs / 1000).toFixed(1)} s${lastScan.range ? ` · Bereich ${lastScan.range}` : ''}`),
      discoveryRows(ctx, r, showAdded.checked, render),
    );
    if ((r.warnings || []).length) out.appendChild(h('div', { class: 'status-detail' }, `Hinweise: ${r.warnings.join(' · ')}`));
  };
  showAdded.addEventListener('change', render);
  render();
  const search = async () => {
    setMessage(msg, '');
    clear(out);
    out.appendChild(h('div', { class: 'row' }, h('span', { class: 'spin' }), 'Suche läuft …'));
    try {
      const range = rangeIn.value.trim();
      const result = await api.post('/api/settings/discovery', { range });
      lastScan = { range, result };
    } catch (e) {
      setMessage(msg, e.message, 'error');
    }
    render();
    return false;
  };
  openDialog({
    title: 'Netzwerk durchsuchen',
    size: 'large',
    body: h('div', {},
      h('div', { class: 'form-grid' },
        field('Adressbereich (Unicast, optional)', rangeIn, 'Einzelne IP oder CIDR bis /22 – z. B. Subnetz hinter der Firewall'),
        field('Anzeige', h('label', { class: 'check' }, showAdded, ' bereits angelegte Geräte anzeigen')),
      ),
      msg,
      out,
    ),
    actions: [
      { label: 'Schließen' },
      { label: 'Suchen', kind: 'primary', onClick: search },
    ],
  });
}

function editDevice(ctx, dev, { prefill = null, onSaved = null } = {}) {
  const isNew = !dev;
  const protocols = ctx.protocols || [];
  const d = dev ? JSON.parse(JSON.stringify(dev)) : {
    id: '', label: '', protocol: protocols[0] ? protocols[0].id : '', host: '', username: 'admin',
    ports: {}, options: {},
    ...(prefill || {}),
  };

  const msg = h('div', { hidden: true });
  const labelIn = h('input', { type: 'text', value: d.label, placeholder: 'optional – sonst die Adresse', maxlength: '80' });
  const protoSel = select(protocols.map((p) => ({ value: p.id, label: p.name })), d.protocol);
  const userIn = h('input', { type: 'text', value: d.username || '', autocomplete: 'off' });
  const passIn = h('input', { type: 'password', value: '', autocomplete: 'new-password', placeholder: isNew ? '' : 'unverändert lassen' });
  const hostIn = h('input', { type: 'text', value: d.host, placeholder: 'IP-Adresse oder Hostname', spellcheck: 'false' });
  // one grid per section so protocol specific options line up with the base fields
  const deviceGrid = h('div', { class: 'form-grid' },
    field('Protokoll', protoSel),
    field('Benutzername', userIn),
    field('Passwort', passIn, isNew ? 'Wird verschlüsselt gespeichert' : 'Leer lassen = Passwort unverändert'),
  );
  const deviceBase = deviceGrid.children.length;
  const networkGrid = h('div', { class: 'form-grid' });
  const advancedGrid = h('div', { class: 'form-grid' });
  const advanced = h('details', { class: 'caps-box' }, h('summary', {}, 'Erweitert (Ports, Kanäle)'), advancedGrid);
  const notes = h('div', { hidden: true, style: { marginTop: '10px' } });
  const capsBox = h('details', { class: 'caps-box' }, h('summary', {}, 'Funktionen dieses Protokolls'));
  const camInfo = h('div', { class: 'faint', style: { marginTop: '10px' } });
  const testResult = h('div', { hidden: true, style: { marginTop: '10px' } });
  const portInputs = {};
  const optInputs = {};

  function current() { return protocolOf(ctx, protoSel.value); }

  function updateCamInfo() {
    const p = current();
    if (!p) { camInfo.textContent = ''; return; }
    const inp = optInputs.channels;
    const n = parseInt(inp ? inp.value : '', 10);
    if (p.autoDetect && !(n > 0)) {
      const sameDevice = !isNew && p.id === dev.protocol && hostIn.value.trim() === dev.host;
      camInfo.textContent = sameDevice && dev.detected && dev.detected.summary
        ? `Erkannt: ${dev.detected.summary} · ${dev.channelCount} Kamera${dev.channelCount === 1 ? '' : 's'}`
        : 'Protokollvariante und Kanalzahl erkennt CNM beim ersten Verbinden; die Kameras werden dann automatisch angelegt.';
      return;
    }
    const count = p.channels > 0 ? p.channels : (n > 0 ? n : 1);
    camInfo.textContent = `Dieses Gerät stellt ${count} Kamera${count === 1 ? '' : 's'} bereit.`;
  }

  function renderParts() {
    const p = current();
    while (deviceGrid.children.length > deviceBase) deviceGrid.lastChild.remove();
    clear(networkGrid);
    clear(advancedGrid);
    for (const k of Object.keys(portInputs)) delete portInputs[k];
    for (const k of Object.keys(optInputs)) delete optInputs[k];
    networkGrid.appendChild(field('IP / Hostname', hostIn));
    if (!p) return;
    for (const ps of p.ports) {
      const inp = h('input', { type: 'number', min: '1', max: '65535', value: (d.ports && d.ports[ps.key]) || ps.default });
      portInputs[ps.key] = inp;
      advancedGrid.appendChild(field(/port/i.test(ps.label) ? ps.label : `${ps.label}-Port`, inp, `Standard ${ps.default}`));
    }
    for (const o of p.options || []) {
      const inp = optionInput(o, d.options ? d.options[o.key] : undefined);
      if (o.placeholder && o.type === 'number') inp.placeholder = o.placeholder;
      optInputs[o.key] = inp;
      const f = field(o.label, inp, o.help);
      if (o.type === 'text' && o.section === 'network') f.classList.add('wide');
      ({ network: networkGrid, advanced: advancedGrid }[o.section] || deviceGrid).appendChild(f);
      if (o.key === 'channels') inp.addEventListener('input', updateCamInfo);
    }
    advanced.hidden = !advancedGrid.children.length;
    while (capsBox.children.length > 1) capsBox.lastChild.remove();
    capsBox.appendChild(capabilityTable(p));
    setMessage(notes, p.notes || '', 'warn');
    updateCamInfo();
  }

  protoSel.addEventListener('change', () => { d.ports = {}; d.options = {}; renderParts(); });
  hostIn.addEventListener('input', updateCamInfo);
  renderParts();

  function payload() {
    const ports = {};
    for (const [k, inp] of Object.entries(portInputs)) ports[k] = parseInt(inp.value, 10) || 0;
    const options = {};
    for (const [k, inp] of Object.entries(optInputs)) options[k] = inp.value;
    return {
      id: isNew ? '' : dev.id,
      label: labelIn.value.trim(),
      protocol: protoSel.value,
      host: hostIn.value.trim(),
      ports,
      options,
      username: userIn.value,
      password: passIn.value,
    };
  }

  openDialog({
    title: isNew ? 'Gerät hinzufügen' : `Gerät bearbeiten – ${dev.label}`,
    size: 'medium',
    body: h('div', {},
      h('div', { class: 'idline' }, h('span', { class: 'k' }, 'ID'), h('span', { class: 'v' }, isNew ? 'wird beim Speichern automatisch vergeben' : dev.id)),
      msg,
      field('Label', labelIn, 'Bezeichnung des Geräts'),
      h('div', { class: 'fieldset' },
        h('div', { class: 'legend' }, 'Device Settings'),
        deviceGrid,
      ),
      h('div', { class: 'fieldset' },
        h('div', { class: 'legend' }, 'Netzwerk'),
        networkGrid,
      ),
      advanced,
      camInfo,
      capsBox,
      notes,
      testResult,
    ),
    actions: [
      { label: 'Verbindung testen', left: true, onClick: async () => {
        setMessage(testResult, '');
        testResult.hidden = false;
        clear(testResult);
        testResult.className = 'msg';
        testResult.append(h('span', { class: 'row' }, h('span', { class: 'spin' }), 'Teste Verbindung …'));
        testResult.scrollIntoView({ block: 'nearest' });
        try {
          const r = await api.post('/api/settings/devices/test', payload());
          if (r.ok) setMessage(testResult, r.message, 'ok');
          else setMessage(testResult, `${r.kind === 'auth' ? 'Authentifizierung fehlgeschlagen: ' : ''}${r.message}`, 'error');
        } catch (e) {
          setMessage(testResult, e.message, 'error');
        }
        testResult.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        return false;
      } },
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        setMessage(msg, '');
        const ok = await guarded(() => api.post('/api/settings/devices', payload()), msg);
        if (ok === false) { msg.scrollIntoView({ block: 'nearest' }); return false; }
        const p = current();
        toast(isNew
          ? (p && p.autoDetect ? 'Gerät angelegt – CNM verbindet sich und legt die Kameras an' : 'Gerät angelegt – die Kameras stehen im Tab „Kameras“ bereit')
          : 'Gerät gespeichert');
        await ctx.reload();
        if (onSaved) onSaved();
        return true;
      } },
    ],
  });
}
