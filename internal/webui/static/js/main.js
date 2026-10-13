// Application entry: authentication flows, app shell, kiosk mode.
import api from './api.js';
import store from './store.js';
import { h, clear, busy } from './dom.js';
import { Control } from './control.js';
import { MonitorView } from './multiview.js';
import poller from './health.js';
import { openFocus, closeFocus, suspendFocus, resumeFocus } from './focus.js';
import { openDialog, setMessage, toast } from './dialog.js';
import { SETTINGS_TABS } from './settings/tabs.js';

const KIOSK = /^\/viewer\/?$/.test(location.pathname);
const app = document.getElementById('app');
const params = new URLSearchParams(location.search);
const kioskMon = KIOSK ? params.get('mon') || '' : '';
const kioskKey = KIOSK ? params.get('key') || '' : '';
// local display output (server GPU port): the display agent finds the window
// by its title; no pointer is shown
const kioskDisplay = KIOSK ? (params.get('display') || '').replace(/[^\w.-]/g, '').slice(0, 32) : '';
if (kioskDisplay) document.documentElement.classList.add('display-output');

function viewerTitle(name) {
  return kioskDisplay ? `CNM-DISPLAY ${kioskDisplay} – ${name}` : `${name} – Viewer`;
}

let identifyTimer = null;
function showIdentify(msg) {
  const old = document.querySelector('.identify');
  if (old) old.remove();
  clearTimeout(identifyTimer);
  const el = h('div', { class: 'identify' }, h('div', { class: 'big' }, msg.text || kioskDisplay || 'Display'), h('div', {}, 'Camera Network Monitor'));
  document.body.appendChild(el);
  identifyTimer = setTimeout(() => el.remove(), Math.min(Math.max(msg.ms || 10000, 1000), 60000));
}

let control = null;
let view = null;
let settings = null;
let shell = null;
let overlay = null;
let unsubs = [];

function storageGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function storageSet(k, v) { try { localStorage.setItem(k, v); } catch { /* ignore */ } }

// "#/settings" or "#/settings/<tab>" → { tab } (tab may be ''), else null
function settingsRoute() {
  const m = /^#\/settings(?:\/([a-z]+))?$/.exec(location.hash);
  return m ? { tab: m[1] || '' } : null;
}

// Startup screen (per user, validated by the server): applied once per app
// start and only without an explicit target in the URL – so it can never
// cause a redirect loop or override a deep link.
function applyStartupScreen(prefs) {
  // a settings link is no target for users without settings access
  if (settingsRoute() && !store.isAdmin()) history.replaceState(null, '', location.pathname + location.search);
  const target = (prefs && prefs.startupScreen) || '';
  if (!target || (location.hash && location.hash !== '#' && location.hash !== '#/')) return;
  const [kind, id] = target.split(':');
  if (kind === 'monitor') storageSet('cnm.monitor', id);
  else if (kind === 'settings' && store.isAdmin() && SETTINGS_TABS.some((t) => t[0] === id)) {
    history.replaceState(null, '', `#/settings/${id}`);
  }
}

// ---- overlays ---------------------------------------------------------------------------------------

function showOverlay(kind, text, sub) {
  hideOverlay();
  overlay = h('div', { class: `overlay ${kind}` }, h('div', { class: 'box' }, h('div', { class: 'big' }, text), sub ? h('div', { class: 'muted' }, sub) : null));
  document.body.appendChild(overlay);
}

function hideOverlay() {
  if (overlay) { overlay.remove(); overlay = null; }
}

// ---- teardown (logout / revoke / blocked) – no hidden background work -------------

function teardown() {
  if (control) { control.stop(); control = null; }
  poller.stop();
  closeFocus();
  if (settings) { settings.destroy(); settings = null; }
  if (view) { view.destroy(); view = null; }
  for (const u of unsubs) u();
  unsubs = [];
  window.removeEventListener('hashchange', route);
  shell = null;
  clear(app);
}

function showBlocked() {
  teardown();
  showOverlay('blocked', 'ZUGRIFF GESPERRT');
}

// ---- auth screens -------------------------------------------------------------------------------------

function authBox(title, intro, fields, submitLabel, onSubmit) {
  const msg = h('div', { hidden: true });
  const btn = h('button', { class: 'btn primary', type: 'submit' }, submitLabel);
  const form = h('form', { class: 'auth' },
    h('div', { class: 'logo' }, h('img', { src: '/assets/logo.svg', alt: '' }), h('div', {}, h('h1', {}, title), h('div', { class: 'muted' }, 'Camera Network Monitor'))),
    intro ? h('p', { class: 'muted' }, intro) : null,
    ...fields,
    msg,
    btn,
  );
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    setMessage(msg, '');
    await busy(btn, async () => {
      try { await onSubmit(msg); } catch (err) { setMessage(msg, err.message || 'Fehler'); }
    });
  });
  clear(app);
  app.appendChild(form);
  const first = form.querySelector('input');
  if (first) first.focus();
}

function showLogin(note) {
  teardown();
  const user = h('input', { type: 'text', autocomplete: 'username', required: true });
  const pass = h('input', { type: 'password', autocomplete: 'current-password', required: true });
  authBox('Anmelden', note, [
    h('label', { class: 'field' }, h('span', {}, 'Benutzername'), user),
    h('label', { class: 'field' }, h('span', {}, 'Passwort'), pass),
  ], 'Anmelden', async () => {
    const me = await api.post('/api/auth/login', { username: user.value, password: pass.value }, { quietAuth: true });
    startApp(me);
  });
}

function showBootstrap() {
  teardown();
  const user = h('input', { type: 'text', autocomplete: 'username', value: 'admin', required: true });
  const pass = h('input', { type: 'password', autocomplete: 'new-password', minlength: '8', required: true });
  const pass2 = h('input', { type: 'password', autocomplete: 'new-password', minlength: '8', required: true });
  authBox('Ersteinrichtung', 'Es existiert noch kein Benutzer. Lege den ersten Administrator an (mindestens 8 Zeichen Passwort).', [
    h('label', { class: 'field' }, h('span', {}, 'Benutzername'), user),
    h('label', { class: 'field' }, h('span', {}, 'Passwort'), pass),
    h('label', { class: 'field' }, h('span', {}, 'Passwort wiederholen'), pass2),
  ], 'Administrator anlegen', async (msg) => {
    if (pass.value !== pass2.value) { setMessage(msg, 'Die Passwörter stimmen nicht überein.'); return; }
    const me = await api.post('/api/auth/bootstrap', { username: user.value, password: pass.value }, { quietAuth: true });
    startApp(me);
  });
}

// ---- boot ---------------------------------------------------------------------------------------------

async function fetchMe() {
  try {
    return await api.get('/api/auth/me', { quietAuth: true });
  } catch (e) {
    if (e.status === 401) return e.body || { authenticated: false };
    throw e;
  }
}

function showBooting(text) {
  clear(app);
  app.appendChild(h('div', { class: 'boot' }, h('img', { src: '/assets/logo.svg', alt: '', width: 56, height: 56 }), h('span', { class: 'spin lg' }), h('span', {}, text || 'Camera Network Monitor wird geladen …')));
}

async function bootNormal() {
  showBooting();
  let me;
  try {
    me = await fetchMe();
  } catch {
    showOverlay('', 'SERVER OFFLINE', 'Verbindung wird wiederhergestellt …');
    setTimeout(bootNormal, 3000);
    return;
  }
  hideOverlay();
  if (me.needsBootstrap) {
    if (me.local) showBootstrap();
    else showLogin('Die Ersteinrichtung ist nur direkt am Server (localhost) möglich.');
    return;
  }
  if (me.authenticated) startApp(me);
  else showLogin();
}

// kioskAuth returns true when a kiosk session exists or could be created,
// false when access is denied; throws when the server is unreachable.
async function kioskAuth() {
  const me = await fetchMe();
  if (me.authenticated && me.principal && me.principal.kind === 'kiosk') return me;
  try {
    return await api.post('/api/auth/kiosk', { mon: kioskMon, key: kioskKey }, { quietAuth: true });
  } catch (e) {
    if (e.status === 403 || e.status === 401 || e.status === 400) return null;
    throw e;
  }
}

async function bootKiosk() {
  document.title = viewerTitle('Camera Network Monitor');
  showBooting('Kiosk wird verbunden …');
  if (!kioskMon || !kioskKey) { showBlocked(); return; }
  api.setKiosk(kioskMon);
  let me;
  try {
    me = await kioskAuth();
  } catch {
    showOverlay('', 'SERVER OFFLINE', 'Verbindung wird wiederhergestellt …');
    setTimeout(bootKiosk, 3000);
    return;
  }
  hideOverlay();
  if (!me) { showBlocked(); return; }
  startApp(me);
}

// ---- app ------------------------------------------------------------------------------------------------

function selectedMonitor() {
  const mons = (store.state && store.state.monitors) || [];
  if (KIOSK) return mons.find((m) => m.id === kioskMon) || null;
  const saved = storageGet('cnm.monitor');
  return mons.find((m) => m.id === saved) || mons[0] || null;
}

function mediaChipText(m) {
  if (!m) return ['', 'Media …'];
  switch (m.state) {
    case 'ok': return ['ok', `Media OK · ${m.desired}`];
    case 'warn': {
      const parts = [`Media WARN · ${m.desired}`];
      if (m.streamErrors) parts.push(`${m.streamErrors} Streamfehler`);
      if (m.preloadErrors) parts.push(`${m.preloadErrors} Preloadfehler`);
      return ['warn', parts.join(' · ')];
    }
    case 'offline': return ['offline', 'Media offline'];
    case 'disabled': return ['', 'Media deaktiviert'];
    default: return ['', 'Media …'];
  }
}

function buildShell() {
  clear(app);
  const content = h('div', { class: 'content' });
  if (KIOSK) {
    app.appendChild(content);
    return { content };
  }
  const monitorSel = h('select', { 'aria-label': 'Monitor', hidden: true });
  monitorSel.addEventListener('change', () => {
    storageSet('cnm.monitor', monitorSel.value);
    if (location.hash) location.hash = '';
    renderMonitor();
  });
  const chip = h('span', { class: 'chip clickable', title: '' }, h('span', { class: 'dot' }), h('span', { class: 'txt' }, 'Media …'));
  chip.addEventListener('click', showMediaDetails);
  const settingsBtn = h('button', { class: 'btn ghost', hidden: !store.isAdmin(), onclick: () => { location.hash = settingsRoute() ? '' : '#/settings'; } }, 'Einstellungen');
  const p = store.principal || {};
  const userBox = h('div', { class: 'usermenu' },
    h('button', { class: 'btn ghost small hide-sm', title: 'Mein Konto – Startbildschirm', onclick: openAccount }, p.username || ''),
    h('button', { class: 'btn ghost small', onclick: logout }, 'Abmelden'));
  const title = h('span', {}, (store.state && store.state.system.name) || 'Camera Network Monitor');
  const bar = h('header', { class: 'topbar' },
    h('div', { class: 'brand' }, h('img', { src: '/assets/logo.svg', alt: '' }), title),
    monitorSel,
    h('span', { class: 'spacer' }),
    chip,
    settingsBtn,
    userBox,
  );
  app.append(bar, content);
  return { content, monitorSel, chip, settingsBtn, title };
}

function updateChip() {
  if (!shell || !shell.chip) return;
  const m = store.media;
  const [cls, text] = mediaChipText(m);
  shell.chip.className = `chip clickable ${cls}`;
  shell.chip.querySelector('.txt').textContent = text;
  const details = m && m.errors ? m.errors.map((e) => `${e.stream}: ${e.message}`).join('\n') : '';
  shell.chip.title = details || (m && m.message) || '';
}

function showMediaDetails() {
  const m = store.media || {};
  const list = (m.errors || []).map((e) => h('tr', {}, h('td', { class: 'mono' }, e.stream), h('td', {}, e.kind === 'preload' ? 'Preload' : 'Stream'), h('td', {}, e.message)));
  openDialog({
    title: 'Media-Status',
    size: 'medium',
    body: h('div', {},
      h('p', {}, mediaChipText(m)[1]),
      m.message ? h('div', { class: 'msg warn' }, m.message) : null,
      m.state === 'offline' && store.isAdmin() ? h('p', { class: 'faint' },
        'Prüfen: Einstellungen → System → „go2rtc selbst verwalten“ (nutzt das mitgelieferte go2rtc). Dort steht auch der Status des go2rtc-Prozesses; Details im Journal: journalctl -u camera-network-monitor') : null,
      m.preloadSupported === false ? h('div', { class: 'msg warn' }, 'Das verwendete go2rtc unterstützt kein Preload – Live funktioniert, Streams werden aber nicht warm gehalten.') : null,
      list.length ? h('table', { class: 'list compact' }, h('thead', {}, h('tr', {}, h('th', {}, 'Stream'), h('th', {}, 'Art'), h('th', {}, 'Fehler'))), h('tbody', {}, list)) : h('p', { class: 'muted' }, 'Keine verifizierten Probleme.'),
      m.version ? h('p', { class: 'faint' }, `go2rtc ${m.version}`) : null,
    ),
    actions: [{ label: 'Schließen' }],
  });
}

function renderShellState() {
  if (!shell || !store.state) return;
  if (shell.title) shell.title.textContent = store.state.system.name;
  document.title = KIOSK ? viewerTitle(store.state.system.name) : store.state.system.name;
  if (shell.monitorSel) {
    const mons = store.state.monitors;
    const sel = selectedMonitor();
    clear(shell.monitorSel);
    for (const m of mons) shell.monitorSel.appendChild(h('option', { value: m.id, selected: sel && m.id === sel.id }, m.label));
    shell.monitorSel.hidden = mons.length <= 1 || !!settingsRoute();
  }
  if (shell.settingsBtn) shell.settingsBtn.hidden = !store.isAdmin();
}

function renderMonitor() {
  // also while the settings are still loading: no live players for a moment
  if (!view || settings || (settingsRoute() && store.isAdmin())) return;
  view.render(selectedMonitor());
}

let routing = Promise.resolve();

// route serialises transitions so fast tab/hash changes never mount the
// settings twice or leave the live view running behind them
function route() {
  routing = routing.then(doRoute, doRoute);
  return routing;
}

async function doRoute() {
  if (KIOSK || !view) return;
  const sr = settingsRoute();
  const wantSettings = !!sr && store.isAdmin();
  if (wantSettings && !settings) {
    // administration: no live players in the background (spec: no warm streams in settings)
    view.suspend();
    view.el.hidden = true;
    closeFocus();
    const mod = await import('./settings/index.js');
    if (!view) return;
    if (!settingsRoute()) { // left again while loading
      view.el.hidden = false;
      view.resume();
      renderShellState();
      return;
    }
    settings = mod.mountSettings(shell.content, sr.tab);
    if (shell.settingsBtn) shell.settingsBtn.classList.add('active');
  } else if (wantSettings && settings && sr.tab) {
    settings.show(sr.tab);
  } else if (!wantSettings && settings) {
    settings.destroy();
    settings = null;
    view.el.hidden = false;
    view.resume();
    if (shell.settingsBtn) shell.settingsBtn.classList.remove('active');
    renderMonitor();
  }
  renderShellState();
}

async function reauth() {
  if (KIOSK) {
    const me = await kioskAuth(); // throws while the server is unreachable
    if (!me) { showBlocked(); return false; }
    if (me.csrf) api.setCSRF(me.csrf);
    return true;
  }
  const me = await fetchMe();
  const prev = store.principal;
  if (!me.authenticated || (prev && prev.kind === 'user' && (!me.principal || me.principal.kind !== 'user'))) {
    showLogin('Die Sitzung ist abgelaufen. Bitte erneut anmelden.');
    return false;
  }
  if (me.csrf) api.setCSRF(me.csrf);
  return true;
}

function startApp(me) {
  teardown();
  hideOverlay();
  store.kiosk = KIOSK;
  store.principal = me.principal;
  store.prefs = me.preferences || {};
  api.setCSRF(me.csrf || '');
  if (!KIOSK) applyStartupScreen(store.prefs);
  shell = buildShell();
  view = new MonitorView(shell.content, {
    onOpen: (cameraId, slotLabel) => openFocus(cameraId, slotLabel, { allowWebRTC: !KIOSK && !!(store.state && store.state.media.allowWebRtc) }),
    allowWebRTC: () => !KIOSK && !!(store.state && store.state.media.allowWebRtc),
  });
  unsubs.push(store.on('state', () => { renderShellState(); renderMonitor(); }));
  unsubs.push(store.on('media', updateChip));
  unsubs.push(store.on('identify', showIdentify));
  api.onUnauthorized = () => { if (control) control.cb.onRevoked(); };
  control = new Control({
    onOffline: () => {
      view && view.suspend();
      suspendFocus();
      poller.stop();
      showOverlay('', 'SERVER OFFLINE', 'Verbindung zum CNM-Server unterbrochen – Wiederverbindung läuft …');
    },
    onOnline: () => {
      hideOverlay();
      if (view && !settings) view.resume();
      resumeFocus();
      poller.start();
      return true;
    },
    onReauth: reauth,
    onRevoked: async () => {
      if (KIOSK) {
        try {
          const me2 = await kioskAuth();
          if (!me2) { showBlocked(); return; }
          startApp(me2);
        } catch {
          showBlocked();
        }
        return;
      }
      teardown();
      bootNormal();
    },
    onReplaced: (names) => view && view.reloadStreams(names),
  });
  control.start();
  poller.start();
  if (!KIOSK) {
    window.addEventListener('hashchange', route);
    route();
  }
}

// ---- personal settings ------------------------------------------------------------------------------

function openAccount() {
  const p = store.principal || {};
  const current = (store.prefs && store.prefs.startupScreen) || '';
  const options = [['', 'Standard (zuletzt gewählter Multiviewer)']];
  for (const m of (store.state && store.state.monitors) || []) options.push([`monitor:${m.id}`, `Multiviewer: ${m.label}`]);
  if (store.isAdmin()) for (const [id, label] of SETTINGS_TABS) options.push([`settings:${id}`, `Einstellungen → ${label}`]);
  const radios = options.map(([value, label]) => {
    const input = h('input', { type: 'radio', name: 'startup', value, checked: value === current });
    return h('label', { class: 'radio' }, input, ' ', label);
  });
  const msg = h('div', { hidden: true });
  openDialog({
    title: `Mein Konto – ${p.username || ''}`,
    size: 'medium',
    body: h('div', {},
      h('div', { class: 'section-title' }, 'Startbildschirm'),
      h('p', { class: 'faint' }, 'Diese Ansicht öffnet sich nach der Anmeldung bzw. beim Start der Anwendung.'),
      h('div', { class: 'radiolist' }, radios),
      msg,
    ),
    actions: [
      { label: 'Abbrechen' },
      { label: 'Speichern', kind: 'primary', onClick: async () => {
        const sel = radios.map((r) => r.querySelector('input')).find((i) => i.checked);
        try {
          store.prefs = await api.put('/api/me/preferences', { startupScreen: sel ? sel.value : '' });
          toast('Startbildschirm gespeichert');
          return true;
        } catch (e) {
          setMessage(msg, e.message);
          return false;
        }
      } },
    ],
  });
}

async function logout() {
  const csrf = api.csrf;
  teardown();
  // the next login starts fresh (its own startup screen, no leftover settings tab)
  history.replaceState(null, '', location.pathname + location.search);
  try { await api.request('POST', '/api/auth/logout', {}, { quietAuth: true }); } catch { /* ignore */ }
  api.setCSRF('');
  void csrf;
  store.principal = null;
  bootNormal();
}

if (KIOSK) bootKiosk();
else bootNormal();
