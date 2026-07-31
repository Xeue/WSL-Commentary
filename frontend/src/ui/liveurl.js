/**
 * Parsing the M2L-X live-operation URL into a host and an event ID.
 *
 * Owner: WP-5b. Pure — no DOM, no network.
 *
 * ======================== WHY THIS IS THE PRIMARY INPUT =====================
 * The Settings screen used to ask for the M2L-X host and the event ID as two
 * separate fields. The host is easy. The event ID — "dl9-5p5ah0bd-empd" — is
 * not: it is a string with no meaning, and there is NO endpoint that lists it.
 * /api/event/list, /api/events, /api/live_operation*, /api/user/me were all
 * checked; there is no event-list endpoint and /api/user/me returns identity
 * only. So the app cannot derive it and the operator cannot remember it.
 *
 * What the operator DOES have is the address bar of the M2L-X GUI they are
 * already looking at:
 *
 *   https://m2lx-wslstudios-matcht.etapsiota.com/live-operation/dl9-5p5ah0bd-empd
 *
 * Both fields are in there. One paste replaces two pieces of typing, and the
 * two individual fields stay visible and editable underneath so nothing becomes
 * un-fixable if the URL shape ever changes.
 * ===========================================================================
 *
 * What is accepted:
 *   - with or without a scheme          (https://host/... or host/...)
 *   - with or without a trailing slash
 *   - with a query string or a fragment (both ignored)
 *   - any path segment before /live-operation/ (a proxy prefix, say)
 *   - extra path segments after the id  (.../dl9-.../audio)
 *
 * What is rejected, with a reason rather than a shrug: anything with no
 * /live-operation/<id> segment, an empty id, or no host.
 */

/** The path segment that marks the event ID as the one that follows it. */
export const LIVE_OPERATION_SEGMENT = 'live-operation';

/**
 * parseLiveOperationURL splits an M2L-X GUI URL into a host and an event ID.
 *
 * @param {unknown} input
 * @returns {{ok: true, host: string, eventId: string}|{ok: false, error: string}}
 */
export function parseLiveOperationURL(input) {
  if (typeof input !== 'string' || input.trim() === '') {
    return { ok: false, error: 'Paste the address of the M2L-X live-operation page.' };
  }

  let rest = input.trim();

  // Strip the scheme if there is one. A scheme this app cannot speak is worth
  // naming: pasting an ftp:// or file:// address means the wrong thing was
  // copied, and "no /live-operation/ segment" would be a confusing answer to it.
  const schemeMatch = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//.exec(rest);
  if (schemeMatch) {
    const scheme = schemeMatch[1].toLowerCase();
    if (scheme !== 'http' && scheme !== 'https') {
      return { ok: false, error: `"${schemeMatch[1]}://" is not an M2L-X address — paste the https:// URL from the browser.` };
    }
    rest = rest.slice(schemeMatch[0].length);
  }

  // Drop the fragment and the query: neither carries the event.
  rest = rest.split('#')[0].split('?')[0];

  // Credentials in the authority (user:pass@host) are not something M2L-X uses,
  // but a pasted URL can carry them and they are not part of the host.
  const slash = rest.indexOf('/');
  let authority = slash >= 0 ? rest.slice(0, slash) : rest;
  const path = slash >= 0 ? rest.slice(slash + 1) : '';
  const at = authority.lastIndexOf('@');
  if (at >= 0) authority = authority.slice(at + 1);

  const host = authority.trim();
  if (host === '') {
    return { ok: false, error: 'That address has no host in it.' };
  }

  const segments = path.split('/').filter((s) => s !== '');
  const idx = segments.findIndex((s) => s.toLowerCase() === LIVE_OPERATION_SEGMENT);
  if (idx < 0) {
    return {
      ok: false,
      error: `That is not a live-operation address: it has no "/${LIVE_OPERATION_SEGMENT}/<event id>" in it.`,
    };
  }

  const eventId = decodeSegment(segments[idx + 1]);
  if (!eventId) {
    return {
      ok: false,
      error: `That address stops at "/${LIVE_OPERATION_SEGMENT}/" with no event ID after it.`,
    };
  }

  return { ok: true, host, eventId };
}

/**
 * decodeSegment percent-decodes a path segment, tolerating a malformed escape
 * rather than throwing: an event ID that will not decode is still more useful
 * to the operator than an exception, and they can edit the field underneath.
 *
 * @param {string|undefined} s
 * @returns {string}
 */
function decodeSegment(s) {
  if (typeof s !== 'string') return '';
  try {
    return decodeURIComponent(s).trim();
  } catch {
    return s.trim();
  }
}

/**
 * bareHost reduces a host that may carry a scheme, a port or a path to the
 * bare host — the JS mirror of internal/config's hostOnly, which is what
 * actually decides the SRT target. It is here so the Settings screen can show
 * the operator the same string the Go side will dial, rather than a guess at
 * it.
 *
 * If the two ever disagree the Settings screen is lying, so keep them in step:
 * internal/config/config.go, hostOnly, and its table test.
 *
 * @param {unknown} host
 * @returns {string}
 */
export function bareHost(host) {
  if (typeof host !== 'string') return '';
  let h = host.trim();
  if (h === '') return '';
  const scheme = h.indexOf('://');
  if (scheme >= 0) h = h.slice(scheme + 3);
  const slash = h.indexOf('/');
  if (slash >= 0) h = h.slice(0, slash);
  if (h.startsWith('[')) {
    const end = h.indexOf(']');
    return end >= 0 ? h.slice(0, end + 1) : h;
  }
  const colon = h.lastIndexOf(':');
  return colon >= 0 ? h.slice(0, colon) : h;
}

/**
 * formatLiveOperationURL rebuilds the canonical URL from a host and event ID,
 * so the Settings screen can show the operator what its two fields amount to
 * without them having to reassemble it in their head.
 *
 * @param {string} host
 * @param {string} eventId
 * @returns {string} '' if either part is missing
 */
export function formatLiveOperationURL(host, eventId) {
  const h = typeof host === 'string' ? host.trim().replace(/^https?:\/\//i, '').replace(/\/+$/, '') : '';
  const e = typeof eventId === 'string' ? eventId.trim() : '';
  if (!h || !e) return '';
  return `https://${h}/${LIVE_OPERATION_SEGMENT}/${e}`;
}
