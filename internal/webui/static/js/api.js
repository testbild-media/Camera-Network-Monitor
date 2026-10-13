// API client. The kiosk identity is selected explicitly per request
// (X-CNM-Kiosk header / kiosk query parameter) so that an admin tab and a
// kiosk tab in the same browser never share an identity.
//
// Every user initiated request shows the global progress bar (spec: every
// loading action shows a loading indicator); background polls pass
// {background: true}.

export class ApiError extends Error {
  constructor(status, code, message, body) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code || '';
    this.body = body;
  }
  get offline() { return this.status === 0; }
}

// ---- global progress bar ---------------------------------------------------------------------------
let pending = 0;
let barTimer = null;
let bar = null;

function progressBar() {
  if (!bar) {
    bar = document.createElement('div');
    bar.className = 'progress';
    bar.setAttribute('role', 'progressbar');
    bar.setAttribute('aria-label', 'Lädt');
    document.body.appendChild(bar);
  }
  return bar;
}

export function beginLoading() {
  pending++;
  if (pending === 1) {
    clearTimeout(barTimer);
    // short delay avoids flicker for instant responses
    barTimer = setTimeout(() => { if (pending > 0) progressBar().classList.add('on'); }, 120);
  }
}

export function endLoading() {
  pending = Math.max(0, pending - 1);
  if (pending === 0) {
    clearTimeout(barTimer);
    if (bar) bar.classList.remove('on');
  }
}

// track wraps any promise with the global loading indicator.
export async function track(promise) {
  beginLoading();
  try {
    return await promise;
  } finally {
    endLoading();
  }
}

const api = {
  kioskMonitor: '',
  csrf: '',
  onUnauthorized: null,

  setKiosk(mon) { this.kioskMonitor = mon || ''; },
  setCSRF(token) { this.csrf = token || ''; },

  headers(extra = {}) {
    const h = { Accept: 'application/json', ...extra };
    if (this.kioskMonitor) h['X-CNM-Kiosk'] = this.kioskMonitor;
    if (this.csrf) h['X-CSRF-Token'] = this.csrf;
    return h;
  },

  // URL helper for resources fetched without custom headers (WebSocket, media)
  url(path, params = {}) {
    const u = new URL(path, location.origin);
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null) u.searchParams.set(k, v);
    if (this.kioskMonitor) u.searchParams.set('kiosk', this.kioskMonitor);
    return u;
  },

  wsUrl(path, params = {}) {
    const u = this.url(path, params);
    u.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return u.toString();
  },

  async request(method, path, body, { signal, raw = false, quietAuth = false, background = false } = {}) {
    if (!background) beginLoading();
    try {
      let resp;
      try {
        resp = await fetch(path, {
          method,
          headers: this.headers(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
          body: body !== undefined ? JSON.stringify(body) : undefined,
          credentials: 'same-origin',
          cache: 'no-store',
          signal,
        });
      } catch (e) {
        if (e.name === 'AbortError') throw e;
        throw new ApiError(0, 'NETWORK', 'Server nicht erreichbar');
      }
      if (raw) return resp;
      let data = null;
      const text = await resp.text();
      if (text) {
        try { data = JSON.parse(text); } catch { data = null; }
      }
      if (!resp.ok) {
        const err = data && data.error ? data.error : {};
        if (resp.status === 401 && !quietAuth && this.onUnauthorized) this.onUnauthorized();
        throw new ApiError(resp.status, err.code || (resp.status === 401 ? 'UNAUTHORIZED' : ''), err.message || `Fehler ${resp.status}`, data);
      }
      return data;
    } finally {
      if (!background) endLoading();
    }
  },

  get(path, opts) { return this.request('GET', path, undefined, opts); },
  post(path, body = {}, opts) { return this.request('POST', path, body, opts); },
  put(path, body = {}, opts) { return this.request('PUT', path, body, opts); },
  del(path, opts) { return this.request('DELETE', path, undefined, opts); },
};

export default api;
