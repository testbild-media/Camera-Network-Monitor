// Users and kiosk keys.
import api from '../api.js';
import { h, clear, select, field, checkbox, fmtDateTime } from '../dom.js';
import { openDialog, confirmDialog, setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';

const ROLES = [{ value: 'viewer', label: 'Viewer' }, { value: 'admin', label: 'Admin' }];

function monitorName(ctx, id) {
  const m = (ctx.data.monitors || []).find((x) => x.id === id);
  return m ? m.label : `${id} (gelöscht)`;
}

export function renderUsers(root, ctx) {
  const tbody = h('tbody');
  root.append(
    h('div', { class: 'toolbar' }, h('h2', {}, 'Benutzer'), h('button', { class: 'btn primary', onclick: () => editUser(ctx, null) }, 'Benutzer hinzufügen')),
    h('table', { class: 'list' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Benutzer'), h('th', {}, 'Rolle'), h('th', {}, 'Status'), h('th', {}, 'Kiosk-Keys'), h('th', {}, 'Angelegt'), h('th', {}))),
      tbody,
    ),
  );
  for (const u of ctx.data.users || []) {
    const active = u.apiKeys.filter((k) => k.enabled).length;
    const self = u.id === ctx.data.self;
    tbody.appendChild(h('tr', {},
      h('td', {}, u.username, self ? h('span', { class: 'faint' }, ' (du)') : null),
      h('td', {}, h('span', { class: `badge ${u.role === 'admin' ? 'admin' : ''}` }, u.role === 'admin' ? 'Admin' : 'Viewer')),
      h('td', {}, u.enabled ? 'aktiv' : h('span', { class: 'badge off' }, 'inaktiv')),
      h('td', {}, `${active} aktiv${u.apiKeys.length > active ? ` · ${u.apiKeys.length - active} widerrufen` : ''}`),
      h('td', { class: 'faint' }, u.createdAt ? fmtDateTime(new Date(u.createdAt).getTime()) : '–'),
      h('td', { class: 'actions' },
        h('button', { class: 'btn small', onclick: () => manageKeys(ctx, u) }, 'Kiosk-Keys'),
        h('button', { class: 'btn small', onclick: () => editUser(ctx, u) }, 'Bearbeiten'),
        h('button', { class: 'btn small danger', disabled: self, title: self ? 'Der eigene Account kann nicht gelöscht werden' : '', onclick: async () => {
          if (!await confirmDialog('Benutzer löschen', `Benutzer „${u.username}“ und alle Kiosk-Keys löschen? Aktive Sitzungen werden sofort beendet.`, { confirmLabel: 'Löschen', danger: true })) return;
          if (await guarded(() => api.del(`/api/settings/users/${encodeURIComponent(u.id)}`)) !== false) ctx.reload();
        } }, 'Löschen'),
      ),
    ));
  }
  return null;
}

function editUser(ctx, u) {
  const isNew = !u;
  const msg = h('div', { hidden: true });
  const name = h('input', { type: 'text', value: u ? u.username : '', autocomplete: 'off', maxlength: '64' });
  const role = select(ROLES, u ? u.role : 'viewer');
  const enabled = checkbox('Account aktiv', u ? u.enabled : true);
  const pw = h('input', { type: 'password', autocomplete: 'new-password', placeholder: isNew ? 'mindestens 8 Zeichen' : 'leer = unverändert' });
  openDialog({
    title: isNew ? 'Benutzer hinzufügen' : `Benutzer bearbeiten – ${u.username}`,
    body: h('div', {}, msg, h('div', { class: 'form-grid' },
      field('Benutzername', name),
      field('Rolle', role),
      field(isNew ? 'Passwort' : 'Neues Passwort', pw),
      h('div', { class: 'field' }, h('span', {}, 'Status'), enabled),
    )),
    actions: [
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        setMessage(msg, '');
        const body = { username: name.value.trim(), role: role.value, enabled: enabled.input.checked };
        if (pw.value) body.password = pw.value;
        if (isNew && pw.value.length < 8) { setMessage(msg, 'Das Passwort muss mindestens 8 Zeichen lang sein.'); return false; }
        const ok = await guarded(() => (isNew ? api.post('/api/settings/users', body) : api.put(`/api/settings/users/${encodeURIComponent(u.id)}`, body)), msg);
        if (ok === false) return false;
        toast('Benutzer gespeichert');
        ctx.reload();
        return true;
      } },
    ],
  });
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // clipboard API needs a secure context; fall back to a temporary textarea
    const ta = h('textarea', { style: { position: 'fixed', opacity: '0' } }, text);
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

function urlBox(path) {
  const full = new URL(path, location.origin).toString();
  const input = h('input', { type: 'text', value: full, readonly: true });
  input.addEventListener('focus', () => input.select());
  return h('div', { class: 'keyurl' }, input, h('button', { class: 'btn', onclick: async () => {
    toast(await copyText(full) ? 'Kiosk-URL kopiert' : 'Kopieren nicht möglich – bitte manuell markieren', '');
  } }, 'Kopieren'));
}

function manageKeys(ctx, u) {
  const list = h('div');
  const created = h('div', { hidden: true, style: { marginTop: '12px' } });
  const label = h('input', { type: 'text', placeholder: 'z. B. Empfang Bildschirm', maxlength: '64' });
  const mons = (ctx.data.monitors || []).map((m) => ({ value: m.id, label: m.label }));
  const mon = select(mons, mons[0] && mons[0].value);
  const msg = h('div', { hidden: true });
  let keys = u.apiKeys.slice();

  function render() {
    clear(list);
    if (!keys.length) { list.appendChild(h('p', { class: 'muted' }, 'Noch keine Kiosk-Keys.')); return; }
    list.appendChild(h('table', { class: 'list compact' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Label'), h('th', {}, 'Monitor'), h('th', {}, 'Angelegt'), h('th', {}, 'Status'), h('th', {}))),
      h('tbody', {}, keys.map((k) => h('tr', {},
        h('td', {}, k.label),
        h('td', {}, monitorName(ctx, k.monitorId)),
        h('td', { class: 'faint' }, k.createdAt ? fmtDateTime(new Date(k.createdAt).getTime()) : '–'),
        h('td', {}, k.enabled ? 'aktiv' : h('span', { class: 'badge off' }, 'widerrufen')),
        h('td', { class: 'actions' },
          k.enabled && k.recoverable ? h('button', { class: 'btn small', onclick: async () => {
            const r = await guarded(() => api.get(`/api/settings/users/${encodeURIComponent(u.id)}/api-keys/${encodeURIComponent(k.id)}/reveal`));
            if (!r) return;
            const full = new URL(r.path, location.origin).toString();
            toast(await copyText(full) ? 'Kiosk-URL kopiert' : 'Kopieren nicht möglich');
          } }, 'URL kopieren') : null,
          k.enabled && !k.recoverable ? h('span', { class: 'faint', title: 'Alter Key: nur als Hash gespeichert' }, 'nicht wiederherstellbar ') : null,
          k.enabled ? h('button', { class: 'btn small danger', onclick: async () => {
            if (!await confirmDialog('Key widerrufen', `Kiosk-Key „${k.label}“ widerrufen? Laufende Kiosk-Sitzungen und Streams werden sofort beendet.`, { confirmLabel: 'Widerrufen', danger: true })) return;
            if (await guarded(() => api.del(`/api/settings/users/${encodeURIComponent(u.id)}/api-keys/${encodeURIComponent(k.id)}`)) === false) return;
            k.enabled = false;
            k.recoverable = false;
            toast('Key widerrufen');
            render();
          } }, 'Widerrufen') : h('button', { class: 'btn small', onclick: async () => {
            if (await guarded(() => api.del(`/api/settings/users/${encodeURIComponent(u.id)}/api-keys/${encodeURIComponent(k.id)}?purge=1`)) === false) return;
            keys = keys.filter((x) => x.id !== k.id);
            render();
          } }, 'Entfernen'),
        ),
      ))),
    ));
  }
  render();
  openDialog({
    title: `Kiosk-Keys – ${u.username}`,
    size: 'medium',
    onClose: () => ctx.reload(),
    body: h('div', {},
      h('p', { class: 'muted' }, 'Ein Kiosk-Key öffnet genau einen Monitor mit Viewer-Rechten – auch wenn der Besitzer Admin ist.'),
      list,
      h('div', { class: 'section-title' }, 'Neuen Key erzeugen'),
      msg,
      h('div', { class: 'form-grid' }, field('Label', label), field('Monitor', mon)),
      h('div', { style: { marginTop: '10px' } }, h('button', { class: 'btn primary', disabled: !mons.length, onclick: async () => {
        setMessage(msg, '');
        const r = await guarded(() => api.post(`/api/settings/users/${encodeURIComponent(u.id)}/api-keys`, { label: label.value, monitorId: mon.value }), msg);
        if (!r) return;
        keys.push(r.key);
        render();
        clear(created);
        created.hidden = false;
        created.append(h('div', { class: 'msg ok' }, 'Key erzeugt. Diese URL im Kiosk-Browser öffnen:'), h('div', { style: { marginTop: '6px' } }, urlBox(r.path)));
        label.value = '';
      } }, 'Key erzeugen')),
      created,
    ),
    actions: [{ label: 'Schließen' }],
  });
}
