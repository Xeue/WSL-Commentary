/**
 * Two seats, one mute: the regression test for the oscillation.
 *
 * Run with:  node --test "src/**\/*.test.js"
 *
 * ======================= THE BUG THIS EXISTS TO CATCH =======================
 *
 * The application serves a remote UI over a LAN bridge, so the desktop app and
 * a browser tab are two SEATS onto one machine, both driving one cough mute.
 * The operator, with both open:
 *
 *   "When we have the webbrowser and the app open at the same time, the mute
 *    keys just flash on and off at a super high frequency when you try and use
 *    them"
 *
 * The cough mute is the control that takes a commentator off air. A mute that
 * oscillates is a microphone chattering on and off into a live contribution
 * feed, so this is a correctness defect in the most safety-critical control on
 * the screen and not a piece of UI polish.
 *
 * ============================ THE MECHANISM =================================
 *
 * Each seat ran its own state machine holding its OWN intent (`held`,
 * `latched`) and treated Go's payload as `confirmed`. The event handler ended
 * in pump(), which drives Go towards THIS seat's intent whenever the two
 * disagree. With one seat that is self-correction. With two:
 *
 *   A latches   -> Go says muted     -> both seats observe
 *   B observes: my latch is false, Go says true  -> B tells Go to UNMUTE
 *   Go says unmuted -> both seats observe
 *   A observes: my latch is true, Go says false  -> A tells Go to MUTE
 *   ... forever, at network speed
 *
 * The disagreement is not a race and does not need one. Seat B's `latched` is
 * false BY CONSTRUCTION the instant seat A latches, so the loop starts on the
 * first press and never converges.
 *
 * Echo suppression does not fix it — the diagnostic that reproduced this
 * measured 300 calls in 300 turns with echoes suppressed — because B fighting A
 * is not an echo. Nor does a debounce, which on a cough key would delay the
 * mute behind the sound it exists to catch. The fix is structural: a payload
 * that ARRIVED from Go may change what a seat displays and may never, by
 * itself, cause that seat to call.
 *
 * ============================ WHAT IS NOT HERE ==============================
 *
 * This file proves the loop is dead. It does NOT prove the multi-seat SEMANTICS
 * are right, because they are not yet: with the seats' intents still private, a
 * producer who toggles their own latch off can still lift a mute the
 * commentator is holding. The full model — Go owning a SET of named holds, with
 * the mute as their OR, so that no seat can withdraw another's — is designed
 * and not yet built. See MUTE-DESIGN.md section 1.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createCoughMute, MUTE_STATE } from './cough.js';

/**
 * shared stands up ONE Go and any number of seats onto it, which is the whole
 * point: the fake echoes every accepted call to every seat's observer, exactly
 * as app.go's event pump does.
 *
 * `calls` is the thing under test. A bounded count is the fix; an unbounded one
 * is the bug, and the count is what tells them apart without a timer.
 */
function shared() {
  const go = { muted: false, calls: [] };
  const seats = [];

  function apply(seat, want) {
    go.calls.push({ seat, want });
    if (go.calls.length > 200) {
      throw new Error(
        `runaway: ${go.calls.length} calls from ${seats.length} seats — the seats are ` +
          'fighting over one shared value, which is the oscillation this test exists to catch',
      );
    }
    go.muted = want;
    const p = { muted: go.muted, available: true, reason: '', by: '', byAddr: '' };
    // Echo to EVERY seat, including the caller, exactly as the event pump does.
    // The originating seat genuinely needs its own event: it is how that seat
    // learns the read-back truth.
    for (const s of seats) s.mute.observe({ ...p });
    return Promise.resolve({ ...p });
  }

  function addSeat(name) {
    const readouts = [];
    const mute = createCoughMute({
      apply: (want) => apply(name, want),
      onChange: (r) => readouts.push(r),
    });
    const seat = { name, mute, readouts, last: () => readouts[readouts.length - 1] };
    seats.push(seat);
    mute.adopt({ muted: go.muted, available: true, reason: '' });
    return seat;
  }

  const settle = async () => {
    for (let i = 0; i < 50; i++) await Promise.resolve();
  };

  return { go, addSeat, settle, seats };
}

test('one seat latching does not start a fight with the other', async () => {
  const s = shared();
  const desk = s.addSeat('desk');
  const browser = s.addSeat('browser');

  desk.mute.toggleLatch();
  await s.settle();

  // ONE call. The desk asked for a mute; the browser heard about it and said
  // nothing back. Before the fix this ran until the harness' own guard fired.
  assert.equal(s.go.calls.length, 1, `expected exactly one call, got ${JSON.stringify(s.go.calls)}`);
  assert.deepEqual(s.go.calls[0], { seat: 'desk', want: true });
  assert.equal(s.go.muted, true);

  // And BOTH seats show the truth, which is the half that was always right.
  assert.equal(desk.last().muted, true);
  assert.equal(browser.last().muted, true);
});

test('the seat that did not latch shows the mute without answering back', async () => {
  const s = shared();
  const desk = s.addSeat('desk');
  const browser = s.addSeat('browser');

  desk.mute.toggleLatch();
  await s.settle();
  const after = s.go.calls.length;

  // Let a long time pass, in event terms: more payloads arrive, as they would
  // from a reconcile or a second subscriber. Still nobody argues.
  for (let i = 0; i < 20; i++) {
    for (const seat of s.seats) {
      seat.mute.observe({ muted: true, available: true, reason: '' });
    }
  }
  await s.settle();

  assert.equal(s.go.calls.length, after, 'repeated payloads must never produce a call');
  assert.equal(browser.last().muted, true);
});

test('three seats are no worse than two', async () => {
  // The loop was quadratic in seats: every seat answered every other seat's
  // answer. If anything survives at two it is unmistakable at three.
  const s = shared();
  const a = s.addSeat('desk');
  s.addSeat('browser-1');
  s.addSeat('browser-2');

  a.mute.toggleLatch();
  await s.settle();
  a.mute.toggleLatch();
  await s.settle();

  assert.equal(
    s.go.calls.length,
    2,
    `a latch on and a latch off is two calls, got ${JSON.stringify(s.go.calls)}`,
  );
  assert.equal(s.go.muted, false);
});

test('a seat joining while the mute is already on does not lift it', async () => {
  // The late-joiner case, and the most dangerous shape of the old bug: a
  // producer opening a browser tab mid-cough would have put the commentator
  // straight back on air, because the new seat's own latch is false.
  const s = shared();
  const desk = s.addSeat('desk');
  desk.mute.toggleLatch();
  await s.settle();

  const late = s.addSeat('browser-joining-late');
  await s.settle();

  assert.equal(s.go.muted, true, 'the mute must survive somebody opening a browser tab');
  assert.equal(s.go.calls.length, 1, 'the late seat must not have called at all');
  assert.equal(late.last().muted, true, 'and it must show the mute it found');
});

test('push-to-mute still lands and still releases, with a second seat watching', async () => {
  const s = shared();
  const desk = s.addSeat('desk');
  s.addSeat('browser');

  desk.mute.press();
  await s.settle();
  assert.equal(s.go.muted, true);

  desk.mute.release();
  await s.settle();

  assert.equal(s.go.muted, false, 'a released cough key must not leave the microphone muted');
  assert.deepEqual(
    s.go.calls.map((c) => c.want),
    [true, false],
    'exactly one call per edge, from the seat whose key it was',
  );
  assert.equal(desk.last().state, MUTE_STATE.LIVE);
});

test('press and release inside one round trip still ends unmuted', async () => {
  // The case the original pump() was careful about and that must survive the
  // change of what it compares against: the operator lets go before the mute
  // call has come back. Getting this wrong strands the microphone muted.
  const s = shared();
  const desk = s.addSeat('desk');
  s.addSeat('browser');

  desk.mute.press();
  desk.mute.release(); // synchronously, while the first call is still in flight
  await s.settle();

  assert.equal(s.go.muted, false, 'the microphone must not be left muted');
  assert.equal(desk.last().state, MUTE_STATE.LIVE);
});
