/**
 * levelmap.js — the return-level slider's taper.
 *
 * The slider used to be LINEAR: 0..100 straight into the 0..1 multiplier on
 * the return's make-up gain. With +18 dB of make-up gain that put a
 * comfortable listening level at 5–10 % of the slider's travel ("we have to
 * turn it down almost all the way to be able to hear it") and wasted the other
 * ninety per cent. Ears work in decibels, so the slider does too:
 *
 *   100  →  0 dB   the full make-up gain, exactly what 100 always was
 *    75  → -10 dB  the default on a machine that has never been set
 *    50  → -20 dB
 *     1  → -39.6 dB
 *     0  →  silence
 *
 * 0.4 dB per step is finer than anyone can hear, and 40 dB of range reaches
 * from "far too loud" to "barely there" without a dead zone at either end.
 *
 * What crosses to the monitor is unchanged: a 0..1 LINEAR multiplier
 * (monitor/gain.js computeGain). Only the slider's reading of it moved.
 */

/** Decibels between the top of the slider and the last position above mute. */
export const LEVEL_RANGE_DB = 40;

/** The slider's top position. */
export const LEVEL_POSITION_MAX = 100;

/**
 * Where a machine that has never had its level set starts: -10 dB. Audible
 * on the first run and not a shock; one drag and it is remembered.
 */
export const DEFAULT_LEVEL_POSITION = 75;

function clampPosition(position) {
  const n = Number(position);
  if (!Number.isFinite(n)) return 0;
  return Math.min(Math.max(Math.round(n), 0), LEVEL_POSITION_MAX);
}

/** positionToDb is the slider position's gain in dB; -Infinity at 0. */
export function positionToDb(position) {
  const pos = clampPosition(position);
  if (pos <= 0) return -Infinity;
  return ((pos - LEVEL_POSITION_MAX) / LEVEL_POSITION_MAX) * LEVEL_RANGE_DB + 0; // + 0: never -0
}

/**
 * sliderToLevel turns a slider position (0..100) into the 0..1 linear level
 * the monitor multiplies its make-up gain by.
 * @param {unknown} position
 * @returns {number}
 */
export function sliderToLevel(position) {
  const db = positionToDb(position);
  if (db === -Infinity) return 0;
  return Math.pow(10, db / 20);
}

/**
 * levelToSlider is the inverse: a 0..1 linear level to the nearest slider
 * position. A level below the slider's range (under -40 dB) reads as 0.
 * @param {unknown} level
 * @returns {number}
 */
export function levelToSlider(level) {
  const n = Number(level);
  if (!Number.isFinite(n) || n <= 0) return 0;
  const db = Math.max(-LEVEL_RANGE_DB, Math.min(0, 20 * Math.log10(n)));
  const pos = Math.round(LEVEL_POSITION_MAX + (db / LEVEL_RANGE_DB) * LEVEL_POSITION_MAX);
  return Math.min(Math.max(pos, 0), LEVEL_POSITION_MAX);
}

/**
 * describeLevelPosition is the readout beside the slider: "0 dB", "-12.4 dB",
 * "Mute".
 * @param {unknown} position
 * @returns {string}
 */
export function describeLevelPosition(position) {
  const db = positionToDb(position);
  if (db === -Infinity) return 'Mute';
  if (db > -0.05) return '0 dB';
  return `${db.toFixed(1)} dB`;
}
