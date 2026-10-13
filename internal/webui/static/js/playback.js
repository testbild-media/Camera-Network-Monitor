// Playback: range picker, fMP4 player (fetch + MSE, no native controls) and
// the CNM timeline. Seeking reopens the DVR source at an absolute time.
import api, { ApiError } from './api.js';
import { h, clear, fmtDateTime, fmtTime, fmtDuration, toLocalInput, fromLocalInput, sameDay } from './dom.js';
import { openDialog, setMessage } from './dialog.js';
import { snapToRecording, tickStep, ticks, formatHover, formatTick, timeAt, percentOf, validateRange } from './timeline.js';

export const DEFAULT_RANGE_MS = 15 * 60 * 1000;
const CLIENT_START_TIMEOUT_MS = 16000;
const MAX_ROWS = 100;

const MESSAGES = {
  NO_STORAGE: 'Das Gerät hat keine Festplatte – es sind keine Aufnahmen vorhanden.',
  NO_RECORDINGS: 'Im gewählten Zeitraum wurden keine Aufnahmen gefunden.',
  PLAYBACK_UNSUPPORTED: 'Für diese Kamera ist keine Wiedergabe verfügbar.',
  INVALID_TIME_RANGE: 'Der Zeitraum ist ungültig. Der Start muss vor dem Ende liegen.',
  RANGE_TOO_LARGE: 'Der Zeitraum darf höchstens 24 Stunden umfassen.',
  PLAYBACK_START_TIMEOUT: 'Die Wiedergabe startet nicht. Das Gerät liefert keine Daten.',
  PLAYBACK_MEDIA_ERROR: 'Die Aufnahme konnte nicht abgespielt werden.',
  DEVICE_OFFLINE: 'Das Gerät ist derzeit nicht verbunden.',
  PLAYBACK_BUSY: 'Zu viele gleichzeitige Wiedergaben für dieses Gerät. Bitte später erneut versuchen.',
  SEARCH_FAILED: 'Die Aufnahmesuche am Gerät ist fehlgeschlagen.',
  NETWORK: 'Der Server ist nicht erreichbar.',
};

export function playbackMessage(code, fallback) {
  return MESSAGES[code] || fallback || 'Die Wiedergabe ist fehlgeschlagen.';
}

// ---- picker -----------------------------------------------------------------------------------------

const HOUR_MS = 3600 * 1000;
const DAY_MS = 24 * HOUR_MS;
const ZOOMS = [[HOUR_MS, '1 h'], [6 * HOUR_MS, '6 h'], [DAY_MS, '24 h']];

const nowMinute = () => Math.ceil(Date.now() / 60000) * 60000;

/**
 * Opens the timeline range picker: the recordings of a window (1 h, 6 h or
 * 24 h) are shown as a timeline; the user drags (or clicks) a range and only
 * then the viewer opens via onPlay({from, to, segments, start}).
 */
export function openPicker(cam, name, onPlay) {
  let span = DAY_MS;
  let viewEnd = nowMinute();
  let segments = [];
  let loaded = null; // {from, to} covered by `segments`
  let sel = null;
  let loadGen = 0;
  let firstLoad = true;

  const msg = h('div', { hidden: true });
  const rangeText = h('div', { class: 'range' });
  const prevBtn = h('button', { class: 'btn small', type: 'button' });
  const nextBtn = h('button', { class: 'btn small', type: 'button' });
  const nowBtn = h('button', { class: 'btn small', type: 'button' }, 'Jetzt');
  const zoomBtns = ZOOMS.map(([ms, label]) => {
    const b = h('button', { class: 'btn small', type: 'button', title: `Zeitleiste zeigt ${label}` }, label);
    b.addEventListener('click', () => setSpan(ms));
    return [ms, b];
  });
  const zoomSeg = h('div', { class: 'seg' }, zoomBtns.map(([, b]) => b));
  const track = h('div', { class: 'pick-track', title: 'Bereich mit der Maus aufziehen oder klicken (15 Minuten)' });
  const hoverLine = h('div', { class: 'tl-hover', hidden: true });
  const hoverTime = h('div', { class: 'tl-hovertime', hidden: true });
  const selEl = h('div', { class: 'pick-sel', hidden: true });
  const overlay = h('div', { class: 'loading-cover' }, h('span', { class: 'spin' }), 'Aufnahmen werden geladen …');
  const fromIn = h('input', { type: 'datetime-local', step: '1' });
  const toIn = h('input', { type: 'datetime-local', step: '1' });
  const dur = h('div', { class: 'dur' });
  const wrap = h('div', { class: 'tl-wrap' }, track, hoverTime);

  const viewStart = () => viewEnd - span;
  const snapMs = () => (span >= 6 * HOUR_MS ? 60000 : 10000);

  function setSpan(ms) {
    if (ms === span) return;
    // keep the selection (or the middle of the current window) in view
    const anchor = sel ? (sel.from + sel.to) / 2 : viewEnd - span / 2;
    span = ms;
    viewEnd = Math.min(Math.ceil((anchor + span / 2) / 60000) * 60000, nowMinute());
    load();
  }

  function renderTrack() {
    clear(track);
    const from = viewStart(), to = viewEnd;
    for (const s of segments) {
      if (s.end <= from || s.start >= to) continue;
      const l = percentOf(Math.max(s.start, from), from, to), r = percentOf(Math.min(s.end, to), from, to);
      track.appendChild(h('div', { class: 'tl-seg', style: { left: `${l}%`, width: `${Math.max(r - l, 0.15)}%` } }));
    }
    const width = track.clientWidth || 800;
    const step = tickStep(span, width, 14);
    const major = tickStep(span, width, 90);
    for (const t of ticks(from, to, step)) {
      const isMajor = (t - new Date(new Date(t).setHours(0, 0, 0, 0)).getTime()) % major === 0;
      const left = `${percentOf(t, from, to)}%`;
      track.appendChild(h('div', { class: `tl-tick${isMajor ? ' major' : ''}`, style: { left } }));
      if (isMajor) track.appendChild(h('div', { class: 'tl-ticklabel', style: { left } }, formatTick(t, major)));
    }
    if (!segments.some((s) => s.end > from && s.start < to) && !overlay.isConnected) {
      track.appendChild(h('div', { class: 'pick-empty' }, 'Keine Aufnahmen in diesem Zeitraum'));
    }
    track.append(selEl, hoverLine);
    renderSel();
  }

  function renderSel() {
    const from = viewStart(), to = viewEnd;
    if (!sel || sel.to <= from || sel.from >= to) { selEl.hidden = true; } else {
      const l = percentOf(sel.from, from, to), r = percentOf(sel.to, from, to);
      selEl.hidden = false;
      selEl.style.left = `${l}%`;
      selEl.style.width = `${Math.max(r - l, 0.2)}%`;
    }
    if (sel) {
      fromIn.value = toLocalInput(sel.from);
      toIn.value = toLocalInput(sel.to);
      dur.textContent = `Dauer: ${fmtDuration(sel.to - sel.from)}`;
    } else {
      dur.textContent = 'Bitte einen Bereich auf der Zeitleiste auswählen.';
    }
    rangeText.textContent = `${fmtDateTime(viewStart())} – ${fmtDateTime(viewEnd)}`;
    const label = ZOOMS.find(([ms]) => ms === span)[1];
    prevBtn.textContent = `◀ ${label}`;
    nextBtn.textContent = `${label} ▶`;
    prevBtn.title = `${label} zurück`;
    nextBtn.title = `${label} vor`;
    nextBtn.disabled = viewEnd >= Date.now() - 60000;
    for (const [ms, b] of zoomBtns) b.classList.toggle('active', ms === span);
  }

  async function load() {
    const gen = ++loadGen;
    const from = viewStart(), to = Math.min(viewEnd, Date.now());
    // a zoomed-in window inside the already searched range needs no new search
    if (loaded && from >= loaded.from && to <= loaded.to) {
      renderTrack();
      return;
    }
    setMessage(msg, '');
    segments = [];
    loaded = null;
    track.appendChild(overlay);
    renderTrack();
    track.appendChild(overlay);
    try {
      const res = await api.post('/api/playback/search', { cameraId: cam.id, from, to });
      if (gen !== loadGen) return;
      segments = res.ranges || [];
      loaded = { from, to };
    } catch (e) {
      if (gen !== loadGen) return;
      segments = [];
      if (e.code === 'NO_RECORDINGS') loaded = { from, to };
      else setMessage(msg, playbackMessage(e.code, e.message));
    }
    overlay.remove();
    if (firstLoad) {
      firstLoad = false;
      // default selection: the last 15 minutes with recorded material
      if (segments.length) {
        const end = Math.min(Math.max(...segments.map((s) => s.end)), viewEnd);
        sel = { from: Math.max(viewStart(), end - DEFAULT_RANGE_MS), to: end };
      }
    }
    renderTrack();
  }

  // ---- interaction ----
  const timeAtEvent = (e) => {
    const rect = track.getBoundingClientRect();
    const q = snapMs();
    return Math.round(timeAt(e.clientX - rect.left, rect.width, viewStart(), viewEnd) / q) * q;
  };
  let drag = null;
  track.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    track.setPointerCapture(e.pointerId);
    drag = { x: e.clientX, t: timeAtEvent(e) };
  });
  track.addEventListener('pointermove', (e) => {
    const rect = track.getBoundingClientRect();
    const x = Math.min(Math.max(e.clientX - rect.left, 0), rect.width);
    const t = timeAtEvent(e);
    hoverLine.hidden = false;
    hoverTime.hidden = false;
    hoverLine.style.left = `${x}px`;
    hoverTime.style.left = `${x}px`;
    hoverTime.textContent = formatHover(t, viewStart(), viewEnd);
    if (drag && Math.abs(e.clientX - drag.x) > 3) {
      sel = { from: Math.min(drag.t, t), to: Math.max(drag.t, t) };
      renderSel();
    }
  });
  track.addEventListener('pointerleave', () => { hoverLine.hidden = true; hoverTime.hidden = true; });
  track.addEventListener('pointerup', (e) => {
    if (!drag) return;
    const t = timeAtEvent(e);
    if (Math.abs(e.clientX - drag.x) <= 3) {
      // click: 15 minutes from the clicked position (clamped to the window)
      const to = Math.min(t + DEFAULT_RANGE_MS, viewEnd);
      sel = { from: Math.max(viewStart(), to - DEFAULT_RANGE_MS), to };
    } else if (sel && sel.to - sel.from < 10000) {
      sel.to = sel.from + 10000;
    }
    drag = null;
    renderSel();
  });
  const onInput = () => {
    const f = fromLocalInput(fromIn.value), t = fromLocalInput(toIn.value);
    const err = validateRange(f, t);
    if (err) { setMessage(msg, err); return; }
    setMessage(msg, '');
    sel = { from: f, to: t };
    if (t > viewEnd || f < viewStart()) {
      while (span < DAY_MS && t - f > span) span = ZOOMS.find(([ms]) => ms > span)[0];
      viewEnd = Math.min(Math.ceil(t / 60000) * 60000 + Math.round(span / 24), nowMinute());
      load();
    } else renderSel();
  };
  fromIn.addEventListener('change', onInput);
  toIn.addEventListener('change', onInput);
  prevBtn.addEventListener('click', () => { viewEnd -= span; load(); });
  nextBtn.addEventListener('click', () => { viewEnd = Math.min(viewEnd + span, nowMinute()); load(); });
  nowBtn.addEventListener('click', () => { viewEnd = nowMinute(); load(); });
  const ro = new ResizeObserver(() => renderTrack());

  const dlg = openDialog({
    title: `Playback – ${name}`,
    size: 'large',
    onClose: () => { ro.disconnect(); loadGen++; },
    body: h('div', {},
      h('div', { class: 'picker-nav' }, prevBtn, rangeText, nextBtn, nowBtn, zoomSeg),
      wrap,
      h('div', { class: 'pick-info' },
        h('label', { class: 'field' }, h('span', {}, 'Start'), fromIn),
        h('label', { class: 'field' }, h('span', {}, 'Ende'), toIn),
        dur,
      ),
      h('div', { style: { marginTop: '10px' } }, msg),
    ),
    actions: [
      { label: 'Als MP4 herunterladen', left: true, onClick: async () => {
        if (!sel) { setMessage(msg, 'Bitte zuerst einen Bereich auf der Zeitleiste auswählen.'); return false; }
        const err = validateRange(sel.from, sel.to);
        if (err) { setMessage(msg, err); return false; }
        if (sel.to - sel.from > 2 * 3600e3) { setMessage(msg, 'Ein Download darf höchstens 2 Stunden umfassen.'); return false; }
        const a = document.createElement('a');
        a.href = `/api/playback/download?cameraId=${encodeURIComponent(cam.id)}&start=${sel.from}&end=${Math.min(sel.to, Date.now())}`;
        a.download = '';
        document.body.appendChild(a);
        a.click();
        a.remove();
        setMessage(msg, 'Download gestartet. Liefert das Gerät die Aufnahme nur in Echtzeit, dauert der Download so lange wie der gewählte Zeitraum.', 'ok');
        return false;
      } },
      { label: 'Abbrechen' },
      { label: 'Abspielen', kind: 'primary', onClick: async () => {
        if (!sel) { setMessage(msg, 'Bitte zuerst einen Bereich auf der Zeitleiste auswählen.'); return false; }
        const err = validateRange(sel.from, sel.to);
        if (err) { setMessage(msg, err); return false; }
        try {
          const res = await api.post('/api/playback/search', { cameraId: cam.id, from: sel.from, to: Math.min(sel.to, Date.now()) });
          const segs = res.ranges || [];
          onPlay({ from: sel.from, to: sel.to, segments: segs, start: snapToRecording(sel.from, segs).t });
          return true;
        } catch (e) {
          setMessage(msg, e.code === 'NO_RECORDINGS' ? 'Im gewählten Bereich gibt es keine Aufnahmen.' : playbackMessage(e.code, e.message));
          return false;
        }
      } },
    ],
  });
  ro.observe(track);
  load();
  return dlg;
}

// ---- player -------------------------------------------------------------------------------------------

export class PlaybackSession {
  /**
   * @param {object} cam camera
   * @param {{from:number,to:number,segments:{start:number,end:number}[]}} range
   * @param {HTMLElement} frame
   * @param {{onTime:(t:number)=>void, onState:(s:string)=>void, onError:(code:string,msg:string)=>void}} cb
   */
  constructor(cam, range, frame, cb) {
    this.cam = cam;
    this.range = range;
    this.frame = frame;
    this.cb = cb;
    this.base = range.from;
    this.origin = null;
    this.wantPaused = false;
    this.gen = 0;
    const v = document.createElement('video');
    v.muted = true;
    v.playsInline = true;
    v.controls = false;
    v.disablePictureInPicture = true;
    v.disableRemotePlayback = true;
    v.setAttribute('playsinline', '');
    v.addEventListener('timeupdate', () => { if (this.origin !== null) this.cb.onTime(this.position()); });
    v.addEventListener('play', () => this.cb.onState('playing'));
    v.addEventListener('pause', () => this.cb.onState('paused'));
    v.addEventListener('ended', () => this.cb.onState('ended'));
    v.addEventListener('contextmenu', (e) => e.preventDefault());
    this.video = v;
    frame.appendChild(v);
  }

  position() {
    if (this.origin === null) return this.base;
    return Math.min(this.range.to, this.base + (this.video.currentTime - this.origin) * 1000);
  }

  playing() { return !this.video.paused && !this.video.ended; }

  _reset() {
    this.gen++;
    if (this.abort) { this.abort.abort(); this.abort = null; }
    clearTimeout(this.startTimer);
    const v = this.video;
    try { v.pause(); } catch { /* ignore */ }
    v.removeAttribute('src');
    v.srcObject = null;
    try { v.load(); } catch { /* ignore */ }
    if (this.objectURL) { URL.revokeObjectURL(this.objectURL); this.objectURL = null; }
    this.origin = null;
  }

  /** Open the DVR stream at absolute time `at`; keep paused when requested. */
  async open(at, paused) {
    this._reset();
    const gen = this.gen;
    this.base = at;
    this.wantPaused = !!paused;
    this.cb.onState('loading');
    this.cb.onTime(at);
    const ac = new AbortController();
    this.abort = ac;
    this.startTimer = setTimeout(() => {
      if (gen === this.gen && this.origin === null) {
        this._reset();
        this.cb.onError('PLAYBACK_START_TIMEOUT', playbackMessage('PLAYBACK_START_TIMEOUT'));
      }
    }, CLIENT_START_TIMEOUT_MS);
    const url = api.url('/api/playback/stream', { cameraId: this.cam.id, start: Math.round(at), end: Math.round(this.range.to) }).toString();
    let resp;
    try {
      resp = await fetch(url, { signal: ac.signal, credentials: 'same-origin', cache: 'no-store', headers: api.headers() });
    } catch (e) {
      if (e.name === 'AbortError' || gen !== this.gen) return;
      clearTimeout(this.startTimer);
      this.cb.onError('NETWORK', playbackMessage('NETWORK'));
      return;
    }
    if (gen !== this.gen) { try { resp.body && resp.body.cancel(); } catch { /* ignore */ } return; }
    if (!resp.ok) {
      clearTimeout(this.startTimer);
      let code = '', message = '';
      try { const j = await resp.json(); code = j.error && j.error.code; message = j.error && j.error.message; } catch { /* ignore */ }
      this.cb.onError(code || 'PLAYBACK_MEDIA_ERROR', playbackMessage(code, message));
      return;
    }
    const codec = resp.headers.get('X-CNM-Codec') || 'avc1.640029';
    const mime = `video/mp4; codecs="${codec}"`;
    const MS = window.ManagedMediaSource || window.MediaSource;
    if (!MS || !MS.isTypeSupported(mime)) {
      // progressive fallback (re-requests the stream)
      ac.abort();
      if (MS && !MS.isTypeSupported(mime) && !document.createElement('video').canPlayType(mime)) {
        clearTimeout(this.startTimer);
        this.cb.onError('PLAYBACK_MEDIA_ERROR', `Der Browser unterstützt das Videoformat der Aufnahme nicht (${codec}).`);
        return;
      }
      this._progressive(url, gen);
      return;
    }
    await this._mse(resp, MS, mime, gen);
  }

  _onFirstData(gen) {
    if (gen !== this.gen || this.origin !== null) return;
    const v = this.video;
    this.origin = v.buffered.length ? v.buffered.start(0) : 0;
    v.currentTime = this.origin;
    clearTimeout(this.startTimer);
    if (this.wantPaused) this.cb.onState('paused');
    else v.play().catch(() => this.cb.onState('paused'));
    this.cb.onTime(this.position());
  }

  _progressive(url, gen) {
    const v = this.video;
    v.addEventListener('loadeddata', () => this._onFirstData(gen), { once: true });
    v.src = url;
  }

  async _mse(resp, MS, mime, gen) {
    const v = this.video;
    const ms = new MS();
    if (window.ManagedMediaSource && MS === window.ManagedMediaSource) v.srcObject = ms;
    else { this.objectURL = URL.createObjectURL(ms); v.src = this.objectURL; }
    await new Promise((res) => ms.addEventListener('sourceopen', res, { once: true }));
    if (gen !== this.gen) return;
    let sb;
    try { sb = ms.addSourceBuffer(mime); } catch {
      this.cb.onError('PLAYBACK_MEDIA_ERROR', 'Der Browser kann dieses Videoformat nicht abspielen.');
      return;
    }
    v.addEventListener('loadeddata', () => this._onFirstData(gen), { once: true });
    const reader = resp.body.getReader();
    const waitIdle = () => new Promise((res) => (sb.updating ? sb.addEventListener('updateend', res, { once: true }) : res()));
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (gen !== this.gen) { reader.cancel().catch(() => {}); return; }
        if (done) break;
        await waitIdle();
        if (gen !== this.gen) return;
        sb.appendBuffer(value);
        await waitIdle();
        // back pressure: keep at most ~60 s ahead of the playhead
        while (gen === this.gen && v.buffered.length && v.buffered.end(v.buffered.length - 1) - v.currentTime > 60) {
          await new Promise((r) => setTimeout(r, 500));
          if (v.buffered.length && v.currentTime - v.buffered.start(0) > 90 && !sb.updating) {
            sb.remove(v.buffered.start(0), v.currentTime - 30);
            await waitIdle();
          }
        }
      }
      await waitIdle();
      if (gen === this.gen && ms.readyState === 'open') ms.endOfStream();
    } catch (e) {
      if (gen !== this.gen || e.name === 'AbortError') return;
      if (this.origin === null) {
        clearTimeout(this.startTimer);
        this.cb.onError('PLAYBACK_MEDIA_ERROR', playbackMessage('PLAYBACK_MEDIA_ERROR'));
      }
    }
  }

  toggle() {
    if (this.origin === null) { this.wantPaused = !this.wantPaused; this.cb.onState(this.wantPaused ? 'paused' : 'loading'); return; }
    if (this.video.paused) this.video.play().catch(() => {});
    else this.video.pause();
  }

  /** Seek keeps the play/pause state. */
  seek(at) {
    const paused = this.origin === null ? this.wantPaused : this.video.paused;
    const t = Math.min(Math.max(at, this.range.from), this.range.to - 1000);
    // inside the already buffered window → local seek, no reopen needed
    const v = this.video;
    if (this.origin !== null && v.buffered.length) {
      const local = this.origin + (t - this.base) / 1000;
      for (let i = 0; i < v.buffered.length; i++) {
        if (local >= v.buffered.start(i) && local <= v.buffered.end(i) - 0.5) {
          v.currentTime = local;
          this.cb.onTime(t);
          return;
        }
      }
    }
    this.open(t, paused);
  }

  destroy() {
    this._reset();
    this.video.remove();
  }
}

// ---- timeline -------------------------------------------------------------------------------------------

export class Timeline {
  /**
   * @param {{from:number,to:number,segments:{start:number,end:number}[]}} range
   * @param {{onSeek:(t:number)=>void, onToggle:()=>void, onStep:(ms:number)=>void}} cb
   */
  constructor(range, cb) {
    this.range = range;
    this.cb = cb;
    this.playBtn = h('button', { class: 'btn icon', title: 'Play/Pause (Leertaste)', onclick: () => cb.onToggle() }, '▶');
    this.clock = h('div', { class: 'tl-clock' }, '--:--:--');
    this.track = h('div', { class: 'tl-track', role: 'slider', 'aria-label': 'Zeitleiste' });
    this.playhead = h('div', { class: 'tl-playhead' });
    this.hoverLine = h('div', { class: 'tl-hover', hidden: true });
    this.hoverTime = h('div', { class: 'tl-hovertime', hidden: true });
    this.wrap = h('div', { class: 'tl-wrap' }, this.track, this.hoverTime);
    this.el = h('div', { class: 'timeline' },
      h('div', { class: 'tl-controls' },
        this.playBtn,
        h('button', { class: 'btn', title: '10 Sekunden zurück', onclick: () => cb.onStep(-10000) }, '−10 s'),
        this.clock,
        h('button', { class: 'btn', title: '10 Sekunden vor', onclick: () => cb.onStep(10000) }, '+10 s'),
        h('span', { class: 'tl-range' }, `${fmtDateTime(range.from)} – ${sameDay(range.from, range.to) ? fmtTime(range.to) : fmtDateTime(range.to)}`),
      ),
      this.wrap,
    );
    this.track.addEventListener('mousemove', (e) => this._hover(e));
    this.track.addEventListener('mouseleave', () => { this.hoverLine.hidden = true; this.hoverTime.hidden = true; });
    this.track.addEventListener('click', (e) => {
      const rect = this.track.getBoundingClientRect();
      cb.onSeek(timeAt(e.clientX - rect.left, rect.width, range.from, range.to));
    });
    this.ro = new ResizeObserver(() => this._render());
    this.ro.observe(this.track);
  }

  _render() {
    const { from, to, segments } = this.range;
    const width = this.track.clientWidth;
    clear(this.track);
    for (const s of segments || []) {
      const l = percentOf(s.start, from, to), r = percentOf(s.end, from, to);
      this.track.appendChild(h('div', { class: 'tl-seg', style: { left: `${l}%`, width: `${Math.max(r - l, 0.15)}%` } }));
    }
    const step = tickStep(to - from, width, 14);
    const major = tickStep(to - from, width, 90);
    for (const t of ticks(from, to, step)) {
      const isMajor = (t - new Date(new Date(t).setHours(0, 0, 0, 0)).getTime()) % major === 0;
      const left = `${percentOf(t, from, to)}%`;
      this.track.appendChild(h('div', { class: `tl-tick${isMajor ? ' major' : ''}`, style: { left } }));
      if (isMajor) this.track.appendChild(h('div', { class: 'tl-ticklabel', style: { left } }, formatTick(t, major)));
    }
    this.track.appendChild(this.playhead);
    this.track.appendChild(this.hoverLine);
  }

  _hover(e) {
    const rect = this.track.getBoundingClientRect();
    const x = Math.min(Math.max(e.clientX - rect.left, 0), rect.width);
    const t = timeAt(x, rect.width, this.range.from, this.range.to);
    this.hoverLine.hidden = false;
    this.hoverTime.hidden = false;
    this.hoverLine.style.left = `${x}px`;
    this.hoverTime.style.left = `${x}px`;
    this.hoverTime.textContent = formatHover(t, this.range.from, this.range.to);
  }

  setTime(t) {
    this.playhead.style.left = `${percentOf(t, this.range.from, this.range.to)}%`;
    this.clock.textContent = sameDay(this.range.from, this.range.to) ? fmtTime(t) : fmtDateTime(t);
  }

  setPlaying(playing) {
    this.playBtn.textContent = playing ? '❚❚' : '▶';
    this.playBtn.title = playing ? 'Pause' : 'Abspielen';
  }

  destroy() {
    this.ro.disconnect();
    this.el.remove();
  }
}

export { ApiError };
