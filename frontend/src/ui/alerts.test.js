/**
 * Tests for WHAT DESERVES ATTENTION mid-match — the severity rules, and the two
 * withdrawals they record.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * The side column fixed the layout half of the alert problem. It did not fix the
 * other half, and the other half is the one the operator actually complained
 * about: "causing concerns when everything is fine".
 *
 * An alert that fires when nothing is wrong trains an operator to ignore the
 * surface it appears on. That cost is not paid by the alert that cried wolf — it
 * is paid by the NEXT one, which is real, and which is now on a surface nobody
 * reads. These tests pin the two decisions that follow, so that either can be
 * reversed on purpose and neither can be reversed by accident.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  SEVERITY,
  STALE_STATUS_SEVERITY,
  NOTE_MONITOR_CODES,
  classifyMonitorError,
  normaliseSeverity,
  describeAttention,
} from './alerts.js';
import { MonitorErrorCode } from '../monitor/errors.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...p) => readFileSync(join(here, ...p), 'utf8');

// ---------------------------------------------------------------------------
// Withdrawal 1: the switcher-status banner
// ---------------------------------------------------------------------------

test('a stale status feed raises nothing at all', () => {
  assert.equal(
    STALE_STATUS_SEVERITY,
    null,
    'null means "raises nothing". It is a VALUE rather than the absence of a call site, so that ' +
      'the decision is something a test can hold and a change has to argue with',
  );
});

test('the staleness fact is still rendered — on the lamps, where it belongs', () => {
  // The banner was a FOURTH copy of something the lamp row already states three
  // ways. Removing the banner must not have removed the honesty, so this reads
  // the derivation and checks it still greys all three with the same words.
  const lamps = read('lamps.js');
  assert.match(
    lamps,
    /if \(status\.stale\) \{[\s\S]*?LEVEL\.GREY, text: 'STATUS UNAVAILABLE'[\s\S]*?switcher: l, video: l, audio: l/,
    'deriveStatusLamps must still grey SWITCHER SEES FEED, VIDEO and AUDIO and say STATUS ' +
      'UNAVAILABLE across all three',
  );
});

test('and the overall indicator can never read GOOD over it', () => {
  // The other half of the argument for deleting the banner: the single
  // indicator folds the same fact in, so nothing was lost by removing the
  // fourth copy. overall.test.js proves the behaviour; this pins that the
  // whitelist which could break it stays short.
  const overall = read('overall.js');
  const list = overall.match(/SETTLED_GREY_TEXTS = Object\.freeze\(\[([^\]]*)\]\)/);
  assert.ok(list, 'overall.js must still declare the settled-grey whitelist');
  assert.equal(
    /STATUS UNAVAILABLE|NO STATUS/.test(list[1]),
    false,
    'a stale status must never be a SETTLED grey: that is exactly the "never show stale green" ' +
      'failure, arriving through the summary instead of through the lamps',
  );
});

// ---------------------------------------------------------------------------
// Withdrawal 2: the deferred output-device switch
// ---------------------------------------------------------------------------

test('the deferred setSinkId is a NOTE, not an alert', () => {
  assert.deepEqual([...NOTE_MONITOR_CODES], [MonitorErrorCode.SINK_ID_DEFERRED]);
  assert.equal(classifyMonitorError({ code: MonitorErrorCode.SINK_ID_DEFERRED }), SEVERITY.NOTE);
});

test('its siblings are NOT notes, and AUTOPLAY_BLOCKED least of all', () => {
  // Blocked autoplay looks like the same class of thing — a browser policy
  // waiting on a gesture — and is not: it means the commentator currently HEARS
  // NOTHING, which is a fault at this desk even though the remedy is one click.
  for (const code of [
    MonitorErrorCode.AUTOPLAY_BLOCKED,
    MonitorErrorCode.SINK_ID_FAILED,
    MonitorErrorCode.SINK_ID_UNSUPPORTED,
    MonitorErrorCode.AUDIO_FAILED,
    MonitorErrorCode.VIDEO_FAILED,
  ]) {
    assert.equal(classifyMonitorError({ code }), SEVERITY.ALERT, `${code} must stay an alert`);
  }
});

test('audio.js no longer reports the deferral in the first place', () => {
  // The classification above is the belt; this is the braces. The best outcome
  // for a state that fixes itself on the next click anywhere is that nobody is
  // told about it at all.
  const audio = read('..', 'monitor', 'audio.js');
  const reports = [...audio.matchAll(/report\(\s*\n?\s*(?:new MonitorError\(|toMonitorError\()\s*\n?\s*MonitorErrorCode\.(\w+)/g)].map(
    (m) => m[1],
  );
  assert.ok(reports.length > 0, 'audio.js must still report the errors that ARE faults');
  assert.equal(
    reports.includes('SINK_ID_DEFERRED'),
    false,
    'the first deferral must be silent: the gesture listener re-applies it on the next click ' +
      'anywhere, so the operator would be reading an alarm about something that fixed itself ' +
      'while they read it',
  );
  assert.ok(
    reports.includes('SINK_ID_FAILED'),
    'and the retry failing must still be loud — that is a device that will never be permitted, ' +
      'and the return really is on the wrong output',
  );
});

// ---------------------------------------------------------------------------
// The vocabulary
// ---------------------------------------------------------------------------

test('an unclassified message over-reports rather than under-reports', () => {
  assert.equal(normaliseSeverity(undefined), SEVERITY.ALERT);
  assert.equal(normaliseSeverity(null), SEVERITY.ALERT);
  assert.equal(normaliseSeverity('warning'), SEVERITY.ALERT, 'there is no third level to fall into');
  assert.equal(normaliseSeverity(SEVERITY.NOTE), SEVERITY.NOTE);
  assert.equal(
    classifyMonitorError(undefined),
    SEVERITY.ALERT,
    'being told about something harmless is a nuisance; not being told about something real is ' +
      'the failure this surface exists to prevent',
  );
});

test('classifyMonitorError takes a bare code as well as an error', () => {
  assert.equal(classifyMonitorError(MonitorErrorCode.SINK_ID_DEFERRED), SEVERITY.NOTE);
  assert.equal(classifyMonitorError('SOMETHING_ELSE'), SEVERITY.ALERT);
});

// ---------------------------------------------------------------------------
// The attention marker
// ---------------------------------------------------------------------------

test('notes are not counted, because the count answers "is there anything for me to do"', () => {
  const a = describeAttention([
    { severity: SEVERITY.ALERT },
    { severity: SEVERITY.NOTE },
    { severity: SEVERITY.ALERT },
  ]);
  assert.equal(a.alerts, 2);
  assert.equal(a.notes, 1);
  assert.equal(a.attention, true);
  assert.equal(a.label, '2 alerts');
});

test('a column holding only notes shows no attention marker', () => {
  const a = describeAttention([{ severity: SEVERITY.NOTE }, { severity: SEVERITY.NOTE }]);
  assert.equal(a.alerts, 0);
  assert.equal(a.attention, false, 'an explanation is not something to do');
  assert.equal(a.label, 'No alerts');
});

test('the singular is not "1 alerts"', () => {
  assert.equal(describeAttention([{ severity: SEVERITY.ALERT }]).label, '1 alert');
});

test('an unclassified entry counts as an alert, and nothing throws on rubbish', () => {
  assert.equal(describeAttention([{}]).alerts, 1);
  assert.equal(describeAttention([null]).alerts, 1);
  assert.equal(describeAttention([]).attention, false);
  assert.equal(describeAttention(null).attention, false);
  assert.equal(describeAttention(undefined).label, 'No alerts');
});
