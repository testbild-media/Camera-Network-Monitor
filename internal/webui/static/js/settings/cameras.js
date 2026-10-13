// Cameras: every camera of every device as a first-class entry. Groups are
// managed in their own tab ("Kamera-Gruppen"); here they only filter.
import api from '../api.js';
import store from '../store.js';
import { h, clear, select, field, checkbox, busy } from '../dom.js';
import { openDialog, setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';
import { StreamSlot } from '../streamslot.js';
import { applyAspect, applyFit } from '../multiview.js';

const ASPECTS = [
  { value: '16:9', label: '16:9' }, { value: '4:3', label: '4:3' }, { value: '1:1', label: '1:1' }, { value: 'auto', label: 'Auto / Quelle' },
];
const FITS = [
  { value: 'fill', label: 'Stretch / Fill' }, { value: 'contain', label: 'Contain' }, { value: 'cover', label: 'Cover / Crop' },
];

const REC = {
  yes: ['vorhanden', 'rec-yes', 'Auf dem Gerät liegen Aufnahmen dieser Kamera vor – Playback verfügbar'],
  no: ['keine', 'rec-no', 'Keine Aufnahmen auf dem Gerät – Playback wird nicht angeboten'],
  unknown: ['unbekannt', 'rec-no', 'Noch nicht geprüft (Gerät offline oder Prüfung läuft)'],
};

export function renderCameras(root, ctx) {
  const state = { filter: 'all', query: '', selected: new Set() };
  const chips = h('div', { class: 'chips' });
  const search = h('input', { type: 'text', placeholder: 'Kameras suchen …', style: { width: '260px' } });
  const bulk = h('div', { class: 'bulkbar' });
  const tbody = h('tbody');
  const all = h('input', { type: 'checkbox', title: 'Alle sichtbaren auswählen' });

  root.append(
    h('div', { class: 'toolbar' },
      h('h2', {}, 'Kameras'),
      search,
      h('button', { class: 'btn', onclick: () => ctx.show('groups') }, 'Kamera-Gruppen …'),
    ),
    chips,
    bulk,
    h('table', { class: 'list' },
      h('thead', {}, h('tr', {},
        h('th', { style: { width: '34px' } }, all),
        h('th', {}, 'Name'), h('th', {}, 'Aktiv'), h('th', {}, 'Gerät · Kanal'), h('th', {}, 'Gruppen'), h('th', { title: 'Aufnahmen auf dem Gerät' }, 'Aufnahmen'), h('th', {}, 'Status'), h('th', {}),
      )),
      tbody,
    ),
  );

  const groups = () => ctx.data.groups || [];
  const groupName = (id) => (groups().find((g) => g.id === id) || {}).label || id;

  function visible() {
    const q = state.query.toLowerCase();
    return (ctx.data.cameras || []).filter((c) => {
      if (state.filter === 'none' && c.groups.length) return false;
      if (state.filter !== 'all' && state.filter !== 'none' && !c.groups.includes(state.filter)) return false;
      if (!q) return true;
      return [ctx.cameraName(c), c.deviceLabel, c.deviceName || '', `kanal ${c.channel + 1}`, ...c.groups.map(groupName)].join(' ').toLowerCase().includes(q);
    });
  }

  function renderChips() {
    clear(chips);
    const cams = ctx.data.cameras || [];
    const chip = (id, label, n) => h('button', { class: `chipbtn${state.filter === id ? ' active' : ''}`, onclick: () => { state.filter = id; render(); } }, label, h('span', { class: 'n' }, String(n)));
    chips.append(chip('all', 'Alle', cams.length));
    for (const g of groups()) chips.append(chip(g.id, g.label, g.cameraIds.length));
    chips.append(chip('none', 'Ohne Gruppe', cams.filter((c) => !c.groups.length).length));
  }

  function renderBulk() {
    clear(bulk);
    // the bar is always present (no layout shift); controls are disabled without a selection
    const none = state.selected.size === 0;
    bulk.classList.toggle('idle', none);
    const ids = [...state.selected];
    const run = (btn, body, text) => busy(btn, async () => {
      if (await guarded(() => api.post('/api/settings/cameras/bulk', { ids, ...body })) !== false) {
        toast(text);
        state.selected.clear();
        await ctx.reload();
      }
    });
    const aspSel = select([{ value: '', label: 'Bildformat …' }, ...ASPECTS], '');
    aspSel.addEventListener('change', () => { if (aspSel.value) run(aspSel, { aspectRatio: aspSel.value }, 'Bildformat gesetzt'); });
    const enBtn = h('button', { class: 'btn small', onclick: (e) => run(e.currentTarget, { enabled: true }, 'Aktiviert') }, 'Aktivieren');
    const disBtn = h('button', { class: 'btn small', onclick: (e) => run(e.currentTarget, { enabled: false }, 'Deaktiviert') }, 'Deaktivieren');
    const clrBtn = h('button', { class: 'btn ghost small', onclick: () => { state.selected.clear(); render(); } }, 'Auswahl aufheben');
    for (const el of [aspSel, enBtn, disBtn, clrBtn]) el.disabled = none;
    bulk.append(
      h('strong', {}, none ? 'Keine Auswahl' : `${ids.length} ausgewählt`),
      aspSel, enBtn, disBtn,
      h('span', { class: 'spacer' }),
      clrBtn,
    );
  }

  function render() {
    renderChips();
    renderBulk();
    clear(tbody);
    const rows = visible();
    all.checked = rows.length > 0 && rows.every((c) => state.selected.has(c.id));
    if (!rows.length) {
      tbody.appendChild(h('tr', {}, h('td', { colspan: '8', class: 'muted' }, (ctx.data.cameras || []).length ? 'Keine Kamera passt zur Suche.' : 'Noch keine Kameras – zuerst im Tab „Geräte“ ein Gerät hinzufügen.')));
      return;
    }
    for (const c of rows) {
      const cb = h('input', { type: 'checkbox', checked: state.selected.has(c.id) });
      cb.addEventListener('change', () => { if (cb.checked) state.selected.add(c.id); else state.selected.delete(c.id); renderBulk(); all.checked = visible().every((x) => state.selected.has(x.id)); });
      const st = store.devices.get(c.deviceId);
      const [sym, cls, title] = c.playbackCapable ? (REC[c.recordings] || REC.unknown) : ['–', 'rec-no', 'Gerät unterstützt kein Playback'];
      tbody.appendChild(h('tr', { class: c.enabled ? '' : 'off' },
        h('td', {}, cb),
        h('td', {}, h('div', {}, ctx.cameraName(c)), c.label && c.deviceName ? h('div', { class: 'faint' }, `Gerät: ${c.deviceName}`) : null),
        h('td', {}, enableToggle(c)),
        h('td', {}, h('div', {}, c.deviceLabel), h('div', { class: 'faint' }, `Kanal ${c.channel + 1}`)),
        h('td', {}, h('div', { class: 'badges' }, c.groups.map((g) => h('span', { class: 'badge' }, groupName(g))))),
        h('td', { class: cls, title }, sym),
        h('td', {}, h('span', { class: `status ${st ? st.state : 'disconnected'}` }, h('span', { class: 'dot' }), st && (st.state === 'connected' || st.state === 'degraded') ? 'online' : 'offline')),
        h('td', { class: 'actions' }, h('button', { class: 'btn small', onclick: () => editCamera(ctx, c) }, 'Bearbeiten')),
      ));
    }
  }

  // Direct activation toggle: optimistic, persisted via the bulk endpoint,
  // only this row changes (no tab rebuild). The server stops all streams of a
  // disabled camera and pushes the new state to every viewer.
  function enableToggle(c) {
    const btn = h('button', { type: 'button', class: 'toggle', role: 'switch' });
    const paint = () => {
      btn.classList.toggle('on', c.enabled);
      btn.setAttribute('aria-checked', String(c.enabled));
      btn.title = c.enabled ? 'Kamera deaktivieren – beendet alle Streams dieser Kamera' : 'Kamera aktivieren';
      clear(btn);
      btn.append(h('span', { class: 'knob' }), c.enabled ? 'Aktiviert' : 'Deaktiviert');
      const tr = btn.closest('tr');
      if (tr) tr.classList.toggle('off', !c.enabled);
    };
    btn.addEventListener('click', async () => {
      const want = !c.enabled;
      btn.disabled = true;
      c.enabled = want;
      paint();
      const ok = await guarded(() => api.post('/api/settings/cameras/bulk', { ids: [c.id], enabled: want }));
      if (ok === false) { c.enabled = !want; paint(); } else toast(`${ctx.cameraName(c)} ${want ? 'aktiviert' : 'deaktiviert'}`);
      btn.disabled = false;
    });
    paint();
    return btn;
  }

  all.addEventListener('change', () => {
    for (const c of visible()) { if (all.checked) state.selected.add(c.id); else state.selected.delete(c.id); }
    render();
  });
  search.addEventListener('input', () => { state.query = search.value.trim(); render(); });
  render();
  return null;
}

function editCamera(ctx, cam) {
  const msg = h('div', { hidden: true });
  const name = h('input', { type: 'text', value: cam.label || '', placeholder: cam.deviceName || `${cam.deviceLabel} · Kanal ${cam.channel + 1}`, maxlength: '80' });
  const enabled = checkbox('Kamera aktiv', cam.enabled);
  const main = checkbox('Mainstream verfügbar', cam.streams.main);
  const sub = checkbox('Substream verfügbar', cam.streams.sub);
  const asp = select(ASPECTS, cam.view.aspectRatio);
  const fit = select(FITS, cam.view.objectFit);
  const groupNames = (ctx.data.groups || []).filter((g) => cam.groups.includes(g.id)).map((g) => g.label);
  const groupList = h('div', { class: 'badges' },
    groupNames.length ? groupNames.map((n) => h('span', { class: 'badge' }, n)) : h('span', { class: 'faint' }, 'keiner Gruppe zugeordnet'),
    h('span', { class: 'faint', style: { marginLeft: '8px' } }, '– bearbeiten im Tab „Kamera-Gruppen“'));

  // live preview to identify the camera: sub stream only (the main stream is
  // never opened here); the player is destroyed when the dialog closes
  const frame = h('div', { class: 'frame' });
  const cover = h('div', { class: 'loading-cover' }, h('span', { class: 'spin lg' }), 'Vorschau lädt …');
  const preview = h('div', { class: 'preview-video' }, frame, cover);
  applyAspect(frame, cam.view.aspectRatio);
  applyFit(frame, cam.view.objectFit);
  asp.addEventListener('change', () => applyAspect(frame, asp.value));
  fit.addEventListener('change', () => applyFit(frame, fit.value));
  let slot = null;
  if (cam.preview && cam.enabled) {
    slot = new StreamSlot(frame, cam.preview, {
      onVisual: (v) => {
        cover.hidden = v === 'ok';
        if (v === 'offline') { clear(cover); cover.append('STREAM OFFLINE'); }
      },
    });
  } else {
    clear(cover);
    cover.append(cam.enabled ? 'Kein Substream – Vorschau aus (der Mainstream wird hier nicht geöffnet)' : 'Kamera deaktiviert');
  }

  const recText = !cam.playbackCapable ? 'Gerät ohne Playback' : { yes: 'Aufnahmen vorhanden', no: 'keine Aufnahmen auf dem Gerät', unknown: 'noch unbekannt' }[cam.recordings] || 'unbekannt';
  openDialog({
    title: `Kamera bearbeiten – ${ctx.cameraName(cam)}`,
    size: 'large',
    onClose: () => { if (slot) slot.destroy(); },
    body: h('div', {},
      msg,
      h('div', { class: 'cam-dialog' },
        h('div', {},
          preview,
          h('div', { class: 'faint', style: { marginTop: '8px' } },
            `Quelle: ${cam.deviceLabel} · Kanal ${cam.channel + 1}`, cam.deviceName ? ` · am Gerät: ${cam.deviceName}` : '',
            h('br'), `Aufnahmen: ${recText}`),
        ),
        h('div', {},
          h('div', { class: 'form-grid' },
            h('div', { class: 'wide' }, field('Name', name, 'Leer = Name vom Gerät bzw. „Gerät · Kanal“')),
            h('div', { class: 'field wide' }, h('span', {}, 'Status'), enabled),
            h('div', { class: 'field wide' }, h('span', {}, 'Streams'), h('div', { class: 'row', style: { gap: '18px' } }, main, sub)),
            field('Bildformat', asp),
            field('Fit', fit),
          ),
          h('div', { class: 'section-title' }, 'Gruppen'),
          groupList,
        ),
      ),
    ),
    actions: [
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        setMessage(msg, '');
        const body = {
          id: cam.id,
          label: name.value.trim(),
          enabled: enabled.input.checked,
          streams: { main: main.input.checked, sub: sub.input.checked },
          view: { aspectRatio: asp.value, objectFit: fit.value },
        };
        const ok = await guarded(() => api.post('/api/settings/cameras', body), msg);
        if (ok === false) return false;
        toast('Kamera gespeichert');
        await ctx.reload();
        return true;
      } },
    ],
  });
}
