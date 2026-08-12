/**
 * Tests for the error log model and the SRT backoff episode rule.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * The two properties that matter most, because each was the actual failure the
 * module exists to prevent:
 *
 * 1. A retry loop counts as ONE row, and raises the banner ONCE per episode.
 *    Raised per retry, the banner reappears seconds after every dismissal for
 *    as long as the far end is down — which for a wrong port is the whole
 *    match.
 *
 * 2. Distinct errors are all kept. The single-message banner showed whichever
 *    error came last and destroyed the one before it.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  createErrorLog,
  createBackoffEpisode,
  describeEntry,
  formatErrorTime,
  ERROR_LOG_LIMIT,
} from './errorlog.js';

/** A clock the tests advance by hand. */
function fakeClock(startMs = 0) {
  let t = startMs;
  return {
    now: () => new Date(t),
    advance(ms) {
      t += ms;
    },
  };
}

test('distinct errors each get a row, newest first', () => {
  const log = createErrorLog(fakeClock().now);
  log.record('the return failed');
  log.record('the mixer refused a write');

  assert.equal(log.size, 2);
  assert.equal(log.entries[0].message, 'the mixer refused a write');
  assert.equal(log.entries[1].message, 'the return failed');
});

test('a repeat of the newest entry increments it instead of adding a row', () => {
  const clock = fakeClock(1_000_000);
  const log = createErrorLog(clock.now);

  log.record('SRT return: connection refused');
  clock.advance(30_000);
  log.record('SRT return: connection refused');
  clock.advance(30_000);
  log.record('SRT return: connection refused');

  assert.equal(log.size, 1, 'three identical failures are one row');
  const e = log.entries[0];
  assert.equal(e.count, 3);
  assert.equal(e.lastAt.getTime() - e.firstAt.getTime(), 60_000, 'firstAt keeps the start, lastAt tracks the latest');
});

test('alternating errors produce alternating rows — dedup is against the newest only', () => {
  const log = createErrorLog(fakeClock().now);
  log.record('A');
  log.record('B');
  log.record('A');

  assert.equal(log.size, 3, 'A recurring AFTER B is a new event, not a repeat of the old row');
  assert.deepEqual(
    log.entries.map((e) => e.message),
    ['A', 'B', 'A'],
  );
});

test('the log is capped and the oldest fall off', () => {
  const log = createErrorLog(fakeClock().now);
  for (let i = 0; i < ERROR_LOG_LIMIT + 7; i++) log.record(`error number ${i}`);

  assert.equal(log.size, ERROR_LOG_LIMIT);
  assert.equal(log.entries[0].message, `error number ${ERROR_LOG_LIMIT + 6}`, 'newest kept');
  assert.equal(
    log.entries[ERROR_LOG_LIMIT - 1].message,
    'error number 7',
    'oldest seven gone',
  );
});

test('clear empties the history', () => {
  const log = createErrorLog(fakeClock().now);
  log.record('x');
  log.clear();
  assert.equal(log.size, 0);
});

test('record coerces non-strings, so a thrown object cannot break the banner', () => {
  const log = createErrorLog(fakeClock().now);
  log.record(undefined);
  log.record(42);
  assert.deepEqual(
    log.entries.map((e) => e.message),
    ['42', 'undefined'],
  );
});

test('describeEntry shows the last time, the message, and a count only when repeated', () => {
  const at = new Date(2026, 7, 12, 14, 3, 9); // local 14:03:09
  const once = { message: 'the return failed', count: 1, firstAt: at, lastAt: at };
  const thrice = { message: 'the return failed', count: 3, firstAt: at, lastAt: at };

  assert.equal(describeEntry(once), '14:03:09  the return failed');
  assert.equal(describeEntry(thrice), '14:03:09  the return failed (×3)');
});

test('formatErrorTime pads to two digits', () => {
  assert.equal(formatErrorTime(new Date(2026, 0, 1, 9, 5, 3)), '09:05:03');
});

// ---------------------------------------------------------------------------
// The backoff episode.
// ---------------------------------------------------------------------------

test('an unbroken failing run raises once, cycles silently, and clears on recovery', () => {
  const ep = createBackoffEpisode();

  assert.equal(ep.track('connecting'), null, 'first dial: nothing to say');
  assert.equal(ep.track('backoff'), 'raise', 'first failure raises the banner');
  assert.equal(ep.track('connecting'), null, 'the retry is part of the same episode');
  assert.equal(ep.track('backoff'), null, 'so is its failure — the banner must NOT be re-raised');
  assert.equal(ep.track('backoff'), null);
  assert.equal(ep.track('connecting'), null);
  assert.equal(ep.track('showing'), 'clear', 'the picture is back; the banner comes down');
});

test('a second failure after recovery is a new episode and raises again', () => {
  const ep = createBackoffEpisode();
  ep.track('backoff'); // raise
  ep.track('showing'); // clear

  assert.equal(ep.track('backoff'), 'raise', 'a fresh failure after recovery deserves a fresh banner');
});

test('stopping the receiver mid-episode clears rather than leaving a stale banner', () => {
  const ep = createBackoffEpisode();
  ep.track('backoff');
  assert.equal(ep.track('stopped'), 'clear', 'nobody wants the picture; the complaint is moot');
});

test('the receiver going away entirely (null state) clears an open episode', () => {
  const ep = createBackoffEpisode();
  ep.track('backoff');
  assert.equal(ep.track(null), 'clear');
});

test('normal healthy states never raise and never clear when there is no episode', () => {
  const ep = createBackoffEpisode();
  assert.equal(ep.track('stopped'), null);
  assert.equal(ep.track('connecting'), null);
  assert.equal(ep.track('showing'), null);
  assert.equal(ep.track(''), null);
});
