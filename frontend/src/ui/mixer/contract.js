/**
 * The seam between the mixer drawer and the rest of the application.
 *
 * Owner: WP-M0. THIS FILE IS A CONTRACT, not an implementation — the factory
 * below throws, and WP-M4 replaces the body with the real drawer in this same
 * directory. It exists so that WP-M4 can be built and reviewed without editing
 * app.js, home.js, settings.js or backend.js, which another workflow owns.
 *
 * The Go side of the same contract is internal/mixer/mixer.go. Where a fact
 * about the mixer is stated here it is stated there too; that duplication is
 * deliberate, because the operator-facing warnings must not depend on a
 * reader having opened the other file.
 *
 * ======================= WHAT THE DRAWER IS FOR =============================
 *
 * The M2L-X advanced audio mixer has seven stereo buses. Two leave the
 * building: master feeds the PGM output, and aux1 feeds the CLN output.
 *
 * AUX1 IS THE CLEAN FEED. Sony's mixer surface calls that bus "AUX" and Sony's
 * output list calls the same bus "cln", and nothing in their UI says they are
 * the same bus. Meanwhile EVERY STRIP DEFAULTS TO ["master","aux1","aux2"], so
 * any input that is unmuted is in the clean feed unless somebody corrected its
 * routing. In the captured live frame the commentary input cam22-1 —
 * display name "CLAUDE-COMMS" — is routed exactly that way.
 *
 * So the drawer's primary job is not control. It is to show, in words an
 * operator cannot misread, which strips are in the clean feed. Control is
 * secondary and gated.
 *
 * ======================= THE SAFETY MODEL ===================================
 *
 * 1. The drawer is READ-ONLY until setArmed(true) is called. Before that it
 *    renders state and offers no control that can reach the mixer.
 * 2. The drawer NEVER WRITES ON ITS OWN. No write on open, on close, on
 *    update, on arm, on destroy, or on a diff appearing. Every call to
 *    opts.sendCommands originates in a specific operator gesture made while
 *    armed. A drawer that "corrects" routing by itself is a drawer that
 *    changes a live clean feed while nobody is looking at it.
 * 3. A resolved sendCommands means SENT, not applied. Confirmation comes from
 *    the next snapshot, never from the promise.
 */

/**
 * BUS_LABELS mirrors mixer.BusLabel in internal/mixer/mixer.go.
 *
 * It is duplicated rather than fetched because these strings are the mitigation
 * for putting commentary into the clean feed by accident, and they must render
 * correctly on the very first frame — including when the backend is
 * unreachable and there is no state to fetch them with.
 *
 * Keep in step with internal/mixer/mixer.go. If the two ever disagree, the Go
 * table is authoritative.
 */
export const BUS_LABELS = Object.freeze({
  master: 'master (PGM)',
  aux1: 'aux1 (CLN - clean feed)',
  aux2: 'aux2 (no egress)',
  mon1: 'mon1',
  mon2: 'mon2',
  mon3: 'mon3',
  mon4: 'mon4 (PFL)',
});

/**
 * ALL_BUSES is the presentation order: the buses with egress first, then the
 * undeliverable one, then the monitors. Iterate this rather than Object.keys
 * of a snapshot so the drawer does not reorder itself between 1 Hz updates.
 */
export const ALL_BUSES = Object.freeze(['master', 'aux1', 'aux2', 'mon1', 'mon2', 'mon3', 'mon4']);

/**
 * CLEAN_FEED_BUS is the bus whose presence on a strip means that strip is in
 * the clean feed. Referenced by name so no call site hard-codes 'aux1' next to
 * a comment that may drift.
 */
export const CLEAN_FEED_BUS = 'aux1';

/**
 * NO_EGRESS_BUSES lists buses that are routable and metered but that reach no
 * output at all. MEASURED: nothing accepts aux2 as a source and it is not on
 * the WebRTC monitor. The drawer must mark these plainly, because a bus with a
 * working fader and a moving meter otherwise looks like a usable send.
 */
export const NO_EGRESS_BUSES = Object.freeze(['aux2']);

/**
 * busLabel returns the operator-facing label for a bus name.
 *
 * Never render a raw bus name. An operator reading "aux1" has no way to know
 * they are looking at the clean feed.
 *
 * @param {string} bus wire name, e.g. 'aux1'
 * @returns {string} label, e.g. 'aux1 (CLN - clean feed)'
 */
export function busLabel(bus) {
  if (Object.prototype.hasOwnProperty.call(BUS_LABELS, bus)) return BUS_LABELS[bus];
  if (typeof bus !== 'string' || bus === '') return '(unnamed bus)';
  return `${bus} (unknown bus)`;
}

/**
 * @typedef {Object} MixerStrip
 * One channel strip, mirroring mixer.Strip. Field names are the Go JSON tags.
 * @property {string} name          wire name, e.g. 'cam22-1'
 * @property {string} input         parent input, e.g. 'cam22'
 * @property {string} displayName   operator-facing name, e.g. 'CLAUDE-COMMS';
 *                                  may be '' — fall back to input, never to blank
 * @property {boolean} muted        mute flag, independent of the fader
 * @property {boolean} follow
 * @property {string[]} followSources  populated even when follow is false; not a proxy for it
 * @property {string} subChMode     effective mode, e.g. 'ST_W' or 'MONO'
 * @property {string[]} outputs     buses this strip feeds; containing 'aux1' means CLEAN FEED
 * @property {string[]} pflOutputs
 * @property {[number,number]} level     dBFS [L,R]; meaningless unless metered
 * @property {[number,number]} peakHold  dBFS [L,R]; meaningless unless metered
 * @property {boolean} metered      false means NO METER DATA. Draw it as absent.
 *                                  Drawing the 0.0 default would show full scale.
 * @property {[number,number]} fader      dB [L,R]; -144 is mute. Per-channel, and the
 *                                        channels can differ
 * @property {[boolean,boolean]} faderEnabled  whether the fader is in circuit; a gain
 *                                        shown without this may not apply to any audio
 */

/**
 * @typedef {Object} MixerBus
 * @property {string} name
 * @property {boolean} muted
 * @property {number} channelCount
 * @property {[number,number]} level
 * @property {[number,number]} peakHold
 * @property {boolean} metered
 * @property {number} fader          units unconfirmed, treated as dB
 * @property {boolean} faderPresent  false for mon1..mon4 in the live frame; a fader
 *                                   of 0 must not be drawn when this is false
 */

/**
 * @typedef {Object} MixerSnapshot
 * @property {MixerStrip[]} strips
 * @property {MixerBus[]} buses
 * @property {string} takenAt  frame timestamp (RFC 3339), NOT the local clock;
 *                             use it to show the view as stale rather than wrong
 */

/**
 * @typedef {Object} MixerDiff
 * One difference from the golden state, mirroring mixer.Diff. Values arrive
 * pre-rendered as strings; the drawer displays them and does not recompute.
 * @property {'strip'|'bus'} kind
 * @property {string} target    wire name of the strip or bus
 * @property {string} label     human name; already resolved, safe to show
 * @property {string} field     e.g. 'outputs', 'muted', 'fader'
 * @property {string} golden    rendered value from the saved golden state
 * @property {string} current   rendered value now
 * @property {'info'|'warn'|'critical'} severity  a strip gaining 'aux1' is critical:
 *                             that is commentary entering the clean feed
 */

/**
 * @typedef {Object} MixerCommand
 * A write, mirroring the mixer.Command implementations. Built by the drawer,
 * serialised and validated on the Go side; the drawer does not construct
 * WebSocket envelopes itself.
 * @property {'setRouting'|'setInputMuted'|'setOutputMuted'|'setChFader'|'setCompLimit'} kind
 * @property {Object} args  shape depends on kind; see internal/mixer/mixer.go
 */

/**
 * @typedef {Object} MixerDrawerOptions
 * @property {HTMLElement} mount
 *   The element the drawer renders into. The drawer owns its contents entirely
 *   and must restore it on destroy(). It must not reach outside this element:
 *   the surrounding DOM belongs to files another workflow owns.
 *
 * @property {() => Promise<MixerSnapshot>} getSnapshot
 *   Fetches current mixer state. Read-only and safe to call at any time,
 *   armed or not. Used for the drawer's own refresh on open; steady-state
 *   updates arrive through update() instead, so the drawer must not poll.
 *
 * @property {(cmds: MixerCommand[]) => Promise<void>} sendCommands
 *   THE WRITE PATH TO A LIVE MIXER. Preconditions the drawer must satisfy:
 *     - only while armed (see setArmed)
 *     - only from an explicit operator gesture, never from a lifecycle hook,
 *       a timer, or the arrival of a diff
 *     - never with an empty array
 *   Resolving means the commands were SENT. It does not mean they took
 *   effect: at least one command in this vocabulary is silently inert when
 *   misused, and its state reads back exactly as sent. The drawer must
 *   confirm from the following update() snapshot and must not report success
 *   to the operator on the promise alone.
 *
 * @property {() => Promise<MixerSnapshot|null>} getGolden
 *   Loads the saved golden snapshot, or null when the operator has never
 *   saved one. Null is a normal state and must render as "no golden saved",
 *   not as "no differences" — the latter is a claim the drawer cannot make.
 *
 * @property {(snapshot: MixerSnapshot) => Promise<void>} setGolden
 *   Saves the current state as golden. This is an operator gesture, never
 *   automatic: silently re-baselining would erase the very difference the
 *   drawer exists to show. It writes application state only — it does NOT
 *   touch the mixer.
 *
 * @property {(err: Error, context: string) => void} onError
 *   Reports a failure to the host UI. The drawer must not swallow errors and
 *   must not render stale state as if it were current: a mixer view that
 *   silently stops updating is worse than one that says it has stopped.
 */

/**
 * @typedef {Object} MixerDrawer
 * @property {() => void} open      Shows the drawer. Read-only; opening never writes.
 * @property {() => void} close     Hides it. Must not disarm silently — see setArmed.
 * @property {() => boolean} isOpen
 * @property {(snapshot: MixerSnapshot) => void} update
 *   Feeds live state in. Called at approximately 1 Hz from the host's existing
 *   status subscription, INCLUDING while the drawer is closed, so it must be
 *   cheap and must not touch the DOM when closed. It is a pure state update:
 *   it must never write to the mixer, however alarming the snapshot is.
 * @property {(armed: boolean) => void} setArmed
 *   Enables or disables the write path. The drawer starts DISARMED and must
 *   present no control that can reach the mixer until armed. Arming by itself
 *   changes nothing on the mixer — it only permits a later operator gesture to.
 * @property {() => void} destroy
 *   Tears down: removes listeners, restores mount, releases the drawer. Must
 *   not write to the mixer, and in particular must not "restore" routing.
 */

/**
 * createMixerDrawer builds the mixer drawer.
 *
 * Implemented by WP-M4 in this directory. This declaration is the contract the
 * host wires against; the coordinator connects it to the application shell.
 *
 * @param {MixerDrawerOptions} opts
 * @returns {MixerDrawer}
 */
export function createMixerDrawer(opts) {
  void opts;
  throw new Error('mixer: createMixerDrawer not implemented (WP-M4)');
}
