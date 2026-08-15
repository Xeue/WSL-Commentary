/**
 * Tests for the VIDEO OK lamp's comparison target, and for deriveHonestLine,
 * the withdrawn honest line.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ================= THE DEFECT THE FIRST HALF PREVENTS =======================
 *
 * The VIDEO OK lamp compared the detected format against a hard-coded
 * {h264, 1920, 1080, 50}. M2L-X's raster is a per-instance CONFIGURATION and
 * every source must match it, so on a correctly configured 720p50 facility that
 * lamp read RED on a feed that was arriving perfectly — and the only remedy
 * available to the operator was to learn to ignore a red lamp, which is the
 * habit the whole row exists to prevent. The raster half of the comparison is
 * now supplied by the app layer; the codec half deliberately is not. Both halves
 * are pinned below, along with the property that survives all of it: on a
 * mismatch the operator is shown what ACTUALLY arrived, verbatim, and not a
 * verdict.
 *
 * ======================= WHY THE SECOND HALF EXISTS =========================
 *
 * The honest line was removed from the main screen at the operator's request,
 * and the instruction with it was that the function and its wording are KEPT —
 * the same treatment internal/mixer's golden/Compare machinery got when the
 * drift panel was withdrawn from the mixer drawer.
 *
 * A kept function with no caller and no test is a function the next person
 * deletes as dead code, and the wording is the part that took several passes to
 * get right: the caveat is in every variant, and the sentence in front of it
 * must not claim the feed is reaching the switcher while nothing is being sent.
 * So the claim is pinned here rather than left to a comment.
 *
 * These tests do NOT assert that anything renders it. Nothing does, on purpose.
 * They assert that if it is ever put back, it will still be true.
 *
 * The DOM half of lamps.js (createLamp, createLampRow) is not covered here:
 * there is no jsdom, and a shim widened until a test passes stops being
 * evidence. The derivation functions are pure and need none.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  LEVEL,
  DEFAULT_CONFORM_TARGET,
  normaliseConformTarget,
  describeConformTarget,
  deriveStatusLamps,
  deriveHonestLine,
} from './lamps.js';

// --------------------------------------------------------------------------
// The VIDEO OK lamp's comparison target
// --------------------------------------------------------------------------

/** healthyStatus builds a "status" payload whose video is `video`. */
function healthyStatus(video) {
  return {
    stale: false,
    streamState: 'streaming',
    video,
    audio: [{ codec: 'aac', sampleRate: 48000, channels: 2, raw: 'aac 48000 2ch' }],
  };
}

/** The measured healthy 1080p50 shape, from internal/m2lx/format.go's capture. */
const VIDEO_1080P50 = {
  codec: 'h264',
  width: 1920,
  height: 1080,
  frameRate: 50,
  raw: 'codec="h264" width=1920 height=1080 frame_rate="50" scan_type="P"',
};

/** The same feed on an instance configured 720p50. */
const VIDEO_720P50 = {
  codec: 'h264',
  width: 1280,
  height: 720,
  frameRate: 50,
  raw: 'codec="h264" width=1280 height=720 frame_rate="50" scan_type="P"',
};

test('with no conform target supplied, the lamp behaves exactly as it always did', () => {
  // The fallback is not a nicety: between page load and the first answer out of
  // GetConformTarget there is a window in which nothing is known, and the
  // lamp must be no worse in it than the build that had only the constant.
  const lamps = deriveStatusLamps(healthyStatus(VIDEO_1080P50));
  assert.equal(lamps.video.level, LEVEL.GREEN);
  assert.equal(lamps.video.text, '1080P50');

  for (const nothing of [undefined, null, {}, 'nonsense', 0]) {
    const l = deriveStatusLamps(healthyStatus(VIDEO_1080P50), nothing);
    assert.equal(l.video.level, LEVEL.GREEN, `${JSON.stringify(nothing)} means "not known"`);
    assert.equal(l.video.text, '1080P50');
  }
});

test('THE DEFECT: a 720p50 feed on a 720p50 instance is GREEN, and says 720P50', () => {
  // This is the whole point. Under the old constant this exact status lit the
  // lamp red on a perfectly good feed, on every instance not configured 1080p50.
  const before = deriveStatusLamps(healthyStatus(VIDEO_720P50));
  assert.equal(before.video.level, LEVEL.RED, 'still red when the switcher is not known to be 720p');

  const after = deriveStatusLamps(healthyStatus(VIDEO_720P50), {
    width: 1280,
    height: 720,
    frameRate: 50,
  });
  assert.equal(after.video.level, LEVEL.GREEN);
  assert.equal(after.video.text, '720P50', 'the green text names the raster, not a constant');
});

test('and the mirror image: a 1080p50 feed into a 720p50 instance is RED', () => {
  // Every source must match the instance's configured raster. A derived target
  // that only ever widened what counts as good would be worse than the constant.
  const lamps = deriveStatusLamps(healthyStatus(VIDEO_1080P50), {
    width: 1280,
    height: 720,
    frameRate: 50,
  });
  assert.equal(lamps.video.level, LEVEL.RED);
  assert.equal(lamps.video.text, VIDEO_1080P50.raw, 'and it shows what arrived, verbatim');
});

test('a mismatch shows Raw and never a verdict — the reason the lamp has text', () => {
  // "1080P50 expected" tells the operator nothing about what they are actually
  // sending. Raw names the fields M2L-X reported, which is the only thing on
  // screen that can start a diagnosis.
  const wrong = { codec: 'h264', width: 1920, height: 1080, frameRate: 25, raw: 'frame_rate="25"' };
  const lamps = deriveStatusLamps(healthyStatus(wrong));
  assert.equal(lamps.video.level, LEVEL.RED);
  assert.equal(lamps.video.text, 'frame_rate="25"');

  // And when there is no format at all — a stopped node, where M2L-X sends
  // format: null and format.go renders Raw as "" — the lamp still says
  // something true rather than showing an empty chip.
  const none = deriveStatusLamps(healthyStatus({ codec: '', width: 0, height: 0, frameRate: 0, raw: '' }));
  assert.equal(none.video.text, 'NO VIDEO');
});

test('the CODEC is absolute: h264 whatever raster is targeted', () => {
  // H.264 is the only thing this build can encode — mfh264enc on Windows,
  // vtenc_h264 on macOS — so it is a statement about our own pipeline. A
  // derived codec would let an instance whose commentary input was left on the
  // h265 default (measured: input 3 SLATE on matchH is configured h265) light
  // this lamp red on a feed the operator has no way to change.
  const h265 = { codec: 'h265', width: 1280, height: 720, frameRate: 50, raw: 'codec="h265"' };
  const lamps = deriveStatusLamps(healthyStatus(h265), {
    width: 1280,
    height: 720,
    frameRate: 50,
    codec: 'h265', // present in the real payload, and deliberately ignored
  });
  assert.equal(lamps.video.level, LEVEL.RED, 'a codec that is not h264 is never green');
  assert.equal(lamps.video.text, 'codec="h265"');
});

test('normaliseConformTarget replaces bad fields ONE AT A TIME', () => {
  // A source that knows the raster but reports a zero frame rate would make the
  // comparison unsatisfiable — no real format has frameRate 0 — and the lamp
  // would sit red forever with nothing on screen saying why.
  assert.deepEqual(normaliseConformTarget({ width: 1280, height: 720, frameRate: 0 }), {
    width: 1280,
    height: 720,
    frameRate: DEFAULT_CONFORM_TARGET.frameRate,
  });
  for (const bad of [undefined, null, 0, -1, NaN, Infinity, '', 'fifty', {}]) {
    const got = normaliseConformTarget({ width: bad, height: 720, frameRate: 25 });
    assert.equal(got.width, DEFAULT_CONFORM_TARGET.width, `width ${String(bad)} falls back`);
    assert.equal(got.height, 720, 'and the fields beside it are kept');
    assert.equal(got.frameRate, 25);
  }
  assert.deepEqual(normaliseConformTarget(null), { ...DEFAULT_CONFORM_TARGET });
});

test('a frame rate arriving as a STRING is accepted: the two sources disagree', () => {
  // internal/m2lx/format.go's documented trap — switcher_status renders
  // frame_rate as the string "50" while width and height beside it are numbers
  // — and REST's /api/input/router/list renders the same quantity as the number
  // 50 (measured, matchH, 2026-08-15). Neither shape may be assumed.
  assert.equal(normaliseConformTarget({ frameRate: '25' }).frameRate, 25);
  const lamps = deriveStatusLamps(healthyStatus(VIDEO_720P50), {
    width: '1280',
    height: '720',
    frameRate: '50',
  });
  assert.equal(lamps.video.level, LEVEL.GREEN);
  assert.equal(lamps.video.text, '720P50');
});

test('describeConformTarget speaks the operator’s vocabulary', () => {
  assert.equal(describeConformTarget({ width: 1920, height: 1080, frameRate: 50 }), '1080P50');
  assert.equal(describeConformTarget({ width: 1280, height: 720, frameRate: 50 }), '720P50');
  assert.equal(describeConformTarget({ width: 3840, height: 2160, frameRate: 25 }), '2160P25');
  // The P is this application's own: everything it can send is progressive (a
  // still image through imagefreeze; there is no interlaced path in
  // gst_cgo.go), so the letter is a claim about the feed and not an unchecked
  // claim about the switcher.
  assert.equal(describeConformTarget(undefined), '1080P50');
});

test('the conform target changes nothing else about the row', () => {
  // The grey and audio rules are untouched by any of this, and must stay so:
  // greying is the honest rendering of "this app cannot see its input", and an
  // EMPTY audio array is the MP2/AC-3 silent-drop signature, red and never grey.
  const target = { width: 1280, height: 720, frameRate: 50 };
  const nothing = deriveStatusLamps(undefined, target);
  assert.equal(nothing.video.level, LEVEL.GREY);
  assert.equal(nothing.video.text, 'NO STATUS');
  assert.equal(nothing.unavailable, false);

  const stale = deriveStatusLamps({ stale: true }, target);
  assert.equal(stale.video.level, LEVEL.GREY);
  assert.equal(stale.video.text, 'STATUS UNAVAILABLE');
  assert.equal(stale.unavailable, true);

  const noAudio = deriveStatusLamps(
    { stale: false, streamState: 'streaming', video: VIDEO_720P50, audio: [] },
    target,
  );
  assert.equal(noAudio.video.level, LEVEL.GREEN, 'video can be green while audio is not');
  assert.equal(noAudio.audio.level, LEVEL.RED);
  assert.equal(noAudio.audio.text, 'NO AUDIO (DROPPED?)');
  assert.equal(noAudio.switcher.level, LEVEL.GREEN);
});

// --------------------------------------------------------------------------
// deriveHonestLine, withdrawn from the GUI and kept
// --------------------------------------------------------------------------

/** The caveat that must be in EVERY variant, in some casing. */
const CAVEAT = /does not confirm you are audible on the broadcast output/i;

test('every honest line carries the caveat, whatever the sender is doing', () => {
  // The caveat is the point of the line. Nothing this application can see
  // proves the commentator is audible on air: the switcher accepting a feed is
  // not the gallery having faded it up.
  for (const state of ['CONNECTED', 'CONNECTING', 'DRAINING', 'BACKOFF', 'STOPPED', undefined, null, 'WAT']) {
    assert.match(deriveHonestLine(state), CAVEAT, `state ${String(state)}`);
  }
});

test('only CONNECTED claims the feed is reaching the switcher', () => {
  // The bug this rules out: the line said "your feed is reaching the switcher"
  // while STOPPED, which was simply false. A permanent honest line that is
  // sometimes a lie is worse than no line at all.
  assert.match(deriveHonestLine('CONNECTED'), /Your feed is reaching the switcher\./);

  for (const state of ['CONNECTING', 'DRAINING', 'BACKOFF', 'STOPPED', undefined, null, 'WAT']) {
    assert.doesNotMatch(
      deriveHonestLine(state),
      /Your feed is reaching the switcher\./,
      `state ${String(state)} must not claim the feed is arriving`,
    );
  }
});

test('the transitional states say NOT YET, and the terminal ones say nothing is being sent', () => {
  for (const state of ['CONNECTING', 'DRAINING']) {
    assert.match(deriveHonestLine(state), /not reaching the switcher yet/i, `state ${state}`);
  }
  assert.match(deriveHonestLine('BACKOFF'), /nothing is reaching the switcher/i);
  // STOPPED, no event yet, and any state this build does not recognise all
  // claim the least: claiming less than is true is the only safe direction to
  // be wrong in here.
  for (const state of ['STOPPED', undefined, null, 'WAT']) {
    assert.match(deriveHonestLine(state), /You are not sending/, `state ${String(state)}`);
  }
});
