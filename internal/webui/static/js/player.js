// LivePlayer – a minimal client for go2rtc's WebSocket API.
//
// Modes are chosen automatically: WebRTC (only if the server allows it for
// this session), MSE (H.264/H.265 fMP4) and MJPEG as fallback for sources
// MSE cannot play. The player never shows go2rtc's mode/status overlays and
// never reconnects by itself – recovery is done by StreamSlot (watchdog).
import api from './api.js';

const VIDEO_CODECS = [
  'avc1.640029', 'avc1.64002A', 'avc1.640033', 'avc1.42E01E', 'avc1.4D401F',
  'hvc1.1.6.L153.B0', 'hev1.1.6.L153.B0',
];

const MAX_QUEUE_BYTES = 12 * 1024 * 1024;

let codecHintCache;
/**
 * Hint for tiles when this browser cannot decode H.264/H.265 at all – e.g. an
 * open-source Chromium build without the proprietary codecs. '' = fine.
 */
export function codecHint() {
  if (codecHintCache === undefined) {
    const MS = window.ManagedMediaSource || window.MediaSource;
    let ok = false;
    try { ok = !!MS && supportedCodecs((t) => MS.isTypeSupported(t)) !== ''; } catch { ok = false; }
    codecHintCache = ok ? '' : 'Browser kann H.264/H.265 nicht abspielen – Chrome, Edge oder Firefox verwenden';
  }
  return codecHintCache;
}

function supportedCodecs(isSupported) {
  return VIDEO_CODECS.filter((c) => {
    try { return isSupported(`video/mp4; codecs="${c}"`); } catch { return false; }
  }).join(',');
}

export class LivePlayer {
  /**
   * @param {string} stream go2rtc stream name
   * @param {{allowWebRTC?: boolean, onState?: (state:string, info?:string)=>void}} opts
   */
  constructor(stream, opts = {}) {
    this.stream = stream;
    this.opts = opts;
    this.state = 'idle';
    this.mode = '';
    this.lastProgress = 0;
    this.startedAt = 0;
    this.destroyed = false;

    const v = document.createElement('video');
    v.muted = true;
    v.defaultMuted = true;
    v.autoplay = true;
    v.playsInline = true;
    v.controls = false;
    v.disablePictureInPicture = true;
    v.disableRemotePlayback = true;
    v.preload = 'auto';
    v.setAttribute('muted', '');
    v.setAttribute('playsinline', '');
    this.video = v;
    this.img = null;
    this.queue = [];
    this.queueBytes = 0;
    this._lastTime = -1;
    this._lastFrames = 0;
    // Progress means decoded pictures. Position changes alone do not count:
    // the live edge seek after every append (e.g. a repeated init segment of
    // a source that connects but sends no video) would look like playback.
    this._onTimeUpdate = () => {
      const v = this.video;
      const q = v.getVideoPlaybackQuality ? v.getVideoPlaybackQuality() : null;
      if (q) {
        if (q.totalVideoFrames > this._lastFrames) {
          this._lastFrames = q.totalVideoFrames;
          this._progress();
        }
      } else if (!v.seeking && v.currentTime > this._lastTime) {
        this._lastTime = v.currentTime;
        this._progress();
      }
    };
  }

  mount(container) {
    container.appendChild(this.video);
    this.container = container;
  }

  _setState(state, info) {
    if (this.destroyed || this.state === state) return;
    this.state = state;
    if (this.opts.onState) this.opts.onState(state, info);
  }

  _progress() {
    this.lastProgress = performance.now();
    if (this.state === 'loading') this._setState('ready');
  }

  _fail(reason) {
    if (this.destroyed) return;
    this._setState('error', reason);
    this._teardown();
  }

  start() {
    this.startedAt = performance.now();
    this.lastProgress = 0;
    this._setState('loading');
    const ws = new WebSocket(api.wsUrl('/media/api/ws', { src: this.stream }));
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    ws.onopen = () => {
      if (this.destroyed) return;
      if (this.opts.allowWebRTC && 'RTCPeerConnection' in window) this._startWebRTC();
      else this._startMSE();
    };
    ws.onmessage = (ev) => {
      if (this.destroyed) return;
      if (typeof ev.data === 'string') {
        let msg;
        try { msg = JSON.parse(ev.data); } catch { return; }
        this._onMessage(msg);
      } else if (this.ondata) {
        this.ondata(ev.data);
      }
    };
    ws.onclose = () => this._fail('connection closed');
    ws.onerror = () => this._fail('connection error');
    this._watchFrames();
  }

  _send(obj) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(obj));
  }

  _onMessage(msg) {
    switch (msg.type) {
      case 'mse':
        if (this.mode === 'mse') this._initSourceBuffer(msg.value);
        break;
      case 'webrtc/answer':
        if (this.pc) this.pc.setRemoteDescription({ type: 'answer', sdp: msg.value }).catch(() => this._webrtcFallback());
        break;
      case 'webrtc/candidate':
        if (this.pc && msg.value) this.pc.addIceCandidate({ candidate: msg.value, sdpMid: '0' }).catch(() => {});
        break;
      case 'error': {
        const v = String(msg.value || '');
        if (this.mode === 'webrtc' && v.startsWith('webrtc')) this._webrtcFallback();
        else if (this.mode === 'mse' && v.startsWith('mse')) this._startMJPEG();
        else this._fail(v);
        break;
      }
    }
  }

  // ---- MSE ----------------------------------------------------------------------------------
  _startMSE() {
    const MS = window.ManagedMediaSource || window.MediaSource;
    if (!MS) { this._startMJPEG(); return; }
    const codecs = supportedCodecs((t) => MS.isTypeSupported(t));
    if (!codecs) { this._startMJPEG(); return; }
    this.mode = 'mse';
    const ms = new MS();
    this.ms = ms;
    ms.addEventListener('sourceopen', () => {
      if (this.objectURL) { URL.revokeObjectURL(this.objectURL); this.objectURL = null; }
      this._send({ type: 'mse', value: codecs });
    }, { once: true });
    if (window.ManagedMediaSource && MS === window.ManagedMediaSource) {
      this.video.srcObject = ms;
    } else {
      this.objectURL = URL.createObjectURL(ms);
      this.video.src = this.objectURL;
    }
    this.video.play().catch(() => {});
  }

  _initSourceBuffer(mime) {
    let sb;
    try {
      sb = this.ms.addSourceBuffer(mime);
    } catch (e) {
      this._startMJPEG();
      return;
    }
    sb.mode = 'segments';
    this.sb = sb;
    sb.addEventListener('updateend', () => this._afterAppend());
    sb.addEventListener('error', () => this._fail('source buffer error'));
    this.ondata = (data) => {
      this.queue.push(new Uint8Array(data));
      this.queueBytes += data.byteLength;
      if (this.queueBytes > MAX_QUEUE_BYTES) { this._fail('buffer overflow'); return; }
      this._pump();
    };
    this.video.addEventListener('error', () => this._fail('video error'), { once: true });
  }

  _pump() {
    const sb = this.sb;
    if (!sb || sb.updating || !this.queue.length || this.destroyed) return;
    let chunk;
    if (this.queue.length === 1) chunk = this.queue[0];
    else {
      chunk = new Uint8Array(this.queueBytes);
      let off = 0;
      for (const q of this.queue) { chunk.set(q, off); off += q.byteLength; }
    }
    this.queue = [];
    this.queueBytes = 0;
    try {
      sb.appendBuffer(chunk);
    } catch (e) {
      if (e.name === 'QuotaExceededError') this._trim(true);
      else this._fail('append failed');
    }
  }

  _afterAppend() {
    if (this.destroyed || !this.sb) return;
    const v = this.video;
    const b = this.sb.buffered;
    if (b.length) {
      const end = b.end(b.length - 1);
      const start = b.start(0);
      if (v.currentTime < start || end - v.currentTime > 4) {
        // jump to the live edge (after stalls or a slow start)
        v.currentTime = Math.max(start, end - 0.4);
      } else {
        const gap = end - v.currentTime;
        v.playbackRate = gap > 1.2 ? 1.15 : 1;
      }
      if (v.paused) v.play().catch(() => {});
      if (end - start > 20 && !this.sb.updating) {
        try { this.sb.remove(start, end - 10); return; } catch { /* ignore */ }
      }
    }
    this._pump();
  }

  _trim(force) {
    const b = this.sb && this.sb.buffered;
    if (!b || !b.length || this.sb.updating) return;
    const end = b.end(b.length - 1);
    try { this.sb.remove(b.start(0), Math.max(b.start(0), end - (force ? 2 : 10))); } catch { /* ignore */ }
  }

  // ---- MJPEG ---------------------------------------------------------------------------------
  _startMJPEG() {
    this._teardownMSE();
    this.mode = 'mjpeg';
    const img = document.createElement('img');
    img.alt = '';
    img.decoding = 'async';
    img.draggable = false;
    this.img = img;
    if (this.container) this.container.appendChild(img);
    this.video.style.display = 'none';
    let url = null;
    this.ondata = (data) => {
      const next = URL.createObjectURL(new Blob([data], { type: 'image/jpeg' }));
      img.onload = () => {
        if (url) URL.revokeObjectURL(url);
        url = next;
        this._progress();
      };
      img.src = next;
    };
    this._revokeImg = () => { if (url) URL.revokeObjectURL(url); };
    this._send({ type: 'mjpeg' });
  }

  // ---- WebRTC (optional, never for kiosks; no external STUN) -----------------------------
  _startWebRTC() {
    this.mode = 'webrtc';
    const pc = new RTCPeerConnection({ iceServers: [], bundlePolicy: 'max-bundle' });
    this.pc = pc;
    pc.addTransceiver('video', { direction: 'recvonly' });
    pc.onicecandidate = (ev) => this._send({ type: 'webrtc/candidate', value: ev.candidate ? ev.candidate.candidate : '' });
    pc.ontrack = (ev) => {
      this.video.srcObject = ev.streams[0] || new MediaStream([ev.track]);
      this.video.play().catch(() => {});
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') this._webrtcFallback();
    };
    pc.createOffer().then((o) => pc.setLocalDescription(o)).then(() => {
      this._send({ type: 'webrtc/offer', value: pc.localDescription.sdp });
    }).catch(() => this._webrtcFallback());
    this._rtcTimer = setTimeout(() => { if (this.state !== 'ready') this._webrtcFallback(); }, 6000);
  }

  _webrtcFallback() {
    if (this.mode !== 'webrtc' || this.destroyed) return;
    this._teardownWebRTC();
    this._startMSE();
  }

  _teardownWebRTC() {
    clearTimeout(this._rtcTimer);
    if (this.pc) {
      try { this.pc.close(); } catch { /* ignore */ }
      this.pc = null;
    }
    this.video.srcObject = null;
  }

  // ---- progress ------------------------------------------------------------------------------
  _watchFrames() {
    const v = this.video;
    if ('requestVideoFrameCallback' in v) {
      const cb = () => {
        if (this.destroyed) return;
        this._progress();
        this._rvfc = v.requestVideoFrameCallback(cb);
      };
      this._rvfc = v.requestVideoFrameCallback(cb);
    }
    v.addEventListener('timeupdate', this._onTimeUpdate);
  }

  intrinsicSize() {
    if (this.mode === 'mjpeg' && this.img) return [this.img.naturalWidth, this.img.naturalHeight];
    return [this.video.videoWidth, this.video.videoHeight];
  }

  // ---- teardown --------------------------------------------------------------------------------
  _teardownMSE() {
    this.ondata = null;
    this.queue = [];
    this.queueBytes = 0;
    if (this.ms && this.ms.readyState === 'open') {
      try { if (this.sb && this.sb.updating) this.sb.abort(); } catch { /* ignore */ }
      try { this.ms.endOfStream(); } catch { /* ignore */ }
    }
    this.sb = null;
    this.ms = null;
    if (this.objectURL) { URL.revokeObjectURL(this.objectURL); this.objectURL = null; }
  }

  _teardown() {
    if (this.ws) {
      this.ws.onclose = this.ws.onerror = this.ws.onmessage = this.ws.onopen = null;
      try { this.ws.close(); } catch { /* ignore */ }
      this.ws = null;
    }
    this._teardownWebRTC();
    this._teardownMSE();
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this._teardown();
    const v = this.video;
    if (this._rvfc && v.cancelVideoFrameCallback) v.cancelVideoFrameCallback(this._rvfc);
    v.removeEventListener('timeupdate', this._onTimeUpdate);
    try { v.pause(); } catch { /* ignore */ }
    v.removeAttribute('src');
    v.srcObject = null;
    try { v.load(); } catch { /* ignore */ }
    v.remove();
    if (this._revokeImg) this._revokeImg();
    if (this.img) { this.img.removeAttribute('src'); this.img.remove(); }
  }
}
