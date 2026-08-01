/**
 * Tests for the mixer drawer itself, against the shim in ./testdom.js.
 *
 * Owner: WP-M4.
 *
 *   cd frontend && node --test "src/ui/mixer/*.test.js"
 *
 * What these prove is the safety model: that the rendered drawer writes NOTHING
 * on open, close, update, arm or destroy; that a crosspoint click while
 * disarmed reaches nothing; that an armed click is INSTANT but is still planned
 * from a frame fetched by that click rather than from the screen; and that a
 * sent change is not reported as applied until a following snapshot shows it.
 *
 * The Apply button is gone — a click is the write now — so every assertion that
 * used to be made about Apply is made about the click instead. In particular
 * the stale-view refusal, which used to be shown on the Apply control, is now
 * asserted IN THE CELL THAT WAS CLICKED: removing the button removed the only
 * place that refusal had to live.
 *
 * What they do NOT prove is appearance. ./testdom.js is a stand-in with no
 * layout and no CSS — see its header, and see demo.html for how the drawer is
 * actually looked at. In particular it cannot see the sticky-header stacking
 * fix in mixer.css.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createMixerDrawer } from './drawer.js';
import { LIVE_FIXTURE } from './demo-fixture.js';
import { buttonWithText, createTestDom, crosspoint, fire, query, queryAll } from './testdom.js';

/* ------------------------------------------------------------------ */
/* Harness                                                             */
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

function snapshot(strips = [strip()]) {
  return {
    strips,
    buses: ['master', 'aux1', 'aux2', 'mon1', 'mon2', 'mon3', 'mon4'].map((name) => ({
      name,
      muted: false,
      channelCount: 2,
      level: [-100, -100],
      peakHold: [-100, -100],
      metered: true,
      fader: 1,
      faderPresent: name === 'master' || name.startsWith('aux'),
    })),
    takenAt: '2026-07-31T18:00:00Z',
  };
}

/**
 * harness builds a drawer over the shim and records everything that crosses the
 * contract boundary, so a test can assert that NOTHING crossed it.
 */
function harness(over = {}) {
  const { doc, mount } = createTestDom();
  const calls = { sendCommands: [], setGolden: [], getSnapshot: 0, getGolden: 0, errors: [], armed: [] };
  const drawer = createMixerDrawer({
    mount,
    getSnapshot: async () => {
      calls.getSnapshot += 1;
      return snapshot();
    },
    sendCommands: async (cmds) => {
      calls.sendCommands.push(cmds);
    },
    getGolden: async () => {
      calls.getGolden += 1;
      return null;
    },
    setGolden: async (snap) => {
      calls.setGolden.push(snap);
    },
    onError: (err, ctx) => calls.errors.push([err.message, ctx]),
    onArmedChange: (a) => calls.armed.push(a),
    ...over,
  });
  return { doc, mount, drawer, calls };
}

/** settle lets the drawer's own async work finish. A crosspoint click is now
 * async — it re-reads before it writes — so every click that is expected to
 * reach the mixer is followed by one of these. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

/** allow presses the header control that permits writes. */
const allow = (mount) => fire(buttonWithText(mount, 'Allow changes'), 'click');

/** noteIn returns the refusal/result note rendered inside one crosspoint cell. */
function noteIn(mount, stripName, bus) {
  const btn = crosspoint(mount, stripName, bus);
  const td = btn.parentNode;
  return query(td, (n) => (n.className || '').startsWith('mx-x-note'));
}

const noticeText = (mount) => query(mount, (n) => (n.className || '').startsWith('mx-notice')).textContent;

/**
 * visibleRows is the matrix as the OPERATOR sees it: rows that are built but
 * hidden do not count. Every input has a second '-2' strip on the wire and the
 * drawer shows one row per input, so "how many rows are there" and "how many
 * rows can be seen" are now different questions.
 */
const visibleRows = (mount) =>
  queryAll(mount, (n) => n.tagName === 'TR' && (n.className || '').startsWith('mx-row') && n.hidden !== true);

/** meterBars returns the two peak-hold bars of the first meter cell. */
const meterBars = (mount) => queryAll(mount, (n) => (n.className || '') === 'mx-meter-bar');

/** stageWidths reports how full each of a bar's three stages is, as numbers. */
const stageWidths = (bar) =>
  bar.childNodes.map((seg) => Number(String(seg.firstChild.style.width).replace('%', '')));

/* ------------------------------------------------------------------ */
/* Lifecycle                                                           */
/* ------------------------------------------------------------------ */

test('the drawer starts closed, disarmed, and having written nothing', async () => {
  const h = harness();
  assert.equal(h.drawer.isOpen(), false);
  assert.deepEqual(h.calls.sendCommands, []);
  h.drawer.destroy();
});

test('opening reads and never writes', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  assert.equal(h.drawer.isOpen(), true);
  assert.equal(h.calls.getSnapshot, 1, 'open refreshes from getSnapshot');
  assert.deepEqual(h.calls.sendCommands, [], 'opening must not write');
  h.drawer.destroy();
});

test('update while closed stores state without touching the DOM', () => {
  const h = harness();
  const before = h.mount.childNodes[0].childNodes.length;
  h.drawer.update(snapshot());
  assert.equal(h.drawer.isOpen(), false);
  assert.equal(h.mount.childNodes[0].hidden, true, 'the drawer stays hidden');
  assert.equal(h.mount.childNodes[0].childNodes.length, before, 'no nodes were added while closed');
  assert.deepEqual(h.calls.sendCommands, []);
  h.drawer.destroy();
});

test('update never writes, however alarming the snapshot', () => {
  const h = harness();
  h.drawer.open();
  h.drawer.setArmed(true);
  // Every strip in the clean feed, unmuted, at full level. The drawer must
  // shout about it and correct nothing.
  h.drawer.update(
    snapshot([
      strip({ muted: false, peakHold: [-0.5, -0.5] }),
      strip({ name: 'MIC 1-1', input: 'MIC 1', displayName: 'MIC 1', peakHold: [-2, -2] }),
    ]),
  );
  assert.deepEqual(h.calls.sendCommands, [], 'update is a pure state update');
  h.drawer.destroy();
});

test('Escape closes the drawer', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const ev = fire(h.doc, 'keydown', { key: 'Escape' });
  assert.equal(h.drawer.isOpen(), false);
  assert.equal(ev.defaultPrevented, true, 'a modal consumes its own Escape');
  h.drawer.destroy();
});

test('Escape does nothing while the drawer is closed', () => {
  const h = harness();
  fire(h.doc, 'keydown', { key: 'Escape' });
  assert.equal(h.drawer.isOpen(), false);
  h.drawer.destroy();
});

test('the close control closes the drawer', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  fire(query(h.mount, (n) => n.getAttribute && n.getAttribute('aria-label') === 'Close the mixer drawer (Escape)'), 'click');
  assert.equal(h.drawer.isOpen(), false);
  h.drawer.destroy();
});

test('closing always locks changes again, and says so rather than doing it silently', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  assert.deepEqual(h.calls.armed, [true]);
  h.drawer.close();
  assert.deepEqual(h.calls.armed, [true, false], 'the host must not be left believing it is still armed');
  assert.deepEqual(h.calls.sendCommands, [], 'locking is not a write');

  // And it is still locked when reopened: opening never arms.
  h.drawer.open();
  await settle();
  const armBtn = buttonWithText(h.mount, 'Allow changes');
  assert.ok(armBtn, 'the control still reads "Allow changes"');
  assert.equal(armBtn.getAttribute('aria-pressed'), 'false');
  h.drawer.destroy();
});

test('destroy removes the drawer, restores the mount and writes nothing', async () => {
  const { doc, mount } = createTestDom();
  const original = doc.createElement('p');
  original.textContent = 'the host owns this';
  mount.appendChild(original);

  const sent = [];
  const drawer = createMixerDrawer({
    mount,
    getSnapshot: async () => snapshot(),
    sendCommands: async (c) => sent.push(c),
    getGolden: async () => null,
    setGolden: async () => {},
  });
  drawer.open();
  await settle();
  drawer.setArmed(true);
  drawer.destroy();

  assert.equal(mount.childNodes.length, 1);
  assert.equal(mount.childNodes[0], original, 'the mount must be restored');
  assert.deepEqual(sent, [], 'destroy must not "restore" routing or anything else');
  drawer.update(snapshot());
  drawer.open();
  assert.equal(drawer.isOpen(), false, 'a destroyed drawer stays destroyed');
});

/* ------------------------------------------------------------------ */
/* The clean feed, which is the whole point                            */
/* ------------------------------------------------------------------ */

test('the drawer says how much is in the clean feed, and names it in the title', async () => {
  // The block banner that used to carry this was removed as clutter. What it
  // must NOT lose is the ability to say so at all: this line is what is left,
  // and it still counts the unmuted ones separately.
  const h = harness();
  h.drawer.open();
  await settle();
  const line = query(h.mount, (n) => (n.className || '').startsWith('mx-cleanline'));
  assert.match(line.textContent, /CLN: 1 routed, 1 unmuted/);
  assert.match(line.getAttribute('title'), /IN THE CLEAN FEED AND UNMUTED: CLAUDE-COMMS/);
  assert.match(line.getAttribute('title'), /CLN output the client receives/);
  h.drawer.destroy();
});

test('the big clean-feed banner is gone', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  assert.equal(query(h.mount, (n) => (n.className || '').startsWith('mx-banner')), null);
  h.drawer.destroy();
});

test('only PGM and CLN are columns until the operator asks for the rest', async () => {
  const h = harness();
  h.drawer.open();
  await settle();

  let headers = queryAll(h.mount, (n) => n.tagName === 'TH' && (n.className || '').includes('mx-th--bus'));
  assert.deepEqual(
    headers.map((th) => th.getAttribute('data-bus')),
    ['master', 'aux1'],
    'the default columns are exactly the two buses that leave the building',
  );
  const names = headers.map((th) => query(th, (n) => (n.className || '') === 'mx-th-name').textContent);
  assert.deepEqual(names, ['PGM', 'CLN'], 'and they are labelled the way an operator says them');
  // The full name is still one hover away, so nothing is LOST by the short one.
  assert.equal(headers[1].getAttribute('title'), 'aux1 (CLN - clean feed)');
  assert.equal(crosspoint(h.mount, 'cam22-1', 'mon1'), null, 'the monitor buses have no crosspoints yet');

  fire(buttonWithText(h.mount, 'Show all buses'), 'click');
  headers = queryAll(h.mount, (n) => n.tagName === 'TH' && (n.className || '').includes('mx-th--bus'));
  assert.equal(headers.length, 7, 'all seven stereo buses once asked for');
  // And the extra ones keep the fuller labels, where the extra words earn it.
  const aux2 = headers.find((th) => th.getAttribute('data-bus') === 'aux2');
  assert.match(aux2.textContent, /aux2/);
  assert.match(aux2.textContent, /no egress/, 'aux2 must be marked undeliverable');
  assert.ok(crosspoint(h.mount, 'cam22-1', 'mon1'), 'and the monitor crosspoints appear');
  h.drawer.destroy();
});

test('the input column is headed "Input", not "Strip"', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const th = query(h.mount, (n) => (n.className || '').includes('mx-th--strip'));
  assert.equal(th.textContent, 'Input', '"strip" is desk jargon; the rest of this application says input');
  h.drawer.destroy();
});

test('a lit crosspoint carries a glyph, not only a colour', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const lit = crosspoint(h.mount, 'cam22-1', 'aux1');
  const unlit = crosspoint(h.mount, 'cam22-1', 'master');
  h.drawer.update(snapshot([strip({ outputs: ['aux1'] })]));
  assert.equal(lit.getAttribute('aria-checked'), 'true');
  assert.equal(unlit.getAttribute('aria-checked'), 'false');
  assert.match(lit.textContent, /◆/, 'the clean-feed column uses a filled diamond when lit');
  assert.match(lit.textContent, /CLN/, 'and says so in text');
  assert.match(unlit.textContent, /□/, 'an unlit crosspoint is an open square');
  assert.match(lit.getAttribute('aria-label'), /CLAUDE-COMMS to aux1 \(CLN - clean feed\): routed/);
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* The armed gate, at the rendered control                             */
/* ------------------------------------------------------------------ */

test('a crosspoint click while LOCKED reaches nothing, and says so in the cell', async () => {
  const h = harness();
  h.drawer.open();
  await settle();

  const cell = crosspoint(h.mount, 'cam22-1', 'aux1');
  assert.equal(cell.getAttribute('aria-disabled'), 'true');
  assert.match(cell.className, /mx-x--locked/, 'and it is visibly non-interactive');

  fire(cell, 'click');
  await settle();

  assert.deepEqual(h.calls.sendCommands, [], 'nothing may reach the mixer while locked');
  assert.equal(h.calls.getSnapshot, 1, 'and a locked click does not even re-read');
  assert.equal(cell.getAttribute('aria-checked'), 'true', 'the crosspoint did not change');
  // The refusal is IN THE CELL. There is no Apply button to put it on any more,
  // and a notice at the top of a scrolled 54-row matrix is one nobody reads.
  assert.match(noteIn(h.mount, 'cam22-1', 'aux1').textContent, /LOCKED/);
  assert.match(noticeText(h.mount), /Locked/, 'and it is explained in full, not silently ignored');
  h.drawer.destroy();
});

test('allowing changes by itself sends nothing', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  assert.deepEqual(h.calls.sendCommands, []);
  h.drawer.destroy();
});

test('the header control allows and locks the write path, and is RED while live', async () => {
  const h = harness();
  h.drawer.open();
  await settle();

  const btn = buttonWithText(h.mount, 'Allow changes');
  assert.match(btn.textContent, /○/, 'locked carries an open circle, not only a colour');
  assert.equal(
    query(h.mount, (n) => (n.className || '').startsWith('mx-armstate')).textContent,
    'READ-ONLY',
  );

  allow(h.mount);
  assert.deepEqual(h.calls.armed, [true]);
  assert.match(btn.className, /mx-allow--on/, 'red when active: writes are live');
  assert.match(btn.textContent, /●/, 'and a filled circle, so colour is never the only signal');
  assert.equal(btn.getAttribute('aria-pressed'), 'true');
  assert.equal(
    query(h.mount, (n) => (n.className || '').startsWith('mx-armstate')).textContent,
    'WRITES LIVE',
  );
  assert.equal(crosspoint(h.mount, 'cam22-1', 'aux1').getAttribute('aria-disabled'), 'false');

  allow(h.mount);
  assert.deepEqual(h.calls.armed, [true, false]);
  assert.deepEqual(h.calls.sendCommands, [], 'neither allowing nor locking is a write');
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* Instant actions: one click, one write                               */
/* ------------------------------------------------------------------ */

test('a crosspoint click while allowed is sent immediately - there is no Apply', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  assert.equal(buttonWithText(h.mount, 'Apply'), null, 'there is no Apply control at all any more');

  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.equal(h.calls.sendCommands.length, 1, 'the click IS the write');
  assert.deepEqual(h.calls.sendCommands[0], [
    { kind: 'setRouting', args: { matrix: 'output', input: 'cam22-1', outputs: ['master', 'aux2'] } },
  ]);
  h.drawer.destroy();
});

test('a click sends the strip\'s WHOLE bus set, including the columns that are hidden', async () => {
  // set_routing is an absolute replace and only two buses have columns. If the
  // hidden five were dropped from the outgoing set, every click would silently
  // un-route the strip from aux2 and the monitors.
  const h = harness({
    getSnapshot: async () => snapshot([strip({ outputs: ['master', 'aux1', 'aux2', 'mon2'] })]),
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.deepEqual(
    h.calls.sendCommands[0][0].args.outputs,
    ['master', 'aux2', 'mon2'],
    'the buses with no column on screen must survive the write',
  );
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* S2: the click is gated on freshness and planned from a fresh frame  */
/* ------------------------------------------------------------------ */

test('a click on a STALE view writes NOTHING and the refusal appears in the clicked cell', async () => {
  // The scenario this closes. The status feed stalls — the controller carries
  // a reconnect supervisor precisely because it does. The header reads STALE
  // and the matrix still shows the old frame. The operator, acting on that
  // matrix, clicks commentary out of the clean feed.
  //
  // set_routing REPLACES the whole bus set, so the bus they aimed at would be
  // correct and every OTHER bus in the command would be a forty-second-old
  // rollback applied to a live desk. Removing the Apply step removed a delay,
  // not this hazard.
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  const readsBefore = h.calls.getSnapshot;
  const realNow = Date.now;
  try {
    Date.now = () => realNow() + 40_000;
    fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
    await settle();
  } finally {
    Date.now = realNow;
  }

  assert.deepEqual(h.calls.sendCommands, [], 'a stale view must reach the mixer with nothing at all');
  assert.equal(h.calls.getSnapshot, readsBefore, 'and it must refuse before it even re-reads');

  // The refusal's new home: the cell that was clicked.
  const note = noteIn(h.mount, 'cam22-1', 'aux1');
  assert.equal(note.hidden, false, 'the refusal must be visible at the point of the click');
  assert.match(note.textContent, /NOT SENT/);
  assert.match(note.textContent, /STALE/, 'and it names the reason where the operator is looking');
  assert.match(crosspoint(h.mount, 'cam22-1', 'aux1').className, /mx-x--refused/, 'shape, not only colour');
  assert.match(crosspoint(h.mount, 'cam22-1', 'aux1').getAttribute('aria-label'), /NOT SENT/);

  // And in full, with the reason staleness matters for this particular command.
  assert.match(noticeText(h.mount), /NOT SENT/);
  assert.match(noticeText(h.mount), /STALE/);
  assert.match(noticeText(h.mount), /replaces/i);
  h.drawer.destroy();
});

test('the stale refusal is a property of the click PATH, not of any disabled attribute', async () => {
  // A disabled attribute is a suggestion. A keyboard activation and a
  // programmatic click both arrive at the handler regardless, so the test fires
  // the click anyway and asserts that nothing reaches the mixer.
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  const realNow = Date.now;
  try {
    Date.now = () => realNow() + 40_000;
    // Force the repaint that marks the header blocked, the way the 1 Hz stale
    // timer does when updates stop arriving.
    h.drawer.update(null); // ignored: not an object, so lastUpdateAt does not move
    const state = query(h.mount, (n) => (n.className || '').startsWith('mx-armstate'));
    assert.match(state.textContent, /BLOCKED/, 'the header says a click will be refused, with the reason');
    fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
    await settle();
  } finally {
    Date.now = realNow;
  }
  assert.deepEqual(h.calls.sendCommands, [], 'clicking anyway must still write nothing');
  h.drawer.destroy();
});

test('a click plans from the frame IT fetches, not from the one on screen', async () => {
  // The second half of S2, and the one a freshness check alone does not fix.
  // Even a live view is up to a second old and the desk is shared.
  //
  // On screen: cam22-1 is master + aux1 + aux2. Between the last update() and
  // the click, somebody takes it off aux2 and puts it on mon1.
  //
  // Planning from the screen sends ["master","aux2"] — right on the bus they
  // aimed at, and a rollback of the other two on a live desk. Planning from the
  // frame this click fetched sends ["master","mon1"].
  let current = snapshot([strip()]);
  const h = harness({ getSnapshot: async () => current });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  // The desk moves and the drawer is NOT told: no update() arrives.
  current = snapshot([strip({ outputs: ['master', 'aux1', 'mon1'] })]);

  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.equal(h.calls.sendCommands.length, 1);
  assert.deepEqual(
    h.calls.sendCommands[0][0].args.outputs,
    ['master', 'mon1'],
    'the unclicked buses must come from the frame the click fetched, not from the stale screen',
  );
  assert.match(noticeText(h.mount), /desk had moved/, 'and the operator is told the base moved');
  h.drawer.destroy();
});

test('a click sends nothing when the re-read shows the change is already in effect', async () => {
  // Somebody else made the same correction in the last second. Every avoidable
  // write to a live mixer is worth avoiding.
  let current = snapshot([strip()]);
  const h = harness({ getSnapshot: async () => current });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  current = snapshot([strip({ outputs: ['master', 'aux2'] })]);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.deepEqual(h.calls.sendCommands, []);
  assert.match(noticeText(h.mount), /already in effect/);
  h.drawer.destroy();
});

test('a click refuses when the mixer cannot be re-read, rather than writing from the old picture', async () => {
  let fail = false;
  const h = harness({
    getSnapshot: async () => {
      if (fail) throw new Error('websocket closed');
      return snapshot();
    },
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  fail = true;
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.deepEqual(h.calls.sendCommands, [], 'an unknown current routing is not a routing to replace');
  assert.match(noteIn(h.mount, 'cam22-1', 'aux1').textContent, /NOT SENT/);
  assert.match(noticeText(h.mount), /could not be re-read/);
  assert.equal(h.calls.errors.length >= 1, true, 'the failure is reported, not swallowed');
  h.drawer.destroy();
});

test('a strip whose name contains a space is written against the right strip', async () => {
  // 'MIC 1-1' is a real strip in the captured live frame. An earlier version
  // keyed changes by joining the strip and bus with a space and parsing them
  // back out, which turned 'MIC 1-1' + 'aux1' into the strip 'MIC'.
  const h = harness({
    getSnapshot: async () =>
      snapshot([strip({ name: 'MIC 1-1', input: 'MIC 1', displayName: 'CLAUDE-TEST-MIC' }), strip()]),
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  fire(crosspoint(h.mount, 'MIC 1-1', 'aux1'), 'click');
  await settle();
  assert.deepEqual(h.calls.sendCommands[0], [
    { kind: 'setRouting', args: { matrix: 'output', input: 'MIC 1-1', outputs: ['master', 'aux2'] } },
  ]);
  // A send that WORKED prints nothing under any cell, so there is no note here
  // to place. That a note lands on the clicked cell rather than a neighbour is
  // pinned by the refusal tests, which are the cases notes now exist for.
  assert.equal(noteIn(h.mount, 'MIC 1-1', 'aux1').hidden, true);
  assert.equal(noteIn(h.mount, 'cam22-1', 'aux1').hidden, true);
  h.drawer.destroy();
});

test('locking discards nothing because nothing is ever held: the next click is a fresh write', async () => {
  // There is no staging to discard any more. What must still hold is that a
  // click made while locked cannot become a write when the path is unlocked
  // later.
  const h = harness();
  h.drawer.open();
  await settle();
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();
  h.drawer.setArmed(true);
  await settle();
  assert.deepEqual(h.calls.sendCommands, [], 'a click made while locked must not be replayed on unlock');
  h.drawer.destroy();
});

test('a second click while the first is still in flight is refused, not queued', async () => {
  // Two set_routing commands for one strip built from two different re-reads
  // race, and set_routing REPLACES, so the loser silently reasserts the bus set
  // the winner just changed.
  let release;
  const inFlight = new Promise((r) => {
    release = r;
  });
  const h = harness({
    // A send that does not resolve until the test lets it, so the second click
    // lands while the first is still open.
    sendCommands: async (cmds) => {
      h.calls.sendCommands.push(cmds);
      await inFlight;
    },
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  const cell = crosspoint(h.mount, 'cam22-1', 'aux1');
  fire(cell, 'click');
  await settle();
  assert.equal(h.calls.sendCommands.length, 1, 'the first click is in flight');

  fire(cell, 'click');
  assert.equal(h.calls.sendCommands.length, 1, 'the second click must not become a second write');
  assert.match(noteIn(h.mount, 'cam22-1', 'aux1').textContent, /another change is still being sent/);

  release();
  await settle();
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* Sent is not applied                                                 */
/* ------------------------------------------------------------------ */

test('a resolved send is reported as SENT, and only a later snapshot confirms it', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.match(noticeText(h.mount), /SENT IS NOT APPLIED/);
  // A send that WORKED prints nothing under the row. The awaiting panel and the
  // notice line carry it; a banner appearing under the clicked cell on every
  // click shoved the matrix around for something that resolves in a second.
  // The note set on the way in ("Reading the mixer before writing...") must be
  // cleared too, or the success path leaves a stale line behind it.
  assert.equal(
    noteIn(h.mount, 'cam22-1', 'aux1').hidden,
    true,
    'a successful send leaves no note under the cell, including the pre-write one',
  );
  let awaiting = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(awaiting.textContent, /awaiting confirmation/);

  // The mixer reports the new routing: only now is it confirmed.
  h.drawer.update(snapshot([strip({ outputs: ['master', 'aux2'] })]));
  awaiting = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(awaiting.textContent, /CONFIRMED by the mixer/);
  h.drawer.destroy();
});

test('the confirmation panel names the clean feed and never a bare "aux1"', async () => {
  // S4. This is the highest-stakes read in the drawer: it appears immediately
  // after a write to a live desk and it is what the operator checks to decide
  // the change did what they meant.
  //
  // "CONFIRMED: CLAUDE-COMMS -> master, aux1" reads as success while confirming
  // that commentary is in the client's clean feed. contract.js: "Never render a
  // raw bus name. An operator reading 'aux1' has no way to know they are
  // looking at the clean feed." The two-letter COLUMN headings do not weaken
  // this: a column carries its meaning in the band, the ruling and the diamond;
  // a sentence carries it only in the words.
  let current = snapshot([strip()]);
  const h = harness({ getSnapshot: async () => current });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(buttonWithText(h.mount, 'Show all buses'), 'click');

  // Take the strip off aux2, leaving it on master AND the clean feed — the
  // case where a bare list reads as success and is not.
  fire(crosspoint(h.mount, 'cam22-1', 'aux2'), 'click');
  await settle();

  const check = (node, where) => {
    assert.match(node.textContent, /aux1 \(CLN - clean feed\)/, `${where} must name the clean feed`);
    assert.doesNotMatch(
      node.textContent,
      /aux1(?!\s*\(CLN)/,
      `${where} must not render a bare bus name anywhere`,
    );
    assert.match(node.textContent, /master \(PGM\)/, `${where} must label master too`);
  };

  check(query(h.mount, (n) => (n.className || '').startsWith('mx-await ')), 'the awaiting line');

  // And the same after the mixer reports it back, which is the line that
  // actually says the word CONFIRMED.
  h.drawer.update(snapshot([strip({ outputs: ['master', 'aux1'] })]));
  const confirmed = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(confirmed.textContent, /CONFIRMED by the mixer/);
  check(confirmed, 'the confirmed line');
  h.drawer.destroy();
});

test('a change the mixer never reports back is called NOT CONFIRMED', async () => {
  // set_comp_limit is silently inert when misused and reads back exactly as
  // sent; the drawer must never treat a resolved promise as proof.
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  // Four seconds of snapshots that still show the old routing.
  const realNow = Date.now;
  try {
    Date.now = () => realNow() + 10_000;
    h.drawer.update(snapshot());
  } finally {
    Date.now = realNow;
  }
  const awaiting = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(awaiting.textContent, /NOT CONFIRMED/);
  assert.match(awaiting.textContent, /may not have taken effect/);
  h.drawer.destroy();
});

test('a failed send is reported as not sent, and the error is not swallowed', async () => {
  const h = harness({
    sendCommands: async () => {
      throw new Error('websocket closed');
    },
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  await settle();

  assert.match(noticeText(h.mount), /NOT SENT/);
  assert.match(noteIn(h.mount, 'cam22-1', 'aux1').textContent, /NOT SENT/);
  assert.equal(h.calls.errors.length, 1);
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* Drift and golden: withdrawn from the GUI, not from the codebase     */
/* ------------------------------------------------------------------ */

test('the drift panel is not in the GUI, and the drawer does not call the golden bindings', async () => {
  // The operator asked for the drift panel to be taken out of the interface and
  // was explicit that none of the machinery behind it may be deleted or
  // weakened. So: nothing renders here, and nothing is fetched for it — while
  // internal/mixer/golden.go, mixer.Compare and their tests, and
  // compareSnapshots / sortDiffs / diffHeadline in ./model.js and THEIR tests,
  // are all untouched. See model.test.js, which still exercises every one of
  // them. Restoring the panel is a change to drawer.js alone.
  const h = harness({
    getGolden: async () => {
      h.calls.getGolden += 1;
      return snapshot([strip({ outputs: ['master'] })]);
    },
    getDiffs: async () => [
      { kind: 'strip', target: 'cam22-1', label: 'CLAUDE-COMMS', field: 'outputs', golden: 'master', current: 'master, aux1', severity: 'critical' },
    ],
  });
  h.drawer.open();
  await settle();
  h.drawer.update(snapshot());

  assert.equal(query(h.mount, (n) => (n.className || '').startsWith('mx-drift')), null, 'no drift panel');
  assert.equal(query(h.mount, (n) => (n.className || '').startsWith('mx-diff')), null, 'no diff list');
  assert.equal(buttonWithText(h.mount, 'Capture golden'), null, 'no golden control');
  assert.equal(h.calls.getGolden, 0, 'and nothing is fetched for a panel that is not there');
  assert.deepEqual(h.calls.setGolden, [], 'and the golden state is never rewritten');
  h.drawer.destroy();
});

test('the drawer still requires the golden options it no longer calls', () => {
  // They stay in the contract and stay wired by the host, so that putting the
  // panel back does not also mean re-doing the wiring.
  const { mount } = createTestDom();
  assert.throws(
    () => createMixerDrawer({ mount, getSnapshot: async () => {}, sendCommands: async () => {} }),
    /needs opts.getGolden/,
  );
});

/* ------------------------------------------------------------------ */
/* Failure and staleness                                               */
/* ------------------------------------------------------------------ */

test('a failed getSnapshot is reported and the view says it may be out of date', async () => {
  const h = harness({
    getSnapshot: async () => {
      throw new Error('backend unreachable');
    },
  });
  h.drawer.open();
  await settle();
  assert.equal(h.calls.errors.length, 1);
  assert.match(h.calls.errors[0][1], /getSnapshot/);
  assert.match(noticeText(h.mount), /out of date/);
  h.drawer.destroy();
});

test('with no state at all the drawer refuses to claim anything about the clean feed', async () => {
  // An empty matrix reads as "nothing is in the clean feed". That is a claim
  // this drawer cannot make, and losing the banner must not make it by
  // omission.
  const h = harness({
    getSnapshot: async () => {
      throw new Error('backend unreachable');
    },
  });
  h.drawer.open();
  await settle();
  const line = query(h.mount, (n) => (n.className || '').startsWith('mx-cleanline'));
  assert.match(line.textContent, /CLN: unknown/);
  assert.doesNotMatch(line.textContent, /nothing routed/);
  assert.match(line.getAttribute('title'), /cannot tell you what is in the clean feed/);
  h.drawer.destroy();
});

test('a meterless strip is drawn as absent, not as full scale', async () => {
  const h = harness({ getSnapshot: async () => snapshot([strip({ metered: false })]) });
  h.drawer.open();
  await settle();
  const meter = query(h.mount, (n) => (n.className || '').includes('mx-meter-text'));
  assert.equal(meter.textContent, 'no meter');
  for (const bar of meterBars(h.mount)) {
    assert.deepEqual(stageWidths(bar), [0, 0, 0], 'no reading draws nothing at all');
  }
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* The meters: the BAR is coloured, the number is not                  */
/* ------------------------------------------------------------------ */

test('the bar shows every stage the peak has passed through, per channel', async () => {
  // A staged broadcast meter: green to -18, amber to -6, red above it. A peak at
  // -3 is green, then amber, then half the red — not one flat colour picked
  // from the peak. The two channels are metered independently.
  const h = harness({ getSnapshot: async () => snapshot([strip({ peakHold: [-3, -30] })]) });
  h.drawer.open();
  await settle();

  const [left, right] = meterBars(h.mount);
  assert.deepEqual(stageWidths(left), [100, 100, 50], '-3 dBFS lights green, amber and half the red');
  assert.deepEqual(stageWidths(right), [71.4, 0, 0], '-30 dBFS is part-way through the green stage, and no further');
  assert.deepEqual(
    left.childNodes.map((seg) => seg.firstChild.className),
    ['mx-meter-fill mx-meter-fill--green', 'mx-meter-fill mx-meter-fill--amber', 'mx-meter-fill mx-meter-fill--red'],
    'the colour is a property of the stage, so it cannot follow the level about',
  );
  // The stages are fixed slices of the scale: -18 and -6 are always at 70% and
  // 90% of the bar, whatever the reading.
  assert.deepEqual(
    left.childNodes.map((seg) => seg.style.width),
    ['70%', '20%', '10%'],
  );
  h.drawer.destroy();
});

test('the bus meter in the column heading is staged the same way', async () => {
  // One meter language. A flat green bar on master — which sums at unity with
  // NO LIMITER — reads as "fine" at -1 dBFS.
  const snap = snapshot();
  snap.buses = snap.buses.map((b) => (b.name === 'master' ? { ...b, peakHold: [-3, -3] } : b));
  const h = harness({ getSnapshot: async () => snap });
  h.drawer.open();
  await settle();
  const busBar = query(h.mount, (n) => (n.className || '') === 'mx-busmeter');
  assert.deepEqual(stageWidths(busBar), [100, 100, 50]);
  h.drawer.destroy();
});

test('the dB text is never recoloured, at any level', async () => {
  // The operator's complaint: the number under the meter changed colour. It is
  // now plain foreground at every level, and colour is still not the only
  // signal because the number itself is the reading.
  for (const [peak, want] of [
    [-3, '-3.0 dB'],
    [-30, '-30.0 dB'],
    [-100, 'silent'],
  ]) {
    const h = harness({ getSnapshot: async () => snapshot([strip({ peakHold: [peak, peak] })]) });
    h.drawer.open();
    await settle();
    const text = query(h.mount, (n) => (n.className || '').includes('mx-meter-text'));
    assert.equal(text.textContent, want);
    assert.equal(text.className, 'mx-meter-text', `${peak} dBFS must not add a colour class to the number`);
    h.drawer.destroy();
  }
});

test('an overload is still spelled out in words beside the number', async () => {
  // It used to be a red ::after on a red number. Removing the colour must not
  // remove the warning with it.
  const h = harness({ getSnapshot: async () => snapshot([strip({ peakHold: [-0.5, -0.5] })]) });
  h.drawer.open();
  await settle();
  const text = query(h.mount, (n) => (n.className || '').includes('mx-meter-text'));
  assert.match(text.textContent, /OVER/);
  assert.match(text.textContent, /-0\.5 dB/, 'and the reading is still there');
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* The prose the operator asked to be rid of                           */
/* ------------------------------------------------------------------ */

test('the legend is the glyph key and nothing else', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const legend = query(h.mount, (n) => (n.className || '') === 'mx-legend');
  // The glyphs stay: ■ □ ◆ ◇ are not self-explanatory.
  for (const glyph of ['■', '□', '◆', '◇', 'dashed outline']) {
    assert.ok(legend.textContent.includes(glyph), `the key must keep ${glyph}`);
  }
  for (const prose of ['SENT IMMEDIATELY', 'Apply step', 'peak hold', 'read-only here']) {
    assert.ok(!legend.textContent.includes(prose), `the legend must not carry "${prose}" any more`);
  }
  h.drawer.destroy();
});

test('the sentence above the matrix is gone, and nothing it said is lost', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  assert.equal(query(h.mount, (n) => n.tagName === 'CAPTION'), null, 'the caption is removed outright');
  const all = h.mount.textContent;
  assert.ok(!all.includes('Rows are inputs'), 'and its text is nowhere else either');
  // What it explained is carried by the screen itself.
  assert.ok(query(h.mount, (n) => (n.className || '').includes('mx-th--strip')).textContent === 'Input');
  assert.ok(buttonWithText(h.mount, 'Show all buses'), 'the control still names what it does');
  const cln = queryAll(h.mount, (n) => n.tagName === 'TH' && (n.className || '').includes('mx-th--bus')).find(
    (th) => th.getAttribute('data-bus') === 'aux1',
  );
  assert.equal(cln.getAttribute('title'), 'aux1 (CLN - clean feed)', 'the full bus name is still a hover away');
  h.drawer.destroy();
});

test('the drawer says STALE once updates stop arriving', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const realNow = Date.now;
  try {
    Date.now = () => realNow() + 60_000;
    h.drawer.setArmed(true); // any render will do; setArmed does not write
  } finally {
    Date.now = realNow;
  }
  const fresh = query(h.mount, (n) => (n.className || '').startsWith('mx-fresh'));
  assert.match(fresh.textContent, /STALE/);
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* The real captured frame                                             */
/* ------------------------------------------------------------------ */

test('the drawer renders the captured live frame and shows CLAUDE-COMMS in the clean feed', async () => {
  // demo-fixture.js is the real 31 July 2026 frame from the dev event. If the
  // shape the drawer expects ever stops matching a genuine snapshot, this is
  // the test that notices.
  const h = harness({ getSnapshot: async () => LIVE_FIXTURE });
  h.drawer.open();
  await settle();

  const rows = queryAll(h.mount, (n) => (n.className || '').startsWith('mx-row'));
  assert.equal(rows.length, 54, 'one row per strip in the live frame');
  // ...but the operator sees one row per INPUT. 27 of the 54 are the '-2' half
  // of an input and none of them is audible on the clean feed, so all 27 are
  // hidden. They are hidden, not absent: the row is still built, so a '-2'
  // strip that becomes audible mid-session appears without a rebuild.
  assert.equal(visibleRows(h.mount).length, 27, 'the "-2" strips are not shown');

  const comms = crosspoint(h.mount, 'cam22-1', 'aux1');
  assert.equal(comms.getAttribute('aria-checked'), 'true', 'the live routing puts commentary in the clean feed');

  const line = query(h.mount, (n) => (n.className || '').startsWith('mx-cleanline'));
  assert.match(line.getAttribute('title'), /CLAUDE-COMMS/);
  // cam22-1 was muted in the captured frame, so it is not counted as audible.
  assert.match(line.getAttribute('title'), /muted/);
  assert.deepEqual(h.calls.sendCommands, [], 'rendering a real frame writes nothing');
  h.drawer.destroy();
});

test('the clean-feed count agrees with the rows the operator can actually see', async () => {
  // 48 of the live frame's strips are routed to aux1 and 24 of those are '-2'
  // strips, hidden and every one of them muted. A header reading "48 routed"
  // over 24 visible rows is a number nobody can reconcile with the screen, so
  // the count is taken from the drawn rows.
  const h = harness({ getSnapshot: async () => LIVE_FIXTURE });
  h.drawer.open();
  await settle();

  const line = query(h.mount, (n) => (n.className || '').startsWith('mx-cleanline'));
  assert.match(line.textContent, /CLN: 24 routed, 1 unmuted/);

  const shown = visibleRows(h.mount).filter(
    (tr) => crosspoint(tr, tr.getAttribute('data-strip'), 'aux1').getAttribute('aria-checked') === 'true',
  );
  assert.equal(shown.length, 24, 'and that is exactly how many lit CLN crosspoints are on screen');

  // The hidden ones are NOT dropped from the drawer's answer: they are still
  // routed to the client's clean feed and the title says so by name.
  assert.match(line.getAttribute('title'), /not shown as rows \(second-channel "-2" strips\)/);
  assert.match(line.getAttribute('title'), /cam22-2/);
  h.drawer.destroy();
});

test('a "-2" strip is hidden, unless it is AUDIBLE on the clean feed', async () => {
  // The exception is not a nicety. Hiding a row whose audio is reaching the
  // client would make the header count — the one number this drawer exists to
  // report — a lie, in exactly the case where it matters.
  const h = harness({
    getSnapshot: async () =>
      snapshot([
        strip({ name: 'cam22-1', displayName: 'CLAUDE-COMMS', muted: true }),
        strip({ name: 'cam22-2', displayName: 'CLAUDE-COMMS', muted: true }),
        strip({ name: 'cam1-2', displayName: 'CAM 1', muted: false }),
      ]),
  });
  h.drawer.open();
  await settle();

  assert.deepEqual(
    visibleRows(h.mount).map((tr) => tr.getAttribute('data-strip')),
    ['cam22-1', 'cam1-2'],
    'the muted -2 strip is hidden; the unmuted one is in the clean feed and must be seen',
  );
  const line = query(h.mount, (n) => (n.className || '').startsWith('mx-cleanline'));
  assert.match(line.textContent, /CLN: 2 routed, 1 unmuted/);
  assert.match(line.getAttribute('title'), /IN THE CLEAN FEED AND UNMUTED: CAM 1/);
  h.drawer.destroy();
});

test('a hidden "-2" strip stays hidden under every filter chip', async () => {
  const h = harness({
    getSnapshot: async () => snapshot([strip({ name: 'cam22-1' }), strip({ name: 'cam22-2', muted: true })]),
  });
  h.drawer.open();
  await settle();
  for (const chip of ['All strips', 'In clean feed', 'Unmuted or audible']) {
    fire(buttonWithText(h.mount, chip), 'click');
    assert.deepEqual(
      visibleRows(h.mount).map((tr) => tr.getAttribute('data-strip')),
      ['cam22-1'],
      `"${chip}" must not bring the hidden strip back`,
    );
  }
  h.drawer.destroy();
});

test('the drawer refuses to be built without the options it needs', () => {
  const { mount } = createTestDom();
  assert.throws(() => createMixerDrawer({}), /needs opts.mount/);
  assert.throws(() => createMixerDrawer({ mount }), /needs opts.getSnapshot/);
  assert.throws(() => createMixerDrawer({ mount, getSnapshot: async () => {} }), /needs opts.sendCommands/);
});
