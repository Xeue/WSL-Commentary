/**
 * The input meters' pure model: scale, zones and peak-hold, no DOM.
 *
 * Owner: WP-5b.
 *
 * These are the meters beside the big picture, fed from the SEND pipeline's
 * own level element through the "levels" event — they show what is ACTUALLY
 * being encoded and sent, which is why they exist at all: a meter fed from the
 * browser's idea of the microphone keeps moving while the wrong device is
 * selected.
 *
 * ==================== THE SCALE IS THE MIXER'S SCALE ========================
 *
 * Every number here is IMPORTED from ./mixer/model.js, not restated: the same
 * -60..0 dBFS scale linear in dB, the same -18/-6 stage boundaries, the same
 * green/amber/red. Two meters in one application that disagree about where
 * amber starts is the two-tables bug this repository documents repeatedly
 * (see ./returns.js and monitor/channels.js for the times it was written and
 * paid for) — an operator comparing the input meter against the mixer drawer
 * must be comparing levels, not calibrations. meters.test.js asserts the
 * equalities against the mixer source, so a future decoupling fails a test
 * instead of shipping.
 *
 * No DOM and no browser API, so `node --test` drives it with nothing
 * installed. home.js is the wiring; this file is where the behaviour is
 * proved (meters.test.js).
 */

import {
  METER_FLOOR_DB,
  METER_CEIL_DB,
  METER_AMBER_DB,
  METER_RED_DB,
  METER_STAGES,
  meterFraction,
} from './mixer/model.js';

/**
 * The scale and stage boundaries, re-exported under the input meters' own
 * names so home.js does not import mixer internals directly — but they ARE the
 * mixer's values, by import, not by copy. See the header.
 */
export const LEVELS_FLOOR_DB = METER_FLOOR_DB;
export const LEVELS_CEIL_DB = METER_CEIL_DB;
export const LEVELS_AMBER_DB = METER_AMBER_DB;
export const LEVELS_RED_DB = METER_RED_DB;

/**
 * LEVELS_SILENCE_DB is the value the Go side clamps silence to and the value
 * the session-end zero-frame carries: -100 dBFS, matching the mixer's own
 * digital-silence reading. Anything at or below it is "no signal", which is
 * how isSilentFrame below decides the meters should dim.
 */
export const LEVELS_SILENCE_DB = -100;

/**
 * dbToFraction maps a dBFS reading onto 0..1 of the drawn meter: -60 and below
 * is 0, 0 and above is 1, linear in dB between — the mixer's meterFraction,
 * re-exported so the two meters cannot use different curves. Non-numeric input
 * clamps to 0 rather than escaping the bar.
 *
 * @param {unknown} db dBFS
 * @returns {number} 0..1
 */
export function dbToFraction(db) {
  return meterFraction(db);
}

/**
 * meterZones is the drawn bar's three coloured zones as fractions of its
 * length, bottom first: [{zone, from, to}] with from/to in 0..1. Derived from
 * the mixer's METER_STAGES so the paint cannot drift from the dB thresholds —
 * on the shared -60..0 scale that is green to 0.7, amber to 0.9, red to 1.
 *
 * @returns {Array<{zone: 'green'|'amber'|'red', from: number, to: number}>}
 */
export function meterZones() {
  return METER_STAGES.map((s) => ({
    zone: s.zone,
    from: meterFraction(s.fromDb),
    to: meterFraction(s.toDb),
  }));
}

/**
 * zoneFills is how full each zone is for one reading, as fractions 0..1 OF
 * THAT ZONE, bottom first — the vertical twin of the mixer's meterStageFills:
 * zones below the reading are 1, the zone containing it is partial, zones
 * above are 0. This is what makes a loud signal read green-then-amber-then-red
 * along its length, the way a real meter is read at a glance, instead of one
 * colour for the whole bar.
 *
 * @param {unknown} db dBFS
 * @returns {number[]} three fractions, 0..1, in meterZones() order
 */
export function zoneFills(db) {
  const f = dbToFraction(db);
  return meterZones().map(({ from, to }) => {
    const span = to - from;
    if (span <= 0) return 0;
    return Math.max(0, Math.min(1, (f - from) / span));
  });
}

/**
 * isSilentFrame reports whether one levels frame is all-silence — every peak
 * at or below the clamp floor — which is what the session-end zero-frame looks
 * like and what the meters dim on. An empty or missing frame counts as silent:
 * no data must never draw as a level.
 *
 * @param {{peak?: unknown} | null | undefined} frame a "levels" event payload
 * @returns {boolean}
 */
export function isSilentFrame(frame) {
  const peak = frame?.peak;
  if (!Array.isArray(peak) || peak.length === 0) return true;
  return peak.every((db) => typeof db !== 'number' || db <= LEVELS_SILENCE_DB + 0.5);
}

/**
 * PEAK_HOLD_MS is how long a peak-hold marker sits at the highest level seen
 * before it starts to fall. ~1.5 s is the broadcast convention: long enough to
 * read after a glance away, short enough that it is this passage and not the
 * pre-match line-up tone.
 */
export const PEAK_HOLD_MS = 1500;

/**
 * PEAK_DECAY_DB_PER_S is how fast the marker falls once the hold expires.
 * 26 dB/s clears the whole usable scale in a little over two seconds —
 * visibly a decay rather than a jump, without haunting the meter with a
 * ten-second-old peak.
 */
export const PEAK_DECAY_DB_PER_S = 26;

/**
 * createPeakHold builds a per-channel peak-hold: feed it each frame's peaks
 * and it returns the marker level per channel — the highest recent peak, held
 * for PEAK_HOLD_MS, then decaying at PEAK_DECAY_DB_PER_S until the live level
 * overtakes it.
 *
 * The clock is injected so the hold and decay are testable without real time:
 * `now` returns milliseconds (Date.now shape). State is per-instance; a new
 * session should get a new instance or call reset(), or the first frame of the
 * new session competes with the ghost of the old one.
 *
 * @param {() => number} [now] millisecond clock, Date.now by default
 * @returns {{update(peaks: number[]): number[], reset(): void}}
 */
export function createPeakHold(now = () => Date.now()) {
  /** @type {Array<{db: number, at: number} | undefined>} per-channel marker */
  let held = [];

  return {
    /**
     * update ingests one frame's per-channel peaks (dBFS) and returns the
     * marker level for each channel, clamped to the silence floor. Channels
     * appearing or vanishing mid-session are tolerated: state is per-index.
     *
     * @param {number[]} peaks
     * @returns {number[]}
     */
    update(peaks) {
      const t = now();
      if (!Array.isArray(peaks)) return [];
      return peaks.map((raw, i) => {
        const db = typeof raw === 'number' ? Math.max(LEVELS_SILENCE_DB, raw) : LEVELS_SILENCE_DB;
        const h = held[i];
        if (!h || db >= h.db) {
          // A new maximum (or the first frame): the marker jumps up
          // immediately. Meters rise instantly and fall slowly, never the
          // reverse — a slow rise would understate an overload.
          held[i] = { db, at: t };
          return db;
        }
        const sinceHold = t - h.at - PEAK_HOLD_MS;
        if (sinceHold <= 0) return h.db; // still holding
        const decayed = h.db - (PEAK_DECAY_DB_PER_S * sinceHold) / 1000;
        if (decayed <= db) {
          // Decayed down to the live level: track it from here.
          held[i] = { db, at: t };
          return db;
        }
        return Math.max(LEVELS_SILENCE_DB, decayed);
      });
    },

    /** reset forgets every marker — for a new session's first frame. */
    reset() {
      held = [];
    },
  };
}
