// Focus view: fullscreen main stream of one camera with Live / Playback modes.
// - click on the live picture closes the view (live mode only)
// - click on the playback picture never closes
// - Escape closes the view unless a dialog is open (dialog.js layer stack)
import { h, clear } from './dom.js';
import store from './store.js';
import { registerEscapeLayer, alertDialog, toast } from './dialog.js';
import { StreamSlot } from './streamslot.js';
import { codecHint } from './player.js';
import { applyAspect, applyFit } from './multiview.js';
import { openPicker, PlaybackSession, Timeline } from './playback.js';
import { snapToRecording } from './timeline.js';

let current = null;

export function focusOpen() { return !!current; }
export function closeFocus() { if (current) current.close(); }
export function suspendFocus() { if (current) current.suspend(); }
export function resumeFocus() { if (current) current.resume(); }

export function openFocus(cameraId, slotLabel, { allowWebRTC }) {
  if (current) current.close();
  current = new FocusView(cameraId, slotLabel, allowWebRTC);
  return current;
}

// When the state changes, the open focus view follows (camera removed → close).
store.on('state', () => {
  if (!current) return;
  const cam = store.camera(current.cameraId);
  if (!cam || !cam.enabled) current.close();
  else current.refresh(cam);
});

class FocusView {
  constructor(cameraId, slotLabel, allowWebRTC) {
    this.cameraId = cameraId;
    this.slotLabel = slotLabel;
    this.allowWebRTC = allowWebRTC;
    this.mode = 'live';
    const cam = store.camera(cameraId);
    this.cam = cam;
    this.title = h('div', { class: 'title' });
    this.altNote = h('span', { class: 'alt-note', hidden: true, title: 'Der Mainstream liefert kein Bild – angezeigt wird der Substream' }, 'Substream');
    this.liveBtn = h('button', { class: 'btn active', onclick: () => this.showLive() }, 'Live');
    this.pbBtn = h('button', { class: 'btn', onclick: () => this.pickPlayback() }, 'Playback');
    this.body = h('div', { class: 'focus-body' });
    this.el = h('div', { class: 'focus', role: 'dialog', 'aria-label': 'Kameraansicht' },
      h('div', { class: 'focus-head' },
        this.title,
        this.altNote,
        h('span', { class: 'spacer' }),
        h('div', { class: 'seg' }, this.liveBtn, this.pbBtn),
        h('button', { class: 'btn', onclick: () => this.close() }, 'Schließen'),
      ),
      this.body,
    );
    document.getElementById('layers').appendChild(this.el);
    this.unregisterEsc = registerEscapeLayer(() => this.close());
    this.refresh(cam);
    this.showLive();
  }

  refresh(cam) {
    this.cam = cam;
    this.title.textContent = store.cameraName(cam.id, this.slotLabel);
    this.pbBtn.hidden = !cam.playback;
    if (this.mode === 'live' && this.slot && (this.slot.primary !== cam.focus || this.slot.alternate !== (cam.focusAlt || ''))) this.showLive();
  }

  _clearBody() {
    if (this.altNote) this.altNote.hidden = true;
    if (this.slot) { this.slot.destroy(); this.slot = null; }
    if (this.session) { this.session.destroy(); this.session = null; }
    if (this.timeline) { this.timeline.destroy(); this.timeline = null; }
    if (this.keyHandler) { document.removeEventListener('keydown', this.keyHandler); this.keyHandler = null; }
    clear(this.body);
    const tl = this.el.querySelector('.timeline');
    if (tl) tl.remove();
  }

  _frame(cls) {
    const frame = h('div', { class: `frame ${cls}` });
    applyAspect(frame, this.cam.aspectRatio);
    applyFit(frame, this.cam.objectFit);
    this.body.appendChild(frame);
    return frame;
  }

  showLive() {
    this.mode = 'live';
    this.liveBtn.classList.add('active');
    this.pbBtn.classList.remove('active');
    this._clearBody();
    const frame = this._frame('live');
    const hint = codecHint();
    const state = h('div', { class: 'state', hidden: true }, h('span', {}, 'STREAM OFFLINE'), hint ? h('small', {}, hint) : null);
    const loading = h('div', { class: 'loading-cover' }, h('span', { class: 'spin lg' }), 'Mainstream wird geladen …');
    this.body.append(state, loading);
    // click on the live picture closes the focus view (live mode only)
    frame.addEventListener('click', () => { if (this.mode === 'live') this.close(); });
    if (!this.cam.focus) { state.hidden = false; loading.remove(); return; }
    this.slot = new StreamSlot(frame, this.cam.focus, {
      allowWebRTC: this.allowWebRTC,
      alternate: this.cam.focusAlt || '',
      // main stream delivers no picture: show the sub stream instead
      onSource: (onAlt) => {
        this.altNote.hidden = !onAlt;
      },
      onVisual: (v) => { state.hidden = v !== 'offline'; loading.hidden = v !== 'loading'; },
      onSize: (w, hh) => {
        if (this.cam.aspectRatio === 'auto') { frame.style.setProperty('--ar-w', String(w)); frame.style.setProperty('--ar-h', String(hh)); }
      },
    });
    if (this.suspended) this.slot.suspend();
  }

  pickPlayback() {
    const name = store.cameraName(this.cam.id, this.slotLabel);
    openPicker(this.cam, name, (sel) => this.showPlayback(sel));
  }

  showPlayback(sel) {
    this.mode = 'playback';
    this.pbBtn.classList.add('active');
    this.liveBtn.classList.remove('active');
    this._clearBody();
    const frame = this._frame('playback');
    const notice = h('div', { class: 'notice', hidden: true });
    const state = h('div', { class: 'state', hidden: true });
    this.body.append(state, notice);
    let noticeTimer;
    const showNotice = (text) => {
      notice.textContent = text;
      notice.hidden = false;
      clearTimeout(noticeTimer);
      noticeTimer = setTimeout(() => { notice.hidden = true; }, 3500);
    };
    const range = { from: sel.from, to: sel.to, segments: sel.segments || [] };
    const session = new PlaybackSession(this.cam, range, frame, {
      onTime: (t) => timeline.setTime(t),
      onState: (s) => {
        timeline.setPlaying(s === 'playing' || (s === 'loading' && !session.wantPaused));
        state.hidden = s !== 'loading';
        clear(state);
        if (s === 'loading') state.append(h('div', { class: 'loading-cover' }, h('span', { class: 'spin lg' }), 'Aufnahme wird geladen …'));
        if (s === 'ended') showNotice('Ende des gewählten Zeitraums erreicht');
      },
      onError: (code, msg) => {
        state.hidden = true;
        alertDialog('Playback', msg, 'error');
      },
    });
    const seekTo = (t) => {
      const snap = snapToRecording(t, range.segments);
      if (snap.snapped) {
        showNotice(snap.direction === 'next'
          ? 'An dieser Stelle gibt es keine Aufnahme – nächste Aufnahme wird abgespielt'
          : 'An dieser Stelle gibt es keine Aufnahme – letzte verfügbare Aufnahme wird abgespielt');
        toast('Keine Aufnahme an der gewählten Stelle');
      }
      session.seek(snap.t);
    };
    const timeline = new Timeline(range, {
      onSeek: seekTo,
      onToggle: () => session.toggle(),
      onStep: (d) => seekTo(session.position() + d),
    });
    this.el.appendChild(timeline.el);
    this.session = session;
    this.timeline = timeline;
    // keyboard: space toggles, arrows step ±10 s (ignored while dialogs/inputs have focus)
    this.keyHandler = (e) => {
      if (e.target && /input|select|textarea/i.test(e.target.tagName)) return;
      if (document.querySelector('.backdrop')) return;
      if (e.code === 'Space') { e.preventDefault(); session.toggle(); }
      else if (e.key === 'ArrowLeft') { e.preventDefault(); seekTo(session.position() - 10000); }
      else if (e.key === 'ArrowRight') { e.preventDefault(); seekTo(session.position() + 10000); }
    };
    document.addEventListener('keydown', this.keyHandler);
    // a click on the playback picture must not close the view
    frame.addEventListener('click', (e) => e.stopPropagation());
    session.open(sel.start ?? range.from, false);
  }

  suspend() {
    this.suspended = true;
    if (this.slot) this.slot.suspend();
  }

  resume() {
    this.suspended = false;
    if (this.slot) this.slot.resume();
  }

  close() {
    this._clearBody();
    this.unregisterEsc();
    this.el.remove();
    if (current === this) current = null;
  }
}
