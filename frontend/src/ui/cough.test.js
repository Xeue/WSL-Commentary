/**
 * Tests for the cough mute: the two behaviours, the one truth, and the ways a
 * microphone gets left dead.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * This is the control whose state being MISREAD puts a cough on air or leaves a
 * commentator talking into a dead microphone, and both failures are silent at
 * this desk: nothing on this machine can hear the broadcast output. There is no
 * feedback loop. The screen is the only evidence the operator has, so every
 * property below is about the screen and the send path agreeing.
 *
 * The four that have actually bitten people, on other systems if not yet on this
 * one:
 *
 *  1. A HELD KEY WHOSE RELEASE IS NEVER SEEN. The window loses focus, the
 *     pointer slides off the button, the operator alt-tabs mid-cough. release()
 *     is idempotent and is called from all of those.
 *  2. A PUSH AND ITS RELEASE INSIDE ONE ROUND TRIP. The release is issued while
 *     the press is in flight; a naive implementation drops it and the microphone
 *     stays muted for the rest of the match.
 *  3. A CALM "MUTED" OVER A LIVE MICROPHONE. The call was refused and the
 *     display showed the INTENT anyway.
 *  4. A MUTE NOBODY AT THIS DESK MADE. A second seat can mute (app_remote.go
 *     allows it deliberately), and a red badge with no explanation is worse than
 *     the mute itself.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  createCoughMute,
  describeMute,
  describeMuteKey,
  describeMuteMode,
  isTypingTarget,
  isSpaceActivated,
  normaliseMuteMode,
  MUTE_KEY_PUSH,
  MUTE_KEY_LATCH,
  MUTE_STATE,
  MUTE_MODE,
  MUTE_BY_REMOTE,
  DEFAULT_MUTE_MODE,
  HOLD_REASON,
  KEY_CAPS,
} from './cough.js';

/** payload builds one of app.go's mutePayloads. */
function payload(muted, extra = {}) {
  return { muted, available: true, reason: '', by: muted ? 'desk' : '', byAddr: '', seq: 1, ...extra };
}

/**
 * fakeGo is a controllable stand-in for App.SetCommentaryMute.
 *
 * It records what it was asked for and lets a test decide WHEN each call
 * resolves, which is the only way to drive the in-flight cases that matter.
 */
function fakeGo() {
  const calls = [];
  let pending = [];
  return {
    calls,
    apply(muted) {
      calls.push(muted);
      return new Promise((resolve, reject) => {
        pending.push({ muted, resolve, reject });
      });
    },
    /** settle resolves every outstanding call with Go's own answer. */
    async settle(answer) {
      const now = pending;
      pending = [];
      for (const p of now) p.resolve(answer ? answer(p.muted) : payload(p.muted));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    },
    /** fail rejects every outstanding call. */
    async fail(message) {
      const now = pending;
      pending = [];
      for (const p of now) p.reject(new Error(message));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    },
    get outstanding() {
      return pending.length;
    },
  };
}

/** build makes a model with a fake Go behind it and captures every readout. */
function build(opts = {}) {
  const go = fakeGo();
  const readouts = [];
  const mute = createCoughMute({
    apply: (m) => go.apply(m),
    onChange: (r) => readouts.push(r),
    ...opts,
  });
  return { go, mute, readouts, last: () => readouts[readouts.length - 1] };
}

// ---------------------------------------------------------------------------
// The two behaviours are an OR, not a mode
// ---------------------------------------------------------------------------

test('holding mutes and releasing goes live again', async () => {
  const g = build();
  g.mute.press();
  assert.deepEqual(g.go.calls, [true]);
  await g.go.settle();
  assert.equal(g.last().state, MUTE_STATE.MUTED);
  assert.equal(g.last().reason, HOLD_REASON.PUSH);

  g.mute.release();
  await g.go.settle();
  assert.equal(g.last().state, MUTE_STATE.LIVE);
  assert.equal(g.last().muted, false);
});

test('the latch survives letting go of the push key', async () => {
  // The whole reason the two are an OR rather than a mode switch: a commentator
  // who has latched and then instinctively holds the key must not have the hold
  // UNDO the latch when they let go.
  const g = build();
  g.mute.toggleLatch();
  await g.go.settle();
  g.mute.press();
  await g.go.settle();
  assert.equal(g.last().reason, HOLD_REASON.BOTH);

  g.mute.release();
  await g.go.settle();
  assert.equal(g.last().state, MUTE_STATE.MUTED, 'still muted — the latch is holding it');
  assert.equal(g.last().reason, HOLD_REASON.LATCH);
  assert.deepEqual(g.go.calls, [true], 'and Go was never asked to unmute, because nothing changed');
});

test('press and release are idempotent, so a repeated key event costs nothing', async () => {
  const g = build();
  g.mute.press();
  g.mute.press();
  g.mute.press();
  await g.go.settle();
  assert.deepEqual(g.go.calls, [true], 'one call, not three');

  g.mute.release();
  g.mute.release();
  await g.go.settle();
  assert.deepEqual(g.go.calls, [true, false]);
});

// ---------------------------------------------------------------------------
// Failure 2: the release that lands inside the press's round trip
// ---------------------------------------------------------------------------

test('a release issued while the press is in flight still reaches Go', async () => {
  // THE DEAD MICROPHONE. A quick cough is well under one IPC round trip. Without
  // the re-check after a flight lands, the release is swallowed and the
  // commentary stays muted with nobody holding anything.
  const g = build();
  g.mute.press();
  assert.equal(g.go.outstanding, 1);

  g.mute.release(); // while the mute is still in flight
  assert.deepEqual(g.go.calls, [true], 'nothing is issued on top of a call in flight');

  await g.go.settle();
  assert.deepEqual(g.go.calls, [true, false], 'and the release is re-issued when the flight lands');
  await g.go.settle();
  assert.equal(g.last().state, MUTE_STATE.LIVE);
  assert.equal(g.last().muted, false);
});

test('several changes inside one round trip collapse to the LAST one', async () => {
  // Latest-wins, not a queue: replaying the intermediate states could leave the
  // send path muted after the operator has already let go.
  const g = build();
  g.mute.press();
  g.mute.release();
  g.mute.press();
  g.mute.release();
  g.mute.press();
  assert.deepEqual(g.go.calls, [true]);

  await g.go.settle();
  assert.deepEqual(g.go.calls, [true], 'the intent still matches what Go was told; nothing to re-issue');
  assert.equal(g.last().state, MUTE_STATE.MUTED);
});

// ---------------------------------------------------------------------------
// Failure 3: never a calm MUTED over a live microphone
// ---------------------------------------------------------------------------

test('a refused mute reads MUTE FAILED, and does not claim to be muted', async () => {
  const g = build();
  g.mute.press();
  await g.go.fail('nothing is being sent yet');
  const r = g.last();
  assert.equal(r.state, MUTE_STATE.FAILED);
  assert.equal(r.muted, false, 'the call was refused, so the send path is whatever it was');
  assert.match(r.detail, /nothing is being sent yet/);
});

test('while a call is in flight the readout says so rather than claiming the mute landed', async () => {
  const g = build();
  g.mute.press();
  const r = g.last();
  assert.equal(r.state, MUTE_STATE.MUTING);
  assert.equal(r.muted, false, 'nothing has been confirmed yet');
  assert.equal(r.held, true, 'but the button shows itself pressed, because it is');
});

test('a mute that was ACCEPTED but reported back as unmuted is believed', async () => {
  // app.go drops a request older than the last one applied and returns the state
  // actually IN FORCE with no error. The model must take that answer rather than
  // its own request: the caller whose key-up won the race is right.
  const g = build();
  g.mute.press();
  await g.go.settle(() => payload(false));
  assert.equal(g.last().muted, false, "Go's answer wins over this side's intent");
});

// ---------------------------------------------------------------------------
// Failure 4: a mute this desk did not make
// ---------------------------------------------------------------------------

test('a mute made by a remote seat says so on the badge, not only in a tooltip', () => {
  const r = describeMute({
    held: false,
    latched: false,
    confirmed: true,
    pending: false,
    failure: '',
    available: true,
    by: MUTE_BY_REMOTE,
    byAddr: '10.0.0.42:51001',
  });
  assert.equal(r.state, MUTE_STATE.MUTED);
  assert.equal(
    r.text,
    'MUTED (REMOTE)',
    'app.go: "a remote seat silently muting the feed with nobody at the desk knowing is the ' +
      'failure that made this decision arguable in the first place"',
  );
  assert.match(r.detail, /10\.0\.0\.42:51001/, 'and the address is reachable for the moment it is wanted');
});

test('adopting an authoritative payload overrides whatever this page thought', async () => {
  const g = build();
  const before = g.readouts.length;
  g.mute.adopt(payload(true, { by: MUTE_BY_REMOTE, byAddr: '10.0.0.7:1' }));

  // The FIRST readout after adopt is the adopted truth: this page is told the
  // commentary is muted and by whom, whatever its own buttons say.
  const adopted = g.readouts[before];
  assert.equal(adopted.muted, true);
  assert.equal(adopted.text, 'MUTED (REMOTE)');

  // And then it re-pumps: this desk's controls say LIVE, so the model asks Go to
  // return to what the operator has actually chosen, and says it is asking.
  assert.deepEqual(g.go.calls, [false]);
  assert.equal(g.last().state, MUTE_STATE.UNMUTING);

  await g.go.settle();
  assert.equal(g.last().state, MUTE_STATE.LIVE);
});

// ---------------------------------------------------------------------------
// Availability: "no session" is not "no mute"
// ---------------------------------------------------------------------------

test('an unavailable payload disables the control and uses the Go side\'s own sentence', () => {
  const r = describeMute({
    held: false,
    latched: false,
    confirmed: false,
    pending: false,
    failure: '',
    available: false,
    reason: 'nothing is being sent yet, so there is nothing to mute.',
  });
  assert.equal(r.state, MUTE_STATE.UNAVAILABLE);
  assert.equal(r.available, false);
  assert.equal(r.level, 'grey', 'not red: this is the ordinary state of a machine before START');
  assert.match(r.detail, /nothing is being sent yet/, 'two descriptions of one fact is one too many');
});

test('nothing is sent to Go while the mute is unavailable', async () => {
  const g = build({ available: false });
  g.mute.press();
  g.mute.toggleLatch();
  assert.deepEqual(g.go.calls, [], 'a call that can only be refused must not be made');
});

test('a session ending drops the HELD state but keeps the LATCH', async () => {
  const g = build();
  g.mute.toggleLatch();
  g.mute.press();
  await g.go.settle();
  assert.equal(g.mute.snapshot.held, true);
  assert.equal(g.mute.snapshot.latched, true);

  // STOP: app.go clears the mute at the session boundary and publishes an
  // unavailable payload.
  g.mute.adopt({ muted: false, available: false, reason: 'nothing is being sent yet' });
  assert.equal(g.mute.snapshot.held, false, 'a key the operator is no longer holding must not be remembered');
  assert.equal(
    g.mute.snapshot.latched,
    true,
    'the latch is a deliberate standing choice; dropping it would put a live microphone up ' +
      'without anybody asking',
  );

  // START again: the latch is re-issued as soon as there is something to mute.
  g.mute.adopt(payload(false, { by: '' }));
  assert.deepEqual(g.go.calls.slice(-1), [true], 'the standing choice is re-applied to the new session');
});

// ---------------------------------------------------------------------------
// The keyboard bindings
// ---------------------------------------------------------------------------

test('the two bound keys are distinct, and both have a printable cap', () => {
  assert.notEqual(MUTE_KEY_PUSH, MUTE_KEY_LATCH);
  assert.equal(describeMuteKey(MUTE_KEY_PUSH), 'Space');
  assert.equal(describeMuteKey(MUTE_KEY_LATCH), 'M');
  for (const code of [MUTE_KEY_PUSH, MUTE_KEY_LATCH]) {
    assert.ok(KEY_CAPS[code], `${code} must have a legend to print on its button`);
  }
  assert.equal(describeMuteKey('KeyQ'), 'KeyQ', 'an unknown code falls back to itself, never to blank');
  assert.equal(describeMuteKey(undefined), '');
});

test('typing in a field is not a cough mute', () => {
  // Without this, Space in the SRT passphrase field mutes the commentary — and
  // the keyup that would unmute it arrives only if the field still has focus.
  for (const tag of ['INPUT', 'TEXTAREA', 'SELECT', 'OPTION']) {
    assert.equal(isTypingTarget({ tagName: tag }), true, `${tag} is somewhere a keystroke is a character`);
  }
  assert.equal(isTypingTarget({ isContentEditable: true, tagName: 'DIV' }), true);
  assert.equal(
    isTypingTarget({ tagName: 'BUTTON' }),
    false,
    'a button is not somewhere a keystroke is a character; whether Space belongs to it is a ' +
      'different question, asked by isSpaceActivated',
  );
  assert.equal(isTypingTarget(null), false);
  assert.equal(isTypingTarget(undefined), false);
  assert.equal(isTypingTarget('not an element'), false);
});

// ---------------------------------------------------------------------------
// The mode is emphasis, and takes nothing away
// ---------------------------------------------------------------------------

test('the mode normalises like config.EffectiveCoughMuteMode, default included', () => {
  assert.equal(normaliseMuteMode(MUTE_MODE.LATCH), MUTE_MODE.LATCH);
  assert.equal(normaliseMuteMode(MUTE_MODE.PUSH), MUTE_MODE.PUSH);
  assert.equal(normaliseMuteMode(''), DEFAULT_MUTE_MODE, 'empty is "not chosen"');
  assert.equal(normaliseMuteMode('  latch '), MUTE_MODE.LATCH);
  assert.equal(normaliseMuteMode('nonsense'), DEFAULT_MUTE_MODE, 'and so is a hand-edited value');
  assert.equal(normaliseMuteMode(undefined), DEFAULT_MUTE_MODE);
  assert.equal(DEFAULT_MUTE_MODE, MUTE_MODE.PUSH, 'mirrors config.DefaultCoughMuteMode');
});

test('BOTH behaviours work whichever mode is chosen', async () => {
  // The mode is about which control is drawn primary. Taking the other away is
  // what would strand a commentator: a mis-pressed mode switch must never remove
  // the control that would release the mute they are holding.
  for (const mode of [MUTE_MODE.PUSH, MUTE_MODE.LATCH]) {
    const g = build({ mode });
    g.mute.press();
    await g.go.settle();
    assert.equal(g.last().muted, true, `push must work in ${mode} mode`);
    g.mute.release();
    await g.go.settle();
    g.mute.toggleLatch();
    await g.go.settle();
    assert.equal(g.last().muted, true, `latch must work in ${mode} mode`);
  }
});

test('with nothing muted the readout names the active mode', () => {
  const g = build({ mode: MUTE_MODE.LATCH });
  assert.equal(g.mute.readout.state, MUTE_STATE.LIVE);
  assert.equal(
    g.mute.readout.reason,
    'LATCH MODE',
    'a preference that only shows itself when it is pressed is a preference nobody can check',
  );
  g.mute.setMode(MUTE_MODE.PUSH);
  assert.equal(g.mute.readout.reason, 'PUSH MODE');
  assert.equal(describeMuteMode(MUTE_MODE.LATCH), 'LATCH');
  assert.equal(describeMuteMode('anything else'), 'PUSH');
});

// ---------------------------------------------------------------------------
// The readout is one object, so the screen cannot disagree with itself
// ---------------------------------------------------------------------------

test('every readout carries the button states as well as the words', async () => {
  const g = build();
  g.mute.toggleLatch();
  await g.go.settle();
  const r = g.last();
  assert.equal(r.latched, true);
  assert.equal(r.held, false);
  assert.equal(r.available, true);
  assert.equal(r.mode, DEFAULT_MUTE_MODE);
  assert.equal(
    r.muted,
    true,
    'the buttons and the word come from ONE object; a button reading its own variable is a second ' +
      'place the truth lives',
  );
});

test('a malformed payload cannot make the control claim a mute', () => {
  const g = build();
  g.mute.adopt(null);
  assert.equal(g.last().muted, false);
  g.mute.adopt('nonsense');
  assert.equal(g.last().muted, false);
  g.mute.adopt({});
  assert.equal(g.last().muted, false);
});

// ---------------------------------------------------------------------------
// THE MUTE IS AT THE SEND PATH OR IT IS NOT A MUTE
//
// Read from source, because this module cannot enforce it: it calls whatever
// `apply` it is given, and the whole risk is that somebody makes that something
// cheaper. Muting the monitor element, the Web Audio gain or the return path
// makes THIS DESK quieter while the cough goes to air — the exact failure
// inverted, with a green light on it, and undetectable from here because nothing
// on this machine can hear the broadcast output.
// ---------------------------------------------------------------------------

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');
const codeOnly = (src) => src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');

test('app.js wires the cough mute to the SEND-PATH binding and to nothing else', () => {
  const app = codeOnly(ui('app.js'));
  const start = app.indexOf('const coughMute = createCoughMute({');
  assert.ok(start > 0, 'app.js must build the model');
  const body = app.slice(start, app.indexOf('});', start));

  assert.match(
    body,
    /apply: \(muted\) => backend\.setCommentaryMute\(muted\)/,
    'apply must be the Go binding that mutes the commentary going to the switcher',
  );
  for (const forbidden of [
    'audioEl',
    'monitor',
    'setMuted',
    'gain',
    'setLevel',
    'returnPath',
    'setAudioEnabled',
  ]) {
    assert.ok(
      !body.includes(forbidden),
      `the cough mute reaches ${forbidden}. Muting a monitor makes this desk quieter while the ` +
        'cough goes to air; the commentary is muted at the SEND path or it is not muted at all',
    );
  }
});

test('the three gestures go straight to the model, with no branch in between', () => {
  const app = codeOnly(ui('app.js'));
  for (const [handler, call] of [
    ['onMutePress', 'coughMute.press()'],
    ['onMuteRelease', 'coughMute.release()'],
    ['onMuteLatchToggle', 'coughMute.toggleLatch()'],
  ]) {
    assert.ok(
      app.includes(`${handler}: () => ${call}`),
      `${handler} must call ${call} and nothing else: a branch here is a second place that ` +
        'decides what a mute means, and the key and the button would eventually disagree',
    );
  }
});

test('home.js draws the mute and never decides it', () => {
  const home = codeOnly(ui('home.js'));
  // It may raise the three gestures and paint one readout object.
  assert.match(home, /handlers\.onMutePress\(\)/);
  assert.match(home, /handlers\.onMuteRelease\(\)/);
  assert.match(home, /handlers\.onMuteLatchToggle\(\)/);
  assert.match(home, /function setMuteReadout\(readout\)/);
  assert.ok(
    !/createCoughMute|setCommentaryMute/.test(home),
    'home.js must not own the state machine or reach the binding — it has no backend knowledge',
  );
  // And the release is wired from every way of losing a key-up.
  for (const escape of ['pointerup', 'pointercancel', 'pointerleave', 'blur', 'visibilitychange']) {
    assert.ok(
      home.includes(escape),
      `a hold whose release arrives as ${escape} must still release: a hold that is never ` +
        'released is a dead microphone for the rest of the match',
    );
  }
});

test('the bound keys are printed on the buttons, not hidden in a tooltip', () => {
  const home = codeOnly(ui('home.js'));
  assert.match(
    home,
    /keyCap\('PUSH TO MUTE', MUTE_KEY_PUSH\)/,
    'this control exists for the moment when looking at the screen is what the operator cannot ' +
      'do; a shortcut nobody can see is a shortcut nobody uses',
  );
  assert.match(home, /keyCap\('LATCH MUTE', MUTE_KEY_LATCH\)/);
  assert.match(home, /cap\.className = 'keycap'/, 'and it is drawn as a key cap, so it reads as one');
});


test('Space belongs to whatever Space already activates', () => {
  // THE FAILURE: the push-to-mute binding is document-level, capturing, and
  // calls preventDefault on both edges of Space. A <button> is activated by
  // Space on the KEYUP, so that binding stopped every button in the application
  // responding to a keyboard — Save in Settings, the Mixer drawer, all of it.
  // Somebody working without a mouse could reach a control and never fire it.
  const attrs = (role) => ({
    tagName: 'DIV',
    getAttribute: (n) => (n === 'role' ? role : null),
  });

  assert.equal(isSpaceActivated({ tagName: 'BUTTON' }), true, 'the case that was broken');
  assert.equal(isSpaceActivated({ tagName: 'SUMMARY' }), true);
  for (const role of ['button', 'checkbox', 'radio', 'switch', 'tab', 'menuitem', 'option']) {
    assert.equal(isSpaceActivated(attrs(role)), true, `role=${role} is activated by Space`);
  }
  assert.equal(isSpaceActivated(attrs('BUTTON')), true, 'the role match is case-insensitive');

  // And the other half of the rule: everywhere the operator's focus actually
  // sits during a match, Space is still the mute and nothing else.
  assert.equal(isSpaceActivated({ tagName: 'BODY' }), false);
  assert.equal(isSpaceActivated({ tagName: 'DIV' }), false);
  assert.equal(isSpaceActivated({ tagName: 'VIDEO' }), false);
  assert.equal(isSpaceActivated(attrs('presentation')), false);
  assert.equal(isSpaceActivated(attrs(null)), false);
  assert.equal(isSpaceActivated({ tagName: 'A' }), false, 'Space scrolls on a link, it does not follow it');
  assert.equal(isSpaceActivated(null), false);
  assert.equal(isSpaceActivated(undefined), false);
  assert.equal(isSpaceActivated('not an element'), false);
});
