/**
 * The one RTCPeerConnection and its eight recvonly transceivers.
 *
 * Owner: WP-5a.
 *
 * spec §7: "The page offers the same eight recvonly transceivers Sony's page
 * offers — 1 video (mid 0) + 7 audio (mids 1–7) — so the mid-to-bus map holds
 * positionally. Silent tracks cost ~1.2 kbps, so leaving mids 3–7 subscribed but
 * unrouted is cheaper than risking a re-mapped answer."
 *
 * The order in TRANSCEIVER_PLAN is therefore load-bearing and must not change.
 */

import { TRANSCEIVER_PLAN, checkMidAssignment } from './buses.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';

/**
 * createViewerPeerConnection builds the peer connection and adds the eight
 * transceivers in plan order.
 *
 * `bundlePolicy: 'max-bundle'` matches what a viewer page normally negotiates
 * and keeps all eight m-lines on one ICE transport: eight separate transports
 * would mean eight candidate gathering runs and eight TURN allocations for a
 * feed that is one media flow.
 *
 * @param {RTCIceServer[]} iceServers
 * @returns {{pc: RTCPeerConnection, transceivers: RTCRtpTransceiver[]}}
 */
export function createViewerPeerConnection(iceServers) {
  if (typeof RTCPeerConnection !== 'function') {
    throw new MonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      'this browser has no RTCPeerConnection; the monitor cannot run here',
    );
  }

  const pc = new RTCPeerConnection({
    iceServers,
    iceTransportPolicy: 'all',
    bundlePolicy: 'max-bundle',
    rtcpMuxPolicy: 'require',
  });

  /** @type {RTCRtpTransceiver[]} */
  const transceivers = [];
  try {
    for (const kind of TRANSCEIVER_PLAN) {
      transceivers.push(pc.addTransceiver(kind, { direction: 'recvonly' }));
    }
  } catch (err) {
    try {
      pc.close();
    } catch {
      /* already closing */
    }
    throw toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'addTransceiver', err);
  }

  return { pc, transceivers };
}

/**
 * createViewerOffer creates the SDP offer and applies it as the local
 * description.
 *
 * No `offerToReceiveAudio`/`offerToReceiveVideo`: those are the legacy way of
 * asking for m-lines and would add m-lines on top of the eight explicit
 * transceivers, which is exactly the re-map the plan exists to avoid.
 *
 * @param {RTCPeerConnection} pc
 * @returns {Promise<RTCSessionDescription>} the applied local description
 */
export async function createViewerOffer(pc) {
  let offer;
  try {
    offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
  } catch (err) {
    throw toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'createOffer/setLocalDescription', err);
  }
  if (!pc.localDescription) {
    throw new MonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      'setLocalDescription completed but localDescription is null',
    );
  }
  return pc.localDescription;
}

/**
 * applyAnswer sets the remote description and reports any mid re-mapping.
 *
 * The mid check is not cosmetic. Everything downstream — which track is the
 * mosaic, which track is the CLN return — is positional, so an answer that
 * assigned mids in a different order would leave the commentator listening to
 * the wrong bus with no indication that anything was wrong. Detecting it does
 * not fix it (fixing it would mean a mid-to-bus discovery subsystem, which spec
 * §9 explicitly rejects), but a MID_REMAPPED error in the UI turns a baffling
 * "I can hear myself" into a one-line diagnosis.
 *
 * @param {RTCPeerConnection} pc
 * @param {RTCRtpTransceiver[]} transceivers in offer order
 * @param {RTCSessionDescriptionInit} answer
 * @returns {Promise<string[]>} mid mismatches; empty means the positional map holds
 */
export async function applyAnswer(pc, transceivers, answer) {
  if (!answer || typeof answer !== 'object' || typeof answer.sdp !== 'string') {
    throw new MonitorError(
      MonitorErrorCode.SIGNALLING_FAILED,
      'the signalling service sent an SDP answer with no sdp',
    );
  }
  try {
    await pc.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
  } catch (err) {
    throw toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'setRemoteDescription', err);
  }
  return checkMidAssignment(transceivers);
}

/**
 * stopReceivers stops every receiver track on the connection.
 *
 * Closing an RTCPeerConnection is documented to stop its transports, but the
 * MediaStreamTracks it produced stay in the 'live' state in Chromium until
 * something stops them, and a track still referenced by a MediaStreamAudioSource
 * keeps the whole decode path alive. Over a match's worth of reconnects that is
 * the leak that matters.
 *
 * @param {RTCPeerConnection} pc
 */
export function stopReceivers(pc) {
  if (!pc) return;
  let receivers = [];
  try {
    receivers = pc.getReceivers ? pc.getReceivers() : [];
  } catch {
    receivers = [];
  }
  for (const r of receivers) {
    try {
      if (r && r.track && typeof r.track.stop === 'function') r.track.stop();
    } catch {
      /* the track was already ended */
    }
  }
}

/**
 * closePeerConnection detaches every handler, stops every receiver track and
 * closes the connection. Safe to call twice.
 *
 * The handlers are nulled *before* close() because close() itself fires a
 * connectionstatechange to 'closed', and a restart driven by that event would
 * race the teardown that caused it.
 *
 * @param {RTCPeerConnection|null} pc
 */
export function closePeerConnection(pc) {
  if (!pc) return;
  pc.ontrack = null;
  pc.onicecandidate = null;
  pc.onconnectionstatechange = null;
  pc.oniceconnectionstatechange = null;
  pc.onicegatheringstatechange = null;
  pc.onsignalingstatechange = null;
  pc.onnegotiationneeded = null;
  stopReceivers(pc);
  try {
    pc.close();
  } catch {
    /* already closed */
  }
}

/**
 * peerConnectionIsUp reports whether pc has a negotiated media transport — i.e.
 * whether the DTLS/SRTP connection exists and media flows WITHOUT the signalling
 * socket.
 *
 * It is the test the monitor applies when the KVS signalling socket closes or
 * errors. The signalling channel exists ONLY to establish the connection: the
 * SDP offer/answer and the trickled ICE candidates. Once connectionState reaches
 * 'connected' the media flows peer-to-peer over DTLS/SRTP and the signalling
 * socket is no longer in the path — AWS KVS routinely closes it at that point,
 * by design. So a signalling close AFTER this returns true is expected and must
 * not disturb the audio; a close BEFORE it means the connection could not be
 * established and the whole chain has to be redone.
 *
 * 'disconnected' counts as up, for the same reason onconnectionstatechange
 * leaves it alone: ICE consent-freshness recovers from a short outage on its own
 * with no signalling, and promotes to 'failed' if it cannot — at which point the
 * connection-state handler, not the signalling handler, rebuilds. Tearing the
 * media down from the signalling side while ICE is still trying to recover would
 * throw away a connection that was about to come back.
 *
 * A null pc, or one still in 'new'/'connecting', is NOT up: the setup has not
 * finished, so losing signalling there is fatal to this attempt.
 *
 * @param {RTCPeerConnection|null} pc
 * @returns {boolean}
 */
export function peerConnectionIsUp(pc) {
  if (!pc) return false;
  const s = pc.connectionState;
  return s === 'connected' || s === 'disconnected';
}
