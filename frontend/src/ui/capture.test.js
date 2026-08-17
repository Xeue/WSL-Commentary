/**
 * Source guards on the always-live CAPTURE seam in ./backend.js.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * # What this file is for
 *
 * backend.js is the boundary every screen reaches the Go side through, and this
 * seam is the one every screen in this change reaches it through. The three
 * bindings, the event, the four states and the fake's LIFETIME are the contract
 * two other work packages are being written against at the same time as this
 * one, so the failures worth guarding are textual: a rename, an availability
 * check narrowed to a subset, a fake quietly re-attached to the session.
 *
 * There is no DOM test library in this repository by design (package.json is
 * frozen), and backend.js's exports mostly do I/O, so this reads source text —
 * for the reason mixerwiring.test.js documents at length.
 *
 * # Why this is its own file rather than more of channelmap.test.js
 *
 * Capture is not a routing question. The routing panel is one of the things it
 * feeds; the preview, the meters, the signal lamp and the cough mute are the
 * others, and putting the seam's guards under the narrowest of its consumers is
 * how a guard ends up deleted along with the feature that happened to host it.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);
const repoRoot = join(here, '..', '..', '..');

/* ------------------------------------------------------------------------ */
/* The bindings                                                              */
/* ------------------------------------------------------------------------ */

test('the capture seam names its three bindings in one table and exports all four calls', () => {
  const backend = ui('backend.js');

  // Wails binds by EXACT NAME, and a drift is not an error anywhere: the call
  // rejects with "not bound" against a build that has the feature. One table,
  // so a rename is one edit — the shape RETURN_METHODS, PRESET_METHODS,
  // PICTURE_METHODS, PREVIEW_METHODS and MUTE_METHODS all use.
  assert.match(backend, /const CAPTURE_METHODS = Object\.freeze\(\{/);
  const table = backend.slice(backend.indexOf('const CAPTURE_METHODS'), backend.indexOf('CAPTURE_METHOD_NAMES'));
  const names = [...table.matchAll(/'([A-Za-z]+)'/g)].map((m) => m[1]);
  assert.deepEqual(
    names,
    ['SelectCommentaryInput', 'RestartCapture', 'GetCaptureState'],
    'these three are app.go’s bound method names; they are not spelled anywhere else in the frontend',
  );

  // ============ AND EACH ONE IS CHECKED AGAINST app.go, WHICH IS THE POINT ====
  //
  // This assertion used to compare the table against a LIST WRITTEN IN THIS FILE
  // and nothing else. That is a spelling test of a literal against itself: the
  // three methods did not exist in Go at all while it passed, which is precisely
  // the state it was written to make impossible. A binding that is not there
  // does not fail at build time in either language — Wails simply never installs
  // it and callGoBound rejects at runtime, during a match — so the only guard
  // available is textual and it has to read the OTHER side.
  //
  // The regex is the receiver form Wails binds on, so a method demoted to a
  // helper (lower case) or moved off *App fails here rather than at the desk.
  const go = read(repoRoot, 'app.go');
  for (const method of names) {
    assert.match(
      go,
      new RegExp(`func \\(a \\*App\\) ${method}\\(`),
      `app.go must export ${method} on *App, or this binding rejects at runtime with "not bound"`,
    );
  }

  // Every bound method needs a remoteAllowlist row (CONTRACT.md), and a missing
  // one is a method that works at the desk and fails from the second seat. The
  // two SETTERS are host-only — which microphone a broadcast switcher hears this
  // position on, and a rebuild that blanks the operator's own picture — and the
  // READ is reachable, so a remote seat can draw the same explanation.
  const allowlist = read(repoRoot, 'app_remote.go');
  for (const method of ['SelectCommentaryInput', 'RestartCapture']) {
    assert.match(
      allowlist,
      new RegExp(`"${method}":\\s*\\{hostOnly: true\\}`),
      `${method} must be host-only in remoteAllowlist`,
    );
  }
  assert.match(allowlist, /"GetCaptureState":\s*\{\}/);

  // And the event name is Go's spelling, not a second one invented here.
  assert.match(go, /EventCapture = "capture"/);
  assert.match(backend, /export const EVENT_CAPTURE = 'capture';/);

  for (const [fn, kind] of [
    ['selectCommentaryInput', 'async function'],
    ['restartCapture', 'async function'],
    ['getCaptureState', 'async function'],
    ['onCapture', 'function'],
  ]) {
    assert.ok(backend.includes(`export ${kind} ${fn}(`), `backend.js must export ${fn}`);
  }

  // The names list is DERIVED from the table, never listed again: a fourth
  // binding added above is covered here without a second edit. Same reasoning as
  // RETURN_METHOD_NAMES.
  assert.match(backend, /const CAPTURE_METHOD_NAMES = Object\.freeze\(Object\.values\(CAPTURE_METHODS\)\);/);
});

test('captureAvailable is all-or-nothing over all three, like every other availability probe', () => {
  // A BUILD THAT COULD SELECT BUT NOT REPORT is the failure this shape exists to
  // stop, and it is the same failure channelMapAvailable's comment describes for
  // the routing grid: SelectCommentaryInput without GetCaptureState re-points the
  // commentary capture and then has no way to learn that it failed. The operator
  // picks a microphone, the screen shows the selection taking effect, and the
  // meter that never moves again is the only evidence anything went wrong.
  const backend = ui('backend.js');
  assert.match(
    backend,
    /export function captureAvailable\(\)\s*\{\s*return CAPTURE_METHOD_NAMES\.every\(hasBinding\);/,
    'captureAvailable must check every name in the table, not a subset',
  );

  // And the sibling it is modelled on is still shaped that way, so the two
  // cannot drift into disagreeing about what "available" means.
  assert.match(
    backend,
    /export function channelMapAvailable\(\)\s*\{\s*return CHANNEL_MAP_METHOD_NAMES\.every\(hasBinding\);/,
  );

  // Reaching the Go side through callGoBound is what turns "this build does not
  // have it" into a BindingMissingError instead of an opaque rejection.
  for (const call of ['CAPTURE_METHODS.select', 'CAPTURE_METHODS.restart', 'CAPTURE_METHODS.state']) {
    assert.ok(
      backend.includes(`callGoBound(${call}`),
      `${call} must be called through callGoBound, so an older build is named rather than guessed at`,
    );
  }
});

test('getCaptureState never throws, and says which kind of not-knowing it is', () => {
  // It is called on the startup path by a screen whose job is to EXPLAIN a
  // fault. A rejection there replaces the explanation with a stack trace, which
  // is the argument getChannelMap and getConformTarget already carry.
  const backend = ui('backend.js');
  const body = backend.slice(backend.indexOf('export async function getCaptureState()'));
  const fn = body.slice(0, body.indexOf('\n}'));
  assert.match(fn, /catch \{\s*return CAPTURE_UNAVAILABLE;/, 'a failed call must answer, not reject');

  // ============ AND IT IS NOT GATED ON captureAvailable() ==================
  //
  // REWRITTEN. This assertion used to be the opposite — it required
  // `if (!captureAvailable()) return CAPTURE_UNAVAILABLE;` — and the two halves
  // of this file's contract could not both hold. captureAvailable() is
  // all-or-nothing over three method names, two of which are host-only, so
  // internal/remote's shim prunes them, hasBinding reads them back as undefined
  // and the probe answers false on EVERY remote seat. GetCaptureState is on the
  // OPEN side of the allowlist — the test below still pins that — so the gate
  // made a method the dispatcher would happily serve unreachable from the only
  // seat that needed it, and a producer joining after the card failed at launch
  // saw nothing at all.
  //
  // The catch above is the real cover for an older build: callGoBound rejects
  // with a BindingMissingError and this answers CAPTURE_UNAVAILABLE.
  assert.equal(
    /captureAvailable\(\)/.test(fn),
    false,
    'getCaptureState must not gate on captureAvailable(): two of the three methods it probes ' +
      'are host-only, so the gate is always false on a remote seat — which is the seat this ' +
      'read exists for',
  );

  // The sibling with the identical shape, as the precedent rather than as a
  // coincidence: four of getPictureState's five methods are host-only too, and
  // it has always called straight through.
  assert.match(
    backend,
    /export async function getPictureState\(\)\s*\{\s*if \(hasWails\(\)\) return callGoBound\(PICTURE_METHODS\.state\);/,
  );

  // CAPTURE_UNAVAILABLE has to be a COMPLETE payload with a reason, so no caller
  // branches on which kind of answer it got and no screen has an "off" to
  // explain and nothing to explain it with.
  const unavailable = backend.slice(backend.indexOf('export const CAPTURE_UNAVAILABLE'));
  const decl = unavailable.slice(0, unavailable.indexOf('});'));
  for (const field of ['picture:', 'commentary:', 'reason:', 'audioDeviceName:']) {
    assert.ok(decl.includes(field), `CAPTURE_UNAVAILABLE must carry ${field}`);
  }
  assert.match(decl, /this build has no always-live capture/, 'the reason must say WHICH not-knowing this is');
});

test('the four capture states are spelled once, lowercase, and opening is one of them', () => {
  const backend = ui('backend.js');
  const decl = backend.slice(backend.indexOf('export const CAPTURE_STATE'), backend.indexOf('CAPTURE_UNAVAILABLE'));
  for (const [name, value] of [['OFF', 'off'], ['OPENING', 'opening'], ['LIVE', 'live'], ['FAILED', 'failed']]) {
    assert.ok(decl.includes(`${name}: '${value}'`), `CAPTURE_STATE.${name} must be '${value}'`);
  }
  // OPENING is the one a screen is most likely to skip, and the one that costs
  // most when it is: a card that is merely slow to lock sits there for a second
  // or more, and drawing "failed" over it has an operator pulling cables at a
  // pipeline that was about to come up.
  assert.match(backend, /OPENING IS NOT A TRANSIENT WORTH SKIPPING/);
  assert.match(backend, /export const EVENT_CAPTURE = 'capture';/);
});

/* ------------------------------------------------------------------------ */
/* The rulings this seam encodes                                             */
/* ------------------------------------------------------------------------ */

test('selecting a device is a call and not a save, and is refused while sending', () => {
  const backend = ui('backend.js');

  // NO SAVE. It is what makes the routing panel appear before Save, and it is
  // also the only way the panel COULD: the width comes from a pad, a pad comes
  // from an open device, and nothing on this side can negotiate one.
  const select = backend.slice(backend.indexOf('export async function selectCommentaryInput('));
  const fn = select.slice(0, select.indexOf('\n}'));
  assert.equal(
    /saveConfig|SaveConfig/.test(fn),
    false,
    'selectCommentaryInput must not save: the panel has to appear before Save, not because of it',
  );
  assert.match(fn, /callGoBound\(CAPTURE_METHODS\.select, k, String\(deviceId \?\? ''\), String\(persistentId \?\? ''\)\)/);

  // REFUSED WHILE SENDING, in the fake too. A second proxysrc attaching to a
  // live proxysink silently steals the stream and kills the first — measured, A
  // stopped dead at 5.994 s the instant B attached at 6.007 s — so re-pointing
  // capture under a running send pipeline is a feed that goes quietly dead with
  // every lamp still green. A fake that allowed it is a dev session in which the
  // screen's handling of the refusal is never once exercised.
  assert.match(fn, /if \(fakeSenderRunning\) \{/);
  assert.match(fn, /cannot be changed while sending/);
  const restart = backend.slice(backend.indexOf('export async function restartCapture()'));
  assert.match(restart.slice(0, restart.indexOf('\n}')), /if \(fakeSenderRunning\) \{/, 'a restart is a device change by another name');
});

test('the three operator rulings are written down where the seam implements them', () => {
  // These are decisions the operator made against this plan's own
  // recommendation in one case and with it in two, and every one of them
  // reverses something a previous reader wrote down. A seam that implements them
  // silently is a seam the next reader reverts.
  const backend = ui('backend.js');

  // A1: no Acquire, no Release, and RestartCapture is why that is survivable.
  assert.match(backend, /THE DECKLINK IS HELD FROM LAUNCH TO QUIT/);
  assert.equal(
    /AcquireCapture|ReleaseCapture/.test(backend),
    false,
    'A1 ruled there is no acquire/release control; adding one is a design change, not a convenience',
  );

  // A2: a pre-air cough mute is carried into the session, and the fear it
  // reverses — a control that lies — is answered by visibility, not by refusal.
  assert.match(backend, /CARRIED INTO THE SESSION/);

  // A3: during a send-side stall the commentary is DROPPED. The consequence has
  // to be stated where the meters are documented, because it is the one place
  // OnLevels' promise stops holding.
  assert.match(backend, /DROPPED, NOT DELAYED/);
  assert.match(
    backend,
    /holds in normal operation and NOT\n\/\/ during a stall/,
    'the levels doc must say where the no-meter-moves-over-silence promise stops',
  );
});

test('the fake capture cannot start a ticker outside a browser', () => {
  // A node --test run imports this module with no `window` at all, so every fake
  // is reachable from the suite — and setInterval there holds the event loop
  // open and the run NEVER EXITS. It does not fail either. This test is the
  // reason that trap is survivable: the guard is one constant, checked by both
  // tickers and by the module-load boot.
  //
  // This file is itself the proof, in a way no assertion is: it imports nothing
  // from backend.js, but settings.test.js does, and that suite still exits.
  const backend = ui('backend.js');
  assert.match(backend, /const fakeBrowserSession = usingFakeBackend && typeof window !== 'undefined';/);
  for (const starter of ['function startFakeLevels()', 'function startFakeChannelLevels()']) {
    const body = backend.slice(backend.indexOf(starter));
    assert.match(
      body.slice(0, body.indexOf('\n}')),
      /!fakeBrowserSession\) return;/,
      `${starter} must refuse to start a ticker outside a browser`,
    );
  }
  assert.match(backend, /if \(fakeBrowserSession\) \{\s*setTimeout\(fakeCaptureUp, 0\);\s*\}/);
});

test('the fake capture boots on a macrotask, so the launch events reach a listener', () => {
  // A LATE SUBSCRIBER MISSES EVERYTHING, and one of the three has no getter.
  //
  // main.js imports backend.js and then mounts every screen, and the screens
  // subscribe as they mount. An event emitted during backend.js's own module
  // evaluation therefore has no listeners at all: getCaptureState and
  // getChannelMap could still be read back, but "signal" has no binding to read
  // — it is published and never asked for — so the camera lamp would sit at
  // UNKNOWN for the whole dev session however the fake was configured.
  //
  // One macrotask is the fake's domReady, which is where the real application
  // builds capture and which is after every screen has subscribed.
  const backend = ui('backend.js');
  assert.match(backend, /DEFERRED BY ONE MACROTASK, WHICH IS THIS FAKE'S domReady/);
  assert.equal(
    /export (async )?function getSignal\(/.test(backend),
    false,
    'if a signal getter is ever added, this deferral stops being the only thing that makes the ' +
      'launch signal reachable — and this test should be re-argued rather than deleted',
  );
});

test('the fake resolves an empty device id rather than treating it as no device', () => {
  // An empty id is not "no device" in either family and the two mean different
  // things by it: a native seat with no id opens the PLATFORM DEFAULT INPUT, and
  // a decklink seat with no id means THE ONLY CARD, which is config.go's
  // documented reading of an empty decklinkPersistentId. A fake that treated ""
  // as no capture would show a first-run dev session an empty screen where the
  // product shows a live mono meter.
  const backend = ui('backend.js');
  const fn = backend.slice(backend.indexOf('function fakeInputDeviceFor('));
  const body = fn.slice(0, fn.indexOf('\n}'));
  assert.match(body, /if \(wanted === ''\)/);
  assert.match(body, /FAKE_DEFAULT_INPUT_ID/, 'a native seat with no id takes the platform default input');
  assert.match(body, /family\.length === 1 \? family\[0\] : null/, 'a decklink seat with no id means the only card');

  // An id that matches nothing is a real answer, not an exception: it is the
  // saved-device-not-plugged-in case, the likeliest fault at a seat whose
  // config.json was copied from another machine, and it must arrive as a capture
  // STATE with a reason rather than as a thrown error.
  assert.match(backend, /commentary: CAPTURE_STATE\.FAILED/);
  assert.match(backend, /is not connected to this machine/);
});

test('a restart rebuilds the LIVE selection, not the saved one', () => {
  // SelectCommentaryInput does not save, so between picking an interface and
  // pressing Save the configuration and the capture disagree about which device
  // this seat is on. RestartCapture has to rebuild the one capture is POINTED
  // AT.
  //
  // Rebuilding from the saved configuration instead produces the worst version
  // of this control, and it is reachable on the first day: an operator picks an
  // interface, it fails to open because it was asleep, they wake it and press
  // Restart capture — and the application silently goes back to the device they
  // were trying to leave, with the reason still on screen. This is a decision
  // app.go has to make the same way, because the same two facts exist there.
  const backend = ui('backend.js');
  assert.match(backend, /let fakeCommentarySelection = null;/);
  assert.match(
    backend,
    /const \{ kind, id \} = fakeCommentarySelection \|\| fakeConfiguredCommentaryInput\(\);/,
    'a rebuild must prefer the live selection and fall back to the configuration',
  );

  // A SAVE supersedes it, because the save is the moment the configuration
  // becomes the truth again — the form that was just written contains the
  // device, and a stale live selection would ignore a save that named another.
  const save = backend.slice(backend.indexOf('export async function saveConfig('));
  assert.match(
    save.slice(0, save.indexOf('\n}')),
    /fakeCommentarySelection = null;/,
    'a save must clear the live selection before rebuilding',
  );
});

test('the fake width comes from the device, never from its kind', () => {
  // THE DISCRIMINATOR IS THE THING BEING DELETED. applyStartChannelMapLocked used
  // to decide whether to write a matrix by asking whether the source could
  // produce channels=2 unaided, which osxaudiosrc's channels: [1, 2147483647]
  // template always says yes to — so no matrix was ever written on the native
  // path. Settled from GStreamer source, not inference: gstosxcoreaudio.c sets
  // `layout = NULL; /* no supported for sources */` unconditionally for every
  // source, so a Focusrite or an RME is byte-for-byte the same unpositioned
  // problem as the card.
  //
  // A fake that special-cased "decklink means sixteen" would put the deleted
  // discriminator back where the dev loop is the only thing that could catch it.
  const backend = ui('backend.js');
  const fn = backend.slice(backend.indexOf('function fakeCommentaryUp('));
  const body = fn.slice(0, fn.indexOf('\n}\n'));
  assert.match(body, /fakeClampWidth\(device\.channels\)/, 'the width is the device entry’s, whatever kind it is');
  assert.equal(
    /=== 'decklink'\s*\?\s*\d+/.test(body),
    false,
    'no width may be chosen by source kind: that is the discriminator this work deletes',
  );

  // The ceiling is gst.MaxInputChannels, raised 16 -> 32 with the measurement
  // beside it. Wider than that is a NAMED REFUSAL at selection time, off air —
  // never a Start that refuses.
  assert.match(backend, /const FAKE_MAX_INPUT_CHANNELS = 32;/);
});
