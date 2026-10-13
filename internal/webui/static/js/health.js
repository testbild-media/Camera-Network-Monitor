// HealthPoller polls /api/media/status for the visible streams.
// Never overlapping, paused in hidden tabs, immediate check when the tab
// becomes visible again.
import api from './api.js';
import store from './store.js';
import { activeSlots } from './streamslot.js';

export const POLL_MS = 5000;

class HealthPoller {
  constructor() {
    this.running = false;
    this.inFlight = false;
    this.timer = null;
    this.abort = null;
    this._onVis = () => {
      if (!document.hidden && this.running && !this.inFlight) {
        clearTimeout(this.timer);
        this.poll();
      }
    };
  }

  start() {
    if (this.running) return;
    this.running = true;
    document.addEventListener('visibilitychange', this._onVis);
    this.poll();
  }

  stop() {
    this.running = false;
    clearTimeout(this.timer);
    this.timer = null;
    if (this.abort) this.abort.abort();
    document.removeEventListener('visibilitychange', this._onVis);
  }

  _schedule() {
    if (!this.running) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.poll(), POLL_MS);
  }

  async poll() {
    if (!this.running || this.inFlight) return;
    if (document.hidden) return; // resumed by visibilitychange
    const slots = [...activeSlots()].filter((s) => !s.suspended);
    const names = [...new Set(slots.map((s) => s.stream))];
    this.inFlight = true;
    this.abort = new AbortController();
    const timeout = setTimeout(() => this.abort && this.abort.abort(), POLL_MS * 2);
    try {
      const res = await api.get(`/api/media/status?streams=${encodeURIComponent(names.join(','))}`, { signal: this.abort.signal, quietAuth: true, background: true });
      if (res.summary) store.setMedia(res.summary);
      for (const s of slots) s.health(res.streams ? res.streams[s.stream] : undefined);
    } catch (e) {
      // server unreachable is handled by the control connection; do not
      // count it as stream failures
    } finally {
      clearTimeout(timeout);
      this.inFlight = false;
      this.abort = null;
      this._schedule();
    }
  }
}

const poller = new HealthPoller();
export default poller;
