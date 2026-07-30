/**
 * Error types for the monitor module.
 *
 * Owner: WP-5a. No other work package writes files in frontend/src/monitor/.
 *
 * Every error the monitor emits on its 'error' channel is a MonitorError with a
 * stable `code`. WP-5b is free to render nothing but `.message`, but the codes
 * exist so that the UI can distinguish "the commentator needs to click something"
 * (AUTOPLAY_BLOCKED) from "the network is broken" (SIGNALLING_FAILED) without
 * string-matching a message.
 *
 * The codes are part of the seam in the weak sense: adding one is safe, changing
 * the meaning of one is not.
 */

/**
 * MonitorErrorCode enumerates every code the monitor attaches to an emitted
 * error. Frozen so a typo in a comparison fails loudly rather than silently
 * matching undefined.
 */
export const MonitorErrorCode = Object.freeze({
  /** getCredentials() returned something the monitor cannot use. */
  BAD_CREDENTIALS: 'BAD_CREDENTIALS',
  /** getCredentials() itself threw or rejected. */
  CREDENTIALS_FAILED: 'CREDENTIALS_FAILED',
  /** DescribeSignalingChannel / GetSignalingChannelEndpoint / GetIceServerConfig failed. */
  SIGNALLING_FAILED: 'SIGNALLING_FAILED',
  /** The signalling WebSocket errored or closed. Per spec §7 this restarts the whole chain. */
  SIGNALLING_CLOSED: 'SIGNALLING_CLOSED',
  /** KVS answered a signalling message with success:false. */
  SIGNALLING_REJECTED: 'SIGNALLING_REJECTED',
  /** RTCPeerConnection.connectionState went to 'failed'. */
  PEER_FAILED: 'PEER_FAILED',
  /**
   * The answer did not assign the mids we offered in the order we offered them.
   * Non-fatal — the monitor keeps running on positional mapping — but it means
   * the mid-to-bus map in buses.js may be pointing at the wrong bus, which is
   * exactly the failure the spec's "leave all seven subscribed" advice exists to
   * avoid. Worth surfacing.
   */
  MID_REMAPPED: 'MID_REMAPPED',
  /** The browser refused to start playback without a user gesture. */
  AUTOPLAY_BLOCKED: 'AUTOPLAY_BLOCKED',
  /** This browser/WebView has no HTMLMediaElement.setSinkId. */
  SINK_ID_UNSUPPORTED: 'SINK_ID_UNSUPPORTED',
  /** setSinkId rejected — usually the device vanished or permission was refused. */
  SINK_ID_FAILED: 'SINK_ID_FAILED',
  /** The Web Audio graph could not be built at all. */
  AUDIO_FAILED: 'AUDIO_FAILED',
  /** The <video> element refused the mosaic stream. */
  VIDEO_FAILED: 'VIDEO_FAILED',
  /** createMonitor was given options it cannot work with. */
  BAD_OPTIONS: 'BAD_OPTIONS',
});

/**
 * MonitorError is every error the monitor emits. `cause` carries the underlying
 * DOMException or AWS SDK error where there was one, because the AWS SDK's
 * messages ("The security token included in the request is invalid") are the
 * only thing that will tell an engineer at 14:55 on a Saturday what is actually
 * wrong.
 */
export class MonitorError extends Error {
  /**
   * @param {string} code one of MonitorErrorCode
   * @param {string} message human-readable, safe to show in the UI
   * @param {unknown} [cause] the underlying error, if any
   */
  constructor(code, message, cause) {
    super(message);
    this.name = 'MonitorError';
    this.code = code;
    if (cause !== undefined) this.cause = cause;
  }
}

/**
 * toMonitorError normalises anything thrown — a MonitorError, a DOMException, an
 * AWS SDK error, a string, a rejected promise with no reason at all — into a
 * MonitorError with the given fallback code.
 *
 * @param {string} code fallback MonitorErrorCode if `err` is not already a MonitorError
 * @param {string} prefix short context, e.g. 'GetIceServerConfig'
 * @param {unknown} err whatever was caught
 * @returns {MonitorError}
 */
export function toMonitorError(code, prefix, err) {
  if (err instanceof MonitorError) return err;
  const detail =
    err && typeof err === 'object' && typeof err.message === 'string'
      ? err.message
      : String(err === undefined ? 'unknown error' : err);
  return new MonitorError(code, `${prefix}: ${detail}`, err);
}
