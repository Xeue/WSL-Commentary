/**
 * What deserves the operator's attention mid-match, and what does not.
 *
 * Owner: WP-5b.
 *
 * The side column fixed the LAYOUT half of the alert problem: nothing that
 * appears there can move the picture, because the column is permanent and its
 * width is a constant. It did not fix the other half, and the other half is the
 * one the operator actually complained about:
 *
 *   "the orrange status banner at the bottom about switcher status is VERY
 *    annoying and keeps cauing layout shifts and causing concerns when
 *    everything is fine"
 *
 * An alert that fires when everything is fine trains an operator to ignore the
 * surface it appears on. That cost is not paid by the alert that cried wolf — it
 * is paid by the next one, which is real, and which is now on a surface nobody
 * reads. So this module is the decision about which is which, written down and
 * tested, rather than a habit spread across call sites.
 *
 * Nothing here touches the DOM.
 *
 * ===================== THE TWO SEVERITIES, AND WHY TWO ======================
 *
 * ALERT   Something is wrong and this desk may be able to do something about it.
 *         It is coloured, it is counted, and it stays until it is dismissed or
 *         it resolves.
 * NOTE    Something happened that explains what the operator can see. It is not
 *         a fault. Grey, uncounted, and it does not raise the column's
 *         attention marker.
 *
 * There is no third level, deliberately. Three levels means an argument about
 * which of two things is "warning" every time one is added, and the operator
 * only ever asks one question of this column: is there anything for me to do.
 *
 * ================= WHAT IS NOT AN ALERT, AND WHY EACH IS NOT ================
 *
 * 1. STATUS UNAVAILABLE — the switcher status feed being silent.
 *
 *    THIS IS THE ONE THE OPERATOR NAMED. It was a full-width orange banner in
 *    document flow that appeared and disappeared as the M2L-X telemetry
 *    WebSocket came and went, shoving the picture on every transition.
 *
 *    What it means, exactly: the status WebSocket has been quiet for 15 s
 *    (m2lx.StaleAfter), or the configured statusKey names nothing in the frames
 *    that ARE arriving. Both mean this application cannot presently SEE the
 *    switcher. Neither means anything has happened to the feed: the contribution
 *    SRT socket is a different connection to a different port, and it is the
 *    SENDING lamp that reports it. A commentator whose telemetry socket blinks
 *    is a commentator whose audio is going out exactly as before.
 *
 *    And it is already on the screen, three times over, in the surface built for
 *    it: deriveStatusLamps greys SWITCHER SEES FEED, VIDEO and AUDIO and writes
 *    STATUS UNAVAILABLE across all three — glyph, text and colour, the lamp
 *    row's own three-way encoding. The overall indicator folds the same fact in
 *    (overall.js, case 3: a grey that is not settled reads WORKING, never GOOD,
 *    because "never show stale green" is spec section 8).
 *
 *    So the banner was a FOURTH copy of a fact already stated three times, in
 *    the one form that could move the picture. It is withdrawn: staleness raises
 *    nothing at all. STALE_STATUS_SEVERITY records that as a decision rather
 *    than as an omission, and homelayout.test.js asserts the element is gone.
 *
 * 2. The deferred output-device switch (monitor/audio.js).
 *
 *    "the browser will not change the audio output device until someone
 *    interacts with the window" — TRUE, and a real browser constraint: WKWebView
 *    refuses setSinkId without transient activation. But it is an explanation of
 *    a thing that is about to fix itself. audio.js arms a one-shot listener on
 *    the next click ANYWHERE and re-applies, so in the ordinary case the operator
 *    would read an alarm about a state that ended before they finished reading
 *    it. audio.js no longer reports the first deferral at all; if the retry is
 *    ALSO refused, that is a device that will never be permitted and it is a
 *    real SINK_ID_FAILED alert.
 *
 *    The code is still classified here, as a NOTE, because a module may report
 *    what it likes and this file is where loudness is decided — a future caller
 *    that surfaces it must not have to guess.
 *
 * 3. The return monitor's ordinary "there was nothing to stop".
 *    Already handled by backend.js's isReturnNotRunningError; named here so the
 *    list of things that are not faults is in one place.
 */

/** SEVERITY is the whole vocabulary. See the header for why there are two. */
export const SEVERITY = Object.freeze({
  ALERT: 'alert',
  NOTE: 'note',
});

/**
 * STALE_STATUS_SEVERITY is null, and null means "raises nothing".
 *
 * It is exported so that the decision is a value another module can read and a
 * test can assert, rather than the absence of a call site that a well-meaning
 * change could reinstate without noticing what it was. See point 1 in the
 * header for why staleness is not an alert.
 */
export const STALE_STATUS_SEVERITY = null;

/**
 * NOTE_MONITOR_CODES are the MonitorErrorCodes that explain rather than warn.
 *
 * SINK_ID_DEFERRED is the operator's screenshotted message. AUTOPLAY_BLOCKED is
 * its sibling and is deliberately NOT here: blocked autoplay means the
 * commentator currently HEARS NOTHING, which is a fault at this desk even though
 * the remedy is one click.
 */
export const NOTE_MONITOR_CODES = Object.freeze(['SINK_ID_DEFERRED']);

/**
 * classifyMonitorError gives a monitor error its severity.
 *
 * @param {{code?: string}|null|undefined} err
 * @returns {'alert'|'note'}
 */
export function classifyMonitorError(err) {
  const code = err && typeof err === 'object' ? err.code : err;
  return NOTE_MONITOR_CODES.includes(code) ? SEVERITY.NOTE : SEVERITY.ALERT;
}

/**
 * normaliseSeverity coerces anything to a valid severity, defaulting to ALERT.
 *
 * The default is the LOUD one on purpose: a caller that forgets to classify
 * should over-report, never under-report. Being told about something harmless is
 * a nuisance; not being told about something real is the failure this whole
 * surface exists to prevent.
 */
export function normaliseSeverity(severity) {
  return severity === SEVERITY.NOTE ? SEVERITY.NOTE : SEVERITY.ALERT;
}

/**
 * describeAttention summarises a list of entries for the column's header and for
 * the collapsed strip — the one place an alert has to be visible when the
 * operator has folded the column away.
 *
 * NOTES ARE NOT COUNTED. The count answers "is there anything for me to do", and
 * a count that includes explanations answers a different question badly.
 *
 * @param {Array<{severity?: string}>} entries
 * @returns {{alerts: number, notes: number, attention: boolean, label: string}}
 */
export function describeAttention(entries) {
  const list = Array.isArray(entries) ? entries : [];
  let alerts = 0;
  let notes = 0;
  for (const e of list) {
    if (normaliseSeverity(e && e.severity) === SEVERITY.NOTE) notes += 1;
    else alerts += 1;
  }
  return {
    alerts,
    notes,
    attention: alerts > 0,
    label: alerts === 0 ? 'No alerts' : alerts === 1 ? '1 alert' : `${alerts} alerts`,
  };
}
