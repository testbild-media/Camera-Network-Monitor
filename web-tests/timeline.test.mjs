// node --test web-tests/
import test from 'node:test';
import assert from 'node:assert/strict';
import { snapToRecording, tickStep, ticks, formatHover, timeAt, percentOf, validateRange } from '../internal/webui/static/js/timeline.js';

const R = [{ start: 1000, end: 2000 }, { start: 5000, end: 8000 }];

test('inside a recording stays unchanged', () => {
  assert.deepEqual(snapToRecording(1500, R), { t: 1500, snapped: false });
});

test('gap snaps to next recording', () => {
  assert.deepEqual(snapToRecording(3000, R), { t: 5000, snapped: true, direction: 'next' });
  assert.deepEqual(snapToRecording(0, R), { t: 1000, snapped: true, direction: 'next' });
});

test('after the last recording clamps into the last range', () => {
  const r = snapToRecording(9000, R);
  assert.equal(r.snapped, true);
  assert.equal(r.direction, 'last');
  assert.ok(r.t >= 5000 && r.t < 8000);
});

test('no ranges → unchanged', () => {
  assert.deepEqual(snapToRecording(42, []), { t: 42, snapped: false });
});

test('tick step keeps minimum spacing', () => {
  const span = 15 * 60 * 1000;
  const step = tickStep(span, 1000, 70);
  assert.ok((step / span) * 1000 >= 70);
  assert.equal(tickStep(24 * 3600 * 1000, 1200, 70), 2 * 3600 * 1000);
});

test('ticks lie within the range and are ascending', () => {
  const from = new Date(2026, 0, 1, 10, 3, 17).getTime();
  const to = from + 3600 * 1000;
  const t = ticks(from, to, 600000);
  assert.ok(t.length >= 5);
  for (let i = 0; i < t.length; i++) {
    assert.ok(t[i] >= from && t[i] <= to);
    if (i) assert.ok(t[i] > t[i - 1]);
    assert.equal(new Date(t[i]).getMinutes() % 10, 0);
  }
});

test('hover format adds the date only across midnight', () => {
  const a = new Date(2026, 0, 1, 23, 50, 0).getTime();
  assert.equal(formatHover(a, a, a + 5 * 60000), '23:50:00');
  assert.equal(formatHover(a, a, a + 20 * 60000), '01.01. 23:50:00');
});

test('position conversions', () => {
  assert.equal(timeAt(50, 100, 0, 1000), 500);
  assert.equal(timeAt(-5, 100, 0, 1000), 0);
  assert.equal(percentOf(250, 0, 1000), 25);
});

test('range validation', () => {
  assert.ok(validateRange(10, 5));
  assert.ok(validateRange(0, 25 * 3600 * 1000));
  assert.equal(validateRange(0, 24 * 3600 * 1000), '');
});
