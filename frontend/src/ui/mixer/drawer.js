/**
 * The mixer drawer: the operator-facing view of the M2L-X advanced audio mixer.
 *
 * Owner: WP-M4. Implements createMixerDrawer from ./contract.js. This module is
 * DOM only — every decision it renders comes from ./model.js, which is pure and
 * tested. It deliberately does NOT import ./mixer.css; ./index.js does that, so
 * that this file can be loaded by `node --test` (see drawer.test.js).
 *
 * ======================= WHAT AN OPERATOR SEES ==============================
 *
 * Two columns: PGM and CLN. CLN is the clean feed the client receives — the bus
 * M2L-X calls "AUX" on the mixer surface and "cln" on the output list, which
 * nothing in Sony's UI says are the same bus. EVERY STRIP DEFAULTS TO
 * ["master","aux1","aux2"], so an unmuted commentary input is in the client's
 * clean feed until somebody corrects its routing. The five remaining buses are
 * behind "Show all buses"; they cannot put commentary on air, so they do not get
 * a column by default.
 *
 * ======================= WHAT IT WILL AND WILL NOT WRITE ====================
 *
 * The only write this drawer offers is set_routing on a crosspoint, and it is
 * INSTANT: one click, one write, no staging and no Apply. Nothing is sent on
 * open, close, update, arm or destroy.
 *
 * Instant does not mean unguarded. set_routing is an ABSOLUTE REPLACE of a
 * strip's whole bus set, so every click re-reads the mixer and computes the
 * outgoing set from THAT frame — never from what is on screen, which is up to a
 * second old on a shared desk. If the view is stale or the re-read fails, the
 * click is REFUSED and the refusal is printed in the cell that was clicked. See
 * onCrosspoint.
 *
 * MUTE AND FADER ARE READ-ONLY HERE, DELIBERATELY. internal/mixer/mixer.go says
 * of SetInputMuted "THE ARGUMENT SHAPE IS NOT MEASURED", and of SetChFader that
 * whether the wire takes a per-channel pair or one message per channel is
 * unconfirmed. Offering a control whose argument shape is a guess, on a mixer
 * feeding a live clean feed, is not a control — it is a coin toss. Both are
 * displayed; neither can be written from here until WP-M1 has measured them.
 * Routing is also the correct fix: pulling a fader leaves the strip routed to
 * aux1, so the next state restore brings it back audible in the clean feed.
 *
 * ======================= WHAT IS WITHDRAWN, NOT DELETED =====================
 *
 * The golden/drift panel is GONE FROM THIS GUI at the operator's request. None
 * of the machinery behind it has been removed or weakened: internal/mixer's
 * golden.go and Compare are untouched, and so are compareSnapshots, sortDiffs
 * and diffHeadline in ./model.js and every one of their tests. What changed is
 * that this file no longer renders them and no longer calls opts.getGolden or
 * opts.getDiffs. Those options are still REQUIRED by the contract and still
 * wired by the host, so putting the panel back is a change to this file alone.
 */

import { ALL_BUSES, CLEAN_FEED_BUS, busLabel } from './contract.js';
import {
  METER_FLOOR_DB,
  buildMatrixModel,
  busListText,
  createWriteGate,
  describeCrosspointChange,
  formatDb,
  meterPercent,
  meterZone,
  routingAfterToggle,
  routingCommand,
  viewFreshness,
} from './model.js';

/**
 * GLYPHS encode state in SHAPE as well as colour.
 *
 * This is read in a dim room by someone in a hurry, possibly on a monochrome
 * monitor a metre away, possibly colourblind: a lit crosspoint that is only a
 * different colour is not lit at all. All of these are in the base Segoe UI /
 * system font set — the facility has no outbound web access, so nothing here
 * may depend on a webfont or an emoji font.
 */
const GLYPHS = Object.freeze({
  routed: '■', // BLACK SQUARE - crosspoint on
  notRouted: '□', // WHITE SQUARE - crosspoint off
  routedClean: '◆', // BLACK DIAMOND - on, and on the clean feed
  notRoutedClean: '◇', // WHITE DIAMOND - off, clean feed column
  allowOn: '●', // BLACK CIRCLE - writes are live
  allowOff: '○', // WHITE CIRCLE - read-only
});

/** CONFIRM_WINDOW_MS is how long a sent change may take to appear in a
 * snapshot before the drawer calls it unconfirmed. update() arrives at about
 * 1 Hz, so this is four frames. A resolved sendCommands means SENT, never
 * APPLIED — the only proof is the state reading back. */
const CONFIRM_WINDOW_MS = 4000;

/**
 * DEFAULT_BUSES are the columns an operator sees without asking.
 *
 * They are exactly the buses that LEAVE THE BUILDING: master is the PGM output
 * and aux1 is the CLN output. Everything else is a monitor or, in aux2's case,
 * a bus nothing accepts as a source. Five extra columns of things that cannot
 * reach the client cost the two that can most of the width of the screen.
 */
const DEFAULT_BUSES = Object.freeze(['master', CLEAN_FEED_BUS]);

/**
 * COLUMN_LABELS are the column headings for the two default buses.
 *
 * Deliberately NOT busLabel(): with only two columns, "PGM" and "CLN" are the
 * names an operator thinks in and the names printed on the router. The full
 * labels ("aux1 (CLN - clean feed)") are still used everywhere a bus is named
 * in prose — the confirmation lines especially, where a bare "aux1" would let
 * "CONFIRMED: CLAUDE-COMMS -> master, aux1" read as success while confirming
 * commentary in the client's clean feed. A COLUMN carries its meaning in the
 * band, the ruling and the diamond glyph as well as in its heading; a SENTENCE
 * carries it only in the words.
 */
const COLUMN_LABELS = Object.freeze({ master: 'PGM', [CLEAN_FEED_BUS]: 'CLN' });

/** columnLabel is the heading for a bus column. */
function columnLabel(bus) {
  return Object.prototype.hasOwnProperty.call(COLUMN_LABELS, bus) ? COLUMN_LABELS[bus] : busLabel(bus);
}

/** FILTERS are the row filters. 54 strips is too many to scan under pressure;
 * "clean feed" answers the only question that matters in a hurry. */
const FILTERS = Object.freeze([
  { id: 'all', label: 'All strips' },
  { id: 'clean', label: 'In clean feed' },
  { id: 'active', label: 'Unmuted or audible' },
]);

/**
 * createMixerDrawer builds the mixer drawer.
 *
 * The returned drawer starts CLOSED and DISARMED and has not written anything.
 * See ./contract.js for the option and return shapes.
 *
 * One option beyond the contract is honoured when the host supplies it, and is
 * optional in every sense — omit it and the drawer behaves exactly as the
 * contract describes:
 *
 *   onArmedChange(armed)   told whenever the armed state changes, INCLUDING the
 *                          forced disarm on close, so that the host's own idea
 *                          of armed cannot drift from the drawer's.
 *
 * opts.getGolden and opts.setGolden are still required by the contract and are
 * still validated here, but this build never calls them: the drift panel has
 * been withdrawn from the GUI. See the file header.
 *
 * @param {import('./contract.js').MixerDrawerOptions} opts
 * @returns {import('./contract.js').MixerDrawer}
 */
export function createMixerDrawer(opts) {
  const o = opts || {};
  const mount = o.mount;
  if (!mount || typeof mount.appendChild !== 'function') {
    throw new Error('mixer: createMixerDrawer needs opts.mount');
  }
  for (const fn of ['getSnapshot', 'sendCommands', 'getGolden', 'setGolden']) {
    if (typeof o[fn] !== 'function') throw new Error(`mixer: createMixerDrawer needs opts.${fn}`);
  }

  const doc = mount.ownerDocument || globalThis.document;
  if (!doc || typeof doc.createElement !== 'function') {
    throw new Error('mixer: createMixerDrawer needs a document');
  }

  const onError = (err, context) => {
    if (typeof o.onError === 'function') o.onError(err instanceof Error ? err : new Error(String(err)), context);
  };

  /* ---------------------------------------------------------------- state */

  const state = {
    open: false,
    armed: false,
    destroyed: false,
    snapshot: null,
    model: buildMatrixModel(null),
    filter: 'all',
    showAllBuses: false,
    lastUpdateAt: null,
    notice: '',
    noticeKind: 'info',
    /**
     * busy is true from the moment a crosspoint click starts its re-read until
     * the write has been sent or refused. A second click in that window is
     * refused rather than queued: two set_routing commands for one strip built
     * from two different re-reads race, and set_routing REPLACES, so the loser
     * silently reasserts the bus set the winner just changed.
     */
    busy: false,
    /**
     * cellNote is the refusal or result printed IN THE CELL THAT WAS CLICKED.
     *
     * With no Apply button there is no longer one place a refusal can live, and
     * a notice at the top of a scrolled 54-row matrix is a refusal the operator
     * does not see. One note at a time: {key, text, kind}, keyed by cellKey().
     */
    cellNote: null,
    /** sent changes awaiting confirmation from a following snapshot */
    awaiting: [],
    restoreFocus: null,
  };

  const gate = createWriteGate({
    sendCommands: (cmds) => o.sendCommands(cmds),
    isArmed: () => state.armed,
    viewIsFresh: () => viewFreshness(state.lastUpdateAt, Date.now()),
    onError,
  });

  /* ------------------------------------------------------------------ DOM */

  const originalChildren = [];
  while (mount.firstChild) {
    originalChildren.push(mount.firstChild);
    mount.removeChild(mount.firstChild);
  }

  const ui = buildShell(doc);
  mount.appendChild(ui.root);
  ui.root.hidden = true;

  /** element registry for in-place updates; rebuilt only when the strip set or
   * the visible bus set changes */
  let rowEls = new Map();
  let busEls = new Map();
  let builtStructureKey = null;

  /* -------------------------------------------------------------- wiring */

  ui.closeBtn.addEventListener('click', () => api.close());
  ui.scrim.addEventListener('click', () => api.close());

  ui.armBtn.addEventListener('click', () => {
    setArmed(!state.armed, 'operator pressed Allow changes');
  });

  ui.busToggle.addEventListener('click', () => {
    state.showAllBuses = !state.showAllBuses;
    render();
  });

  for (const f of FILTERS) {
    ui.filterBtns.get(f.id).addEventListener('click', () => {
      state.filter = f.id;
      render();
    });
  }

  const onKeyDown = (ev) => {
    if (!state.open) return;
    const key = ev && ev.key;
    if (key === 'Escape') {
      // The drawer is modal while open, so it consumes its own Escape rather
      // than letting the host also act on it.
      if (typeof ev.stopPropagation === 'function') ev.stopPropagation();
      if (typeof ev.preventDefault === 'function') ev.preventDefault();
      api.close();
      return;
    }
    if (key === 'Tab') trapFocus(ev);
  };
  doc.addEventListener('keydown', onKeyDown, true);

  const staleTimer = startStaleTimer();

  /* ----------------------------------------------------------- behaviour */

  function setArmed(armed, why) {
    const next = armed === true;
    if (next === state.armed) return;
    state.armed = next;
    state.cellNote = null;
    if (typeof o.onArmedChange === 'function') o.onArmedChange(next);
    notice(
      next
        ? 'CHANGES ARE LIVE. A crosspoint click is now sent to the mixer immediately - there is no Apply step.'
        : `Changes LOCKED (${why}). The matrix is read-only again; nothing was sent.`,
      next ? 'warn' : 'info',
    );
    render();
  }

  /**
   * onCrosspoint is the operator gesture on a crosspoint, and the ONLY path in
   * this module that reaches the write gate.
   *
   * ===================== ONE CLICK IS ONE WRITE, AND WHY IT RE-READS =========
   *
   * set_routing REPLACES a strip's whole bus set. The click says what ONE bus
   * should become; every other bus in the command is copied from the routing
   * this drawer happens to be holding. If that routing is old, the command is
   * one intended change wrapped in a rollback of everything else on that strip
   * — applied to a desk that is on air. That is true whether the operator
   * pressed Apply thirty seconds later or clicked once; removing the staging
   * step removed a delay, not the hazard.
   *
   * So a click has three defences, in this order:
   *
   *   1. REFUSE A STALE VIEW, before anything is read or planned. If updates
   *      have stopped, nothing is sent and the operator is told why, IN THE
   *      CELL THEY CLICKED, with the age.
   *   2. PLAN FROM A FRESH FRAME. The desired set is computed from the snapshot
   *      fetched by THIS click, never from the frame on screen when it was
   *      made. If the base moved, the operator is told after the fact — their
   *      intent is still expressed exactly, on the routing that is really there.
   *      A re-read that FAILS refuses the click; it does not fall back.
   *   3. THE GATE. createWriteGate re-checks armed AND freshness at the moment
   *      of the write, so none of this depends on this function remembering to.
   *
   * The refusal is a property of this path and of the gate, never of a disabled
   * attribute: a keyboard activation or a programmatic click arrives here just
   * the same, and aria-disabled stops neither.
   */
  async function onCrosspoint(stripName, bus) {
    const key = cellKey(stripName, bus);

    if (!state.armed) {
      cellNote(key, `LOCKED - press "Allow changes" first. Nothing was sent.`, 'info');
      notice(
        `Locked: changes are not allowed, so ${busLabel(bus)} on this strip cannot be changed. ` +
          'Press "Allow changes" in the header first.',
        'info',
      );
      render();
      return;
    }

    if (state.busy) {
      cellNote(key, 'NOT SENT - another change is still being sent. Try again in a moment.', 'warn');
      render();
      return;
    }

    const before = state.model.rows.find((r) => r.name === stripName);
    if (!before) return;
    // What the operator asked for, decided from the cell they could see. Only
    // the DIRECTION comes from the screen; the bus set it is applied to does
    // not.
    const desiredRouted = !before.outputs.includes(bus);

    // 1. Freshness, before anything is read or planned.
    const fresh = viewFreshness(state.lastUpdateAt, Date.now());
    if (!fresh.fresh) {
      cellNote(key, `NOT SENT - ${fresh.text.toUpperCase()}. Nothing was written.`, 'warn');
      notice(
        `NOT SENT - ${fresh.text.toUpperCase()}. Nothing was written. ` +
          "set_routing replaces a strip's WHOLE bus set, so acting on a view this old would send every other bus " +
          'back to what it was when the feed stopped - to a live desk. ' +
          'Wait for the view to go live again and click it again.',
        'warn',
      );
      render();
      return;
    }

    state.busy = true;
    cellNote(key, 'Reading the mixer before writing...', 'info');
    render();
    try {
      // 2. Re-read, and plan from what came back.
      let snap;
      try {
        snap = await o.getSnapshot();
        if (!snap || typeof snap !== 'object') throw new Error('the mixer returned no state');
      } catch (err) {
        onError(err, 'mixer: getSnapshot before a crosspoint write');
        refuse(
          key,
          'NOT SENT - the mixer could not be re-read.',
          'NOT SENT: the mixer could not be re-read immediately before writing, so the routing this change would ' +
            'replace is unknown. Nothing was written.',
        );
        return;
      }
      applySnapshot(snap);
      if (!state.model.hasData) {
        refuse(
          key,
          'NOT SENT - the mixer reported no state.',
          'NOT SENT: the mixer reported no state when re-read, so there is no routing to build a replacement from. ' +
            'Nothing was written.',
        );
        return;
      }

      const row = state.model.rows.find((r) => r.name === stripName);
      if (!row) {
        refuse(
          key,
          'NOT SENT - this strip is not in the state just read back.',
          `NOT SENT: ${before.label} is not present in the state just read back from the mixer, so there is no ` +
            'routing to replace. Nothing was written.',
        );
        return;
      }

      const desired = routingAfterToggle(row.outputs, bus, desiredRouted);
      if (sameSet(desired, row.outputs)) {
        // Somebody else just made the same correction. Not an error, and
        // emphatically not a write.
        cellNote(key, 'Nothing sent - already in that state on the mixer.', 'info');
        notice(
          `Nothing sent: on the state just read back, ${describeCrosspointChange(row, bus, desiredRouted)} is ` +
            'already in effect.',
          'info',
        );
        return;
      }

      const moved = !sameSet(before.outputs, row.outputs);

      // 3. The gate. It re-checks armed and freshness itself.
      const result = await gate.submit(
        [routingCommand(stripName, desired)],
        'operator clicked a crosspoint',
      );
      if (!result.sent) {
        refuse(key, `NOT SENT - ${result.reason}`, `NOT SENT: ${result.reason}`);
        return;
      }

      state.awaiting = [
        {
          strip: stripName,
          label: row.label,
          desired: desired.slice(),
          base: row.outputs.slice(),
          deadline: Date.now() + CONFIRM_WINDOW_MS,
          state: 'pending',
        },
      ];
      cellNote(key, 'SENT - waiting for the mixer to report it back.', 'warn');
      const movedWord = moved
        ? ' The desk had moved since the view you clicked on: your change was planned onto the routing that is ' +
          'actually there, so no other bus was rolled back.'
        : '';
      notice(
        `Sent: ${describeCrosspointChange(row, bus, desiredRouted)}. ` +
          `SENT IS NOT APPLIED - waiting for the mixer to report it back.${movedWord}`,
        'warn',
      );
    } finally {
      state.busy = false;
      render();
    }
  }

  /** refuse records one refusal in the clicked cell AND in the notice line. */
  function refuse(key, short, long) {
    cellNote(key, short, 'warn');
    notice(long, 'warn');
  }

  function cellNote(key, text, kind) {
    state.cellNote = { key, text, kind: kind || 'info' };
  }

  function notice(text, kind) {
    state.notice = text;
    state.noticeKind = kind || 'info';
  }

  function startStaleTimer() {
    // Reads the clock and repaints two labels. It never fetches and never
    // writes; the contract's "must not poll" is about getSnapshot.
    if (typeof setInterval !== 'function') return null;
    const t = setInterval(() => {
      if (!state.open || state.destroyed) return;
      renderFreshness();
      // renderArm too, so that when the feed stops the header says a click will
      // be refused without waiting for an update() that is not coming. This is
      // the only case where the drawer changes without new state, and it is
      // exactly the case that matters.
      renderArm();
    }, 1000);
    if (t && typeof t.unref === 'function') t.unref();
    return t;
  }

  /* -------------------------------------------------------------- render */

  function render() {
    if (!state.open || state.destroyed) return;
    renderCleanLine();
    renderArm();
    renderNotice();
    renderMatrix();
    renderFreshness();
  }

  /**
   * renderCleanLine is what is left of the clean-feed banner: one line in the
   * header instead of a block at the top of the body.
   *
   * The block was removed as clutter. This line is not decoration — with no
   * banner, a drawer that has NO STATE would otherwise show an empty matrix,
   * and an empty matrix reads as "nothing is in the clean feed", which is a
   * claim the drawer cannot make. So the unknown case is spelled out, and the
   * known case gives the count with the names in the title.
   */
  function renderCleanLine() {
    const l = ui.cleanLine;
    if (!state.model.hasData) {
      l.className = 'mx-cleanline mx-cleanline--unknown';
      l.textContent = 'CLN: unknown - no mixer state';
      l.setAttribute('title', 'This drawer cannot tell you what is in the clean feed: no state has been received.');
      return;
    }
    const routed = state.model.cleanFeed;
    const audible = routed.filter((r) => !r.muted);
    if (routed.length === 0) {
      l.className = 'mx-cleanline mx-cleanline--ok';
      l.textContent = `${GLYPHS.notRoutedClean} CLN: nothing routed`;
      l.setAttribute('title', `Nothing is routed to ${busLabel(CLEAN_FEED_BUS)}, the CLN output the client receives.`);
      return;
    }
    const muted = routed.filter((r) => r.muted);
    l.className = audible.length > 0 ? 'mx-cleanline mx-cleanline--alert' : 'mx-cleanline mx-cleanline--warn';
    l.textContent =
      `${audible.length > 0 ? GLYPHS.routedClean : GLYPHS.notRoutedClean} ` +
      `CLN: ${routed.length} routed, ${audible.length} unmuted`;
    // BOTH groups, always. A strip that is in the clean feed but muted is one
    // un-mute away from being in it audibly, and the captured live frame is
    // exactly that case: CLAUDE-COMMS is routed to aux1 and muted. Naming only
    // the audible ones would have left it out of the only place it is named.
    const parts = [];
    if (audible.length > 0) parts.push(`IN THE CLEAN FEED AND UNMUTED: ${audible.map((r) => r.label).join(', ')}.`);
    if (muted.length > 0) parts.push(`Routed to the clean feed but muted: ${muted.map((r) => r.label).join(', ')}.`);
    parts.push(`${busLabel(CLEAN_FEED_BUS)} is the CLN output the client receives.`);
    l.setAttribute('title', parts.join(' '));
  }

  function renderArm() {
    // The label does not change with the state; the GLYPH, the aria-pressed and
    // the state word beside it do. A toggle whose label becomes the opposite
    // action is one an operator reads backwards under pressure.
    ui.armBtn.textContent = `${state.armed ? GLYPHS.allowOn : GLYPHS.allowOff} Allow changes`;
    ui.armBtn.setAttribute('aria-pressed', String(state.armed));
    ui.armBtn.className = state.armed ? 'mx-btn mx-allow mx-allow--on' : 'mx-btn mx-allow';
    ui.armBtn.setAttribute(
      'aria-label',
      state.armed
        ? 'Allow changes: ON. Crosspoint clicks are written to the live mixer immediately. Activate to lock.'
        : 'Allow changes: OFF. The matrix is read-only. Activate to allow writes.',
    );

    const fresh = viewFreshness(state.lastUpdateAt, Date.now());
    if (!state.armed) {
      ui.armState.textContent = 'READ-ONLY';
      ui.armState.className = 'mx-armstate';
      ui.armState.setAttribute('title', 'Crosspoints are locked; nothing here can reach the mixer.');
    } else if (!fresh.fresh) {
      // Shown, not relied on. The refusal itself lives in onCrosspoint and in
      // the write gate; this is so the operator learns before they click rather
      // than after.
      ui.armState.textContent = `WRITES LIVE - BLOCKED (${fresh.text})`;
      ui.armState.className = 'mx-armstate mx-armstate--blocked';
      ui.armState.setAttribute('title', 'A click will be refused until the view is live again.');
    } else {
      ui.armState.textContent = 'WRITES LIVE';
      ui.armState.className = 'mx-armstate mx-armstate--armed';
      ui.armState.setAttribute('title', 'A crosspoint click is sent to the live mixer immediately.');
    }

    renderAwaiting();
  }

  /**
   * renderAwaiting is the HIGHEST-STAKES READ IN THIS DRAWER.
   *
   * It appears immediately after a write to a live desk and it is what the
   * operator checks to decide the change did what they meant. Every bus in it
   * goes through busListText, never a bare join and never the column headings:
   * "CONFIRMED: CLAUDE-COMMS -> master, aux1" reads as success while confirming
   * that commentary is in the client's clean feed, which is the single failure
   * this whole drawer exists to prevent. Rendered through the labels it reads
   * "... -> master (PGM), aux1 (CLN - clean feed)" and cannot be misread.
   */
  function renderAwaiting() {
    clear(ui.awaitList);
    ui.awaitBox.hidden = state.awaiting.length === 0;
    for (const a of state.awaiting) {
      const buses = busListText(a.desired);
      const text =
        a.state === 'confirmed'
          ? `CONFIRMED by the mixer: ${a.label} -> ${buses}`
          : a.state === 'failed'
            ? `NOT CONFIRMED: ${a.label} was sent as ${buses} but the mixer has not reported it. The write may not have taken effect.`
            : `Sent, awaiting confirmation: ${a.label} -> ${buses}`;
      ui.awaitList.appendChild(
        el(doc, 'li', {
          className: `mx-await mx-await--${a.state}`,
          text,
        }),
      );
    }
  }

  function renderNotice() {
    ui.notice.textContent = state.notice;
    ui.notice.className = `mx-notice mx-notice--${state.noticeKind}`;
    ui.notice.hidden = state.notice === '';
  }

  function renderMatrix() {
    if (!state.model.hasData) {
      ui.matrixNote.hidden = false;
      ui.matrixNote.textContent = 'No mixer state to show.';
      return;
    }
    ui.matrixNote.hidden = true;

    const key = `${state.model.structureKey}::${state.showAllBuses}`;
    if (builtStructureKey !== key) {
      buildMatrixRows();
      builtStructureKey = key;
    }
    for (const f of FILTERS) {
      ui.filterBtns.get(f.id).setAttribute('aria-pressed', String(state.filter === f.id));
      ui.filterBtns.get(f.id).className = state.filter === f.id ? 'mx-chip mx-chip--on' : 'mx-chip';
    }
    ui.busToggle.textContent = state.showAllBuses ? 'Hide the other buses' : 'Show all buses';
    ui.busToggle.setAttribute('aria-pressed', String(state.showAllBuses));
    ui.busToggle.className = state.showAllBuses ? 'mx-chip mx-chip--on' : 'mx-chip';
    updateLive();
  }

  /** visibleBuses is the column set: the two that leave the building, or all
   * seven once the operator asks for them. */
  function visibleBuses() {
    return state.showAllBuses ? ALL_BUSES : DEFAULT_BUSES;
  }

  function buildMatrixRows() {
    clear(ui.thead);
    clear(ui.tbody);
    rowEls = new Map();
    busEls = new Map();

    const buses = visibleBuses();

    const hr = el(doc, 'tr');
    // "Input", not "Strip": "strip" is desk jargon and the rest of this
    // application calls the thing an input.
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--strip', text: 'Input', attrs: { scope: 'col' } }));
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--meter', text: 'Peak hold (dBFS)', attrs: { scope: 'col' } }));
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--state', text: 'Mute / fader', attrs: { scope: 'col' } }));
    for (const bus of buses) {
      const clean = bus === CLEAN_FEED_BUS;
      const th = el(doc, 'th', {
        className: `mx-th mx-th--bus${clean ? ' mx-col-clean' : ''}`,
        attrs: { scope: 'col', 'data-bus': bus, title: busLabel(bus) },
      });
      th.appendChild(el(doc, 'span', { className: 'mx-th-name', text: columnLabel(bus) }));
      const sub = el(doc, 'span', { className: 'mx-th-sub' });
      th.appendChild(sub);
      const meter = el(doc, 'span', { className: 'mx-busmeter' });
      const fill = el(doc, 'span', { className: 'mx-busmeter-fill' });
      meter.appendChild(fill);
      th.appendChild(meter);
      hr.appendChild(th);
      busEls.set(bus, { sub, fill });
    }
    ui.thead.appendChild(hr);

    for (const row of state.model.rows) {
      const tr = el(doc, 'tr', { className: 'mx-row', attrs: { 'data-strip': row.name } });

      const nameCell = el(doc, 'th', { className: 'mx-cell mx-cell--name', attrs: { scope: 'row' } });
      const label = el(doc, 'span', { className: 'mx-strip-label', text: row.label });
      const wire = el(doc, 'span', { className: 'mx-strip-wire', text: row.name });
      nameCell.appendChild(label);
      nameCell.appendChild(wire);
      tr.appendChild(nameCell);

      const meterCell = el(doc, 'td', { className: 'mx-cell mx-cell--meter' });
      const meterBox = el(doc, 'span', { className: 'mx-meter' });
      const barL = el(doc, 'span', { className: 'mx-meter-bar' });
      const fillL = el(doc, 'span', { className: 'mx-meter-fill' });
      barL.appendChild(fillL);
      const barR = el(doc, 'span', { className: 'mx-meter-bar' });
      const fillR = el(doc, 'span', { className: 'mx-meter-fill' });
      barR.appendChild(fillR);
      meterBox.appendChild(barL);
      meterBox.appendChild(barR);
      const meterText = el(doc, 'span', { className: 'mx-meter-text' });
      meterCell.appendChild(meterBox);
      meterCell.appendChild(meterText);
      tr.appendChild(meterCell);

      const stateCell = el(doc, 'td', { className: 'mx-cell mx-cell--state' });
      const muteEl = el(doc, 'span', { className: 'mx-mute' });
      const faderEl = el(doc, 'span', { className: 'mx-fader' });
      stateCell.appendChild(muteEl);
      stateCell.appendChild(faderEl);
      tr.appendChild(stateCell);

      const cells = new Map();
      for (const bus of buses) {
        const clean = bus === CLEAN_FEED_BUS;
        const td = el(doc, 'td', { className: `mx-cell mx-cell--x${clean ? ' mx-col-clean' : ''}` });
        const btn = el(doc, 'button', {
          className: 'mx-x',
          attrs: { type: 'button', role: 'switch', 'data-strip': row.name, 'data-bus': bus },
        });
        const glyph = el(doc, 'span', { className: 'mx-x-glyph', attrs: { 'aria-hidden': 'true' } });
        const text = el(doc, 'span', { className: 'mx-x-text', attrs: { 'aria-hidden': 'true' } });
        btn.appendChild(glyph);
        btn.appendChild(text);
        btn.addEventListener('click', () => {
          void onCrosspoint(row.name, bus);
        });
        // The note that carries a refusal, in the cell that was clicked. There
        // is no Apply button to put it on any more, and a notice at the top of
        // a scrolled matrix is a refusal nobody reads.
        const note = el(doc, 'span', { className: 'mx-x-note', attrs: { role: 'alert' } });
        note.hidden = true;
        td.appendChild(btn);
        td.appendChild(note);
        tr.appendChild(td);
        cells.set(bus, { btn, glyph, text, note });
      }

      ui.tbody.appendChild(tr);
      rowEls.set(row.name, { tr, label, wire, fillL, fillR, meterText, muteEl, faderEl, cells });
    }
  }

  /** updateLive repaints the values that change at 1 Hz, in place, so that a
   * keyboard user's focus survives an update. */
  function updateLive() {
    for (const busView of state.model.buses) {
      const refs = busEls.get(busView.name);
      if (!refs) continue;
      const bits = [];
      if (!busView.known) bits.push('not reported');
      if (busView.muted) bits.push('BUS MUTED');
      if (busView.noEgress) bits.push('no egress: nothing receives this bus');
      if (busView.metered) bits.push(`peak ${formatDb(busView.peakMax)}`);
      else bits.push('no meter');
      refs.sub.textContent = bits.join(' · ');
      refs.fill.style.width = `${busView.metered ? meterPercent(busView.peakMax) : 0}%`;
    }

    for (const row of state.model.rows) {
      const refs = rowEls.get(row.name);
      if (!refs) continue;

      const visible = matchesFilter(row);
      refs.tr.hidden = !visible;
      refs.tr.className = `mx-row${row.audibleOnCleanFeed ? ' mx-row--onair' : ''}`;
      if (!visible) continue;

      refs.label.textContent = row.label;
      refs.wire.textContent = row.name;

      if (row.metered) {
        refs.fillL.style.width = `${meterPercent(row.peakHold[0])}%`;
        refs.fillR.style.width = `${meterPercent(row.peakHold[1])}%`;
        const zone = meterZone(row.peakMax);
        refs.fillL.className = `mx-meter-fill mx-zone--${meterZone(row.peakHold[0])}`;
        refs.fillR.className = `mx-meter-fill mx-zone--${meterZone(row.peakHold[1])}`;
        refs.meterText.textContent = zone === 'silence' ? 'silent' : formatDb(row.peakMax);
        refs.meterText.className = `mx-meter-text mx-zone--${zone}`;
      } else {
        // metered:false means NO METER DATA. Drawing the default would show
        // full scale, which is a lie an operator would act on.
        refs.fillL.style.width = '0%';
        refs.fillR.style.width = '0%';
        refs.meterText.textContent = 'no meter';
        refs.meterText.className = 'mx-meter-text mx-meter-text--absent';
      }

      refs.muteEl.textContent = row.muted ? 'MUTED' : 'unmuted';
      refs.muteEl.className = row.muted ? 'mx-mute mx-mute--on' : 'mx-mute';
      refs.faderEl.textContent = `fader ${row.faderText}${row.subChMode ? ` · ${row.subChMode}` : ''}`;

      for (const cell of row.cells) {
        const cRefs = refs.cells.get(cell.bus);
        if (!cRefs) continue;
        paintCrosspoint(cRefs, row, cell);
      }
    }
  }

  function paintCrosspoint(refs, row, cell) {
    const shown = cell.routed;
    const key = cellKey(row.name, cell.bus);
    const note = state.cellNote && state.cellNote.key === key ? state.cellNote : null;

    const glyph = cell.cleanFeed
      ? shown
        ? GLYPHS.routedClean
        : GLYPHS.notRoutedClean
      : shown
        ? GLYPHS.routed
        : GLYPHS.notRouted;

    // The glyph is always the state, never the lock: an operator must be able
    // to read the routing whether or not changes are allowed. Locked cells are
    // marked instead by a dashed outline (a SHAPE, not a colour), by
    // aria-disabled, and by READ-ONLY in the header.
    refs.glyph.textContent = glyph;
    refs.text.textContent = cell.cleanFeed && shown ? 'CLN' : '';

    const classes = ['mx-x'];
    classes.push(shown ? 'mx-x--on' : 'mx-x--off');
    if (cell.cleanFeed) classes.push('mx-x--clean');
    if (cell.noEgress) classes.push('mx-x--noegress');
    if (!state.armed) classes.push('mx-x--locked');
    if (note && note.kind === 'warn') classes.push('mx-x--refused');
    refs.btn.className = classes.join(' ');

    refs.note.textContent = note ? note.text : '';
    refs.note.className = note ? `mx-x-note mx-x-note--${note.kind}` : 'mx-x-note';
    refs.note.hidden = note === null;

    refs.btn.setAttribute('aria-checked', String(shown));
    refs.btn.setAttribute('aria-disabled', String(!state.armed));
    const lockWord = state.armed ? '' : '. Locked: changes are not allowed';
    const noteWord = note ? `. ${note.text}` : '';
    refs.btn.setAttribute(
      'aria-label',
      `${row.label} to ${cell.label}: ${cell.routed ? 'routed' : 'not routed'}${lockWord}${noteWord}`,
    );
    refs.btn.setAttribute('title', `${row.label} (${row.name}) → ${cell.label}`);
  }

  function matchesFilter(row) {
    if (state.filter === 'clean') return row.inCleanFeed;
    // "Unmuted or audible" keeps a strip that is muted but still producing a
    // reading: that combination is worth a second look, not a hidden row.
    if (state.filter === 'active') return !row.muted || (row.metered && row.peakMax > METER_FLOOR_DB);
    return true;
  }

  function renderFreshness() {
    const f = viewFreshness(state.lastUpdateAt, Date.now());
    const taken = state.model.takenAt ? ` · frame ${state.model.takenAt}` : '';
    ui.freshness.textContent = f.fresh ? `Live · updating${taken}` : `${f.text}${taken}`;
    ui.freshness.className = f.fresh ? 'mx-fresh' : 'mx-fresh mx-fresh--stale';
  }

  /* ------------------------------------------------------- confirmation */

  function reconcileAwaiting() {
    if (state.awaiting.length === 0) return;
    const now = Date.now();
    let changed = false;
    for (const a of state.awaiting) {
      if (a.state !== 'pending') continue;
      const row = state.model.rows.find((r) => r.name === a.strip);
      if (row && sameSet(row.outputs, a.desired)) {
        a.state = 'confirmed';
        changed = true;
      } else if (now > a.deadline) {
        a.state = 'failed';
        changed = true;
      }
    }
    if (changed && state.awaiting.every((a) => a.state === 'confirmed')) {
      notice('The mixer has reported the change back: it is in effect.', 'info');
      const done = state.awaiting;
      later(() => {
        if (state.destroyed) return;
        if (state.awaiting === done) {
          state.awaiting = [];
          // The "SENT - waiting" note in the cell goes with it. Left behind it
          // would sit on a crosspoint that has long since been confirmed and
          // read as if a write were still outstanding.
          state.cellNote = null;
          render();
        }
      }, 4000);
    }
  }

  /* ------------------------------------------------------------ lifecycle */

  const api = {
    /**
     * open shows the drawer and refreshes from getSnapshot. It is READ-ONLY:
     * opening never writes, and it never arms.
     */
    open() {
      if (state.destroyed || state.open) return;
      state.open = true;
      state.restoreFocus = doc.activeElement || null;
      ui.root.hidden = false;
      render();
      if (typeof ui.closeBtn.focus === 'function') ui.closeBtn.focus();

      void (async () => {
        try {
          const snap = await o.getSnapshot();
          applySnapshot(snap);
        } catch (err) {
          onError(err, 'mixer: getSnapshot on open');
          notice('Could not read the mixer. What is shown may be out of date.', 'warn');
        }
        render();
      })();
    },

    /**
     * close hides the drawer and LOCKS changes again.
     *
     * The contract says close "must not disarm silently"; this task requires
     * that it always disarm, because a drawer left able to write is a foot-gun.
     * Both are satisfied by disarming LOUDLY: the operator is told, the host is
     * told through onArmedChange when it supplied one, and the notice is still
     * on screen the next time the drawer is opened. What close must never do is
     * leave the host believing writes are still allowed.
     */
    close() {
      if (state.destroyed || !state.open) return;
      if (state.armed) setArmed(false, 'the drawer was closed');
      state.cellNote = null;
      state.open = false;
      ui.root.hidden = true;
      const target = state.restoreFocus;
      state.restoreFocus = null;
      if (target && typeof target.focus === 'function') target.focus();
    },

    isOpen() {
      return state.open;
    },

    /**
     * update feeds live state in. Called at ~1 Hz INCLUDING while closed, so
     * when closed it stores and returns without touching the DOM. It never
     * writes to the mixer, however alarming the snapshot is.
     */
    update(snapshot) {
      if (state.destroyed) return;
      applySnapshot(snapshot);
      if (!state.open) return;
      render();
    },

    /**
     * setArmed allows or forbids writes. Allowing by itself changes nothing on
     * the mixer; it only permits a later operator gesture to.
     */
    setArmed(armed) {
      if (state.destroyed) return;
      setArmed(armed, armed ? 'the host allowed changes' : 'the host locked changes');
    },

    /** destroy tears down. It removes listeners, restores the mount and
     * releases the drawer. It does NOT write to the mixer and in particular
     * does not restore routing. */
    destroy() {
      if (state.destroyed) return;
      state.destroyed = true;
      state.open = false;
      state.armed = false;
      state.cellNote = null;
      doc.removeEventListener('keydown', onKeyDown, true);
      if (staleTimer !== null && typeof clearInterval === 'function') clearInterval(staleTimer);
      if (ui.root.parentNode === mount) mount.removeChild(ui.root);
      for (const child of originalChildren) mount.appendChild(child);
      rowEls = new Map();
      busEls = new Map();
    },
  };

  function applySnapshot(snapshot) {
    if (!snapshot || typeof snapshot !== 'object') return;
    state.snapshot = snapshot;
    state.model = buildMatrixModel(snapshot);
    state.lastUpdateAt = Date.now();
    reconcileAwaiting();
  }

  function trapFocus(ev) {
    const focusables = collectFocusable(ui.drawer);
    if (focusables.length === 0) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = doc.activeElement;
    const back = ev.shiftKey === true;
    if (!back && active === last) {
      if (typeof ev.preventDefault === 'function') ev.preventDefault();
      first.focus();
    } else if (back && active === first) {
      if (typeof ev.preventDefault === 'function') ev.preventDefault();
      last.focus();
    }
  }

  return api;
}

/* ========================================================================== */
/* DOM construction                                                           */
/* ========================================================================== */

/**
 * buildShell creates the drawer chrome once. Everything below the matrix header
 * is filled in by render(); this only establishes the elements that never move,
 * so that a 1 Hz update does not rebuild the tree under the operator's cursor.
 *
 * The "Allow changes" control is in the HEADER, not in the body: it is the one
 * control an operator hunts for, and every row of chrome above the matrix is a
 * row of strips they cannot see.
 */
function buildShell(doc) {
  const root = el(doc, 'div', { className: 'mx-root' });
  const scrim = el(doc, 'div', { className: 'mx-scrim', attrs: { 'aria-hidden': 'true' } });
  const drawer = el(doc, 'aside', {
    className: 'mx-drawer',
    attrs: { role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Audio mixer' },
  });

  const head = el(doc, 'header', { className: 'mx-head' });
  const titles = el(doc, 'div', { className: 'mx-titles' });
  titles.appendChild(el(doc, 'h2', { className: 'mx-title', text: 'Audio mixer' }));
  const freshness = el(doc, 'span', { className: 'mx-fresh', text: 'no state received yet' });
  titles.appendChild(freshness);
  const cleanLine = el(doc, 'span', { className: 'mx-cleanline mx-cleanline--unknown', text: 'CLN: unknown - no mixer state' });
  titles.appendChild(cleanLine);
  head.appendChild(titles);

  const headControls = el(doc, 'div', { className: 'mx-headctl' });
  const armState = el(doc, 'span', { className: 'mx-armstate', text: 'READ-ONLY' });
  const armBtn = el(doc, 'button', {
    className: 'mx-btn mx-allow',
    text: `${GLYPHS.allowOff} Allow changes`,
    attrs: { type: 'button', 'aria-pressed': 'false' },
  });
  const closeBtn = el(doc, 'button', {
    className: 'mx-btn mx-btn--close',
    text: 'Close',
    attrs: { type: 'button', 'aria-label': 'Close the mixer drawer (Escape)' },
  });
  headControls.appendChild(armState);
  headControls.appendChild(armBtn);
  headControls.appendChild(closeBtn);
  head.appendChild(headControls);
  drawer.appendChild(head);

  const body = el(doc, 'div', { className: 'mx-body' });

  const notice = el(doc, 'p', { className: 'mx-notice', attrs: { role: 'status', 'aria-live': 'polite' } });
  notice.hidden = true;
  body.appendChild(notice);

  const awaitBox = el(doc, 'section', { className: 'mx-awaitbox' });
  awaitBox.appendChild(el(doc, 'h3', { className: 'mx-h3', text: 'Sent - awaiting confirmation from the mixer' }));
  const awaitList = el(doc, 'ul', { className: 'mx-list' });
  awaitBox.appendChild(awaitList);
  awaitBox.hidden = true;
  body.appendChild(awaitBox);

  const tools = el(doc, 'div', { className: 'mx-tools' });
  tools.appendChild(el(doc, 'span', { className: 'mx-tools-label', text: 'Show:' }));
  const filterBtns = new Map();
  for (const f of FILTERS) {
    const b = el(doc, 'button', { className: 'mx-chip', text: f.label, attrs: { type: 'button', 'aria-pressed': String(f.id === 'all') } });
    tools.appendChild(b);
    filterBtns.set(f.id, b);
  }
  const busToggle = el(doc, 'button', {
    className: 'mx-chip',
    text: 'Show all buses',
    attrs: { type: 'button', 'aria-pressed': 'false' },
  });
  tools.appendChild(busToggle);
  body.appendChild(tools);

  const legend = el(doc, 'p', { className: 'mx-legend' });
  legend.textContent =
    `${GLYPHS.routed} routed · ${GLYPHS.notRouted} not routed · ` +
    `${GLYPHS.routedClean} routed to the CLEAN FEED · ${GLYPHS.notRoutedClean} not on the clean feed · ` +
    'dashed outline = locked, changes are not allowed. ' +
    'While changes are allowed a click is SENT IMMEDIATELY - there is no Apply step. ' +
    'Meters are peak hold, two channels, scaled -60 to 0 dBFS. Mute and fader are read-only here.';
  body.appendChild(legend);

  const matrixNote = el(doc, 'p', { className: 'mx-matrix-note', text: 'No mixer state to show.' });
  body.appendChild(matrixNote);

  const wrap = el(doc, 'div', { className: 'mx-matrix-wrap', attrs: { tabindex: '0', role: 'region', 'aria-label': 'Routing matrix' } });
  const table = el(doc, 'table', { className: 'mx-matrix' });
  const caption = el(doc, 'caption', { className: 'mx-caption' });
  caption.textContent =
    'Rows are inputs. PGM is the programme output; CLN is the clean feed the client receives ' +
    `(${busLabel(CLEAN_FEED_BUS)}). "Show all buses" adds the five that do not leave the building.`;
  const thead = el(doc, 'thead');
  const tbody = el(doc, 'tbody');
  table.appendChild(caption);
  table.appendChild(thead);
  table.appendChild(tbody);
  wrap.appendChild(table);
  body.appendChild(wrap);

  drawer.appendChild(body);
  root.appendChild(scrim);
  root.appendChild(drawer);

  return {
    root,
    scrim,
    drawer,
    closeBtn,
    freshness,
    cleanLine,
    armBtn,
    armState,
    notice,
    awaitBox,
    awaitList,
    filterBtns,
    busToggle,
    matrixNote,
    thead,
    tbody,
  };
}

/**
 * el creates an element. A deliberately small helper over a deliberately small
 * slice of the DOM API — createElement, appendChild, setAttribute, className,
 * textContent, hidden, style.width, addEventListener, focus — so that the whole
 * drawer can be exercised under `node --test` against the shim in testdom.js.
 *
 * @param {Document} doc
 * @param {string} tag
 * @param {{className?: string, text?: string, attrs?: Object<string,string>}} [spec]
 * @returns {HTMLElement}
 */
function el(doc, tag, spec = {}) {
  const node = doc.createElement(tag);
  if (spec.className) node.className = spec.className;
  if (spec.text !== undefined) node.textContent = spec.text;
  if (spec.attrs) {
    for (const [k, v] of Object.entries(spec.attrs)) node.setAttribute(k, v);
  }
  return node;
}

/**
 * cellKey identifies one crosspoint.
 *
 * JSON, not a joined string. Strip names contain spaces ('MIC 1-1') and
 * hyphens ('cam22-1'), so there is no printable delimiter that is safe by
 * inspection; JSON.stringify of the pair is unambiguous for any pair of
 * strings, and nothing ever parses it back.
 *
 * @param {string} strip
 * @param {string} bus
 * @returns {string}
 */
function cellKey(strip, bus) {
  return JSON.stringify([strip, bus]);
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/**
 * later schedules a repaint. The handle is unref'd where the host provides it
 * (Node, under the tests) so that a pending cosmetic timer cannot hold a
 * process open; in a browser unref does not exist and this is a plain
 * setTimeout. Nothing scheduled through here may write to the mixer.
 */
function later(fn, ms) {
  if (typeof setTimeout !== 'function') return;
  const t = setTimeout(fn, ms);
  if (t && typeof t.unref === 'function') t.unref();
}

function sameSet(a, b) {
  const la = Array.isArray(a) ? a : [];
  const lb = Array.isArray(b) ? b : [];
  if (la.length !== lb.length) return false;
  const sa = new Set(la);
  return lb.every((x) => sa.has(x));
}

/**
 * collectFocusable walks the drawer for controls a Tab can reach. Only the
 * element types this drawer creates are considered; it does not attempt to be a
 * general focus-trap implementation.
 */
function collectFocusable(rootNode) {
  const out = [];
  const walk = (node) => {
    if (!node || node.hidden === true) return;
    const tag = typeof node.tagName === 'string' ? node.tagName.toLowerCase() : '';
    if (tag === 'button' || node.getAttribute?.('tabindex') === '0') out.push(node);
    const kids = node.childNodes || [];
    for (const k of kids) walk(k);
  };
  walk(rootNode);
  return out;
}
