/**
 * The return-audio gain law.
 *
 * Owner: WP-5a. Pure — no Web Audio, no DOM.
 *
 * The commentator's headphone level is the product of two things:
 *
 *   1. a fixed make-up gain in dB, `returnGainDb`, which exists to undo a
 *      measured deficit in the monitor path, and
 *   2. the 0..1 level slider on the front panel.
 *
 * DEFAULT_GAIN_DB is 18 because the monitor track arrives about 18 dB below
 * programme level: docs/test-results.md line 121 — "the monitor track level
 * equals the M2L-X bus meter (sine peak basis) to within 0.1 dB, and both sit
 * ~18 dB below the SRT-ingested peak level. Repeatable at two injection levels.
 * Cause not established." Without the make-up gain the return is far too quiet
 * to commentate over. With it, unity on the slider puts the return back at
 * roughly programme level.
 *
 * The cause of the 18 dB is not established, so this is a compensation, not a
 * calibration. It lives in config.json as `returnGainDb` so it can be changed
 * without a build.
 */

/**
 * DEFAULT_GAIN_DB — the measured make-up gain. See the module comment; the
 * measurement is docs/test-results.md line 121.
 */
export const DEFAULT_GAIN_DB = 18;

/**
 * MIN_GAIN_DB / MAX_GAIN_DB bound the make-up gain.
 *
 * +40 dB is the ceiling because this is a *headphone* feed on a commentator's
 * head. 18 dB is the measured need; +40 dB is twenty-two decibels of headroom
 * over that, which is generous for an unexplained deficit and still 100x rather
 * than 10000x if someone fat-fingers the config file. -40 dB is a floor rather
 * than -Infinity so that "quiet" and "off" stay distinguishable.
 */
export const MIN_GAIN_DB = -40;
export const MAX_GAIN_DB = 40;

/**
 * dbToLinear converts decibels to a linear amplitude ratio: 10^(dB/20).
 *
 * @param {number} db
 * @returns {number}
 */
export function dbToLinear(db) {
  return Math.pow(10, db / 20);
}

/**
 * linearToDb is the inverse, for display. Zero and negatives map to
 * -Infinity rather than NaN.
 *
 * @param {number} linear
 * @returns {number}
 */
export function linearToDb(linear) {
  if (!Number.isFinite(linear) || linear <= 0) return -Infinity;
  return 20 * Math.log10(linear);
}

/**
 * clampGainDb resolves a make-up gain from config into a usable number.
 * Non-finite input falls back to DEFAULT_GAIN_DB — a missing or corrupt
 * `returnGainDb` must leave the commentator with a usable return, not silence.
 *
 * @param {unknown} db
 * @returns {number}
 */
export function clampGainDb(db) {
  const n = typeof db === 'string' && db.trim() !== '' ? Number(db) : db;
  if (typeof n !== 'number' || !Number.isFinite(n)) return DEFAULT_GAIN_DB;
  return Math.min(Math.max(n, MIN_GAIN_DB), MAX_GAIN_DB);
}

/**
 * clampLevel resolves the 0..1 slider value.
 *
 * A non-finite level resolves to 0 — silence — and not to 1. This is
 * deliberate and asymmetric with clampGainDb: a level slider that reads NaN is
 * a bug in the caller, and of the two ways to be wrong, "the commentator
 * reports no return audio" is noticed in seconds and costs nothing, while
 * "full gain into headphones on someone's head" is a hearing injury. Fail
 * towards silence on the slider, fail towards audible on the fixed gain.
 *
 * @param {unknown} level
 * @returns {number} in [0, 1]
 */
export function clampLevel(level) {
  const n = typeof level === 'string' && level.trim() !== '' ? Number(level) : level;
  if (typeof n !== 'number' || !Number.isFinite(n)) return 0;
  return Math.min(Math.max(n, 0), 1);
}

/**
 * computeGain is the whole law: 10^(gainDb/20) multiplied by the 0..1 level,
 * with the result clamped to the linear equivalent of MAX_GAIN_DB so that no
 * combination of inputs can exceed the ceiling.
 *
 * @param {unknown} gainDb make-up gain in dB
 * @param {unknown} level 0..1 slider
 * @returns {number} linear GainNode value
 */
export function computeGain(gainDb, level) {
  const g = dbToLinear(clampGainDb(gainDb)) * clampLevel(level);
  return Math.min(Math.max(g, 0), dbToLinear(MAX_GAIN_DB));
}

/**
 * GAIN_RAMP_SECONDS is the time constant used with
 * GainNode.gain.setTargetAtTime when the level changes.
 *
 * 10 ms: long enough that dragging the level slider does not produce a zipper
 * of discontinuities in the headphones, short enough that it feels immediate.
 * A direct assignment to `.value` steps the gain within one render quantum and
 * clicks audibly, which on headphones is unpleasant and on a live commentary
 * position is unacceptable.
 */
export const GAIN_RAMP_SECONDS = 0.01;
