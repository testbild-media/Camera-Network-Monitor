// Client side application state with a tiny event emitter.

class Store extends EventTarget {
  constructor() {
    super();
    this.kiosk = false;
    this.principal = null;
    this.prefs = {};         // personal preferences of the logged in user
    this.state = null;       // PublicState from the server
    this.media = null;       // media summary
    this.devices = new Map(); // admin: device status by id
    this.cameras = new Map();
  }

  emit(type, detail) { this.dispatchEvent(new CustomEvent(type, { detail })); }
  on(type, fn) {
    const h = (e) => fn(e.detail);
    this.addEventListener(type, h);
    return () => this.removeEventListener(type, h);
  }

  setState(state) {
    this.state = state;
    this.cameras = new Map((state.cameras || []).map((c) => [c.id, c]));
    this.emit('state', state);
  }

  setMedia(media) {
    this.media = media;
    this.emit('media', media);
  }

  setDevice(status) {
    this.devices.set(status.id, status);
    this.emit('device', status);
  }

  isAdmin() {
    return !!this.principal && this.principal.kind === 'user' && this.principal.role === 'admin';
  }

  camera(id) { return this.cameras.get(id); }

  // Display name priority: slot label → channel label → camera
  // (device reported) name → generic fallback.
  cameraName(cameraId, slotLabel) {
    if (slotLabel) return slotLabel;
    const c = this.cameras.get(cameraId);
    if (!c) return slotLabel || 'Kamera';
    if (c.label) return c.label;
    if (c.deviceName) return c.deviceName;
    return `${c.deviceLabel || c.deviceId} · Kanal ${c.channel + 1}`;
  }
}

const store = new Store();
export default store;
