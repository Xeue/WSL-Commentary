/**
 * Tests for the event auto-select / picker rule (events.js).
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= WHAT THIS PROVES ===================================
 *
 * The operator's request in one line: "by default for the event to be auto
 * selected when there is only 1" — and a picker only when there is a genuine
 * choice. chooseEvent is the whole of that decision, kept pure so it can be
 * driven for real here while settings.js does nothing but turn its answer into a
 * hidden field, a note or a <select>.
 *
 * The cases that matter are the boundaries: nothing (fall back to the URL), one
 * (auto-select, NO picker), several (a picker whose default is the current id if
 * present else the first), and the operator's real event id classifying the way
 * a single-event instance must.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import { chooseEvent } from './events.js';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');

/** Strips comments, so prose ABOUT a call cannot satisfy a guard about the call. */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

// The operator's real event, verbatim from the live instance and the fake
// backend's default — the one a single-event instance must auto-select.
const REAL = { id: 'dl9-5p5ah0bd-empd', name: 'MatchT', status: 'Running' };

test('no events: keep the current (URL-derived) id and offer no picker', () => {
  const choice = chooseEvent([], 'dl9-from-the-url');
  assert.equal(choice.count, 0);
  assert.equal(choice.autoSelected, false);
  assert.equal(choice.needsPicker, false);
  assert.equal(choice.selectedId, 'dl9-from-the-url', 'the URL-derived id must stand');
  assert.equal(choice.selectedName, '');
});

test('a non-array or missing event list is treated as no events', () => {
  for (const events of [undefined, null, 'nope', 42, {}]) {
    const choice = chooseEvent(events, 'url-id');
    assert.equal(choice.count, 0, `${JSON.stringify(events)} must be treated as empty`);
    assert.equal(choice.needsPicker, false);
    assert.equal(choice.selectedId, 'url-id');
  }
});

test('exactly one event auto-selects it, with NO picker — the operator default', () => {
  const choice = chooseEvent([REAL], '');
  assert.equal(choice.count, 1);
  assert.equal(choice.autoSelected, true, 'a lone event must be auto-selected');
  assert.equal(choice.needsPicker, false, 'a picker with one option is a question with one answer');
  assert.equal(choice.selectedId, REAL.id);
  assert.equal(choice.selectedName, 'MatchT');
});

test('one event auto-selects even when a DIFFERENT id came from the URL', () => {
  // The events API supersedes the URL: a single real event wins over whatever
  // stale id the pasted address carried.
  const choice = chooseEvent([REAL], 'a-different-stale-id');
  assert.equal(choice.autoSelected, true);
  assert.equal(choice.selectedId, REAL.id, 'the sole event supersedes the URL-derived id');
});

test('several events need a picker, defaulting to the current id when it is one of them', () => {
  const events = [
    { id: 'a', name: 'Alpha', status: 'Running' },
    REAL,
    { id: 'z', name: 'Zulu', status: 'Idle' },
  ];
  const choice = chooseEvent(events, REAL.id);
  assert.equal(choice.count, 3);
  assert.equal(choice.autoSelected, false);
  assert.equal(choice.needsPicker, true);
  assert.equal(choice.selectedId, REAL.id, 're-opening must not move a choice already in the list');
  assert.equal(choice.selectedName, 'MatchT');
});

test('several events with the current id ABSENT default to the first', () => {
  const events = [
    { id: 'a', name: 'Alpha', status: 'Running' },
    { id: 'b', name: 'Bravo', status: 'Idle' },
  ];
  const choice = chooseEvent(events, 'not-in-the-list');
  assert.equal(choice.needsPicker, true);
  assert.equal(choice.selectedId, 'a', 'an absent current id falls back to the first event');
  assert.equal(choice.selectedName, 'Alpha');
});

test('several events with an empty current id default to the first', () => {
  const events = [
    { id: 'a', name: 'Alpha', status: 'Running' },
    { id: 'b', name: 'Bravo', status: 'Idle' },
  ];
  const choice = chooseEvent(events, '');
  assert.equal(choice.needsPicker, true);
  assert.equal(choice.selectedId, 'a');
});

test('id-less entries are dropped, so they never become the auto-selected one', () => {
  // The Go side drops these already; the rule repeats it because an entry with
  // no id cannot be fed to the KVS calls, so it must never be what "exactly one
  // event" selects. One id-less entry beside one real event is ONE usable event.
  const choice = chooseEvent([{ id: '', name: 'ghost', status: '' }, REAL], '');
  assert.equal(choice.count, 1, 'the id-less entry does not count');
  assert.equal(choice.autoSelected, true);
  assert.equal(choice.selectedId, REAL.id);
});

test('two entries where one is id-less collapse to an auto-select, not a picker', () => {
  const choice = chooseEvent([REAL, { id: '', name: 'ghost' }], 'url-id');
  assert.equal(choice.needsPicker, false, 'one usable event is not a choice');
  assert.equal(choice.autoSelected, true);
});

test('a non-string current id is tolerated as no current id', () => {
  const choice = chooseEvent(
    [
      { id: 'a', name: 'Alpha' },
      { id: 'b', name: 'Bravo' },
    ],
    undefined,
  );
  assert.equal(choice.needsPicker, true);
  assert.equal(choice.selectedId, 'a', 'a missing current id defaults to the first');
});

// ---------------------------------------------------------------------------
// The wiring: backend.listEvents and the Settings screen
// ---------------------------------------------------------------------------

test('backend.js binds ListEvents through callGoBound, with the fake default of one event', () => {
  const js = ui('backend.js');
  assert.match(js, /export async function listEvents\(\)/, 'backend.js must export listEvents');
  assert.match(
    js,
    /callGoBound\(EVENTS_METHOD\)/,
    'listEvents must go through callGoBound so an older build without it degrades to the URL',
  );
  // The fake default is ONE event, so the dev loop shows the auto-select path.
  assert.match(js, /const EVENTS_METHOD = 'ListEvents'/, 'the bound name must be App.ListEvents');
  assert.match(
    js,
    /let fakeEvents = \[\{ id: 'dl9-5p5ah0bd-empd', name: 'MatchT', status: 'Running' \}\]/,
    'the fake must default to exactly one event, so `npm run dev` shows the auto-select path',
  );
});

test('settings.js runs chooseEvent over listEvents and writes the SAME hidden eventId field', () => {
  const js = codeOnly(ui('settings.js'));
  assert.match(js, /await backend\.listEvents\(\)/, 'settings.js must ask the backend for the events');
  assert.match(js, /chooseEvent\(/, 'and run the pure rule over them');
  // The picker and the auto-select both write fields.eventId — the same hidden
  // field collectConfig already reads — so the value's SOURCE changes and
  // nothing else does.
  assert.match(
    js,
    /fields\.eventId\.input\.value = choice\.selectedId/,
    'both paths must write the hidden eventId, or the picked event is never saved',
  );
});

test('the event listing failure cannot break the Settings screen', () => {
  // Not signed in, no host, an older build, a network error: all must leave the
  // URL-derived id untouched. So the listEvents call is wrapped in try/catch.
  const js = codeOnly(ui('settings.js'));
  const refresh = js.slice(js.indexOf('async function refreshEvents()'), js.indexOf('// --- SRT output'));
  assert.ok(refresh.length > 0, 'settings.js must have refreshEvents');
  assert.match(refresh, /try \{/, 'the events call must be guarded');
  assert.match(refresh, /catch \(err\)/, 'a failure must be caught, never thrown out of open()');
});

test('the event picker did not change collectConfig — eventId is still collected from the hidden field', () => {
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  assert.match(
    collect,
    /eventId: fields\.eventId\.input\.value\.trim\(\)/,
    'collectConfig must still read eventId from the one hidden field, unchanged',
  );
  // The picker is a SOURCE for that field, never a second collected key.
  assert.ok(!/eventSelect/.test(collect), 'collectConfig must not read the picker directly');
});
