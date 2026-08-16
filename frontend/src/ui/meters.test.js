/**
 * Tests for the input meters' pure model (./meters.js), plus the source
 * guards on how home.js, app.js, backend.js and app.go wire it.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * The behaviour half runs the real module: meters.js is pure (no DOM, no
 * browser API — its one import chain is mixer/model.js -> mixer/contract.js,
 * both pure). The wiring half reads source text, for the reason
 * mixerwiring.test.js documents at length: the failures being guarded are
 * textual (an element appended to the wrong parent, an import that is not
 * there), and this repo has no DOM test library by design (package.json is
 * frozen).
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  dbToFraction,
  meterZones,
  zoneFills,
  isSilentFrame,
  frameChannels,
  createPeakHold,
  PEAK_HOLD_MS,
  PEAK_DECAY_DB_PER_S,
  LEVELS_FLOOR_DB,
  LEVELS_CEIL_DB,
  LEVELS_AMBER_DB,
  LEVELS_RED_DB,
  LEVELS_SILENCE_DB,
} from './meters.js';

// The mixer's OWN constants, imported straight from its source. This is the
// anti-drift assertion the two-tables bug demands: if meters.js ever stops
// deriving its scale from mixer/model.js — a copied number, a "temporary"
// hardcode — the equalities below fail against whatever the mixer actually
// says that day, not against a remembered value.
import {
  METER_FLOOR_DB,
  METER_CEIL_DB,
  METER_AMBER_DB,
  METER_RED_DB,
  meterFraction,
} from './mixer/model.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

/* ------------------------------------------------------------------------ */
/* The scale                                                                 */
/* ------------------------------------------------------------------------ */

test('dbToFraction maps -60..0 dBFS onto 0..1 and clamps beyond both ends', () => {
  assert.equal(dbToFraction(LEVELS_FLOOR_DB), 0);
  assert.equal(dbToFraction(LEVELS_CEIL_DB), 1);
  assert.equal(dbToFraction(-30), 0.5, 'the scale is linear in dB');
  assert.equal(dbToFraction(-100), 0, 'the silence floor clamps to empty');
  assert.equal(dbToFraction(-1000), 0);
  assert.equal(dbToFraction(3), 1, 'above full scale clamps to full');
  assert.equal(dbToFraction('nonsense'), 0, 'non-numeric input clamps rather than escaping the bar');
  assert.equal(dbToFraction(undefined), 0);
});

test('the scale and zone thresholds ARE the mixer meter\'s, not a copy of them', () => {
  // Two meters in one application must be the same instrument. These compare
  // against live imports of the mixer's source, so a decoupling fails here.
  assert.equal(LEVELS_FLOOR_DB, METER_FLOOR_DB, 'floor must equal the mixer\'s METER_FLOOR_DB');
  assert.equal(LEVELS_CEIL_DB, METER_CEIL_DB, 'ceiling must equal the mixer\'s METER_CEIL_DB');
  assert.equal(LEVELS_AMBER_DB, METER_AMBER_DB, 'amber boundary must equal the mixer\'s METER_AMBER_DB');
  assert.equal(LEVELS_RED_DB, METER_RED_DB, 'red boundary must equal the mixer\'s METER_RED_DB');

  // Same curve, not merely same endpoints.
  for (const db of [-60, -47.3, -30, -18, -6, -0.5, 0]) {
    assert.equal(dbToFraction(db), meterFraction(db), `dbToFraction(${db}) must equal the mixer's meterFraction`);
  }

  // And the documented convention, stated in numbers: green to -18, amber to
  // -6, red above, on -60..0. If the mixer ever legitimately moves these, this
  // test moves with it via the imports — this block is here so the CONVENTION
  // is written where the input meters' tests live.
  const zones = meterZones();
  assert.deepEqual(
    zones.map((z) => z.zone),
    ['green', 'amber', 'red'],
  );
  assert.equal(zones[0].from, 0);
  assert.equal(zones[2].to, 1);
  assert.equal(zones[0].to, zones[1].from, 'zones must tile the bar with no gap');
  assert.equal(zones[1].to, zones[2].from);
});

test('zoneFills lights every zone the level has passed through', () => {
  // -3 dBFS: green and amber full, red partial — the way a real staged meter
  // reads at a glance. On the -60..0 scale, -3 is 0.95 of the bar and red
  // spans 0.9..1, so the red zone is half lit.
  const hot = zoneFills(-3);
  assert.equal(hot[0], 1);
  assert.equal(hot[1], 1);
  assert.ok(Math.abs(hot[2] - 0.5) < 1e-9, `red fill for -3 dBFS = ${hot[2]}, want 0.5`);

  // -30 dBFS: half the bar, all of it inside the green zone (0..0.7).
  const mid = zoneFills(-30);
  assert.ok(Math.abs(mid[0] - 0.5 / 0.7) < 1e-9);
  assert.equal(mid[1], 0);
  assert.equal(mid[2], 0);

  // Silence and garbage draw nothing.
  assert.deepEqual(zoneFills(-60), [0, 0, 0]);
  assert.deepEqual(zoneFills(undefined), [0, 0, 0]);
});

test('isSilentFrame recognises the zero-frame, absence, and nothing else', () => {
  assert.equal(isSilentFrame(null), true);
  assert.equal(isSilentFrame(undefined), true);
  assert.equal(isSilentFrame({}), true, 'no peak array is no data, and no data must not draw');
  assert.equal(isSilentFrame({ peak: [] }), true);
  assert.equal(
    isSilentFrame({ peak: [LEVELS_SILENCE_DB, LEVELS_SILENCE_DB] }),
    true,
    'the session-end zero-frame dims the meters',
  );
  assert.equal(isSilentFrame({ peak: [LEVELS_SILENCE_DB, -40] }), false, 'one live channel is a live frame');
  assert.equal(isSilentFrame({ peak: [-12, -14] }), false);
});

test('frameChannels always yields exactly the number of meters that are on screen', () => {
  // The DeckLink routing grid draws one meter per NEGOTIATED capture channel,
  // and the meter count and the frame width are decided by different things at
  // different moments. The failure being closed is not a crash: it is a bar left
  // standing at its last level after the channel behind it stopped existing,
  // which an operator reads as a live commentator.
  const four = frameChannels({ peak: [-12, -18, -24, -30], rms: [-20, -26, -32, -38] }, 4);
  assert.deepEqual(four.peak, [-12, -18, -24, -30]);
  assert.deepEqual(four.rms, [-20, -26, -32, -38]);

  // Short frames pad with SILENCE, not with the last value and not with undefined.
  const padded = frameChannels({ peak: [-12, -18] }, 4);
  assert.deepEqual(padded.peak, [-12, -18, LEVELS_SILENCE_DB, LEVELS_SILENCE_DB]);
  assert.deepEqual(padded.rms, new Array(4).fill(LEVELS_SILENCE_DB), 'a frame with no rms draws no rms');

  // Long frames truncate: a channel with no meter must not shift the others.
  assert.deepEqual(frameChannels({ peak: [-1, -2, -3] }, 2).peak, [-1, -2]);

  // Absence, garbage and out-of-range values all read as silence.
  assert.deepEqual(frameChannels(null, 2).peak, [LEVELS_SILENCE_DB, LEVELS_SILENCE_DB]);
  assert.deepEqual(frameChannels({ peak: ['x', -Infinity] }, 2).peak, [LEVELS_SILENCE_DB, LEVELS_SILENCE_DB]);
  assert.deepEqual(frameChannels({ peak: [-400] }, 1).peak, [LEVELS_SILENCE_DB], 'the clamp floor is the floor');

  // No meters is no work, not a throw.
  assert.deepEqual(frameChannels({ peak: [-12] }, 0), { peak: [], rms: [] });
});

/* ------------------------------------------------------------------------ */
/* Peak-hold                                                                 */
/* ------------------------------------------------------------------------ */

test('the peak-hold holds for PEAK_HOLD_MS and then decays on the injected clock', () => {
  let t = 0;
  const ph = createPeakHold(() => t);

  // Rises instantly to a new maximum.
  assert.deepEqual(ph.update([-20]), [-20]);

  // Holds through quieter frames inside the hold window.
  t = 100;
  assert.deepEqual(ph.update([-40]), [-20]);
  t = PEAK_HOLD_MS;
  assert.deepEqual(ph.update([-40]), [-20], 'the hold boundary itself still holds');

  // Decays past the window at PEAK_DECAY_DB_PER_S.
  t = PEAK_HOLD_MS + 500;
  const decayed = ph.update([-40])[0];
  const expected = -20 - (PEAK_DECAY_DB_PER_S * 500) / 1000;
  assert.ok(Math.abs(decayed - expected) < 1e-9, `decayed marker = ${decayed}, want ${expected}`);
  assert.ok(decayed < -20 && decayed > -40, 'a decay is between the held peak and the live level');

  // Far enough out, the marker lands on the live level and tracks it.
  t = PEAK_HOLD_MS + 60_000;
  assert.deepEqual(ph.update([-40]), [-40]);

  // A louder frame takes over immediately, restarting the hold.
  t += 10;
  assert.deepEqual(ph.update([-10]), [-10]);
  t += PEAK_HOLD_MS - 1;
  assert.deepEqual(ph.update([-50]), [-10]);
});

test('the peak-hold is per channel and reset() forgets everything', () => {
  let t = 0;
  const ph = createPeakHold(() => t);

  assert.deepEqual(ph.update([-20, -6]), [-20, -6]);
  t = 200;
  assert.deepEqual(ph.update([-40, -30]), [-20, -6], 'each channel holds its own peak');

  ph.reset();
  assert.deepEqual(ph.update([-50, -50]), [-50, -50], 'after reset there is no ghost of the old peaks');

  // Garbage never escapes the floor. A fresh instance, because on a running
  // one an earlier real peak would (correctly) still be held.
  const fresh = createPeakHold(() => 0);
  assert.deepEqual(fresh.update(['x', -200]), [LEVELS_SILENCE_DB, LEVELS_SILENCE_DB]);
});

/* ------------------------------------------------------------------------ */
/* Wiring guards                                                             */
/* ------------------------------------------------------------------------ */

test('home.js draws the meters OUTSIDE the tile, beside it in the stage', () => {
  // The native SRT overlay is an opaque child window covering exactly the
  // tile's rectangle (measurePictureRect measures .pgm-tile), so a meter
  // appended inside the tile is invisible for as long as the good picture is
  // up — precisely when it is needed. The meters must be a SIBLING in
  // .pgm-stage.
  const src = ui('home.js');
  assert.match(
    src,
    /pgmStage\.append\(pgmTile, metersEl\)/,
    'the meters container must be appended to pgmStage beside the tile',
  );
  assert.ok(
    !/pgmTile\.(?:append|appendChild)\([^)]*metersEl/.test(src),
    'the meters container must never be appended to pgmTile — the native overlay covers that rectangle',
  );
  // And the overlay's rectangle is still measured from the tile alone, so
  // adding the meters cannot have widened what the native window covers.
  assert.match(
    src,
    /pgmTile\.getBoundingClientRect\(\)/,
    'measurePictureRect must still measure .pgm-tile and nothing else',
  );
});

test('the levels transport exists and the event name matches app.go', () => {
  const backend = ui('backend.js');
  assert.match(backend, /export const EVENT_LEVELS = 'levels';/);
  assert.match(backend, /export function onLevels\(cb\)/, 'backend.js must expose the onLevels subscriber');

  // The string contract with the Go side, asserted against app.go itself so a
  // rename on either side fails a test instead of silently emptying the
  // meters.
  const appGo = read(repoRoot, 'app.go');
  assert.match(appGo, /EventLevels = "levels"/, 'app.go must emit the event under the same name');

  // And app.js actually wires the event to the view.
  const app = ui('app.js');
  assert.match(app, /backend\.onLevels\(/, 'app.js must subscribe to the levels event');
  assert.match(app, /home\.setLevels\(/, 'app.js must forward frames to home.setLevels');
});

test('the fake backend moves the meters in a dev session and silences them on stop', () => {
  // npm run dev has no Go side, so the fake must emit the same shaped frames
  // — moving while the fake session runs, one all-silence frame on stop — or
  // the meters are only developable against a real build.
  const backend = ui('backend.js');
  assert.match(backend, /startFakeLevels\(\)/);
  assert.match(backend, /stopFakeLevels\(\)/);
  assert.match(
    backend,
    /fakeEmit\(EVENT_LEVELS, \{\s*peak,/,
    'the fake must emit {peak, rms} frames while running',
  );
  assert.match(
    backend,
    /peak: \[FAKE_LEVELS_SILENCE_DB, FAKE_LEVELS_SILENCE_DB\]/,
    'the fake must emit the zero-frame on stop, as app.go does',
  );
});
