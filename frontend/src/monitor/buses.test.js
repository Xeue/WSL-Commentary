/**
 * Tests for the transceiver plan and the mid-to-bus map.
 *
 * Owner: WP-5a. `cd frontend && node --test src/monitor/`
 *
 * These are guard-rail tests, not arithmetic tests. The map is measured
 * configuration (docs/test-results.md §2.1) and the plan is what makes it hold
 * positionally; both are the kind of thing that gets "tidied" by someone who
 * does not know that mid 2 is the only bus the commentator can safely hear.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  VIDEO_MID,
  AUDIO_MIDS,
  TRANSCEIVER_PLAN,
  DEFAULT_RETURN_MID,
  BUS_MAP,
  busForMid,
  isValidReturnMid,
  normaliseReturnMid,
  checkMidAssignment,
} from './buses.js';

test('the transceiver plan is exactly what spec §7 requires', () => {
  assert.equal(TRANSCEIVER_PLAN.length, 8, 'eight transceivers: 1 video + 7 audio');
  assert.equal(TRANSCEIVER_PLAN[0], 'video', 'video first, so it takes mid 0');
  for (let i = 1; i < 8; i++) {
    assert.equal(TRANSCEIVER_PLAN[i], 'audio', `mid ${i} must be audio`);
  }
  assert.equal(VIDEO_MID, 0);
  assert.deepEqual([...AUDIO_MIDS], [1, 2, 3, 4, 5, 6, 7]);
});

test('the default return is mid 4, mon1/MIC1 ("Monitor 1")', () => {
  // The operator's chosen default: MIC1, a mix-minus (N-1) derived from the mic
  // inputs, so a commentator on it hears the match without hearing themselves
  // delayed by the ~489 ms cloud round trip.
  assert.equal(DEFAULT_RETURN_MID, 4);
  assert.equal(busForMid(4).bus, 'mon1');
  assert.equal(busForMid(4).sony, 'MIC1');
});

test('the measured mid-to-bus map', async (t) => {
  // docs/test-results.md §2.1: tone injected on one bus at a time, read back,
  // then swapped as a control. 94-95 dB isolation.
  const cases = [
    { mid: 0, kind: 'video', bus: 'multiviewer' },
    { mid: 1, kind: 'audio', bus: 'master', sony: 'PGM' },
    { mid: 2, kind: 'audio', bus: 'aux1', sony: 'CLN' },
    { mid: 3, kind: 'audio', bus: 'aux2', sony: 'MON' },
    { mid: 4, kind: 'audio', bus: 'mon1', sony: 'MIC1' },
    { mid: 5, kind: 'audio', bus: 'mon2', sony: 'MIC2' },
    { mid: 6, kind: 'audio', bus: 'mon3', sony: 'MIC3' },
    { mid: 7, kind: 'audio', bus: 'mon4', sony: 'PFL' },
  ];

  for (const c of cases) {
    await t.test(`mid ${c.mid} is ${c.bus}`, () => {
      const d = busForMid(c.mid);
      assert.ok(d, `mid ${c.mid} must be in the map`);
      assert.equal(d.mid, c.mid);
      assert.equal(d.kind, c.kind);
      assert.equal(d.bus, c.bus);
      if (c.sony) assert.equal(d.sony, c.sony);
    });
  }

  await t.test('the map covers exactly the eight offered m-lines', () => {
    assert.equal(BUS_MAP.length, 8);
    BUS_MAP.forEach((d, i) => assert.equal(d.mid, i, 'index must equal mid'));
  });
});

test('busForMid rejects anything outside the plan', async (t) => {
  const cases = [-1, 8, 99, 1.5, NaN, Infinity, undefined, null, '2'];
  for (const c of cases) {
    await t.test(`busForMid(${String(c)}) is null`, () => {
      assert.equal(busForMid(c), null);
    });
  }
});

test('isValidReturnMid', async (t) => {
  const cases = [
    { in: 1, want: true },
    { in: 2, want: true },
    { in: 7, want: true },
    { in: 0, want: false, why: 'mid 0 is the video mosaic, not a bus' },
    { in: 8, want: false },
    { in: -1, want: false },
    { in: 2.5, want: false },
    { in: '2', want: false, why: 'strings are normalised elsewhere, not accepted here' },
    { in: NaN, want: false },
    { in: undefined, want: false },
    { in: null, want: false },
  ];
  for (const c of cases) {
    await t.test(`${String(c.in)} -> ${c.want}${c.why ? ` (${c.why})` : ''}`, () => {
      assert.equal(isValidReturnMid(c.in), c.want);
    });
  }
});

test('normaliseReturnMid', async (t) => {
  const cases = [
    { name: 'a valid mid passes through', in: 1, fallback: undefined, want: 1 },
    { name: 'the MIC1 default passes through', in: 4, fallback: undefined, want: 4 },
    { name: 'PFL passes through', in: 7, fallback: undefined, want: 7 },
    {
      name: 'a numeric string from config.json is accepted',
      in: '3',
      fallback: undefined,
      want: 3,
    },
    { name: 'mid 0 is not a bus and falls back', in: 0, fallback: undefined, want: 4 },
    { name: 'out of range falls back', in: 12, fallback: undefined, want: 4 },
    { name: 'undefined falls back', in: undefined, fallback: undefined, want: 4 },
    { name: 'NaN falls back', in: NaN, fallback: undefined, want: 4 },
    { name: 'garbage falls back', in: 'CLN', fallback: undefined, want: 4 },
    {
      name: 'an explicit fallback is honoured — this is how setReturnMid keeps the current bus',
      in: 'nonsense',
      fallback: 5,
      want: 5,
    },
    {
      name: 'a bad explicit fallback falls back to the MIC1 default',
      in: 'nonsense',
      fallback: 99,
      want: 4,
    },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      assert.equal(normaliseReturnMid(c.in, c.fallback), c.want);
    });
  }
});

test('checkMidAssignment', async (t) => {
  const inOrder = (n) => Array.from({ length: n }, (_, i) => ({ mid: String(i) }));

  const cases = [
    {
      name: 'eight transceivers in offer order have no problems',
      in: inOrder(8),
      wantCount: 0,
    },
    {
      name: 'numeric mids are compared as strings, so a browser using numbers still passes',
      in: Array.from({ length: 8 }, (_, i) => ({ mid: i })),
      wantCount: 0,
    },
    {
      name: 'an unnegotiated transceiver is reported',
      in: [{ mid: '0' }, { mid: null }, { mid: '2' }],
      wantCount: 1,
    },
    {
      name: 'an undefined mid is reported',
      in: [{ mid: '0' }, { mid: undefined }],
      wantCount: 1,
    },
    {
      name: 'a swapped pair is two problems',
      in: [{ mid: '0' }, { mid: '2' }, { mid: '1' }, { mid: '3' }],
      wantCount: 2,
    },
    {
      name: 'a wholesale re-map is reported for every m-line',
      in: [{ mid: '7' }, { mid: '6' }, { mid: '5' }, { mid: '4' }],
      wantCount: 4,
    },
    { name: 'no transceivers is no problems', in: [], wantCount: 0 },
  ];

  for (const c of cases) {
    await t.test(c.name, () => {
      const problems = checkMidAssignment(c.in);
      assert.equal(problems.length, c.wantCount, problems.join(' | '));
    });
  }

  await t.test('the message names the bus that would be wrong', () => {
    const problems = checkMidAssignment([{ mid: '0' }, { mid: '2' }, { mid: '1' }]);
    assert.ok(
      problems.some((p) => p.includes('master')),
      `expected a mention of the master bus in: ${problems.join(' | ')}`,
    );
    assert.ok(
      problems.some((p) => p.includes('aux1')),
      `expected a mention of aux1 in: ${problems.join(' | ')}`,
    );
  });
});
