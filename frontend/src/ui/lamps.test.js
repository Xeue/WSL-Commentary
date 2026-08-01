/**
 * Tests for deriveHonestLine, the withdrawn honest line.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THIS FILE EXISTS ===============================
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
 * package.json is frozen, there is no jsdom, and inventing a shim for it is a
 * larger piece of work than this file is. The derivation functions are pure and
 * need none.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { deriveHonestLine } from './lamps.js';

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
