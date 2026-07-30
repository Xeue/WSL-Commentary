/**
 * Credential providers for harness.html.
 *
 * Owner: WP-5a. Nothing in the shipped app imports this file — the app's
 * provider is a one-line wrapper around the Wails-bound GetKVSCredentials:
 *
 *   const getCredentials = () => window.go.main.App.GetKVSCredentials();
 *
 * These exist so the monitor can be exercised without Go running, and so the
 * failure paths (bad credentials, an M2L-X that is down, credentials that expire
 * mid-match) can be provoked deliberately rather than waited for.
 */

/**
 * SAMPLE_CREDENTIALS documents the exact JSON shape internal/kvs.Credentials
 * marshals to. The values are fictional; the field names are contract.
 *
 * Measured sample behind it — docs/test-results.md §2.4 item 6:
 *
 *   GET /api/live_operation/kvs/webrtc_info/{event}
 *     -> {"region":"eu-west-1","signaling_channel":{"pgm":["webrtc-wslstudios-matcht"]}}
 *
 * Note the absent ARN: `channelArn` is empty and `channelName` is the
 * authoritative identifier.
 */
export const SAMPLE_CREDENTIALS = Object.freeze({
  region: 'eu-west-1',
  channelName: 'webrtc-wslstudios-matcht',
  channelArn: '',
  accessKeyId: 'ASIA…',
  secretKey: '…',
  sessionToken: '…',
  expiry: '2030-01-01T12:00:00Z',
});

/** LOCAL_STORAGE_KEY is where the harness remembers what was typed in. */
export const LOCAL_STORAGE_KEY = 'wslcomms.monitor.harness.credentials';

/**
 * createStaticCredentialProvider returns a provider that always resolves to the
 * same object.
 *
 * @param {object} credentials
 * @returns {() => Promise<object>}
 */
export function createStaticCredentialProvider(credentials) {
  return async () => credentials;
}

/**
 * createFormCredentialProvider returns a provider that reads whatever the
 * harness form currently holds. Read at call time rather than captured, so that
 * editing the form and letting the monitor restart picks up the new values —
 * which is exactly how the real thing behaves, since every restart re-fetches.
 *
 * @param {() => object} read
 * @returns {() => Promise<object>}
 */
export function createFormCredentialProvider(read) {
  return async () => read();
}

/**
 * createFailingCredentialProvider always rejects. Use it to watch the restart
 * ladder in backoff.js: 1 s, 2 s, 5 s, 10 s, then 15 s capped, each with up to
 * +/-15% jitter.
 *
 * @param {string} [message]
 * @returns {() => Promise<never>}
 */
export function createFailingCredentialProvider(message = 'M2L-X is not reachable') {
  return async () => {
    throw new Error(message);
  };
}

/**
 * createFlakyCredentialProvider fails the first `failures` calls and then
 * succeeds. This is the interesting case: it proves the monitor recovers on its
 * own rather than needing a click, and that the ladder resets after a good
 * session.
 *
 * @param {() => Promise<object>} inner
 * @param {number} failures
 * @returns {() => Promise<object>}
 */
export function createFlakyCredentialProvider(inner, failures) {
  let calls = 0;
  return async () => {
    calls += 1;
    if (calls <= failures) {
      throw new Error(`simulated M2L-X failure ${calls} of ${failures}`);
    }
    return inner();
  };
}

/**
 * loadStoredCredentials reads the harness form's last contents.
 *
 * NOTE: this puts a live AWS session token in localStorage. That is acceptable
 * for a developer harness on a developer machine and is NOT acceptable anywhere
 * near the shipped app, which is why this file is not imported by it. The
 * credentials are short-lived Cognito session credentials scoped to one KVS
 * channel, but treat the machine you paste them into as you would any machine
 * holding a live token.
 *
 * @returns {object|null}
 */
export function loadStoredCredentials() {
  try {
    const raw = window.localStorage.getItem(LOCAL_STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === 'object' ? parsed : null;
  } catch {
    return null;
  }
}

/**
 * storeCredentials remembers the harness form's contents. See the warning on
 * loadStoredCredentials.
 *
 * @param {object} credentials
 */
export function storeCredentials(credentials) {
  try {
    window.localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(credentials));
  } catch {
    /* private browsing, or storage disabled; the harness works without it */
  }
}

/** clearStoredCredentials wipes them. */
export function clearStoredCredentials() {
  try {
    window.localStorage.removeItem(LOCAL_STORAGE_KEY);
  } catch {
    /* nothing to do */
  }
}
