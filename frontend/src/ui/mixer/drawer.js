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
 * The first thing on screen is not the matrix. It is the sentence naming every
 * strip currently in the CLEAN FEED, because that is the failure this drawer
 * exists to catch: every strip defaults to ["master","aux1","aux2"], aux1 is
 * the CLN output, and nothing in Sony's UI says so. An unmuted commentary input
 * is in the client's clean feed until somebody corrects its routing.
 *
 * ======================= WHAT IT WILL AND WILL NOT WRITE ====================
 *
 * The only write this drawer offers is set_routing on a crosspoint, and even
 * that is staged: clicking a crosspoint marks an intention, and a separate
 * Apply gesture sends it. Nothing is sent on open, close, update, arm, destroy
 * or on a diff appearing.
 *
 * MUTE AND FADER ARE READ-ONLY HERE, DELIBERATELY. internal/mixer/mixer.go says
 * of SetInputMuted "THE ARGUMENT SHAPE IS NOT MEASURED", and of SetChFader that
 * whether the wire takes a per-channel pair or one message per channel is
 * unconfirmed. Offering a control whose argument shape is a guess, on a mixer
 * feeding a live clean feed, is not a control — it is a coin toss. Both are
 * displayed; neither can be written from here until WP-M1 has measured them.
 * Routing is also the correct fix: pulling a fader leaves the strip routed to
 * aux1, so the next state restore brings it back audible in the clean feed.
 */

import { ALL_BUSES, CLEAN_FEED_BUS, busLabel } from './contract.js';
import {
  METER_FLOOR_DB,
  buildMatrixModel,
  busListText,
  changeSeverity,
  compareSnapshots,
  createWriteGate,
  describeCrosspointChange,
  diffHeadline,
  formatDb,
  meterPercent,
  meterZone,
  routingAfterToggle,
  routingCommand,
  sortDiffs,
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
  pending: '✱', // HEAVY ASTERISK - staged, not sent
});

/** CONFIRM_WINDOW_MS is how long a sent change may take to appear in a
 * snapshot before the drawer calls it unconfirmed. update() arrives at about
 * 1 Hz, so this is four frames. A resolved sendCommands means SENT, never
 * APPLIED — the only proof is the state reading back. */
const CONFIRM_WINDOW_MS = 4000;

/** GOLDEN_CONFIRM_MS is how long the two-step "capture golden" control stays
 * armed. Re-baselining silently erases the very difference the drawer exists to
 * show, so it takes two deliberate clicks. */
const GOLDEN_CONFIRM_MS = 5000;

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
 * Two options beyond the contract are honoured when the host supplies them, and
 * are optional in every sense — omit them and the drawer behaves exactly as the
 * contract describes:
 *
 *   getDiffs()             a diff source backed by mixer.Compare. Without it
 *                          the drawer compares golden against current itself
 *                          (see compareSnapshots), because MixerDrawerOptions
 *                          has no diff member and getGolden returns a snapshot.
 *   onArmedChange(armed)   told whenever the armed state changes, INCLUDING the
 *                          forced disarm on close, so that the host's own idea
 *                          of armed cannot drift from the drawer's.
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
    golden: null,
    goldenLoaded: false,
    diffs: [],
    driftVisible: true,
    filter: 'all',
    lastUpdateAt: null,
    notice: '',
    noticeKind: 'info',
    /**
     * Staged crosspoint changes: pendingKey() -> {strip, bus, routed}.
     *
     * The value carries its own strip and bus rather than being parsed back
     * out of the key. STRIP NAMES CONTAIN SPACES - 'MIC 1-1' is a real strip in
     * the captured live frame - so any parseable key format is one delimiter
     * collision away from staging a change against the wrong strip, and a
     * routing change against the wrong strip is a wrong strip on air.
     */
    pending: new Map(),
    /** sent changes awaiting confirmation from a following snapshot */
    awaiting: [],
    goldenArmedUntil: 0,
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

  /** element registry for in-place updates; rebuilt only when the strip set changes */
  let rowEls = new Map();
  let busEls = new Map();
  let builtStructureKey = null;

  /* -------------------------------------------------------------- wiring */

  ui.closeBtn.addEventListener('click', () => api.close());
  ui.scrim.addEventListener('click', () => api.close());

  ui.armBtn.addEventListener('click', () => {
    setArmed(!state.armed, 'operator pressed Arm');
  });

  ui.applyBtn.addEventListener('click', () => {
    void applyPending();
  });

  ui.discardBtn.addEventListener('click', () => {
    state.pending.clear();
    notice('Staged changes discarded. Nothing was sent.', 'info');
    render();
  });

  ui.driftToggle.addEventListener('click', () => {
    state.driftVisible = !state.driftVisible;
    render();
  });

  ui.goldenBtn.addEventListener('click', () => {
    void captureGolden();
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
    if (!next) state.pending.clear();
    if (typeof o.onArmedChange === 'function') o.onArmedChange(next);
    notice(
      next
        ? 'WRITE PATH ARMED. Crosspoint changes are staged; nothing is sent until you press Apply.'
        : `Write path DISARMED (${why}). Staged changes discarded; nothing was sent.`,
      next ? 'warn' : 'info',
    );
    render();
  }

  /**
   * applyPending sends the staged crosspoint changes. The ONLY place in this
   * module that reaches the write gate, and it runs solely from the Apply
   * click handler.
   *
   * ===================== WHY THIS RE-READS BEFORE IT SENDS ==================
   *
   * set_routing REPLACES a strip's whole bus set. The staged crosspoint says
   * what ONE bus should become; every other bus in the command comes from the
   * routing this drawer happens to be holding. If that routing is old, the
   * command is one intended change wrapped in a rollback of everything else on
   * that strip — applied to a desk that is on air.
   *
   * So the apply path has three separate defences, in this order:
   *
   *   1. REFUSE A STALE VIEW. If updates have stopped, nothing is sent and the
   *      operator is told why, with the age. Staged changes are KEPT, because
   *      the intention was fine — it is the picture that was not.
   *   2. RE-PLAN FROM A FRESH FRAME. Even a live view is up to a second old,
   *      and the desk is shared. The staged crosspoints are re-applied to the
   *      frame that arrives AFTER Apply was pressed, never to the one that was
   *      on screen when it was. If the base moved, the operator is told after
   *      the fact — their intent is still expressed exactly, on the routing
   *      that is actually there.
   *   3. THE GATE. createWriteGate re-checks armed AND freshness at the moment
   *      of the write, so none of this depends on this function remembering to.
   *
   * The refusal is a property of this path and of the gate, never of the Apply
   * button's disabled state: a keyboard activation or a programmatic click
   * arrives here just the same, and a disabled attribute stops neither.
   */
  async function applyPending() {
    if (state.pending.size === 0) {
      notice('Nothing staged to send.', 'info');
      render();
      return;
    }

    // 1. Freshness, before anything is planned or read.
    const fresh = viewFreshness(state.lastUpdateAt, Date.now());
    if (!fresh.fresh) {
      notice(
        `NOT SENT - ${fresh.text.toUpperCase()}. Nothing was written. ` +
          'set_routing replaces a strip\'s WHOLE bus set, so applying from a view this old would send every other bus ' +
          'back to what it was when the feed stopped - to a live desk. ' +
          'Your staged changes have been KEPT. Wait for the view to go live again and press Apply.',
        'warn',
      );
      render();
      return;
    }

    // 2. Re-plan from the frame that arrives AFTER Apply.
    const before = state.model;
    try {
      const snap = await o.getSnapshot();
      if (!snap || typeof snap !== 'object') throw new Error('the mixer returned no state');
      applySnapshot(snap);
    } catch (err) {
      onError(err, 'mixer: getSnapshot before Apply');
      notice(
        'NOT SENT: the mixer could not be re-read immediately before writing, so the routing this change would ' +
          'replace is unknown. Nothing was written and your staged changes have been kept.',
        'warn',
      );
      render();
      return;
    }
    if (!state.model.hasData) {
      notice(
        'NOT SENT: the mixer reported no state when re-read, so there is no routing to build a replacement from. ' +
          'Nothing was written and your staged changes have been kept.',
        'warn',
      );
      render();
      return;
    }

    const plan = planPending();
    if (plan.commands.length === 0) {
      // Everything staged is already true on the fresh frame — most likely
      // somebody else just made the same correction. Not an error, and
      // emphatically not a write.
      state.pending.clear();
      notice(
        'Nothing sent: on the state just read back, every staged change is already in effect. ' +
          'The staged list has been cleared.',
        'info',
      );
      render();
      return;
    }

    const moved = plan.intents.filter((i) => {
      const was = before.rows.find((r) => r.name === i.strip);
      return was && !sameSet(was.outputs, i.base);
    });

    // 3. The gate. It re-checks armed and freshness itself.
    const result = await gate.submit(plan.commands, 'operator pressed Apply');
    if (!result.sent) {
      notice(`NOT SENT: ${result.reason}`, 'warn');
      render();
      return;
    }
    state.pending.clear();
    const deadline = Date.now() + CONFIRM_WINDOW_MS;
    state.awaiting = plan.intents.map((i) => ({ ...i, deadline, state: 'pending' }));
    const movedWord =
      moved.length === 0
        ? ''
        : ` The desk had moved since the view you acted on: ${moved
            .map((i) => i.label)
            .join(', ')} - your change was re-planned onto the routing that is actually there, so no other bus was rolled back.`;
    notice(
      `Sent ${plan.commands.length} routing change(s). SENT IS NOT APPLIED - waiting for the mixer to report it back.${movedWord}`,
      'warn',
    );
    render();
  }

  /**
   * planPending folds the staged crosspoints into one set_routing per strip.
   *
   * One command per strip, never one per crosspoint: set_routing REPLACES the
   * whole bus set, so two separate commands for the same strip would have the
   * second undo the first.
   *
   * IT PLANS FROM state.model, WHICH IS WHATEVER FRAME WAS LAST APPLIED. That
   * is the whole reason applyPending re-reads before calling this: the base
   * every unstaged bus is copied from is only as current as the last snapshot,
   * and set_routing sends that base back to the desk verbatim. Each intent
   * carries the base it was planned from so the caller can say whether the
   * desk moved underneath the operator.
   */
  function planPending() {
    const byStrip = new Map();
    for (const { strip: stripName, bus, routed } of state.pending.values()) {
      const row = state.model.rows.find((r) => r.name === stripName);
      if (!row) continue;
      const entry = byStrip.get(stripName) || { row, outputs: row.outputs.slice() };
      entry.outputs = routingAfterToggle(entry.outputs, bus, routed);
      byStrip.set(stripName, entry);
    }

    const commands = [];
    const intents = [];
    for (const [stripName, entry] of byStrip) {
      // Already in that state: sending would be a write with no effect, and
      // every avoidable write to a live mixer is worth avoiding.
      if (sameSet(entry.outputs, entry.row.outputs)) continue;
      commands.push(routingCommand(stripName, entry.outputs));
      intents.push({
        strip: stripName,
        label: entry.row.label,
        desired: entry.outputs.slice(),
        base: entry.row.outputs.slice(),
      });
    }
    return { commands, intents };
  }

  /**
   * captureGolden saves the current snapshot as golden. Two clicks: silently
   * re-baselining would erase the difference the drawer exists to show. It
   * writes application state only and NEVER touches the mixer, so it is
   * available whether or not the drawer is armed.
   */
  async function captureGolden() {
    if (!state.snapshot) {
      notice('No state to capture yet.', 'warn');
      render();
      return;
    }
    const now = Date.now();
    if (now > state.goldenArmedUntil) {
      state.goldenArmedUntil = now + GOLDEN_CONFIRM_MS;
      notice('Press "Capture golden" again to overwrite the saved golden state. This does not touch the mixer.', 'warn');
      render();
      later(() => {
        if (!state.destroyed && Date.now() > state.goldenArmedUntil) render();
      }, GOLDEN_CONFIRM_MS + 50);
      return;
    }
    state.goldenArmedUntil = 0;
    try {
      await o.setGolden(state.snapshot);
      state.golden = state.snapshot;
      state.goldenLoaded = true;
      notice('Golden state captured from the current mixer state. The mixer was not changed.', 'info');
    } catch (err) {
      onError(err, 'mixer: setGolden');
      notice('Could not save the golden state.', 'warn');
    }
    await refreshDiffs();
    render();
  }

  async function refreshGolden() {
    try {
      state.golden = await o.getGolden();
      state.goldenLoaded = true;
    } catch (err) {
      state.goldenLoaded = false;
      onError(err, 'mixer: getGolden');
    }
  }

  async function refreshDiffs() {
    if (typeof o.getDiffs === 'function') {
      try {
        state.diffs = sortDiffs(await o.getDiffs());
        return;
      } catch (err) {
        onError(err, 'mixer: getDiffs');
        state.diffs = [];
        return;
      }
    }
    state.diffs = compareSnapshots(state.golden, state.snapshot);
  }

  function notice(text, kind) {
    state.notice = text;
    state.noticeKind = kind || 'info';
  }

  function startStaleTimer() {
    // Reads the clock and repaints one label. It never fetches and never
    // writes; the contract's "must not poll" is about getSnapshot.
    if (typeof setInterval !== 'function') return null;
    const t = setInterval(() => {
      if (!state.open || state.destroyed) return;
      renderFreshness();
      // renderArm too, so that when the feed stops the Apply control says
      // BLOCKED without waiting for an update() that is not coming. This is
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
    renderCleanFeedBanner();
    renderArm();
    renderNotice();
    renderDrift();
    renderMatrix();
    renderFreshness();
  }

  function renderCleanFeedBanner() {
    const b = ui.cleanBanner;
    if (!state.model.hasData) {
      b.className = 'mx-banner mx-banner--unknown';
      b.textContent = 'No mixer state received. This drawer cannot tell you what is in the clean feed.';
      return;
    }
    const audible = state.model.cleanFeed.filter((r) => !r.muted);
    const muted = state.model.cleanFeed.filter((r) => r.muted);
    if (state.model.cleanFeed.length === 0) {
      b.className = 'mx-banner mx-banner--ok';
      b.textContent = `Nothing is routed to ${busLabel(CLEAN_FEED_BUS)}.`;
      return;
    }
    b.className = audible.length > 0 ? 'mx-banner mx-banner--alert' : 'mx-banner mx-banner--warn';
    const parts = [];
    if (audible.length > 0) {
      parts.push(`IN THE CLEAN FEED AND UNMUTED: ${audible.map((r) => r.label).join(', ')}.`);
    }
    if (muted.length > 0) {
      parts.push(`Routed to the clean feed but muted: ${muted.map((r) => r.label).join(', ')}.`);
    }
    parts.push(`${busLabel(CLEAN_FEED_BUS)} is the CLN output the client receives.`);
    b.textContent = parts.join(' ');
  }

  function renderArm() {
    ui.armBtn.textContent = state.armed ? 'Disarm write path' : 'Arm write path';
    ui.armBtn.setAttribute('aria-pressed', String(state.armed));
    ui.armBtn.className = state.armed ? 'mx-btn mx-btn--armed' : 'mx-btn';
    ui.armState.textContent = state.armed
      ? 'ARMED - crosspoints can be staged and applied to the live mixer'
      : 'READ-ONLY - crosspoints are locked; nothing here can reach the mixer';
    ui.armState.className = state.armed ? 'mx-armstate mx-armstate--armed' : 'mx-armstate';

    const staged = state.pending.size;
    // The freshness state is SHOWN on the Apply control as well as enforced in
    // applyPending and in the write gate. Showing it is not the gate — an
    // aria-disabled button still activates from a keyboard and still fires
    // from a programmatic click — it is so the operator learns why before they
    // press it rather than after.
    const fresh = viewFreshness(state.lastUpdateAt, Date.now());
    ui.applyBtn.hidden = !state.armed;
    ui.discardBtn.hidden = !state.armed || staged === 0;
    ui.applyBtn.textContent = !fresh.fresh
      ? `Apply BLOCKED - ${fresh.text}`
      : staged === 0
        ? 'Apply (nothing staged)'
        : `Apply ${staged} change(s) to the live mixer`;
    ui.applyBtn.setAttribute('aria-disabled', String(staged === 0 || !fresh.fresh));
    ui.applyBtn.className = staged === 0 || !fresh.fresh ? 'mx-btn' : 'mx-btn mx-btn--danger';

    renderStagedList();
    renderAwaiting();

    ui.goldenBtn.textContent =
      Date.now() <= state.goldenArmedUntil ? 'Capture golden - press again to confirm' : 'Capture golden';
  }

  function renderStagedList() {
    clear(ui.stagedList);
    ui.stagedBox.hidden = state.pending.size === 0;
    for (const { strip: stripName, bus, routed } of state.pending.values()) {
      const row = state.model.rows.find((r) => r.name === stripName);
      // Ranked by the same rule the drift list uses, so the words an operator
      // reads BEFORE a write match the words they read after one.
      const li = el(doc, 'li', {
        className: `mx-staged mx-sev--${changeSeverity(bus)}`,
        text: `${GLYPHS.pending} ${describeCrosspointChange(row || { label: stripName }, bus, routed)}`,
      });
      ui.stagedList.appendChild(li);
    }
  }

  /**
   * renderAwaiting is the HIGHEST-STAKES READ IN THIS DRAWER.
   *
   * It appears immediately after a write to a live desk and it is what the
   * operator checks to decide the change did what they meant. Every bus in it
   * goes through busListText, never a bare join: "CONFIRMED: CLAUDE-COMMS ->
   * master, aux1" reads as success while confirming that commentary is in the
   * client's clean feed, which is the single failure this whole drawer exists
   * to prevent. Rendered through the labels it reads "... -> master (PGM),
   * aux1 (CLN - clean feed)" and cannot be misread as success.
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

  function renderDrift() {
    ui.driftToggle.textContent = state.driftVisible ? 'Hide drift' : 'Show drift';
    ui.driftToggle.setAttribute('aria-expanded', String(state.driftVisible));
    ui.driftBody.hidden = !state.driftVisible;

    clear(ui.driftList);
    if (!state.goldenLoaded) {
      ui.driftSummary.textContent = 'Golden state not loaded.';
      ui.driftSummary.className = 'mx-drift-summary';
      return;
    }
    if (!state.golden) {
      // Null golden is a normal state and must NOT read as "no differences":
      // that is a claim the drawer cannot make.
      ui.driftSummary.textContent = 'No golden saved. Capture one to be told when the mixer drifts from it.';
      ui.driftSummary.className = 'mx-drift-summary mx-drift-summary--none';
      return;
    }
    const diffs = state.diffs;
    const crit = diffs.filter((d) => d.severity === 'critical').length;
    const warn = diffs.filter((d) => d.severity === 'warn').length;
    if (diffs.length === 0) {
      ui.driftSummary.textContent = 'No differences from the golden state.';
      ui.driftSummary.className = 'mx-drift-summary mx-drift-summary--ok';
      return;
    }
    ui.driftSummary.textContent = `${diffs.length} difference(s) from golden - ${crit} CRITICAL, ${warn} warning(s).`;
    ui.driftSummary.className = crit > 0 ? 'mx-drift-summary mx-drift-summary--alert' : 'mx-drift-summary mx-drift-summary--warn';

    for (const d of diffs) {
      const li = el(doc, 'li', { className: `mx-diff mx-sev--${d.severity}` });
      li.appendChild(
        el(doc, 'span', {
          className: 'mx-diff-badge',
          text: d.severity === 'critical' ? 'CRITICAL' : d.severity === 'warn' ? 'WARNING' : 'INFO',
        }),
      );
      li.appendChild(el(doc, 'span', { className: 'mx-diff-head', text: diffHeadline(d) }));
      li.appendChild(
        el(doc, 'span', {
          className: 'mx-diff-detail',
          // d.label, never d.target: for a bus diff the target IS a raw bus
          // name, so "aux1 muted: golden ..." would be the clean feed
          // described as a string the operator cannot decode. MixerDiff says
          // label is "already resolved, safe to show" — this is the read it
          // was resolved for.
          text: `${d.label} ${d.field}: golden "${d.golden}" -> now "${d.current}"`,
        }),
      );
      ui.driftList.appendChild(li);
    }
  }

  function renderMatrix() {
    if (!state.model.hasData) {
      ui.matrixNote.hidden = false;
      ui.matrixNote.textContent = 'No mixer state to show.';
      return;
    }
    ui.matrixNote.hidden = true;

    if (builtStructureKey !== state.model.structureKey) {
      buildMatrixRows();
      builtStructureKey = state.model.structureKey;
    }
    for (const f of FILTERS) {
      ui.filterBtns.get(f.id).setAttribute('aria-pressed', String(state.filter === f.id));
      ui.filterBtns.get(f.id).className = state.filter === f.id ? 'mx-chip mx-chip--on' : 'mx-chip';
    }
    updateLive();
  }

  function buildMatrixRows() {
    clear(ui.thead);
    clear(ui.tbody);
    rowEls = new Map();
    busEls = new Map();

    const hr = el(doc, 'tr');
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--strip', text: 'Strip', attrs: { scope: 'col' } }));
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--meter', text: 'Peak hold (dBFS)', attrs: { scope: 'col' } }));
    hr.appendChild(el(doc, 'th', { className: 'mx-th mx-th--state', text: 'Mute / fader', attrs: { scope: 'col' } }));
    for (const bus of ALL_BUSES) {
      const clean = bus === CLEAN_FEED_BUS;
      const th = el(doc, 'th', {
        className: `mx-th mx-th--bus${clean ? ' mx-col-clean' : ''}`,
        attrs: { scope: 'col' },
      });
      th.appendChild(el(doc, 'span', { className: 'mx-th-name', text: busLabel(bus) }));
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
      for (const bus of ALL_BUSES) {
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
        btn.addEventListener('click', () => onCrosspoint(row.name, bus));
        td.appendChild(btn);
        tr.appendChild(td);
        cells.set(bus, { btn, glyph, text });
      }

      ui.tbody.appendChild(tr);
      rowEls.set(row.name, { tr, label, wire, fillL, fillR, meterText, muteEl, faderEl, cells });
    }
  }

  /**
   * onCrosspoint is the operator gesture on a crosspoint. It NEVER sends: it
   * stages an intention, which the Apply gesture may later send. While
   * disarmed it explains itself rather than silently doing nothing.
   */
  function onCrosspoint(stripName, bus) {
    if (!state.armed) {
      notice(
        `Locked: the write path is disarmed, so ${busLabel(bus)} on this strip cannot be changed. Press "Arm write path" first.`,
        'info',
      );
      render();
      return;
    }
    const row = state.model.rows.find((r) => r.name === stripName);
    if (!row) return;
    const key = pendingKey(stripName, bus);
    const current = row.outputs.includes(bus);
    if (state.pending.has(key)) state.pending.delete(key);
    else state.pending.set(key, { strip: stripName, bus, routed: !current });
    render();
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
    const key = pendingKey(row.name, cell.bus);
    const staged = state.pending.has(key);
    const shown = staged ? state.pending.get(key).routed : cell.routed;

    const glyph = cell.cleanFeed
      ? shown
        ? GLYPHS.routedClean
        : GLYPHS.notRoutedClean
      : shown
        ? GLYPHS.routed
        : GLYPHS.notRouted;

    // The glyph is always the state, never the lock: an operator must be able
    // to read the routing whether or not the drawer is armed. Locked cells are
    // marked instead by a dashed outline (a SHAPE, not a colour), by
    // aria-disabled, and by the READ-ONLY line in the arm bar.
    refs.glyph.textContent = glyph;
    refs.text.textContent = cell.cleanFeed && shown ? 'CLN' : '';

    const classes = ['mx-x'];
    classes.push(shown ? 'mx-x--on' : 'mx-x--off');
    if (cell.cleanFeed) classes.push('mx-x--clean');
    if (cell.noEgress) classes.push('mx-x--noegress');
    if (staged) classes.push('mx-x--staged');
    if (!state.armed) classes.push('mx-x--locked');
    refs.btn.className = classes.join(' ');

    refs.btn.setAttribute('aria-checked', String(shown));
    refs.btn.setAttribute('aria-disabled', String(!state.armed));
    const stagedWord = staged ? `. STAGED, not sent: will become ${shown ? 'routed' : 'not routed'}` : '';
    const lockWord = state.armed ? '' : '. Locked: the write path is disarmed';
    refs.btn.setAttribute(
      'aria-label',
      `${row.label} to ${cell.label}: ${cell.routed ? 'routed' : 'not routed'}${stagedWord}${lockWord}`,
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
      notice('All sent changes confirmed by the mixer.', 'info');
      const done = state.awaiting;
      later(() => {
        if (state.destroyed) return;
        if (state.awaiting === done) {
          state.awaiting = [];
          render();
        }
      }, 4000);
    }
  }

  /* ------------------------------------------------------------ lifecycle */

  const api = {
    /**
     * open shows the drawer and refreshes from getSnapshot and getGolden. It
     * is READ-ONLY: opening never writes, and it never arms.
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
        await refreshGolden();
        await refreshDiffs();
        render();
      })();
    },

    /**
     * close hides the drawer and DISARMS the write path.
     *
     * The contract says close "must not disarm silently"; this task requires
     * that it always disarm, because an armed drawer left open is a foot-gun.
     * Both are satisfied by disarming LOUDLY: the operator is told, the host is
     * told through onArmedChange when it supplied one, and the notice is still
     * on screen the next time the drawer is opened. What close must never do is
     * leave the host believing the drawer is still armed.
     */
    close() {
      if (state.destroyed || !state.open) return;
      if (state.armed) setArmed(false, 'the drawer was closed');
      state.pending.clear();
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
      if (typeof o.getDiffs === 'function') {
        void refreshDiffs().then(render);
        return;
      }
      state.diffs = compareSnapshots(state.golden, state.snapshot);
      render();
    },

    /**
     * setArmed enables or disables the write path. Arming by itself changes
     * nothing on the mixer; it only permits a later operator gesture to.
     */
    setArmed(armed) {
      if (state.destroyed) return;
      setArmed(armed, armed ? 'the host armed the drawer' : 'the host disarmed the drawer');
    },

    /** destroy tears down. It removes listeners, restores the mount and
     * releases the drawer. It does NOT write to the mixer and in particular
     * does not restore routing. */
    destroy() {
      if (state.destroyed) return;
      state.destroyed = true;
      state.open = false;
      state.armed = false;
      state.pending.clear();
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
  head.appendChild(titles);
  const closeBtn = el(doc, 'button', {
    className: 'mx-btn mx-btn--close',
    text: 'Close',
    attrs: { type: 'button', 'aria-label': 'Close the mixer drawer (Escape)' },
  });
  head.appendChild(closeBtn);
  drawer.appendChild(head);

  const body = el(doc, 'div', { className: 'mx-body' });

  const cleanBanner = el(doc, 'p', { className: 'mx-banner mx-banner--unknown', attrs: { role: 'status' } });
  body.appendChild(cleanBanner);

  const armBar = el(doc, 'div', { className: 'mx-armbar' });
  const armBtn = el(doc, 'button', { className: 'mx-btn', text: 'Arm write path', attrs: { type: 'button', 'aria-pressed': 'false' } });
  const armState = el(doc, 'span', { className: 'mx-armstate' });
  const applyBtn = el(doc, 'button', { className: 'mx-btn', text: 'Apply', attrs: { type: 'button' } });
  const discardBtn = el(doc, 'button', { className: 'mx-btn', text: 'Discard staged', attrs: { type: 'button' } });
  applyBtn.hidden = true;
  discardBtn.hidden = true;
  armBar.appendChild(armBtn);
  armBar.appendChild(applyBtn);
  armBar.appendChild(discardBtn);
  armBar.appendChild(armState);
  body.appendChild(armBar);

  const notice = el(doc, 'p', { className: 'mx-notice', attrs: { role: 'status', 'aria-live': 'polite' } });
  notice.hidden = true;
  body.appendChild(notice);

  const stagedBox = el(doc, 'section', { className: 'mx-stagedbox' });
  stagedBox.appendChild(el(doc, 'h3', { className: 'mx-h3', text: 'Staged, not sent' }));
  const stagedList = el(doc, 'ul', { className: 'mx-list' });
  stagedBox.appendChild(stagedList);
  stagedBox.hidden = true;
  body.appendChild(stagedBox);

  const awaitBox = el(doc, 'section', { className: 'mx-awaitbox' });
  awaitBox.appendChild(el(doc, 'h3', { className: 'mx-h3', text: 'Sent - awaiting confirmation from the mixer' }));
  const awaitList = el(doc, 'ul', { className: 'mx-list' });
  awaitBox.appendChild(awaitList);
  awaitBox.hidden = true;
  body.appendChild(awaitBox);

  const drift = el(doc, 'section', { className: 'mx-drift' });
  const driftHead = el(doc, 'div', { className: 'mx-drift-head' });
  driftHead.appendChild(el(doc, 'h3', { className: 'mx-h3', text: 'Drift from golden' }));
  const driftToggle = el(doc, 'button', { className: 'mx-btn mx-btn--small', text: 'Hide drift', attrs: { type: 'button', 'aria-expanded': 'true' } });
  const goldenBtn = el(doc, 'button', { className: 'mx-btn mx-btn--small', text: 'Capture golden', attrs: { type: 'button' } });
  driftHead.appendChild(driftToggle);
  driftHead.appendChild(goldenBtn);
  drift.appendChild(driftHead);
  const driftBody = el(doc, 'div', { className: 'mx-drift-body' });
  const driftSummary = el(doc, 'p', { className: 'mx-drift-summary', text: 'Golden state not loaded.' });
  const driftList = el(doc, 'ul', { className: 'mx-list' });
  driftBody.appendChild(driftSummary);
  driftBody.appendChild(driftList);
  drift.appendChild(driftBody);
  body.appendChild(drift);

  const tools = el(doc, 'div', { className: 'mx-tools' });
  tools.appendChild(el(doc, 'span', { className: 'mx-tools-label', text: 'Show:' }));
  const filterBtns = new Map();
  for (const f of FILTERS) {
    const b = el(doc, 'button', { className: 'mx-chip', text: f.label, attrs: { type: 'button', 'aria-pressed': String(f.id === 'all') } });
    tools.appendChild(b);
    filterBtns.set(f.id, b);
  }
  body.appendChild(tools);

  const legend = el(doc, 'p', { className: 'mx-legend' });
  legend.textContent =
    `${GLYPHS.routed} routed · ${GLYPHS.notRouted} not routed · ` +
    `${GLYPHS.routedClean} routed to the CLEAN FEED · ${GLYPHS.notRoutedClean} not on the clean feed · ` +
    `${GLYPHS.pending} staged, not sent · dashed outline = locked, the write path is disarmed. ` +
    'Meters are peak hold, two channels, scaled -60 to 0 dBFS. Mute and fader are read-only here.';
  body.appendChild(legend);

  const matrixNote = el(doc, 'p', { className: 'mx-matrix-note', text: 'No mixer state to show.' });
  body.appendChild(matrixNote);

  const wrap = el(doc, 'div', { className: 'mx-matrix-wrap', attrs: { tabindex: '0', role: 'region', 'aria-label': 'Routing matrix' } });
  const table = el(doc, 'table', { className: 'mx-matrix' });
  const caption = el(doc, 'caption', { className: 'mx-caption' });
  caption.textContent =
    'Rows are channel strips, columns are the seven mixer buses. ' +
    `${busLabel(CLEAN_FEED_BUS)} is the feed the client receives.`;
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
    cleanBanner,
    armBtn,
    armState,
    applyBtn,
    discardBtn,
    notice,
    stagedBox,
    stagedList,
    awaitBox,
    awaitList,
    driftToggle,
    goldenBtn,
    driftBody,
    driftSummary,
    driftList,
    filterBtns,
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
 * pendingKey identifies one crosspoint in the staged-changes map.
 *
 * JSON, not a joined string. Strip names contain spaces ('MIC 1-1') and
 * hyphens ('cam22-1'), so there is no printable delimiter that is safe by
 * inspection; JSON.stringify of the pair is unambiguous for any pair of
 * strings, and nothing ever parses it back — the map's VALUE carries the strip
 * and bus.
 *
 * @param {string} strip
 * @param {string} bus
 * @returns {string}
 */
function pendingKey(strip, bus) {
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
