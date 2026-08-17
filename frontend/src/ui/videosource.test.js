/**
 * Tests for the VIDEO SOURCE: what this position sends, the camera lamp that
 * says whether the card is seeing anything, and the confidence preview beside
 * it.
 *
 * Run with:  node --test "src/**\/*.test.js"
 *
 * Owner: WP-DECKLINK tier 3, frontend half.
 *
 * ======================= THE FOUR FAILURES THESE PREVENT ====================
 *
 * 1. A CAMERA THAT IS SENDING BLACK, WITH EVERY LAMP GREEN. MEASURED: a DeckLink
 *    that loses its input goes on emitting black frames at full rate for ever —
 *    no error, no EOS, and the muxer never starves. The sender stays CONNECTED,
 *    the switcher reports a healthy correctly-formatted feed, the audio meters
 *    keep moving. deriveCameraLamp is the only thing in this application that
 *    can say otherwise, so its four states and their four appearances are driven
 *    for real here.
 *
 * 2. A SAVE THAT PUTS A LIVE CAMERA BACK ON THE SLATE. collectConfig replaces
 *    the whole stored document, so a form that failed to restate videoSource
 *    would take a position off air because somebody corrected a typo in the
 *    event id — silently, with nothing on screen saying a value was thrown away.
 *
 * 3. A CONFIGURATION THAT CANNOT START, offered as though it could. A DeckLink
 *    capture on a machine with no card is not-negotiated (-4) in about 100
 *    microseconds, naming neither the device nor the cause.
 *
 * 4. A PREVIEW TOGGLE THAT LOOKS BROKEN. It takes effect when it is saved, and
 *    only off air, because splicing the branch into a live pipeline was measured
 *    to take the ON-AIR leg to 0 fps permanently with the pipeline still
 *    reporting PLAYING — and the pipeline it would be spliced into is now
 *    PLAYING from launch to quit, so a rebuild is the only way the toggle is
 *    honoured. A control that appears to do nothing, with no reason beside it,
 *    is a control an operator presses again.
 *
 * ======================= WHY SOME OF THIS READS SOURCE ======================
 *
 * Everything in videosource.js is pure and is driven directly. settings.js,
 * home.js and app.js build against a real DOM and a Wails runtime, and
 * package.json is frozen so there is no jsdom — a shim widened until a test
 * passes stops being evidence. Their half is asserted from their TEXT, exactly
 * as settings.test.js, overlay.test.js and mixerwiring.test.js already do.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  VIDEO_SOURCE_SLATE,
  VIDEO_SOURCE_DECKLINK,
  DEFAULT_VIDEO_SOURCE,
  VIDEO_SOURCE_KEY,
  PREVIEW_KEY,
  LAMP_CAMERA,
  VIDEO_SOURCES,
  normaliseVideoSource,
  normalisePreviewEnabled,
  countDeckLinkDevices,
  deriveVideoSourceEffects,
  describeToAir,
  describeCardAvailability,
  describePreviewBox,
  PICTURE_CAPTURE,
  deriveCameraLamp,
  PREVIEW_AT_START_CAVEAT,
  describeCardOptionRefusal,
  VIDEO_LEG_WHILE_SENDING,
} from './videosource.js';
import { LEVEL } from './lamps.js';
import { deriveSignalLamp, deriveAudioLamp } from './channelmap.js';
import { validateConfig } from './validate.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);
const css = () => read(here, '..', 'styles', 'main.css');

/** codeOnly strips comments, so prose ABOUT a call site is not the call site. */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/** A device list entry as App.ListInputDevices renders one. */
const nativeDevice = { id: 'native-1', name: 'Microphone (Focusrite)', kind: 'native' };
const cardDevice = { id: '2747401380', name: 'Blackmagic UltraStudio 4K Mini', kind: 'decklink' };

// ---------------------------------------------------------------------------
// The two values, and the default
// ---------------------------------------------------------------------------

test('the two sources are spelled the way internal/config spells them', () => {
  // A drift here is the SILENT kind. config.EffectiveVideoSource substitutes
  // "slate" for anything it does not recognise as a value, so a kind that
  // differed only in case would save cleanly, start cleanly, and quietly put a
  // still image on air in place of the camera the operator selected.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /VideoSourceSlate = "slate"/, 'internal/config no longer spells the slate "slate"');
  assert.match(
    go,
    /VideoSourceDeckLink = "decklink"/,
    'internal/config no longer spells the card kind "decklink"',
  );
  assert.equal(VIDEO_SOURCE_SLATE, 'slate');
  assert.equal(VIDEO_SOURCE_DECKLINK, 'decklink');
});

test('the default is the slate on both sides of the boundary', () => {
  // THE ONE DEFAULT IT WOULD BE WORST TO CHANGE. Moving it would put a camera on
  // the switcher for every position that upgraded, on the next launch, without
  // anybody having asked.
  assert.equal(DEFAULT_VIDEO_SOURCE, VIDEO_SOURCE_SLATE);
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /DefaultVideoSource = VideoSourceSlate/, 'the Go default moved off the slate');
});

test('the config keys are the json tags internal/config actually declares', () => {
  // A key mismatch does not fail: encoding/json ignores what it does not
  // recognise, Go keeps its default, and the Settings screen goes on showing
  // what the operator chose while the pipeline is built from something else.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /VideoSource string `json:"videoSource"`/, 'the video source tag moved in Go');
  assert.match(
    go,
    /DeckLinkPreviewEnabled bool `json:"decklinkPreviewEnabled"`/,
    'the preview tag moved in Go — or stopped being a bool',
  );
  assert.equal(VIDEO_SOURCE_KEY, 'videoSource');
  assert.equal(PREVIEW_KEY, 'decklinkPreviewEnabled');
  for (const file of ['settings.js', 'validate.js', 'backend.js', 'app.js']) {
    assert.ok(ui(file).includes('videoSource'), `${file} must use the same key as config.go`);
  }
  for (const file of ['settings.js', 'backend.js', 'app.js']) {
    assert.ok(
      ui(file).includes('decklinkPreviewEnabled'),
      `${file} must use the same key as config.go`,
    );
  }
});

test('normaliseVideoSource falls back to the slate for everything it does not know', () => {
  assert.equal(normaliseVideoSource(VIDEO_SOURCE_DECKLINK), VIDEO_SOURCE_DECKLINK);
  for (const value of [
    'slate',
    '',
    undefined,
    null,
    'DECKLINK', // a case drift, which is exactly what must not become a camera
    'Decklink',
    'blackmagic',
    'camera',
    0,
    true,
    {},
  ]) {
    assert.equal(
      normaliseVideoSource(value),
      VIDEO_SOURCE_SLATE,
      `${JSON.stringify(value)} must read as the slate: nobody's silence may become a live camera`,
    );
  }
});

test('the preview flag is off for everything that is not exactly true', () => {
  assert.equal(normalisePreviewEnabled(true), true);
  for (const value of [false, undefined, null, 0, 1, '', 'true', 'on', {}, []]) {
    assert.equal(
      normalisePreviewEnabled(value),
      false,
      `${JSON.stringify(value)} must read as OFF — the string "true" especially, which is what a ` +
        'checkbox read with .value instead of .checked would produce',
    );
  }
});

// ---------------------------------------------------------------------------
// Is there a card
// ---------------------------------------------------------------------------

test('the card is found by KIND and never by parsing an id', () => {
  assert.equal(countDeckLinkDevices([nativeDevice, cardDevice]), 1);
  assert.equal(countDeckLinkDevices([cardDevice, { ...cardDevice, id: '99' }]), 2);
  assert.equal(countDeckLinkDevices([nativeDevice]), 0);
  // A device with no kind at all — an older Go build — is not a card. Guessing
  // one from the NAME would find "Blackmagic UltraStudio 4K Mini", which is the
  // name of the CoreAudio twin that measures -96 dBFS on all sixteen channels.
  assert.equal(countDeckLinkDevices([{ id: 'x', name: 'Blackmagic UltraStudio 4K Mini' }]), 0);
  for (const notAList of [null, undefined, {}, 'decklink', 3]) {
    assert.equal(countDeckLinkDevices(notAList), 0);
  }

  // And the source of the string. devices.js pins DEVICE_KIND against gst.go;
  // this pins that videosource.js reads the kind rather than the id, which is
  // the mistake gst.go's own comment warns about — a DeckLink card's audio and
  // video entries publish the SAME persistent-id, so an id says less here than
  // it looks like it does.
  const src = codeOnly(ui('videosource.js'));
  assert.match(src, /d\.kind === DEVICE_KIND\.DECKLINK/);
  assert.ok(
    !/\bid\b\s*\.\s*(startsWith|includes|match)/.test(src),
    'videosource.js must not parse a device id; internal/gst documents it as opaque',
  );
});

test('"no card" and "we could not look" are different states', () => {
  // THE ONE THAT MATTERS. A machine whose device listing failed has not been
  // shown to have no card, and a warning saying it has would send an engineer to
  // look for hardware that is sitting in the slot.
  const unread = deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, null);
  assert.equal(unread.cardKnown, false);
  assert.equal(unread.startable, true, 'an unread list is not evidence of absence');
  assert.equal(unread.toAir, VIDEO_SOURCE_DECKLINK);
  assert.match(describeCardAvailability(unread), /could not be listed/);
  assert.match(describeCardAvailability(unread), /unknown/i);

  const empty = deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, []);
  assert.equal(empty.cardKnown, true);
  assert.equal(empty.startable, false, 'an EMPTY list IS evidence of absence');
  assert.equal(empty.toAir, 'nothing');
});

test('the effects say which of three things goes to air', () => {
  const slate = deriveVideoSourceEffects(VIDEO_SOURCE_SLATE, [nativeDevice, cardDevice]);
  assert.equal(slate.wantCard, false);
  assert.equal(slate.startable, true);
  assert.equal(slate.toAir, VIDEO_SOURCE_SLATE);

  const card = deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, [nativeDevice, cardDevice]);
  assert.equal(card.wantCard, true);
  assert.equal(card.cardPresent, true);
  assert.equal(card.cardCount, 1);
  assert.equal(card.toAir, VIDEO_SOURCE_DECKLINK);

  const missing = deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, [nativeDevice]);
  assert.equal(missing.startable, false);
  assert.equal(
    missing.toAir,
    'nothing',
    '"the slate goes to air" and "START will be refused" are different sentences and only the ' +
      'second is something somebody has to act on',
  );

  // An unrecognised saved value must be the slate in every one of these, not a
  // fourth state somebody has to interpret.
  assert.equal(deriveVideoSourceEffects('camera', [cardDevice]).toAir, VIDEO_SOURCE_SLATE);
});

test('describeToAir names the thing on air and never just the option', () => {
  const slate = describeToAir(deriveVideoSourceEffects(VIDEO_SOURCE_SLATE, [cardDevice]));
  assert.match(slate, /SLATE/);
  assert.match(slate, /nothing from a card/i, 'and say what is NOT sent, which is the other half');

  const card = describeToAir(deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, [cardDevice]));
  assert.match(card, /THE CARD/);
  assert.match(card, /slate is\s+not sent/i);
  assert.match(
    card,
    /CAMERA lamp/,
    'the sentence has to point at the one indicator that can tell a live picture from black',
  );

  const missing = describeToAir(deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, []));
  assert.match(missing, /NOTHING/);
  assert.match(missing, /START/, 'and say when it will fail');
  assert.match(missing, /slate|fit the card/i, 'and what to do about it');
});

test('the availability note is silent on a seat that is already right', () => {
  // Never an empty flourish under a control that needs no attention.
  assert.equal(describeCardAvailability(deriveVideoSourceEffects(VIDEO_SOURCE_SLATE, [nativeDevice])), '');
  assert.equal(
    describeCardAvailability(deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, [cardDevice])),
    '',
  );

  // A card fitted and not used gets one sentence, and the plural is right.
  const idle = describeCardAvailability(deriveVideoSourceEffects(VIDEO_SOURCE_SLATE, [cardDevice]));
  assert.match(idle, /A DeckLink card IS fitted/);
  const two = describeCardAvailability(
    deriveVideoSourceEffects(VIDEO_SOURCE_SLATE, [cardDevice, { ...cardDevice, id: '2' }]),
  );
  assert.match(two, /^2 DeckLink cards are fitted/);

  // And the fault case names the fault rather than describing the hardware.
  const missing = describeCardAvailability(deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, []));
  assert.match(missing, /NO DECKLINK CARD WAS FOUND/);
});

// ---------------------------------------------------------------------------
// The camera lamp
// ---------------------------------------------------------------------------

test('the camera lamp reads SLATE on a slate position, and is never absent', () => {
  // A lamp that appeared and disappeared with a setting is a lamp nobody learns
  // to look at, and an absent lamp reads as "this build has not got one" rather
  // than as "the slate is going to air". Grey/SLATE is the exact truth on every
  // position shipping today.
  for (const signal of [undefined, null, { state: 'OK' }, { state: 'LOST' }, { state: 'UNKNOWN' }]) {
    const lamp = deriveCameraLamp(VIDEO_SOURCE_SLATE, signal);
    assert.deepEqual(lamp, { level: LEVEL.GREY, text: 'SLATE' });
  }
  // Including for a saved value nothing recognises, which normalises to slate.
  assert.equal(deriveCameraLamp('camera', { state: 'LOST' }).text, 'SLATE');
});

test('the camera lamp is the signal watchdog, not a second rendering of it', () => {
  // The hysteresis is internal/gst's, with asymmetric hold-offs measured against
  // the real card. Two renderings of one watchdog would be two lamps that can
  // disagree about the same card — this one and the CARD VIDEO lamp on the
  // Settings routing screen, which is drawn from the same function.
  for (const signal of [
    undefined,
    { state: 'UNKNOWN', flaps: 0 },
    { state: 'OK', flaps: 0 },
    { state: 'OK', flaps: 4 },
    { state: 'OK', flaps: 40 },
    { state: 'LOST', flaps: 1 },
    { state: 'something a future build sends' },
  ]) {
    assert.deepEqual(
      deriveCameraLamp(VIDEO_SOURCE_DECKLINK, signal),
      deriveSignalLamp(signal),
      `the card states must be deriveSignalLamp's exactly: ${JSON.stringify(signal)}`,
    );
  }
});

test('the four camera states are the four an operator has to tell apart', () => {
  const card = (signal) => deriveCameraLamp(VIDEO_SOURCE_DECKLINK, signal);
  assert.deepEqual(card(undefined), { level: LEVEL.GREY, text: 'NOT MEASURED' });
  assert.deepEqual(card({ state: 'OK' }), { level: LEVEL.GREEN, text: 'SIGNAL' });
  assert.deepEqual(card({ state: 'OK', flaps: 4 }), { level: LEVEL.AMBER, text: 'UNSTABLE (4)' });
  assert.deepEqual(card({ state: 'LOST' }), { level: LEVEL.RED, text: 'NO SIGNAL' });
  // UNKNOWN IS NOT A FAULT. It is the state of every machine before the first
  // measurement, and of every machine with no card in it.
  assert.equal(card({ state: 'UNKNOWN' }).level, LEVEL.GREY);
});

test('NO CAMERA reads differently from NO AUDIO, in four ways at once', () => {
  // They sit next to each other on one row and they send whoever reads them to
  // different places with different urgency: no video signal is a cable, a
  // router crosspoint or a source that is off, fixed in the machine room, and
  // black is going to air until it is; no audio is an embedder, a mute or a
  // microphone, fixed at the position, now, by the person reading the lamp.
  const noCamera = deriveCameraLamp(VIDEO_SOURCE_DECKLINK, { state: 'LOST' });
  const noAudio = deriveAudioLamp({ peak: [-100, -100], rms: [-100, -100] }, 2);

  assert.notEqual(noCamera.text, noAudio.text, 'the two states must not read the same');
  assert.notEqual(
    noCamera.level,
    noAudio.level,
    'and must not be the same colour: a red cross against an amber triangle is a different GLYPH ' +
      'as well, which is what survives a colour-blind reader and a washed-out projector',
  );
  assert.equal(noCamera.level, LEVEL.RED, 'no camera is a fault that is on air now');
  assert.equal(
    noAudio.level,
    LEVEL.AMBER,
    'silence on every channel is also what a rehearsal gap and the end of a session look like; ' +
      'a red lamp that goes green on its own teaches the desk to ignore red',
  );
  // And the NAMES differ, which is the fourth signal.
  assert.equal(LAMP_CAMERA, 'CAMERA');
  assert.notEqual(LAMP_CAMERA, 'AUDIO');
});

// ---------------------------------------------------------------------------
// The preview, and its caveat
// ---------------------------------------------------------------------------

test('the preview caveat is the two facts an operator acts on, and nothing else', () => {
  // IT WAS 432 CHARACTERS, the second-largest block of prose on the Settings
  // screen, and it argued its own case at length: that switching the preview
  // during a live feed was MEASURED to stop the picture going to air
  // permanently, with everything still reporting success, and that the
  // application therefore refuses to do it.
  //
  // Every word of that is true and none of it is the operator's business at the
  // moment they are ticking a box. Two things are: WHEN it takes effect, because
  // a control that appears to do nothing is a control they press again, and that
  // it does not touch the transmitted feed, because that is the fear a preview
  // beside an on-air path creates.
  //
  // WHEN IT TAKES EFFECT HAS CHANGED, AND THIS ASSERTION WITH IT. It used to
  // require the word START, because the preview branch was built when a session
  // started and there was no earlier moment at which the tick box could mean
  // anything. The picture capture is built at launch now and rebuilt when this
  // field is saved, so START is not what applies it and an assertion demanding
  // that word would pin a sentence that is no longer true. What the operator
  // still needs is the same two facts, and the first of them is now the SAVE.
  assert.match(PREVIEW_AT_START_CAVEAT, /save/i, 'it must name the moment it takes effect');
  assert.ok(
    !/\bSTART\b/.test(PREVIEW_AT_START_CAVEAT),
    'and must not send the operator to a button that no longer applies it: the preview appears ' +
      'when capture is rebuilt, which is before anything is sent',
  );
  assert.match(
    PREVIEW_AT_START_CAVEAT,
    /changes nothing that is transmitted/i,
    'the reassurance that the preview itself is not on the feed',
  );
  assert.ok(
    PREVIEW_AT_START_CAVEAT.length < 100,
    `the caveat is ${PREVIEW_AT_START_CAVEAT.length} characters and is rendered permanently under ` +
      'a tick box; the measurement that justifies it belongs in videosource.js',
  );

  // AND THE MEASUREMENT IS STILL WRITTEN DOWN, in the source, where the next
  // person tempted to make the preview switch live will meet it. Deleting the
  // knowledge is a different act from not rendering it, and this is the guard
  // against the two being confused.
  const src = ui('videosource.js');
  assert.match(src, /50 fps to 0/, 'the measured consequence must survive as a comment');
  assert.match(src, /PERMANENTLY/, 'including that it does not recover');

  // The SOURCE control's own version of the caveat is gone entirely. It was a
  // second permanent paragraph saying the same thing about the control above,
  // and the fact is now carried where it can be acted on — on the control, as
  // the reason it is disabled, while a feed is actually up.
  assert.equal(
    /VIDEO_SOURCE_AT_START_CAVEAT/.test(src.replace(/\/\/[^\n]*/g, '')),
    false,
    'the source caveat is back as a rendered string; VIDEO_LEG_WHILE_SENDING is where that fact ' +
      'belongs, because off air there is nothing to caveat',
  );
});

test('the card option refuses in one line, and only when it is actually refused', () => {
  // The owner's report was "The declink input is grayed out in settings?" on a
  // machine with a working card. Two separate things had to be true of the
  // rendering afterwards: the refusal must be readable at a glance rather than
  // being a paragraph, and it must not be attached to an option that is NOT
  // refused — a configuration naming the card on a machine with none leaves the
  // option enabled on purpose, so that the screen and config.json cannot
  // disagree, and a tooltip claiming no card was found would contradict it.
  const line = describeCardOptionRefusal();
  assert.match(line, /No DeckLink card was found/);
  assert.ok(line.length < 80, 'a tooltip is read at a glance or not at all');

  const js = codeOnly(ui('settings.js'));
  assert.match(
    js,
    /cardOption\.title = cardOption\.disabled \? describeCardOptionRefusal\(\) : ''/,
    'the reason must be keyed on the option being DISABLED, not on the card being absent',
  );
});

test('the while-sending reason agrees with the refusal Go would make', () => {
  // A screen that offered these controls and let the operator find the refusal
  // by pressing would be worse than one that disables them; a screen that
  // disabled them but let a Save carry the change anyway would be one making a
  // promise it does not keep. The wording has to match the Go refusals, because
  // an operator can meet both about the same press.
  assert.match(VIDEO_LEG_WHILE_SENDING, /Disabled while SENDING/);
  assert.match(VIDEO_LEG_WHILE_SENDING, /STOP, change it, then START/);

  const go = read(repoRoot, 'app.go');
  assert.match(
    go,
    /errVideoSourceWhileSending = errors\.New\(/,
    'app.go no longer refuses a video-source change while sending; this screen claims it does',
  );
  assert.match(go, /errPreviewChangeWhileSending = errors\.New\(/);
  for (const name of ['errVideoSourceWhileSending', 'errPreviewChangeWhileSending']) {
    const at = go.indexOf(`${name} = errors.New(`);
    assert.ok(
      go.slice(at, at + 700).includes('Press STOP, change it, then START'),
      `${name} must still tell the operator the same thing this screen does`,
    );
  }
});

test('the video leg is gated on the sending state and on being a local seat', () => {
  const js = codeOnly(ui('settings.js'));
  const render = js.slice(js.indexOf('function renderVideoSource()'));
  const body = render.slice(0, render.indexOf('\n  }'));

  assert.match(body, /fields\.videoSource\.input\.disabled = remote \|\| sendingNow/);
  assert.match(
    body,
    /fields\.decklinkPreviewEnabled\.input\.disabled = remote \|\| sendingNow \|\| !previewSupported/,
  );
  assert.match(body, /VIDEO_LEG_WHILE_SENDING/, 'the reason must be ON the control, not in a log');

  // And the gate is redrawn from the same place the preset gate is, so the two
  // cannot disagree about whether a session is up.
  const setSending = js.slice(js.indexOf('function setSending(sending)'));
  assert.match(setSending.slice(0, setSending.indexOf('\n  }')), /renderVideoSource\(\)/);
  assert.match(
    codeOnly(ui('app.js')),
    /settings\.setSending\(/,
    'app.js must drive it, from the same derivation the SENDING lamp uses',
  );
});

test('a remote Settings screen keeps its video-leg boxes fresh, or its next save is refused', () => {
  // App.SaveConfig REFUSES a remote save whose videoSource, decklinkPreviewEnabled
  // or COMMENTARY DEVICE differ from the live ones — the enforcement that makes
  // the host-only setters mean anything. The Settings form is a page cache
  // refreshed only by open(), so without this a remote seat that had the screen
  // open across a host-side change would have its next save of ANYTHING refused,
  // naming a field it is not allowed to change.
  //
  // The guard was renamed from refuseRemoteVideoLegChange when the capture layer
  // became always-live: a save now RE-POINTS a running capture, so the three
  // commentary-device fields joined the two video-leg ones. A remote whole-document
  // save could otherwise take the desk's microphone — and on a card seat the
  // exclusive DeckLink — away from the person sitting in front of it.
  const go = read(repoRoot, 'app.go');
  assert.match(go, /func \(a \*App\) refuseRemoteCaptureChange\(c \*config\.Config\) error/);
  assert.match(go, /videoSource is %q here and this save would make it %q/);
  assert.match(go, /audioSourceKind is %q here and this save would make it %q/);
  assert.match(go, /audioDeviceId names the commentary microphone/);
  assert.match(go, /decklinkPersistentId names the capture card/);

  const js = codeOnly(ui('settings.js'));
  assert.match(js, /function adoptVideoLeg\(config\) \{/);
  const adopt = js.slice(js.indexOf('function adoptVideoLeg(config)'));
  assert.ok(
    !/lastLoadedConfig =/.test(adopt.slice(0, adopt.indexOf('\n  }'))),
    'adoptVideoLeg must not move the preset diff BASELINE: that is what was POPULATED',
  );

  const app = codeOnly(ui('app.js'));
  const at = app.indexOf('backend.onConfig((payload)');
  assert.ok(at > 0, 'app.js must still listen for another seat’s save');
  const handler = app.slice(at, app.indexOf('});', at));
  assert.match(handler, /settings\.adoptVideoLeg\(payload\.config\)/);
  assert.ok(
    !/settings\.open\(\)|populate\(/.test(handler),
    'and must NOT redraw the whole form under somebody mid-edit — the reason this handler is not ' +
      'onConfigSaved in the first place',
  );
});

test('both host-only setters are declared in the remote allowlist', () => {
  // CONTRACT.md: every bound method needs a remoteAllowlist entry. What matters
  // for these three is not merely that they HAVE one but that it is host-only —
  // what a switcher receives, and an opaque window on the operator's own screen,
  // are not a remote seat's to decide.
  const allowlist = read(repoRoot, 'app_remote.go');
  for (const method of [
    'SetVideoSource',
    'SetDeckLinkPreviewEnabled',
    'SetPreviewRect',
    'SetPreviewVisible',
  ]) {
    assert.match(
      allowlist,
      new RegExp(`"${method}":\\s*\\{hostOnly: true\\}`),
      `${method} must be host-only in remoteAllowlist`,
    );
  }
});

test('the preview box explains itself from the CAPTURE state, never from a button', () => {
  // ===================== REWRITTEN, AND THE REWRITE IS THE CHANGE ============
  //
  // This test used to assert that the caption said "press START" off air and
  // "STOP and START to see it" during a session, and both were right while the
  // preview was a branch of the session's own pipeline: pressing START was
  // literally what filled the box in.
  //
  // The picture capture is built at launch and held until the application quits
  // now, so the box fills in before anything is sent and stays filled after
  // STOP. Left as it was, this test would have pinned an instruction to press a
  // button that changes nothing about this box — beside a black rectangle whose
  // real reason is one of the four below.
  //
  // What has NOT changed is the state the page cannot know: capture being live
  // does not prove a preview BRANCH was built, because the build retries without
  // one when the surface will not attach and no event reports the branch itself.
  // That is the case the last assertion covers.
  assert.match(describePreviewBox(PICTURE_CAPTURE.OPENING), /opening/i);
  assert.match(describePreviewBox(PICTURE_CAPTURE.FAILED), /did not open/i);
  assert.match(
    describePreviewBox(PICTURE_CAPTURE.FAILED),
    /alerts/i,
    'the reason is Go\'s own sentence and it is long: it goes to the column, and this box says ' +
      'where to look. A 16:9 box 120 px tall cannot hold it',
  );
  assert.match(
    describePreviewBox(PICTURE_CAPTURE.LIVE),
    /no picture is being painted/i,
    'capture live with the caption still visible is the one state the page cannot explain from an ' +
      'event, so it says so rather than showing an unexplained black box',
  );
  assert.match(describePreviewBox(PICTURE_CAPTURE.OFF), /not being captured/i);

  for (const state of [...Object.values(PICTURE_CAPTURE), undefined, null, 'nonsense']) {
    assert.match(describePreviewBox(state), /^PREVIEW/, 'and say what the black box IS');
    assert.ok(
      !/\bSTART\b/.test(describePreviewBox(state)),
      `"${describePreviewBox(state)}" sends the operator to START, which no longer fills this box`,
    );
  }
});

test('the four capture states are one vocabulary, spelled the same in three places', () => {
  // videosource.js keeps its own copy so that it stays pure — no backend, no
  // DOM, so node --test drives every rule in it with nothing installed — which
  // is the same arrangement channelmap.js's SIGNAL_STATE has and carries the
  // same risk: a private copy of a vocabulary can drift into matching no event
  // that ever arrives, and the failure is silent. A caption stuck on "the card
  // is not being captured" over a live picture is what that looks like.
  const backend = ui('backend.js');
  for (const [name, value] of Object.entries(PICTURE_CAPTURE)) {
    assert.match(
      backend,
      new RegExp(`${name}: '${value}'`),
      `backend.js's CAPTURE_STATE must spell ${name} as "${value}"`,
    );
  }

  // THE THIRD PLACE IS app.go, WHICH MINTS THEM, and it is deliberately not
  // asserted here yet: the bindings have not landed, so the assertion would fail
  // this gate today and a conditional one would pass vacuously for ever, which is
  // the worse of the two. It belongs beside the rest of the seam's Go-side drift
  // guards in capture.test.js — the pattern is meters.test.js's
  // `assert.match(appGo, /EventLevels = "levels"/)` — and it is owed the moment
  // EventCapture exists.
});

// ---------------------------------------------------------------------------
// The Settings form's half
// ---------------------------------------------------------------------------

test('the video source is loaded and restated, or a Save takes a camera off air', () => {
  // THE DATA-LOSS GUARD FOR THE FIELD THAT DECIDES WHAT THE SWITCHER RECEIVES.
  // collectConfig replaces the whole stored document, so a form that does not
  // restate this puts a live position back on the slate because somebody
  // corrected a typo in the event id — silently, with every lamp green.
  // settings.test.js's reflection over config.go covers it generically; this
  // says what is lost when it is the one that goes.
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.ok(collect.length > 0 && populate.length > 0);

  assert.match(
    collect,
    /videoSource: normaliseVideoSource\(fields\.videoSource\.input\.value\)/,
    'collectConfig must restate the video source, normalised',
  );
  assert.match(
    populate,
    /fields\.videoSource\.input\.value = normaliseVideoSource\(config\.videoSource\)/,
    'and populate must normalise on the way IN too — an unrecognised value assigned to a <select> ' +
      'selects nothing, and the next Save would write that nothing back',
  );

  // The tick box is read with .checked and never .value: a checkbox's .value is
  // the string "on" whether or not it is ticked, and Go's field is a bool.
  assert.match(
    collect,
    /decklinkPreviewEnabled: fields\.decklinkPreviewEnabled\.input\.checked === true/,
  );
  assert.ok(
    !/decklinkPreviewEnabled\.input\.value/.test(js),
    'the preview flag must never be read through .value',
  );
});

test('the video source is a <select> of exactly the two sources, first in its group', () => {
  const js = ui('settings.js');
  assert.match(js, /selectInput\(\s*'f-videoSource',/, 'it must be a <select>, not free text');
  assert.match(js, /addField\(\s*'videoSource',/, 'and a real field, not a carried value');

  // FIRST IN THE GROUP, above the bitrate and the format, because those two
  // describe whatever this one chooses.
  const heading = js.indexOf("videoHeading.textContent = 'Contribution video'");
  const source = js.indexOf("addField(\n    'videoSource',");
  const bitrate = js.indexOf("addField(\n    'videoBitrateKbps',");
  const format = js.indexOf("addField('videoFormatOverride',");
  assert.ok(heading > 0 && source > 0 && bitrate > 0 && format > 0);
  assert.ok(source > heading, 'the source belongs in the Contribution video group');
  assert.ok(source < bitrate && source < format, 'and above the two settings that describe it');

  // The options come from the shared table rather than being written out here,
  // so a third option cannot exist on the screen and nowhere else.
  assert.match(js, /VIDEO_SOURCES\.map\(\(s\) => \(\{ value: s\.value, label: s\.label \}\)\)/);
  assert.equal(VIDEO_SOURCES.length, 2);
  assert.deepEqual(
    VIDEO_SOURCES.map((s) => s.value),
    [VIDEO_SOURCE_SLATE, VIDEO_SOURCE_DECKLINK],
    'exactly the two sources, slate first — the default is the one an operator lands on',
  );

  // AND THE CONTROL CARRIES NO HINT AT ALL. It used to render both options'
  // summary and cost plus the at-START caveat: 933 characters, the largest block
  // of prose on the screen and the one the owner named. The <select> now gets
  // three arguments, not four, and describeToAir below it is what answers the
  // operator's actual question.
  const call = js.slice(source, js.indexOf('videoToAirLine', source));
  assert.equal(
    /s\.summary|s\.cost|AT_START_CAVEAT/.test(call),
    false,
    'the video-source hint is back; the measurements it carried belong in videosource.js',
  );
  for (const field of ['summary', 'cost']) {
    assert.equal(
      Object.prototype.hasOwnProperty.call(VIDEO_SOURCES[0], field),
      false,
      `VIDEO_SOURCES still carries ${field}: data with no consumer is the rendered paragraph ` +
        'waiting to come back, and the figures are written out in videosource.js instead',
    );
  }
  // The figures themselves must not have been lost with the fields. The card
  // being the CHEAPER leg is the counter-intuitive fact an engineer needs.
  const src = ui('videosource.js');
  assert.match(src, /18\.5-23\.9 %/, 'the slate cost must survive as a comment');
  assert.match(src, /9\.3-14\.6 %/, 'and the card cost, which is the lower of the two');
});

test('the form says which one is going to air, and marks the one that cannot start', () => {
  const js = codeOnly(ui('settings.js'));
  const render = js.slice(js.indexOf('function renderVideoSource()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.ok(body.length > 0, 'settings.js must define renderVideoSource');

  assert.match(body, /videoToAirLine\.textContent = describeToAir\(effects\)/);
  assert.match(body, /videoCardNote\.textContent = availability/);
  assert.match(
    body,
    /classList\.toggle\('video-to-air--unstartable', !effects\.startable\)/,
    'the unstartable case must be MARKED as well as worded — a sentence two screenfuls down a ' +
      'form is not findable, and a colour alone is not readable',
  );

  // Called from populate as well as from the control, because assigning a
  // <select>'s value from script fires neither 'input' nor 'change'.
  assert.match(
    js,
    /fields\.videoSource\.input\.addEventListener\('change', renderVideoSource\)/,
    'the control must redraw its own lines',
  );
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(
    populate,
    /renderVideoSource\(\)/,
    'and populate must too, or the screen opens describing the previous configuration',
  );

  // And main.css has to be able to draw both marks, or the class is a no-op.
  const sheet = css();
  for (const selector of ['.video-to-air', '.video-to-air--unstartable', '.video-card-note--missing']) {
    assert.ok(sheet.includes(selector), `main.css must style ${selector}`);
  }
});

test('the card option is withdrawn when there is no card — but never hidden from a config that names it', () => {
  // Offering an option that fails in a ten-thousandth of a second, naming
  // neither the device nor the cause, is the failure this control exists to
  // avoid. REMOVING it would be the opposite failure: a configuration that
  // already says "decklink" would show the slate selected while config.json said
  // otherwise, which is the screen and the file disagreeing.
  const js = codeOnly(ui('settings.js'));
  const render = js.slice(js.indexOf('function renderVideoSource()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.match(body, /cardOption\.disabled = unavailable && !effects\.wantCard/);
  assert.ok(!/removeChild|cardOption\.remove\(\)/.test(body), 'the option must be disabled, not removed');

  // And the device list is asked for, once, on open — with its own failure
  // swallowed to "not known" rather than to "no card".
  //
  // ONE LISTING, TWO CONTROLS. It was `videoDevices`, read only here; the
  // commentary-input picker is a list of the same devices, and asking twice
  // would be two chances for the two controls to disagree about whether a card
  // is fitted — on a screen where one of them greys out an option over it.
  assert.match(js, /async function refreshInputDevices\(\)/);
  assert.match(js, /inputDevices = Array\.isArray\(devices\) \? devices : null/);
  assert.match(js, /inputDevices = null/, 'a failed listing must read as unknown, never as empty');
  const refresh = js.slice(js.indexOf('async function refreshInputDevices()'));
  const body2 = refresh.slice(0, refresh.indexOf('\n  }'));
  assert.match(body2, /renderVideoSource\(\)/);
  assert.match(body2, /renderAudioInput\(\)/, 'the picker must be redrawn from the same answer');
  const open = js.slice(js.indexOf('async function open()'));
  assert.match(open, /await refreshInputDevices\(\)/, 'open() must ask');

  // THE GREYING IS ON POSITIVE EVIDENCE ONLY, which is the half of the owner's
  // "The declink input is grayed out in settings?" that this file can hold. A
  // listing that FAILED leaves cardKnown false and the option enabled — see
  // deriveVideoSourceEffects — so no amount of enumeration trouble can withdraw
  // the option, and a greyed option is therefore always a real empty answer.
  assert.equal(
    deriveVideoSourceEffects(VIDEO_SOURCE_DECKLINK, null).cardKnown,
    false,
    'a failed listing must never be evidence that no card is fitted',
  );
});

test('the preview toggle is offered only where there is something to preview', () => {
  const js = codeOnly(ui('settings.js'));
  assert.match(js, /checkboxInput\('f-decklinkPreviewEnabled'\)/);
  assert.match(js, /addField\(\s*'decklinkPreviewEnabled',/);
  assert.match(js, /PREVIEW_AT_START_CAVEAT,/, 'the caveat must be the field HINT, not a tooltip');

  const render = js.slice(js.indexOf('function renderVideoSource()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.match(
    body,
    /fields\.decklinkPreviewEnabled\.wrap\.hidden = !effects\.wantCard/,
    'a confidence monitor of a still PNG is the still PNG',
  );
  assert.match(
    body,
    /previewSendingNote\.hidden = !sendingNow/,
    'and the "disabled while sending" line belongs to a running feed only',
  );
  // The build with no bindings says so on the control rather than reserving a
  // rectangle nothing will paint. Same all-or-nothing rule as the presets, the
  // picture and the channel map.
  assert.match(js, /const previewSupported = backend\.usingFakeBackend \|\| backend\.previewAvailable\(\)/);
  assert.match(
    body,
    /fields\.decklinkPreviewEnabled\.input\.disabled = remote \|\| sendingNow \|\| !previewSupported/,
    'the build-capability gate is one of the three the tick box carries, in one expression',
  );

  // setSending drives the note from the same state the preset gate uses, so the
  // two cannot disagree about whether a session is up.
  const setSending = js.slice(js.indexOf('function setSending(sending)'));
  assert.match(setSending.slice(0, setSending.indexOf('\n  }')), /renderVideoSource\(\)/);
});

test('a remote seat is told it cannot change what goes to air', () => {
  // The Go side keeps both setters host-only AND refuses a remote SaveConfig
  // that changes either field. This is the visible half: a remote operator is
  // told rather than left to make an edit whose whole save is then refused. The
  // value is still COLLECTED — a disabled control keeps its value, and this form
  // replaces the whole document.
  const js = codeOnly(ui('settings.js'));
  const render = js.slice(js.indexOf('function renderVideoSource()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.match(body, /const remote = backend\.isRemoteClient\(\)/);
  assert.match(body, /remoteReason/, 'and the reason must be ON the control');
  // Both controls, from the one expression each, so the pair cannot end up in a
  // state where one is offered to a remote seat and the other is not.
  assert.match(body, /fields\.videoSource\.input\.disabled = remote \|\|/);
  assert.match(body, /fields\.decklinkPreviewEnabled\.input\.disabled = remote \|\|/);

  // AND THE MICROPHONE PICKER, which is the third control the Go refusal
  // guards and the one it was silently missing.
  //
  // refuseRemoteCaptureChange covers audioSourceKind, audioDeviceId and
  // decklinkPersistentId as well as the two video fields — without it a remote
  // whole-document SaveConfig could take the desk's microphone, and on a card
  // seat close and reopen the exclusive DeckLink, from another building. The one
  // control that writes all three is this dropdown, and it used to be left
  // enabled: a remote producer who touched it had EVERY subsequent save refused,
  // naming a field they were never told they may not change.
  assert.match(body, /audioInputSelect\.disabled = remote/);
  assert.match(body, /audioInputSelect\.title = remote/, 'and the reason must be ON the control');
  assert.equal(
    /audioInputSelect\.disabled = remote \|\| sendingNow/.test(body),
    false,
    'the picker must NOT be disabled while sending: SelectCommentaryInput refuses with a ' +
      'sentence of its own that names the reason, and that refusal reaches the operator',
  );
});

test('a microphone another seat chose is adopted, so a stale cache cannot refuse a save', () => {
  // adoptVideoLeg's twin, and it exists for the identical reason. This form is a
  // page cache refreshed only by open(); App.SaveConfig refuses a remote save
  // whose audioSourceKind, audioDeviceId or decklinkPersistentId differ from the
  // live ones. A remote seat that had Settings open when the desk changed
  // microphone would otherwise carry the OLD values in a disabled control, and
  // its next save of anything at all — a port, a status key — would be refused,
  // naming a field that seat cannot see a way to correct.
  const js = codeOnly(ui('settings.js'));

  const adopt = js.slice(js.indexOf('function adoptAudioLeg(config)'));
  const body = adopt.slice(0, adopt.indexOf('\n  }'));
  assert.ok(body.length > 0, 'settings.js must expose adoptAudioLeg');
  for (const field of ['audioSourceKind', 'audioDeviceId', 'decklinkPersistentId']) {
    assert.ok(
      body.includes(`fields.${field}.input.value =`),
      `adoptAudioLeg must write ${field}: it is one of the three refuseRemoteCaptureChange guards`,
    );
  }
  // The picker's value is DERIVED from the three hidden fields, so the <select>
  // is rebuilt from them rather than written here — renderAudioInput is the one
  // function that knows the encoding.
  assert.match(body, /renderAudioInput\(\)/);
  // And the routing panel's gate re-runs, because it compares the SELECTED
  // device's key against the one the negotiated width was stamped with: a device
  // that moved under this seat leaves a grid offering crosspoints over a pad
  // that is not there.
  assert.match(body, /renderChannelMapGroup\(\)/);

  // It is NOT populate(): re-drawing the whole form under somebody mid-edit is
  // exactly what app.js's onConfig handler exists to avoid.
  assert.equal(/\bpopulate\(/.test(body), false, 'adoptAudioLeg must not redraw the whole form');

  // And app.js calls it from the same place it calls the video twin.
  assert.match(ui('app.js'), /settings\.adoptAudioLeg\(payload\.config\)/);
});

test('validateConfig accepts both sources, an absent one, and nothing else', () => {
  const base = {
    m2lxHost: 'm2lx.example.com',
    alias: 'a',
    eventId: 'e',
    srtPort: 40001,
    srtLatencyMs: 120,
    pbkeylen: 0,
    videoBitrateKbps: 2000,
    videoFormatOverride: '',
    statusKey: '',
    audioDeviceId: '',
    audioSourceKind: 'native',
    decklinkPersistentId: '',
    headphoneDeviceId: '',
    headphoneEndpointId: '',
    returnMid: 2,
    returnChannel: 'stereo',
    returnSource: 'webrtc',
    srtReturnPort: 40501,
    srtReturnPBKeyLen: 0,
    pictureLatencyMs: 120,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
    slatePath: 'slate.png',
  };
  assert.deepEqual(validateConfig({ ...base, videoSource: 'slate' }), {});
  assert.deepEqual(validateConfig({ ...base, videoSource: 'decklink' }), {});
  // ABSENT IS VALID, and that is the upgrade path: every config.json written
  // before this field existed has no videoSource, and refusing it would make
  // Settings unsavable on the first launch after an upgrade over a field the
  // operator has never seen.
  assert.deepEqual(validateConfig(base), {});

  // null is ACCEPTED alongside undefined, and that is Go's behaviour rather than
  // a gap: encoding/json leaves a string field untouched for a JSON null, so a
  // hand-edited `"videoSource": null` keeps whatever the struct already held —
  // the slate. Refusing it here would be this form disagreeing with the file it
  // is showing. audioSourceKind's check reads the same way, for the same reason.
  assert.equal(validateConfig({ ...base, videoSource: null }).videoSource, undefined);

  for (const bad of ['camera', 'DECKLINK', '', 'blackmagic', 2]) {
    const { videoSource } = validateConfig({ ...base, videoSource: bad });
    assert.ok(videoSource, `${JSON.stringify(bad)} must be refused`);
  }

  // AND THE ABSENCE OF A CARD IS NOT A VALIDATION FAILURE. An engineer must be
  // able to configure a position before the hardware is patched into it — the
  // standing rule at the top of validate.js — so the absence is a warning on the
  // control and a refusal at START, never a form that cannot be saved.
  assert.equal(validateConfig({ ...base, videoSource: 'decklink' }).videoSource, undefined);
});

// ---------------------------------------------------------------------------
// The main screen's half
// ---------------------------------------------------------------------------

test('the CAMERA lamp is on the main screen, beside SENDING and before the switcher lamps', () => {
  const home = codeOnly(ui('home.js'));
  const names = home.match(/const LAMP_NAMES = \[([^\]]*)\]/);
  assert.ok(names, 'home.js must still declare LAMP_NAMES');
  const list = names[1]
    .split(',')
    .map((s) => s.trim().replace(/^'|'$/g, ''))
    .filter(Boolean);
  assert.deepEqual(list, [
    'SENDING',
    'LAMP_CAMERA',
    'SWITCHER SEES FEED',
    'SWITCHER VIDEO',
    'SWITCHER AUDIO',
    'MONITOR',
  ]);

  // home.js holds no derivation and no backend knowledge — app.js is the wire,
  // as it is for every other lamp on the row.
  assert.ok(
    !/deriveCameraLamp/.test(home),
    'home.js must not derive the lamp; it builds DOM and exposes setters',
  );
  const app = codeOnly(ui('app.js'));
  assert.match(
    app,
    /home\.lamps\[LAMP_CAMERA\]\.update\(deriveCameraLamp\(currentVideoSource, currentSignal\)\)/,
    'app.js must paint the lamp from the video source AND the watchdog — neither alone is an answer',
  );
  assert.match(app, /backend\.onSignal\(\(payload\) => \{/, 'and subscribe to the watchdog');
});

test('the two switcher lamps say SWITCHER, because this desk now measures itself', () => {
  // ===================== WHY THE RENAME IS NOT COSMETIC ======================
  //
  // VIDEO and AUDIO were unambiguous while nothing on this screen measured this
  // seat's own video or audio outside a session. Both do now: the input meters
  // are the commentary capture's and the CAMERA lamp is the card's, and both are
  // live from launch.
  //
  // So a commentator would watch their own meter MOVING beside a lamp reading
  // "AUDIO — NO STATUS" — a true statement about a quiet telemetry socket, read
  // by the person in front of it as "this application says my microphone is
  // dead". They would hunt a fault at the desk, before kick-off, in a rig that
  // is working.
  const home = codeOnly(ui('home.js'));
  const app = codeOnly(ui('app.js'));

  assert.match(app, /home\.lamps\['SWITCHER VIDEO'\]\.update\(video\)/);
  assert.match(app, /home\.lamps\['SWITCHER AUDIO'\]\.update\(audio\)/);
  // The old spellings must be gone from BOTH sides, not merely unused on one:
  // createLampRow keys its lamps by name, so a surviving `home.lamps.AUDIO`
  // would be an update() on undefined, and a surviving name in LAMP_NAMES would
  // draw a seventh pill nothing ever paints.
  for (const stale of [/home\.lamps\.VIDEO\b/, /home\.lamps\.AUDIO\b/]) {
    assert.equal(stale.test(app), false, `app.js still paints ${stale}`);
  }
  const names = home.match(/const LAMP_NAMES = \[([^\]]*)\]/)[1];
  assert.equal(/'VIDEO'/.test(names), false, 'LAMP_NAMES still holds the bare VIDEO');
  assert.equal(/'AUDIO'/.test(names), false, 'LAMP_NAMES still holds the bare AUDIO');

  // And the reason is written where the next person to shorten them will read
  // it, because "SWITCHER VIDEO" is longer than it needs to be for any reason
  // except this one.
  assert.match(
    ui('home.js'),
    /moving input meter|MOVING INPUT METER/,
    'home.js must record why the lamps carry the word SWITCHER',
  );
});

test('the frontend adds no second hysteresis to the watchdog', () => {
  // The debounce is internal/gst's, with asymmetric hold-offs measured against
  // the real card. Two filters in series lag a real loss by however long both
  // take to agree, and nobody could reason about the result.
  const app = codeOnly(ui('app.js'));
  const at = app.indexOf('backend.onSignal(');
  assert.ok(at > 0);
  const handler = app.slice(at, app.indexOf('});', at));
  assert.ok(
    !/setTimeout|setInterval|Date\.now/.test(handler),
    'the signal handler must record and render, nothing else',
  );
  assert.match(handler, /currentSignal = payload/);
});

test('the preview is a second native surface, positioned the way the picture already is', () => {
  // NOT A SECOND MECHANISM. overlay.js owns the rectangle, the CSS-pixels-plus-
  // ratio call and the SET of blocking reasons; this is a second createOverlay
  // over a second reserved box, which is what "follow the picture's rect
  // plumbing" means.
  const app = codeOnly(ui('app.js'));
  assert.match(app, /const previewOverlay = createOverlay\(\{/);
  assert.match(app, /measure: \(\) => home\.measurePreviewRect\(\)/);
  assert.match(app, /backend\.setPreviewRect\(css, dpr\)/);
  assert.match(app, /backend\.setPreviewVisible\(on\)/);

  // TWO BOXES, NEVER ONE. Two opaque native windows told to occupy overlapping
  // rectangles erase one another, and neither side reports anything wrong.
  const home = codeOnly(ui('home.js'));
  // The preview box lives in .stage-side beside the picture, with the input
  // meters, which is where the operator put both back: "the meters should still
  // be next to the preview and not in the settings sidebar".
  //
  // What this assertion is FOR is unchanged and is the second half of the line:
  // the preview box is never a DESCENDANT of the tile. Two opaque native
  // windows told to occupy overlapping rectangles erase one another and neither
  // side reports anything wrong, so the box the preview overlay is measured
  // from must sit outside the box the picture overlay is measured from.
  assert.match(home, /pgmStage\.append\(pgmTile, previewTile, metersEl\)/);
  assert.match(home, /pgmStage\.append\(pgmTile, previewTile, metersEl\)/);
  assert.ok(
    !/pgmTile\.appendChild\(previewTile\)/.test(home),
    'the preview box must never be inside the box the picture overlay is measured from',
  );

  // And home.js still has no opinion about device pixels — overlay.js owns the
  // one conversion in this application, for both surfaces.
  assert.ok(!home.includes('devicePixelRatio'));
});

test('both native surfaces are hidden for Settings and for the mixer drawer', () => {
  // The whole hazard, twice: an opaque child window outside the page's stacking
  // context, with a configuration form or a live routing matrix underneath it.
  const app = codeOnly(ui('app.js'));
  const show = app.slice(app.indexOf('function showSettings()'), app.indexOf('function showHome()'));
  assert.ok(show.includes('previewOverlay.block(BLOCK_SETTINGS)'));
  assert.ok(show.includes('previewOverlay.block(BLOCK_HIDDEN)'));

  const home = app.slice(app.indexOf('function showHome()'), app.indexOf('function toggleMixer()'));
  assert.ok(home.includes('previewOverlay.unblock(BLOCK_SETTINGS)'));
  assert.ok(home.includes('previewOverlay.unblock(BLOCK_HIDDEN)'));
  assert.ok(home.includes('previewOverlay.sync()'));

  const at = app.indexOf('onOpenChange:');
  const handler = app.slice(at, app.indexOf('onStatus:', at));
  assert.ok(handler.includes('previewOverlay.block(BLOCK_MIXER)'));
  assert.ok(handler.includes('previewOverlay.unblock(BLOCK_MIXER)'));
});

test('there is no start, no stop and no live teardown of the preview branch', () => {
  // MEASURED: a set_state(NULL) inside a blocking pad probe took the ON-AIR leg
  // from 50 fps to 0 permanently, with the pipeline still reporting PLAYING. The
  // branch is built at START from the saved configuration and there is nothing
  // on this side that could ask for it to be built or torn down at any other
  // moment — which is why the adapter has two methods and not five.
  const backend = codeOnly(ui('backend.js'));
  assert.match(backend, /rect: 'SetPreviewRect'/);
  assert.match(backend, /visible: 'SetPreviewVisible'/);
  for (const forbidden of ['StartPreview', 'StopPreview']) {
    assert.ok(
      !backend.includes(forbidden),
      `backend.js must not bind ${forbidden}: there is no safe moment to call it`,
    );
  }
  const app = codeOnly(ui('app.js'));
  assert.ok(!/startPreview|stopPreview/.test(app));

  // All-or-nothing availability, for pictureAvailable's reason: a build with the
  // rect binding and not the visible one would paint a window it could never
  // take off the Settings screen.
  assert.match(backend, /export function previewAvailable\(\) \{\s*return PREVIEW_METHOD_NAMES\.every\(hasBinding\);/);
  assert.match(app, /previewBindingsPresent = backend\.usingFakeBackend \|\| backend\.previewAvailable\(\)/);
});

test('the preview box is reserved from the configuration and captioned honestly', () => {
  const app = codeOnly(ui('app.js'));
  const render = app.slice(app.indexOf('function renderPreview()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.ok(body.length > 0, 'app.js must define renderPreview');
  assert.match(
    body,
    /const reserved = previewBindingsPresent && effects\.wantCard && currentPreviewEnabled/,
    'no bindings, no card selected or no preview asked for must all reserve nothing',
  );
  assert.match(body, /home\.setPreviewCaption\(describePreviewBox\(currentCapture\?\.picture\)\)/);

  // ============ THE SURFACE IS WANTED ON THE CONFIGURATION ALONE =============
  //
  // It used to be `reserved && running`, because the preview was a branch of the
  // session's pipeline and there was nothing to show before START. The picture
  // capture is built at launch and held until the application quits now, so
  // gating on a session would hide a live preview for the whole of the period
  // the operator actually uses it in: the setting-up, before anything is sent.
  assert.match(body, /previewOverlay\.setWanted\(reserved\)/);
  assert.equal(
    /running/.test(body),
    false,
    'renderPreview must not read the sender state at all: every answer it gives is now a function ' +
      'of the saved configuration and the capture state',
  );

  // RESERVING IS A LAYOUT CHANGE: .pgm-tile is sized against what is left in the
  // stage, so the commentator's picture has just moved and BOTH surfaces have to
  // be re-measured. An overlay left at yesterday's rectangle is a native window
  // over the controls beside it.
  assert.match(body, /overlay\.sync\(\);\s*previewOverlay\.sync\(\);/);

  // AND THE SENDER EDGE NO LONGER REDRAWS IT. This is the other half of the same
  // change and it is asserted separately because deleting the call is what makes
  // the preview survive STOP: a renderPreview on the STOP edge would take the
  // box away again a frame after capture had filled it.
  const sender = app.slice(app.indexOf('backend.onSender('));
  const handler = sender.slice(0, sender.indexOf('\n  });'));
  assert.equal(
    /renderPreview\(\)/.test(handler),
    false,
    'the sender edge must not touch the preview: it is capture, not a session, that fills the box',
  );
});

test('a capture fault reaches the operator as an alert row, and is retired when it clears', () => {
  // §12: "a capture fault names itself on screen". The column is the only
  // surface on this screen that may gain and lose rows — the operator's rule is
  // that nothing which arrives, changes or clears may move the picture — so this
  // is an alert and never a banner, and home.js's column is fixed-width with its
  // own scroll.
  const app = codeOnly(ui('app.js'));
  assert.match(app, /backend\.onCapture\(\(payload\) => \{/, 'app.js must subscribe to the event');
  const render = app.slice(app.indexOf('function renderCapture()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.ok(body.length > 0, 'app.js must define renderCapture');

  // ONE ROW, RAISED AND RETIRED. The event repeats on every state change and a
  // device that will not open can produce a run of them, so only a CHANGE of
  // message is acted on, and the standing row is taken down BY NAME rather than
  // by clearing a column that holds everything else the operator has not read.
  assert.match(body, /if \(message === captureAlert\) return;/);
  assert.match(body, /home\.clearErrorIf\(captureAlert\)/);
  assert.equal(
    /home\.clearError\(\)/.test(body),
    false,
    'clearing the whole feed to retire one row eats every unrelated alert with it',
  );

  // The reason is Go's own sentence, passed through rather than reworded: it
  // names the device and the way out, and a second copy on this side would be a
  // worse one that drifts.
  const fault = app.slice(app.indexOf('function describeCaptureFault(capture)'));
  assert.match(fault.slice(0, fault.indexOf('\n  }')), /capture\.reason/);

  const home = ui('home.js');
  assert.match(home, /\n    clearErrorIf,/, 'home.js must expose clearErrorIf on the view');
});

test('the preview box has a home in the stylesheet, and the caption is inside it', () => {
  const sheet = css();
  for (const selector of ['.preview-tile', '.preview-caption', '.field--check']) {
    assert.ok(sheet.includes(selector), `main.css must style ${selector}`);
  }
  // It must not be able to cover the commentator's picture: capped, ratio-locked,
  // and beside the tile rather than over it.
  //
  // ANCHORED AT THE LINE START, and that is not fussiness: it was already wrong
  // once. Any descendant rule — `.something .preview-tile {` — CONTAINS the
  // string '.preview-tile {', so an unanchored indexOf silently reads whichever
  // rule is written first and asserts the base rule's properties against it.
  const at = sheet.indexOf('\n.preview-tile {');
  assert.ok(at > 0, 'main.css must carry a base .preview-tile rule');
  const tile = sheet.slice(at, sheet.indexOf('}', at));
  assert.match(tile, /aspect-ratio:\s*16 \/ 9/);
  assert.match(tile, /clamp\(/, 'the preview must be capped, not a fraction of an arbitrarily large window');
  assert.ok(!/position:\s*absolute/.test(tile), 'it is a flex sibling of the picture, never laid over it');

});

test('the video source is adopted from every path that adopts a configuration', () => {
  // One function, called from all of them, so the lamp and the reserved
  // rectangle cannot end up describing different configurations. The three paths
  // are the startup load, a save (Settings, and an applied preset through
  // applyConfigLive) and another seat's save.
  //
  // The second argument is WHOSE HAND IT WAS, and it is the whole of the rule
  // below: only a local action may resize this desk's picture.
  const app = codeOnly(ui('app.js'));
  assert.match(app, /function adoptVideoConfig\(config, local\) \{/);
  for (const fn of ['function applyConfigLive(config)', 'function applyRemoteConfig(config)']) {
    const at = app.indexOf(fn);
    assert.ok(at > 0, `app.js must still have ${fn}`);
    const body = app.slice(at, app.indexOf('\n  }', at));
    assert.match(
      body,
      /adoptVideoConfig\(config, (?:true|false)\)/,
      `${fn} must adopt the video leg, or this desk's CAMERA lamp goes on reading SLATE after the ` +
        'position has been moved onto the card',
    );
  }
  const init = app.slice(app.indexOf('(async function init()'));
  assert.match(init, /adoptVideoConfig\(currentConfig, true\)/, 'and the startup load must too');
});

test("another seat's save never moves this commentator's picture", () => {
  // ==================== THE RULE, IN THE OPERATOR'S WORDS ====================
  //
  //   "For the comentators watching a live match, anything casuing their video
  //    to move is a massive no."
  //
  // Reserving the preview box is a LAYOUT CHANGE: .pgm-tile is sized against
  // what is left in the stage. Measured at 1600x900, turning the preview on took
  // the picture from x=70.172 y=85.781 w=1076.656 h=605.609 to x=20 y=126.422
  // w=932.141 h=524.328 — a 144.5px (13.4%) width loss and a 40.6px vertical
  // shift. Acceptable when the operator at THIS desk asked for it; not
  // acceptable at all when the config arrived over the wire, because then the
  // commentator's picture jumps mid-match at a stranger's keystroke.
  //
  // So the remote path adopts the readouts and stops short of the layout.
  const app = codeOnly(ui('app.js'));

  const remoteAt = app.indexOf('function applyRemoteConfig(config)');
  const remote = app.slice(remoteAt, app.indexOf('\n  }', remoteAt));
  assert.match(remote, /adoptVideoConfig\(config, false\)/, 'a remote save is not a local one');

  const liveAt = app.indexOf('function applyConfigLive(config)');
  const live = app.slice(liveAt, app.indexOf('\n  }', liveAt));
  assert.match(live, /adoptVideoConfig\(config, true\)/, "this desk's own save may resize it");

  // And the flag has to REACH the layout call, not merely be accepted.
  const adoptAt = app.indexOf('function adoptVideoConfig(config, local)');
  const adopt = app.slice(adoptAt, app.indexOf('\n  }', adoptAt));
  assert.match(adopt, /if \(local\) renderPreview\(\);/, 'renderPreview is the one that reserves');
  assert.match(adopt, /else renderPreviewCaptionOnly\(\);/);

  // The caption-only path must not reserve, and must not sync overlays: it has
  // moved nothing, so there is nothing to re-measure.
  const capAt = app.indexOf('function renderPreviewCaptionOnly()');
  assert.ok(capAt > 0, 'app.js must define renderPreviewCaptionOnly');
  const cap = app.slice(capAt, app.indexOf('\n  }', capAt));
  assert.match(cap, /home\.setPreviewCaption\(/, 'it still tells the truth about the state');
  assert.ok(
    !cap.includes('setPreviewReserved'),
    'reserving is the layout change this whole path exists to avoid',
  );
});
