// Camera groups: create, rename, delete and edit the members of a group. A
// camera may belong to several groups; membership changes are saved per
// click without rebuilding the tab.
import api from '../api.js';
import { h, clear, field, busy } from '../dom.js';
import { openDialog, confirmDialog, toast } from '../dialog.js';
import { guarded } from './index.js';

let selectedGroup = ''; // kept while the settings page lives (also across reloads of the tab)

export function renderGroups(root, ctx) {
  const list = h('div', { class: 'group-list' });
  const detail = h('div', { class: 'group-detail' });
  const search = h('input', { type: 'search', placeholder: 'Kameras suchen …' });
  const onlyMembers = h('input', { type: 'checkbox' });
  root.append(
    h('div', { class: 'toolbar' },
      h('h2', {}, 'Kamera-Gruppen'),
      h('span', { class: 'faint' }, 'Gruppen ordnen Kameras unabhängig vom Gerät; eine Kamera kann mehreren Gruppen angehören.'),
      h('button', { class: 'btn primary', onclick: () => editGroup(ctx, null, (g) => { if (g && g.id) selectedGroup = g.id; }) }, '+ Gruppe anlegen'),
    ),
    h('div', { class: 'groups-layout' }, list, detail),
  );

  const groups = () => ctx.data.groups || [];
  const cameras = () => ctx.data.cameras || [];
  const group = () => groups().find((g) => g.id === selectedGroup);

  function renderList() {
    clear(list);
    if (!groups().length) {
      list.appendChild(h('p', { class: 'muted' }, 'Noch keine Gruppen.'));
    }
    for (const g of groups()) {
      list.appendChild(h('button', { class: `group-item${g.id === selectedGroup ? ' active' : ''}`, onclick: () => { selectedGroup = g.id; renderList(); renderDetail(); } },
        h('span', { class: 'nm' }, g.label), h('span', { class: 'n' }, String(g.cameraIds.length))));
    }
    const none = cameras().filter((c) => !c.groups.length).length;
    list.appendChild(h('div', { class: 'faint', style: { marginTop: '10px', fontSize: '12px' } }, `${none} Kamera${none === 1 ? '' : 's'} ohne Gruppe`));
  }

  // membership toggles update the local data immediately (no tab rebuild)
  async function setMember(g, cams, member) {
    const ids = cams.map((c) => c.id);
    const ok = await guarded(() => api.post('/api/settings/cameras/bulk', { ids, [member ? 'addGroup' : 'removeGroup']: g.id }));
    if (ok === false) return false;
    const set = new Set(g.cameraIds);
    for (const c of cams) {
      if (member) { set.add(c.id); if (!c.groups.includes(g.id)) c.groups.push(g.id); } else { set.delete(c.id); c.groups = c.groups.filter((x) => x !== g.id); }
    }
    g.cameraIds = [...set];
    return true;
  }

  function camName(c) { return ctx.cameraName(c); }

  function renderDetail() {
    clear(detail);
    const g = group();
    if (!g) {
      detail.appendChild(h('p', { class: 'muted' }, groups().length ? 'Gruppe links auswählen, um ihre Kameras zu bearbeiten.' : 'Mit „+ Gruppe anlegen“ die erste Gruppe erstellen.'));
      return;
    }
    const q = search.value.trim().toLowerCase();
    const members = new Set(g.cameraIds);
    const shown = cameras().filter((c) => {
      if (onlyMembers.checked && !members.has(c.id)) return false;
      if (!q) return true;
      return `${camName(c)} ${c.deviceLabel} ${c.deviceName || ''} kanal ${c.channel + 1}`.toLowerCase().includes(q);
    });
    const byDev = new Map();
    for (const c of shown) {
      if (!byDev.has(c.deviceId)) byDev.set(c.deviceId, { label: c.deviceLabel, cams: [] });
      byDev.get(c.deviceId).cams.push(c);
    }
    const count = h('span', { class: 'faint' }, `${g.cameraIds.length} Kamera${g.cameraIds.length === 1 ? '' : 's'}`);
    const addAll = h('button', { class: 'btn small', disabled: !shown.some((c) => !members.has(c.id)), onclick: (e) => busy(e.currentTarget, async () => {
      if (await setMember(g, shown.filter((c) => !members.has(c.id)), true)) { toast('Kameras hinzugefügt'); renderList(); renderDetail(); }
    }) }, 'Alle angezeigten hinzufügen');
    const rmAll = h('button', { class: 'btn small', disabled: !shown.some((c) => members.has(c.id)), onclick: (e) => busy(e.currentTarget, async () => {
      if (await setMember(g, shown.filter((c) => members.has(c.id)), false)) { toast('Kameras entfernt'); renderList(); renderDetail(); }
    }) }, 'Alle angezeigten entfernen');
    const rows = [];
    for (const { label, cams } of byDev.values()) {
      rows.push(h('div', { class: 'dev-head' }, label));
      for (const c of cams) {
        const cb = h('input', { type: 'checkbox', checked: members.has(c.id) });
        cb.addEventListener('change', async () => {
          cb.disabled = true;
          if (!await setMember(g, [c], cb.checked)) cb.checked = !cb.checked;
          cb.disabled = false;
          renderList();
          count.textContent = `${g.cameraIds.length} Kamera${g.cameraIds.length === 1 ? '' : 's'}`;
        });
        rows.push(h('label', { class: 'member' }, cb,
          h('span', { class: 'nm' }, camName(c)),
          h('span', { class: 'faint' }, `Kanal ${c.channel + 1}`),
          c.enabled ? null : h('span', { class: 'badge off' }, 'deaktiviert'),
          c.groups.filter((x) => x !== g.id).length ? h('span', { class: 'faint', title: 'weitere Gruppen' }, `+${c.groups.filter((x) => x !== g.id).length}`) : null));
      }
    }
    detail.append(
      h('div', { class: 'row', style: { gap: '10px', marginBottom: '10px' } },
        h('h3', { style: { margin: 0 } }, g.label), count,
        h('span', { class: 'spacer' }),
        h('button', { class: 'btn small', onclick: () => editGroup(ctx, g) }, 'Umbenennen'),
        h('button', { class: 'btn small danger', onclick: async (e) => {
          const btn = e.currentTarget;
          if (!await confirmDialog('Gruppe löschen', `Gruppe „${g.label}“ löschen? Die Kameras bleiben erhalten.`, { confirmLabel: 'Löschen', danger: true })) return;
          await busy(btn, async () => {
            if (await guarded(() => api.del(`/api/settings/groups/${encodeURIComponent(g.id)}`)) !== false) { selectedGroup = ''; await ctx.reload(); }
          });
        } }, 'Löschen'),
      ),
      h('div', { class: 'row', style: { gap: '10px', marginBottom: '8px', flexWrap: 'wrap' } },
        search,
        h('label', { class: 'check' }, onlyMembers, ' nur Mitglieder'),
        h('span', { class: 'spacer' }),
        addAll, rmAll,
      ),
      h('div', { class: 'member-list' }, rows.length ? rows : h('p', { class: 'muted' }, 'Keine Kamera passt zur Suche.')),
    );
  }

  search.addEventListener('input', renderDetail);
  onlyMembers.addEventListener('change', renderDetail);
  if (!group()) selectedGroup = groups().length ? groups()[0].id : '';
  renderList();
  renderDetail();
  return null;
}

function editGroup(ctx, g, after) {
  const msg = h('div', { hidden: true });
  const name = h('input', { type: 'text', value: g ? g.label : '', maxlength: '64', placeholder: 'z. B. Außenbereich' });
  openDialog({
    title: g ? 'Gruppe umbenennen' : 'Gruppe anlegen',
    body: h('div', {}, msg, field('Name', name)),
    actions: [
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        const saved = await guarded(() => api.post('/api/settings/groups', { id: g ? g.id : '', label: name.value }), msg);
        if (saved === false) return false;
        if (after) after(saved);
        await ctx.reload();
        return true;
      } },
    ],
  });
}
