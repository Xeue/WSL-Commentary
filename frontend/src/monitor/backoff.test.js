/**
 * Tests for the restart delay ladder.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  restartDelayMs,
  baseDelayMs,
  RESTART_LADDER_MS,
  RESTART_CAP_MS,
  JITTER_FRACTION,
} from './backoff.js';

test('the ladder is 1, 2, 5, 10 then 15 s capped', () => {
  assert.deepEqual([...RESTART_LADDER_MS], [1_000, 2_000, 5_000, 10_000]);
  assert.equal(RESTART_CAP_MS, 15_000);
});

test('baseDelayMs', async (t) => {
  const cases = [
    { name: 'the first restart is quick — the commentator is looking at it', attempt: 0, want: 1_000 },
    { attempt: 1, want: 2_000 },
    { attempt: 2, want: 5_000 },
    { attempt: 3, want: 10_000 },
    { name: 'the fifth failure and everything after it is capped', attempt: 4, want: 15_000 },
    { attempt: 50, want: 15_000 },
    { name: 'a fifteen-hour outage is still 15 s, never longer', attempt: 3600, want: 15_000 },
    { name: 'a negative attempt is treated as the first', attempt: -1, want: 1_000 },
    { name: 'a non-integer attempt is treated as the first', attempt: 1.5, want: 1_000 },
    { name: 'NaN is treated as the first', attempt: NaN, want: 1_000 },
  ];
  for (const c of cases) {
    await t.test(c.name || `attempt ${c.attempt} -> ${c.want} ms`, () => {
      assert.equal(baseDelayMs(c.attempt), c.want);
    });
  }
});

test('restartDelayMs applies bounded jitter', async (t) => {
  const cases = [
    { name: 'the low end of the jitter range', random: () => 0, attempt: 0, want: 850 },
    { name: 'the middle of the jitter range is the base delay', random: () => 0.5, attempt: 0, want: 1_000 },
    { name: 'the high end of the jitter range', random: () => 1, attempt: 0, want: 1_150 },
    { name: 'the cap jitters too', random: () => 1, attempt: 99, want: Math.round(15_000 * 1.15) },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      assert.equal(restartDelayMs(c.attempt, c.random), c.want);
    });
  }
});

test('restartDelayMs never returns something a setTimeout would misbehave on', () => {
  const randoms = [() => 0, () => 0.5, () => 1, () => NaN, Math.random, () => 'x'];
  for (const r of randoms) {
    for (const attempt of [0, 1, 2, 3, 4, 100, -5, NaN, 1.5]) {
      const d = restartDelayMs(attempt, r);
      assert.ok(Number.isFinite(d), `attempt ${attempt} gave ${d}`);
      assert.ok(d >= 0, `attempt ${attempt} gave ${d}`);
      assert.ok(Number.isInteger(d), `attempt ${attempt} gave ${d}`);
    }
  }
});

test('the jittered delay stays within the fraction of the base', () => {
  for (let attempt = 0; attempt <= 8; attempt++) {
    const base = baseDelayMs(attempt);
    for (let i = 0; i < 500; i++) {
      const d = restartDelayMs(attempt);
      assert.ok(
        d >= Math.floor(base * (1 - JITTER_FRACTION)) && d <= Math.ceil(base * (1 + JITTER_FRACTION)),
        `attempt ${attempt}: ${d} outside +/-${JITTER_FRACTION * 100}% of ${base}`,
      );
    }
  }
});

test('the monitor ladder is faster than the SRT ladder, on purpose', () => {
  // spec §6.2's SRT ladder starts at 7 s because M2L-X's listener refuses
  // re-accept for ~5 s and there is a one-peer race to lose. A KVS signalling
  // channel serves ten viewers, so there is no race here and no reason to leave
  // the commentator's picture dark for seven seconds.
  assert.ok(RESTART_LADDER_MS[0] < 6_000);
  assert.ok(RESTART_CAP_MS <= 30_000);
});
