/**
 * The monitor: R3 (programme picture) and R4 (return audio) from one
 * RTCPeerConnection.
 *
 * Owner: WP-5a. This file is the seam with WP-5b and nothing outside
 * frontend/src/monitor/ may reach past it.
 *
 * THE SEAM
 * --------
 *   import { createMonitor } from './monitor/monitor.js';
 *
 *   const monitor = createMonitor({
 *     videoEl,          // HTMLVideoElement the mosaic is attached to
 *     audioEl,          // HTMLAudioElement the return is played through
 *     getCredentials,   // async () => Credentials — wraps GetKVSCredentials
 *     tile,             // {x,y,w,h} from config.monitorTile
 *     returnMid,        // number, from config.returnMid
 *     gainDb,           // number, from config.returnGainDb (default 18)
 *   });
 *
 *   monitor.start();              // async; opens the peer connection
 *   monitor.stop();               // async; tears everything down
 *   monitor.setReturnMid(n);      // switch the routed audio track live
 *   monitor.setGainDb(db);        // absolute make-up gain in dB
 *   monitor.setLevel(fraction);   // 0..1 user level, multiplies the gain
 *   monitor.setSinkId(deviceId);  // route to a headphone device
 *   monitor.on(event, cb);        // 'state' -> connectionState string
 *                                 // 'error' -> Error
 *
 * Two additions to the seam, both driven by the home screen's return controls:
 *
 *   monitor.setChannelMode(mode)  // 'stereo' | 'left' | 'right' — WHICH SOURCE
 *                                 // CHANNEL reaches the ears. Live, like
 *                                 // setReturnMid: no peer connection is
 *                                 // dropped. See ./channels.js.
 *   monitor.setAudioEnabled(on)   // whether this path is audible AT ALL.
 *                                 // The PICTURE is unaffected either way.
 *
 * setAudioEnabled(false) is what the return SOURCE control uses when the
 * commentator switches to the native SRT return: only one path may be audible
 * or they hear the same programme twice at different offsets. The monitor knows
 * nothing about SRT and must not — it is told to be silent, not why.
 *
 * ./channels.js is the one other module in here that WP-5b may import, and only
 * for CHANNEL_MODES and its labels. It is pure data and pure arithmetic, and the
 * alternative is a second hand-written copy of the same three words on the UI
 * side, which is the exact bug frontend/src/ui/returns.js exists to record.
 *
 * Additive to that list, because spec §7 requires the page to be able to scale
 * the tile to the window and the seam as written has no way to say so:
 *
 *   monitor.setScale(k)           // absolute CSS scale factor, returns the
 *                                 // value used after clamping
 *   monitor.fitTo(w, h?)          // scale the tile into a box; returns the
 *                                 // scale used. Pass width only to fill the
 *                                 // window width, per spec §10.
 *   monitor.getLayout()           // {tile, scale, width, height, mosaic}
 *   monitor.setTile(t)            // change the cropped region live
 *   monitor.resumeAudio()         // retry playback after an autoplay block
 *   monitor.getState()            // a diagnostics snapshot
 *   monitor.destroy()             // stop() plus unwrap the DOM and drop listeners
 *
 * WP-5b may ignore all of those except setScale/fitTo; none of them changes the
 * behaviour of the six methods above.
 *
 * One additive OPTION, for the same reason in reverse:
 *
 *   manageVideoLayout: false   // the page crops the mosaic itself; the monitor
 *                              // touches no layout, only srcObject
 *
 * The monitor's default is to own the crop, because spec §7 describes the crop
 * as the page's job and this module is the page's monitor. If WP-5b's own
 * `.pgm-tile` CSS crop is kept instead, pass false — two crops on one <video> is
 * one crop too many. See the "OPTING OUT" note at the top of video.js.
 *
 * LIFECYCLE AND FAILURE
 * ---------------------
 * spec §7: "If the peer connection fails or the signalling socket closes, tear
 * the whole thing down, re-fetch credentials in Go and redo the chain. No
 * refresh scheduler." That is exactly what happens here. There is no partial
 * recovery, no ICE restart, no credential refresh timer. Every restart begins
 * with a fresh getCredentials() and runs the whole four-step chain again. The
 * only thing between one attempt and the next is the short delay ladder in
 * backoff.js, which exists so that a dead network does not turn into a hot loop
 * against AWS.
 *
 * What is deliberately NOT rebuilt on a restart: the AudioContext and the DOM
 * wrappers. Both live for the lifetime of the monitor object. Chromium caps
 * AudioContexts per document at six, so building one per reconnect would silence
 * the return permanently part-way through a match — which is precisely the class
 * of bug that only shows up in the WP-9 soak.
 */

import { createEmitter } from './emitter.js';
import { createMosaicView } from './video.js';
import { createReturnAudio } from './audio.js';
import { normaliseCredentials, credentialsExpired, describeCredentials } from './credentials.js';
import { resolveViewerEndpoints, fetchIceServers, createSignalingClient, newViewerClientId } from './signalling.js';
import { createViewerPeerConnection, createViewerOffer, applyAnswer, closePeerConnection } from './peer.js';
import { VIDEO_MID, normaliseReturnMid, busForMid } from './buses.js';
import { normaliseTile } from './geometry.js';
import { normaliseChannelMode } from './channels.js';
import { clampGainDb, clampLevel, DEFAULT_GAIN_DB } from './gain.js';
import { restartDelayMs } from './backoff.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';

/** The two events on the seam. */
const EVENTS = ['state', 'error'];

/**
 * createMonitor builds a monitor bound to one pair of media elements.
 *
 * Nothing network-facing happens until start(). Construction only wraps the
 * <video> element in its crop structure, so it is safe to construct during page
 * setup and start later.
 *
 * @param {object} opts
 * @param {HTMLVideoElement} opts.videoEl
 * @param {HTMLAudioElement} opts.audioEl
 * @param {() => Promise<object>} opts.getCredentials
 * @param {{x: number, y: number, w: number, h: number}} [opts.tile]
 * @param {number} [opts.returnMid]
 * @param {number} [opts.gainDb]
 * @param {number} [opts.level] 0..1; defaults to 1
 * @param {string} [opts.sinkId] initial headphone device id
 * @param {string} [opts.channelMode] 'stereo' | 'left' | 'right'; default stereo
 * @param {boolean} [opts.audioEnabled] default true. False builds the return
 *        graph silent — the picture is unaffected — for a page that starts with
 *        the native SRT return selected.
 * @param {boolean} [opts.manageVideoLayout] default true. Set false if the page
 *        crops the mosaic itself; the monitor then touches no layout and
 *        setScale/fitTo become no-ops that still return a clamped number. See
 *        the "OPTING OUT" note at the top of video.js — WP-5b's `.pgm-tile` CSS
 *        is a second, percentage-based implementation of the same crop, and only
 *        one of the two may be active.
 * @returns {object} the Monitor
 */
export function createMonitor(opts) {
  const o = opts || {};

  if (!o.videoEl || o.videoEl.nodeName !== 'VIDEO') {
    throw new MonitorError(
      MonitorErrorCode.BAD_OPTIONS,
      'createMonitor: opts.videoEl must be an HTMLVideoElement',
    );
  }
  if (!o.audioEl || o.audioEl.nodeName !== 'AUDIO') {
    throw new MonitorError(
      MonitorErrorCode.BAD_OPTIONS,
      'createMonitor: opts.audioEl must be an HTMLAudioElement',
    );
  }
  if (typeof o.getCredentials !== 'function') {
    throw new MonitorError(
      MonitorErrorCode.BAD_OPTIONS,
      'createMonitor: opts.getCredentials must be a function returning KVS credentials',
    );
  }

  const emitter = createEmitter(EVENTS);
  const getCredentials = o.getCredentials;

  let returnMid = normaliseReturnMid(o.returnMid);
  let gainDb = clampGainDb(o.gainDb === undefined ? DEFAULT_GAIN_DB : o.gainDb);
  let level = o.level === undefined ? 1 : clampLevel(o.level);
  let sinkId = typeof o.sinkId === 'string' ? o.sinkId : '';
  let channelMode = normaliseChannelMode(o.channelMode);
  let audioEnabled = o.audioEnabled !== false;

  /** Reported connectionState. Starts at 'closed': nothing is open yet. */
  let state = 'closed';

  let running = false;
  let destroyed = false;
  /** Bumped on every attempt and on stop(); stale async work checks it. */
  let generation = 0;
  /** Consecutive failures, for the backoff ladder. Reset on 'connected'. */
  let attempt = 0;
  /** @type {ReturnType<typeof setTimeout>|null} */
  let restartTimer = null;
  /** @type {Session|null} */
  let session = null;
  /** @type {ReturnType<typeof createReturnAudio>|null} — built on first routed track. */
  let audio = null;
  /** Diagnostics only. */
  let lastCredentialSummary = '';

  const emitError = (err) => {
    const e = err instanceof Error ? err : new Error(String(err));
    // The console line is what an engineer with DevTools open will read; the
    // event is what the commentator's UI will show.
    console.error('[monitor]', e);
    emitter.emit('error', e);
  };

  const setState = (next) => {
    if (next === state) return;
    state = next;
    emitter.emit('state', next);
  };

  const view = createMosaicView({
    videoEl: o.videoEl,
    tile: normaliseTile(o.tile),
    manageLayout: o.manageVideoLayout !== false,
    onError: (err) => emitError(toMonitorError(MonitorErrorCode.VIDEO_FAILED, 'programme picture', err)),
  });

  /**
   * @typedef {object} Session
   * @property {number} gen
   * @property {AbortController} abort
   * @property {RTCPeerConnection|null} pc
   * @property {any} sc the KVS SignalingClient
   * @property {RTCRtpTransceiver[]} transceivers
   * @property {Map<number, MediaStreamTrack>} tracks keyed by offer index == mid
   * @property {boolean} closed
   */

  /** stale reports whether a session's async continuation should stop. */
  const stale = (s) => destroyed || !running || !s || s.closed || s.gen !== generation;

  /** ensureAudio builds the return-audio chain on first use. */
  function ensureAudio() {
    if (audio || destroyed) return audio;
    try {
      audio = createReturnAudio({
        audioEl: o.audioEl,
        gainDb,
        level,
        sinkId,
        channelMode,
        muted: !audioEnabled,
        onError: emitError,
      });
    } catch (err) {
      emitError(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'return audio', err));
      audio = null;
    }
    return audio;
  }

  /**
   * routeReturn routes the track for the currently selected mid.
   *
   * If that track has not arrived yet nothing happens and nothing is torn down:
   * whatever is already routed keeps playing until the wanted track appears, at
   * which point onTrack calls back here. Going silent while waiting would be the
   * worse choice — all seven tracks arrive within a few hundred milliseconds of
   * the answer, so the window is short, and a commentator hearing the previous
   * bus for 200 ms is better than one hearing nothing.
   */
  function routeReturn(s) {
    if (stale(s)) return;
    const track = s.tracks.get(returnMid);
    if (!track) return;
    const a = ensureAudio();
    if (!a) return;
    a.attach(track).catch((err) =>
      emitError(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'routing the return', err)),
    );
  }

  /** onTrack maps an arriving track onto its offer position, which is its mid. */
  function onTrack(s, ev) {
    if (stale(s)) return;
    const idx = s.transceivers.indexOf(ev.transceiver);
    if (idx < 0) {
      emitError(
        new MonitorError(
          MonitorErrorCode.MID_REMAPPED,
          'a media track arrived on a transceiver the monitor did not offer; ' +
            'the mid-to-bus map may not hold for this channel',
        ),
      );
      return;
    }

    if (idx === VIDEO_MID) {
      // A fresh MediaStream rather than ev.streams[0]: the msid the master sends
      // groups tracks in ways we do not control, and we want exactly this track
      // on the element.
      view.attach(new MediaStream([ev.track]));
      return;
    }

    s.tracks.set(idx, ev.track);
    if (idx === returnMid) routeReturn(s);
  }

  /** teardownSession closes everything belonging to one attempt. Idempotent. */
  function teardownSession() {
    const s = session;
    session = null;
    if (!s) return;
    s.closed = true;

    try {
      s.abort.abort();
    } catch {
      /* already aborted */
    }

    if (s.sc) {
      try {
        // The SDK's SignalingClient is an EventEmitter; drop every listener
        // before closing so its own 'close' event cannot re-enter scheduleRestart.
        if (typeof s.sc.removeAllListeners === 'function') s.sc.removeAllListeners();
      } catch {
        /* nothing depends on it */
      }
      try {
        s.sc.close();
      } catch {
        /* already closed */
      }
      s.sc = null;
    }

    closePeerConnection(s.pc);
    s.pc = null;
    s.transceivers = [];
    s.tracks.clear();

    if (audio) audio.detach();
    view.detach();
  }

  function clearRestartTimer() {
    if (restartTimer !== null) {
      clearTimeout(restartTimer);
      restartTimer = null;
    }
  }

  /**
   * scheduleRestart tears the session down and queues another whole-chain
   * attempt. Guarded against the common cascade where the peer connection
   * failing and the signalling socket closing both fire.
   */
  function scheduleRestart() {
    if (destroyed || !running) return;
    if (restartTimer !== null) return;
    teardownSession();
    const delay = restartDelayMs(attempt);
    attempt = Math.min(attempt + 1, 1_000_000);
    restartTimer = setTimeout(() => {
      restartTimer = null;
      runAttempt();
    }, delay);
  }

  /**
   * runAttempt performs the whole chain once: credentials, ARN, endpoints, ICE
   * servers, peer connection, presigned WSS connect, offer.
   *
   * Every `await` is followed by a staleness check, because stop() can be called
   * at any point and an attempt that carries on after stop() would leave an open
   * peer connection nobody holds a reference to.
   */
  async function runAttempt() {
    if (destroyed || !running) return;

    teardownSession();

    const myGen = ++generation;
    /** @type {Session} */
    const s = {
      gen: myGen,
      abort: new AbortController(),
      pc: null,
      sc: null,
      transceivers: [],
      tracks: new Map(),
      closed: false,
    };
    session = s;

    setState('new');

    try {
      let raw;
      try {
        raw = await getCredentials();
      } catch (err) {
        throw toMonitorError(MonitorErrorCode.CREDENTIALS_FAILED, 'GetKVSCredentials', err);
      }
      if (stale(s)) return;

      const creds = normaliseCredentials(raw);
      lastCredentialSummary = describeCredentials(creds);
      if (credentialsExpired(creds)) {
        throw new MonitorError(
          MonitorErrorCode.BAD_CREDENTIALS,
          `the KVS credentials from M2L-X had already expired (${lastCredentialSummary}); ` +
            'check the clock on this machine and on M2L-X',
        );
      }

      const endpoints = await resolveViewerEndpoints(creds, { signal: s.abort.signal });
      if (stale(s)) return;

      const iceServers = await fetchIceServers(creds, endpoints, { signal: s.abort.signal });
      if (stale(s)) return;

      const { pc, transceivers } = createViewerPeerConnection(iceServers);
      s.pc = pc;
      s.transceivers = transceivers;

      pc.ontrack = (ev) => onTrack(s, ev);

      pc.onconnectionstatechange = () => {
        if (stale(s)) return;
        const cs = pc.connectionState;
        setState(cs);
        if (cs === 'connected') {
          // A session that reached 'connected' resets the ladder, so a second
          // outage an hour later starts at 1 s again rather than 15 s.
          attempt = 0;
          return;
        }
        if (cs === 'failed') {
          emitError(
            new MonitorError(
              MonitorErrorCode.PEER_FAILED,
              'the monitor peer connection failed; reconnecting',
            ),
          );
          scheduleRestart();
          return;
        }
        if (cs === 'closed') {
          // Our own teardown nulls this handler before closing, so reaching
          // here means the remote end or the browser closed it.
          scheduleRestart();
        }
        // 'disconnected' is deliberately left alone: ICE consent-freshness
        // recovers from short outages by itself and promotes to 'failed' if it
        // cannot. Restarting on 'disconnected' would throw away a connection
        // that was about to come back.
      };

      pc.onicecandidate = (ev) => {
        if (stale(s) || !ev.candidate || !s.sc) return;
        try {
          s.sc.sendIceCandidate(ev.candidate);
        } catch (err) {
          // The SDK throws if the socket is not open. That is a race we lose
          // harmlessly: the candidate is dropped and ICE proceeds on the
          // others, or the socket close handler restarts us.
          console.warn('[monitor] could not send ICE candidate:', err);
        }
      };

      const sc = createSignalingClient({
        creds,
        endpoints,
        clientId: newViewerClientId(),
      });
      s.sc = sc;

      sc.on('open', async () => {
        if (stale(s)) return;
        try {
          const offer = await createViewerOffer(pc);
          if (stale(s)) return;
          sc.sendSdpOffer(offer);
        } catch (err) {
          if (stale(s)) return;
          emitError(toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'sending the SDP offer', err));
          scheduleRestart();
        }
      });

      sc.on('sdpAnswer', async (answer) => {
        if (stale(s)) return;
        try {
          const problems = await applyAnswer(pc, s.transceivers, answer);
          if (stale(s)) return;
          if (problems.length > 0) {
            emitError(
              new MonitorError(
                MonitorErrorCode.MID_REMAPPED,
                'the signalling answer did not keep the mid order the monitor offered — ' +
                  'the return may be the wrong bus: ' +
                  problems.join('; '),
              ),
            );
          }
        } catch (err) {
          if (stale(s)) return;
          emitError(toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'applying the SDP answer', err));
          scheduleRestart();
        }
      });

      sc.on('iceCandidate', (candidate) => {
        if (stale(s)) return;
        Promise.resolve(pc.addIceCandidate(candidate)).catch((err) => {
          // A rejected remote candidate is normal — they arrive for transports
          // we have already given up on — and is not worth an error lamp.
          console.warn('[monitor] addIceCandidate rejected:', err);
        });
      });

      sc.on('statusResponse', (status) => {
        if (stale(s)) return;
        const ok = status && (status.success === true || status.success === 'true');
        if (ok) return;
        emitError(
          new MonitorError(
            MonitorErrorCode.SIGNALLING_REJECTED,
            `the signalling service rejected a message: ${
              (status && (status.description || status.errorType || status.statusCode)) || 'no detail'
            }`,
          ),
        );
      });

      sc.on('error', (err) => {
        if (stale(s)) return;
        emitError(toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'signalling socket', err));
        scheduleRestart();
      });

      sc.on('close', () => {
        if (stale(s)) return;
        emitError(
          new MonitorError(
            MonitorErrorCode.SIGNALLING_CLOSED,
            'the signalling socket closed; tearing the monitor down and redoing the chain',
          ),
        );
        scheduleRestart();
      });

      sc.open();
    } catch (err) {
      if (stale(s)) return;
      emitError(toMonitorError(MonitorErrorCode.SIGNALLING_FAILED, 'opening the monitor', err));
      scheduleRestart();
    }
  }

  // Named rather than returned inline so that destroy() can call stop() without
  // depending on `this` — WP-5b is free to destructure the returned object.
  const api = {
    /**
     * start opens the peer connection. Resolves once the first attempt has been
     * launched — not once the picture is up; watch the 'state' event for that.
     * Never rejects: failures arrive on the 'error' event and are retried.
     */
    async start() {
      if (destroyed) {
        throw new MonitorError(MonitorErrorCode.BAD_OPTIONS, 'start(): this monitor was destroyed');
      }
      if (running) return;
      running = true;
      attempt = 0;
      await runAttempt();
    },

    /**
     * stop tears everything down: the signalling socket, the peer connection,
     * every receiver track, the Web Audio graph and the AudioContext. The DOM
     * wrappers and the registered listeners survive, so start() can be called
     * again on the same object.
     */
    async stop() {
      running = false;
      clearRestartTimer();
      // Invalidate every in-flight continuation before tearing down, so nothing
      // that is mid-await can resurrect a peer connection after this returns.
      generation += 1;
      attempt = 0;
      teardownSession();
      if (audio) {
        const a = audio;
        audio = null;
        await a.close();
      }
      view.detach();
      setState('closed');
    },

    /**
     * setReturnMid switches the routed audio bus live. The peer connection is
     * untouched: all seven audio transceivers are already subscribed, so this is
     * a Web Audio source swap and nothing more.
     *
     * @param {number} n one of mids 1..7; see buses.js for the map
     */
    setReturnMid(n) {
      const mid = normaliseReturnMid(n, returnMid);
      if (mid === returnMid) return mid;
      returnMid = mid;
      if (session) routeReturn(session);
      return mid;
    },

    /**
     * setChannelMode picks which SOURCE channel of the selected bus reaches the
     * ears: 'stereo', 'left' or 'right'.
     *
     * Live, and for the same reason setReturnMid is: nothing about the peer
     * connection changes. It rewires two Web Audio nodes that were built on the
     * first routed track and outlive every bus switch.
     *
     * "left" means the LEFT SOURCE CHANNEL IN BOTH EARS, not "sound in the left
     * ear". See channels.js.
     *
     * @param {string} mode
     * @returns {string} the mode in force
     */
    setChannelMode(mode) {
      channelMode = normaliseChannelMode(mode, channelMode);
      if (audio) audio.setChannelMode(channelMode);
      return channelMode;
    },

    /**
     * setAudioEnabled decides whether this path is audible at all.
     *
     * false severs the Web Audio graph from the output element and pauses it.
     * The PROGRAMME PICTURE IS UNAFFECTED: it rides on the same peer connection
     * and keeps coming. Nothing here knows what the alternative audio path is;
     * the page does.
     *
     * @param {boolean} enabled
     * @returns {boolean} the state in force
     */
    setAudioEnabled(enabled) {
      audioEnabled = enabled !== false;
      if (audio) audio.setMuted(!audioEnabled);
      return audioEnabled;
    },

    /**
     * setGainDb sets the absolute make-up gain in dB. Default 18 — see gain.js
     * and docs/test-results.md line 121.
     * @param {number} db
     */
    setGainDb(db) {
      gainDb = clampGainDb(db);
      if (audio) audio.setGainDb(gainDb);
      return gainDb;
    },

    /**
     * setLevel sets the 0..1 user level slider, which multiplies the make-up
     * gain.
     * @param {number} fraction
     */
    setLevel(fraction) {
      level = clampLevel(fraction);
      if (audio) audio.setLevel(level);
      return level;
    },

    /**
     * setSinkId routes the return to a headphone device. Safe to call before
     * start(): the request is remembered and applied as soon as there is a
     * stream to apply it to.
     * @param {string} deviceId '' for the system default
     * @returns {Promise<void>}
     */
    async setSinkId(deviceId) {
      sinkId = typeof deviceId === 'string' ? deviceId : '';
      if (audio) await audio.setSinkId(sinkId);
    },

    /**
     * on registers a listener for 'state' (an RTCPeerConnection connectionState
     * string) or 'error' (an Error). Returns an unsubscribe function.
     * @param {'state'|'error'} event
     * @param {Function} cb
     * @returns {() => void}
     */
    on(event, cb) {
      return emitter.on(event, cb);
    },

    // ---- additive, for spec §7's "export a way to drive the scale factor" ----

    /** setScale sets the CSS scale factor; returns the clamped value used. */
    setScale(k) {
      return view.setScale(k);
    },

    /**
     * fitTo scales the tile into a box and returns the scale used. Pass a width
     * only to fill the window width, per spec §10.
     */
    fitTo(w, h) {
      return view.fitTo(w, h);
    },

    /** getLayout reports {tile, scale, width, height, mosaic} for sizing. */
    getLayout() {
      return view.getLayout();
    },

    /** setTile changes the cropped region live, e.g. from the Settings view. */
    setTile(t) {
      view.setTile(t);
      return view.getLayout();
    },

    /** resumeAudio retries playback after an AUTOPLAY_BLOCKED error. */
    async resumeAudio() {
      if (audio) await audio.resume();
    },

    /** getState is a diagnostics snapshot; the harness renders it. */
    getState() {
      return {
        connectionState: state,
        running,
        destroyed,
        restartAttempt: attempt,
        restartScheduled: restartTimer !== null,
        returnMid,
        returnBus: busForMid(returnMid),
        channelMode,
        audioEnabled,
        gainDb,
        level,
        sinkId,
        tracks: session ? [...session.tracks.keys()].sort((a, b) => a - b) : [],
        credentials: lastCredentialSummary,
        audio: audio ? audio.getState() : null,
        layout: view.getLayout(),
      };
    },

    /**
     * destroy is stop() plus giving WP-5b's <video> element back unwrapped and
     * dropping every registered listener. Only needed if the page tears the
     * monitor down for good; the app never does.
     */
    async destroy() {
      if (destroyed) return;
      await api.stop();
      destroyed = true;
      view.destroy();
      emitter.clear();
    },
  };

  return api;
}
