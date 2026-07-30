/**
 * R4 — the effects return: one KVS audio track through Web Audio to the
 * commentator's headphones.
 *
 * Owner: WP-5a. gain.js holds the gain law; this module owns the graph.
 *
 * The graph is deliberately the shortest one that does the job:
 *
 *   remote track ─► MediaStreamAudioSourceNode ─► GainNode ─► MediaStreamAudioDestinationNode
 *                                                                      │
 *                                                       <audio>.srcObject
 *                                                                      │
 *                                                       setSinkId(headphoneDeviceId)
 *
 * Three things in here are not obvious and each of them fails *silently* — the
 * commentator hears nothing, every lamp is green, and there is nothing in the
 * console. They are the reason this file is as long as it is.
 *
 * 1. THE CHROMIUM REMOTE-TRACK BUG. A MediaStreamAudioSourceNode built from a
 *    WebRTC *remote* stream produces digital silence unless that same stream is
 *    also attached to an HTMLMediaElement that is playing. This is Chromium
 *    issue 121673 and it has been open for years; WebView2 is Chromium, so it
 *    applies to us. The fix is the `keepAlive` element below: a detached, muted
 *    <audio> holding the same stream. It plays nothing anybody can hear; it
 *    exists purely to make the audio engine pull packets.
 *
 * 2. setSinkId BEFORE THERE IS A STREAM. Calling setSinkId on an <audio>
 *    element with no srcObject resolves successfully and then has no effect
 *    once a stream is attached — output goes to the default device. WP-5b will
 *    naturally call setSinkId from the headphone dropdown at any moment,
 *    including before the peer connection is up. So the requested sink is
 *    *remembered* here and re-applied every time the element gets a new stream.
 *
 * 3. AUTOPLAY. spec §7 sets WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS to include
 *    --autoplay-policy=no-user-gesture-required, so in the shipped app play()
 *    will not be blocked. In a plain browser — which is where harness.html
 *    runs, and where an engineer will first try this — it is blocked, play()
 *    rejects with NotAllowedError, and the AudioContext stays 'suspended'. Both
 *    are handled: an AUTOPLAY_BLOCKED error is emitted so the UI can say so, and
 *    a one-shot listener retries on the next click or keypress anywhere in the
 *    document.
 */

import { computeGain, clampGainDb, clampLevel, GAIN_RAMP_SECONDS } from './gain.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';

/** Events that count as a user gesture for the purposes of unblocking audio. */
const GESTURE_EVENTS = Object.freeze(['pointerdown', 'keydown', 'touchend']);

/**
 * createReturnAudio builds the return-audio chain.
 *
 * Nothing is constructed until the first attach(): an AudioContext created
 * before there is anything to play is an AudioContext that spends the whole
 * pre-match period in 'suspended' or, worse, running and burning a core.
 *
 * @param {object} args
 * @param {HTMLAudioElement} args.audioEl the element the return is played through
 * @param {number} args.gainDb make-up gain in dB (default 18, measured)
 * @param {number} [args.level] 0..1 user level; defaults to 1
 * @param {string} [args.sinkId] headphone device id to route to
 * @param {(err: Error) => void} args.onError
 * @returns {ReturnAudio}
 */
export function createReturnAudio({ audioEl, gainDb, level = 1, sinkId = '', onError }) {
  if (!audioEl || typeof audioEl.play !== 'function') {
    throw new MonitorError(
      MonitorErrorCode.BAD_OPTIONS,
      'createReturnAudio: audioEl must be an HTMLAudioElement',
    );
  }

  let currentGainDb = clampGainDb(gainDb);
  let currentLevel = clampLevel(level);
  let requestedSinkId = typeof sinkId === 'string' ? sinkId : '';
  /** The sink actually applied to the element; '' means the system default. */
  let appliedSinkId = null;

  /** @type {AudioContext|null} */
  let ctx = null;
  /** @type {GainNode|null} */
  let gainNode = null;
  /** @type {MediaStreamAudioDestinationNode|null} */
  let dest = null;
  /** @type {MediaStreamAudioSourceNode|null} */
  let source = null;
  /** @type {MediaStream|null} */
  let sourceStream = null;
  /** @type {HTMLAudioElement|null} — see note 1 in the module comment. */
  let keepAlive = null;
  /** @type {((ev: Event) => void)|null} the armed one-shot gesture listener */
  let gestureHandler = null;
  let closed = false;

  const report = (err) => {
    if (onError) onError(err);
  };

  /** ensureGraph builds the AudioContext, gain and destination once. */
  function ensureGraph() {
    if (ctx) return;
    const Ctor = typeof AudioContext !== 'undefined' ? AudioContext : window.webkitAudioContext;
    if (typeof Ctor !== 'function') {
      throw new MonitorError(
        MonitorErrorCode.AUDIO_FAILED,
        'this browser has no Web Audio API; the return cannot be routed',
      );
    }
    // 'interactive' latency hint: the return is a monitor feed the commentator
    // talks over, and the whole path already carries a measured ~489 ms upper
    // bound (docs/test-results.md §2.2). Every millisecond the browser can be
    // persuaded not to add is worth having.
    ctx = new Ctor({ latencyHint: 'interactive' });
    gainNode = ctx.createGain();
    gainNode.gain.value = computeGain(currentGainDb, currentLevel);
    dest = ctx.createMediaStreamDestination();
    gainNode.connect(dest);

    audioEl.srcObject = dest.stream;
    audioEl.autoplay = true;
    audioEl.muted = false;
    audioEl.volume = 1;
  }

  /** disconnectSource tears down just the per-track part of the graph. */
  function disconnectSource() {
    if (source) {
      try {
        source.disconnect();
      } catch {
        /* already disconnected */
      }
      source = null;
    }
    if (keepAlive) {
      try {
        keepAlive.pause();
      } catch {
        /* not playing */
      }
      keepAlive.srcObject = null;
    }
    sourceStream = null;
  }

  /** applyGain ramps rather than steps — see GAIN_RAMP_SECONDS in gain.js. */
  function applyGain() {
    if (!gainNode || !ctx) return;
    const target = computeGain(currentGainDb, currentLevel);
    try {
      gainNode.gain.setTargetAtTime(target, ctx.currentTime, GAIN_RAMP_SECONDS);
    } catch {
      // Some engines reject setTargetAtTime while the context is suspended.
      gainNode.gain.value = target;
    }
  }

  /**
   * applySinkId routes the element to the requested device.
   *
   * Deliberately a no-op when the element has no stream: see note 2 in the
   * module comment. The request is kept and re-applied from attach().
   */
  async function applySinkId(force = false) {
    if (closed) return;
    if (!audioEl.srcObject) return; // remembered, applied on attach
    if (!force && appliedSinkId === requestedSinkId) return;

    if (typeof audioEl.setSinkId !== 'function') {
      // Only worth complaining about if a specific device was asked for; with
      // no request the default device is the right answer anyway.
      if (requestedSinkId) {
        report(
          new MonitorError(
            MonitorErrorCode.SINK_ID_UNSUPPORTED,
            'this browser cannot choose an audio output device (no setSinkId); ' +
              'the return will play on the system default device',
          ),
        );
      }
      appliedSinkId = '';
      return;
    }

    try {
      // '' is the documented way to ask for the default device.
      await audioEl.setSinkId(requestedSinkId || '');
      appliedSinkId = requestedSinkId;
    } catch (err) {
      appliedSinkId = null;
      report(
        toMonitorError(
          MonitorErrorCode.SINK_ID_FAILED,
          `could not route the return to output device "${requestedSinkId}"`,
          err,
        ),
      );
    }
  }

  /**
   * hookGesture installs a one-shot listener that retries playback on the next
   * user interaction. Removed as soon as it fires or the module closes, so a
   * long match does not accumulate listeners.
   */
  function hookGesture() {
    if (gestureHandler || closed || typeof document === 'undefined') return;
    gestureHandler = () => {
      unhookGesture();
      resume().catch(() => {
        /* resume() reports its own failures */
      });
    };
    for (const e of GESTURE_EVENTS) {
      document.addEventListener(e, gestureHandler, { once: true, capture: true, passive: true });
    }
  }

  function unhookGesture() {
    if (!gestureHandler) return;
    if (typeof document !== 'undefined') {
      for (const e of GESTURE_EVENTS) {
        document.removeEventListener(e, gestureHandler, { capture: true });
      }
    }
    gestureHandler = null;
  }

  /**
   * startPlayback resumes the context and starts the element, reporting an
   * AUTOPLAY_BLOCKED error and arming the gesture retry if the browser refuses.
   */
  async function startPlayback() {
    if (closed || !ctx) return;

    let blocked = false;

    if (ctx.state === 'suspended') {
      try {
        await ctx.resume();
      } catch {
        blocked = true;
      }
      if (!closed && ctx.state === 'suspended') blocked = true;
    }

    if (keepAlive) {
      const kp = keepAlive.play();
      if (kp && typeof kp.catch === 'function') {
        await kp.catch((err) => {
          if (err && err.name === 'NotAllowedError') blocked = true;
          // AbortError just means a newer attach superseded this play().
        });
      }
    }

    try {
      const p = audioEl.play();
      if (p && typeof p.then === 'function') await p;
    } catch (err) {
      if (err && err.name === 'NotAllowedError') blocked = true;
      else if (!err || err.name !== 'AbortError') {
        report(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'could not start the return audio', err));
      }
    }

    if (blocked && !closed) {
      report(
        new MonitorError(
          MonitorErrorCode.AUTOPLAY_BLOCKED,
          'the browser blocked the return audio until someone interacts with the window — ' +
            'click anywhere to start it',
        ),
      );
      hookGesture();
    }
  }

  /**
   * resume retries everything that autoplay policy can block. Safe to call at
   * any time; called automatically on the first user gesture after a block.
   * @returns {Promise<void>}
   */
  async function resume() {
    if (closed || !ctx) return;
    await startPlayback();
    await applySinkId(true);
    applyGain();
  }

  /** @typedef {object} ReturnAudio */
  return {
    /**
     * attach routes a single remote audio track into the graph, replacing
     * whatever was routed before.
     *
     * The AudioContext, GainNode, destination and <audio> element are NOT
     * rebuilt: only the source node changes. That is what lets setReturnMid
     * switch buses live without dropping the peer connection, and it is what
     * keeps setSinkId, the level and the element's play state intact across the
     * switch.
     *
     * @param {MediaStreamTrack} track
     */
    async attach(track) {
      if (closed) return;
      if (!track || track.kind !== 'audio') {
        report(
          new MonitorError(
            MonitorErrorCode.AUDIO_FAILED,
            'the monitor was asked to route something that is not an audio track',
          ),
        );
        return;
      }

      try {
        ensureGraph();
      } catch (err) {
        report(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'Web Audio', err));
        return;
      }

      disconnectSource();

      sourceStream = new MediaStream([track]);

      // Note 1 in the module comment: without this the source node yields
      // silence in Chromium. Detached from the document on purpose — it is not
      // part of anybody's layout — and muted so it cannot reach a speaker even
      // if a future Chromium changes the rule.
      if (!keepAlive) {
        keepAlive = document.createElement('audio');
        keepAlive.setAttribute('data-monitor-keepalive', '');
        keepAlive.muted = true;
        keepAlive.defaultMuted = true;
        keepAlive.volume = 0;
        keepAlive.autoplay = true;
      }
      keepAlive.srcObject = sourceStream;

      try {
        source = ctx.createMediaStreamSource(sourceStream);
        source.connect(gainNode);
      } catch (err) {
        report(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'createMediaStreamSource', err));
        return;
      }

      applyGain();
      await startPlayback();
      await applySinkId(true);
    },

    /** detach stops routing audio but keeps the graph and the sink selection. */
    detach() {
      if (closed) return;
      disconnectSource();
    },

    /**
     * setGainDb sets the absolute make-up gain in dB.
     * @param {number} db
     */
    setGainDb(db) {
      currentGainDb = clampGainDb(db);
      applyGain();
      return currentGainDb;
    },

    /**
     * setLevel sets the 0..1 user level, which multiplies the make-up gain.
     * @param {number} fraction
     */
    setLevel(fraction) {
      currentLevel = clampLevel(fraction);
      applyGain();
      return currentLevel;
    },

    /**
     * setSinkId asks for an output device. Remembered and re-applied whenever
     * the element gets a stream — see note 2 in the module comment.
     * @param {string} deviceId '' for the system default
     * @returns {Promise<void>}
     */
    async setSinkId(deviceId) {
      requestedSinkId = typeof deviceId === 'string' ? deviceId : '';
      await applySinkId(true);
    },

    /** resume retries anything autoplay policy blocked. */
    resume,

    /** getState is for the harness and for diagnostics. */
    getState() {
      return {
        contextState: ctx ? ctx.state : 'none',
        gainDb: currentGainDb,
        level: currentLevel,
        linearGain: computeGain(currentGainDb, currentLevel),
        requestedSinkId,
        appliedSinkId,
        routed: !!source,
        sampleRate: ctx ? ctx.sampleRate : 0,
      };
    },

    /**
     * close tears the whole thing down: source, gain, destination, the
     * keep-alive element, the gesture listener and the AudioContext itself.
     *
     * Closing the AudioContext is the important one. Browsers cap the number of
     * AudioContexts per document (Chromium's limit is six) and leaking one per
     * reconnect would, over a match, first exhaust the cap — at which point the
     * return goes silent for good — and then the audio thread. That is the
     * failure this method exists to prevent.
     *
     * @returns {Promise<void>}
     */
    async close() {
      if (closed) return;
      closed = true;
      unhookGesture();
      disconnectSource();
      if (keepAlive) {
        keepAlive.srcObject = null;
        keepAlive = null;
      }
      try {
        audioEl.pause();
      } catch {
        /* not playing */
      }
      audioEl.srcObject = null;
      if (gainNode) {
        try {
          gainNode.disconnect();
        } catch {
          /* already disconnected */
        }
        gainNode = null;
      }
      if (dest) {
        try {
          dest.disconnect();
        } catch {
          /* already disconnected */
        }
        dest = null;
      }
      if (ctx) {
        const c = ctx;
        ctx = null;
        try {
          await c.close();
        } catch {
          /* already closed */
        }
      }
      appliedSinkId = null;
    },
  };
}
