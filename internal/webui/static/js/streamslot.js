// StreamSlot = one live player + its watchdog. It distinguishes the two
// levels of health: (1) the media source/producer is online (server
// health) and (2) the browser player actually advances frames. Only the
// affected player instance is recreated, with backoff – never the page.
import { LivePlayer } from './player.js';

export const STALL_MS = 20000;          // no decoded progress → recreate
export const LOAD_TIMEOUT_MS = 25000;   // never became ready → recreate
export const OFFLINE_VISUAL_MS = 8000;  // no progress → show STREAM OFFLINE
export const SOURCE_FAILS = 6;          // failed health polls before recovery
export const NO_PICTURE_MS = 12000;     // with an alternate stream: switch after this long without picture
export const PRIMARY_RETRY_MS = 300000; // on the alternate: try the primary stream again
const BACKOFF = [1000, 2000, 4000, 8000, 15000, 30000];

const slots = new Set();
let ticker = null;
let graceUntil = 0;

function tickAll() {
  if (document.hidden) return; // frame callbacks do not run in hidden tabs
  const now = performance.now();
  for (const s of slots) s.tick(now);
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) {
    graceUntil = performance.now() + 10000;
    for (const s of slots) s.refreshBaseline();
  }
});

export class StreamSlot {
  /**
   * @param {HTMLElement} frame container for the media element
   * @param {string} stream stream name
   * @param {{allowWebRTC?:boolean, alternate?:string, onSource?:(alternate:boolean)=>void, onVisual?:(v:string)=>void, onSize?:(w:number,h:number)=>void}} opts
   *   alternate: stream used when the primary one delivers no picture (the
   *   multiview falls back to main, the single view to sub)
   */
  constructor(frame, stream, opts = {}) {
    this.frame = frame;
    this.stream = stream;
    this.primary = stream;
    this.alternate = opts.alternate && opts.alternate !== stream ? opts.alternate : '';
    this.onAlternate = false;
    this.altSince = 0;
    this.failsHere = 0; // consecutive failures of the current stream
    this.opts = opts;
    this.player = null;
    this.visual = '';
    this.attempt = 0;
    this.restartTimer = null;
    this.sourceFails = 0;
    this.readySince = 0;
    this.suspended = false;
    this.destroyed = false;
    slots.add(this);
    if (!ticker) ticker = setInterval(tickAll, 1000);
    this._create();
  }

  _create() {
    if (this.destroyed || this.suspended) return;
    const p = new LivePlayer(this.stream, {
      allowWebRTC: this.opts.allowWebRTC,
      onState: (st) => {
        if (st === 'ready') {
          this.readySince = performance.now();
          this.failsHere = 0;
          if (this.opts.onSize) {
            const [w, h] = p.intrinsicSize();
            if (w && h) this.opts.onSize(w, h);
          }
        }
        this._updateVisual(performance.now());
      },
    });
    this.player = p;
    p.mount(this.frame);
    p.start();
    this._setVisual('loading');
  }

  refreshBaseline() {
    if (this.player && this.player.lastProgress) this.player.lastProgress = performance.now();
  }

  _setVisual(v) {
    if (v === this.visual) return;
    this.visual = v;
    if (this.opts.onVisual) this.opts.onVisual(v);
  }

  _updateVisual(now) {
    const p = this.player;
    if (!p) { this._setVisual(this.suspended ? 'suspended' : 'offline'); return; }
    if (p.state === 'error') { this._setVisual('offline'); return; }
    if (p.state === 'ready') {
      this._setVisual(now - p.lastProgress < OFFLINE_VISUAL_MS ? 'ok' : 'offline');
      return;
    }
    // loading: a long GOP may delay the first picture – only declare offline
    // after a generous period or when the server confirms the source is down
    this._setVisual(now - p.startedAt > 15000 || this.sourceFails >= 2 ? 'offline' : 'loading');
  }

  tick(now) {
    if (this.destroyed || this.suspended || !this.player || this.restartTimer) return;
    const p = this.player;
    this._updateVisual(now);
    if (now < graceUntil) return;
    if (p.state === 'error') this.restart('error');
    else if (p.state === 'ready' && now - p.lastProgress > STALL_MS) this.restart('stall');
    else if (p.state === 'loading' && this.alternate && now - p.startedAt > NO_PICTURE_MS) this.restart('no-picture');
    else if (p.state === 'loading' && now - p.startedAt > LOAD_TIMEOUT_MS) this.restart('load-timeout');
    else if (p.state === 'ready' && now - this.readySince > 60000) this.attempt = 0; // stable again
    // running on the alternate: give the primary stream another chance now and then
    if (this.onAlternate && !this.restartTimer && p.state === 'ready' && now - this.altSince > PRIMARY_RETRY_MS) this._use(false);
  }

  // _use switches between primary and alternate stream (immediate restart)
  _use(alternate) {
    this.onAlternate = alternate;
    this.altSince = performance.now();
    this.stream = alternate ? this.alternate : this.primary;
    this.failsHere = 0;
    this.attempt = 0;
    this.sourceFails = 0;
    if (this.opts.onSource) this.opts.onSource(alternate);
    clearTimeout(this.restartTimer);
    this.restartTimer = null;
    if (this.player) { this.player.destroy(); this.player = null; }
    this._create();
  }

  // health is called with the server side health of this stream (or undefined)
  health(h) {
    if (this.destroyed || this.suspended) return;
    if (h && h.online) this.sourceFails = 0;
    else this.sourceFails++;
    if (this.sourceFails >= SOURCE_FAILS && !this.restartTimer) {
      this.sourceFails = 0;
      this.restart('source-offline');
    }
    this._updateVisual(performance.now());
  }

  restart(reason) {
    if (this.destroyed || this.suspended || this.restartTimer) return;
    if (this.alternate) {
      // no picture from the current stream: change to the other one (both
      // directions, so a failing alternate leads back to the primary)
      this.failsHere++;
      if (reason === 'no-picture' || this.failsHere >= 2) {
        this.lastReason = reason;
        this._use(!this.onAlternate);
        return;
      }
    }
    if (this.player) { this.player.destroy(); this.player = null; }
    this._setVisual('offline');
    const delay = BACKOFF[Math.min(this.attempt, BACKOFF.length - 1)];
    this.attempt++;
    this.lastReason = reason;
    this.restartTimer = setTimeout(() => {
      this.restartTimer = null;
      this._create();
    }, delay);
  }

  // immediate restart (stream definition replaced on the server)
  reload() {
    if (this.destroyed || this.suspended) return;
    clearTimeout(this.restartTimer);
    this.restartTimer = null;
    if (this.player) { this.player.destroy(); this.player = null; }
    this.attempt = 0;
    this._create();
  }

  suspend() {
    this.suspended = true;
    clearTimeout(this.restartTimer);
    this.restartTimer = null;
    if (this.player) { this.player.destroy(); this.player = null; }
    this._setVisual('suspended');
  }

  resume() {
    if (!this.suspended || this.destroyed) return;
    this.suspended = false;
    // after a pause start with the primary stream again
    if (this.onAlternate) {
      this.onAlternate = false;
      this.stream = this.primary;
      if (this.opts.onSource) this.opts.onSource(false);
    }
    this.attempt = 0;
    this.sourceFails = 0;
    this._create();
  }

  destroy() {
    this.destroyed = true;
    clearTimeout(this.restartTimer);
    if (this.player) { this.player.destroy(); this.player = null; }
    slots.delete(this);
    if (!slots.size && ticker) { clearInterval(ticker); ticker = null; }
  }
}

export function activeSlots() { return slots; }
