/**
 * Loading AWS's KVS WebRTC signalling client into a Vite browser bundle.
 *
 * Owner: WP-5a.
 *
 * ============================== READ THIS FIRST ==============================
 * This module imports the SDK's **pre-built browser bundle**
 * (`dist/kvs-webrtc.min.js`) rather than its package entry point
 * (`lib/index.js`). That is not a stylistic choice; the package entry point
 * cannot be used here at all, and finding that out empirically costs an
 * afternoon.
 *
 * `amazon-kinesis-video-streams-webrtc@2.8.1`'s CommonJS build does two things
 * that a browser bundle cannot satisfy:
 *
 *   lib/index.js:19          exports.VERSION = process.env.PACKAGE_VERSION;
 *   lib/SignalingClient.js:5 var events_1 = require("events");
 *
 * `events` is a Node builtin, and there is no `events` npm shim in this tree —
 * package.json is frozen (CONTRACT.md rule 1), so one cannot be added. Vite
 * resolves the builtin to an empty browser-external stub, `EventEmitter` comes
 * out `undefined`, and `class SignalingClient extends undefined` throws while
 * the module graph is still initialising. That does not break the monitor; it
 * breaks the *entire bundle*, including everything WP-5b wrote, with the message
 * "Class extends value undefined is not a constructor or null" and no hint as to
 * which import caused it. Verified against vite 7.3.6 in this tree.
 *
 * `dist/kvs-webrtc.min.js` is the same SDK, same version, built by AWS with
 * webpack for browsers: it carries its own `events` polyfill, has no
 * `process.env` reference, and ends with `window.KVSWebRTC = r`. It is 31 kB
 * minified. Importing it for side effect and reading the global is the shape
 * AWS's own browser samples use.
 *
 * This still satisfies spec §3's requirement that the SDK be "npm-vendored at
 * build time... Never loaded from a CDN — facility networks have no outbound web
 * access": the file comes out of node_modules and is bundled into the .exe by
 * Vite and then by `//go:embed all:frontend/dist`.
 *
 * If package.json is ever unfrozen, adding the `events` npm package would let
 * this become a plain `import { SignalingClient } from
 * 'amazon-kinesis-video-streams-webrtc'`. That is a coordinator decision, not
 * ours. Reported.
 * =============================================================================
 */

// Side-effect import: the bundle assigns window.KVSWebRTC when it evaluates.
// It has no ESM exports, so there is nothing to name here.
import 'amazon-kinesis-video-streams-webrtc/dist/kvs-webrtc.min.js';

import { MonitorError, MonitorErrorCode } from './errors.js';

/**
 * @typedef {object} KvsWebRtcSdk
 * @property {new (config: object) => any} SignalingClient
 * @property {{MASTER: 'MASTER', VIEWER: 'VIEWER'}} Role
 * @property {new (region: string, credentials: object) => any} SigV4RequestSigner
 * @property {(config: {useFipsEndpoints: boolean, useDualStackEndpoints: boolean, region: string}) => string} generateStunUrl
 * @property {string} VERSION
 */

/**
 * kvsSdk returns the loaded SDK.
 *
 * It resolves lazily rather than at module scope on purpose: a throw at module
 * scope in an ES module is unrecoverable and takes the whole page with it. A
 * throw from a function call is a MonitorError the UI can render.
 *
 * @returns {KvsWebRtcSdk}
 */
export function kvsSdk() {
  const sdk = typeof window !== 'undefined' ? window.KVSWebRTC : undefined;
  if (!sdk || typeof sdk.SignalingClient !== 'function') {
    throw new MonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      'the Kinesis Video Streams WebRTC SDK did not load (window.KVSWebRTC is missing); ' +
        'the monitor cannot start',
    );
  }
  return sdk;
}

/**
 * kvsSdkVersion is for the diagnostics line in the harness, and for confirming
 * at a glance that the bundled SDK is the version that was tested.
 *
 * @returns {string}
 */
export function kvsSdkVersion() {
  try {
    return kvsSdk().VERSION || 'unknown';
  } catch {
    return 'not loaded';
  }
}
