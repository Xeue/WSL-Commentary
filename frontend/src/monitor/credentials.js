/**
 * Normalising what the Wails-bound GetKVSCredentials returns.
 *
 * Owner: WP-5a. Pure — no AWS SDK, no browser API.
 *
 * The wire shape is `internal/kvs.Credentials` and its JSON tags are contract:
 *
 *   { region, channelName, channelArn, accessKeyId, secretKey, sessionToken, expiry }
 *
 * Two things about that shape matter here.
 *
 * 1. `secretKey`, not `secretAccessKey`. Both the AWS SDK v3 clients and the
 *    KVS WebRTC SDK's SigV4RequestSigner want `secretAccessKey`. The rename
 *    happens here, once, and nowhere else. Getting it wrong produces a signature
 *    mismatch at the WSS connect and nothing else — no useful error.
 *
 * 2. `channelArn` will normally be EMPTY. CONTRACT.md: "M2L-X gives a channel
 *    name. Credentials.ChannelName is therefore the authoritative identifier and
 *    Credentials.ChannelARN will normally be empty... do not branch on it:
 *    resolve the ARN from ChannelName instead." So this module prefers the name
 *    unconditionally and only falls back to a supplied ARN when there is no name
 *    at all, which is a defensive path for a shape we have exactly one sample of
 *    (open question SP-1).
 */

import { MonitorError, MonitorErrorCode } from './errors.js';

/**
 * pickString returns the first key present on `raw` whose value is a non-empty
 * string. It accepts several spellings because SP-1 rests on one measured
 * response from one instance; if a live instance spells a field differently the
 * monitor should still connect, and the discrepancy should be REPORTED as a
 * change to m2lx.KVSInfo / m2lx.KVSToken rather than papered over in nine
 * places.
 *
 * @param {Record<string, unknown>} raw
 * @param {string[]} keys in order of preference
 * @returns {string} '' when none match
 */
function pickString(raw, keys) {
  for (const k of keys) {
    const v = raw[k];
    if (typeof v === 'string' && v.trim() !== '') return v.trim();
  }
  return '';
}

/**
 * @typedef {object} NormalisedCredentials
 * @property {string} region                       AWS region, measured as 'eu-west-1'
 * @property {string} channelName                  authoritative channel identifier
 * @property {string} channelArn                   usually '' — resolve it from channelName
 * @property {{accessKeyId: string, secretAccessKey: string, sessionToken: string|undefined}} aws
 *           credentials in the shape both AWS SDKs expect
 * @property {Date|null} expiry                    null when M2L-X supplied no expiry
 */

/**
 * normaliseCredentials validates and reshapes one GetKVSCredentials result.
 *
 * Throws a MonitorError with code BAD_CREDENTIALS listing every missing field,
 * rather than the first — an engineer diagnosing this at an OB truck should get
 * the whole list in one line, not one field per reconnect cycle.
 *
 * @param {unknown} raw
 * @returns {NormalisedCredentials}
 */
export function normaliseCredentials(raw) {
  if (!raw || typeof raw !== 'object') {
    throw new MonitorError(
      MonitorErrorCode.BAD_CREDENTIALS,
      `GetKVSCredentials returned ${raw === null ? 'null' : typeof raw}, expected an object`,
    );
  }

  const region = pickString(raw, ['region', 'Region']);
  const channelName = pickString(raw, ['channelName', 'ChannelName', 'signalingChannelName']);
  const channelArn = pickString(raw, ['channelArn', 'channelARN', 'ChannelARN', 'ChannelArn']);
  const accessKeyId = pickString(raw, ['accessKeyId', 'AccessKeyId', 'AccessKeyID']);
  const secretAccessKey = pickString(raw, ['secretKey', 'secretAccessKey', 'SecretKey', 'SecretAccessKey']);
  const sessionToken = pickString(raw, ['sessionToken', 'SessionToken']);

  const missing = [];
  if (!region) missing.push('region');
  if (!channelName && !channelArn) missing.push('channelName (or channelArn)');
  if (!accessKeyId) missing.push('accessKeyId');
  if (!secretAccessKey) missing.push('secretKey');

  if (missing.length > 0) {
    throw new MonitorError(
      MonitorErrorCode.BAD_CREDENTIALS,
      `GetKVSCredentials is missing: ${missing.join(', ')}`,
    );
  }

  return {
    region,
    channelName,
    channelArn,
    aws: {
      accessKeyId,
      secretAccessKey,
      // Cognito's GetCredentialsForIdentity always issues a session token, but
      // an empty one must be omitted rather than sent as '': SigV4 signs the
      // X-Amz-Security-Token header only when it is present, and an empty
      // string is present.
      sessionToken: sessionToken || undefined,
    },
    expiry: parseExpiry(raw),
  };
}

/**
 * parseExpiry reads the `expiry` field. Go marshals time.Time as RFC 3339, and
 * a zero time.Time marshals as "0001-01-01T00:00:00Z" — which parses fine but
 * means "not set", so it is treated as null.
 *
 * @param {Record<string, unknown>} raw
 * @returns {Date|null}
 */
export function parseExpiry(raw) {
  const s = pickString(raw, ['expiry', 'Expiry', 'expiration', 'Expiration']);
  if (!s) return null;
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return null;
  // Go's zero time. Anything before 1971 is not a credential expiry.
  if (d.getUTCFullYear() < 1971) return null;
  return d;
}

/**
 * SKEW_MS is how far ahead of the stated expiry credentials are considered
 * spent. Thirty seconds covers clock skew between the commentary PC and AWS
 * plus the four round trips of the signalling chain.
 */
export const SKEW_MS = 30_000;

/**
 * credentialsExpired reports whether credentials are too close to expiry to
 * start a session with.
 *
 * There is no refresh scheduler by design (spec §7). This is used only to turn
 * "the WSS connect failed with an opaque 403" into "the credentials M2L-X gave
 * us had already expired", which is a materially more useful error message.
 *
 * @param {NormalisedCredentials} creds
 * @param {number} [nowMs] defaults to Date.now()
 * @param {number} [skewMs] defaults to SKEW_MS
 * @returns {boolean} false when there is no expiry to compare against
 */
export function credentialsExpired(creds, nowMs = Date.now(), skewMs = SKEW_MS) {
  if (!creds || !creds.expiry) return false;
  return creds.expiry.getTime() - skewMs <= nowMs;
}

/**
 * describeCredentials produces a one-line summary safe to log. It never
 * includes the secret key or the session token: those are the two values that
 * must not reach a log line, a screenshot or a support bundle.
 *
 * @param {NormalisedCredentials} creds
 * @returns {string}
 */
export function describeCredentials(creds) {
  if (!creds) return 'no credentials';
  const akid = creds.aws && creds.aws.accessKeyId ? creds.aws.accessKeyId : '';
  const masked = akid ? `${akid.slice(0, 4)}…${akid.slice(-4)}` : 'none';
  const exp = creds.expiry ? creds.expiry.toISOString() : 'unstated';
  return `region=${creds.region} channel=${creds.channelName || creds.channelArn} akid=${masked} expiry=${exp}`;
}
