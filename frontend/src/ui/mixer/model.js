/**
 * The mixer drawer's pure model: everything the drawer decides that is not DOM.
 *
 * Owner: WP-M4. No DOM, no browser API, no imports beyond ./contract.js — so it
 * runs under `node --test` with nothing installed (package.json is frozen).
 *
 * ======================= WHY THE LOGIC LIVES HERE ===========================
 *
 * The drawer's write path can change a live clean feed. Every decision that
 * could put audio on air — what a crosspoint toggle means, whether a write is
 * permitted, which differences are CRITICAL — is a pure function here, tested
 * in model.test.js. drawer.js is then only wiring: it renders what this module
 * computes and hands operator gestures back to it.
 *
 * The one rule this module enforces above all others: NOTHING HERE SENDS.
 * createWriteGate is the only function that touches opts.sendCommands, and it
 * refuses unless it is both armed and given a named operator gesture.
 */

import { ALL_BUSES, CLEAN_FEED_BUS, NO_EGRESS_BUSES, busLabel } from './contract.js';

/* ========================================================================== */
/* Meter scale                                                                */
/* ========================================================================== */

/**
 * METER_FLOOR_DB is the bottom of the drawn meter, in dBFS.
 *
 * MEASURED: in the captured live frame (internal/m2lx/testdata/
 * switcher_status-live-2026-07-31.json) an idle strip reads -100.0 and an idle
 * bus reads -99.99999237060547, while the live commentary strip cam22-1 read
 * -87.7 and the one strip carrying programme, cam1-1, read -27.6/-28.1.
 *
 * A meter drawn from -100 would put every one of those in the bottom eighth and
 * -27 dBFS — real, audible programme — barely off the floor. -60 dBFS is the
 * bottom of the useful range: below it nothing is audible on air, and above it
 * the whole scale is spent on levels an operator can act on.
 */
export const METER_FLOOR_DB = -60;

/** METER_CEIL_DB is the top of the drawn meter: 0 dBFS, digital full scale. */
export const METER_CEIL_DB = 0;

/**
 * DIGITAL_SILENCE_DB is the threshold at or below which a reading is treated as
 * digital silence rather than as a very quiet signal.
 *
 * MEASURED: silence reads -100.0 (strips) or -99.99999237060547 (buses). The
 * threshold sits above both so neither is drawn as a signal, and far below any
 * level that could be heard.
 */
export const DIGITAL_SILENCE_DB = -99;

/**
 * MUTE_FADER_DB is the fader gain that means mute. MEASURED: set_ch_fader takes
 * dB and -144 is its mute value. Mirrors mixer.MuteFaderDB.
 */
export const MUTE_FADER_DB = -144;

/**
 * OVER_DB is where the meter reads as overloaded.
 *
 * MEASURED and the reason this matters: master sums at unity with NO LIMITER.
 * Two sources at -5 dBFS summed to +1 dBFS and produced a -27 dB distortion
 * residual. There is no headroom above 0 to spend, so the meter calls anything
 * from -1 dBFS up an overload rather than waiting for a clip that has already
 * been broadcast.
 */
export const OVER_DB = -1;

/** HOT_DB is where the meter warns that a source is approaching OVER_DB. */
export const HOT_DB = -6;

/** NOMINAL_DB is the bottom of normal programme level. */
export const NOMINAL_DB = -24;

/**
 * STALE_AFTER_MS is how long the drawer waits for an update() before it stops
 * claiming its view is current.
 *
 * update() arrives at approximately 1 Hz, so three missed frames is a stopped
 * feed rather than a slow one. A mixer view that silently stops updating is
 * worse than one that says it has stopped.
 */
export const STALE_AFTER_MS = 3500;

/**
 * isDigitalSilence reports whether a dBFS reading is the mixer's silence value
 * rather than a quiet signal.
 *
 * @param {unknown} db
 * @returns {boolean}
 */
export function isDigitalSilence(db) {
  const n = finite(db);
  return n === null || n <= DIGITAL_SILENCE_DB;
}

/**
 * meterFraction maps a dBFS reading onto 0..1 of the drawn meter.
 *
 * The scale is LINEAR IN dB across METER_FLOOR_DB..METER_CEIL_DB, not linear in
 * amplitude: -6 dBFS sits at 90% of the bar, -20 at 67%, -40 at 33%. A meter
 * linear in amplitude would compress everything below -20 dBFS into the bottom
 * tenth, which is where the levels this drawer exists to show actually live.
 *
 * Out-of-range and non-numeric readings clamp rather than escape the bar; a
 * missing meter must be reported through MixerStrip.metered, not inferred from
 * a value here.
 *
 * @param {unknown} db dBFS
 * @returns {number} 0..1
 */
export function meterFraction(db) {
  const n = finite(db);
  if (n === null) return 0;
  if (n <= METER_FLOOR_DB) return 0;
  if (n >= METER_CEIL_DB) return 1;
  return (n - METER_FLOOR_DB) / (METER_CEIL_DB - METER_FLOOR_DB);
}

/**
 * meterPercent is meterFraction as a percentage rounded to 0.1, ready for a CSS
 * width. Rounded so a 1 Hz update does not rewrite the style with a
 * seventeen-digit float.
 *
 * @param {unknown} db dBFS
 * @returns {number} 0..100
 */
export function meterPercent(db) {
  return Math.round(meterFraction(db) * 1000) / 10;
}

/**
 * meterZone classifies a reading for presentation. Returned as a word, not a
 * colour: the drawer pairs it with a glyph and a number so it survives a dim
 * room, a monochrome monitor and a colourblind operator.
 *
 * @param {unknown} db dBFS
 * @returns {'silence'|'low'|'nominal'|'hot'|'over'}
 */
export function meterZone(db) {
  if (isDigitalSilence(db)) return 'silence';
  const n = finite(db);
  if (n === null) return 'silence';
  if (n >= OVER_DB) return 'over';
  if (n >= HOT_DB) return 'hot';
  if (n >= NOMINAL_DB) return 'nominal';
  return 'low';
}

/**
 * formatDb renders a dBFS or dB value for the operator.
 *
 * MUTE_FADER_DB renders as 'mute' and digital silence as 'silent', because
 * '-144.0 dB' and '-100.0 dB' are values an operator has to decode under
 * pressure, and both mean something simpler than they look.
 *
 * @param {unknown} db
 * @param {{silenceAsWord?: boolean}} [opts] silenceAsWord defaults to true
 * @returns {string}
 */
export function formatDb(db, opts = {}) {
  const silenceAsWord = opts.silenceAsWord !== false;
  const n = finite(db);
  if (n === null) return '--';
  if (n <= MUTE_FADER_DB) return 'mute';
  if (silenceAsWord && n <= DIGITAL_SILENCE_DB) return 'silent';
  const rounded = Math.round(n * 10) / 10;
  return `${rounded > 0 ? '+' : ''}${rounded.toFixed(1)} dB`;
}

/* ========================================================================== */
/* Snapshot -> matrix model                                                   */
/* ========================================================================== */

/**
 * @typedef {Object} MatrixCell
 * @property {string} bus
 * @property {string} label       busLabel(bus)
 * @property {boolean} routed     current routing from the snapshot
 * @property {boolean} cleanFeed  bus is CLEAN_FEED_BUS
 * @property {boolean} noEgress   bus reaches no output at all
 */

/**
 * @typedef {Object} MatrixRow
 * @property {string} name        strip wire name, e.g. 'cam22-1'
 * @property {string} input       parent input, e.g. 'cam22'
 * @property {string} label       operator-facing name; never blank
 * @property {boolean} muted
 * @property {boolean} inCleanFeed  routed to CLEAN_FEED_BUS
 * @property {boolean} audibleOnCleanFeed  in the clean feed AND not muted
 * @property {string[]} outputs
 * @property {MatrixCell[]} cells one per bus, in ALL_BUSES order
 * @property {boolean} metered
 * @property {[number,number]} peakHold
 * @property {[number,number]} level
 * @property {number} peakMax     louder of the two peak-hold channels
 * @property {string} faderText
 * @property {string} subChMode
 */

/**
 * @typedef {Object} MatrixModel
 * @property {boolean} hasData    false when there is no snapshot yet; the drawer
 *                                must say so rather than render an empty mixer
 * @property {MatrixRow[]} rows
 * @property {Object[]} buses     per-bus header state
 * @property {string} takenAt
 * @property {MatrixRow[]} cleanFeed  rows routed to CLEAN_FEED_BUS
 * @property {string} structureKey  changes only when the set of strips or buses
 *                                changes; lets the drawer update in place at
 *                                1 Hz instead of rebuilding and stealing focus
 */

/**
 * stripLabel returns the name to show an operator for a strip.
 *
 * MixerStrip.displayName may be '' — the contract says fall back to the input,
 * never to blank. In the captured live frame the per-strip display_name is the
 * wire name ('cam22-1') while the operator-facing name ('CLAUDE-COMMS') is on
 * the per-input node, so the input is the better of the two fallbacks.
 *
 * @param {import('./contract.js').MixerStrip} strip
 * @returns {string}
 */
export function stripLabel(strip) {
  if (!strip || typeof strip !== 'object') return '(unknown strip)';
  const dn = str(strip.displayName);
  if (dn !== '') return dn;
  const input = str(strip.input);
  if (input !== '') return input;
  const name = str(strip.name);
  return name !== '' ? name : '(unnamed strip)';
}

/**
 * faderText renders a strip's per-channel fader for display.
 *
 * The channels are reported separately and CAN DIFFER, so a single number would
 * be a guess. Where a channel's fader is not in circuit the gain is not applied
 * to any audio and is shown as 'off' rather than as a gain the operator might
 * believe in.
 *
 * MEASURED, and the reason 'off' is not a hypothetical: in the captured live
 * frame only 8 of the 54 strips have a ch_fader in circuit. The other 46 report
 * enabled=[false,false] with gain=[0,0] — and 0 dB is exactly the value that
 * would render as a believable unity gain if the enabled flag were ignored.
 *
 * @param {import('./contract.js').MixerStrip} strip
 * @returns {string}
 */
export function faderText(strip) {
  const gain = pair(strip && strip.fader);
  const enabled = boolPair(strip && strip.faderEnabled);
  const one = (i) => (enabled[i] ? formatDb(gain[i], { silenceAsWord: false }) : 'off');
  const l = one(0);
  const r = one(1);
  return l === r ? l : `L ${l} / R ${r}`;
}

/**
 * buildMatrixModel turns a MixerSnapshot into everything the matrix renders.
 *
 * Rows keep the snapshot's order, and columns are always ALL_BUSES, so the
 * drawer does not reorder itself between 1 Hz updates. A null or malformed
 * snapshot yields hasData:false rather than an empty-looking mixer: "no state"
 * and "nothing is routed" must never look the same.
 *
 * @param {import('./contract.js').MixerSnapshot|null|undefined} snapshot
 * @returns {MatrixModel}
 */
export function buildMatrixModel(snapshot) {
  const strips = Array.isArray(snapshot && snapshot.strips) ? snapshot.strips : null;
  const busStates = Array.isArray(snapshot && snapshot.buses) ? snapshot.buses : [];

  if (!strips) {
    return {
      hasData: false,
      rows: [],
      buses: busHeaders([]),
      takenAt: '',
      cleanFeed: [],
      structureKey: '',
    };
  }

  const rows = strips
    .filter((s) => s && typeof s === 'object' && str(s.name) !== '')
    .map((s) => {
      const outputs = names(s.outputs);
      const peakHold = pair(s.peakHold);
      const muted = s.muted === true;
      const inCleanFeed = outputs.includes(CLEAN_FEED_BUS);
      return {
        name: str(s.name),
        input: str(s.input),
        label: stripLabel(s),
        muted,
        inCleanFeed,
        audibleOnCleanFeed: inCleanFeed && !muted,
        outputs,
        cells: ALL_BUSES.map((bus) => ({
          bus,
          label: busLabel(bus),
          routed: outputs.includes(bus),
          cleanFeed: bus === CLEAN_FEED_BUS,
          noEgress: NO_EGRESS_BUSES.includes(bus),
        })),
        metered: s.metered === true,
        peakHold,
        level: pair(s.level),
        peakMax: Math.max(peakHold[0], peakHold[1]),
        faderText: faderText(s),
        subChMode: str(s.subChMode),
      };
    });

  return {
    hasData: true,
    rows,
    buses: busHeaders(busStates),
    takenAt: str(snapshot.takenAt),
    cleanFeed: rows.filter((r) => r.inCleanFeed),
    structureKey: JSON.stringify(rows.map((r) => r.name)),
  };
}

function busHeaders(busStates) {
  const byName = new Map();
  for (const b of busStates) {
    if (b && typeof b === 'object' && str(b.name) !== '') byName.set(str(b.name), b);
  }
  return ALL_BUSES.map((bus) => {
    const b = byName.get(bus) || null;
    const peakHold = pair(b && b.peakHold);
    return {
      name: bus,
      label: busLabel(bus),
      cleanFeed: bus === CLEAN_FEED_BUS,
      noEgress: NO_EGRESS_BUSES.includes(bus),
      known: b !== null,
      muted: b ? b.muted === true : false,
      metered: b ? b.metered === true : false,
      peakHold,
      peakMax: Math.max(peakHold[0], peakHold[1]),
      faderPresent: b ? b.faderPresent === true : false,
      fader: b && typeof b.fader === 'number' ? b.fader : 0,
    };
  });
}

/* ========================================================================== */
/* Crosspoint edits                                                           */
/* ========================================================================== */

/**
 * routingAfterToggle returns the COMPLETE resulting bus set after setting one
 * crosspoint.
 *
 * set_routing is a REPLACE, not an add or a remove: whatever this returns is
 * exactly what the strip will be routed to afterwards. Unknown bus names in the
 * current routing are preserved rather than dropped — a bus this build has
 * never heard of is still carrying audio, and silently un-routing it would be a
 * write the operator did not ask for.
 *
 * @param {string[]} currentOutputs the strip's present routing
 * @param {string} bus the crosspoint being changed
 * @param {boolean} routed desired state of that crosspoint
 * @returns {string[]} the complete resulting set, known buses in ALL_BUSES order
 */
export function routingAfterToggle(currentOutputs, bus, routed) {
  const target = str(bus);
  if (target === '') throw new Error('mixer: routingAfterToggle needs a bus name');

  const current = new Set(names(currentOutputs));
  if (routed) current.add(target);
  else current.delete(target);

  const known = ALL_BUSES.filter((b) => current.has(b));
  const unknown = [...current].filter((b) => !ALL_BUSES.includes(b)).sort();
  return [...known, ...unknown];
}

/**
 * routingCommand builds one set_routing write from a COMPLETE resulting bus
 * set.
 *
 * Only the output matrix is offered. PFL routing feeds mon4 and cannot reach
 * air, so it is not worth a live write from this drawer.
 *
 * NOTE the argument key: set_routing's "input" takes a STRIP name ('cam22-1'),
 * not an input name ('cam22'), despite the key. Mirrors mixer.SetRouting.
 *
 * There is deliberately no guard against an empty outputs array. Sending one
 * un-routes the strip from programme as well, which is occasionally what an
 * operator means; the drawer's job is to say so in words before it is sent, not
 * to quietly refuse a gesture that was made on purpose. mixer.SetRouting
 * rejects a NIL Outputs, which is a different thing: "no change" expressed as a
 * command.
 *
 * @param {string} strip strip wire name, e.g. 'cam22-1'
 * @param {string[]} outputs complete resulting bus set
 * @returns {import('./contract.js').MixerCommand}
 */
export function routingCommand(strip, outputs) {
  const name = str(strip);
  if (name === '') throw new Error('mixer: routingCommand needs a strip name');
  if (!Array.isArray(outputs)) throw new Error('mixer: routingCommand needs the complete resulting output set');
  return {
    kind: 'setRouting',
    args: { matrix: 'output', input: name, outputs: names(outputs) },
  };
}

/**
 * crosspointCommand builds the single set_routing write for one crosspoint.
 *
 * @param {MatrixRow} row
 * @param {string} bus
 * @param {boolean} routed desired crosspoint state
 * @returns {import('./contract.js').MixerCommand}
 */
export function crosspointCommand(row, bus, routed) {
  if (!row || str(row.name) === '') throw new Error('mixer: crosspointCommand needs a strip');
  return routingCommand(str(row.name), routingAfterToggle(row.outputs, bus, routed));
}

/**
 * describeCrosspointChange states, in the operator's language, what a staged
 * crosspoint change will do. Shown next to the Apply control, because "apply 3
 * changes" is not informed consent for putting a microphone on the clean feed.
 *
 * @param {MatrixRow} row
 * @param {string} bus
 * @param {boolean} routed desired state
 * @returns {string}
 */
export function describeCrosspointChange(row, bus, routed) {
  const label = row && row.label ? row.label : '(unknown strip)';
  if (bus === CLEAN_FEED_BUS) {
    return routed
      ? `${label} INTO the clean feed (${busLabel(bus)})`
      : `${label} OUT of the clean feed (${busLabel(bus)})`;
  }
  return routed ? `${label} into ${busLabel(bus)}` : `${label} out of ${busLabel(bus)}`;
}

/**
 * changeSeverity ranks a staged crosspoint change the same way a Diff is
 * ranked, so the confirmation an operator reads before writing uses the same
 * words as the drift list they read afterwards.
 *
 * @param {string} bus
 * @returns {'info'|'warn'|'critical'}
 */
export function changeSeverity(bus) {
  return EGRESS_BUSES.includes(bus) ? 'critical' : 'warn';
}

/**
 * EGRESS_BUSES are the buses that leave the building: master feeds PGM and
 * aux1 feeds CLN. A change to either is audible to the client.
 */
export const EGRESS_BUSES = Object.freeze(['master', CLEAN_FEED_BUS]);

/* ========================================================================== */
/* The write gate                                                             */
/* ========================================================================== */

/**
 * @typedef {Object} WriteResult
 * @property {boolean} sent   true only when sendCommands resolved
 * @property {string} reason  why not, when sent is false; '' when sent
 */

/**
 * STALE_VIEW_REFUSAL is why a write from a stale view is refused, in the words
 * the operator is shown. Exported so the drawer and the tests quote the same
 * sentence rather than two that might drift.
 */
export const STALE_VIEW_REFUSAL =
  'the view is not live, and set_routing REPLACES a strip\'s whole bus set - ' +
  'applying from a stale matrix would send every OTHER bus back to what it was when the feed stopped';

/**
 * createWriteGate is the ONLY path from this drawer to the mixer.
 *
 * Every precondition the contract puts on opts.sendCommands is enforced here
 * rather than at the call sites, so that adding a control cannot accidentally
 * add a way around them:
 *
 *   - armed. isArmed() is read at the moment of the write, not captured when
 *     the control was built, so a control rendered while armed cannot fire
 *     after a disarm.
 *   - a FRESH VIEW. viewIsFresh() is likewise read at the moment of the write.
 *   - a named operator gesture. Lifecycle hooks and timers have no gesture to
 *     name, which is the point: a drawer that "corrects" routing by itself
 *     changes a live clean feed while nobody is looking at it.
 *   - never an empty array.
 *
 * ======================= WHY FRESHNESS IS A GATE ============================
 *
 * set_routing is an ABSOLUTE REPLACE, and the drawer builds its argument from
 * the routing it has on screen. So a write from a stale view is not "one
 * change, slightly late": it is one change PLUS a rollback of every other bus
 * on that strip to whatever they were when the feed stopped.
 *
 * The scenario is not hypothetical — the status feed stalls, which is why the
 * controller carries a full reconnect supervisor. The header reads STALE, the
 * matrix still shows the old frame, somebody changes routing on the desk, and
 * the operator takes commentary out of the clean feed from the stale matrix.
 * The bus they aimed at is correct; every other bus in that command is a
 * forty-second-old rollback applied to a live desk.
 *
 * This check lives HERE, at the gate, and not on the Apply button's disabled
 * state, because a disabled button is a suggestion: a keyboard activation, a
 * programmatic click, or a future control that forgets to consult freshness
 * all reach submit() and none of them reach the button. The caller is expected
 * to refuse earlier and more legibly as well — see applyPending in drawer.js,
 * which re-reads and re-plans — but nothing gets past this.
 *
 * A resolved promise means SENT, NOT APPLIED. This returns {sent:true} and
 * nothing more; confirmation must come from a following snapshot. At least one
 * command in this vocabulary is silently inert when misused and reads back
 * exactly as sent (MEASURED: set_comp_limit's limiter_th does nothing unless
 * agc_mode is "limiter").
 *
 * @param {{
 *   sendCommands: (cmds: import('./contract.js').MixerCommand[]) => Promise<void>,
 *   isArmed: () => boolean,
 *   viewIsFresh: () => {fresh: boolean, text: string},
 *   onError: (err: Error, context: string) => void,
 * }} deps
 * @returns {{submit: (cmds: import('./contract.js').MixerCommand[], gesture: string) => Promise<WriteResult>}}
 */
export function createWriteGate(deps) {
  const { sendCommands, isArmed, viewIsFresh, onError } = deps || {};
  if (typeof sendCommands !== 'function') throw new Error('mixer: write gate needs sendCommands');
  if (typeof isArmed !== 'function') throw new Error('mixer: write gate needs isArmed');
  // Required, not optional. An optional safety check is one a future call site
  // omits by accident, and this one stands between a stale matrix and a live
  // clean feed.
  if (typeof viewIsFresh !== 'function') throw new Error('mixer: write gate needs viewIsFresh');

  const report = (err, context) => {
    if (typeof onError === 'function') onError(err, context);
  };

  return {
    async submit(cmds, gesture) {
      const named = str(gesture);
      if (named === '') {
        // No gesture means no operator. Refused before anything else so that a
        // lifecycle hook cannot reach the mixer even while armed.
        return { sent: false, reason: 'refused: a write must name the operator gesture that caused it' };
      }
      if (!isArmed()) {
        return { sent: false, reason: 'the write path is disarmed - press Arm to enable changes' };
      }
      // Read at the moment of the write, exactly like isArmed: a plan built
      // while the feed was live must not be sent after it stopped.
      const fresh = viewIsFresh();
      if (!fresh || fresh.fresh !== true) {
        const detail = fresh && str(fresh.text) !== '' ? ` (${str(fresh.text)})` : '';
        return { sent: false, reason: `refused: ${STALE_VIEW_REFUSAL}${detail}` };
      }
      if (!Array.isArray(cmds) || cmds.length === 0) {
        return { sent: false, reason: 'nothing to send' };
      }
      try {
        await sendCommands(cmds);
      } catch (err) {
        const e = err instanceof Error ? err : new Error(String(err));
        report(e, `mixer write (${named})`);
        return { sent: false, reason: `send failed: ${e.message}` };
      }
      return { sent: true, reason: '' };
    },
  };
}

/* ========================================================================== */
/* Drift                                                                      */
/* ========================================================================== */

/**
 * SEVERITY_RANK orders severities most severe first. Mirrors the ranking in
 * mixer.Severity: the question is audibility on an output that leaves the
 * building, not how many fields changed.
 */
export const SEVERITY_RANK = Object.freeze({ critical: 0, warn: 1, info: 2 });

/**
 * sortDiffs orders diffs most severe first, stable within a severity.
 *
 * Applied even to diffs computed elsewhere. The drawer's job is to put CRITICAL
 * where the eye lands, and it must not depend on another component's ordering
 * to do it.
 *
 * @param {import('./contract.js').MixerDiff[]} diffs
 * @returns {import('./contract.js').MixerDiff[]} a new array
 */
export function sortDiffs(diffs) {
  const list = Array.isArray(diffs) ? diffs.filter((d) => d && typeof d === 'object') : [];
  return list
    .map((d, i) => ({ d, i }))
    .sort((a, b) => {
      const ra = rank(a.d.severity);
      const rb = rank(b.d.severity);
      return ra !== rb ? ra - rb : a.i - b.i;
    })
    .map((x) => x.d);
}

function rank(sev) {
  return Object.prototype.hasOwnProperty.call(SEVERITY_RANK, sev) ? SEVERITY_RANK[sev] : SEVERITY_RANK.info;
}

/**
 * diffHeadline renders one difference as a sentence an operator can act on.
 *
 * It works from the Diff's rendered strings alone, so it produces the same
 * sentence for a diff computed here and for one computed by the Go Compare.
 * The clean-feed cases are called out by name: 'outputs: master, aux1, aux2'
 * is not a sentence that makes anybody check a microphone.
 *
 * @param {import('./contract.js').MixerDiff} diff
 * @returns {string}
 */
export function diffHeadline(diff) {
  if (!diff || typeof diff !== 'object') return '(empty difference)';
  const label = str(diff.label) || str(diff.target) || '(unknown)';
  const field = str(diff.field);
  const golden = str(diff.golden);
  const current = str(diff.current);

  if (field === 'outputs') {
    const wasClean = renderedHasBus(golden, CLEAN_FEED_BUS);
    const nowClean = renderedHasBus(current, CLEAN_FEED_BUS);
    if (!wasClean && nowClean) {
      return `${label} IS NOW IN THE CLEAN FEED (${busLabel(CLEAN_FEED_BUS)}) - it was not in the golden state`;
    }
    if (wasClean && !nowClean) {
      return `${label} has been REMOVED from the clean feed (${busLabel(CLEAN_FEED_BUS)})`;
    }
    const wasPgm = renderedHasBus(golden, 'master');
    const nowPgm = renderedHasBus(current, 'master');
    if (wasPgm && !nowPgm) return `${label} has been REMOVED from programme (master (PGM))`;
    if (!wasPgm && nowPgm) return `${label} is now in programme (master (PGM))`;
    return `${label} routing changed: was ${golden}, now ${current}`;
  }

  if (field === 'muted') {
    if (current === 'true' || current === 'muted') return `${label} is now MUTED (it was not in the golden state)`;
    return `${label} is now UNMUTED (it was muted in the golden state)`;
  }

  if (field === 'present') {
    return current === 'absent'
      ? `${label} has DISAPPEARED from the mixer since golden`
      : `${label} has APPEARED on the mixer since golden`;
  }

  return `${label}: ${field || 'value'} was ${golden || '(none)'}, now ${current || '(none)'}`;
}

/**
 * compareSnapshots computes drift from a golden snapshot, in the drawer.
 *
 * WHY THIS EXISTS AT ALL, and it is a deviation worth reading: MixerDiff says
 * "Values arrive pre-rendered as strings; the drawer displays them and does not
 * recompute" — but MixerDrawerOptions has no member that returns diffs. It
 * offers getGolden(), which returns a SNAPSHOT. With the options as written,
 * something in the drawer has to do the comparison. This does it, following the
 * severity rules documented on mixer.Severity so the two agree:
 *
 *   critical - anything that changes what is heard on an output that leaves the
 *              building. A strip GAINING aux1 is the defining case; losing
 *              master is programme audio disappearing.
 *   warn     - audible only on a monitored or undeliverable bus (mon1..mon4,
 *              aux2), or on a strip that is muted anyway.
 *   info     - changes nobody hears, e.g. a display name.
 *
 * Levels and peak-hold are DELIBERATELY NOT COMPARED. They change every frame;
 * emitting them would bury the criticals under a hundred info lines a second,
 * which is the same as not showing the criticals at all.
 *
 * If the host later gains a diff source backed by mixer.Compare, pass it as
 * opts.getDiffs and this function goes unused. See drawer.js.
 *
 * @param {import('./contract.js').MixerSnapshot|null} golden
 * @param {import('./contract.js').MixerSnapshot|null} current
 * @returns {import('./contract.js').MixerDiff[]} most severe first
 */
export function compareSnapshots(golden, current) {
  if (!golden || !current) return [];
  const out = [];

  const gStrips = indexBy(golden.strips, 'name');
  const cStrips = indexBy(current.strips, 'name');

  for (const name of union(gStrips, cStrips)) {
    const g = gStrips.get(name) || null;
    const c = cStrips.get(name) || null;
    const label = stripLabel(c || g);

    if (!c) {
      out.push(diff('strip', name, label, 'present', 'present', 'absent', touchesEgress(names(g.outputs)) ? 'critical' : 'warn'));
      continue;
    }
    if (!g) {
      out.push(diff('strip', name, label, 'present', 'absent', 'present', touchesEgress(names(c.outputs)) ? 'critical' : 'warn'));
      continue;
    }

    const gOut = names(g.outputs);
    const cOut = names(c.outputs);
    if (!sameSet(gOut, cOut)) {
      out.push(diff('strip', name, label, 'outputs', renderBuses(gOut), renderBuses(cOut), routingSeverity(gOut, cOut)));
    }

    const gPfl = names(g.pflOutputs);
    const cPfl = names(c.pflOutputs);
    if (!sameSet(gPfl, cPfl)) {
      out.push(diff('strip', name, label, 'pflOutputs', renderBuses(gPfl), renderBuses(cPfl), 'warn'));
    }

    // A mute change on a strip routed to air is audible to the client, so it
    // is ranked with the routing changes rather than below them.
    const onAir = touchesEgress(gOut) || touchesEgress(cOut);
    if ((g.muted === true) !== (c.muted === true)) {
      out.push(diff('strip', name, label, 'muted', String(g.muted === true), String(c.muted === true), onAir ? 'critical' : 'warn'));
    }

    // A fader move only reaches air if the strip is routed there AND unmuted.
    const audible = onAir && c.muted !== true;
    if (!samePair(g.fader, c.fader)) {
      out.push(diff('strip', name, label, 'fader', faderText(g), faderText(c), audible ? 'critical' : 'warn'));
    }
    if (!sameBoolPair(g.faderEnabled, c.faderEnabled)) {
      out.push(diff('strip', name, label, 'faderEnabled', renderBoolPair(g.faderEnabled), renderBoolPair(c.faderEnabled), audible ? 'critical' : 'warn'));
    }
    if (str(g.subChMode) !== str(c.subChMode)) {
      out.push(diff('strip', name, label, 'subChMode', str(g.subChMode), str(c.subChMode), audible ? 'critical' : 'warn'));
    }
    if ((g.follow === true) !== (c.follow === true)) {
      // follow does not change what is heard now; it changes what happens the
      // next time the source changes, which is exactly how a corrected routing
      // comes back.
      out.push(diff('strip', name, label, 'follow', String(g.follow === true), String(c.follow === true), 'warn'));
    }
    if (!sameSet(names(g.followSources), names(c.followSources))) {
      out.push(diff('strip', name, label, 'followSources', names(g.followSources).join(', '), names(c.followSources).join(', '), 'warn'));
    }
    if (str(g.displayName) !== str(c.displayName)) {
      out.push(diff('strip', name, label, 'displayName', str(g.displayName), str(c.displayName), 'info'));
    }
  }

  const gBuses = indexBy(golden.buses, 'name');
  const cBuses = indexBy(current.buses, 'name');
  for (const name of union(gBuses, cBuses)) {
    const g = gBuses.get(name) || null;
    const c = cBuses.get(name) || null;
    const label = busLabel(name);
    const egress = EGRESS_BUSES.includes(name);

    if (!c || !g) {
      out.push(diff('bus', name, label, 'present', g ? 'present' : 'absent', c ? 'present' : 'absent', egress ? 'critical' : 'warn'));
      continue;
    }
    if ((g.muted === true) !== (c.muted === true)) {
      out.push(diff('bus', name, label, 'muted', String(g.muted === true), String(c.muted === true), egress ? 'critical' : 'warn'));
    }
    if (g.faderPresent === true && c.faderPresent === true && finite(g.fader) !== finite(c.fader)) {
      out.push(diff('bus', name, label, 'fader', String(finite(g.fader)), String(finite(c.fader)), egress ? 'critical' : 'warn'));
    }
    if (intOr(g.channelCount, -1) !== intOr(c.channelCount, -1)) {
      out.push(diff('bus', name, label, 'channelCount', String(intOr(g.channelCount, -1)), String(intOr(c.channelCount, -1)), 'warn'));
    }
  }

  return sortDiffs(out);
}

/**
 * routingSeverity ranks a routing change by which buses entered or left.
 *
 * A change touching master or aux1 is CRITICAL unconditionally: those are the
 * PGM and CLN outputs. Everything else is a monitored or undeliverable bus.
 *
 * @param {string[]} goldenOutputs
 * @param {string[]} currentOutputs
 * @returns {'info'|'warn'|'critical'}
 */
export function routingSeverity(goldenOutputs, currentOutputs) {
  const g = new Set(names(goldenOutputs));
  const c = new Set(names(currentOutputs));
  const changed = [...new Set([...g, ...c])].filter((b) => g.has(b) !== c.has(b));
  return touchesEgress(changed) ? 'critical' : 'warn';
}

function touchesEgress(buses) {
  return names(buses).some((b) => EGRESS_BUSES.includes(b));
}

function diff(kind, target, label, field, golden, current, severity) {
  return { kind, target, label, field, golden, current, severity };
}

/**
 * busListText renders a set of buses for an operator to READ.
 *
 * Every bus goes through busLabel. contract.js states the rule this
 * implements: "Never render a raw bus name. An operator reading 'aux1' has no
 * way to know they are looking at the clean feed." A list is the case where
 * that bites hardest, because 'master, aux1, aux2' is the DEFAULT routing of
 * every strip — the exact string an operator is most likely to skim past — and
 * it is the string that says commentary is in the client's clean feed.
 *
 * This is also what mixer.renderBuses in internal/mixer/golden.go does, so a
 * diff computed here and one computed by the Go Compare read identically.
 *
 * Empty renders as '(none)' rather than as blank: "not routed anywhere" is a
 * statement, and a blank is an absence the operator has to interpret.
 *
 * @param {string[]} buses wire names
 * @returns {string}
 */
export function busListText(buses) {
  const l = names(buses);
  return l.length === 0 ? '(none)' : l.map(busLabel).join(', ');
}

function renderBuses(list) {
  return busListText(list);
}

function renderBoolPair(v) {
  const p = boolPair(v);
  return `${p[0] ? 'on' : 'off'} / ${p[1] ? 'on' : 'off'}`;
}

/**
 * renderedHasBus tests a rendered value string for a bus as a whole token, so
 * that 'aux1' does not match inside 'aux10' or 'maux1'.
 *
 * @param {string} rendered e.g. 'master, aux1, aux2'
 * @param {string} bus
 * @returns {boolean}
 */
export function renderedHasBus(rendered, bus) {
  if (typeof rendered !== 'string' || rendered === '') return false;
  return rendered
    .split(/[\s,]+/)
    .filter((t) => t !== '')
    .includes(bus);
}

/* ========================================================================== */
/* Staleness                                                                  */
/* ========================================================================== */

/**
 * viewFreshness reports whether the drawer may still claim its view is current.
 *
 * lastUpdateAt is the LOCAL clock at the last update() call, not the frame
 * timestamp: the question is "is state still arriving", which a frame's own
 * timestamp cannot answer once frames have stopped arriving.
 *
 * @param {number|null} lastUpdateAt Date.now() at the last update(), or null
 * @param {number} now Date.now()
 * @returns {{fresh: boolean, ageMs: number, text: string}}
 */
export function viewFreshness(lastUpdateAt, now) {
  if (typeof lastUpdateAt !== 'number' || !Number.isFinite(lastUpdateAt)) {
    return { fresh: false, ageMs: Infinity, text: 'no state received yet' };
  }
  const ageMs = Math.max(0, now - lastUpdateAt);
  if (ageMs <= STALE_AFTER_MS) return { fresh: true, ageMs, text: 'live' };
  return { fresh: false, ageMs, text: `STALE - no update for ${Math.round(ageMs / 1000)}s` };
}

/* ========================================================================== */
/* Coercion helpers                                                           */
/* ========================================================================== */
/* Snapshots cross a JSON bridge from Go. Every field is treated as untrusted:
 * a missing meter must render as absent, and a missing routing must never
 * render as "not in the clean feed" by accident. */

function finite(v) {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function str(v) {
  return typeof v === 'string' ? v : '';
}

function intOr(v, fallback) {
  return typeof v === 'number' && Number.isFinite(v) ? Math.trunc(v) : fallback;
}

function names(v) {
  if (!Array.isArray(v)) return [];
  return v.filter((x) => typeof x === 'string' && x !== '');
}

function pair(v) {
  if (!Array.isArray(v)) return [METER_FLOOR_DB, METER_FLOOR_DB];
  const a = finite(v[0]);
  const b = finite(v[1]);
  return [a === null ? METER_FLOOR_DB : a, b === null ? METER_FLOOR_DB : b];
}

function boolPair(v) {
  if (!Array.isArray(v)) return [false, false];
  return [v[0] === true, v[1] === true];
}

function samePair(a, b) {
  const pa = pair(a);
  const pb = pair(b);
  return pa[0] === pb[0] && pa[1] === pb[1];
}

function sameBoolPair(a, b) {
  const pa = boolPair(a);
  const pb = boolPair(b);
  return pa[0] === pb[0] && pa[1] === pb[1];
}

function sameSet(a, b) {
  if (a.length !== b.length) return false;
  const sa = new Set(a);
  return b.every((x) => sa.has(x));
}

function indexBy(list, key) {
  const m = new Map();
  if (!Array.isArray(list)) return m;
  for (const item of list) {
    if (item && typeof item === 'object' && str(item[key]) !== '') m.set(str(item[key]), item);
  }
  return m;
}

function union(a, b) {
  return [...new Set([...a.keys(), ...b.keys()])];
}
