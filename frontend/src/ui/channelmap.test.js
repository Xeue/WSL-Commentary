/**
 * Tests for the DeckLink channel map: the routing arithmetic, the wording that
 * states what the arithmetic costs, the two lamps, and the wiring that puts all
 * of it on the Settings screen.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURES THESE PREVENT =========================
 *
 * Every one of them is measured on the real card, and every one is SILENT —
 * which is why they are worth a test file rather than a careful read:
 *
 *   - A MAP NAMING A CHANNEL THE PAD DID NOT NEGOTIATE stops the feed. Writing a
 *     2x8 matrix to a pipeline running 2x16 gave "Internal data stream error ...
 *     reason error (-5)" with every coefficient in it perfectly legal. So the
 *     grid may never be sized from a constant, from the card's advertised
 *     max-channels, or from the width of whatever was saved last time.
 *   - A GAIN OUTSIDE [-1, 1] IS REFUSED SILENTLY, leaving the previous matrix in
 *     force: 1.0000001 moved the meter by 0.0000 dB. So nothing here may offer
 *     gain above unity, and dB arithmetic that lands on 1.0000000000000002 has
 *     to be rounded before it goes anywhere.
 *   - AN EMPTY MAP IS NOT SILENCE, it is "nobody has chosen", and it resolves to
 *     channel 1 left / channel 2 right. A screen that saved the default it draws
 *     would turn "not chosen" into "chosen" on every machine that opened it.
 *   - SIGNAL UNKNOWN IS NOT A FAULT. It is the state of every machine with no
 *     card in it, which is every machine running this application today.
 *
 * ======================= WHY SOME OF THIS READS SOURCE ======================
 *
 * The behaviour half runs the real module: everything above createChannelMapView
 * is pure, and its one import chain (lamps.js, meters.js -> mixer/model.js) is
 * pure too. The wiring half reads source text, for the reason meters.test.js and
 * settings.test.js give at length — package.json has no jsdom, the failures being
 * guarded are textual, and a DOM shim widened until a test passes stops being
 * evidence.
 *
 * Several tests read internal/gst's own source, in the idiom settings.test.js
 * uses against internal/config: the model lives there, this is the screen, and a
 * number mirrored across the boundary must fail a test rather than drift.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  MAX_INPUT_CHANNELS,
  OUTPUTS,
  OUTPUT_LEFT,
  OUTPUT_RIGHT,
  GAIN_LIMIT,
  TRIM_FLOOR_DB,
  SIGNAL_STATE,
  SIGNAL_FLAP_ALERT,
  ROUTER_CAVEAT,
  LAMP_VIDEO,
  LAMP_AUDIO,
  clampGain,
  gainToDb,
  dbToGain,
  isInverted,
  describeGain,
  crosspointLabel,
  inputChannelCount,
  isNeverMapped,
  defaultChannelMap,
  emptyGrid,
  gridFromMap,
  mapFromGrid,
  describeDropped,
  outputSummary,
  describeOutputSummary,
  deriveSignalLamp,
  deriveAudioLamp,
} from './channelmap.js';

import { LEVEL } from './lamps.js';
import { LEVELS_SILENCE_DB, LEVELS_FLOOR_DB } from './meters.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);
const css = () => read(here, '..', 'styles', 'main.css');
const gst = (name) => read(repoRoot, 'internal', 'gst', name);

/* ------------------------------------------------------------------------ */
/* Gains: the clamp is GStreamer's, not a policy                             */
/* ------------------------------------------------------------------------ */

test('a gain can never leave this module outside [-1, 1]', () => {
  // 1.0 is accepted and 1.0000001 is REFUSED — and the refusal is silent, with
  // the previous matrix left in force and the meter not moving by 0.0000 dB. So
  // the failure of an unclamped UI is a control that sometimes does nothing and
  // never says so.
  assert.equal(clampGain(1), GAIN_LIMIT);
  assert.equal(clampGain(1.0000001), GAIN_LIMIT);
  assert.equal(clampGain(4), GAIN_LIMIT);
  assert.equal(clampGain(-1.5), -GAIN_LIMIT);
  assert.equal(clampGain(0.5), 0.5);
  assert.equal(clampGain(0), 0);
});

test('a gain is rounded before it is clamped, so dB arithmetic cannot overshoot', () => {
  // 10^(0/20) is 1.0000000000000002 on some paths, and that is exactly the kind
  // of last-place error the silent rejection is triggered by.
  assert.equal(dbToGain(0), 1, '0 dB must be exactly unity, not 1.0000000000000002');
  assert.ok(String(clampGain(0.1 + 0.2)).length <= 8, 'a gain must not carry seventeen digits');
});

test('bad input becomes silence, never something loud', () => {
  for (const bad of [undefined, null, 'x', NaN, Infinity, -Infinity, {}]) {
    assert.equal(clampGain(bad), 0, `${String(bad)} must clamp to silence`);
  }
});

test('the dB scale runs from the floor to 0 and has no travel above it', () => {
  assert.equal(gainToDb(1), 0);
  assert.equal(gainToDb(0.5), -6);
  assert.equal(gainToDb(0.25), -12);
  assert.equal(gainToDb(0), null, 'not routed is not a level');
  assert.equal(gainToDb(0.000001), TRIM_FLOOR_DB, 'below the floor reads as the floor');

  assert.equal(dbToGain(0), 1);
  assert.ok(Math.abs(dbToGain(-6) - 0.501187) < 1e-5);
  assert.equal(dbToGain(TRIM_FLOOR_DB), 0.01);
  // ASKING FOR GAIN IS NOT AN ERROR TO REPORT, IT IS A VALUE THAT CANNOT EXIST.
  assert.equal(dbToGain(3), 1, '+3 dB does not exist on this path; it pulls to unity');
  assert.equal(dbToGain('nonsense'), 0);
});

test('polarity survives the round trip, because a stored map can carry it', () => {
  // A negative gain is a real thing to want on a desk that has sent a leg out of
  // phase, and gst allows the whole range. A screen that could only set positive
  // gains would silently flip an inverted contribution back the first time
  // anybody dragged its trim.
  assert.equal(dbToGain(-6, true) < 0, true);
  assert.equal(isInverted(-0.5), true);
  assert.equal(isInverted(0.5), false);
  assert.equal(isInverted(0), false, 'silence has no polarity');
  assert.equal(gainToDb(-0.5), -6, 'the dB reading is the MAGNITUDE; ø carries the sign');
});

test('a crosspoint says its level in words, not in colour', () => {
  assert.equal(describeGain(0), '—');
  assert.equal(describeGain(1), '0.0 dB');
  assert.equal(describeGain(0.5), '-6.0 dB');
  assert.equal(describeGain(-0.5), 'ø -6.0 dB');
});

test('the +1 between gst channel 0 and the operator channel 1 happens exactly once', () => {
  // gst.ChannelContribution counts inputs from zero because the matrix does; the
  // SDI embedder in front of the operator counts from one. A conversion in two
  // places is a conversion that eventually gets done twice.
  assert.equal(crosspointLabel(0, OUTPUT_LEFT), 'Channel 1 → Left');
  assert.equal(crosspointLabel(15, OUTPUT_RIGHT), 'Channel 16 → Right');
});

/* ------------------------------------------------------------------------ */
/* The map: width and shape are the two pipeline kills                       */
/* ------------------------------------------------------------------------ */

test('the model constants are internal/gst\'s, not a second opinion', () => {
  // The anti-drift assertions. The model lives in Go; these three numbers are
  // mirrored here because the screen has to draw before it has asked anything,
  // and a mirrored number that drifts is exactly how a UI starts refusing maps
  // Go accepts, or offering maps it does not.
  const model = gst('channelmap.go');
  assert.match(model, /const MaxInputChannels = 16/, 'internal/gst no longer bounds inputs at 16');
  assert.equal(MAX_INPUT_CHANNELS, 16);
  assert.match(model, /const ChannelGainLimit = 1\.0/, 'internal/gst no longer clamps gain at unity');
  assert.equal(GAIN_LIMIT, 1);
  assert.match(model, /OutputLeft {2}= 0/);
  assert.match(model, /OutputRight = 1/);
  assert.equal(OUTPUT_LEFT, 0);
  assert.equal(OUTPUT_RIGHT, 1);
  assert.match(model, /const ChannelMapOutputs = 2/, 'a third output row would need this whole screen re-laid out');
  assert.equal(OUTPUTS.length, 2);
});

test('the default map is gst.DefaultChannelMap, which is the previous behaviour', () => {
  // Channel 1 left, channel 2 right, at unity: the identity on the two channels
  // decklinkaudiosrc channels=2 used to pass through, so a seat that never opens
  // this screen hears exactly what it heard before the screen existed.
  assert.deepEqual(defaultChannelMap(16), [
    { output: OUTPUT_LEFT, input: 0, gain: 1 },
    { output: OUTPUT_RIGHT, input: 1, gain: 1 },
  ]);
  // A mono input in one ear is a commentator half the audience cannot hear.
  assert.deepEqual(defaultChannelMap(1), [
    { output: OUTPUT_LEFT, input: 0, gain: 1 },
    { output: OUTPUT_RIGHT, input: 0, gain: 1 },
  ]);
  assert.deepEqual(defaultChannelMap(0), [], 'no channels is no map, not a throw');

  // And it is the same rule the Go side applies, asserted against its source.
  const model = gst('channelmap.go');
  const fn = model.slice(model.indexOf('func DefaultChannelMap'), model.indexOf('func (m ChannelMap) IsDefault'));
  assert.match(fn, /case inputChannels == 1:/, 'the one-channel case must still exist in Go');
  assert.match(fn, /\{Output: OutputLeft, Input: 0, Gain: 1\}/);
  assert.match(fn, /\{Output: OutputRight, Input: 1, Gain: 1\}/);
});

test('an empty map means NOBODY HAS CHOSEN, and it draws the default', () => {
  assert.equal(isNeverMapped([]), true);
  assert.equal(isNeverMapped(null), true);
  assert.equal(isNeverMapped(undefined), true);
  assert.equal(isNeverMapped('x'), true, 'a value that is not a list is not a choice either');
  assert.equal(isNeverMapped([{ output: 0, input: 0, gain: 1 }]), false);

  // The zero value RESOLVES rather than silencing: a position whose map is
  // missing from config.json, or predates the feature, must go on air with audio
  // on it. An empty map meaning silence is a feed that is healthy by every lamp
  // in the application and carries nothing, discovered by the director.
  const { grid } = gridFromMap([], 16);
  assert.equal(grid[OUTPUT_LEFT][0], 1);
  assert.equal(grid[OUTPUT_RIGHT][1], 1);
  assert.equal(grid[OUTPUT_LEFT].slice(1).every((g) => g === 0), true);
});

test('the grid is always OUTPUT ROWS x the negotiated width', () => {
  for (const channels of [1, 2, 8, 16]) {
    for (const grid of [emptyGrid(channels), gridFromMap([], channels).grid, gridFromMap(null, channels).grid]) {
      assert.equal(grid.length, OUTPUTS.length, `${channels}ch: two rows, one per output`);
      for (const row of grid) {
        assert.equal(row.length, channels, `${channels}ch: every row is as wide as the pad`);
      }
    }
  }
});

test('the negotiated count is the only thing that sizes the grid', () => {
  assert.equal(inputChannelCount({ inputChannels: 16 }), 16);
  assert.equal(inputChannelCount({ inputChannels: 8 }), 8);
  assert.equal(inputChannelCount({ inputChannels: 2.9 }), 2, 'a fractional count floors rather than rounds up');
  // ZERO IS A NORMAL ANSWER — it is what InputChannels() returns before Start —
  // and it means "draw no grid": with no authoritative width there is nothing to
  // guess, and a guessed width is the pipeline kill.
  for (const nothing of [undefined, null, {}, { inputChannels: 0 }, { inputChannels: -4 }, { inputChannels: 'x' }]) {
    assert.equal(inputChannelCount(nothing), 0, `${JSON.stringify(nothing)} must size no grid at all`);
  }
  assert.equal(inputChannelCount({ inputChannels: 64 }), MAX_INPUT_CHANNELS, 'a garbled report clamps to the ceiling');
});

test('a saved map naming channels the pad does not have is NARROWED, and the loss is reported', () => {
  // The map of a day when the pad negotiated sixteen, loaded on a day it
  // negotiates eight. Channel 11's commentator is not in the feed any more, and
  // the only acceptable outcome is that somebody is told.
  const saved = [
    { output: OUTPUT_LEFT, input: 0, gain: 1 },
    { output: OUTPUT_LEFT, input: 10, gain: 0.5 },
    { output: OUTPUT_RIGHT, input: 12, gain: -1 },
  ];
  const { grid, dropped } = gridFromMap(saved, 8);
  assert.equal(grid[OUTPUT_LEFT].length, 8, 'the grid is the pad\'s width, always');
  assert.equal(grid[OUTPUT_LEFT][0], 1, 'the channels that survive keep their gains');
  assert.deepEqual(dropped, [11, 13], 'dropped channels are reported by their 1-based number');

  // And what leaves the screen afterwards names only channels that exist, which
  // is the map Go will accept rather than the one it refuses by name.
  assert.deepEqual(mapFromGrid(grid), [{ output: OUTPUT_LEFT, input: 0, gain: 1 }]);

  const note = describeDropped(dropped, 8);
  assert.match(note, /11 and 13/);
  assert.match(note, /Re-route/, 'the note must say what to do, not merely what happened');
});

test('narrowing past a SILENT contribution reports nothing: nothing was lost', () => {
  const saved = [
    { output: OUTPUT_LEFT, input: 0, gain: 1 },
    { output: OUTPUT_RIGHT, input: 9, gain: 0 },
  ];
  assert.deepEqual(gridFromMap(saved, 2).dropped, []);
  assert.equal(describeDropped([], 2), '', 'no empty flourish on the normal case');
});

test('a value that is not a map at all draws the DEFAULT, never silence', () => {
  // A stored value this screen cannot read as a list is not a licence to take a
  // working seat off air, and the default is the routing that was already there.
  // These are the shapes a corrupted or hand-edited config.json produces.
  for (const junk of ['x', 42, {}, true]) {
    const map = mapFromGrid(gridFromMap(junk, 4).grid);
    assert.deepEqual(map, defaultChannelMap(4), `${JSON.stringify(junk)} must fall back to the default map`);
  }
});

test('a list whose entries are unusable routes nothing, and the screen says so', () => {
  // This is the OTHER half of the case above and it goes the other way on
  // purpose. A non-empty list IS a choice — somebody wrote it — so it is not
  // treated as "nobody has chosen", and an entry naming an output or an input
  // that does not exist is exactly the map internal/gst refuses BY NAME rather
  // than one this screen may quietly reinterpret.
  //
  // What must not happen is that the result is silent AND unremarked. It is not:
  // the sum line under each output says the leg carries nothing, in words.
  const junk = [null, { output: 9, input: 0, gain: 1 }, { output: 0, input: -1, gain: 1 }];
  const { grid } = gridFromMap(junk, 4);
  assert.deepEqual(mapFromGrid(grid), []);
  for (const out of OUTPUTS) {
    assert.match(describeOutputSummary(outputSummary(grid, out.index)), /nothing routed .* silent/);
  }
});

test('two contributions to one cell cannot leave this screen', () => {
  // gst.MixMatrix refuses a duplicated cell BY NAME — one cell holds one
  // coefficient, and two would have to be summed into a gain nobody chose. A
  // hand-edited file can carry one; the grid resolves it and re-emits a map Go
  // accepts.
  const duplicated = [
    { output: OUTPUT_LEFT, input: 3, gain: 0.25 },
    { output: OUTPUT_LEFT, input: 3, gain: 1 },
  ];
  const { grid } = gridFromMap(duplicated, 8);
  const map = mapFromGrid(grid);
  assert.deepEqual(map, [{ output: OUTPUT_LEFT, input: 3, gain: 1 }], 'the last one wins and only one survives');

  const cells = new Set(map.map((c) => `${c.output}:${c.input}`));
  assert.equal(cells.size, map.length, 'no cell may appear twice in a map this screen produced');
});

test('a silent cell is not a contribution', () => {
  // Go would accept a zero coefficient, and accepting it here would make every
  // saved map thirty-two entries long, most of them meaningless, and a diff of
  // two maps unreadable.
  const grid = emptyGrid(4);
  grid[OUTPUT_LEFT][2] = 0.5;
  grid[OUTPUT_RIGHT][3] = 0;
  assert.deepEqual(mapFromGrid(grid), [{ output: OUTPUT_LEFT, input: 2, gain: 0.5 }]);
  assert.deepEqual(mapFromGrid(emptyGrid(16)), [], 'a grid with nothing routed is an empty map');
});

test('an out-of-range gain in a stored map is clamped, not carried', () => {
  const hot = [
    { output: OUTPUT_LEFT, input: 0, gain: 9 },
    { output: OUTPUT_RIGHT, input: 1, gain: -9 },
  ];
  const { grid } = gridFromMap(hot, 2);
  assert.deepEqual(mapFromGrid(grid), [
    { output: OUTPUT_LEFT, input: 0, gain: GAIN_LIMIT },
    { output: OUTPUT_RIGHT, input: 1, gain: -GAIN_LIMIT },
  ]);
});

/* ------------------------------------------------------------------------ */
/* Summing: the thing the operator must be told before they discover it      */
/* ------------------------------------------------------------------------ */

test('the caveat states the measured facts, in the words the operator needs', () => {
  assert.match(ROUTER_CAVEAT, /ROUTER/, 'it must say what this is');
  assert.match(ROUTER_CAVEAT, /not a mixer/i);
  assert.match(ROUTER_CAVEAT, /0 dB is the maximum/i, 'the clamp, stated as a limit rather than as an error');
  assert.match(ROUTER_CAVEAT, /ADD TOGETHER and clip/i, 'what happens when two channels share an output');
  assert.match(ROUTER_CAVEAT, /no gain stage after this one/i);
  assert.match(ROUTER_CAVEAT, /immediately/i, 'a live control has to say that it is live');
});

test('an output sums the MAGNITUDES of everything routed to it', () => {
  // The screen's twin of gst.ChannelMap.OutputGain, computed from the grid so it
  // moves as the operator drags.
  const grid = emptyGrid(16);
  grid[OUTPUT_LEFT][0] = 1;
  grid[OUTPUT_LEFT][2] = 0.6;
  grid[OUTPUT_LEFT][5] = -0.5; // inverted, and it still counts towards the worst case

  const left = outputSummary(grid, OUTPUT_LEFT);
  assert.deepEqual(left.sources, [1, 3, 6]);
  assert.equal(left.sum, 2.1);
  assert.equal(left.clips, true);

  const right = outputSummary(grid, OUTPUT_RIGHT);
  assert.deepEqual(right.sources, []);
  assert.equal(right.sum, 0);
  assert.equal(right.clips, false);
});

test('exactly unity does not clip, and a rounding crumb above it does not either', () => {
  const exact = emptyGrid(2);
  exact[OUTPUT_LEFT][0] = 1;
  assert.equal(outputSummary(exact, OUTPUT_LEFT).clips, false, 'a full-scale single source is the normal case');

  const pair = emptyGrid(2);
  pair[OUTPUT_LEFT][0] = 0.5;
  pair[OUTPUT_LEFT][1] = 0.5;
  assert.equal(outputSummary(pair, OUTPUT_LEFT).sum, 1);
  assert.equal(outputSummary(pair, OUTPUT_LEFT).clips, false, 'a warning nobody can act on is a warning that gets ignored');

  const over = emptyGrid(2);
  over[OUTPUT_LEFT][0] = 0.6;
  over[OUTPUT_LEFT][1] = 0.5;
  assert.equal(outputSummary(over, OUTPUT_LEFT).clips, true);
});

test('the clip verdict agrees with the number the operator can see', () => {
  // "worst case 1.00 of full scale — WILL CLIP" is a sentence nobody can act on:
  // the number in it says it does not. So the verdict is taken from the sum at
  // the resolution it is PRINTED at, and the two cannot disagree at any
  // combination of gains. Unity plus a -6 dB trim (really 0.501187) is the case
  // that produced the contradiction.
  for (const pair of [
    [1, dbToGain(-6)],
    [0.5, dbToGain(-6)],
    [dbToGain(-0.5), dbToGain(-40)],
    [1, 1],
    [0.999999, 0.000001],
  ]) {
    const grid = emptyGrid(2);
    grid[OUTPUT_LEFT][0] = pair[0];
    grid[OUTPUT_LEFT][1] = pair[1];
    const summary = outputSummary(grid, OUTPUT_LEFT);
    assert.equal(
      summary.clips,
      Number(summary.sum.toFixed(2)) > 1,
      `${pair.join(' + ')} prints ${summary.sum.toFixed(2)} but claims clips=${summary.clips}`,
    );
  }
});

test('the per-output sentence says what will happen and what to do about it', () => {
  const silent = describeOutputSummary(outputSummary(emptyGrid(4), OUTPUT_LEFT));
  assert.match(silent, /Left: nothing routed/);
  assert.match(silent, /silent/, 'a silent leg of the feed is worth saying plainly');

  const fine = emptyGrid(4);
  fine[OUTPUT_RIGHT][0] = 0.5;
  assert.match(
    describeOutputSummary(outputSummary(fine, OUTPUT_RIGHT)),
    /^Right: channel 1, worst case 0\.50 of full scale\.$/,
  );

  const hot = emptyGrid(4);
  hot[OUTPUT_LEFT][0] = 1;
  hot[OUTPUT_LEFT][1] = 1;
  const clipping = describeOutputSummary(outputSummary(hot, OUTPUT_LEFT));
  assert.match(clipping, /channels 1 and 2/, 'it names the channels, so the operator knows which to turn down');
  assert.match(clipping, /WILL CLIP/);
  assert.match(clipping, /Turn one of these contributions down/);
  // Summing is ALLOWED — two commentators into a mono leg is the commonest
  // correct map a commentary position has — so this is never phrased as a
  // refusal. The map is legal, Go will build it, and it is the AUDIO that clips.
  assert.equal(
    /invalid|error|refus/i.test(clipping),
    false,
    'the map is legal and WILL be applied; only the audio clips',
  );
});

/* ------------------------------------------------------------------------ */
/* The lamps: no camera and no audio are different faults                    */
/* ------------------------------------------------------------------------ */

test('the three signal states are gst.SignalState\'s, spelled the same way', () => {
  const watch = gst('signalwatch.go');
  assert.match(watch, /SignalUnknown\s+SignalState = "UNKNOWN"/);
  assert.match(watch, /SignalOK\s+SignalState = "OK"/);
  assert.match(watch, /SignalLost\s+SignalState = "LOST"/);
  assert.deepEqual(SIGNAL_STATE, { UNKNOWN: 'UNKNOWN', OK: 'OK', LOST: 'LOST' });
  // And the flap threshold, mirrored for the reason SIGNAL_FLAP_ALERT documents:
  // a lamp that read "unstable" on any non-zero count would sit amber for the
  // rest of the match after one perfectly ordinary lock.
  assert.match(watch, /signalFlapAlert = 4/, 'internal/gst no longer forces a report at 4 flaps');
  assert.equal(SIGNAL_FLAP_ALERT, 4);
});

test('UNKNOWN is drawn as "cannot tell", never as a fault and never as good', () => {
  // It is the state of every machine with no capture card in it — which is every
  // machine running this application today — and of every session before the
  // first measurement.
  for (const payload of [
    undefined,
    null,
    {},
    { state: SIGNAL_STATE.UNKNOWN, flaps: 0 },
    { state: '' },
    { state: 'SOMETHING_A_LATER_BUILD_SENDS' },
  ]) {
    const lamp = deriveSignalLamp(payload);
    assert.equal(lamp.level, LEVEL.GREY, `${JSON.stringify(payload)} must be grey`);
    assert.notEqual(lamp.level, LEVEL.RED);
    assert.match(lamp.text, /NOT MEASURED/);
  }
});

test('the video lamp reads the debounced state, and marks a flapping input', () => {
  assert.deepEqual(deriveSignalLamp({ state: SIGNAL_STATE.OK, flaps: 0 }), {
    level: LEVEL.GREEN,
    text: 'SIGNAL',
  });
  assert.deepEqual(deriveSignalLamp({ state: SIGNAL_STATE.LOST, flaps: 0 }), {
    level: LEVEL.RED,
    text: 'NO SIGNAL',
  });

  // An ordinary lock reports OK with a flap or two — the transition itself is a
  // flap — and must stay green, or the lamp sits amber for the rest of the match
  // after one clean acquisition.
  assert.equal(deriveSignalLamp({ state: SIGNAL_STATE.OK, flaps: 1 }).level, LEVEL.GREEN);
  assert.equal(deriveSignalLamp({ state: SIGNAL_STATE.OK, flaps: SIGNAL_FLAP_ALERT - 1 }).level, LEVEL.GREEN);

  // At the alert threshold the watchdog was FORCED to report: the input dropped
  // lock four times since the last one, which is a marginal cable to fix before
  // the match rather than during it.
  const unstable = deriveSignalLamp({ state: SIGNAL_STATE.OK, flaps: SIGNAL_FLAP_ALERT });
  assert.equal(unstable.level, LEVEL.AMBER);
  assert.match(unstable.text, /UNSTABLE \(4\)/, 'the count is in the text, so the claim is checkable');
});

test('no audio and no camera are different lamps, different colours and different words', () => {
  const noAudio = deriveAudioLamp({ peak: [LEVELS_SILENCE_DB, LEVELS_SILENCE_DB] }, 16);
  const noVideo = deriveSignalLamp({ state: SIGNAL_STATE.LOST, flaps: 0 });

  assert.equal(noAudio.level, LEVEL.AMBER);
  assert.equal(noVideo.level, LEVEL.RED);
  assert.notEqual(noAudio.text, noVideo.text);
  assert.notEqual(LAMP_AUDIO, LAMP_VIDEO);
  // The two faults are fixed in different rooms by different people. A single
  // "card" lamp would send whoever reads it to the wrong one half the time.
  assert.match(noAudio.text, /AUDIO/);
  assert.match(noVideo.text, /SIGNAL/);
});

test('the audio lamp counts channels that are actually carrying something', () => {
  assert.deepEqual(deriveAudioLamp(null, 16), { level: LEVEL.GREY, text: 'NO LEVELS' });
  assert.deepEqual(deriveAudioLamp({}, 16), { level: LEVEL.GREY, text: 'NO LEVELS' });
  assert.deepEqual(deriveAudioLamp({ peak: [] }, 16), { level: LEVEL.GREY, text: 'NO LEVELS' });

  const silent = new Array(16).fill(LEVELS_SILENCE_DB);
  assert.equal(deriveAudioLamp({ peak: silent }, 16).level, LEVEL.AMBER);

  // A noise floor is not a commentator: a card whose sixteen channels are
  // connected to nothing must not read green.
  const noise = new Array(16).fill(LEVELS_FLOOR_DB - 10);
  assert.deepEqual(deriveAudioLamp({ peak: noise }, 16), {
    level: LEVEL.AMBER,
    text: 'NO AUDIO ON ANY CHANNEL',
  });

  const live = new Array(16).fill(LEVELS_SILENCE_DB);
  live[2] = -18;
  live[7] = -24;
  assert.deepEqual(deriveAudioLamp({ peak: live }, 16), { level: LEVEL.GREEN, text: 'AUDIO ON 2 OF 16' });
});

/* ------------------------------------------------------------------------ */
/* Wiring: the module, the screen, the transport and the paint               */
/* ------------------------------------------------------------------------ */

test('the grid is sized from the report and from nothing else', () => {
  const src = ui('channelmap.js');
  assert.match(src, /function inputChannelCount\(state\)/);
  assert.match(src, /padChannels = channels/, 'the view\'s width must come from inputChannelCount\'s answer');
  assert.equal(
    /new Array\(16\)|length = 16|channels = 16/.test(src),
    false,
    'a sixteen written as a size is a matrix width that did not come from the pad',
  );
});

test('the meter idiom is imported, not reimplemented horizontally', () => {
  const src = ui('channelmap.js');
  assert.match(
    src,
    /import \{[\s\S]*?meterZones,[\s\S]*?zoneFills,[\s\S]*?createPeakHold,[\s\S]*?\} from '\.\/meters\.js'/,
    'the scale, the zones and the peak-hold must come from meters.js — three meters in one app ' +
      'that disagree about where amber starts is the two-tables bug again',
  );
  assert.equal(
    /'green'|"green"|'amber'|"amber"/.test(src),
    false,
    'a zone table written here is a second calibration; the zones come from meterZones()',
  );
  assert.match(
    src,
    /input-meter-fill--\$\{zone\}/,
    'the fill colours are the input meters\' own classes, so one rule decides what amber looks like',
  );
});

test('the caveat is drawn as permanent prose, never as a tooltip', () => {
  const src = ui('channelmap.js');
  assert.match(src, /caveat\.textContent = ROUTER_CAVEAT/, 'it must be text in the flow of the screen');
  assert.equal(
    /title = ROUTER_CAVEAT/.test(src),
    false,
    'a tooltip is found only by somebody who already suspected there was something to find',
  );
  assert.match(
    src,
    /el\.append\(caveat,/,
    'the caveat must be the FIRST child of the group: it is read before the first route is made',
  );
});

test('the routing screen is only reachable from a DeckLink input', () => {
  const js = ui('settings.js');
  assert.match(js, /openGroup\(channelMapHeading, 'settings-group--channelmap'\)/);
  assert.match(
    js,
    /channelMapGroup\.hidden = !decklink/,
    'the whole group hides for a native input: that seat must see this screen exactly as it was',
  );
  assert.match(
    js,
    /fields\.audioSourceKind\.input\.addEventListener\('change', renderChannelMapGroup\)/,
    'the group must follow the capture-kind control live',
  );
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(
    populate,
    /renderChannelMapGroup\(\)/,
    'assigning a <select> from script fires no event, so populate must call it itself',
  );
});

test('the map is populated and collected, so a Save cannot silently delete it', () => {
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(populate, /channelMapView\.setMap\(config\.decklinkChannelMap\)/);
  assert.match(collect, /decklinkChannelMap: channelMapView\.collect\(\)/);
  // AND IT IS COLLECTED WHILE THE GROUP IS HIDDEN. collectConfig replaces the
  // whole document, so a native-input seat pressing Save on an unrelated field
  // would otherwise delete a commentator's channel assignment.
  assert.equal(
    /if \(!?decklink\)[\s\S]{0,80}decklinkChannelMap/.test(collect),
    false,
    'the map must be collected unconditionally, never only when the group is on screen',
  );
});

test('a never-mapped seat saves an empty map, not the default it is drawing', () => {
  // Writing the default out would turn "nobody has chosen" into "somebody chose
  // this" on every machine that ever opened Settings — after which the line that
  // says nobody has chosen would be wrong everywhere, and the Go side's zero
  // value would never be reachable again.
  const src = ui('channelmap.js');
  const collect = src.slice(src.indexOf('function collect()'), src.indexOf('/** setSignal paints'));
  assert.match(collect, /neverMapped \? \[\] : mapFromGrid\(cells\)/);
});

test('the change is applied live, with no apply button anywhere', () => {
  const js = ui('settings.js');
  assert.match(
    js,
    /onChange: \(map\) => \{[\s\S]*?backend\.setChannelMap\(map\)/,
    'every change must reach the pipeline as it is made: the live write was measured at 119 us ' +
      'with the pipeline staying PLAYING',
  );
  assert.match(js, /Could not apply the channel routing/, 'a refused map must be reported, not swallowed');
  assert.equal(
    /Apply routing|applyChannelMapBtn|channelMapApplyBtn/.test(js),
    false,
    'an Apply button here would describe a restriction that does not exist',
  );
});

test('the transport names the three events and the two bindings in one place each', () => {
  const backend = ui('backend.js');
  assert.match(backend, /export const EVENT_CHANNEL_MAP = 'channelMap';/);
  assert.match(backend, /export const EVENT_CHANNEL_LEVELS = 'channelLevels';/);
  assert.match(backend, /export const EVENT_SIGNAL = 'signal';/);
  assert.match(backend, /state: 'GetChannelMap'/);
  assert.match(backend, /set: 'SetChannelMap'/);
  assert.match(backend, /export function channelMapAvailable\(\)/);
  for (const fn of ['getChannelMap', 'setChannelMap', 'onChannelMap', 'onChannelLevels', 'onSignal']) {
    assert.ok(
      backend.includes(`export ${fn.startsWith('on') ? 'function' : 'async function'} ${fn}(`),
      `backend.js must export ${fn}`,
    );
  }

  // The "signal" event's name is app.go's, asserted against it: a rename on
  // either side would leave the lamp grey for ever with nothing to say why.
  assert.match(read(repoRoot, 'app.go'), /EventSignal = "signal"/, 'app.go must emit the signal event under this name');
});

test('the two level events are kept apart, because they measure different points', () => {
  // MEASURED: every level element in the process posts a structure named
  // "level", 39 messages a second from the two of them, and a handler that
  // matched on the name alone would feed one meter a two-entry frame and a
  // sixteen-entry frame alternately. internal/gst routes on msg.Source(); this
  // side must never treat one event as a fallback for the other.
  const backend = ui('backend.js');
  assert.notEqual('channelLevels', 'levels');
  const levels = gst('levels.go');
  assert.match(levels, /levelElementName {8}= "alevel"/);
  assert.match(levels, /channelLevelElementName = "chlevel"/);

  const settings = ui('settings.js');
  assert.match(settings, /backend\.onChannelLevels\(\(frame\) => channelMapView\.setLevels\(frame\)\)/);
  assert.equal(
    /backend\.onLevels\(/.test(settings),
    false,
    'the routing grid must never be fed the programme meter: it is downstream of the matrix the ' +
      'grid controls and cannot identify anybody',
  );
  assert.ok(
    backend.includes('must never treat one as a fallback for the other'),
    'backend.js must record why the two level events are separate',
  );
});

test('the fake backend brings a card up, so the dev loop can exercise all of it', () => {
  const backend = ui('backend.js');
  assert.match(backend, /function fakeCardUp\(\)/);
  assert.match(backend, /fakeCardUp\(\);/, 'the fake session must negotiate a pad and measure a signal');
  assert.match(backend, /fakeCardDown\(\);/, 'and take both away on stop: a lamp still green after STOP teaches a bad habit');
  assert.match(
    backend,
    /const FAKE_LIVE_CHANNELS = \[\d+, \d+\];/,
    'only some fake channels may carry audio — a fake where all sixteen moved together would let ' +
      'the find-the-commentator interaction break unnoticed',
  );
  assert.match(backend, /inputChannels: 0,/, 'the fake must start with no negotiated pad, as InputChannels() does');
  assert.match(backend, /setSignal: \(state, flaps\) =>/, 'a real loss is reported once in a match; this is how it is reached');
  assert.match(backend, /setChannelCount: \(channels\) =>/);
});

test('every class the routing screen adds is styled, and no threshold lives in the CSS', () => {
  const sources = ui('channelmap.js') + ui('settings.js');
  const sheet = css();
  const classes = new Set([...sources.matchAll(/\b(channelmap-[a-z0-9-]+)/g)].map((m) => m[1]));
  assert.ok(classes.size > 15, `found only ${classes.size} channelmap classes; the regex is broken`);
  for (const name of classes) {
    assert.ok(sheet.includes(`.${name}`), `main.css must style .${name}, or the grid draws nothing`);
  }
  assert.ok(sheet.includes('.settings-group--channelmap'), 'the group needs its full-row rule');
  // The zone widths are set inline from meters.js. A percentage in the sheet
  // would be a second copy of the dB boundaries.
  const block = sheet.slice(sheet.indexOf('DECKLINK CHANNEL ROUTING'));
  assert.equal(
    /flex-basis:\s*\d/.test(block),
    false,
    'a zone width in the stylesheet is a threshold the dB scale cannot correct',
  );
  assert.equal(
    /grid-column:\s*span/.test(block),
    false,
    'a span wider than the track count adds implicit columns rather than clamping',
  );
});
