// Modal layer stack.
// - Clicking the dark backdrop never closes a dialog.
// - Escape closes only the top-most layer: dialogs first, then the focus view.
import { h, clear } from './dom.js';

const stack = [];        // open dialogs (top = last)
const escHandlers = [];  // lower layers (e.g. focus view), top = last
let toastBox;

function layersRoot() {
  return document.getElementById('layers') || document.body;
}

document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape' || ev.defaultPrevented) return;
  if (stack.length) {
    ev.preventDefault();
    const top = stack[stack.length - 1];
    if (top.escape !== false) top.close('escape');
    return;
  }
  if (escHandlers.length) {
    ev.preventDefault();
    escHandlers[escHandlers.length - 1]();
  }
});

// registerEscapeLayer adds a non-dialog layer (focus view) to the Escape chain.
export function registerEscapeLayer(fn) {
  escHandlers.push(fn);
  return () => {
    const i = escHandlers.indexOf(fn);
    if (i >= 0) escHandlers.splice(i, 1);
  };
}

export function dialogOpen() { return stack.length > 0; }

/**
 * openDialog({ title, body, actions:[{label, kind, onClick, left}], size, escape, onClose })
 * onClick may return false (or a Promise resolving to false) to keep the dialog open.
 */
export function openDialog({ title, body, actions = [], size = '', escape = true, enterSubmits = true, onClose } = {}) {
  const foot = h('div', { class: 'dialog-foot' });
  const left = h('div', { class: 'left' });
  const closeBtn = h('button', { class: 'btn ghost icon', title: 'Schließen', 'aria-label': 'Schließen' }, '✕');
  const bodyEl = h('div', { class: 'dialog-body' });
  if (body) bodyEl.appendChild(body);
  const dlg = h('div', { class: `dialog ${size}`, role: 'dialog', 'aria-modal': 'true' },
    h('div', { class: 'dialog-head' }, h('h2', {}, title || ''), closeBtn),
    bodyEl,
    foot,
  );
  const backdrop = h('div', { class: 'backdrop' }, dlg);
  const entry = { escape, closed: false, close: null, el: dlg, body: bodyEl, buttons: [] };
  const prevFocus = document.activeElement;

  entry.close = (reason) => {
    if (entry.closed) return;
    entry.closed = true;
    const i = stack.indexOf(entry);
    if (i >= 0) stack.splice(i, 1);
    backdrop.remove();
    if (prevFocus && prevFocus.focus) prevFocus.focus();
    if (onClose) onClose(reason);
  };
  closeBtn.addEventListener('click', () => entry.close('close'));

  for (const a of actions) {
    const b = h('button', { class: `btn ${a.kind || ''}` }, a.label);
    b.addEventListener('click', async () => {
      if (!a.onClick) { entry.close(a.label); return; }
      b.disabled = true;
      const spinTimer = setTimeout(() => b.classList.add('loading'), 150);
      try {
        const r = await a.onClick(entry);
        if (r !== false) entry.close(a.label);
      } finally {
        clearTimeout(spinTimer);
        b.classList.remove('loading');
        b.disabled = false;
      }
    });
    entry.buttons.push(b);
    (a.left ? left : foot).appendChild(b);
  }
  if (left.childNodes.length) foot.prepend(left);
  if (!actions.length) foot.remove();

  // Enter in a plain input field triggers the primary action (not in search
  // fields, text areas or dialogs that opt out with enterSubmits:false).
  const primary = entry.buttons[actions.findIndex((a) => /\bprimary\b/.test(a.kind || ''))];
  if (primary && enterSubmits) {
    dlg.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter' || ev.isComposing || ev.defaultPrevented) return;
      const t = ev.target;
      if (!t || t.tagName !== 'INPUT') return;
      if (/^(search|checkbox|radio|button|submit|file|range|color)$/i.test(t.type)) return;
      ev.preventDefault();
      if (!primary.disabled) primary.click();
    });
  }

  stack.push(entry);
  layersRoot().appendChild(backdrop);
  const first = dlg.querySelector('input:not([type=hidden]), select, textarea, button.primary');
  (first || closeBtn).focus();
  return entry;
}

export function alertDialog(title, message, kind = '') {
  return new Promise((resolve) => {
    openDialog({
      title,
      body: h('div', { class: `msg ${kind}` }, message),
      actions: [{ label: 'OK', kind: 'primary' }],
      onClose: () => resolve(),
    });
  });
}

export function confirmDialog(title, message, { confirmLabel = 'OK', danger = false } = {}) {
  return new Promise((resolve) => {
    let ok = false;
    openDialog({
      title,
      body: h('div', {}, message),
      actions: [
        { label: 'Abbrechen' },
        { label: confirmLabel, kind: danger ? 'danger' : 'primary', onClick: () => { ok = true; } },
      ],
      onClose: () => resolve(ok),
    });
  });
}

export function toast(message, kind = '', ms = 3500) {
  if (!toastBox) {
    toastBox = h('div', { class: 'toasts', 'aria-live': 'polite' });
    document.body.appendChild(toastBox);
  }
  const t = h('div', { class: `toast ${kind}` }, message);
  toastBox.appendChild(t);
  setTimeout(() => t.remove(), ms);
}

// setMessage shows (or clears) an inline message element.
export function setMessage(el, text, kind = 'error') {
  clear(el);
  el.className = text ? `msg ${kind}` : '';
  if (text) el.textContent = text;
  el.hidden = !text;
}
