/**
 * Tests for the mixer drawer's pure model.
 *
 * Owner: WP-M4. Node's built-in runner, matching the convention in
 * src/ui/tile.test.js — package.json is frozen, so there is no test framework:
 *
 *   cd frontend && node --test "src/ui/mixer/*.test.js"
 *
 * Every fixture below is shaped like the captured live frame
 * (internal/m2lx/testdata/switcher_status-live-2026-07-31.json): cam22-1 is
 * "CLAUDE-COMMS", routed to the default ["master","aux1","aux2"], i.e. already
 * in the clean feed.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { ALL_BUSES, CLEAN_FEED_BUS, busLabel } from './contract.js';
import {
  METER_AMBER_DB,
  METER_CEIL_DB,
  METER_FLOOR_DB,
  METER_RED_DB,
  METER_STAGES,
  MUTE_FADER_DB,
  buildMatrixModel,
  busListText,
  changeSeverity,
  compareSnapshots,
  createWriteGate,
  crosspointCommand,
  describeCrosspointChange,
  diffHeadline,
  faderText,
  formatDb,
  isDigitalSilence,
  isDisplayedStrip,
  isSecondChannelStrip,
  meterFraction,
  meterPercent,
  meterStageFills,
  meterStageWidths,
  meterZone,
  renderedHasBus,
  routingAfterToggle,
  routingCommand,
  routingSeverity,
  sortDiffs,
  stripLabel,
  viewFreshness,
} from './model.js';

/* ------------------------------------------------------------------ */
/* Fixtures                                                            */
/* ------------------------------------------------------------------ */

function strip(over = {}) {
  return {
    name: 'cam22-1',
    input: 'cam22',
    displayName: 'CLAUDE-COMMS',
    muted: false,
    follow: false,
    followSources: ['cam22'],
    subChMode: 'ST_W',
    outputs: ['master', 'aux1', 'aux2'],
    pflOutputs: [],
    level: [-87.7, -87.65],
    peakHold: [-87.7, -87.65],
    metered: true,
    fader: [0, 0],
    faderEnabled: [false, false],
    ...over,
  };
}

function bus(over = {}) {
  return {
    name: 'master',
    muted: false,
    channelCount: 2,
    level: [-100, -100],
    peakHold: [-100, -100],
    metered: true,
    fader: 1,
    faderPresent: true,
    ...over,
  };
}

function snapshot(strips, buses = [bus(), bus({ name: 'aux1' })]) {
  return { strips, buses, takenAt: '2026-07-31T18:00:00Z' };
}

/* ------------------------------------------------------------------ */
/* Meter scale                                                         */
/* ------------------------------------------------------------------ */

test('meterFraction maps dBFS onto the drawn meter, clamping at both ends', () => {
  // The point of the scale: -27 dBFS is real programme (cam1-1 in the live
  // frame) and must be halfway up the bar, not a sliver above the floor.
  const cases = [
    { db: 0, want: 1 },
    { db: METER_CEIL_DB, want: 1 },
    { db: 5, want: 1, why: 'above full scale clamps rather than overflowing the bar' },
    { db: -6, want: 0.9 },
    { db: -20, want: 2 / 3 },
    { db: -30, want: 0.5 },
    { db: -60, want: 0 },
    { db: METER_FLOOR_DB, want: 0 },
    { db: -87.7, want: 0, why: 'the live cam22-1 reading, below the useful range' },
    { db: -100, want: 0, why: 'digital silence' },
    { db: NaN, want: 0 },
    { db: undefined, want: 0 },
    { db: 'loud', want: 0 },
  ];
  for (const c of cases) {
    assert.ok(
      Math.abs(meterFraction(c.db) - c.want) < 1e-9,
      `meterFraction(${String(c.db)}) = ${meterFraction(c.db)}, want ${c.want}${c.why ? ` (${c.why})` : ''}`,
    );
  }
});

test('meterFraction is linear in dB, not in amplitude', () => {
  // Halfway up the bar is halfway down the dB range. An amplitude-linear meter
  // would put -30 dBFS at 3% and bury everything this drawer exists to show.
  const mid = (METER_FLOOR_DB + METER_CEIL_DB) / 2;
  assert.equal(meterFraction(mid), 0.5);
  assert.equal(meterFraction(-15), 0.75);
  assert.equal(meterFraction(-45), 0.25);
});

test('meterPercent is a rounded percentage safe to write into a CSS width', () => {
  assert.equal(meterPercent(-30), 50);
  assert.equal(meterPercent(-100), 0);
  assert.equal(meterPercent(0), 100);
  // -87.65 is a real reading from the live frame; it must not produce a
  // seventeen-digit float rewritten into style every second.
  assert.equal(meterPercent(-87.65), 0);
  assert.equal(String(meterPercent(-19.999)).length <= 5, true);
});

test('isDigitalSilence recognises both values the mixer reports for silence', () => {
  // MEASURED: strips read -100.0, buses read -99.99999237060547.
  assert.equal(isDigitalSilence(-100), true);
  assert.equal(isDigitalSilence(-99.99999237060547), true);
  assert.equal(isDigitalSilence(-87.7), false, 'a quiet signal is not silence');
  assert.equal(isDigitalSilence(undefined), true, 'no reading is not a signal');
});

test('meterZone classifies readings in words, not colours', () => {
  const cases = [
    [-100, 'silence'],
    [-99.99999237060547, 'silence'],
    [-87.7, 'low'],
    [-30, 'low'],
    [-24, 'nominal'],
    [-10, 'nominal'],
    [-6, 'hot'],
    [-2, 'hot'],
    [-1, 'over'],
    [0, 'over'],
    [1, 'over'],
  ];
  for (const [db, want] of cases) {
    assert.equal(meterZone(db), want, `meterZone(${db})`);
  }
});

test('the drawn bar is staged green / amber / red at -18 and -6', () => {
  assert.equal(METER_AMBER_DB, -18);
  assert.equal(METER_RED_DB, -6);
  assert.deepEqual(
    METER_STAGES.map((s) => s.zone),
    ['green', 'amber', 'red'],
    'bottom stage first, so index order is bar order',
  );
  // The stage widths are the boundaries expressed as a share of the -60..0
  // scale. They are computed, not written down, so this is what pins them.
  assert.deepEqual(meterStageWidths(), [70, 20, 10]);
  assert.equal(
    meterStageWidths().reduce((a, b) => a + b, 0),
    100,
    'the three stages are the whole bar and nothing else',
  );
});

test('a peak shows every stage it has passed THROUGH, not one flat colour', () => {
  // This is the whole point of staging it: a real meter at -3 dBFS reads green,
  // then amber, then red along its length. Colouring the bar by the zone of the
  // peak alone would paint the entire bar red, which says nothing about level.
  assert.deepEqual(meterStageFills(-3), [100, 100, 50], '-3 dBFS: green and amber full, half the red');
  assert.deepEqual(meterStageFills(-6), [100, 100, 0], 'exactly at the red boundary lights no red');
  assert.deepEqual(meterStageFills(-12), [100, 50, 0], 'halfway through amber');
  assert.deepEqual(meterStageFills(-18), [100, 0, 0], 'exactly at the amber boundary lights no amber');
  assert.deepEqual(meterStageFills(0), [100, 100, 100], 'full scale fills the bar');
  assert.deepEqual(meterStageFills(-39), [50, 0, 0], 'halfway through green');
});

test('nothing is drawn for silence, the floor, or a reading that is not a number', () => {
  // A bar that draws something for "no reading" is a level an operator would
  // act on. Absence is reported through MixerStrip.metered, never inferred.
  for (const db of [-100, -99.99999237060547, METER_FLOOR_DB, -87.7, null, undefined, NaN, 'loud']) {
    assert.deepEqual(meterStageFills(db), [0, 0, 0], `meterStageFills(${String(db)})`);
  }
});

test('a stage fill is a percentage safe to write into a CSS width', () => {
  for (const db of [-19.999, -5.317, -0.0001, -59.99]) {
    for (const pct of meterStageFills(db)) {
      assert.ok(pct >= 0 && pct <= 100, `${pct} is within the bar`);
      assert.ok(String(pct).length <= 5, `${pct} is not a seventeen-digit float rewritten every second`);
    }
  }
});

test('formatDb turns the two magic values into words', () => {
  const cases = [
    [MUTE_FADER_DB, 'mute'],
    [-144, 'mute'],
    [-200, 'mute'],
    [-100, 'silent'],
    // Math.round is half-up towards positive infinity, so a negative exact
    // half rounds towards zero. It is a display rounding of 0.05 dB.
    [-6.25, '-6.2 dB'],
    [-6.26, '-6.3 dB'],
    [0, '0.0 dB'],
    [3, '+3.0 dB'],
    [undefined, '--'],
    [NaN, '--'],
  ];
  for (const [db, want] of cases) {
    assert.equal(formatDb(db), want, `formatDb(${String(db)})`);
  }
  assert.equal(formatDb(-100, { silenceAsWord: false }), '-100.0 dB');
});

/* ------------------------------------------------------------------ */
/* Snapshot -> matrix model                                            */
/* ------------------------------------------------------------------ */

test('buildMatrixModel says it has no data rather than showing an empty mixer', () => {
  // "No state" and "nothing is routed" must never look the same: one is a
  // broken connection, the other is a claim about what is on air.
  for (const bad of [null, undefined, {}, { strips: 'nope' }]) {
    const m = buildMatrixModel(bad);
    assert.equal(m.hasData, false, `buildMatrixModel(${JSON.stringify(bad)}).hasData`);
    assert.deepEqual(m.rows, []);
    assert.deepEqual(m.cleanFeed, []);
  }
  assert.equal(buildMatrixModel(snapshot([strip()])).hasData, true);
});

test('buildMatrixModel builds one row per strip with a cell per bus, in order', () => {
  const m = buildMatrixModel(snapshot([strip(), strip({ name: 'cam23-1', input: 'cam23', displayName: '' })]));
  assert.equal(m.rows.length, 2);
  for (const row of m.rows) {
    assert.deepEqual(
      row.cells.map((c) => c.bus),
      [...ALL_BUSES],
      'columns must not reorder themselves between 1 Hz updates',
    );
  }
});

test('buildMatrixModel marks the default routing as being in the clean feed', () => {
  // The whole reason the drawer exists: ["master","aux1","aux2"] is the default
  // and aux1 is the CLN output.
  const m = buildMatrixModel(snapshot([strip()]));
  const row = m.rows[0];
  assert.equal(row.inCleanFeed, true);
  assert.equal(row.audibleOnCleanFeed, true, 'unmuted and in the clean feed');
  assert.deepEqual(m.cleanFeed.map((r) => r.name), ['cam22-1']);
  const cell = row.cells.find((c) => c.bus === CLEAN_FEED_BUS);
  assert.equal(cell.routed, true);
  assert.equal(cell.cleanFeed, true);
  assert.match(cell.label, /clean feed/i, 'the column must never render the raw bus name');
});

test('buildMatrixModel separates muted from audible in the clean feed', () => {
  const m = buildMatrixModel(snapshot([strip({ muted: true })]));
  assert.equal(m.rows[0].inCleanFeed, true);
  assert.equal(m.rows[0].audibleOnCleanFeed, false);
});

/* ------------------------------------------------------------------ */
/* One row per input: the '-2' strips                                  */
/* ------------------------------------------------------------------ */

test('isSecondChannelStrip matches the name, not a channel count or a position', () => {
  // The rule is the trailing '-2' and nothing else. Anything inferred from the
  // shape of the list hides the wrong row the first time an input arrives with
  // one strip or three.
  for (const name of ['cam22-2', 'cam1-2', 'MIC 1-2', 'replay1-2', 'vtr2-2']) {
    assert.equal(isSecondChannelStrip(name), true, `${name} is a second-channel strip`);
  }
  for (const name of ['cam22-1', 'cam2-1', 'MIC 1-1', 'cam2', 'cam12-21', 'cam2-20', '', undefined, null, 2]) {
    assert.equal(isSecondChannelStrip(name), false, `${String(name)} is not a second-channel strip`);
  }
});

test('a "-2" strip is hidden, but never one that is AUDIBLE on the clean feed', () => {
  // The exception is the whole safety of the change: hiding a row whose audio
  // is reaching the client's clean feed is the single failure this drawer
  // exists to prevent, and it would also make the header count a lie.
  const cases = [
    { name: 'cam22-1', inCleanFeed: true, muted: false, want: true, why: 'a -1 strip is always shown' },
    { name: 'cam22-1', inCleanFeed: false, muted: true, want: true, why: 'even muted and unrouted' },
    { name: 'cam22-2', inCleanFeed: false, muted: false, want: false, why: 'not in the clean feed: hidden' },
    { name: 'cam22-2', inCleanFeed: true, muted: true, want: false, why: 'routed but muted: inaudible, hidden' },
    { name: 'cam22-2', inCleanFeed: true, muted: false, want: true, why: 'ROUTED AND UNMUTED: it must be visible' },
  ];
  for (const c of cases) {
    assert.equal(isDisplayedStrip(c), c.want, `${c.name} (${c.why})`);
  }
});

test('buildMatrixModel flags the hidden rows without removing them from anything', () => {
  const m = buildMatrixModel(
    snapshot([
      strip({ name: 'cam22-1' }),
      strip({ name: 'cam22-2', muted: true }),
      strip({ name: 'MIC 1-2', muted: true }),
    ]),
  );
  assert.equal(m.rows.length, 3, 'every strip is still in the model; this is a display flag only');
  assert.deepEqual(
    m.rows.map((r) => r.displayed),
    [true, false, false],
  );
  // cleanFeed is the model's answer to "what is in the clean feed" and stays
  // complete. cleanFeedDisplayed is what the operator can see of it, and it is
  // what the drawer's header count is drawn from.
  assert.deepEqual(m.cleanFeed.map((r) => r.name), ['cam22-1', 'cam22-2', 'MIC 1-2']);
  assert.deepEqual(m.cleanFeedDisplayed.map((r) => r.name), ['cam22-1']);
});

test('a "-2" strip that is audible on the clean feed stays in both lists', () => {
  const m = buildMatrixModel(snapshot([strip({ name: 'cam22-2', muted: false })]));
  assert.equal(m.rows[0].displayed, true);
  assert.deepEqual(m.cleanFeedDisplayed.map((r) => r.name), ['cam22-2']);
  // The invariant that makes the count safe: anything counted as unmuted in the
  // clean feed is displayed, so hiding rows can never shrink that number.
  const audible = m.cleanFeed.filter((r) => !r.muted);
  assert.ok(audible.every((r) => r.displayed), 'nothing audible on the clean feed is ever hidden');
});

test('buildMatrixModel flags aux2 as reaching nothing', () => {
  const m = buildMatrixModel(snapshot([strip()]));
  const aux2 = m.rows[0].cells.find((c) => c.bus === 'aux2');
  assert.equal(aux2.routed, true, 'the default routes to aux2');
  assert.equal(aux2.noEgress, true, 'and nothing receives aux2');
});

test('buildMatrixModel keeps the row order and reports a structure key', () => {
  const m = buildMatrixModel(snapshot([strip({ name: 'a-1' }), strip({ name: 'b-1' })]));
  assert.deepEqual(m.rows.map((r) => r.name), ['a-1', 'b-1']);
  const same = buildMatrixModel(snapshot([strip({ name: 'a-1' }), strip({ name: 'b-1' })]));
  assert.equal(m.structureKey, same.structureKey, 'the same strips must not force a rebuild');
  const different = buildMatrixModel(snapshot([strip({ name: 'a-1' })]));
  assert.notEqual(m.structureKey, different.structureKey);
});

test('the structure key survives strip names containing the separator', () => {
  // 'MIC 1-1' is a real strip name. A key built by joining on a space would
  // call these two different mixers identical and skip the rebuild, leaving
  // the operator looking at a matrix labelled with the wrong strips.
  const a = buildMatrixModel(snapshot([strip({ name: 'MIC 1' }), strip({ name: '1-1' })]));
  const b = buildMatrixModel(snapshot([strip({ name: 'MIC' }), strip({ name: '1 1-1' })]));
  assert.notEqual(a.structureKey, b.structureKey);
});

test('buildMatrixModel treats a missing meter as absent, not as full scale', () => {
  const m = buildMatrixModel(snapshot([strip({ metered: false, peakHold: undefined })]));
  assert.equal(m.rows[0].metered, false);
  assert.deepEqual(m.rows[0].peakHold, [METER_FLOOR_DB, METER_FLOOR_DB], 'the default 0.0 must never survive');
});

test('buildMatrixModel drops nameless strips instead of rendering a blank row', () => {
  const m = buildMatrixModel(snapshot([strip(), { name: '' }, null]));
  assert.equal(m.rows.length, 1);
});

test('buildMatrixModel reports every bus even when the snapshot omits it', () => {
  const m = buildMatrixModel(snapshot([strip()], [bus({ name: 'aux1', muted: true })]));
  assert.deepEqual(m.buses.map((b) => b.name), [...ALL_BUSES]);
  const aux1 = m.buses.find((b) => b.name === 'aux1');
  assert.equal(aux1.known, true);
  assert.equal(aux1.muted, true);
  assert.equal(aux1.cleanFeed, true);
  const mon1 = m.buses.find((b) => b.name === 'mon1');
  assert.equal(mon1.known, false, 'a bus with no state must say so, not read as unmuted');
});

test('stripLabel falls back to the input, never to blank', () => {
  const cases = [
    [strip(), 'CLAUDE-COMMS'],
    [strip({ displayName: '' }), 'cam22'],
    [strip({ displayName: '', input: '' }), 'cam22-1'],
    [{}, '(unnamed strip)'],
    [null, '(unknown strip)'],
  ];
  for (const [s, want] of cases) assert.equal(stripLabel(s), want);
});

test('faderText shows a fader that is not in circuit as off, not as a gain', () => {
  // MEASURED: every ch_fader in the live frame is enabled=[false,false],
  // gain=[0,0]. Rendering "0.0 dB" would claim a unity gain that is not applied.
  assert.equal(faderText(strip()), 'off');
  assert.equal(faderText(strip({ faderEnabled: [true, true] })), '0.0 dB');
  assert.equal(faderText(strip({ faderEnabled: [true, true], fader: [-6, -3] })), 'L -6.0 dB / R -3.0 dB');
  assert.equal(faderText(strip({ faderEnabled: [true, false], fader: [-6, -6] })), 'L -6.0 dB / R off');
  assert.equal(faderText(strip({ faderEnabled: [true, true], fader: [MUTE_FADER_DB, MUTE_FADER_DB] })), 'mute');
});

/* ------------------------------------------------------------------ */
/* Crosspoint edits                                                    */
/* ------------------------------------------------------------------ */

test('routingAfterToggle returns the complete resulting set, because set_routing replaces', () => {
  const cases = [
    { from: ['master', 'aux1', 'aux2'], bus: 'aux1', routed: false, want: ['master', 'aux2'] },
    { from: ['master', 'aux2'], bus: 'aux1', routed: true, want: ['master', 'aux1', 'aux2'] },
    { from: ['master'], bus: 'master', routed: true, want: ['master'], why: 'already there, idempotent' },
    { from: ['master'], bus: 'aux1', routed: false, want: ['master'], why: 'not there, idempotent' },
    { from: ['master'], bus: 'master', routed: false, want: [], why: 'emptying is allowed and explicit' },
    { from: ['mon2', 'master'], bus: 'mon1', routed: true, want: ['master', 'mon1', 'mon2'] },
  ];
  for (const c of cases) {
    assert.deepEqual(
      routingAfterToggle(c.from, c.bus, c.routed),
      c.want,
      `${JSON.stringify(c.from)} ${c.bus}=${c.routed}${c.why ? ` (${c.why})` : ''}`,
    );
  }
});

test('routingAfterToggle keeps a bus this build has never heard of', () => {
  // Dropping it would be a write the operator did not ask for, on a bus that
  // is still carrying audio.
  assert.deepEqual(routingAfterToggle(['master', 'aux9'], 'aux1', true), ['master', 'aux1', 'aux9']);
});

test('routingAfterToggle refuses a nameless bus rather than guessing', () => {
  assert.throws(() => routingAfterToggle(['master'], '', true), /needs a bus name/);
});

test('crosspointCommand builds set_routing with the strip name under the "input" key', () => {
  const row = buildMatrixModel(snapshot([strip()])).rows[0];
  assert.deepEqual(crosspointCommand(row, 'aux1', false), {
    kind: 'setRouting',
    args: { matrix: 'output', input: 'cam22-1', outputs: ['master', 'aux2'] },
  });
});

test('crosspointCommand refuses a strip with no name', () => {
  assert.throws(() => crosspointCommand({ outputs: [] }, 'aux1', true), /needs a strip/);
  assert.throws(() => routingCommand('cam22-1', undefined), /complete resulting output set/);
});

test('describeCrosspointChange names the clean feed in words', () => {
  const row = buildMatrixModel(snapshot([strip()])).rows[0];
  assert.match(describeCrosspointChange(row, 'aux1', true), /CLAUDE-COMMS INTO the clean feed/);
  assert.match(describeCrosspointChange(row, 'aux1', false), /OUT of the clean feed/);
  assert.match(describeCrosspointChange(row, 'mon1', true), /CLAUDE-COMMS into mon1/);
});

test('changeSeverity ranks the buses that leave the building above the rest', () => {
  assert.equal(changeSeverity('aux1'), 'critical');
  assert.equal(changeSeverity('master'), 'critical');
  assert.equal(changeSeverity('aux2'), 'warn');
  assert.equal(changeSeverity('mon4'), 'warn');
});

/* ------------------------------------------------------------------ */
/* The write gate                                                      */
/* ------------------------------------------------------------------ */

function gateHarness(armed) {
  const sent = [];
  const errors = [];
  let isArmed = armed;
  let fresh = true;
  const gate = createWriteGate({
    sendCommands: async (cmds) => {
      sent.push(cmds);
    },
    isArmed: () => isArmed,
    viewIsFresh: () => (fresh ? { fresh: true, text: 'live' } : { fresh: false, text: 'STALE - no update for 40s' }),
    onError: (err, ctx) => errors.push([err.message, ctx]),
  });
  return {
    gate,
    sent,
    errors,
    arm: (v) => { isArmed = v; },
    setFresh: (v) => { fresh = v; },
  };
}

const ONE_COMMAND = [{ kind: 'setRouting', args: { matrix: 'output', input: 'cam22-1', outputs: ['master'] } }];

test('the write gate blocks every write while disarmed', async () => {
  const h = gateHarness(false);
  const r = await h.gate.submit(ONE_COMMAND, 'operator pressed Apply');
  assert.equal(r.sent, false);
  assert.match(r.reason, /disarmed/);
  assert.deepEqual(h.sent, [], 'sendCommands must not have been called at all');
});

test('the write gate blocks a write with no named operator gesture, even armed', async () => {
  // This is what stops a lifecycle hook, a timer or a diff arriving from
  // reaching the mixer: none of them has a gesture to name.
  const h = gateHarness(true);
  for (const gesture of ['', undefined, null, 0]) {
    const r = await h.gate.submit(ONE_COMMAND, gesture);
    assert.equal(r.sent, false, `gesture ${JSON.stringify(gesture)}`);
    assert.match(r.reason, /operator gesture/);
  }
  assert.deepEqual(h.sent, []);
});

test('the write gate never sends an empty array', async () => {
  const h = gateHarness(true);
  for (const cmds of [[], null, undefined, 'setRouting']) {
    const r = await h.gate.submit(cmds, 'operator pressed Apply');
    assert.equal(r.sent, false);
    assert.equal(r.reason, 'nothing to send');
  }
  assert.deepEqual(h.sent, []);
});

test('the write gate sends when armed and asked by a named gesture', async () => {
  const h = gateHarness(true);
  const r = await h.gate.submit(ONE_COMMAND, 'operator pressed Apply');
  assert.equal(r.sent, true);
  assert.equal(r.reason, '');
  assert.deepEqual(h.sent, [ONE_COMMAND]);
});

test('the write gate reads armed at the moment of the write, not when it was built', async () => {
  // A control rendered while armed must not be able to fire after a disarm.
  const h = gateHarness(true);
  h.arm(false);
  const r = await h.gate.submit(ONE_COMMAND, 'operator pressed Apply');
  assert.equal(r.sent, false);
  assert.deepEqual(h.sent, []);
});

test('the write gate REFUSES a write from a stale view, however armed and however gestured', async () => {
  // S2. set_routing is an absolute replace, so a write planned from a stale
  // matrix carries a rollback of every bus the operator did not touch. This is
  // the gate, not the button: submit() is what a keyboard activation, a
  // programmatic click and any future control all reach.
  const h = gateHarness(true);
  h.setFresh(false);
  const r = await h.gate.submit(ONE_COMMAND, 'operator pressed Apply');
  assert.equal(r.sent, false);
  assert.match(r.reason, /REPLACES/);
  assert.match(r.reason, /STALE - no update for 40s/, 'the refusal must carry the age, not just the fact');
  assert.deepEqual(h.sent, [], 'sendCommands must not have been called at all');
});

test('the write gate reads freshness at the moment of the write, not when it was built', async () => {
  // The same property isArmed has, for the same reason: a plan built while the
  // feed was live must not be sent after it stopped.
  const h = gateHarness(true);
  h.setFresh(false);
  assert.equal((await h.gate.submit(ONE_COMMAND, 'operator pressed Apply')).sent, false);
  h.setFresh(true);
  assert.equal((await h.gate.submit(ONE_COMMAND, 'operator pressed Apply')).sent, true);
  assert.equal(h.sent.length, 1);
});

test('the write gate reports a failed send and does not claim it was sent', async () => {
  const errors = [];
  const gate = createWriteGate({
    sendCommands: async () => {
      throw new Error('websocket closed');
    },
    isArmed: () => true,
    viewIsFresh: () => ({ fresh: true, text: 'live' }),
    onError: (err, ctx) => errors.push([err.message, ctx]),
  });
  const r = await gate.submit(ONE_COMMAND, 'operator pressed Apply');
  assert.equal(r.sent, false);
  assert.match(r.reason, /websocket closed/);
  assert.equal(errors.length, 1, 'the error must not be swallowed');
  assert.match(errors[0][1], /mixer write/);
});

test('createWriteGate refuses to exist without its dependencies', () => {
  assert.throws(() => createWriteGate({}), /needs sendCommands/);
  assert.throws(() => createWriteGate({ sendCommands: async () => {} }), /needs isArmed/);
  // viewIsFresh is REQUIRED, not optional. An optional safety check is one a
  // future call site omits by accident, and this one stands between a stale
  // matrix and a live clean feed.
  assert.throws(
    () => createWriteGate({ sendCommands: async () => {}, isArmed: () => true }),
    /needs viewIsFresh/,
  );
});

/* ------------------------------------------------------------------ */
/* Drift                                                               */
/* ------------------------------------------------------------------ */

test('compareSnapshots ranks a strip gaining aux1 as CRITICAL and puts it first', () => {
  // The defining case. A reset, a re-added input or a restored device profile
  // silently produces exactly this.
  const golden = snapshot([strip({ outputs: ['master'] }), strip({ name: 'cam23-1', displayName: 'Input 23' })]);
  const current = snapshot([
    strip({ outputs: ['master', 'aux1', 'aux2'] }),
    strip({ name: 'cam23-1', displayName: 'RENAMED' }),
  ]);
  const diffs = compareSnapshots(golden, current);
  assert.equal(diffs[0].severity, 'critical', 'CRITICAL must sort above the display-name change');
  assert.equal(diffs[0].field, 'outputs');
  assert.equal(diffs[0].target, 'cam22-1');
  assert.equal(diffs[0].label, 'CLAUDE-COMMS');
  assert.match(diffHeadline(diffs[0]), /IS NOW IN THE CLEAN FEED/);
  assert.equal(diffs[diffs.length - 1].severity, 'info');
});

test('compareSnapshots ranks losing master as CRITICAL too', () => {
  const diffs = compareSnapshots(snapshot([strip()]), snapshot([strip({ outputs: ['aux1', 'aux2'] })]));
  assert.equal(diffs[0].severity, 'critical');
  assert.match(diffHeadline(diffs[0]), /REMOVED from programme/);
});

test('compareSnapshots ranks a monitor-only routing change as a warning', () => {
  const diffs = compareSnapshots(snapshot([strip()]), snapshot([strip({ outputs: ['master', 'aux1', 'aux2', 'mon1'] })]));
  assert.equal(diffs.length, 1);
  assert.equal(diffs[0].severity, 'warn');
});

test('compareSnapshots ranks a mute change by whether the strip reaches air', () => {
  const onAir = compareSnapshots(snapshot([strip()]), snapshot([strip({ muted: true })]));
  assert.equal(onAir[0].field, 'muted');
  assert.equal(onAir[0].severity, 'critical');

  const monitorOnly = compareSnapshots(
    snapshot([strip({ outputs: ['mon1'] })]),
    snapshot([strip({ outputs: ['mon1'], muted: true })]),
  );
  assert.equal(monitorOnly[0].severity, 'warn');
});

test('compareSnapshots ranks a fader move on a muted strip below one on a live strip', () => {
  const live = compareSnapshots(
    snapshot([strip({ faderEnabled: [true, true] })]),
    snapshot([strip({ faderEnabled: [true, true], fader: [-6, -6] })]),
  );
  assert.equal(live[0].field, 'fader');
  assert.equal(live[0].severity, 'critical');

  const muted = compareSnapshots(
    snapshot([strip({ muted: true, faderEnabled: [true, true] })]),
    snapshot([strip({ muted: true, faderEnabled: [true, true], fader: [-6, -6] })]),
  );
  assert.equal(muted[0].severity, 'warn');
});

test('compareSnapshots never emits a diff for a meter reading', () => {
  // Levels change every frame. Emitting them would bury the criticals under a
  // hundred info lines a second, which is the same as hiding them.
  const diffs = compareSnapshots(
    snapshot([strip({ level: [-100, -100], peakHold: [-100, -100] })]),
    snapshot([strip({ level: [-3, -3], peakHold: [-3, -3] })]),
  );
  assert.deepEqual(diffs, []);
});

test('compareSnapshots reports a strip appearing or disappearing', () => {
  const gone = compareSnapshots(snapshot([strip()]), snapshot([]));
  assert.equal(gone[0].field, 'present');
  assert.equal(gone[0].severity, 'critical', 'it was on master and aux1');
  assert.match(diffHeadline(gone[0]), /DISAPPEARED/);

  const arrived = compareSnapshots(snapshot([]), snapshot([strip()]));
  assert.equal(arrived[0].severity, 'critical');
  assert.match(diffHeadline(arrived[0]), /APPEARED/);
});

test('compareSnapshots ranks bus mutes by whether the bus leaves the building', () => {
  const cleanMuted = compareSnapshots(
    snapshot([strip()], [bus({ name: 'aux1' })]),
    snapshot([strip()], [bus({ name: 'aux1', muted: true })]),
  );
  assert.equal(cleanMuted[0].severity, 'critical');

  const monMuted = compareSnapshots(
    snapshot([strip()], [bus({ name: 'mon1' })]),
    snapshot([strip()], [bus({ name: 'mon1', muted: true })]),
  );
  assert.equal(monMuted[0].severity, 'warn');
});

test('compareSnapshots returns nothing when there is no golden, and says nothing about equality', () => {
  assert.deepEqual(compareSnapshots(null, snapshot([strip()])), []);
  assert.deepEqual(compareSnapshots(snapshot([strip()]), null), []);
});

test('compareSnapshots finds no difference between a snapshot and itself', () => {
  const s = snapshot([strip(), strip({ name: 'cam23-1' })]);
  assert.deepEqual(compareSnapshots(s, s), []);
});

test('routingSeverity looks at what changed, not at what is there', () => {
  const cases = [
    [['master'], ['master', 'mon1'], 'warn'],
    [['master'], ['master', 'aux1'], 'critical'],
    [['master', 'aux1'], ['master'], 'critical'],
    [['mon1'], ['mon2'], 'warn'],
    [['master', 'aux1'], ['aux1', 'master'], 'warn', 'nothing changed; order is not a change'],
  ];
  for (const [g, c, want] of cases) {
    assert.equal(routingSeverity(g, c), want, `${JSON.stringify(g)} -> ${JSON.stringify(c)}`);
  }
});

test('sortDiffs is stable within a severity', () => {
  const diffs = [
    { severity: 'info', target: 'i1' },
    { severity: 'critical', target: 'c1' },
    { severity: 'warn', target: 'w1' },
    { severity: 'critical', target: 'c2' },
    { severity: undefined, target: 'u1' },
  ];
  assert.deepEqual(
    sortDiffs(diffs).map((d) => d.target),
    ['c1', 'c2', 'w1', 'i1', 'u1'],
  );
  assert.deepEqual(sortDiffs(null), []);
});

test('diffHeadline speaks the operator language, not the wire language', () => {
  const cases = [
    [
      { label: 'CLAUDE-COMMS', field: 'outputs', golden: 'master', current: 'master, aux1, aux2' },
      /CLAUDE-COMMS IS NOW IN THE CLEAN FEED/,
    ],
    [
      { label: 'CLAUDE-COMMS', field: 'outputs', golden: 'master, aux1', current: 'master' },
      /REMOVED from the clean feed/,
    ],
    [{ label: 'MIC 1', field: 'muted', golden: 'false', current: 'true' }, /is now MUTED/],
    [{ label: 'MIC 1', field: 'muted', golden: 'true', current: 'false' }, /is now UNMUTED/],
    [{ label: 'MIC 1', field: 'subChMode', golden: 'ST_W', current: 'MONO' }, /subChMode was ST_W, now MONO/],
  ];
  for (const [d, want] of cases) assert.match(diffHeadline(d), want);
  assert.equal(diffHeadline(null), '(empty difference)');
});

test('busListText never renders a raw bus name, and a diff computed here reads like one from Go', () => {
  // S4. contract.js: "Never render a raw bus name. An operator reading 'aux1'
  // has no way to know they are looking at the clean feed." The default
  // routing of every strip is the worst case, because 'master, aux1, aux2' is
  // both the most common string and the one that says commentary is in the
  // client's clean feed.
  const got = busListText(['master', 'aux1', 'aux2']);
  assert.match(got, /clean feed/, 'the clean feed must be named as the clean feed');
  assert.equal(got, 'master (PGM), aux1 (CLN - clean feed), aux2 (no egress)');
  assert.equal(busListText([]), '(none)');
  assert.equal(busListText(null), '(none)');
  for (const bus of ALL_BUSES) {
    assert.equal(busListText([bus]), busLabel(bus), `${bus} must render through busLabel`);
  }

  // The diff values compareSnapshots produces go through it too, so the drift
  // panel cannot show a bare 'aux1' either.
  const diffs = compareSnapshots(snapshot([strip({ outputs: ['master'] })]), snapshot([strip()]));
  const outputs = diffs.find((d) => d.field === 'outputs');
  assert.ok(outputs, 'expected an outputs diff');
  assert.match(outputs.current, /clean feed/);
  assert.match(outputs.golden, /PGM/);

  // And diffHeadline still classifies correctly off the labelled strings —
  // renderedHasBus tokenises, so it reads both the labelled form produced here
  // and the one mixer.Compare produces in Go.
  assert.match(diffHeadline(outputs), /IS NOW IN THE CLEAN FEED/);
});

test('renderedHasBus matches whole tokens so aux1 does not match aux10', () => {
  assert.equal(renderedHasBus('master, aux1, aux2', 'aux1'), true);
  assert.equal(renderedHasBus('master, aux10', 'aux1'), false);
  assert.equal(renderedHasBus('(none)', 'aux1'), false);
  assert.equal(renderedHasBus('', 'aux1'), false);
});

/* ------------------------------------------------------------------ */
/* Staleness                                                           */
/* ------------------------------------------------------------------ */

test('viewFreshness stops claiming the view is current once updates stop', () => {
  const now = 1_000_000;
  assert.equal(viewFreshness(now, now).fresh, true);
  assert.equal(viewFreshness(now - 1000, now).fresh, true);
  const stale = viewFreshness(now - 10_000, now);
  assert.equal(stale.fresh, false);
  assert.match(stale.text, /STALE/);
  const never = viewFreshness(null, now);
  assert.equal(never.fresh, false);
  assert.match(never.text, /no state received yet/);
});
