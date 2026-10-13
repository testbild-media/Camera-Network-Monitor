// Monitors / layouts with a drag & drop editor: cameras are dragged from a
// searchable list onto the slots of the row based layout.
import api from '../api.js';
import store from '../store.js';
import { h, clear, select, field, checkbox, busy } from '../dom.js';
import { openDialog, confirmDialog, setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';

const rid = (p) => p + Math.random().toString(16).slice(2, 12);
const DT_CAM = 'application/x-cnm-camera';
const DT_SLOT = 'application/x-cnm-slot';
const DT_SECTION = 'application/x-cnm-cameras'; // a whole device/group (JSON list of camera ids)

function pickerMode() {
  return (store.prefs && store.prefs.cameraPicker) === 'group' ? 'group' : 'device';
}

// the choice is a personal preference (saved for the user, failures ignored)
function setPickerMode(mode) {
  store.prefs = { ...(store.prefs || {}), cameraPicker: mode };
  api.put('/api/me/preferences', { cameraPicker: mode }, { background: true }).catch(() => {});
}

export function renderMonitors(root, ctx) {
  const cards = h('div', { class: 'cards' });
  root.append(
    h('div', { class: 'toolbar' }, h('h2', {}, 'Monitore / Layouts'), h('button', { class: 'btn primary', onclick: () => editMonitor(ctx, null) }, 'Monitor hinzufügen')),
    cards,
  );
  for (const m of ctx.data.monitors || []) {
    const assigned = new Set();
    let slots = 0;
    for (const r of m.layout.rows) for (const s of r.slots) { slots++; if (s.cameraId) assigned.add(s.cameraId); }
    cards.appendChild(h('div', { class: 'card' },
      h('div', { class: 'row' }, h('h3', {}, m.label), h('span', { class: 'spacer' }), m.enabled ? null : h('span', { class: 'badge off' }, 'deaktiviert')),
      h('div', { class: 'muted' }, `${assigned.size} Kamera${assigned.size === 1 ? '' : 's'} zugewiesen · ${slots} Slots · ${m.layout.rows.length} Zeilen`),
      preview(ctx, m.layout),
      h('div', { class: 'row' },
        h('button', { class: 'btn small primary', onclick: () => editMonitor(ctx, m) }, 'Layout bearbeiten'),
        h('span', { class: 'spacer' }),
        h('button', { class: 'btn small danger', onclick: async (e) => {
          const btn = e.currentTarget;
          const last = (ctx.data.monitors || []).length <= 1;
          const text = last ? `„${m.label}“ ist der letzte Monitor. Beim Löschen wird automatisch ein neuer Standardmonitor (Monitor 1, 2×2) angelegt.` : `Monitor „${m.label}“ löschen? Kiosk-Keys für diesen Monitor verlieren ihren Zugriff.`;
          if (!await confirmDialog('Monitor löschen', text, { confirmLabel: 'Löschen', danger: true })) return;
          await busy(btn, async () => {
            if (await guarded(() => api.del(`/api/settings/monitors/${encodeURIComponent(m.id)}`)) !== false) await ctx.reload();
          });
        } }, 'Löschen'),
      ),
    ));
  }
  return null;
}

function preview(ctx, layout) {
  const el = h('div', { class: 'preview' });
  el.style.gap = `${Math.min(layout.gap, 6) / 2}px`;
  for (const r of layout.rows) {
    const row = h('div', { class: 'prow' });
    row.style.flexGrow = String(r.height);
    for (const s of r.slots) {
      const cam = s.cameraId ? ctx.camera(s.cameraId) : null;
      const slot = h('div', { class: `pslot${cam ? ' set' : ''}`, title: cam ? ctx.cameraName(cam) : 'nicht belegt' });
      slot.style.flexGrow = String(s.width);
      slot.textContent = s.label || (cam ? ctx.cameraName(cam) : '');
      row.appendChild(slot);
    }
    el.appendChild(row);
  }
  return el;
}

function stepper(value, { min, max, step, fmt }, onChange) {
  const span = h('span', {}, fmt(value));
  const set = (v) => {
    const n = Math.min(max, Math.max(min, Math.round(v / step) * step));
    span.textContent = fmt(n);
    onChange(n);
  };
  return h('div', { class: 'stepper' },
    h('button', { type: 'button', title: 'kleiner', onclick: () => set(value = Math.max(min, value - step)) }, '−'),
    span,
    h('button', { type: 'button', title: 'größer', onclick: () => set(value = Math.min(max, value + step)) }, '+'),
  );
}

function editMonitor(ctx, mon) {
  const isNew = !mon;
  const m = mon ? JSON.parse(JSON.stringify(mon)) : {
    id: '', label: `Monitor ${(ctx.data.monitors || []).length + 1}`, enabled: true,
    layout: { template: '2x2', gap: 4, rows: [] },
  };
  if (isNew) applyTemplate(ctx, m, '2x2');
  let selSlot = (m.layout.rows[0] && m.layout.rows[0].slots[0]) ? m.layout.rows[0].slots[0].id : null;

  const msg = h('div', { hidden: true });
  const labelIn = h('input', { type: 'text', value: m.label, maxlength: '64' });
  const enabled = checkbox('Monitor aktiv', m.enabled);
  const gapIn = h('input', { type: 'range', min: '0', max: '24', step: '1', value: String(m.layout.gap) });
  const gapVal = h('span', { class: 'mono' }, `${m.layout.gap}px`);
  const tplSel = select((ctx.templates || []).map((t) => ({ value: t.id, label: t.label })), m.layout.template || '2x2');
  const searchIn = h('input', { type: 'search', placeholder: 'Kamera suchen …' });
  let mode = pickerMode();
  // palette sections ("device:<id>", "group:<id>") start collapsed each time
  const expanded = new Set();
  const modeBtns = {
    device: h('button', { type: 'button', class: 'btn small', onclick: () => switchMode('device') }, 'Geräte'),
    group: h('button', { type: 'button', class: 'btn small', onclick: () => switchMode('group') }, 'Gruppen'),
  };
  const modeSeg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Sortierung' }, modeBtns.device, modeBtns.group);
  function switchMode(m) {
    if (m === mode) return;
    mode = m;
    setPickerMode(m);
    renderPalette();
  }
  const palList = h('div', { class: 'palette-list' });
  const canvas = h('div', { class: 'canvas' });
  const inspector = h('div', { class: 'inspector' });

  const find = () => {
    for (const r of m.layout.rows) for (const s of r.slots) if (s.id === selSlot) return { row: r, slot: s };
    return null;
  };
  const usage = () => {
    const n = new Map();
    for (const r of m.layout.rows) for (const s of r.slots) if (s.cameraId) n.set(s.cameraId, (n.get(s.cameraId) || 0) + 1);
    return n;
  };
  const custom = () => { m.layout.template = ''; };

  function assign(slotId, cameraId) {
    for (const r of m.layout.rows) for (const s of r.slots) if (s.id === slotId) s.cameraId = cameraId;
    renderAll();
  }

  // fill drops a whole device/group: its cameras go into the slots from the
  // target on, in reading order (left to right, top to bottom)
  function fill(slotId, cameraIds) {
    const slots = [];
    for (const r of m.layout.rows) for (const s of r.slots) slots.push(s);
    let i = slots.findIndex((s) => s.id === slotId);
    if (i < 0) return;
    const start = i;
    let placed = 0;
    for (const id of cameraIds) {
      if (i >= slots.length) break;
      slots[i++].cameraId = id;
      placed++;
    }
    selSlot = slots[Math.min(i, slots.length) - 1].id;
    renderAll();
    const rest = cameraIds.length - placed;
    toast(`${placed} Kamera${placed === 1 ? '' : 's'} ab Slot ${start + 1} eingetragen${rest > 0 ? ` – ${rest} passten nicht mehr (Layout hat zu wenige Slots)` : ''}`);
  }

  function swap(fromId, toId) {
    let a, b;
    for (const r of m.layout.rows) for (const s of r.slots) { if (s.id === fromId) a = s; if (s.id === toId) b = s; }
    if (!a || !b || a === b) return;
    [a.cameraId, b.cameraId] = [b.cameraId, a.cameraId];
    selSlot = b.id;
    renderAll();
  }

  // ---- palette ----
  // Sections by device or by camera group (a camera may appear in several
  // groups, cameras without group under "Ohne Gruppe"). Disabled cameras are
  // never offered for new assignments. While searching, every section with a
  // hit is shown expanded so collapsing never hides results.
  function paletteItem(c, used) {
    const n = used.get(c.id) || 0;
    // the device is the section in device mode – then the channel suffices
    const sub = mode === 'device' ? `K${c.channel + 1}` : `${c.deviceLabel} · K${c.channel + 1}`;
    const item = h('div', { class: 'pal-item', draggable: 'true', title: `${ctx.cameraName(c)} – ${c.deviceLabel}, Kanal ${c.channel + 1}: auf einen Slot ziehen oder Slot wählen und anklicken` },
      h('div', { class: 'nm' }, ctx.cameraName(c), h('span', { class: 'sub' }, sub)),
      n ? h('span', { class: 'used', title: 'bereits in diesem Layout' }, `${n}×`) : null,
    );
    item.addEventListener('dragstart', (e) => { e.dataTransfer.setData(DT_CAM, c.id); e.dataTransfer.effectAllowed = 'copy'; item.classList.add('dragging'); });
    item.addEventListener('dragend', () => item.classList.remove('dragging'));
    item.addEventListener('click', () => { if (selSlot) assign(selSlot, c.id); });
    return item;
  }

  function sections() {
    const cams = (ctx.data.cameras || []).filter((c) => c.enabled);
    const out = [];
    if (mode === 'group') {
      for (const g of ctx.data.groups || []) {
        const ids = new Set(g.cameraIds);
        out.push({ key: `group:${g.id}`, label: g.label, cams: cams.filter((c) => ids.has(c.id)) });
      }
      const none = cams.filter((c) => !c.groups.length);
      if (none.length) out.push({ key: 'group:', label: 'Ohne Gruppe', cams: none });
    } else {
      const byDev = new Map();
      for (const c of cams) {
        if (!byDev.has(c.deviceId)) byDev.set(c.deviceId, { key: `device:${c.deviceId}`, label: c.deviceLabel, cams: [] });
        byDev.get(c.deviceId).cams.push(c);
      }
      out.push(...byDev.values());
    }
    return out;
  }

  function renderPalette() {
    clear(palList);
    for (const [m, b] of Object.entries(modeBtns)) b.classList.toggle('active', m === mode);
    const q = searchIn.value.trim().toLowerCase();
    const used = usage();
    const match = (c) => !q || `${ctx.cameraName(c)} ${c.deviceLabel} ${c.deviceName || ''} kanal ${c.channel + 1}`.toLowerCase().includes(q);
    let shown = 0;
    for (const sec of sections()) {
      const hits = sec.cams.filter(match);
      if (q && !hits.length) continue; // section without hits is hidden while searching
      const open = q ? true : expanded.has(sec.key);
      const head = h('button', { type: 'button', class: `pal-head${open ? ' open' : ''}`, 'aria-expanded': String(open),
        draggable: hits.length ? 'true' : 'false',
        title: hits.length ? `Auf-/zuklappen – oder ganz auf einen Slot ziehen: die ${hits.length} Kameras füllen die folgenden Slots` : '',
        onclick: () => {
          if (q) return; // search results always stay visible
          if (expanded.has(sec.key)) expanded.delete(sec.key); else expanded.add(sec.key);
          renderPalette();
        } }, h('span', { class: 'caret' }, open ? '▼' : '▶'), h('span', { class: 'lbl' }, sec.label), h('span', { class: 'n' }, String(q ? hits.length : sec.cams.length)));
      head.addEventListener('dragstart', (e) => {
        e.dataTransfer.setData(DT_SECTION, JSON.stringify(hits.map((c) => c.id)));
        e.dataTransfer.effectAllowed = 'copy';
        head.classList.add('dragging');
      });
      head.addEventListener('dragend', () => head.classList.remove('dragging'));
      palList.appendChild(head);
      if (!open) continue;
      const body = h('div', { class: 'pal-sec' });
      if (!hits.length) body.appendChild(h('div', { class: 'faint', style: { padding: '4px 10px' } }, 'keine aktivierte Kamera'));
      for (const c of hits) body.appendChild(paletteItem(c, used));
      palList.appendChild(body);
      shown += hits.length;
    }
    if (!palList.childNodes.length || (q && !shown)) {
      clear(palList);
      const any = (ctx.data.cameras || []).some((c) => c.enabled);
      palList.appendChild(h('div', { class: 'faint', style: { padding: '8px' } }, q ? 'Keine Kamera gefunden.'
        : any ? (mode === 'group' ? 'Noch keine Gruppen – im Tab „Kamera-Gruppen“ anlegen.' : 'Keine aktivierte Kamera.')
          : 'Keine aktivierte Kamera – zuerst ein Gerät anlegen bzw. Kameras aktivieren.'));
    }
  }
  // dropping a slot onto the palette clears it
  palList.addEventListener('dragover', (e) => { if (e.dataTransfer.types.includes(DT_SLOT)) e.preventDefault(); });
  palList.addEventListener('drop', (e) => {
    const sid = e.dataTransfer.getData(DT_SLOT);
    if (sid) { e.preventDefault(); assign(sid, ''); }
  });

  // ---- canvas ----
  function renderCanvas() {
    clear(canvas);
    canvas.style.gap = `${m.layout.gap / 2}px`;
    canvas.style.padding = `${m.layout.gap / 2}px`;
    const sel = find();
    for (const r of m.layout.rows) {
      const row = h('div', { class: `crow${sel && sel.row === r ? ' sel' : ''}` });
      row.style.flexGrow = String(r.height);
      row.style.gap = `${m.layout.gap / 2}px`;
      for (const s of r.slots) {
        const cam = s.cameraId ? ctx.camera(s.cameraId) : null;
        const off = cam && !cam.enabled;
        const el = h('div', { class: `cslot${cam ? ' set' : ''}${off ? ' off' : ''}${s.id === selSlot ? ' sel' : ''}`, draggable: cam ? 'true' : 'false', title: off ? 'Kamera deaktiviert – bleibt zugewiesen, zeigt kein Bild; kann ersetzt oder entfernt werden' : '' },
          h('span', { class: 'nm' }, cam ? ctx.cameraName(cam) : 'leer'),
          off ? h('span', { class: 'badge off' }, 'deaktiviert') : null,
          s.label ? h('span', { class: 'lbl' }, s.label) : null,
          cam ? h('button', { class: 'clear', type: 'button', title: 'Slot leeren', onclick: (e) => { e.stopPropagation(); assign(s.id, ''); } }, '✕') : null,
        );
        el.style.flexGrow = String(s.width);
        el.addEventListener('click', () => { selSlot = s.id; renderCanvas(); renderInspector(); });
        el.addEventListener('dragstart', (e) => { e.dataTransfer.setData(DT_SLOT, s.id); e.dataTransfer.effectAllowed = 'move'; });
        el.addEventListener('dragover', (e) => {
          if ([DT_CAM, DT_SLOT, DT_SECTION].some((t) => e.dataTransfer.types.includes(t))) { e.preventDefault(); el.classList.add('over'); }
        });
        el.addEventListener('dragleave', () => el.classList.remove('over'));
        el.addEventListener('drop', (e) => {
          e.preventDefault();
          el.classList.remove('over');
          const camId = e.dataTransfer.getData(DT_CAM);
          const from = e.dataTransfer.getData(DT_SLOT);
          const list = e.dataTransfer.getData(DT_SECTION);
          if (camId) { selSlot = s.id; assign(s.id, camId); } else if (from) swap(from, s.id);
          else if (list) { try { fill(s.id, JSON.parse(list)); } catch { /* foreign drag data */ } }
        });
        row.appendChild(el);
      }
      canvas.appendChild(row);
    }
  }

  // ---- inspector ----
  function renderInspector() {
    clear(inspector);
    const sel = find();
    const addRow = h('button', { class: 'btn small', type: 'button', disabled: m.layout.rows.length >= 16, onclick: () => {
      const slot = { id: rid('s_'), cameraId: '', width: 1, label: '' };
      m.layout.rows.push({ id: rid('r_'), height: 1, slots: [slot] });
      selSlot = slot.id;
      custom();
      renderAll();
    } }, '+ Zeile');
    if (!sel) {
      inspector.append(h('div', { class: 'title' }, 'Slot auswählen, um ihn zu bearbeiten'), addRow);
      return;
    }
    const { row, slot } = sel;
    const lbl = h('input', { type: 'text', value: slot.label || '', placeholder: 'optional', maxlength: '64', style: { width: '180px' } });
    lbl.addEventListener('input', () => { slot.label = lbl.value; renderCanvas(); });
    const rowIdx = m.layout.rows.indexOf(row) + 1;
    const slotIdx = row.slots.indexOf(slot) + 1;
    inspector.append(
      h('div', { class: 'title' }, `Zeile ${rowIdx} · Slot ${slotIdx}`),
      h('div', { class: 'group' },
        field('Slot-Label', lbl),
        h('label', { class: 'field' }, h('span', {}, 'Breite'), stepper(slot.width, { min: 1, max: 12, step: 1, fmt: (v) => String(v) }, (v) => { slot.width = v; custom(); renderCanvas(); })),
        h('button', { class: 'btn small', type: 'button', disabled: !slot.cameraId, onclick: () => assign(slot.id, '') }, 'Leeren'),
        h('button', { class: 'btn small danger', type: 'button', disabled: row.slots.length <= 1, onclick: () => {
          const i = row.slots.indexOf(slot);
          row.slots.splice(i, 1);
          selSlot = row.slots[Math.max(0, i - 1)].id;
          custom();
          renderAll();
        } }, 'Slot löschen'),
      ),
      h('div', { class: 'group' },
        h('label', { class: 'field' }, h('span', {}, 'Zeilenhöhe'), stepper(row.height, { min: 0.25, max: 6, step: 0.25, fmt: (v) => v.toFixed(2) }, (v) => { row.height = v; custom(); renderCanvas(); })),
        h('button', { class: 'btn small', type: 'button', disabled: row.slots.length >= 16, onclick: () => {
          const ns = { id: rid('s_'), cameraId: '', width: 1, label: '' };
          row.slots.splice(row.slots.indexOf(slot) + 1, 0, ns);
          selSlot = ns.id;
          custom();
          renderAll();
        } }, '+ Slot'),
        h('button', { class: 'btn small danger', type: 'button', disabled: m.layout.rows.length <= 1, onclick: () => {
          const i = m.layout.rows.indexOf(row);
          m.layout.rows.splice(i, 1);
          const nr = m.layout.rows[Math.max(0, i - 1)];
          selSlot = nr ? nr.slots[0].id : null;
          custom();
          renderAll();
        } }, 'Zeile löschen'),
        addRow,
      ),
    );
  }

  function renderAll() {
    renderPalette();
    renderCanvas();
    renderInspector();
  }

  gapIn.addEventListener('input', () => { m.layout.gap = parseInt(gapIn.value, 10); gapVal.textContent = `${m.layout.gap}px`; renderCanvas(); });
  searchIn.addEventListener('input', renderPalette);
  renderAll();

  openDialog({
    title: isNew ? 'Monitor hinzufügen' : `Layout bearbeiten – ${mon.label}`,
    size: 'xl',
    enterSubmits: false,
    body: h('div', { style: { display: 'flex', flexDirection: 'column', minHeight: '0', flex: '1' } },
      msg,
      h('div', { class: 'row', style: { gap: '14px', alignItems: 'flex-end' } },
        field('Name', labelIn),
        h('div', { class: 'field' }, h('span', {}, 'Status'), enabled),
        h('label', { class: 'field' }, h('span', {}, 'Abstand (Gap)'), h('div', { class: 'row' }, gapIn, gapVal)),
        h('div', { class: 'field' }, h('span', {}, 'Vorlage'), h('div', { class: 'row' }, tplSel, h('button', { class: 'btn small', type: 'button', onclick: async () => {
          if (!await confirmDialog('Vorlage anwenden', 'Das Raster wird durch die Vorlage ersetzt. Bereits zugeordnete Kameras werden der Reihe nach übernommen.', { confirmLabel: 'Anwenden' })) return;
          applyTemplate(ctx, m, tplSel.value);
          gapIn.value = String(m.layout.gap);
          gapVal.textContent = `${m.layout.gap}px`;
          selSlot = m.layout.rows[0].slots[0].id;
          renderAll();
        } }, 'Anwenden'))),
      ),
      h('div', { class: 'dnd' },
        h('div', { class: 'palette' },
          h('div', { class: 'row', style: { gap: '8px', alignItems: 'center' } }, h('strong', {}, 'Kameras'), h('span', { class: 'spacer' }), h('span', { class: 'faint', style: { fontSize: '12px' } }, 'Sortierung'), modeSeg),
          searchIn,
          palList,
        ),
        h('div', { class: 'canvas-wrap' }, h('div', { class: 'canvas-box' }, canvas), inspector),
      ),
    ),
    actions: [
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        setMessage(msg, '');
        m.label = labelIn.value.trim();
        m.enabled = enabled.input.checked;
        const ok = await guarded(() => api.post('/api/settings/monitors', m), msg);
        if (ok === false) return false;
        toast('Monitor gespeichert – das Layout ist sofort aktiv');
        await ctx.reload();
        return true;
      } },
    ],
  });
}

function applyTemplate(ctx, m, id) {
  const t = (ctx.templates || []).find((x) => x.id === id);
  if (!t) return;
  const cams = [];
  for (const r of m.layout.rows || []) for (const s of r.slots) if (s.cameraId) cams.push(s.cameraId);
  let n = 0;
  m.layout.template = t.id;
  m.layout.rows = t.rows.map((count, i) => ({
    id: rid('r_'),
    height: t.heights[i],
    slots: Array.from({ length: count }, () => ({ id: rid('s_'), cameraId: cams[n++] || '', width: 1, label: '' })),
  }));
}
