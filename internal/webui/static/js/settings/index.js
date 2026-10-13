// Settings shell. Entering the settings lands on "Monitore / Layouts" unless
// a tab is named (#/settings/<tab>, e.g. the user's startup screen).
// Sections render once when opened; live device status updates patch
// individual cells only, so forms being edited are never rebuilt.
import api from '../api.js';
import store from '../store.js';
import { h, clear, loadingBox } from '../dom.js';
import { alertDialog } from '../dialog.js';
import { renderMonitors } from './monitors.js';
import { renderCameras } from './cameras.js';
import { renderGroups } from './groups.js';
import { SETTINGS_TABS } from './tabs.js';
import { renderDevices } from './devices.js';
import { renderDisplays } from './displays.js';
import { renderUsers } from './users.js';
import { renderSystem } from './system.js';

const RENDER = {
  monitors: renderMonitors,
  cameras: renderCameras,
  groups: renderGroups,
  devices: renderDevices,
  displays: renderDisplays,
  users: renderUsers,
  system: renderSystem,
};
const TABS = SETTINGS_TABS.map(([id, label]) => [id, label, RENDER[id]]);

export function mountSettings(container, initialTab) {
  const tabBar = h('nav', { class: 'tabs' });
  const body = h('div', { class: 'settings-body' });
  const root = h('div', { class: 'settings' }, tabBar, body);
  container.appendChild(root);
  let current = null;
  let active = '';

  const ctx = {
    data: null,
    protocols: null,
    templates: null,
    async load() {
      const [data, protocols, templates] = await Promise.all([
        api.get('/api/settings'),
        ctx.protocols ? Promise.resolve(ctx.protocols) : api.get('/api/protocols'),
        ctx.templates ? Promise.resolve(ctx.templates) : api.get('/api/layout-templates'),
      ]);
      ctx.data = data;
      ctx.protocols = protocols || [];
      ctx.templates = templates || [];
      for (const d of data.devices || []) if (d.status) store.devices.set(d.id, d.status);
      return data;
    },
    async reload() {
      await ctx.load();
      show(active, true);
    },
    show: (id) => show(id),
    cameraName(cam) {
      if (!cam) return '–';
      if (cam.label) return cam.label;
      if (cam.deviceName) return cam.deviceName;
      return `${cam.deviceLabel} · Kanal ${cam.channel + 1}`;
    },
    camera(id) { return (ctx.data.cameras || []).find((c) => c.id === id); },
  };

  async function show(id, force) {
    if (!TABS.some((t) => t[0] === id)) id = 'monitors';
    if (id === active && !force) return;
    active = id;
    // keep the URL in sync for reloads without triggering the router
    if (location.hash !== `#/settings/${id}`) history.replaceState(null, '', `#/settings/${id}`);
    for (const b of tabBar.children) b.classList.toggle('active', b.dataset.tab === id);
    if (current && current.destroy) current.destroy();
    current = null;
    const inner = h('div', { class: 'inner' });
    if (!ctx.data) {
      clear(body);
      body.appendChild(inner);
      inner.appendChild(loadingBox('Einstellungen werden geladen …'));
      try { await ctx.load(); } catch (e) { clear(inner); inner.appendChild(h('div', { class: 'msg error' }, e.message)); return; }
      if (active !== id) return;
    }
    clear(inner);
    clear(body);
    body.appendChild(inner);
    const tab = TABS.find((t) => t[0] === id) || TABS[0];
    current = tab[2](inner, ctx) || null;
  }

  for (const [id, label] of TABS) {
    tabBar.appendChild(h('button', { dataset: { tab: id }, onclick: () => show(id) }, label));
  }
  const unsub = store.on('device', (st) => { if (current && current.onDevice) current.onDevice(st); });
  show(initialTab || 'monitors');

  return {
    show: (id) => show(id),
    destroy() {
      unsub();
      if (current && current.destroy) current.destroy();
      root.remove();
    },
  };
}

// shared helper for save handlers
export async function guarded(fn, msgEl) {
  try {
    return await fn();
  } catch (e) {
    if (msgEl) {
      msgEl.className = 'msg error';
      msgEl.hidden = false;
      msgEl.textContent = e.message || 'Fehler';
    } else {
      await alertDialog('Fehler', e.message || 'Fehler', 'error');
    }
    return false;
  }
}
