// Pure timeline helpers (no DOM) – unit tested with node --test.

export const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

/**
 * Snap a requested absolute time onto recorded material.
 * - inside a recording → unchanged
 * - in a gap → start of the next recording
 * - after the last recording → shortly before the end of the last one
 * @param {number} t requested time (ms)
 * @param {{start:number,end:number}[]} ranges sorted recordings
 * @returns {{t:number, snapped:boolean, direction?:'next'|'last'}}
 */
export function snapToRecording(t, ranges) {
  if (!ranges || !ranges.length) return { t, snapped: false };
  for (const r of ranges) {
    if (t >= r.start && t < r.end) return { t, snapped: false };
  }
  for (const r of ranges) {
    if (r.start > t) return { t: r.start, snapped: true, direction: 'next' };
  }
  const last = ranges[ranges.length - 1];
  return { t: Math.max(last.start, last.end - 5000), snapped: true, direction: 'last' };
}

const STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600].map((s) => s * 1000);

/** Choose a tick interval so that ticks are at least minPx apart. */
export function tickStep(spanMs, widthPx, minPx = 70) {
  if (spanMs <= 0 || widthPx <= 0) return STEPS[STEPS.length - 1];
  for (const s of STEPS) {
    if ((s / spanMs) * widthPx >= minPx) return s;
  }
  return STEPS[STEPS.length - 1];
}

/** Tick positions (local time aligned) within [from, to]. */
export function ticks(from, to, step) {
  const out = [];
  // align to local midnight so ticks fall on round wall clock values
  const d = new Date(from);
  const midnight = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  let t = midnight + Math.ceil((from - midnight) / step) * step;
  for (let i = 0; t <= to && i < 2000; i++, t += step) out.push(t);
  return out;
}

const pad = (n) => String(n).padStart(2, '0');

/** Hover label: HH:MM:SS, with date when the range spans several days. */
export function formatHover(t, from, to) {
  const d = new Date(t);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  const a = new Date(from), b = new Date(to);
  const multiDay = a.getFullYear() !== b.getFullYear() || a.getMonth() !== b.getMonth() || a.getDate() !== b.getDate();
  return multiDay ? `${pad(d.getDate())}.${pad(d.getMonth() + 1)}. ${time}` : time;
}

/** Label for a tick (seconds only for sub-minute steps). */
export function formatTick(t, step) {
  const d = new Date(t);
  if (step < 60000) return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  if (d.getHours() === 0 && d.getMinutes() === 0) return `${pad(d.getDate())}.${pad(d.getMonth() + 1)}.`;
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** Convert an x position on the track into an absolute time. */
export function timeAt(x, width, from, to) {
  if (width <= 0) return from;
  return from + clamp(x / width, 0, 1) * (to - from);
}

/** Convert an absolute time into a percentage position. */
export function percentOf(t, from, to) {
  if (to <= from) return 0;
  return clamp(((t - from) / (to - from)) * 100, 0, 100);
}

/** Validate a playback range. */
export const MAX_RANGE_MS = 24 * 3600 * 1000;
export function validateRange(from, to) {
  if (!Number.isFinite(from) || !Number.isFinite(to)) return 'Bitte Start und Ende angeben.';
  if (from >= to) return 'Start muss vor dem Ende liegen.';
  if (to - from > MAX_RANGE_MS) return 'Der Zeitraum darf höchstens 24 Stunden umfassen.';
  return '';
}
