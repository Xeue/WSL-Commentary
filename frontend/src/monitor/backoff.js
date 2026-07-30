/**
 * The restart delay ladder for the monitor.
 *
 * Owner: WP-5a. Pure.
 *
 * spec §7 is explicit about recovery: "If the peer connection fails or the
 * signalling socket closes, tear the whole thing down, re-fetch credentials in
 * Go and redo the chain. No refresh scheduler." This module is not a refresh
 * scheduler — nothing here runs while the monitor is healthy. It is only the
 * delay between one torn-down attempt and the next, and it exists because a
 * zero-delay redo of the chain is a hot loop over four network calls:
 * GetKVSCredentials (which is an M2L-X REST call plus a Cognito exchange),
 * DescribeSignalingChannel, GetSignalingChannelEndpoint and GetIceServerConfig.
 * On a facility network that has just gone down, that loop would generate
 * hundreds of requests per minute and get us throttled by AWS at exactly the
 * moment we need to reconnect.
 *
 * The ladder is deliberately much faster than the SRT ladder in spec §6.2
 * (7, 7, 10, 15, 20, 30 s). The SRT ladder is slow because M2L-X's listener
 * accepts exactly one peer and refuses re-accept for ~5 s, so there is a race to
 * lose. There is no such race here: a KVS signalling channel serves up to ten
 * viewers, so reconnecting quickly cannot displace anything. And the monitor is
 * the picture and sound the commentator is working to — every second of it dark
 * is a second they are working blind.
 */

/**
 * RESTART_LADDER_MS are the delays before attempts 1, 2, 3 and 4 after a
 * failure. The first is 1 s so that a transient blip recovers before the
 * commentator has finished noticing it.
 */
export const RESTART_LADDER_MS = Object.freeze([1_000, 2_000, 5_000, 10_000]);

/** RESTART_CAP_MS is the delay from the fifth consecutive failure onwards. */
export const RESTART_CAP_MS = 15_000;

/**
 * JITTER_FRACTION spreads restarts by up to ±15%. It matters for exactly one
 * scenario, but it is a scenario this product has: several commentary positions
 * at the same facility, all pointed at the same KVS channel, all losing the
 * uplink at the same instant. Without jitter they would retry in lockstep
 * forever.
 */
export const JITTER_FRACTION = 0.15;

/**
 * restartDelayMs returns the delay before the given consecutive-failure attempt.
 *
 * @param {number} attempt 0 for the first restart after a healthy session
 * @param {() => number} [random] injectable for tests; defaults to Math.random
 * @returns {number} milliseconds, always >= 0
 */
export function restartDelayMs(attempt, random = Math.random) {
  const i = Number.isInteger(attempt) && attempt >= 0 ? attempt : 0;
  const base = i < RESTART_LADDER_MS.length ? RESTART_LADDER_MS[i] : RESTART_CAP_MS;
  const r = typeof random === 'function' ? random() : 0.5;
  const jitter = Number.isFinite(r) ? (r * 2 - 1) * JITTER_FRACTION : 0;
  return Math.max(0, Math.round(base * (1 + jitter)));
}

/**
 * baseDelayMs is restartDelayMs without the jitter — the number to quote in a
 * log line or a test assertion.
 *
 * @param {number} attempt
 * @returns {number}
 */
export function baseDelayMs(attempt) {
  const i = Number.isInteger(attempt) && attempt >= 0 ? attempt : 0;
  return i < RESTART_LADDER_MS.length ? RESTART_LADDER_MS[i] : RESTART_CAP_MS;
}
