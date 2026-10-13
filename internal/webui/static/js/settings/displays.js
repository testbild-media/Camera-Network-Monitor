// Local displays: the server's own GPU outputs show multiviewers as kiosks.
// Per port: multiviewer, resolution (from EDID / compositor) and rotation.
import api from '../api.js';
import { h, clear, select, field, busy, loadingBox, fmtAgo } from '../dom.js';
import { confirmDialog, setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';

const ROTATIONS = [
  { value: '0', label: 'Normal' },
  { value: '90', label: '90° (hochkant)' },
  { value: '180', label: '180°' },
  { value: '270', label: '270° (hochkant)' },
];

const BROWSERS = [
  { value: 'auto', label: 'Automatisch (Firefox, sonst Chromium)' },
  { value: 'chromium', label: 'Chromium' },
  { value: 'firefox', label: 'Firefox' },
];

const STATE_TEXT = {
  running: 'Läuft',
  starting: 'Browser startet …',
  restarting: 'Neustart in Kürze',
  'no-output': 'Kein Bildschirm angeschlossen',
  off: 'Aus',
  error: 'Fehler',
};

export function renderDisplays(root, ctx) {
  const agentBox = h('div', { class: 'disp-agent' });
  const optsBox = h('div', { class: 'disp-opts' });
  const gpuBox = h('div', { class: 'faint', style: { margin: '-6px 0 12px' }, hidden: true });
  const grid = h('div', { class: 'disp-grid' });
  const empty = h('div', { hidden: true });
  root.append(
    h('div', { class: 'toolbar' },
      h('h2', {}, 'Displays'),
      h('span', { class: 'faint' }, 'Grafikausgänge dieses Servers zeigen Multiviewer direkt an – ohne Maus und Tastatur.'),
    ),
    agentBox, gpuBox, optsBox, empty, grid,
  );
  grid.appendChild(loadingBox('Grafikausgänge werden ermittelt …'));

  let data = null;
  let alive = true;
  let timer = null;
  const cards = new Map(); // connector → card api

  function monitorOptions() {
    const opts = [{ value: '', label: '– Aus –' }];
    for (const m of data.monitors) opts.push({ value: m.id, label: m.enabled ? m.label : `${m.label} (deaktiviert)` });
    return opts;
  }

  function renderAgent() {
    const a = data.agent || {};
    clear(agentBox);
    const st = a.compositor ? a.compositor.state : '';
    const healthy = st === 'running' || (st === 'idle' && !(a.compositor && a.compositor.error));
    agentBox.className = `disp-agent ${a.connected && healthy ? 'ok' : 'bad'}`;
    if (a.connected) {
      const comp = a.compositor || {};
      const b = a.browser || {};
      const parts = [];
      if (comp.version) parts.push(`sway ${comp.version}`);
      if (b.version) parts.push(b.version); else if (b.kind) parts.push(b.kind);
      agentBox.append(
        h('span', { class: `status ${comp.state === 'running' ? 'connected' : 'error'}` }, h('span', { class: 'dot' })),
        h('div', { class: 'grow' },
          h('div', { class: 'title' }, comp.state === 'running' ? 'Display-Dienst aktiv'
            : comp.state === 'idle' ? 'Display-Dienst bereit – kein Multiviewer zugewiesen, die Konsole am Server bleibt frei'
              : 'Display-Dienst: Anzeige gestört'),
          h('div', { class: 'faint' }, parts.join(' · ') || '–'),
          comp.error ? h('div', { class: 'msg error', style: { marginTop: '6px' } }, comp.error) : null,
          b.error ? h('div', { class: 'msg error', style: { marginTop: '6px' } }, b.error) : null,
        ),
      );
    } else {
      agentBox.append(
        h('span', { class: 'status error' }, h('span', { class: 'dot' })),
        h('div', { class: 'grow' },
          h('div', { class: 'title' }, 'Display-Dienst nicht verbunden'),
          h('div', { class: 'disp-help' },
            'Die Ausgabe übernimmt der Dienst ', h('code', {}, 'camera-network-monitor-display'),
            '. Einrichtung einmalig mit ', h('code', {}, 'sudo ./install.sh --with-display'),
            ', Status mit ', h('code', {}, 'systemctl status camera-network-monitor-display'), '.',
            a.lastSeen ? ` Zuletzt gemeldet ${fmtAgo(a.lastSeen)}.` : '',
            ' Einstellungen lassen sich trotzdem schon vornehmen.'),
        ),
      );
    }
  }

  // one fixed line (updated in place – never appended again on refresh)
  function renderGpu() {
    gpuBox.hidden = !data.gpu;
    if (!data.gpu) return;
    const card = (data.ports || []).find((p) => p.card);
    gpuBox.textContent = `Grafikkarte für die Displays: ${card ? card.card + ' · ' : ''}PCI ${data.gpu} (Auswahl im Setup; andere Karten, z. B. die Konsole, bleiben unberührt)`;
  }

  function renderOptions() {
    clear(optsBox);
    const o = data.options || {};
    // only installed browsers can be chosen (the display service reports them)
    const inst = data.agent && data.agent.connected && Array.isArray(data.agent.installed) ? data.agent.installed : null;
    const browserSel = select(BROWSERS.map((b) => (inst && b.value !== 'auto' && !inst.includes(b.value)
      ? { ...b, label: `${b.label} – nicht installiert`, disabled: true } : b)), o.browser || 'auto');
    const daily = h('input', { type: 'time', value: o.dailyRestart || '' });
    const msg = h('div', { hidden: true });
    const btn = h('button', { class: 'btn', onclick: () => busy(btn, async () => {
      setMessage(msg, '');
      const r = await guarded(() => api.post('/api/settings/displays/options', { browser: browserSel.value, dailyRestart: daily.value }), msg);
      if (r !== false) { data.options = r; toast('Display-Optionen gespeichert'); }
    }) }, 'Übernehmen');
    optsBox.append(
      field('Browser', browserSel),
      field('Tägliche Auffrischung', daily, 'leer = aus; lädt alle Anzeigen kurz neu'),
      btn, msg,
    );
  }

  function liveLine(port) {
    const a = data.agent || {};
    const st = port.status;
    const d = port.display;
    const el = h('div', { class: 'live' });
    if (!d || !d.enabled) {
      el.append(port.connected ? 'Nicht zugewiesen – Ausgang ist aus' : (port.detected ? 'Kein Bildschirm angeschlossen' : 'Anschluss derzeit nicht erkannt'));
      return el;
    }
    if (!a.connected) {
      el.append('Wird angezeigt, sobald der Display-Dienst läuft');
      return el;
    }
    if (!st) {
      el.append(h('span', { class: 'spin' }), 'Wird übernommen …');
      return el;
    }
    if (st.state === 'starting') el.append(h('span', { class: 'spin' }));
    else el.append(h('span', { class: `status ${st.state === 'running' ? 'connected' : st.state === 'error' || st.state === 'restarting' ? 'error' : 'disconnected'}` }, h('span', { class: 'dot' })));
    const parts = [STATE_TEXT[st.state] || st.state];
    if (st.state === 'running' && port.output && port.output.current) parts.push(modeLabel(port.output.current));
    if (st.state === 'running') parts.push(port.viewerConnected ? 'mit CNM verbunden' : 'verbindet …');
    if (st.restarts) parts.push(`${st.restarts}× neu gestartet`);
    el.append(parts.join(' · '));
    if (st.error) {
      el.classList.add('err');
      el.append(h('span', {}, ` – ${st.error}`));
    }
    return el;
  }

  function buildCard(port) {
    const d = port.display || { id: '', label: '', enabled: false, monitorId: '', mode: '', rotation: 0 };
    const monSel = select(monitorOptions(), d.enabled ? d.monitorId : '');
    const prefMode = port.modes.find((m) => m.preferred);
    const modeOpts = [{ value: '', label: prefMode ? `Automatisch (${prefMode.label})` : 'Automatisch (bevorzugte Auflösung)' }];
    for (const m of port.modes) modeOpts.push({ value: m.value, label: m.label + (m.preferred ? ' ★' : '') });
    if (d.mode && !port.modes.some((m) => m.value === d.mode)) modeOpts.push({ value: d.mode, label: `${d.mode} (gespeichert)` });
    const modeSel = select(modeOpts, d.mode || '');
    const rotSel = select(ROTATIONS, String(d.rotation || 0));
    const labelIn = h('input', { type: 'text', value: d.label || '', placeholder: 'optional, z. B. Wand links', maxlength: '80' });
    const msg = h('div', { hidden: true });
    let live = liveLine(port);
    let dirty = false;
    for (const el of [monSel, modeSel, rotSel, labelIn]) el.addEventListener('input', () => { dirty = true; saveBtn.classList.add('primary'); });

    const saveBtn = h('button', { class: 'btn' }, 'Speichern');
    saveBtn.addEventListener('click', () => busy(saveBtn, async () => {
      setMessage(msg, '');
      const body = {
        connector: port.connector, label: labelIn.value.trim(), enabled: !!monSel.value,
        monitorId: monSel.value, mode: modeSel.value, rotation: parseInt(rotSel.value, 10) || 0,
      };
      const r = await guarded(() => api.post('/api/settings/displays', body), msg);
      if (r === false) return;
      dirty = false;
      saveBtn.classList.remove('primary');
      toast(body.enabled ? `${port.connector}: wird übernommen` : `${port.connector}: Ausgang aus`);
      await refresh(true);
    }));
    const identBtn = h('button', { class: 'btn', title: 'Zeigt den Anschlussnamen 10 Sekunden groß auf diesem Bildschirm' }, 'Identifizieren');
    identBtn.addEventListener('click', () => busy(identBtn, async () => {
      if (await guarded(() => api.post(`/api/settings/displays/${encodeURIComponent(port.display.id)}/identify`), msg) !== false) toast(`${port.connector}: Kennung wird angezeigt`);
    }));
    const reloadBtn = h('button', { class: 'btn', title: 'Startet die Anzeige auf diesem Ausgang neu' }, 'Neu laden');
    reloadBtn.addEventListener('click', () => busy(reloadBtn, async () => {
      if (await guarded(() => api.post(`/api/settings/displays/${encodeURIComponent(port.display.id)}/reload`), msg) !== false) toast(`${port.connector}: Anzeige wird neu geladen`);
    }));
    const removeBtn = h('button', { class: 'btn danger', title: 'Eintrag für diesen nicht mehr vorhandenen Anschluss entfernen' }, 'Entfernen');
    removeBtn.addEventListener('click', async () => {
      if (!await confirmDialog('Display entfernen', `Den Eintrag für ${port.connector} entfernen?`, { confirmLabel: 'Entfernen', danger: true })) return;
      await busy(removeBtn, async () => {
        if (await guarded(() => api.del(`/api/settings/displays/${encodeURIComponent(port.display.id)}`), msg) !== false) await refresh(true);
      });
    });

    const badge = h('span', { class: `badge ${port.connected ? '' : 'off'}` }, port.connected ? 'angeschlossen' : (port.detected ? 'kein Bildschirm' : 'nicht erkannt'));
    const head = h('div', { class: 'head' },
      h('div', {},
        h('div', { class: 'port' }, port.connector),
        h('div', { class: 'screen' }, [port.screen, port.serial ? `SN ${port.serial}` : '', port.card].filter(Boolean).join(' · ') || '–'),
      ),
      h('span', { class: 'spacer' }),
      badge,
    );
    const actions = h('div', { class: 'actions' });
    const el = h('div', { class: 'disp-card' }, head, live,
      h('div', { class: 'form-grid' },
        field('Multiviewer', monSel),
        field('Auflösung', modeSel, port.modes.length ? null : 'Keine Modi bekannt – bevorzugte Auflösung wird verwendet'),
        field('Drehung', rotSel),
        field('Bezeichnung', labelIn),
      ),
      msg, actions,
    );
    function updateActions(p) {
      clear(actions);
      const configured = !!(p.display && p.display.id);
      const on = configured && p.display.enabled;
      identBtn.disabled = !on || !p.viewerConnected;
      reloadBtn.disabled = !on || !(data.agent && data.agent.connected);
      if (configured && !p.detected) actions.append(removeBtn);
      actions.append(h('span', { class: 'spacer' }), identBtn, reloadBtn, saveBtn);
      el.classList.toggle('off', !on);
    }
    updateActions(port);
    return {
      el,
      get dirty() { return dirty; },
      update(p) {
        port = p;
        const next = liveLine(p);
        live.replaceWith(next);
        live = next;
        badge.className = `badge ${p.connected ? '' : 'off'}`;
        badge.textContent = p.connected ? 'angeschlossen' : (p.detected ? 'kein Bildschirm' : 'nicht erkannt');
        updateActions(p);
      },
      sig: JSON.stringify([port.display, port.modes.map((m) => m.value), port.screen]),
    };
  }

  function render(force) {
    renderAgent();
    renderGpu();
    if (force || !optsBox.childNodes.length) renderOptions();
    const ports = data.ports || [];
    empty.hidden = ports.length > 0;
    if (!ports.length) {
      clear(grid);
      cards.clear();
      clear(empty);
      empty.append(h('div', { class: 'msg' },
        data.supported === false && !(data.agent && data.agent.connected)
          ? 'Auf diesem Server wurden keine Grafikausgänge gefunden (keine GPU, virtuelle Maschine oder kein Linux). Displays lassen sich nur an Servern mit Grafikkarte und angeschlossenem Bildschirm nutzen.'
          : 'Keine Grafikausgänge gefunden.'));
      return;
    }
    // rebuild only cards whose structure changed and that are not being edited
    const want = new Set(ports.map((p) => p.connector));
    for (const [k, c] of cards) if (!want.has(k) && !c.dirty) { c.el.remove(); cards.delete(k); }
    if (grid.querySelector('.loading-box')) clear(grid);
    for (const p of ports) {
      const c = cards.get(p.connector);
      const sig = JSON.stringify([p.display, p.modes.map((m) => m.value), p.screen]);
      if (c && (c.sig === sig || c.dirty) && !force) { c.update(p); continue; }
      if (c && c.dirty && force) { c.update(p); continue; }
      const nc = buildCard(p);
      if (c) c.el.replaceWith(nc.el); else grid.appendChild(nc.el);
      cards.set(p.connector, nc);
    }
    // keep the server's order
    for (const p of ports) grid.appendChild(cards.get(p.connector).el);
  }

  async function refresh(force) {
    try {
      data = await api.get('/api/settings/displays', { background: !force });
      if (alive) render(force);
    } catch (e) {
      if (alive && !data) { clear(grid); grid.appendChild(h('div', { class: 'msg error' }, e.message)); }
    }
  }

  async function loop() {
    if (!alive) return;
    if (!document.hidden) await refresh(false);
    timer = setTimeout(loop, 4000);
  }
  refresh(true).then(() => { timer = setTimeout(loop, 4000); });

  return {
    destroy() { alive = false; clearTimeout(timer); },
  };
}

function modeLabel(m) {
  if (!m) return '';
  const hz = m.refresh ? ` @ ${String(Math.round(m.refresh / 10) / 100).replace(/\.?0+$/, '')} Hz` : '';
  return `${m.width} × ${m.height}${hz}`;
}
