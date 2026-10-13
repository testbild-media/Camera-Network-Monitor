// MonitorView renders one monitor layout. DOM and players are rebuilt only
// for structural changes (layout, slot assignment, stream, presentation);
// status changes are applied incrementally.
import { h, clear } from './dom.js';
import store from './store.js';
import { StreamSlot } from './streamslot.js';
import { codecHint } from './player.js';

export function applyAspect(frame, aspect) {
  frame.classList.toggle('auto', aspect === 'auto');
  const m = /^(\d+):(\d+)$/.exec(aspect || '');
  if (m) {
    frame.style.setProperty('--ar-w', m[1]);
    frame.style.setProperty('--ar-h', m[2]);
  }
}

export function applyFit(frame, fit) {
  frame.classList.remove('fit-fill', 'fit-contain', 'fit-cover');
  frame.classList.add(`fit-${fit || 'fill'}`);
}

class Tile {
  constructor(view, slotId) {
    this.view = view;
    this.slotId = slotId;
    this.sig = null;
    this.slot = null;
    this.stream = null;
    this.labelEl = h('div', { class: 'label' });
    this.stateEl = h('div', { class: 'state' });
    this.spinner = h('div', { class: 'spinner', hidden: true });
    this.altBadge = h('div', { class: 'alt-badge', hidden: true, title: 'Substream liefert kein Bild – Mainstream wird angezeigt' }, 'Main');
    this.frame = h('div', { class: 'frame' });
    this.el = h('div', { class: 'tile' }, this.frame, this.stateEl, this.spinner, this.altBadge, this.labelEl);
    this.el.addEventListener('click', (ev) => {
      if (this.cameraId && this.kind === 'camera') {
        ev.stopPropagation();
        this.view.opts.onOpen(this.cameraId, this.slotLabel);
      }
    });
  }

  update(slot, weight) {
    this.el.style.flexGrow = String(weight);
    const cam = slot.cameraId ? store.camera(slot.cameraId) : null;
    this.cameraId = cam ? cam.id : null;
    this.slotLabel = slot.label || '';
    let kind = 'empty';
    if (cam && !cam.enabled) kind = 'disabled';
    else if (cam) kind = 'camera';
    this.kind = kind;
    const stream = kind === 'camera' ? cam.multiview || '' : '';
    const alt = kind === 'camera' ? cam.multiviewAlt || '' : '';
    const sig = [kind, stream, alt, cam ? cam.aspectRatio : '', cam ? cam.objectFit : ''].join('|');
    const name = cam ? store.cameraName(cam.id, slot.label) : (slot.label || '');
    // a disabled camera keeps its slot: placeholder, no stream, name stays visible
    this.labelEl.textContent = kind === 'camera' || kind === 'disabled' ? name : '';
    this.labelEl.hidden = kind !== 'camera' && kind !== 'disabled';
    this.el.classList.toggle('camera', kind === 'camera');
    this.el.title = kind === 'camera' ? `${name} – Klick öffnet Mainstream` : '';
    if (sig === this.sig) return;
    this.sig = sig;
    this._destroyStream();
    applyAspect(this.frame, cam ? cam.aspectRatio : '16:9');
    applyFit(this.frame, cam ? cam.objectFit : 'fill');
    if (kind === 'empty') {
      this._setState('empty', 'Nicht belegt');
    } else if (kind === 'disabled') {
      this._setState('disabled', 'Kamera deaktiviert');
    } else if (!stream) {
      this._setState('offline', 'STREAM OFFLINE');
    } else {
      this.altBadge.hidden = true;
      this.stream = new StreamSlot(this.frame, stream, {
        allowWebRTC: this.view.opts.allowWebRTC(),
        alternate: alt,
        // the substream delivers no picture: the tile shows the main stream
        onSource: (onAlt) => { this.altBadge.hidden = !onAlt; },
        onVisual: (v) => this._visual(v),
        onSize: (w, h2) => {
          if (cam.aspectRatio === 'auto') {
            this.frame.style.setProperty('--ar-w', String(w));
            this.frame.style.setProperty('--ar-h', String(h2));
          }
        },
      });
      if (this.view.suspended) this.stream.suspend();
    }
  }

  _setState(kind, text) {
    this.stateEl.className = `state ${kind}`;
    const hint = kind === 'offline' ? codecHint() : '';
    if (hint) {
      clear(this.stateEl);
      this.stateEl.append(h('span', {}, text), h('small', {}, hint));
    } else {
      this.stateEl.textContent = text;
    }
    this.stateEl.hidden = !text;
    this.spinner.hidden = true;
  }

  _visual(v) {
    switch (v) {
      case 'ok':
        this._setState('', '');
        break;
      case 'loading':
        this._setState('', '');
        this.spinner.hidden = false;
        break;
      case 'suspended':
        this._setState('', '');
        break;
      default:
        this._setState('offline', 'STREAM OFFLINE');
    }
  }

  _destroyStream() {
    if (this.altBadge) this.altBadge.hidden = true;
    if (this.stream) {
      this.stream.destroy();
      this.stream = null;
    }
  }

  destroy() {
    this._destroyStream();
    this.el.remove();
  }
}

export class MonitorView {
  /**
   * @param {HTMLElement} root
   * @param {{onOpen:(cameraId:string, slotLabel:string)=>void, allowWebRTC:()=>boolean}} opts
   */
  constructor(root, opts) {
    this.root = root;
    this.opts = opts;
    this.tiles = new Map();
    this.structSig = null;
    this.monitorId = null;
    this.suspended = false;
    this.el = h('div', { class: 'monitor' });
    root.appendChild(this.el);
  }

  render(monitor) {
    if (!monitor) {
      this._clearAll();
      clear(this.el);
      this.el.appendChild(h('div', { class: 'empty-monitor' }, h('div', {}, 'Kein Monitor verfügbar')));
      this.structSig = null;
      return;
    }
    if (monitor.id !== this.monitorId) {
      this._clearAll();
      this.monitorId = monitor.id;
      this.structSig = null;
    }
    const layout = monitor.layout;
    this.el.style.setProperty('--gap', `${layout.gap}px`);
    const struct = JSON.stringify(layout.rows.map((r) => [r.id, r.height, r.slots.map((s) => [s.id, s.width])]));
    const seen = new Set();
    if (struct !== this.structSig) {
      // rebuild rows, re-using tiles (and their running players) by slot id
      const rows = layout.rows.map((r) => {
        const rowEl = h('div', { class: 'mrow' });
        rowEl.style.flexGrow = String(r.height);
        for (const s of r.slots) {
          let t = this.tiles.get(s.id);
          if (!t) { t = new Tile(this, s.id); this.tiles.set(s.id, t); }
          rowEl.appendChild(t.el);
          seen.add(s.id);
        }
        return rowEl;
      });
      clear(this.el);
      for (const r of rows) this.el.appendChild(r);
      for (const [id, t] of this.tiles) if (!seen.has(id)) { t.destroy(); this.tiles.delete(id); }
      this.structSig = struct;
      // moved media elements may have been paused by the browser
      for (const v of this.el.querySelectorAll('video')) if (v.paused) v.play().catch(() => {});
    }
    for (const r of layout.rows) for (const s of r.slots) {
      const t = this.tiles.get(s.id);
      if (t) t.update(s, s.width);
    }
  }

  // restart players whose stream definition was replaced on the server
  reloadStreams(names) {
    const set = new Set(names);
    for (const t of this.tiles.values()) if (t.stream && set.has(t.stream.stream)) t.stream.reload();
  }

  suspend() {
    this.suspended = true;
    for (const t of this.tiles.values()) if (t.stream) t.stream.suspend();
  }

  resume() {
    this.suspended = false;
    for (const t of this.tiles.values()) if (t.stream) t.stream.resume();
  }

  _clearAll() {
    for (const t of this.tiles.values()) t.destroy();
    this.tiles.clear();
  }

  destroy() {
    this._clearAll();
    this.el.remove();
  }
}
