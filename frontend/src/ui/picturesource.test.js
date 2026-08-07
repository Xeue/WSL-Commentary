/**
 * Tests for the PICTURE source control, and for the one thing it must never do.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * There was a control in this position that switched the commentator's AUDIO
 * between WebRTC and SRT. It was built from a misreading of what was asked for:
 *
 *   "I want DIRTY PICTURES. I want PGM high res pictures in what the
 *    commentator sees. With the audio coming from Kinesis."
 *
 * SRT is the picture. Built as an audio path, selecting "SRT" took the
 * operator's audio away — which is not a subtle failure, it is the single worst
 * thing this application can do, and it shipped.
 *
 * So the first group below is about the control doing what it says, and the
 * SECOND group is about it being structurally incapable of reaching the audio at
 * all. That second group reads SOURCE, for the reason mixerwiring.test.js does:
 * the property is "there is no call site", and a property about the absence of
 * call sites cannot be demonstrated by driving anything.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  PICTURE_SOURCE_SRT,
  PICTURE_SOURCE_MOSAIC,
  PICTURE_SOURCES,
  PICTURE_SOURCE_KEY,
  DEFAULT_PICTURE_SOURCE,
  PICTURE_NOTE,
  PICTURE_STATE_SHOWING,
  PICTURE_STATE_CONNECTING,
  PICTURE_STATE_BACKOFF,
  PICTURE_STATE_STOPPED,
  isValidPictureSource,
  normalisePictureSource,
  derivePictureSourceEffects,
  pictureSourceLabel,
  describePictureSource,
  describePictureShowing,
} from './picturesource.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

/**
 * codeOnly strips comments, so that a note DISCUSSING a call cannot satisfy — or
 * break — a guard that counts call sites.
 */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

// --------------------------------------------------------------------------
// The switch itself
// --------------------------------------------------------------------------

test('there are exactly two pictures, and both are labelled and costed', () => {
  assert.deepEqual(
    PICTURE_SOURCES.map((s) => s.value),
    [PICTURE_SOURCE_SRT, PICTURE_SOURCE_MOSAIC],
    'SRT is drawn first: it is the one that was asked for',
  );
  for (const s of PICTURE_SOURCES) {
    assert.ok(s.label.length > 0);
    assert.ok(s.summary.length > 0);
    assert.ok(s.cost.length > 0);
  }
  assert.match(pictureSourceLabel('nonsense'), /not one of the two/);
});

test('SRT is the default picture', () => {
  // The mosaic is what you FALL BACK to, not what you start on by choice.
  assert.equal(DEFAULT_PICTURE_SOURCE, PICTURE_SOURCE_SRT);
  assert.equal(derivePictureSourceEffects(undefined).source, PICTURE_SOURCE_SRT);
});

test('normalisePictureSource never returns a picture that does not exist', () => {
  for (const bad of [undefined, null, '', 'webrtc', 'kvs', 3, {}, [], NaN, true]) {
    assert.ok(
      isValidPictureSource(normalisePictureSource(bad)),
      `${String(bad)} normalises to a real picture`,
    );
  }
  assert.equal(normalisePictureSource('nope', PICTURE_SOURCE_MOSAIC), PICTURE_SOURCE_MOSAIC);
  assert.equal(normalisePictureSource('nope', 'also nope'), DEFAULT_PICTURE_SOURCE);
});

test('selecting SRT wants the receiver running; selecting the mosaic does not', () => {
  assert.equal(derivePictureSourceEffects(PICTURE_SOURCE_SRT).wantSRT, true);
  assert.equal(derivePictureSourceEffects(PICTURE_SOURCE_MOSAIC).wantSRT, false);
});

test('SRT is only ON SCREEN once the receiver says SHOWING', () => {
  // Running and showing are different claims. A receiver in CONNECTING or
  // BACKOFF is holding a fan-out slot and delivering nothing; painting an opaque
  // native window for it would replace a soft picture of the match with black.
  const showing = derivePictureSourceEffects(PICTURE_SOURCE_SRT, PICTURE_STATE_SHOWING);
  assert.equal(showing.showingSRT, true);
  assert.equal(showing.showingMosaic, false);

  for (const state of [PICTURE_STATE_CONNECTING, PICTURE_STATE_BACKOFF, PICTURE_STATE_STOPPED, null]) {
    const e = derivePictureSourceEffects(PICTURE_SOURCE_SRT, state);
    assert.equal(e.wantSRT, true, `${state}: the receiver is still wanted`);
    assert.equal(e.showingSRT, false, `${state}: but nothing is painted over the page`);
    assert.equal(e.showingMosaic, true, `${state}: the mosaic is what is on screen`);
    assert.equal(e.fallenBack, true, `${state}: and it is a fallback, not a choice`);
  }
});

test('THERE IS NO STATE IN WHICH NEITHER PICTURE IS SHOWING', () => {
  // A commentator staring at black, wondering whether the match has started, is
  // the outcome this function exists to make unreachable. Checked over every
  // input including ones nobody means to pass — a hand-edited config, an older
  // file, a state string from a future Go build.
  const sources = [PICTURE_SOURCE_SRT, PICTURE_SOURCE_MOSAIC, undefined, null, '', 'SRT', 0, {}, []];
  const states = [
    PICTURE_STATE_SHOWING,
    PICTURE_STATE_CONNECTING,
    PICTURE_STATE_BACKOFF,
    PICTURE_STATE_STOPPED,
    'showing',
    'WHAT',
    undefined,
    null,
    42,
  ];
  for (const source of sources) {
    for (const state of states) {
      const e = derivePictureSourceEffects(source, state);
      assert.equal(
        e.showingSRT || e.showingMosaic,
        true,
        `${JSON.stringify(source)}/${JSON.stringify(state)} must leave SOME picture on screen`,
      );
      assert.equal(
        e.showingSRT,
        !e.showingMosaic,
        `${JSON.stringify(source)}/${JSON.stringify(state)} must show exactly one`,
      );
    }
  }
});


test('the effects object is frozen, so nothing can flip one flag and not the other', () => {
  assert.ok(Object.isFrozen(derivePictureSourceEffects(PICTURE_SOURCE_SRT, PICTURE_STATE_SHOWING)));
});

test('choosing the mosaic is not reported as a fallback', () => {
  // "Mosaic because you chose mosaic" is a choice. "Mosaic because SRT is not
  // up" is a degradation. Dressing the first as the second is a permanent amber
  // badge that people learn to ignore.
  const chosen = derivePictureSourceEffects(PICTURE_SOURCE_MOSAIC, PICTURE_STATE_STOPPED);
  assert.equal(chosen.fallenBack, false);
  assert.equal(describePictureShowing(chosen, PICTURE_STATE_STOPPED).good, true);
});

test('the badge says which picture is on screen, and calls out the fallback', () => {
  const srt = describePictureShowing(
    derivePictureSourceEffects(PICTURE_SOURCE_SRT, PICTURE_STATE_SHOWING),
    PICTURE_STATE_SHOWING,
  );
  assert.match(srt.text, /SRT/);
  assert.match(srt.text, /1080/, 'the resolution is the whole reason for choosing it');
  assert.equal(srt.good, true);

  const fallen = describePictureShowing(
    derivePictureSourceEffects(PICTURE_SOURCE_SRT, PICTURE_STATE_BACKOFF),
    PICTURE_STATE_BACKOFF,
  );
  assert.match(fallen.text, /MOSAIC/);
  assert.equal(fallen.good, false, 'the fallback must be visibly a fallback');
  assert.match(fallen.detail, /retrying/i, 'and must say what the SRT picture is doing');

  for (const s of [srt, fallen]) {
    assert.ok(s.text.length > 0 && s.detail.length > 0);
  }
});

test('the SRT option states what it is and what it costs', () => {
  const srt = PICTURE_SOURCES.find((s) => s.value === PICTURE_SOURCE_SRT);
  assert.match(srt.summary, /1920x1080|1080/, 'the resolution');
  assert.match(srt.summary, /dirty|programme/i, 'and that it is the DIRTY programme feed');
  assert.match(srt.cost, /15 Mbit/i, 'the real bitrate');
  assert.match(srt.cost, /fan-out/i, 'and the fan-out slot it holds');
  assert.match(describePictureSource(PICTURE_SOURCE_SRT), /15 Mbit/i);
});

test('the mosaic option is honest about being soft rather than being hidden', () => {
  const mosaic = PICTURE_SOURCES.find((s) => s.value === PICTURE_SOURCE_MOSAIC);
  assert.match(mosaic.summary, /soft/i);
  assert.match(mosaic.summary, /2240x1440|mosaic|multiviewer/i);
});

// --------------------------------------------------------------------------
// AND IT MUST NOT REACH THE AUDIO
// --------------------------------------------------------------------------

test('the effects object carries nothing about audio', () => {
  // A field here is a field somebody wires up. The last time this control had
  // one, selecting it silenced the operator.
  const e = derivePictureSourceEffects(PICTURE_SOURCE_SRT, PICTURE_STATE_SHOWING);
  assert.deepEqual(
    Object.keys(e).sort(),
    ['connected', 'fallenBack', 'showingMosaic', 'showingSRT', 'source', 'wantSRT'],
    'derivePictureSourceEffects must describe the picture and nothing else',
  );
});

test('picturesource.js does not know the word for the headphones', () => {
  const src = codeOnly(ui('picturesource.js'));
  for (const forbidden of [
    'setAudioEnabled',
    'webrtcAudible',
    'srtRunning',
    'setSinkId',
    'headphoneDeviceId',
    'headphoneEndpointId',
    'returnSource',
    'returnChannel',
    'returnMid',
  ]) {
    assert.ok(
      !src.includes(forbidden),
      `picturesource.js must not mention ${forbidden}: this module decides a picture`,
    );
  }
});

test('the note under the control says the audio does not move', () => {
  assert.match(PICTURE_NOTE, /audio/i);
  assert.match(PICTURE_NOTE, /kinesis/i, 'and says where it comes from instead');
  assert.match(PICTURE_NOTE, /discard/i, "and that the SRT stream's own audio is thrown away");
  assert.ok(ui('home.js').includes('PICTURE_NOTE'), 'home.js must render it');
});

test('the picture handler in app.js touches nothing audible', () => {
  // The structural statement, and the one that survives a refactor: whatever
  // else onPictureSourceChange grows, it may not reach the audio.
  const src = codeOnly(ui('app.js'));
  const start = src.indexOf('async function onPictureSourceChange(source)');
  assert.ok(start > 0, 'app.js must handle the picture switch');
  const body = src.slice(start, src.indexOf('\n  }', start));

  for (const forbidden of [
    'returnPath',
    'setAudioEnabled',
    'setSinkId',
    'setChannelMode',
    'setReturnMid',
    'startReturn',
    'stopReturn',
    'returnSource',
    'safeMonitorCall',
  ]) {
    assert.ok(
      !body.includes(forbidden),
      `onPictureSourceChange reaches ${forbidden}. Selecting a PICTURE must not be able ` +
        "to change what is in the commentator's ears — that is the bug this work removed.",
    );
  }
});

test('the picture is persisted under its own config key, not over the audio one', () => {
  // Writing the picture choice into returnSource would be the same bug wearing
  // a different name: config.ValidateReturn would accept "srt", App.StartReturn
  // would start the AUDIO return on the next launch, and the commentator would
  // be silenced by a control that says "Picture".
  assert.equal(PICTURE_SOURCE_KEY, 'pictureSource');
  assert.notEqual(PICTURE_SOURCE_KEY, 'returnSource');
  const src = codeOnly(ui('app.js'));
  assert.match(
    src,
    /persistConfig\(\{ \[PICTURE_SOURCE_KEY\]: next \}\)/,
    'the picture choice must be saved under its own key',
  );
});

test('the home screen no longer offers an audio-path control', () => {
  const src = ui('home.js');
  for (const gone of ['RETURN_SOURCES', 'setReturnSource', 'onReturnSourceChange', 'deriveReturnSourceEffects']) {
    assert.ok(!src.includes(gone), `home.js must not still build the old audio control (${gone})`);
  }
  // And the audio controls that REMAIN are untouched: the bus and the channel.
  // The operator needs left-only to hear FX now that CLN carries FX hard-left
  // and comms hard-right, and this work must not have cost them that.
  assert.match(src, /handlers\.onReturnChange\(Number\(returnSelect\.value\)\)/, 'the bus dropdown stays');
  assert.match(src, /handlers\.onReturnChannelChange\(mode\)/, 'and the stereo/left/right selector stays');
  assert.match(src, /'return-channel'/, 'as a segmented control of its own');
});

test('backend.js binds the picture calls in one table, separate from the audio one', () => {
  const js = ui('backend.js');
  const tableStart = js.indexOf('const PICTURE_METHODS');
  assert.ok(tableStart > 0, 'backend.js must name the Go methods in one table');
  const table = js.slice(tableStart, js.indexOf('});', tableStart));

  for (const [jsName, goName] of [
    ['startPicture', 'StartPicture'],
    ['stopPicture', 'StopPicture'],
    ['setPictureRect', 'SetPictureRect'],
    ['setPictureVisible', 'SetPictureVisible'],
    ['getPictureState', 'GetPictureState'],
  ]) {
    assert.match(js, new RegExp(`export async function ${jsName}\\(`), `backend.js exports ${jsName}`);
    assert.ok(table.includes(`'${goName}'`), `PICTURE_METHODS must name Go's ${goName}`);
  }

  // Availability is decided on ALL of them, for the reason srtReturnAvailable
  // is: every one is called on a path that has already assumed the option was
  // offered. A build with StartPicture but no SetPictureRect would start a
  // receiver and paint it at a native default — an opaque box over the whole
  // application, which is worse than no picture at all.
  assert.match(
    js,
    /PICTURE_METHOD_NAMES = Object\.freeze\(Object\.values\(PICTURE_METHODS\)\)/,
    'the name list must be derived from the table, not listed again',
  );
  const fn = js.slice(js.indexOf('export function pictureAvailable()'));
  assert.match(fn.slice(0, fn.indexOf('\n}')), /PICTURE_METHOD_NAMES\.every\(/);
});

test('the fake backend never claims a picture is SHOWING', () => {
  // There is no native decoder and no child window in a browser tab. A fake that
  // reported SHOWING would make home.js hide the mosaic behind an overlay that
  // does not exist — a black rectangle, in the one session where somebody is
  // looking at the layout. BACKOFF is honest and it is also the state a dev
  // session most needs to be able to see, because it is the fallback.
  const js = ui('backend.js');
  const fn = js.slice(js.indexOf('export async function startPicture('));
  const body = codeOnly(fn.slice(0, fn.indexOf('\n}')));
  assert.ok(
    !body.includes('PICTURE_STATE.SHOWING'),
    'the fake must not drive itself to SHOWING',
  );
  assert.ok(body.includes('PICTURE_STATE.BACKOFF'), 'it must land in BACKOFF and say why');
});

test('the "picture" event has its own name, and is not the audio one renamed', () => {
  const js = ui('backend.js');
  assert.match(js, /export const EVENT_PICTURE = 'picture'/);
  assert.ok(
    !/EVENT_PICTURE = 'return'/.test(js),
    'it is not the audio return event under a new name',
  );
});

test('the four picture states are gst.PictureState, spelled the way Go spells them', () => {
  // AND THEY ARE LOWERCASE, where gst.ReturnState is uppercase. The two enums
  // are neighbours and carry nearly the same four words; a copy made from the
  // wrong one compares unequal to every event that arrives, and the status line
  // sits for ever on whatever was last recognised while the picture comes and
  // goes underneath it.
  const go = read(join(here, '..', '..', '..'), 'internal', 'gst', 'picture.go');
  const js = ui('backend.js');

  const states = {
    STOPPED: PICTURE_STATE_STOPPED,
    CONNECTING: PICTURE_STATE_CONNECTING,
    SHOWING: PICTURE_STATE_SHOWING,
    BACKOFF: PICTURE_STATE_BACKOFF,
  };

  for (const [name, value] of Object.entries(states)) {
    const goName = `PictureState${name[0]}${name.slice(1).toLowerCase()}`;
    assert.ok(
      go.includes(`${goName} PictureState = "${value}"`),
      `internal/gst must define ${goName} as "${value}"`,
    );
    assert.ok(js.includes(`${name}: '${value}'`), `backend.js must carry ${name} as '${value}'`);
    assert.equal(value, value.toLowerCase(), 'gst.PictureState is lowercase; do not shout it here');
  }
});

test('an unrecognised state never counts as a picture being on screen', () => {
  // The safe direction. Painting an opaque native window over the page on the
  // strength of a word this build does not recognise replaces a soft picture of
  // the match with a black rectangle.
  for (const junk of ['SHOWING!', 'receiving', 'up', '', null, undefined, 7, {}]) {
    assert.equal(
      derivePictureSourceEffects(PICTURE_SOURCE_SRT, junk).showingSRT,
      false,
      `${JSON.stringify(junk)} must not be read as showing`,
    );
  }
  // …but the four real ones, in any case, are read correctly.
  for (const good of ['showing', 'SHOWING', 'Showing']) {
    assert.equal(derivePictureSourceEffects(PICTURE_SOURCE_SRT, good).showingSRT, true);
  }
});
