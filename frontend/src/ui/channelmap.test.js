/**
 * Tests for the channel map: the routing arithmetic, the wording that states
 * what the arithmetic costs, the two lamps, and the wiring that puts all of it
 * on the Settings screen.
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
  deviceKeyOf,
  captureKindOf,
  describePad,
  describeRoutingHeading,
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
  // READ OUT OF GO RATHER THAN PINNED AT 16, and the difference matters right
  // now: §4.3 of the always-live-capture plan raises gst.MaxInputChannels to 32
  // with the measurement beside it (a 2x32 mix-matrix passes audio and `level`
  // reports 32 rms entries per message, verified on this machine), and the two
  // sides must move in the same commit. Pinned at the literal, this test would
  // have failed on the Go change with a message about the number rather than
  // about the pair. Read across, it fails on the Go change saying exactly which
  // other line has to move — and it goes on failing until it does, which is the
  // whole job: a ceiling raised in Go alone silently CLAMPS a 32-channel pad to
  // a 16-row grid, and a grid drawn wider than Go's bound has every press
  // refused.
  const bound = /const MaxInputChannels = (\d+)/.exec(model);
  assert.ok(bound, 'internal/gst no longer declares MaxInputChannels');
  assert.equal(
    MAX_INPUT_CHANNELS,
    Number(bound[1]),
    'channelmap.js\'s MAX_INPUT_CHANNELS must equal gst.MaxInputChannels exactly',
  );
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
  // A TRANSPOSE IS INVISIBLE AT 2x2, which is why the non-square widths are in
  // this list and why 2 alone would prove nothing. At two in and two out the
  // matrix is square, a transposed one is a legal matrix, GStreamer accepts it
  // without a word, and the only symptom is that left and right are swapped. The
  // first non-square width — eight in and two out, on an interface — is where
  // anybody holding the orientation backwards finds out, and there the refusal
  // is SILENT and leaves the previous matrix in force.
  for (const channels of [1, 2, 3, 8, 16, MAX_INPUT_CHANNELS]) {
    for (const grid of [emptyGrid(channels), gridFromMap([], channels).grid, gridFromMap(null, channels).grid]) {
      assert.equal(grid.length, OUTPUTS.length, `${channels}ch: two rows, one per output`);
      for (const row of grid) {
        assert.equal(row.length, channels, `${channels}ch: every row is as wide as the pad`);
      }
    }
  }

  // And a map round-trips through the grid the same way round it went in: a
  // contribution on input 6 must not come back on output 6.
  const wide = [{ output: OUTPUT_RIGHT, input: 6, gain: 0.5 }];
  assert.deepEqual(mapFromGrid(gridFromMap(wide, 8).grid), wide, 'output and input must not swap');
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
  assert.equal(
    inputChannelCount({ inputChannels: MAX_INPUT_CHANNELS * 4 }),
    MAX_INPUT_CHANNELS,
    'a garbled report clamps to the ceiling',
  );
});

test('a width is only half a report: the device it belongs to is the other half', () => {
  // THE STAMP IS THE WHOLE GUARD, and the failure it stops is measured. Selecting
  // an interface while the card is open does not renegotiate instantly; for the
  // length of that reopen the last width anybody published is the card's sixteen,
  // and a crosspoint pressed against a two-channel pad writes a 2x16 matrix —
  // "streaming stopped, reason error (-5)", the capture chain dead before the
  // next level message, every coefficient in the matrix perfectly legal.
  assert.equal(deviceKeyOf({ deviceKey: 'decklink:2747401380' }), 'decklink:2747401380');
  assert.equal(deviceKeyOf({ inputChannels: 16 }), '', 'a report naming no device names none');
  for (const nothing of [undefined, null, {}, { deviceKey: 7 }, { deviceKey: null }]) {
    assert.equal(deviceKeyOf(nothing), '', `${JSON.stringify(nothing)} must name no device`);
  }

  // AN ID MAY CONTAIN COLONS, so the split is on the FIRST one and the rest is
  // carried verbatim. This is the rule internal/config.AudioDeviceKeyFor and
  // audioinput.js's encodeAudioInput both follow, and three places spelling one
  // key two ways is a routing filed where nothing will look for it again.
  assert.equal(captureKindOf('decklink:2747401380'), 'decklink');
  assert.equal(captureKindOf('native:{0.0.1.00000000}.{d6ca5cf3}'), 'native');
  assert.equal(captureKindOf('decklink:a:b:c'), 'decklink', 'only the first colon divides');
  // An unrecognised kind normalises to native on both sides of the boundary, so
  // a key from a newer build cannot make this screen draw a card's furniture
  // over a microphone.
  for (const odd of ['', 'coreaudio:x', 'DECKLINK:x', 'decklink', undefined, null, 7]) {
    assert.equal(captureKindOf(odd), 'native', `${String(odd)} must not read as the card`);
  }
});

test('the mono case gets a line of its own, because a 2x1 grid does not look like one', () => {
  // THE OPERATOR'S RULING. The panel is drawn at every width, including 1 and 2:
  //
  //   "I think we always show it. You may want to flip the channels on a stereo
  //    source, on a mono you may want to route it to be dual mono etc"
  //
  // At width 1 that is two buttons in a column, and two buttons in a column look
  // like a screen that failed to draw rather than like a routing decision. What
  // they actually offer is the choice between both ears and one, and half an
  // audience cannot hear the second — so it is stated rather than inferred.
  const mono = describePad(1, true);
  assert.match(mono, /One input channel/);
  assert.match(mono, /both sides/, 'the mono line must say what routing to both sides is FOR');
  assert.equal(
    /channel 1 left, channel 2 right/.test(mono),
    false,
    'the two-channel default is not the default at width 1, and saying so would be a lie',
  );
  assert.match(describePad(1, false), /One input channel/);
  assert.equal(
    /Nothing routed yet/.test(describePad(1, false)),
    false,
    'a seat that has chosen must not be told nobody has',
  );

  // Two and up keep the sized line, and the never-mapped clause is gst's
  // IsDefault: "nobody has chosen" is a different statement from the routing on
  // screen, and the difference is the first question asked after a wrong feed.
  assert.match(describePad(2, true), /Sized from the 2 channels/);
  assert.match(describePad(2, true), /channel 1 left, channel 2 right/);
  assert.equal(describePad(16, false), 'Sized from the 16 channels this capture negotiated.');
  assert.match(describePad(MAX_INPUT_CHANNELS, false), new RegExp(`${MAX_INPUT_CHANNELS} channels`));

  // WIDTH 0 IS NOT "PRESS START" ANY MORE. Capture is built at launch and held to
  // quit, so a missing width means this input has not negotiated — it is opening,
  // or it failed — and pressing START would not change it by one buffer. The
  // instruction is gone with the design it described.
  const nothing = describePad(0, true);
  assert.equal(/START/.test(nothing), false, 'no width is not a thing START can fix any more');
  assert.match(nothing, /has not negotiated/);
  assert.equal(
    /Press START once to size this grid/.test(ui('channelmap.js')),
    false,
    'the old copy must be gone from the source too, or the next reader restores it',
  );
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
  assert.match(ROUTER_CAVEAT, /immediately/i, 'a live control has to say that it is live');

  // AND IT IS SHORT ENOUGH TO BE READ. It used to carry two more clauses — that
  // there is no gain stage after this one to absorb the sum, and the instruction
  // to turn a contribution down rather than expect the feed to take it — which
  // are the REASON for the third fact and what that fact MEANS. Both are still
  // written down, in ROUTER_CAVEAT's own doc comment, where somebody proposing to
  // allow boost above 0 dB will read them. A settings screen is scanned, and a
  // five-sentence paragraph above a grid is a paragraph nobody finishes.
  assert.ok(
    ROUTER_CAVEAT.length < 200,
    `the caveat is ${ROUTER_CAVEAT.length} characters; it is permanent prose above a grid and ` +
      'the argument for each sentence belongs in the source, not on the screen',
  );
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

test('the grid rebuilds on the DEVICE as well as the width, and collects for neither else', () => {
  const src = ui('channelmap.js');
  const setPad = src.slice(src.indexOf('  function setPad(state) {'), src.indexOf('  function setMap('));
  assert.ok(setPad.length > 0, 'channelmap.js must define setPad');

  // TWO DEVICES CAN NEGOTIATE THE SAME WIDTH. A stereo microphone and a stereo
  // interface both report 2, and a grid that rebuilt only on the number would
  // carry the first one's routing straight onto the second — silently, at a width
  // where it fits perfectly and so goes unnoticed. It is the one case where the
  // width test alone is not merely incomplete but actively wrong.
  assert.match(
    setPad,
    /if \(channels !== padChannels \|\| key !== padKey\)/,
    'a rebuild must follow either half of (deviceKey, width) moving',
  );
  assert.match(setPad, /const key = deviceKeyOf\(state\)/);

  // THE NOT-MY-DEVICE GUARD. Go's SetChannelMap validates a LIVE write against
  // InputChannels() and refuses one that does not fit, so the write itself is
  // safe — but nothing validates a config.json. A Save landing in the window
  // between selecting a device and its pad negotiating would write the PREVIOUS
  // device's routing under the NEW device's key, with a commentator's channel
  // assignment in it.
  const collect = src.slice(src.indexOf('  function collect() {'), src.indexOf('/** setSignal paints'));
  assert.ok(collect.length > 0, 'channelmap.js must define collect');
  assert.match(
    collect,
    /if \(padChannels > 0 && padKey === storedKey\)/,
    'the grid may only speak for the device whose map it is holding',
  );
  assert.match(
    collect,
    /return Array\.isArray\(stored\) \? stored\.map\(\(c\) => \(\{ \.\.\.c \}\)\) : \[\]/,
    'and when it may not, the answer is the map as it was loaded — untouched, and certainly ' +
      'about this device',
  );

  // The map arrives WITH its device, or the guard above has nothing to compare.
  assert.match(src, /function setMap\(map, deviceKey\)/);
  assert.match(src, /storedKey = deviceKeyOf\(\{ deviceKey \}\)/);
});

test('the two lamps are de-carded, and the card one is drawn only on a card seat', () => {
  // LAMP_AUDIO LOST THE WORD "CARD" because the grid did: it reports the channels
  // of whatever device commentary is captured from, and a lamp reading CARD AUDIO
  // beside a Focusrite's eight meters is a lamp about a machine that is not in
  // the path.
  assert.equal(/CARD/.test(LAMP_AUDIO), false, 'the audio lamp must not name hardware it is not about');
  assert.match(LAMP_AUDIO, /AUDIO/);

  // LAMP_VIDEO KEPT IT and is drawn only on a card seat, which is not a leftover
  // — it is the reason the lamp is in this panel at all. decklinkaudiosrc cannot
  // preroll without a decklinkvideosrc in the same pipeline, so on a card seat
  // "no video signal" is the explanation for "no audio on any channel" and the
  // two lamps are read together. On a microphone seat there is no such coupling,
  // and a permanent NOT MEASURED beside a working input is furniture that teaches
  // an operator to ignore a grey lamp.
  assert.match(LAMP_VIDEO, /CARD/);
  const src = ui('channelmap.js');
  assert.match(
    src,
    /lampRow\.lamps\[LAMP_VIDEO\]\.el\.hidden = captureKindOf\(padKey\) !== 'decklink'/,
    'the card lamp must follow the pad’s own device key, not a kind passed in beside it',
  );
  // It starts hidden, or it flashes onto every microphone seat's screen once per
  // open and is taken away again on the first report.
  assert.match(src, /lampRow\.lamps\[LAMP_VIDEO\]\.el\.hidden = true;/);
});

test('nothing the routing screen RENDERS says DeckLink any more', () => {
  // The gate went first and the copy has to follow it, or an operator on an
  // interface reads a panel that names somebody else's hardware and stops
  // reading it. Comments are stripped: this file and channelmap.js discuss the
  // card at length — the measurements are the card's, and the video lamp's whole
  // justification is the card's — and a guard that the WORD is gone must never be
  // satisfiable, or breakable, by an explanation of why.
  const strip = (s) => s.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
  assert.equal(
    /DeckLink|the card’s|the card's/.test(strip(ui('channelmap.js'))),
    false,
    'channelmap.js still renders the word DeckLink or "the card\'s" in the routing copy',
  );

  // validate.js is SLICED to its two routing functions rather than scanned
  // whole, because the rest of it validates videoSource and audioSourceKind —
  // where "DeckLink card" is the correct name of a thing the operator is
  // choosing from a list, and de-carding it would be wrong.
  const validate = ui('validate.js');
  const routing = strip(
    validate.slice(validate.indexOf('function channelMapError('), validate.indexOf('function videoFormatError(')),
  );
  assert.ok(routing.length > 0, 'validate.js must define channelMapError');
  assert.equal(
    /DeckLink|a card presents|the card’s|the card's/.test(routing),
    false,
    'the routing refusals must name the INPUT, not one manufacturer’s hardware',
  );
  // And the ceiling in the message is the CONSTANT, never a typed number: it was
  // "a card presents at most 16" until internal/gst raised gst.MaxInputChannels
  // to 32, and a refusal quoting a bound the application no longer has is worse
  // than no refusal — the operator corrects a channel that was already legal.
  assert.match(routing, /\$\{MAX_INPUT_CHANNELS\}/);
  assert.equal(/at most 16|at most 32/.test(routing), false, 'the bound must not be typed out');

  // settings.js is not stripped-and-scanned whole for the same reason: it builds
  // the video-source control too. The routing group's own strings are what
  // matters.
  const js = ui('settings.js');
  assert.equal(
    /channelMapUnsupported\.textContent =\s*\n?\s*'[^']*card/.test(js),
    false,
    'the unsupported hint must say what the INPUT cannot do, whatever the input is',
  );
  assert.match(js, /This build cannot route this input’s channels/);
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

test('the routing screen is reachable from ANY device that negotiates a width', () => {
  // REWRITTEN, NOT REPAIRED, and the old name is worth keeping in the diff: this
  // test used to be called "the routing screen is only reachable from a DeckLink
  // input" and it asserted `channelMapGroup.hidden = !decklink`. That was the
  // correct guard while a card was the only device that could present
  // unpositioned multichannel audio to this application — but it never was.
  // gstosxcoreaudio.c:886-889 sets the layout to NULL for EVERY CoreAudio source
  // unconditionally, so a Focusrite, an RME or an aggregate negotiates
  // channels=N,mask=0x0 exactly as the card's sixteen do; measured on this
  // machine at 3 and at 16.
  //
  // And the operator overruled the narrower version of the replacement too. The
  // plan proposed `width > 2` on the grounds that they had asked for multitrack:
  //
  //   "I think we always show it. You may want to flip the channels on a stereo
  //    source, on a mono you may want to route it to be dual mono etc"
  //
  // So the gate is a WIDTH, at 1 and up, and a DEVICE. Not a kind.
  const js = ui('settings.js');
  assert.match(js, /openGroup\(channelMapHeading, 'settings-group--channelmap'\)/);
  assert.match(
    js,
    /channelMapGroup\.hidden = !\(channelMapSupported && known && width >= 1\)/,
    'the gate is the build, the device and a width of at least one — never the capture kind',
  );
  assert.equal(
    /channelMapGroup\.hidden = !decklink/.test(js),
    false,
    'gating the routing panel on the card would hide it from every interface that needs it',
  );
  const render = js.slice(
    js.indexOf('function renderChannelMapGroup()'),
    js.indexOf('// NO LISTENER ON THE KIND FIELD'),
  );
  assert.ok(render.length > 0, 'settings.js must define renderChannelMapGroup');
  // KNOWN IS THE DEVICE STAMP, and it is the half a Go-side refusal cannot buy.
  // Selecting an interface while the card is open does not renegotiate
  // instantly; for the length of that reopen the last width published is the
  // card's sixteen, and a crosspoint pressed against a two-channel pad writes a
  // 2x16 matrix — measured as "streaming stopped, reason error (-5)" with the
  // capture chain dead before the next level message.
  assert.match(
    render,
    /const known = channelMapState\.deviceKey === currentAudioDeviceKey\(\)/,
    'the width must be for the device the FORM is showing, or the grid is a stale one',
  );
  assert.match(
    render,
    /const width = known \? inputChannelCount\(channelMapState\) : 0/,
    'an unknown device has no width to draw, whatever number arrived with it',
  );

  // THE GROUP FOLLOWS THE PICKER, NOT A LISTENER ON THE FIELD. audioSourceKind
  // used to be a <select> the operator touched, so a 'change' listener on it was
  // the live link. It is a HIDDEN INPUT now — the one commentary-input picker
  // writes it, along with the two device ids — and assigning an input's value
  // from script fires neither 'input' nor 'change', so that listener would be a
  // group that never opened. The picker calls the renderer directly instead.
  assert.match(
    js,
    /audioInputSelect\.addEventListener\('change', applyAudioInputSelection\)/,
    'the picker must be what drives the setup',
  );
  const apply = js.slice(
    js.indexOf('function applyAudioInputSelection()'),
    js.indexOf("audioInputSelect.addEventListener('change'"),
  );
  assert.ok(apply.length > 0, 'settings.js must define applyAudioInputSelection');
  assert.match(
    apply,
    /renderChannelMapGroup\(\)/,
    'the group must follow the picker live: choosing a device is what reveals its routing',
  );
  // AND SELECTING RE-POINTS CAPTURE, WITHOUT A SAVE. This is the other half of
  // "as soon as the device is selected": a width can only come from a pad that
  // has negotiated, and nothing negotiates until capture is pointed at the
  // device. A screen that waited for Save would show the panel at the next
  // launch, which is the state R2 exists to remove.
  assert.match(
    apply,
    /backend\s*\.selectCommentaryInput\(/,
    'picking a device must re-point capture, or its width never arrives',
  );
  assert.equal(
    /saveConfig|collectConfig/.test(apply),
    false,
    'selecting is not committing: the operator must be able to try an interface and pick again',
  );
  assert.match(apply, /Could not switch the commentary input/, 'a refusal must be reported, not swallowed');

  assert.equal(
    /fields\.audioSourceKind\.input\.addEventListener/.test(js),
    false,
    'a listener on a hidden input never fires; the group would open only on a re-populate',
  );
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(
    populate,
    /renderChannelMapGroup\(\)/,
    'assigning a <select> from script fires no event, so populate must call it itself',
  );
  // AND THE PAD REPORT CALLS IT TOO, which populate cannot cover: a device
  // opening, failing or being swapped happens long after this screen was drawn.
  const adopt = js.slice(
    js.indexOf('function adoptChannelMapState(state)'),
    js.indexOf('// The pad can negotiate while this screen is open'),
  );
  assert.ok(adopt.length > 0, 'settings.js must define adoptChannelMapState');
  assert.match(
    adopt,
    /renderChannelMapGroup\(\)/,
    'the group must follow the width as well as the picker, or it appears only on a re-open',
  );
});

test('the routing heading is built from the width and the device, never from DeckLink', () => {
  // An operator on a Focusrite who reads "DeckLink channel routing" above their
  // own eight channels concludes the panel is about somebody else's hardware and
  // stops reading it. And the width is IN the heading because it is the one
  // number that says whether the right thing opened — sixteen crosspoints where
  // two were expected is visible from across the room there, and not in a grid.
  assert.equal(describeRoutingHeading(0), 'Channel routing');
  assert.equal(describeRoutingHeading(1), 'Channel routing — 1 channel');
  assert.equal(describeRoutingHeading(2), 'Channel routing — 2 channels');
  assert.equal(
    describeRoutingHeading(8, 'Scarlett 18i20'),
    'Channel routing — Scarlett 18i20, 8 channels',
  );
  // A device with no name yet is a device this screen has not been told about,
  // not a device called "". The width still earns the heading.
  assert.equal(describeRoutingHeading(16, '   '), 'Channel routing — 16 channels');

  const js = ui('settings.js');
  assert.equal(
    /'DeckLink channel routing'/.test(js),
    false,
    'the heading must not be a literal naming one manufacturer’s hardware',
  );
  assert.match(
    js,
    /channelMapHeading\.textContent = describeRoutingHeading\(/,
    'settings.js must build the heading rather than restate it',
  );
});

test('the map is populated and collected, so a Save cannot silently delete it', () => {
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  // THE STORE IS PER DEVICE and the grid draws exactly one entry of it: the one
  // belonging to the device this seat captures from. populate reads that entry,
  // collectConfig writes it back over the others rather than instead of them.
  assert.match(populate, /carriedChannelMaps = adoptChannelMaps\(config\.channelMaps\)/);
  assert.match(populate, /const savedRoutingKey = currentAudioDeviceKey\(\)/);
  // THE KEY GOES IN WITH THE MAP. collect() will not write a grid back under a
  // device whose map it is not holding (the not-my-device guard), and the only
  // way it can know which device that is, is to be told when the map arrives.
  assert.match(
    populate,
    /channelMapView\.setMap\(carriedChannelMaps\[savedRoutingKey\], savedRoutingKey\)/,
    'the map and the device it belongs to must be handed in together',
  );
  assert.match(collect, /channelMaps: collectChannelMaps\(\)/);
  // AND IT IS COLLECTED WHILE THE GROUP IS HIDDEN. collectConfig replaces the
  // whole document, so a seat whose routing panel is not on screen pressing Save
  // on an unrelated field would otherwise delete a commentator's channel
  // assignment.
  assert.equal(
    /if \(!?decklink\)[\s\S]{0,80}channelMaps/.test(collect),
    false,
    'the store must be collected unconditionally, never only when the group is on screen',
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

test('the fake backend brings CAPTURE up at load, not a card up at START', () => {
  // REWRITTEN, not repaired. The previous version of this test asserted that
  // fakeCardUp() was called from the fake session start and fakeCardDown() from
  // its stop, which was the correct guard while capture WAS the session: one
  // pipeline, built at START, destroyed at STOP.
  //
  // Capture is now built at launch and held to quit, and the send pipeline is
  // the only thing START makes. So the old assertions have been inverted rather
  // than dropped — the fake must bring capture up at MODULE LOAD, and START and
  // STOP must not touch it. Leaving the old ones passing would have meant the
  // dev loop could only ever see the routing grid while sending, which is the
  // exact state R2 exists to remove.
  const backend = ui('backend.js');
  assert.match(backend, /function fakeCaptureUp\(\)/);
  assert.match(
    backend,
    /if \(fakeBrowserSession\) \{\s*setTimeout\(fakeCaptureUp, 0\);/,
    'the fake capture must come up at module load: meters, pad, signal and preview before START. ' +
      'One macrotask late, which is this fake’s domReady — see capture.test.js for why that is not ' +
      'the same as "at load, whenever"',
  );

  // ...and only in a browser. node --test imports this module with no window,
  // and a ticker started there would hang the suite rather than fail it.
  assert.match(backend, /const fakeBrowserSession = usingFakeBackend && typeof window !== 'undefined';/);

  // START AND STOP MUST NOT OWN CAPTURE. Sliced rather than searched whole,
  // because the names appear all over the fake and what matters is which
  // function calls them.
  const start = backend.slice(backend.indexOf('function fakeStart()'), backend.indexOf('function fakeStop()'));
  const stop = backend.slice(backend.indexOf('function fakeStop()'), backend.indexOf('function installFakeConsoleHandle()'));
  assert.ok(start.length > 0 && stop.length > 0, 'the slices must have found both functions');
  for (const [what, src] of [['fakeStart', start], ['fakeStop', stop]]) {
    assert.equal(
      /fakeCaptureUp\(\)|fakeCaptureDown\(\)|startFakeLevels\(\)|stopFakeLevels\(\)|startFakeChannelLevels\(\)|stopFakeChannelLevels\(\)/.test(src),
      false,
      `${what} must not build or tear down capture: meters, the negotiated width, the routing and ` +
        'the signal all survive STOP, and a fake that took them away would agree with the product ' +
        'this change replaced',
    );
  }

  // The teardown still exists — it is what a device change, a restart and a
  // quit do — and it is still what emits the zero-frames.
  assert.match(backend, /function fakeCaptureDown\(\)/);
  assert.match(
    backend,
    /peak: \[FAKE_LEVELS_SILENCE_DB, FAKE_LEVELS_SILENCE_DB\]/,
    'a capture teardown must silence the meters rather than freeze them at the last level',
  );

  // Only SOME channels carry audio, and no two of them move together. The first
  // is the find-the-commentator interaction; the second is the matrix, where a
  // transpose is invisible at 2x2 and identical channels would make a flipped
  // map look exactly like a correct one on the programme meter they feed.
  assert.match(
    backend,
    /const FAKE_LIVE_CHANNELS = \[\d+(?:, \d+)+\];/,
    'only some fake channels may carry audio — a fake where all sixteen moved together would let ' +
      'the find-the-commentator interaction break unnoticed',
  );
  assert.match(
    backend,
    /const FAKE_CHANNEL_PHASE_STEPS = \d+;/,
    'each fake input channel needs its own phase, or a transposed matrix passes unnoticed',
  );
  assert.match(
    backend,
    /function fakeProgrammePeaksAt\(step\)/,
    'the programme meter must be the per-channel frame reduced THROUGH the map in force, which is ' +
      'where alevel sits — otherwise the routing grid has no visible effect in a dev session',
  );

  // The device model reaches every width the matrix was measured working at.
  const table = backend.slice(backend.indexOf('const FAKE_DEVICES = ['), backend.indexOf('FAKE_DEFAULT_INPUT_ID ='));
  const widths = new Set([...table.matchAll(/channels: (\d+),/g)].map((m) => Number(m[1])));
  for (const width of [1, 2, 3, 8, 16, 32]) {
    assert.ok(
      widths.has(width),
      `the fake device table must be able to present ${width} channels: the panel is drawn at every ` +
        'width the pad negotiates, including 1 (dual mono) and 2 (a stereo flip), and 32 is ' +
        'gst.MaxInputChannels',
    );
  }

  // Zero is still reachable and still draws no grid — it is the reopen window of
  // a device change now, rather than "nobody has pressed START".
  assert.match(backend, /inputChannels: 0,/, 'not-negotiated must still be expressible');
  assert.match(backend, /setSignal: \(state, flaps\) =>/, 'a real loss is reported once in a match; this is how it is reached');
  assert.match(backend, /setChannelCount: \(channels\) =>/);
  assert.match(backend, /selectInput: \(kind, id\) =>/, 'the dev loop must be able to change device without a Wails build');
});

test('the channelMap payload carries the device the width belongs to', () => {
  // THE STAMP IS THE WHOLE GUARD. Selecting a Focusrite while the card is open
  // does not re-negotiate instantly, and for the length of that reopen the last
  // width published is the card's 16. A grid that believed it still offered
  // sixteen crosspoints over a two-channel pad writes a 2x16 matrix on the first
  // press — measured on the real card as "streaming stopped, reason error (-5)"
  // with the capture chain dead before the next level message.
  //
  // Go's SetChannelMap refuses a map that does not fit InputChannels(), so the
  // write itself is safe; what the key buys is that the button is never on
  // screen, and — the half a refusal cannot help with at all — that a narrowed
  // grid collected at Save cannot overwrite the other device's saved routing.
  const backend = ui('backend.js');
  assert.match(
    backend,
    /export const EVENT_CHANNEL_MAP = 'channelMap';/,
    'the event name is app.go’s and must not drift',
  );

  // THE GO SIDE IS ASSERTED, NOT ONLY THE FAKE, and that is the half this test
  // was missing rather than a belt on one it already had. Every assertion below
  // this point reads backend.js's fake device model, which is written in this
  // repository to be correct — so the whole suite went green over an app.go that
  // published no key at all, a real build in which the gate could never match
  // and the routing panel never appeared on any seat, at any width. The fake
  // cannot be the witness for a contract whose other end is in Go.
  assert.match(
    read(repoRoot, 'app.go'),
    /DeviceKey\s+string\s+`json:"deviceKey"`/,
    'app.go’s channelMapPayload must carry the device key: settings.js hides the routing group ' +
      'until the key in the report equals the key of the device the form is showing, so a payload ' +
      'without one hides the panel forever',
  );

  assert.match(backend, /deviceKey: fakeCommentaryKey,/, 'the fake must stamp every payload with its device');
  assert.match(
    backend,
    /\{ inputChannels: 0, map: \[\], isDefault: true, deviceKey: '' \}/,
    'and the could-not-ask fallback must carry an EMPTY key: it matches no selection, so the grid ' +
      'draws nothing rather than reading "this device negotiated zero channels"',
  );

  // The saved routing is per device, for the reason config.ChannelMaps exists:
  // a single slot cannot say which device its contents belong to, and with
  // always-live capture a Save made while a stereo microphone is selected would
  // narrow the card's sixteen-wide routing without saying so.
  assert.match(backend, /const fakeChannelMaps = new Map\(\);/);
  assert.match(backend, /fakeChannelMaps\.set\(fakeCommentaryKey, written\);/);
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
  // BOUNDED AT THE NEXT APPENDED BLOCK, not run to the end of the file. This
  // used to slice to EOF, which was right only for as long as the routing block
  // was the last one in the stylesheet — and the first block appended after it
  // (the video source and the card preview) failed this assertion with a
  // flex-basis that has nothing to do with a dB scale. main.css's convention is
  // that each late block opens with its own banner, so that is the boundary.
  //
  // AND THE LOCATOR IS CHECKED BEFORE IT IS USED — TWICE OVER, because it was
  // wrong in two different ways and both of them passed.
  //
  // It read sheet.indexOf('DECKLINK CHANNEL ROUTING') until the banner was
  // de-carded with the rest of this screen. On a miss indexOf is -1, slice(-1)
  // is the last CHARACTER of the file, and every assertion below passes on it.
  // Hence the `> 0`.
  //
  // The second was quieter and had been there all along: main.css writes a
  // banner as one `/* … */` comment PER LINE, so slicing from the banner TEXT
  // and stopping at the next `/* ====` stopped at the banner's own closing rule
  // — and the "block" being scanned for a flex-basis was six lines of prose,
  // with not one CSS rule in it. So the slice starts AFTER the banner closes and
  // runs to the next appended block's banner, and the assertions below are
  // asserted against the rules they were written for.
  const banner = sheet.indexOf('CHANNEL ROUTING (appended block');
  assert.ok(banner > 0, 'main.css must still carry the routing block’s banner, or this test proves nothing');
  const bannerEnd = sheet.indexOf('*/', sheet.indexOf('/* ====', banner)) + 2;
  const next = sheet.indexOf('/* ====', bannerEnd);
  const block = sheet.slice(bannerEnd, next > 0 ? next : undefined);
  assert.ok(
    block.includes('.channelmap-grid {') && block.includes('.channelmap-trim {'),
    'the slice must reach the routing block’s RULES, not just its banner prose',
  );
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

  // THE ROW COUNT IS NOW 1..32, and the two ends need different things of the
  // layout. gst.MaxInputChannels doubled, so a full grid is about 1,300px — it
  // would push the per-output sums, the clip warning and the trim panel off the
  // bottom of the screen, which are the three things the grid is read WITH. The
  // grid scrolls itself rather than the form.
  assert.match(block, /max-height:\s*60vh/, 'a 32-row grid must scroll itself, not the settings form');
  assert.match(block, /overflow-y:\s*auto/);
  // And the header row stays put, or an operator on channel 28 is looking at four
  // unlabelled columns — two of which decide which ear a commentator arrives in,
  // where guessing wrong is inaudible until somebody complains.
  const head = block.slice(block.indexOf('.channelmap-head {'), block.indexOf('.channelmap-name {'));
  assert.ok(head.length > 0, 'main.css must style .channelmap-head');
  assert.match(head, /position:\s*sticky/);
  assert.match(head, /top:\s*0/);
  assert.match(
    head,
    /background:\s*var\(--bg\)/,
    'a transparent sticky cell lets the rows show through the row gap as they pass underneath',
  );
  // NO ROW COUNT ANYWHERE IN THE BLOCK. A height, a repeat() or a minmax sized to
  // sixteen rows is the advertised width creeping back in through the stylesheet,
  // which is the one place the pad's report cannot correct it.
  assert.equal(
    /repeat\(\s*(?:16|32)\b|grid-template-rows/.test(block),
    false,
    'the row count comes from the pad and may never be written into the CSS',
  );
});
