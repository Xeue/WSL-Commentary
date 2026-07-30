/**
 * The transceiver plan and the mid-to-bus map.
 *
 * Owner: WP-5a.
 *
 * ============================ THIS IS CONFIGURATION ============================
 * The mid-to-bus map below is **M2L-X configuration measured on the dev event**,
 * not a constant of the protocol and not something the app can detect. It is
 * reproduced here because the transceiver *plan* (how many, of which kind, in
 * which order) has to be compiled in — but the map from a mid to a bus is
 * exactly the kind of thing Sony can change. spec §9 puts `returnMid` in
 * config.json for that reason: "If Sony changes the multiviewer layout, that is
 * two edited numbers, not a code change and not a detection subsystem."
 *
 * If a live instance disagrees with this table, the fix is `returnMid` in
 * config.json, and a report — not an edit to the offer order.
 * ===============================================================================
 *
 * Measurement: docs/test-results.md §2.1, "Measured track map". A tone was
 * injected on one bus at a time, read back off each track, then the routing was
 * swapped as a control; the tones swapped tracks exactly, proving the mapping
 * follows the BUS, not the strip. Isolation was 94–95 dB.
 *
 *   | mid | bus          | Sony's enum name | note
 *   |-----|--------------|------------------|------------------------------------
 *   |  0  | video mosaic | —                | 2240x1440 multiviewer, one track
 *   |  1  | master       | PGM              |
 *   |  2  | aux1         | CLN              | the return: inherently N-1 (spec §7)
 *   |  3  | aux2         | "MON"            | has egress after all, via WebRTC
 *   |  4  | mon1         | "MIC1"           | an ordinary bus, not a mic feed
 *   |  5  | mon2         | "MIC2"           |
 *   |  6  | mon3         | "MIC3"           |
 *   |  7  | mon4         | PFL              | reachable only via matrix "pfl"
 *
 * All seven audio transceivers are offered even though exactly one is routed.
 * A silent track costs 1.2 kbps (docs/test-results.md §2.2) and all seven run at
 * 50.0 pkt/s, so subscribing to the lot is ~8.4 kbps idle. That is cheaper than
 * the risk of the answer re-mapping mids if we offer a different number of
 * m-lines from the one Sony's own GUI offers. Do not reorder or trim this plan.
 */

/** VIDEO_MID is the mid of the single video m-line: the multiviewer mosaic. */
export const VIDEO_MID = 0;

/** AUDIO_MIDS are the mids of the seven audio m-lines, in offer order. */
export const AUDIO_MIDS = Object.freeze([1, 2, 3, 4, 5, 6, 7]);

/**
 * TRANSCEIVER_PLAN is the exact order in which transceivers are added to the
 * peer connection. Index in this array == mid number, because SDP m-lines are
 * numbered from zero in the order the offerer emits them. That positional
 * identity is the whole basis of the mid-to-bus map.
 */
export const TRANSCEIVER_PLAN = Object.freeze([
  'video',
  'audio',
  'audio',
  'audio',
  'audio',
  'audio',
  'audio',
  'audio',
]);

/**
 * DEFAULT_RETURN_MID is mid 2 — aux1 / CLN. spec §7 and §9: with effects routed
 * to master+aux1 and commentary routed to master only, aux1 is inherently an
 * N-1, so the commentator hears the match without hearing themselves delayed by
 * the ~489 ms cloud round trip. The app cannot verify that routing convention;
 * it is in the handover note.
 */
export const DEFAULT_RETURN_MID = 2;

/**
 * BUS_MAP describes each mid. `label` is short enough for a dropdown; `detail`
 * is what you would put in a tooltip.
 *
 * @type {ReadonlyArray<{mid: number, kind: 'video'|'audio', bus: string, sony: string, label: string, detail: string}>}
 */
export const BUS_MAP = Object.freeze([
  Object.freeze({
    mid: 0,
    kind: 'video',
    bus: 'multiviewer',
    sony: '—',
    label: 'Multiviewer',
    detail: '2240x1440 mosaic; the PGM tile is cropped out of it in CSS',
  }),
  Object.freeze({
    mid: 1,
    kind: 'audio',
    bus: 'master',
    sony: 'PGM',
    label: 'PGM',
    detail: 'master bus — programme, includes commentary',
  }),
  Object.freeze({
    mid: 2,
    kind: 'audio',
    bus: 'aux1',
    sony: 'CLN',
    label: 'CLN',
    detail: 'aux1 — effects without commentary, the intended return',
  }),
  Object.freeze({
    mid: 3,
    kind: 'audio',
    bus: 'aux2',
    sony: 'MON',
    label: 'AUX2',
    detail: 'aux2 — a third independently routable bus',
  }),
  Object.freeze({
    mid: 4,
    kind: 'audio',
    bus: 'mon1',
    sony: 'MIC1',
    label: 'MON1',
    detail: 'mon1 — an ordinary bus with MIC 1 routed to it by factory default',
  }),
  Object.freeze({
    mid: 5,
    kind: 'audio',
    bus: 'mon2',
    sony: 'MIC2',
    label: 'MON2',
    detail: 'mon2 — an ordinary bus with MIC 2 routed to it by factory default',
  }),
  Object.freeze({
    mid: 6,
    kind: 'audio',
    bus: 'mon3',
    sony: 'MIC3',
    label: 'MON3',
    detail: 'mon3 — an ordinary bus with MIC 3 routed to it by factory default',
  }),
  Object.freeze({
    mid: 7,
    kind: 'audio',
    bus: 'mon4',
    sony: 'PFL',
    label: 'PFL',
    detail: 'mon4 — reachable only via the "pfl" matrix',
  }),
]);

/**
 * busForMid returns the descriptor for a mid, or null if the mid is outside the
 * eight we offer.
 *
 * @param {number} mid
 * @returns {{mid: number, kind: string, bus: string, sony: string, label: string, detail: string}|null}
 */
export function busForMid(mid) {
  if (!Number.isInteger(mid) || mid < 0 || mid >= BUS_MAP.length) return null;
  return BUS_MAP[mid];
}

/**
 * isValidReturnMid reports whether a mid names one of the seven audio buses.
 * mid 0 is the video mosaic and is not a legal return.
 *
 * @param {unknown} mid
 * @returns {boolean}
 */
export function isValidReturnMid(mid) {
  return Number.isInteger(mid) && AUDIO_MIDS.includes(mid);
}

/**
 * normaliseReturnMid coerces whatever came out of config.json into a usable
 * audio mid. A string "2" is accepted because JSON round-trips through the Wails
 * boundary and a hand-edited config.json is an expected input; anything that is
 * not one of mids 1..7 falls back rather than throwing, because a bad number in
 * a config file must not stop the commentator hearing the match.
 *
 * @param {unknown} mid
 * @param {number} [fallback] defaults to DEFAULT_RETURN_MID (2 = aux1/CLN)
 * @returns {number}
 */
export function normaliseReturnMid(mid, fallback = DEFAULT_RETURN_MID) {
  const n = typeof mid === 'string' && mid.trim() !== '' ? Number(mid) : mid;
  if (isValidReturnMid(n)) return n;
  return isValidReturnMid(fallback) ? fallback : DEFAULT_RETURN_MID;
}

/**
 * checkMidAssignment compares the mids the peer actually assigned against the
 * offer order. Returns a list of human-readable mismatches; an empty list means
 * the positional map in BUS_MAP holds for this session.
 *
 * Call it after setRemoteDescription: before the answer, `transceiver.mid` is
 * null for a freshly added transceiver, and a null mid is not a mismatch — it is
 * "not negotiated yet" — so nulls are reported separately by the caller if it
 * cares.
 *
 * @param {ReadonlyArray<{mid: string|null}>} transceivers in offer order
 * @returns {string[]} one message per transceiver whose mid is not its index
 */
export function checkMidAssignment(transceivers) {
  const problems = [];
  transceivers.forEach((t, i) => {
    const actual = t ? t.mid : undefined;
    if (actual === null || actual === undefined) {
      problems.push(`transceiver ${i} has no mid after the answer`);
      return;
    }
    if (String(actual) !== String(i)) {
      const expected = busForMid(i);
      problems.push(
        `transceiver ${i} (expected ${expected ? expected.bus : 'unknown'}) was assigned mid "${actual}"`,
      );
    }
  });
  return problems;
}
