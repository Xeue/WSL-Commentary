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
 * THE DISPLAY LABELS ARE THE FACILITY'S OWN NAMES, set by the operator; the
 * `name` field keeps M2L-X's enum code (PGM, CLN, MON, MIC1..3, PFL) as the
 * technical truth. The mapping the operator chose is, in mid order: Program
 * Dirty, Program Clean, Aux, Monitor 1, Monitor 2, Monitor 3, PFL. Mids 4 to 6
 * (labelled Monitor 1..3) are still the MIX-MINUS feeds derived from the MIC
 * inputs — on an event that uses none of them they carry nothing at all.
 *
 * Note that frontend/src/monitor/buses.js carries its own mid-to-BUS map, which
 * is a measured claim about M2L-X's routing and is WP-5a's to change. Its
 * `sony` column already carries these same seven names. Nothing here should be
 * derived from it: the seam with WP-5a is createMonitor and nothing else.
 */

/**
 * DEFAULT_RETURN_MID is mid 4, MIC1 (labelled "Monitor 1"): the operator's
 * chosen default return. config.DefaultReturnMid on the Go side agrees, and so
 * does monitor/buses.js.
 */
export const DEFAULT_RETURN_MID = 4;

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
// a track by its number.
export const RETURN_BUSES = Object.freeze([
  Object.freeze({
    mid: 1,
    name: 'PGM',
    label: 'Program Dirty',
  }),
  Object.freeze({
    mid: 2,
    name: 'CLN',
    label: 'Program Clean',
  }),
  Object.freeze({
    mid: 3,
    name: 'MON',
    label: 'Aux',
  }),
  Object.freeze({
    mid: 4,
    name: 'MIC1',
    label: 'Monitor 1',
  }),
  Object.freeze({
    mid: 5,
    name: 'MIC2',
    label: 'Monitor 2',
  }),
  Object.freeze({
    mid: 6,
    name: 'MIC3',
    label: 'Monitor 3',
  }),
  Object.freeze({
    mid: 7,
    name: 'PFL',
    label: 'PFL',
  }),
]);

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
