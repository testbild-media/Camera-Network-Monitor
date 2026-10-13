// Small DOM helpers. All text is set via textContent – never innerHTML with data.

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k === 'disabled') el.disabled = !!v;
    else if (k === 'selected') el.selected = !!v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, String(v));
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

// busy shows a spinner in a button while the async action runs.
export async function busy(btn, fn) {
  if (btn.classList.contains('loading')) return undefined;
  btn.disabled = true;
  btn.classList.add('loading');
  try {
    return await fn();
  } finally {
    btn.classList.remove('loading');
    btn.disabled = false;
  }
}

// loadingBox is a placeholder shown while content loads.
export function loadingBox(text = 'Lädt …') {
  return h('div', { class: 'loading-box' }, h('span', { class: 'spin' }), text);
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function option(value, label, selected) {
  return h('option', { value, selected: selected ? true : undefined }, label);
}

export function select(options, value, attrs = {}) {
  const el = h('select', attrs);
  for (const o of options) {
    const opt = option(o.value, o.label, o.value === value);
    if (o.disabled) opt.disabled = true;
    el.appendChild(opt);
  }
  el.value = value ?? (options[0] && options[0].value);
  return el;
}

export function field(label, input, hint) {
  return h('label', { class: 'field' }, h('span', {}, label), input, hint ? h('span', { class: 'hint' }, hint) : null);
}

export function checkbox(label, checked, attrs = {}) {
  const input = h('input', { type: 'checkbox', checked, ...attrs });
  const el = h('label', { class: 'check' }, input, label);
  el.input = input;
  return el;
}

const pad = (n, l = 2) => String(n).padStart(l, '0');

export function fmtTime(ms) {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function fmtDate(ms) {
  const d = new Date(ms);
  return `${pad(d.getDate())}.${pad(d.getMonth() + 1)}.${d.getFullYear()}`;
}

export function fmtDateTime(ms) {
  return `${fmtDate(ms)} ${fmtTime(ms)}`;
}

export function sameDay(a, b) {
  const x = new Date(a), y = new Date(b);
  return x.getFullYear() === y.getFullYear() && x.getMonth() === y.getMonth() && x.getDate() === y.getDate();
}

// value for <input type="datetime-local" step="1">
export function toLocalInput(ms) {
  const d = new Date(ms);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function fromLocalInput(v) {
  if (!v) return NaN;
  const t = new Date(v).getTime();
  return t;
}

export function fmtDuration(ms) {
  const s = Math.round(ms / 1000);
  const hh = Math.floor(s / 3600), mm = Math.floor((s % 3600) / 60), ss = s % 60;
  if (hh) return `${hh} h ${pad(mm)} min`;
  if (mm) return `${mm} min ${pad(ss)} s`;
  return `${ss} s`;
}

export function fmtAgo(iso) {
  if (!iso) return '–';
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 60000) return 'gerade eben';
  if (ms < 3600000) return `vor ${Math.round(ms / 60000)} min`;
  if (ms < 86400000) return `vor ${Math.round(ms / 3600000)} h`;
  return fmtDateTime(new Date(iso).getTime());
}
