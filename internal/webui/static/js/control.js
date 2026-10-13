// Control connection (/ws) with server-offline detection and recovery.
// "Server offline" (this connection lost) is distinct from "Media offline"
// (server reachable, go2rtc not).
import api from './api.js';
import store from './store.js';

const BACKOFF = [500, 1000, 2000, 4000, 8000, 10000];
const SILENCE_MS = 70000; // server sends a tick every 25 s
const OFFLINE_DELAY_MS = 1500;

export class Control {
  /**
   * @param {{onOnline:()=>Promise<boolean>|boolean, onOffline:()=>void, onRevoked:()=>void, onReplaced:(names:string[])=>void}} cb
   */
  constructor(cb) {
    this.cb = cb;
    this.ws = null;
    this.attempt = 0;
    this.stopped = false;
    this.offline = false;
    this.offlineTimer = null;
    this.retryTimer = null;
    this.silenceTimer = null;
  }

  start() {
    this.stopped = false;
    this._connect();
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.retryTimer);
    clearTimeout(this.offlineTimer);
    clearTimeout(this.silenceTimer);
    if (this.ws) {
      this.ws.onclose = this.ws.onmessage = this.ws.onerror = null;
      try { this.ws.close(); } catch { /* ignore */ }
      this.ws = null;
    }
  }

  _armSilence() {
    clearTimeout(this.silenceTimer);
    this.silenceTimer = setTimeout(() => {
      if (this.ws) try { this.ws.close(); } catch { /* ignore */ }
    }, SILENCE_MS);
  }

  _connect() {
    if (this.stopped) return;
    const ws = new WebSocket(api.wsUrl('/ws'));
    this.ws = ws;
    let opened = false;
    ws.onopen = () => {
      opened = true;
      this._armSilence();
    };
    ws.onmessage = async (ev) => {
      this._armSilence();
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      switch (msg.type) {
        case 'hello':
          if (msg.principal) store.principal = msg.principal;
          break;
        case 'state':
          store.setState(msg.state);
          if (this.offline || this.attempt > 0) {
            clearTimeout(this.offlineTimer);
            const wasOffline = this.offline;
            this.offline = false;
            this.attempt = 0;
            if (wasOffline) await this.cb.onOnline();
          }
          this.attempt = 0;
          break;
        case 'device':
          store.setDevice(msg.device);
          break;
        case 'media':
          store.setMedia(msg.media);
          break;
        case 'streamsReplaced':
          this.cb.onReplaced(msg.streams || []);
          break;
        case 'identify':
          store.emit('identify', msg);
          break;
        case 'closing':
          if (msg.reason === 'revoked') this.revoked = true;
          break;
      }
    };
    ws.onclose = (ev) => {
      clearTimeout(this.silenceTimer);
      this.ws = null;
      if (this.stopped) return;
      if (ev.code === 4403 || this.revoked) {
        this.revoked = false;
        this.cb.onRevoked();
        return;
      }
      if (!opened && this.attempt === 0) this._markOffline(0);
      else this._markOffline(OFFLINE_DELAY_MS);
      const delay = BACKOFF[Math.min(this.attempt, BACKOFF.length - 1)];
      this.attempt++;
      this.retryTimer = setTimeout(() => this._reconnect(), delay);
    };
    ws.onerror = () => {};
  }

  _markOffline(delay) {
    if (this.offline) return;
    clearTimeout(this.offlineTimer);
    this.offlineTimer = setTimeout(() => {
      if (this.stopped || (this.ws && this.ws.readyState === WebSocket.OPEN)) return;
      this.offline = true;
      this.cb.onOffline();
    }, delay);
  }

  async _reconnect() {
    if (this.stopped) return;
    // after a server restart in-memory sessions are gone: let the app
    // re-authenticate (kiosk: with the URL key) before reopening /ws
    if (this.offline || this.attempt > 1) {
      try {
        const ok = await this.cb.onReauth();
        if (ok === false) return; // app took over (login screen / blocked)
      } catch {
        // still unreachable
        const delay = BACKOFF[Math.min(this.attempt, BACKOFF.length - 1)];
        this.attempt++;
        this.retryTimer = setTimeout(() => this._reconnect(), delay);
        return;
      }
    }
    this._connect();
  }
}
