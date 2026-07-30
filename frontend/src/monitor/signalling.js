/**
 * The KVS viewer signalling chain, spec §7 step by step.
 *
 * Owner: WP-5a.
 *
 * Go has already done the first half of the chain (internal/kvs):
 *
 *   GET /api/live_operation/kvs/webrtc_info/{eventId}   → region, channel NAME
 *   GET /api/live_operation/kvs/webrtc_token/{eventId}  → Cognito identity, token
 *   Cognito GetCredentialsForIdentity                   → temporary credentials
 *
 * This module does the second half, in the browser, because AWS ships a
 * supported KVS WebRTC signalling client for JavaScript and none for Go:
 *
 *   1. DescribeSignalingChannel        channel NAME  → channel ARN
 *   2. GetSignalingChannelEndpoint     role VIEWER   → WSS and HTTPS endpoints
 *   3. GetIceServerConfig                            → TURN credentials
 *   4. SigV4-presigned WSS connect                   → SignalingClient
 *
 * Step 1 is not optional and not a convenience. CONTRACT.md: "Note what
 * webrtc_info does NOT return: a channel ARN. M2L-X gives a channel name...
 * WP-5a resolves the ARN in JavaScript with DescribeSignalingChannel before
 * GetSignalingChannelEndpoint — which is why go.mod has the Cognito client but
 * no kinesisvideo client, and package.json has both KVS clients. Do not move
 * that boundary."
 *
 * All four calls use the temporary credentials from Go. None of them use the
 * ambient AWS credential chain: there is none on a commentary PC, and letting
 * the SDK look for one would produce a confusing several-second delay while it
 * probes the EC2 instance metadata endpoint on a facility network that will not
 * answer.
 */

import {
  KinesisVideoClient,
  DescribeSignalingChannelCommand,
  GetSignalingChannelEndpointCommand,
} from '@aws-sdk/client-kinesis-video';
import {
  KinesisVideoSignalingClient,
  GetIceServerConfigCommand,
} from '@aws-sdk/client-kinesis-video-signaling';

import { kvsSdk } from './kvs-sdk.js';
import { buildIceServers, kvsStunUrl } from './ice.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';

/**
 * SDK_REQUEST_TIMEOUT_MS bounds each of the three control-plane calls.
 *
 * Ten seconds. The measured RTT to eu-west-1 from the facility is 21 ms median
 * (spec §5), so ten seconds is roughly 500x the expected latency: it will never
 * fire on a working network. It exists so that a facility firewall that
 * blackholes 443 to kinesisvideo.eu-west-1.amazonaws.com produces a restart in
 * ten seconds rather than hanging on the browser's default TCP timeout, which
 * on Windows is over two minutes of a dark monitor.
 */
export const SDK_REQUEST_TIMEOUT_MS = 10_000;

/**
 * newViewerClientId generates the X-Amz-ClientId for this viewer session.
 *
 * KVS constrains client ids to /^[a-zA-Z0-9_.-]{1,256}$/ (the SDK validates
 * correlation ids against exactly that pattern) and requires viewers on the same
 * channel to be distinct — a duplicate id displaces the earlier viewer, which
 * would mean two commentary positions on the same event fighting over one slot.
 * A fresh random suffix per session avoids that, and per *session* rather than
 * per *process* so that a reconnect never collides with the socket AWS has not
 * yet finished tearing down.
 *
 * @returns {string}
 */
export function newViewerClientId() {
  const rand =
    typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID().replace(/-/g, '')
      : Math.random().toString(36).slice(2) + Date.now().toString(36);
  return `wslcomms-${rand}`.slice(0, 256);
}

/**
 * awsClientConfig builds the shared configuration for both AWS SDK v3 clients.
 *
 * @param {import('./credentials.js').NormalisedCredentials} creds
 * @param {string} [endpoint] overrides the default regional endpoint
 * @returns {object}
 */
function awsClientConfig(creds, endpoint) {
  const cfg = {
    region: creds.region,
    credentials: {
      accessKeyId: creds.aws.accessKeyId,
      secretAccessKey: creds.aws.secretAccessKey,
      sessionToken: creds.aws.sessionToken,
    },
    // Retries cost us time we would rather spend on a clean restart: the whole
    // chain is idempotent and the restart ladder already handles failure. One
    // attempt, then fail fast.
    maxAttempts: 1,
    requestHandler: { requestTimeout: SDK_REQUEST_TIMEOUT_MS },
  };
  if (endpoint) cfg.endpoint = endpoint;
  return cfg;
}

/**
 * @typedef {object} ViewerEndpoints
 * @property {string} channelARN   the resolved ARN
 * @property {string} wss          the WSS signalling endpoint for role VIEWER
 * @property {string} https        the HTTPS endpoint for GetIceServerConfig
 */

/**
 * resolveViewerEndpoints performs steps 1 and 2: name → ARN → endpoints.
 *
 * @param {import('./credentials.js').NormalisedCredentials} creds
 * @param {{signal?: AbortSignal}} [opts]
 * @returns {Promise<ViewerEndpoints>}
 */
export async function resolveViewerEndpoints(creds, opts = {}) {
  const client = new KinesisVideoClient(awsClientConfig(creds));
  try {
    const channelARN = await resolveChannelArn(client, creds, opts);

    let endpointResponse;
    try {
      endpointResponse = await client.send(
        new GetSignalingChannelEndpointCommand({
          ChannelARN: channelARN,
          SingleMasterChannelEndpointConfiguration: {
            // WSS carries signalling; HTTPS is where GetIceServerConfig lives.
            Protocols: ['WSS', 'HTTPS'],
            // VIEWER, not MASTER: we consume the mosaic and the buses, we
            // publish nothing. A MASTER role here would put us in the position
            // M2L-X's media server already occupies.
            Role: kvsSdk().Role.VIEWER,
          },
        }),
        { abortSignal: opts.signal },
      );
    } catch (err) {
      throw toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'GetSignalingChannelEndpoint', err);
    }

    const list = (endpointResponse && endpointResponse.ResourceEndpointList) || [];
    const endpoints = {};
    for (const e of list) {
      if (e && typeof e.Protocol === 'string' && typeof e.ResourceEndpoint === 'string') {
        endpoints[e.Protocol.toUpperCase()] = e.ResourceEndpoint;
      }
    }

    if (!endpoints.WSS) {
      throw new MonitorError(
        MonitorErrorCode.SIGNALLING_FAILED,
        `GetSignalingChannelEndpoint returned no WSS endpoint for ${channelARN}`,
      );
    }
    if (!endpoints.HTTPS) {
      throw new MonitorError(
        MonitorErrorCode.SIGNALLING_FAILED,
        `GetSignalingChannelEndpoint returned no HTTPS endpoint for ${channelARN}`,
      );
    }

    return { channelARN, wss: endpoints.WSS, https: endpoints.HTTPS };
  } finally {
    // The v3 clients hold a keep-alive HTTP handler. One per attempt, destroyed
    // per attempt: over a match's worth of reconnects the difference between
    // destroying these and not is real.
    destroyQuietly(client);
  }
}

/**
 * resolveChannelArn turns the channel NAME from M2L-X into an ARN.
 *
 * Prefers the name unconditionally, per CONTRACT.md. Falls back to a supplied
 * ARN only when there is no name at all — which the one measured response says
 * will not happen, but SP-1 is one sample from one instance and an empty
 * `channelName` should degrade to "try the ARN" rather than to "no monitor".
 *
 * @param {KinesisVideoClient} client
 * @param {import('./credentials.js').NormalisedCredentials} creds
 * @param {{signal?: AbortSignal}} opts
 * @returns {Promise<string>}
 */
async function resolveChannelArn(client, creds, opts) {
  if (!creds.channelName) {
    if (creds.channelArn) return creds.channelArn;
    throw new MonitorError(
      MonitorErrorCode.BAD_CREDENTIALS,
      'neither channelName nor channelArn was supplied',
    );
  }

  let described;
  try {
    described = await client.send(
      new DescribeSignalingChannelCommand({ ChannelName: creds.channelName }),
      { abortSignal: opts.signal },
    );
  } catch (err) {
    throw toMonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      `DescribeSignalingChannel("${creds.channelName}")`,
      err,
    );
  }

  const arn =
    described && described.ChannelInfo && described.ChannelInfo.ChannelARN
      ? described.ChannelInfo.ChannelARN
      : '';
  if (!arn) {
    throw new MonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      `DescribeSignalingChannel("${creds.channelName}") returned no ChannelARN`,
    );
  }
  return arn;
}

/**
 * fetchIceServers performs step 3 and assembles the RTCIceServer list.
 *
 * @param {import('./credentials.js').NormalisedCredentials} creds
 * @param {ViewerEndpoints} endpoints
 * @param {{signal?: AbortSignal}} [opts]
 * @returns {Promise<RTCIceServer[]>}
 */
export async function fetchIceServers(creds, endpoints, opts = {}) {
  const client = new KinesisVideoSignalingClient(awsClientConfig(creds, endpoints.https));
  try {
    let response;
    try {
      response = await client.send(
        new GetIceServerConfigCommand({ ChannelARN: endpoints.channelARN }),
        { abortSignal: opts.signal },
      );
    } catch (err) {
      throw toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'GetIceServerConfig', err);
    }
    return buildIceServers(
      creds.region,
      (response && response.IceServerList) || [],
      sdkStunUrl,
    );
  } finally {
    destroyQuietly(client);
  }
}

/**
 * sdkStunUrl asks the KVS SDK for the regional STUN URL, falling back to the
 * documented form if the SDK failed to load. Non-FIPS, non-dual-stack: M2L-X's
 * channel is in a commercial region (measured: eu-west-1) and nothing in the
 * facility requires FIPS endpoints.
 *
 * @param {string} region
 * @returns {string}
 */
function sdkStunUrl(region) {
  try {
    return kvsSdk().generateStunUrl({
      useFipsEndpoints: false,
      useDualStackEndpoints: false,
      region,
    });
  } catch {
    return kvsStunUrl(region);
  }
}

/**
 * createSignalingClient performs step 4: a SigV4-presigned WSS connect as
 * role VIEWER.
 *
 * The returned client is NOT opened — the caller must have its listeners
 * attached and its peer connection built before `open()`, because KVS can
 * deliver the SDP answer within a few milliseconds of the socket opening and an
 * answer with no listener is an answer lost.
 *
 * @param {object} args
 * @param {import('./credentials.js').NormalisedCredentials} args.creds
 * @param {ViewerEndpoints} args.endpoints
 * @param {string} args.clientId
 * @returns {any} a KVSWebRTC.SignalingClient
 */
export function createSignalingClient({ creds, endpoints, clientId }) {
  const sdk = kvsSdk();
  return new sdk.SignalingClient({
    channelARN: endpoints.channelARN,
    channelEndpoint: endpoints.wss,
    role: sdk.Role.VIEWER,
    region: creds.region,
    clientId,
    credentials: {
      accessKeyId: creds.aws.accessKeyId,
      secretAccessKey: creds.aws.secretAccessKey,
      sessionToken: creds.aws.sessionToken,
    },
  });
}

/**
 * destroyQuietly releases an AWS SDK v3 client's HTTP handler. Failures are
 * swallowed: this runs in `finally` blocks on paths that are already reporting
 * a more interesting error.
 *
 * @param {{destroy?: () => void}} client
 */
function destroyQuietly(client) {
  try {
    if (client && typeof client.destroy === 'function') client.destroy();
  } catch {
    /* nothing useful to do, and nothing depends on it */
  }
}
