/**
 * Turning a KVS IceServerList into an RTCIceServer[].
 *
 * Owner: WP-5a. Pure — no AWS SDK, no browser API — so that it can be unit
 * tested. It is separated from signalling.js for exactly that reason: a
 * malformed entry here produces a peer connection that gathers no relay
 * candidates and then fails ICE several seconds later, at which point the cause
 * is four layers away from the symptom.
 */

/**
 * kvsStunUrl is the regional KVS STUN endpoint, in the form the KVS WebRTC
 * SDK's own generateStunUrl produces for the non-FIPS, non-dual-stack case.
 * Duplicated here as a fallback so that this module stays pure; signalling.js
 * passes the SDK's function in when it is available.
 *
 * @param {string} region
 * @returns {string}
 */
export function kvsStunUrl(region) {
  return `stun:stun.kinesisvideo.${region}.amazonaws.com:443`;
}

/**
 * buildIceServers assembles the ICE server list for the viewer's peer
 * connection: the regional KVS STUN server first, then every well-formed TURN
 * entry from GetIceServerConfig.
 *
 * The STUN server goes in unconditionally. Sony's own GUI includes it and so
 * does every AWS viewer sample; without it a peer behind a NAT that TURN cannot
 * traverse has no server-reflexive candidate at all.
 *
 * Malformed entries are skipped rather than passed through. An RTCIceServer
 * with an empty `urls` throws from the RTCPeerConnection constructor in
 * Chromium, which would take out the whole session because one TURN entry in a
 * list of three came back blank.
 *
 * @param {string} region
 * @param {ReadonlyArray<{Uris?: string[], Username?: string, Password?: string}>} iceServerList
 * @param {(region: string) => string} [stunUrl] injectable; defaults to kvsStunUrl
 * @returns {RTCIceServer[]}
 */
export function buildIceServers(region, iceServerList, stunUrl = kvsStunUrl) {
  const servers = [];

  let stun;
  try {
    stun = stunUrl(region);
  } catch {
    stun = kvsStunUrl(region);
  }
  if (typeof stun === 'string' && stun !== '') servers.push({ urls: stun });

  for (const s of iceServerList || []) {
    if (!s || !Array.isArray(s.Uris)) continue;
    const uris = s.Uris.filter((u) => typeof u === 'string' && u.trim() !== '');
    if (uris.length === 0) continue;
    const entry = { urls: uris };
    // Only set the credentials when both are present: a TURN server with a
    // username and no password is rejected by the constructor, and a STUN-only
    // entry must not carry them at all.
    if (typeof s.Username === 'string' && s.Username !== '' && typeof s.Password === 'string' && s.Password !== '') {
      entry.username = s.Username;
      entry.credential = s.Password;
    }
    servers.push(entry);
  }

  return servers;
}
