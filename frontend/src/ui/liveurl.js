/**
 * Parsing the M2L-X address field into a host and — when the address happens to
 * carry one — an event ID, and writing the field back.
 *
 * The two directions are deliberately NOT symmetrical, and formatM2LXAddress at
 * the foot of this file records why: the app reads either form and writes only
 * the base address.
 *
 * Owner: WP-5b. Pure — no DOM, no network.
 *
 * ==================== WHAT THIS FIELD TAKES, AND WHY ========================
 * The Settings screen used to ask for the M2L-X host and the event ID as two
 * separate fields. The host is easy. The event ID — "dl9-5p5ah0bd-empd" — is
 * not: it is a string with no meaning that nobody remembers.
 *
 * There IS an endpoint that lists the events: GET /api/events/overview, called
 * by internal/m2lx/events.go and exposed as App.ListEvents. An earlier pass in
 * this very file recorded the opposite — that nothing could enumerate them —
 * and that was wrong. The probe behind it tried /api/event/list, /api/events
 * and /api/live_operation*, and /api/events is a base for POST sub-paths only
 * (create/start/stop/…), so it answered nothing; the list is one path segment
 * further down. That mistake is what made a /live-operation/<id> segment
 * COMPULSORY here, and the compulsion is what the operator hit: pasting the
 * instance's own address was refused, so they were still sent off to find a
 * full live-operation URL for an id the app can now ask for itself.
 *
 * So the address field takes EITHER form, and parseM2LXAddress is what the
 * Settings screen calls:
 *
 *   https://m2lx-wslstudios-matcht.etapsiota.com
 *       -> host only. The event id is left to the events API; ui/events.js
 *          decides what to do with the answer (one event is auto-selected,
 *          several are offered as a picker, none leaves the field alone).
 *
 *   https://m2lx-wslstudios-matcht.etapsiota.com/live-operation/dl9-5p5ah0bd-empd
 *       -> host AND event id, exactly as before.
 *
 * THE PASTE PATH IS STILL LOAD-BEARING, and not only for muscle memory and the
 * URLs already written down in operators' notes. Listing events needs a
 * signed-in client against a reachable instance; before the first successful
 * sign-in, on an instance that is not answering, or on a build older than the
 * ListEvents binding, the id in the pasted URL is the ONLY source of it there
 * is. It is the fallback the whole feature degrades to, so removing it would
 * trade one dead end for another.
 * ===========================================================================
 *
 * What both parsers accept:
 *   - with or without a scheme          (https://host/... or host/...)
 *   - with or without a trailing slash
 *   - with a query string or a fragment (both ignored)
 *   - with credentials in the authority (user:pass@host — stripped)
 *   - with a port, which is part of the host
 *   - any path segment before /live-operation/ (a proxy prefix, say)
 *   - extra path segments after the id  (.../dl9-.../audio)
 *
 * What is rejected, with a reason rather than a shrug: a scheme this app
 * cannot speak, no host at all, and — on the host-only form, where nothing
 * downstream will ever re-examine the string — a "host" that cannot be one.
 */

/** The path segment that marks the event ID as the one that follows it. */
export const LIVE_OPERATION_SEGMENT = 'live-operation';

/**
 * splitAddress reduces any accepted address to its host and its path segments.
 *
 * Both parsers below are built on it so that "what counts as the host" cannot
 * drift between the strict form and the bare form: the same paste — credentials
 * and all — must yield the same host either way, because that host is what
 * internal/config.EffectiveSRTHost then derives the SRT target from. Two
 * near-identical copies of this stripping would be two chances for it not to.
 *
 * @param {unknown} input
 * @returns {{ok: true, host: string, segments: string[]}|{ok: false, error: string}}
 */
function splitAddress(input) {
  if (typeof input !== 'string' || input.trim() === '') {
    return {
      ok: false,
      error: 'Enter the M2L-X address: the instance address on its own, or the full live-operation URL.',
    };
  }

  let rest = input.trim();

  // Strip the scheme if there is one. A scheme this app cannot speak is worth
  // naming: pasting an ftp:// or file:// address means the wrong thing was
  // copied, and a complaint about the host or the path would be a confusing
  // answer to it.
  const schemeMatch = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//.exec(rest);
  if (schemeMatch) {
    const scheme = schemeMatch[1].toLowerCase();
    if (scheme !== 'http' && scheme !== 'https') {
      return { ok: false, error: `"${schemeMatch[1]}://" is not an M2L-X address — it must be http:// or https://.` };
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

  return { ok: true, host, segments: path.split('/').filter((s) => s !== '') };
}

/**
 * parseLiveOperationURL splits an M2L-X GUI URL into a host and an event ID.
 *
 * STRICT, deliberately: it demands the /live-operation/<id> segment and says so
 * when there is none. It is the inner parser — parseM2LXAddress is what the
 * Settings field uses — and it is kept strict because "this string names an
 * event" is a different question from "this string names an instance", and the
 * app has to be able to ask the first one on its own.
 *
 * It deliberately does NOT check that the host is plausible: it never did, this
 * path ships on air on Windows, and a live-operation URL always brings its host
 * along with a real path behind it. The plausibility check belongs where the
 * host arrives ALONE — see parseM2LXAddress.
 *
 * @param {unknown} input
 * @returns {{ok: true, host: string, eventId: string}|{ok: false, error: string}}
 */
export function parseLiveOperationURL(input) {
  const split = splitAddress(input);
  if (!split.ok) return split;
  const { host, segments } = split;

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
 * parseM2LXAddress reads the Settings screen's M2L-X address field, in either
 * of the two forms the header describes.
 *
 * eventId is '' — never absent, so a caller can compare it without reaching for
 * `?.` — when the address carried no event: that is the signal that the events
 * API is the source, not that anything went wrong.
 *
 * A truncated live-operation URL ("…/live-operation" or "…/live-operation/")
 * is treated as the host-only form rather than refused. The operator has still
 * named the instance unambiguously, and the id they lost off the end is exactly
 * the one the events API can now supply; refusing it would be the old dead end
 * arriving by a slightly different route.
 *
 * @param {unknown} input
 * @returns {{ok: true, host: string, eventId: string}|{ok: false, error: string}}
 */
export function parseM2LXAddress(input) {
  // The strict parser is asked FIRST and is the only thing that decides whether
  // an address names an event, so the two answers cannot drift apart: a URL the
  // strict parser accepts is accepted identically here, event id and all.
  const strict = parseLiveOperationURL(input);
  if (strict.ok) return strict;

  // It said no. That is either "no event in this address", which is now a
  // perfectly good answer, or a fault in the address itself — an unusable
  // scheme, no host — which splitAddress reports again here in the same words.
  const split = splitAddress(input);
  if (!split.ok) return split;
  const { host } = split;

  // Host-only. NOTHING downstream will look at this string again — it is saved
  // as m2lxHost, dialled by the API client and reduced to the SRT target — so a
  // word that cannot be a host has to be caught here, in front of the operator,
  // rather than surfacing later as a DNS failure inside a reconnect ladder that
  // does not say which field caused it. On the full-URL form the /live-operation/
  // segment is itself the evidence that a real address was pasted, which is why
  // the check is only on this branch.
  const fault = hostFault(host);
  if (fault !== '') return { ok: false, error: fault };

  return { ok: true, host, eventId: '' };
}

/**
 * hostFault returns why `host` cannot be a host name, or '' when it can be one.
 *
 * This is a plausibility check, not inet_pton and not RFC 1123 in full: the job
 * is to catch a paste that is plainly not an address (a word, a sentence, a
 * stray port) while never refusing something an operator's DNS might genuinely
 * resolve. Anything it lets through still has to answer on the network.
 *
 * A single label is refused unless it is "localhost", which is the one
 * single-label name that means something everywhere. The instance addresses
 * this application is pointed at are full names (m2lx-….etapsiota.com), and
 * accepting a bare word silently is what "do not let a typo through as a
 * hostname" means in practice — the operator gets told which word it was.
 *
 * @param {string} host  the authority as splitAddress produced it: name,
 *        optional :port, IPv6 in brackets.
 * @returns {string} '' when the host is plausible, else the reason
 */
function hostFault(host) {
  let name = host;
  let port = null;

  if (name.startsWith('[')) {
    const end = name.indexOf(']');
    if (end < 0) {
      return `"${host}" is not a host: an IPv6 address needs its closing "]", as in [2001:db8::1].`;
    }
    const after = name.slice(end + 1);
    name = name.slice(0, end + 1);
    if (after !== '') {
      if (after[0] !== ':') return `"${host}" is not a host: there is text after the "]".`;
      port = after.slice(1);
    }
  } else {
    // lastIndexOf, matching bareHost: an unbracketed IPv6 literal is refused
    // below rather than silently losing its last group to a "port".
    const colon = name.lastIndexOf(':');
    if (colon >= 0) {
      port = name.slice(colon + 1);
      name = name.slice(0, colon);
    }
  }

  if (port !== null && !(/^[0-9]+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535)) {
    return `"${port}" is not a port number — an address with a port ends in something like ":8443".`;
  }

  if (name.startsWith('[')) {
    if (!/^\[[0-9A-Fa-f:.]+\]$/.test(name)) return `"${name}" is not an IPv6 address.`;
    return '';
  }

  // A trailing dot is the DNS root and is legal in an address bar; judge the
  // labels without it.
  const bare = name.endsWith('.') ? name.slice(0, -1) : name;
  if (bare === '') return 'That address has no host in it.';

  const labels = bare.split('.');
  for (const label of labels) {
    // Underscores are tolerated inside a label because internal DNS zones do
    // use them; a leading or trailing hyphen or underscore, an empty label
    // (host..example) or any other character is not a host.
    if (!/^[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?$/.test(label)) {
      return `"${host}" is not a host name — check the address that was pasted.`;
    }
  }

  if (labels.length === 1 && bare.toLowerCase() !== 'localhost') {
    return `"${bare}" is one word, not an M2L-X address — paste the whole address, like m2lx-wslstudios-matcht.etapsiota.com.`;
  }

  return '';
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
 * formatM2LXAddress is what populate() puts INTO the address field: the
 * instance's BASE address, and nothing else.
 *
 * ============ WHY THERE IS NO LONGER AN eventId PARAMETER ==================
 *
 * There used to be one, and this function asked formatLiveOperationURL first,
 * so a config that had both halves drew "https://<host>/live-operation/<id>"
 * in the box on every load. The operator's complaint was exactly that: they
 * pasted an instance address and the app handed back a longer one, "writing
 * URLs with extra parts". The long form is a page on the M2L-X GUI, not the
 * address of the instance, and this box asks for the instance.
 *
 * The event id has no business here either way. It is discovered from
 * GET /api/events/overview (internal/m2lx/events.go -> App.ListEvents ->
 * refreshEvents), it is shown on its own row by the picker or the
 * auto-selected note, and a preset never carries it — a preset is which venue,
 * never which match. So the parameter is GONE rather than merely ignored: an
 * ignored parameter is an invitation to pass the id again and expect it to
 * appear, which is how the long form got into the write path the first time.
 *
 * READING is untouched. parseM2LXAddress still accepts a full live-operation
 * URL and still fills both fields from it — operators have those URLs in their
 * notes, and before the first successful sign-in the id in a pasted URL is the
 * only source of one there is. This is only about what the app WRITES BACK.
 * The consequence, deliberate: paste a long URL, and once the form is
 * re-populated the box shows the base address while the event stays on its own
 * row. The field's hint says so, because a box that silently shortens what was
 * pasted into it otherwise reads as something being lost.
 * ===========================================================================
 *
 * @param {string} host
 * @returns {string} '' only when there is no host
 */
export function formatM2LXAddress(host) {
  const h = typeof host === 'string' ? host.trim().replace(/^https?:\/\//i, '').replace(/\/+$/, '') : '';
  return h === '' ? '' : `https://${h}`;
}
