/**
 * The Return dropdown's seven audio tracks, and the words shown beside them.
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THIS FILE EXISTS ===============================
 *
 * This table used to be written out twice — once in home.js and once in
 * settings.js — with a comment in each saying it mirrored the other. Both
 * copies were wrong in the same way, and being wrong twice is how it survived:
 * the two agreed with each other, so nothing looked out of step.
 *
 * There is now ONE table and both screens import it. A future correction has
 * one place to land.
 *
 * ======================= WHAT THE SEVEN TRACKS ARE ==========================
 *
 * From the M2L-X bundle's OWN ENUM for the monitor's seven audio tracks:
 *
 *   mid 1  PGM   the master bus - programme
 *   mid 2  CLN   the clean feed - aux1
 *   mid 3  MON
 *   mid 4  MIC1  mix-minus (N-1)
 *   mid 5  MIC2  mix-minus (N-1)
 *   mid 6  MIC3  mix-minus (N-1)
 *   mid 7  PFL   pre-fade listen
 *
 * The labels this file previously carried for mids 3 to 7 - "aux2", "mon1",
 * "mon2", "mon3", "mon4 (PFL)" - were the MIXER's bus names, which are a
 * different vocabulary from the monitor's track list and do not line up with it
 * track for track. An operator hunting for a clean return was being offered
 * five options named after something else.
 *
 * MIDS 4 TO 6 ARE MIX-MINUS FEEDS DERIVED FROM THE MIC INPUTS, and that is
 * stated in the UI rather than left as folklore: on an event that uses none of
 * the MIC inputs they carry nothing at all. A commentator who switches to one
 * and hears silence needs to know the feed does not exist, because the
 * alternative reading - "my headphones have died, thirty seconds before
 * kick-off" - is the one they will otherwise reach.
 *
 * Note that frontend/src/monitor/buses.js carries its own mid-to-BUS map, which
 * is a measured claim about M2L-X's routing and is WP-5a's to change. Its
 * `sony` column already carries these same seven names. Nothing here should be
 * derived from it: the seam with WP-5a is createMonitor and nothing else.
 */

/**
 * DEFAULT_RETURN_MID is mid 2, CLN. Unchanged: it is the intended return, and
 * config.DefaultReturnMid on the Go side agrees.
 */
export const DEFAULT_RETURN_MID = 2;

/**
 * RETURN_BUSES is every audio track the monitor subscribes to, in mid order.
 *
 * `name` is the enum name on its own, for anywhere that has no room for a
 * sentence. `label` is the dropdown option text.
 *
 * @type {ReadonlyArray<{mid: number, name: string, label: string}>}
 */
// The labels are deliberately short: they name the track and nothing else.
//
// The mid number is NOT in them, so nothing else in the interface may refer to
// a track by its number. RETURN_HINT below names MIC1/MIC2/MIC3 rather than
// "mids 4-6" for exactly that reason — a hint pointing at numbers the operator
// cannot see anywhere is worse than no hint. The mid remains what
// config.returnMid stores and what the monitor subscribes to; it is simply not
// the operator's vocabulary.
export const RETURN_BUSES = Object.freeze([
  Object.freeze({
    mid: 1,
    name: 'PGM',
    label: 'PGM (master)',
  }),
  Object.freeze({
    mid: 2,
    name: 'CLN',
    label: 'CLN (aux1)',
  }),
  Object.freeze({
    mid: 3,
    name: 'MON',
    label: 'MON (monitor)',
  }),
  Object.freeze({
    mid: 4,
    name: 'MIC1',
    label: 'MIC1 (mix-minus)',
  }),
  Object.freeze({
    mid: 5,
    name: 'MIC2',
    label: 'MIC2 (mix-minus)',
  }),
  Object.freeze({
    mid: 6,
    name: 'MIC3',
    label: 'MIC3 (mix-minus)',
  }),
  Object.freeze({
    mid: 7,
    name: 'PFL',
    label: 'PFL (pre-fade listen)',
  }),
]);

/**
 * RETURN_HINT is the sentence shown under the dropdown on both screens.
 *
 * The first half is the original note and is deliberately kept: the app is
 * reporting a routing fact rather than hiding one, because nothing here can
 * change what the gallery sends to a bus.
 *
 * The second half is the mix-minus warning. Silence on mid 4, 5 or 6 is a
 * normal, correct reading of an event with no MIC inputs, and it looks exactly
 * like a failure.
 */
export const RETURN_HINT =
  'If you can hear yourself, the gallery is routing commentary to that bus. Try another — the ' +
  'switch is immediate and does not interrupt your feed. MIC1, MIC2 and MIC3 are mix-minus (N-1) feeds ' +
  'derived from the MIC inputs, so on an event that uses none of them they are silent: hearing ' +
  'nothing there means that feed does not exist, not that your headphones have failed.';

/**
 * isValidReturnMid reports whether mid names one of the seven audio tracks.
 * Mirrors config.ReturnMid's valid range, 1..7, which is unchanged.
 *
 * @param {unknown} mid
 * @returns {boolean}
 */
export function isValidReturnMid(mid) {
  return RETURN_BUSES.some((b) => b.mid === Number(mid));
}

/**
 * returnLabel returns the dropdown text for a mid, or a marker for one outside
 * the plan. It never returns the empty string: a blank option reads as "no
 * return" to somebody about to commentate.
 *
 * @param {number} mid
 * @returns {string}
 */
export function returnLabel(mid) {
  const found = RETURN_BUSES.find((b) => b.mid === Number(mid));
  return found ? found.label : `mid ${String(mid)} — (not one of the seven audio tracks)`;
}
