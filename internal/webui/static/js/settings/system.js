// System settings.
import api from '../api.js';
import { h, field, checkbox } from '../dom.js';
import { setMessage, toast } from '../dialog.js';
import { guarded } from './index.js';

export function renderSystem(root, ctx) {
  const s = JSON.parse(JSON.stringify(ctx.data.system));
  const m = s.media;
  const msg = h('div', { hidden: true });
  const num = (v, attrs = {}) => h('input', { type: 'number', value: String(v), ...attrs });
  const txt = (v, attrs = {}) => h('input', { type: 'text', value: v || '', ...attrs });

  const f = {
    name: txt(s.name, { maxlength: '80' }),
    host: txt(s.listenHost),
    port: num(s.listenPort, { min: '1', max: '65535' }),
    health: num(s.healthIntervalMs / 1000, { min: '5', max: '3600', step: '1' }),
    rmin: num(s.reconnectMinMs / 1000, { min: '1', max: '600', step: '1' }),
    rmax: num(s.reconnectMaxMs / 1000, { min: '1', max: '3600', step: '1' }),
    media: checkbox('Media Layer aktiv', m.enabled),
    warm: checkbox('Multiview-Streams warm halten', m.preloadMultiview),
    warmMain: checkbox('Mainstreams warm halten', m.preloadMain),
    webrtc: checkbox('WebRTC erlauben (nicht für Kiosk)', m.allowWebRtc),
    url: txt(m.go2rtcUrl),
    reconcile: num(m.reconcileIntervalMs / 1000, { min: '2', max: '600', step: '1' }),
    manage: checkbox('go2rtc selbst verwalten', m.manageGo2rtc),
    bin: txt(m.go2rtcBin, { placeholder: 'automatisch (bin/go2rtc oder PATH)' }),
    ffmpeg: txt(m.ffmpegBin, { placeholder: 'automatisch (PATH)' }),
    cfg: txt(m.go2rtcConfigPath),
    apiListen: txt(m.go2rtcApiListen),
    rtsp: txt(m.go2rtcRtspListen),
    webrtcListen: txt(m.go2rtcWebrtcListen),
  };

  const proc = ctx.data.process;
  const media = ctx.data.media || {};
  root.append(
    h('div', { class: 'toolbar' }, h('h2', {}, 'System')),
    msg,
    h('div', { class: 'section-title' }, 'Allgemein'),
    h('div', { class: 'form-grid' },
      field('Systemname', f.name),
      field('CNM Listen Host', f.host, 'Wirksam nach Dienstneustart'),
      field('CNM Listen Port', f.port, 'Wirksam nach Dienstneustart'),
    ),
    h('div', { class: 'section-title' }, 'Geräte'),
    h('div', { class: 'form-grid' },
      field('Healthcheck-Intervall (s)', f.health),
      field('Reconnect Minimum (s)', f.rmin),
      field('Reconnect Maximum (s)', f.rmax),
    ),
    h('div', { class: 'section-title' }, 'Media'),
    h('div', { class: 'form-grid' },
      h('div', { class: 'field wide' }, h('div', { class: 'row', style: { gap: '18px' } }, f.media, f.warm, f.warmMain, f.webrtc)),
      field('go2rtc API URL', f.url),
      field('Media Reconcile-Intervall (s)', f.reconcile),
      field('FFmpeg Binary', f.ffmpeg),
    ),
    h('div', { class: 'msg', style: { marginTop: '10px' } },
      `Media-Status: ${media.state || '–'}${media.version ? ` · go2rtc ${media.version}` : ''}${media.preloadSupported === false ? ' · Preload nicht unterstützt' : ''}`,
      proc && proc.managed ? ` · verwalteter Prozess ${proc.running ? `läuft (PID ${proc.pid})` : 'gestoppt'}${proc.restarts ? `, ${proc.restarts} Neustarts` : ''}${proc.lastError ? ` · ${proc.lastError}` : ''}` : '',
    ),
    h('div', { class: 'section-title' }, 'Verwaltetes go2rtc'),
    h('div', { class: 'form-grid' },
      h('div', { class: 'field wide' }, f.manage),
      field('go2rtc Binary', f.bin),
      field('go2rtc Config-Pfad', f.cfg),
      field('API Listen', f.apiListen, 'Möglichst nur 127.0.0.1'),
      field('RTSP Relay Listen', f.rtsp),
      field('WebRTC Listen', f.webrtcListen, 'Nur aktiv, wenn WebRTC erlaubt ist'),
    ),
    h('div', { class: 'row', style: { marginTop: '18px' } },
      h('span', { class: 'spacer' }),
      h('button', { class: 'btn primary', onclick: save }, 'Speichern'),
    ),
  );

  async function save() {
    setMessage(msg, '');
    const sec = (el) => Math.round(parseFloat(el.value) * 1000);
    const body = {
      ...s,
      name: f.name.value.trim(),
      listenHost: f.host.value.trim(),
      listenPort: parseInt(f.port.value, 10),
      healthIntervalMs: sec(f.health),
      reconnectMinMs: sec(f.rmin),
      reconnectMaxMs: sec(f.rmax),
      media: {
        ...m,
        enabled: f.media.input.checked,
        preloadMultiview: f.warm.input.checked,
        preloadMain: f.warmMain.input.checked,
        allowWebRtc: f.webrtc.input.checked,
        go2rtcUrl: f.url.value.trim(),
        reconcileIntervalMs: sec(f.reconcile),
        manageGo2rtc: f.manage.input.checked,
        go2rtcBin: f.bin.value.trim(),
        ffmpegBin: f.ffmpeg.value.trim(),
        go2rtcConfigPath: f.cfg.value.trim(),
        go2rtcApiListen: f.apiListen.value.trim(),
        go2rtcRtspListen: f.rtsp.value.trim(),
        go2rtcWebrtcListen: f.webrtcListen.value.trim(),
      },
    };
    const r = await guarded(() => api.put('/api/settings/system', body), msg);
    if (!r) return;
    if (r.restartRequired) setMessage(msg, 'Gespeichert. Listen Host/Port werden beim nächsten Dienststart wirksam.', 'warn');
    else toast('Systemeinstellungen gespeichert');
    ctx.data.system = r.system;
  }
  return null;
}
