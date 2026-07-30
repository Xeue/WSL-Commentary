/**
 * Tests for the return-audio gain law.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  dbToLinear,
  linearToDb,
  clampGainDb,
  clampLevel,
  computeGain,
  DEFAULT_GAIN_DB,
  MIN_GAIN_DB,
  MAX_GAIN_DB,
} from './gain.js';

/** close asserts within a tolerance; the gain law is floating point. */
function close(actual, want, epsilon = 1e-9) {
  assert.ok(
    Math.abs(actual - want) <= epsilon,
    `expected ${actual} to be within ${epsilon} of ${want}`,
  );
}

test('the measured default is 18 dB', () => {
  // docs/test-results.md line 121: the monitor track sits ~18 dB below the
  // SRT-ingested peak level. If this constant changes, the return goes quiet.
  assert.equal(DEFAULT_GAIN_DB, 18);
});

test('dbToLinear', async (t) => {
  const cases = [
    { name: '0 dB is unity', db: 0, want: 1 },
    { name: '6 dB is about 2x', db: 6, want: 1.9952623149688795 },
    { name: '20 dB is exactly 10x', db: 20, want: 10 },
    { name: '-20 dB is exactly 0.1x', db: -20, want: 0.1 },
    { name: '40 dB is exactly 100x', db: 40, want: 100 },
    {
      name: 'the measured 18 dB make-up gain is about 7.94x',
      db: 18,
      want: 7.943282347242816,
    },
  ];
  for (const c of cases) {
    await t.test(c.name, () => close(dbToLinear(c.db), c.want, 1e-12));
  }
});

test('linearToDb', async (t) => {
  const cases = [
    { name: 'unity is 0 dB', linear: 1, want: 0 },
    { name: '10x is 20 dB', linear: 10, want: 20 },
    { name: '0.1x is -20 dB', linear: 0.1, want: -20 },
    { name: 'silence is -Infinity, not NaN', linear: 0, want: -Infinity },
    { name: 'a negative amplitude is -Infinity', linear: -1, want: -Infinity },
    { name: 'NaN is -Infinity', linear: NaN, want: -Infinity },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      const got = linearToDb(c.linear);
      if (c.want === -Infinity) assert.equal(got, -Infinity);
      else close(got, c.want, 1e-12);
    });
  }
});

test('clampGainDb fails towards audible', async (t) => {
  const cases = [
    { name: 'the measured default passes through', in: 18, want: 18 },
    { name: 'zero is a legitimate setting', in: 0, want: 0 },
    { name: 'a negative trim is allowed', in: -6, want: -6 },
    { name: 'a numeric string from config.json is accepted', in: '18', want: 18 },
    {
      name: 'undefined falls back to the measured default, not to silence',
      in: undefined,
      want: DEFAULT_GAIN_DB,
    },
    { name: 'null falls back to the measured default', in: null, want: DEFAULT_GAIN_DB },
    { name: 'NaN falls back to the measured default', in: NaN, want: DEFAULT_GAIN_DB },
    { name: 'a non-numeric string falls back', in: 'loud', want: DEFAULT_GAIN_DB },
    { name: 'Infinity falls back', in: Infinity, want: DEFAULT_GAIN_DB },
    { name: 'above the ceiling clamps down', in: 200, want: MAX_GAIN_DB },
    { name: 'below the floor clamps up', in: -200, want: MIN_GAIN_DB },
  ];
  for (const c of cases) {
    await t.test(c.name, () => assert.equal(clampGainDb(c.in), c.want));
  }
});

test('clampLevel fails towards silence', async (t) => {
  const cases = [
    { name: 'unity', in: 1, want: 1 },
    { name: 'a half', in: 0.5, want: 0.5 },
    { name: 'zero', in: 0, want: 0 },
    { name: 'a numeric string from an <input type=range>', in: '0.75', want: 0.75 },
    { name: 'above one clamps down', in: 4, want: 1 },
    { name: 'below zero clamps up', in: -1, want: 0 },
    {
      name: 'NaN resolves to silence, NOT to full gain into headphones',
      in: NaN,
      want: 0,
    },
    { name: 'undefined resolves to silence', in: undefined, want: 0 },
    { name: 'a non-numeric string resolves to silence', in: 'max', want: 0 },
    { name: 'Infinity resolves to silence', in: Infinity, want: 0 },
  ];
  for (const c of cases) {
    await t.test(c.name, () => assert.equal(clampLevel(c.in), c.want));
  }
});

test('computeGain is 10^(dB/20) times the level', async (t) => {
  const cases = [
    {
      name: 'the shipped default: 18 dB at full level',
      db: 18,
      level: 1,
      want: 7.943282347242816,
    },
    {
      name: '18 dB at half level',
      db: 18,
      level: 0.5,
      want: 3.971641173621408,
    },
    { name: '0 dB at full level is unity', db: 0, level: 1, want: 1 },
    { name: '20 dB at full level is 10x', db: 20, level: 1, want: 10 },
    { name: 'any gain at zero level is silence', db: 40, level: 0, want: 0 },
    {
      name: 'a NaN level silences rather than blasting',
      db: 18,
      level: NaN,
      want: 0,
    },
    {
      name: 'a missing gain still uses the measured 18 dB',
      db: undefined,
      level: 1,
      want: 7.943282347242816,
    },
    {
      name: 'nothing can exceed the +40 dB ceiling',
      db: 1000,
      level: 1,
      want: 100,
    },
  ];
  for (const c of cases) {
    await t.test(c.name, () => close(computeGain(c.db, c.level), c.want, 1e-9));
  }
});

test('computeGain is monotonic in the level', () => {
  // A level slider that is not monotonic is a level slider that will surprise
  // someone wearing headphones.
  let previous = -1;
  for (let i = 0; i <= 100; i++) {
    const g = computeGain(DEFAULT_GAIN_DB, i / 100);
    assert.ok(g >= previous, `gain went down between level ${(i - 1) / 100} and ${i / 100}`);
    previous = g;
  }
});

test('computeGain never returns a value a GainNode would reject', () => {
  const nasty = [undefined, null, NaN, Infinity, -Infinity, 'x', {}, [], -1e9, 1e9];
  for (const db of nasty) {
    for (const level of nasty) {
      const g = computeGain(db, level);
      assert.ok(Number.isFinite(g), `computeGain(${String(db)}, ${String(level)}) = ${g}`);
      assert.ok(g >= 0, `computeGain(${String(db)}, ${String(level)}) = ${g}`);
      assert.ok(g <= 100, `computeGain(${String(db)}, ${String(level)}) = ${g}`);
    }
  }
});
