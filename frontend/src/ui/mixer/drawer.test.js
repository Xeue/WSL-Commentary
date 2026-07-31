/**
 * Tests for the mixer drawer itself, against the shim in ./testdom.js.
 *
 * Owner: WP-M4.
 *
 *   cd frontend && node --test "src/ui/mixer/*.test.js"
 *
 * What these prove is the safety model: that the rendered drawer writes NOTHING
 * on open, close, update, arm or destroy; that a crosspoint click while
 * disarmed reaches nothing; that even armed, a click only stages and a separate
 * Apply sends; and that a sent change is not reported as applied until a
 * following snapshot shows it.
 *
 * What they do NOT prove is appearance. ./testdom.js is a stand-in with no
 * layout and no CSS — see its header, and see demo.html for how the drawer is
 * actually looked at.
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

/** settle lets the drawer's own async refresh on open finish. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

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

test('closing always disarms, and says so rather than doing it silently', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  assert.deepEqual(h.calls.armed, [true]);
  h.drawer.close();
  assert.deepEqual(h.calls.armed, [true, false], 'the host must not be left believing it is still armed');
  assert.deepEqual(h.calls.sendCommands, [], 'disarming is not a write');

  // And it is still armed-off when reopened: opening never arms.
  h.drawer.open();
  await settle();
  const armBtn = buttonWithText(h.mount, 'Arm write path');
  assert.ok(armBtn, 'the arm control reads "Arm write path" again');
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

test('the drawer names the strips that are in the clean feed, in words', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const banner = query(h.mount, (n) => (n.className || '').startsWith('mx-banner'));
  assert.match(banner.textContent, /IN THE CLEAN FEED AND UNMUTED: CLAUDE-COMMS/);
  assert.match(banner.textContent, /CLN output the client receives/);
  h.drawer.destroy();
});

test('the clean feed column is labelled as the clean feed, never as aux1 alone', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const headers = queryAll(h.mount, (n) => n.tagName === 'TH' && (n.className || '').includes('mx-th--bus'));
  assert.equal(headers.length, 7, 'seven stereo buses');
  const clean = headers.find((th) => th.textContent.includes('aux1'));
  assert.match(clean.textContent, /CLN - clean feed/);
  assert.ok(
    headers.find((th) => th.textContent.includes('aux2') && th.textContent.includes('no egress')),
    'aux2 must be marked undeliverable',
  );
  h.drawer.destroy();
});

test('a lit crosspoint carries a glyph, not only a colour', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const lit = crosspoint(h.mount, 'cam22-1', 'aux1');
  const unlit = crosspoint(h.mount, 'cam22-1', 'mon1');
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

test('a crosspoint click while DISARMED reaches nothing and stages nothing', async () => {
  const h = harness();
  h.drawer.open();
  await settle();

  const cell = crosspoint(h.mount, 'cam22-1', 'aux1');
  assert.equal(cell.getAttribute('aria-disabled'), 'true');
  assert.match(cell.className, /mx-x--locked/, 'and it is visibly non-interactive');

  fire(cell, 'click');

  assert.deepEqual(h.calls.sendCommands, [], 'nothing may reach the mixer while disarmed');
  const apply = buttonWithText(h.mount, 'Apply');
  assert.equal(apply.hidden, true, 'there is no Apply control to press while disarmed');
  assert.equal(cell.getAttribute('aria-checked'), 'true', 'the crosspoint did not change');
  const notice = query(h.mount, (n) => (n.className || '').startsWith('mx-notice'));
  assert.match(notice.textContent, /disarmed/, 'the click is explained, not silently ignored');
  h.drawer.destroy();
});

test('arming by itself sends nothing', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  assert.deepEqual(h.calls.sendCommands, []);
  h.drawer.destroy();
});

test('a crosspoint click while ARMED only stages; Apply is what sends', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  assert.deepEqual(h.calls.sendCommands, [], 'a crosspoint click is not a write');

  const staged = query(h.mount, (n) => (n.className || '').startsWith('mx-staged'));
  assert.match(staged.textContent, /CLAUDE-COMMS OUT of the clean feed/, 'and it says what it will do');

  const apply = buttonWithText(h.mount, 'Apply 1 change');
  assert.ok(apply, 'the Apply control counts the staged changes');
  fire(apply, 'click');
  await settle();

  assert.equal(h.calls.sendCommands.length, 1);
  assert.deepEqual(h.calls.sendCommands[0], [
    { kind: 'setRouting', args: { matrix: 'output', input: 'cam22-1', outputs: ['master', 'aux2'] } },
  ]);
  h.drawer.destroy();
});

test('two crosspoints on one strip become ONE set_routing, because set_routing replaces', async () => {
  // Two commands would have the second undo the first.
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  fire(crosspoint(h.mount, 'cam22-1', 'aux2'), 'click');
  fire(buttonWithText(h.mount, 'Apply 2 change'), 'click');
  await settle();

  assert.equal(h.calls.sendCommands.length, 1);
  assert.equal(h.calls.sendCommands[0].length, 1, 'one command for one strip');
  assert.deepEqual(h.calls.sendCommands[0][0].args.outputs, ['master']);
  h.drawer.destroy();
});

test('a strip whose name contains a space stages and sends against the right strip', async () => {
  // 'MIC 1-1' is a real strip in the captured live frame. An earlier version
  // keyed staged changes by joining the strip and bus with a space and parsing
  // them back out, which turned 'MIC 1-1' + 'aux1' into the strip 'MIC'.
  const h = harness({
    getSnapshot: async () =>
      snapshot([strip({ name: 'MIC 1-1', input: 'MIC 1', displayName: 'CLAUDE-TEST-MIC' }), strip()]),
  });
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);

  fire(crosspoint(h.mount, 'MIC 1-1', 'aux1'), 'click');
  const staged = query(h.mount, (n) => (n.className || '').startsWith('mx-staged'));
  assert.match(staged.textContent, /CLAUDE-TEST-MIC OUT of the clean feed/);

  fire(buttonWithText(h.mount, 'Apply 1 change'), 'click');
  await settle();
  assert.deepEqual(h.calls.sendCommands[0], [
    { kind: 'setRouting', args: { matrix: 'output', input: 'MIC 1-1', outputs: ['master', 'aux2'] } },
  ]);
  h.drawer.destroy();
});

test('clicking a staged crosspoint again un-stages it, and Apply then sends nothing', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  const cell = crosspoint(h.mount, 'cam22-1', 'aux1');
  fire(cell, 'click');
  fire(cell, 'click');
  fire(buttonWithText(h.mount, 'Apply'), 'click');
  await settle();
  assert.deepEqual(h.calls.sendCommands, [], 'nothing was staged, so nothing may be sent');
  h.drawer.destroy();
});

test('disarming discards staged changes rather than leaving them primed', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  h.drawer.setArmed(true);
  fire(crosspoint(h.mount, 'cam22-1', 'aux1'), 'click');
  h.drawer.setArmed(false);
  h.drawer.setArmed(true);
  fire(buttonWithText(h.mount, 'Apply'), 'click');
  await settle();
  assert.deepEqual(h.calls.sendCommands, []);
  h.drawer.destroy();
});

test('the arm control in the drawer arms and disarms the write path', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  fire(buttonWithText(h.mount, 'Arm write path'), 'click');
  assert.deepEqual(h.calls.armed, [true]);
  assert.equal(crosspoint(h.mount, 'cam22-1', 'aux1').getAttribute('aria-disabled'), 'false');
  fire(buttonWithText(h.mount, 'Disarm write path'), 'click');
  assert.deepEqual(h.calls.armed, [true, false]);
  assert.deepEqual(h.calls.sendCommands, [], 'neither arming nor disarming is a write');
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
  fire(buttonWithText(h.mount, 'Apply 1 change'), 'click');
  await settle();

  const notice = query(h.mount, (n) => (n.className || '').startsWith('mx-notice'));
  assert.match(notice.textContent, /SENT IS NOT APPLIED/);
  let awaiting = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(awaiting.textContent, /awaiting confirmation/);

  // The mixer reports the new routing: only now is it confirmed.
  h.drawer.update(snapshot([strip({ outputs: ['master', 'aux2'] })]));
  awaiting = query(h.mount, (n) => (n.className || '').startsWith('mx-await '));
  assert.match(awaiting.textContent, /CONFIRMED by the mixer/);
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
  fire(buttonWithText(h.mount, 'Apply 1 change'), 'click');
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
  fire(buttonWithText(h.mount, 'Apply 1 change'), 'click');
  await settle();

  const notice = query(h.mount, (n) => (n.className || '').startsWith('mx-notice'));
  assert.match(notice.textContent, /NOT SENT/);
  assert.equal(h.calls.errors.length, 1);
  h.drawer.destroy();
});

/* ------------------------------------------------------------------ */
/* Drift and golden                                                    */
/* ------------------------------------------------------------------ */

test('no golden reads as "no golden saved", never as "no differences"', async () => {
  const h = harness();
  h.drawer.open();
  await settle();
  const summary = query(h.mount, (n) => (n.className || '').startsWith('mx-drift-summary'));
  assert.match(summary.textContent, /No golden saved/);
  h.drawer.destroy();
});

test('drift from golden shows the clean-feed entry first and in the operator language', async () => {
  const h = harness({
    getGolden: async () => snapshot([strip({ outputs: ['master'], displayName: 'CLAUDE-COMMS' })]),
  });
  h.drawer.open();
  await settle();
  const summary = query(h.mount, (n) => (n.className || '').startsWith('mx-drift-summary'));
  assert.match(summary.textContent, /1 CRITICAL/);
  const first = query(h.mount, (n) => (n.className || '').startsWith('mx-diff '));
  assert.match(first.textContent, /CRITICAL/);
  assert.match(first.textContent, /CLAUDE-COMMS IS NOW IN THE CLEAN FEED/);
  h.drawer.destroy();
});

test('capture golden takes two presses, writes application state and not the mixer', async () => {
  const h = harness();
  h.drawer.open();
  await settle();

  fire(buttonWithText(h.mount, 'Capture golden'), 'click');
  assert.deepEqual(h.calls.setGolden, [], 'one press only arms it');
  const notice = query(h.mount, (n) => (n.className || '').startsWith('mx-notice'));
  assert.match(notice.textContent, /press again to confirm|again to overwrite/i);

  fire(buttonWithText(h.mount, 'Capture golden'), 'click');
  await settle();
  assert.equal(h.calls.setGolden.length, 1);
  assert.deepEqual(h.calls.sendCommands, [], 'capturing golden must not touch the mixer');
  h.drawer.destroy();
});

test('a diff source supplied by the host is used instead of the local comparison', async () => {
  const h = harness({
    getDiffs: async () => [
      { kind: 'strip', target: 'cam22-1', label: 'CLAUDE-COMMS', field: 'outputs', golden: 'master', current: 'master, aux1', severity: 'critical' },
    ],
    getGolden: async () => snapshot(),
  });
  h.drawer.open();
  await settle();
  const first = query(h.mount, (n) => (n.className || '').startsWith('mx-diff '));
  assert.match(first.textContent, /IS NOW IN THE CLEAN FEED/);
  h.drawer.destroy();
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
  const notice = query(h.mount, (n) => (n.className || '').startsWith('mx-notice'));
  assert.match(notice.textContent, /out of date/);
  h.drawer.destroy();
});

test('with no state at all the drawer refuses to claim anything about the clean feed', async () => {
  const h = harness({
    getSnapshot: async () => {
      throw new Error('backend unreachable');
    },
  });
  h.drawer.open();
  await settle();
  const banner = query(h.mount, (n) => (n.className || '').startsWith('mx-banner'));
  assert.match(banner.textContent, /cannot tell you what is in the clean feed/);
  h.drawer.destroy();
});

test('a meterless strip is drawn as absent, not as full scale', async () => {
  const h = harness({ getSnapshot: async () => snapshot([strip({ metered: false })]) });
  h.drawer.open();
  await settle();
  const meter = query(h.mount, (n) => (n.className || '').includes('mx-meter-text'));
  assert.equal(meter.textContent, 'no meter');
  const fill = query(h.mount, (n) => (n.className || '').includes('mx-meter-fill'));
  assert.equal(fill.style.width, '0%');
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

test('the drawer renders the captured live frame and names CLAUDE-COMMS in the clean feed', async () => {
  // demo-fixture.js is the real 31 July 2026 frame from the dev event. If the
  // shape the drawer expects ever stops matching a genuine snapshot, this is
  // the test that notices.
  const h = harness({ getSnapshot: async () => LIVE_FIXTURE });
  h.drawer.open();
  await settle();

  const rows = queryAll(h.mount, (n) => (n.className || '').startsWith('mx-row'));
  assert.equal(rows.length, 54, 'one row per strip in the live frame');

  const comms = crosspoint(h.mount, 'cam22-1', 'aux1');
  assert.equal(comms.getAttribute('aria-checked'), 'true', 'the live routing puts commentary in the clean feed');

  const banner = query(h.mount, (n) => (n.className || '').startsWith('mx-banner'));
  assert.match(banner.textContent, /CLAUDE-COMMS/);
  // cam22-1 was muted in the captured frame, so it is called out as routed but
  // muted rather than as audible.
  assert.match(banner.textContent, /muted: .*CLAUDE-COMMS/);
  assert.deepEqual(h.calls.sendCommands, [], 'rendering a real frame writes nothing');
  h.drawer.destroy();
});

test('the drawer refuses to be built without the options it needs', () => {
  const { mount } = createTestDom();
  assert.throws(() => createMixerDrawer({}), /needs opts.mount/);
  assert.throws(() => createMixerDrawer({ mount }), /needs opts.getSnapshot/);
  assert.throws(() => createMixerDrawer({ mount, getSnapshot: async () => {} }), /needs opts.sendCommands/);
});
