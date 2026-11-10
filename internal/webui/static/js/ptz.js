// PTZ panel of the focus view: take over a motorised camera, steer it
// (hold a direction button or an arrow key), zoom, go to stored positions
// and hand it back. While a user steers, CNM pauses the camera's own
// tracking and its return to the guard ("fixed") position; when the user
// ends the control, or after the return time without input, the camera
// moves back to its fixed position and tracks again by itself.
import { h, clear } from './dom.js';
import api from './api.js';
import store from './store.js';
import { confirmDialog, toast } from './dialog.js';

const DIRS = [
  ['↖', -1, 1, 'Links oben'], ['↑', 0, 1, 'Hoch'], ['↗', 1, 1, 'Rechts oben'],
  ['←', -1, 0, 'Links'], null, ['→', 1, 0, 'Rechts'],
  ['↙', -1, -1, 'Links unten'], ['↓', 0, -1, 'Runter'], ['↘', 1, -1, 'Rechts unten'],
];
const KEYS = { ArrowLeft: [-1, 0, 0], ArrowRight: [1, 0, 0], ArrowUp: [0, 1, 0], ArrowDown: [0, -1, 0], '+': [0, 0, 1], '-': [0, 0, -1] };
const RENEW_MS = 3000; // a held move is renewed (the server stops moves that are not)
const POLL_MS = 4000;

function fmtLeft(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

export class PTZPanel {
  constructor(cameraId) {
    this.cameraId = cameraId;
    this.base = `/api/cameras/${encodeURIComponent(cameraId)}/ptz`;
    this.view = null;
    this.ctl = null;
    this.speed = 0.5;
    this.moving = null;
    this.keyCode = null; // physical key of a move started with the keyboard
    this.chain = Promise.resolve(); // commands are sent one after another
    this.inflight = 0;
    this.status = h('div', { class: 'ptz-status' }, 'Lädt …');
    this.controls = h('div', { class: 'ptz-controls' });
    this.el = h('div', { class: 'ptz-panel', role: 'group', 'aria-label': 'PTZ-Steuerung' }, this.status, this.controls);
    // the live picture closes the view on click: keep clicks in the panel
    this.el.addEventListener('click', (e) => e.stopPropagation());
    this.keyDown = (e) => this._keyDown(e);
    this.keyUp = (e) => this._keyUp(e);
    // a key or button released outside the window never sends keyup/pointerup
    this.onBlur = () => this.stopMove();
    this.onVisibility = () => { if (document.hidden) this.stopMove(); };
    document.addEventListener('keydown', this.keyDown);
    document.addEventListener('keyup', this.keyUp);
    window.addEventListener('blur', this.onBlur);
    document.addEventListener('visibilitychange', this.onVisibility);
    this.load();
    this.poll = setInterval(() => this.refreshStatus(), POLL_MS);
    this.tick = setInterval(() => this.renderStatus(), 1000);
  }

  get self() { return !!(this.ctl && this.ctl.self); }

  async load() {
    try {
      this.view = await api.get(this.base, { background: true });
      this.ctl = this.view.controller || null;
    } catch (e) {
      this.view = null;
      this.status.textContent = e.message;
      clear(this.controls);
      return;
    }
    this.render();
  }

  async refreshStatus() {
    if (!this.view || document.hidden) return;
    try {
      const r = await api.get(`${this.base}/status`, { background: true });
      const was = this.self;
      this.ctl = r.controller || null;
      if (was !== this.self) {
        if (was && !this.self) toast(this.ctl ? `${this.ctl.username} hat die Steuerung übernommen` : 'Steuerung beendet – die Kamera fährt in ihre feste Position');
        this.render();
      } else {
        this.renderStatus();
      }
    } catch { /* keep the last state */ }
  }

  renderStatus() {
    if (!this.view) return;
    const c = this.ctl;
    const v = this.view;
    let text;
    if (!c) {
      const auto = [];
      if (v.tracking) auto.push('verfolgt selbst');
      if (v.features.guard && v.guardSet && v.guard) auto.push('kehrt in ihre feste Position zurück');
      text = auto.length ? `Automatik: Kamera ${auto.join(' und ')}` : 'Frei';
    } else if (c.releasing) {
      text = 'Kamera wird in ihre feste Position zurückgestellt …';
    } else if (c.self) {
      text = `Du steuerst · Rückkehr in ${fmtLeft(Date.parse(c.returnAt) - Date.now())} ohne Eingabe`;
    } else {
      text = `Gesteuert von ${c.username}`;
    }
    this.status.textContent = text;
  }

  render() {
    this.renderStatus();
    clear(this.controls);
    const v = this.view;
    if (!v) return;
    if (!this.self) {
      const other = this.ctl && !this.ctl.releasing && !this.ctl.self;
      this.controls.append(h('button', { class: 'btn primary', type: 'button', onclick: () => this.take(other) },
        other ? 'Steuerung übernehmen …' : 'Steuerung übernehmen'));
      return;
    }
    const f = v.features;
    if (f.panTilt) {
      const pad = h('div', { class: 'ptz-pad' });
      for (const d of DIRS) {
        if (!d) {
          pad.append(f.guard && v.guardSet
            ? h('button', { class: 'btn ptz-home', type: 'button', title: 'Zur festen Position', onclick: () => this.guard('goto') }, '⌂')
            : h('span'));
          continue;
        }
        pad.append(this._holdButton(d[0], d[3], d[1], d[2], 0));
      }
      this.controls.append(pad);
    }
    const row = h('div', { class: 'ptz-row' });
    if (f.zoom) {
      row.append(this._holdButton('−', 'Zoom heraus', 0, 0, -1), this._holdButton('+', 'Zoom hinein', 0, 0, 1));
    }
    if (f.panTilt) {
      const speed = h('input', { type: 'range', min: '0.1', max: '1', step: '0.1', value: String(this.speed), title: 'Geschwindigkeit', 'aria-label': 'Geschwindigkeit' });
      speed.addEventListener('input', () => { this.speed = Number(speed.value); });
      row.append(h('label', { class: 'ptz-speed' }, 'Tempo', speed));
    }
    this.controls.append(row);
    if (f.presets && v.presets.length) {
      const sel = h('select', { 'aria-label': 'Gespeicherte Position' }, h('option', { value: '' }, 'Gespeicherte Position …'),
        v.presets.map((p) => h('option', { value: String(p.id) }, p.name || `Position ${p.id}`)));
      sel.addEventListener('change', () => {
        if (!sel.value) return;
        this.send('preset', { id: Number(sel.value) });
        sel.value = '';
      });
      this.controls.append(sel);
    }
    if (f.guard && store.isAdmin()) {
      this.controls.append(h('button', { class: 'btn small', type: 'button', onclick: () => this.guard('save') }, 'Als feste Position speichern'));
    }
    if (f.guard && !v.guardSet) {
      this.controls.append(h('p', { class: 'faint' }, 'Die Kamera hat keine feste Position gespeichert – nach der Steuerung bleibt sie stehen.'));
    }
    this.controls.append(h('button', { class: 'btn', type: 'button', onclick: () => this.release() }, 'Steuerung beenden'));
  }

  _holdButton(label, title, pan, tilt, zoom) {
    const b = h('button', { class: 'btn ptz-btn', type: 'button', title, 'aria-label': title }, label);
    const start = (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      b.setPointerCapture?.(e.pointerId);
      b.classList.add('active');
      this.startMove(pan, tilt, zoom);
    };
    const end = () => {
      if (!b.classList.contains('active')) return;
      b.classList.remove('active');
      this.stopMove();
    };
    b.addEventListener('pointerdown', start);
    b.addEventListener('pointerup', end);
    b.addEventListener('pointercancel', end);
    b.addEventListener('lostpointercapture', end);
    return b;
  }

  startMove(pan, tilt, zoom) {
    const vec = { pan: pan * this.speed, tilt: tilt * this.speed, zoom: zoom * Math.max(this.speed, 0.5) };
    clearInterval(this.renew);
    this.moving = vec;
    this.send('move', vec);
    // renew while held; skipped while a command is still under way
    this.renew = setInterval(() => { if (this.moving && this.inflight === 0) this.send('move', this.moving); }, RENEW_MS);
  }

  stopMove() {
    this.keyCode = null;
    if (!this.moving) return;
    clearInterval(this.renew);
    this.moving = null;
    this.send('move', { pan: 0, tilt: 0, zoom: 0 });
  }

  _keyDown(e) {
    if (!this.self || e.repeat || this.keyCode) return;
    if (e.target && /input|select|textarea/i.test(e.target.tagName)) return;
    if (document.querySelector('.backdrop')) return;
    const k = KEYS[e.key];
    if (!k) return;
    e.preventDefault();
    this.keyCode = e.code;
    this.startMove(k[0], k[1], k[2]);
  }

  _keyUp(e) {
    // compared by physical key: '+' may come back as '=' once Shift is up
    if (this.keyCode && e.code === this.keyCode) {
      e.preventDefault();
      this.stopMove();
    }
  }

  // send queues a command behind the previous ones, so a stop never
  // overtakes the move it ends
  send(what, body, background = true) {
    this.inflight++;
    this.chain = this.chain.then(async () => {
      try {
        await api.post(`${this.base}/${what}`, body, { background });
      } catch (e) {
        if (e.code === 'PTZ_NOT_CONTROLLING' || e.code === 'PTZ_BUSY') {
          this.moving = null;
          this.keyCode = null;
          clearInterval(this.renew);
          this.refreshStatus();
        }
        toast(e.message, 'error');
      } finally {
        this.inflight--;
      }
    });
    return this.chain;
  }

  async take(other) {
    if (other && !await confirmDialog('Steuerung übernehmen', `Die Kamera wird gerade von ${this.ctl.username} gesteuert. Trotzdem übernehmen?`, { confirmLabel: 'Übernehmen' })) return;
    try {
      const r = await api.post(`${this.base}/control`, { take: true, force: !!other });
      this.ctl = r.controller;
      await this.load();
    } catch (e) {
      toast(e.message, 'error');
      this.refreshStatus();
    }
  }

  async release() {
    this.stopMove();
    await this.chain; // the stop goes out before the control ends
    try {
      const r = await api.post(`${this.base}/control`, { take: false });
      toast(r && r.pending ? r.message : 'Steuerung beendet – die Kamera fährt in ihre feste Position und verfolgt wieder selbst');
    } catch (e) {
      toast(e.message, 'error');
    }
    await this.load();
  }

  async guard(action) {
    if (action === 'save' && !await confirmDialog('Feste Position speichern', 'Die aktuelle Position wird die feste Position, in die die Kamera nach dem Verfolgen und nach der Steuerung zurückfährt.', { confirmLabel: 'Speichern' })) return;
    this.stopMove();
    await this.chain;
    try {
      await api.post(`${this.base}/guard`, { action }, { background: action === 'goto' });
      if (action === 'save') {
        toast('Feste Position gespeichert');
        await this.load();
      }
    } catch (e) {
      toast(e.message, 'error');
    }
  }

  destroy() {
    if (this.moving) this.stopMove();
    clearInterval(this.poll);
    clearInterval(this.tick);
    clearInterval(this.renew);
    document.removeEventListener('keydown', this.keyDown);
    document.removeEventListener('keyup', this.keyUp);
    window.removeEventListener('blur', this.onBlur);
    document.removeEventListener('visibilitychange', this.onVisibility);
    if (this.self) toast(`Steuerung läuft weiter – die Kamera kehrt spätestens nach ${this.view ? this.view.returnMinutes : 5} Minuten ohne Eingabe zurück`);
    this.el.remove();
  }
}
