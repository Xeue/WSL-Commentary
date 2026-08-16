/**
 * R4 — the effects return: one KVS audio track through Web Audio to the
 * commentator's headphones.
 *
 * Owner: WP-5a. gain.js holds the gain law; this module owns the graph.
 *
 * The graph is deliberately the shortest one that does the job:
 *
 *   remote track ─► MediaStreamAudioSourceNode
 *                        │
 *                        ▼
 *                  ChannelSplitter(2) ══► ChannelMerger(2) ─► GainNode ─► MediaStreamAudioDestinationNode
 *                                                                                      │
 *                                                                       <audio>.srcObject
 *                                                                                      │
 *                                                                       setSinkId(headphoneDeviceId)
 *
 * THE SPLITTER AND MERGER are the channel selector: Stereo / Left only / Right
 * only. Which splitter output is wired to which merger input is the whole of it,
 * and that table lives in channels.js — read the note there about why "Left
 * only" puts the LEFT SOURCE channel in BOTH ears rather than silencing an ear.
 * They are built once, in ensureGraph, and only the wiring between them changes,
 * so switching channel is as cheap as switching bus and neither touches the peer
 * connection.
 *
 * THE gain ─► dest EDGE is the mute. When the commentator switches the return
 * source to SRT, the browser must go genuinely silent, and it is severed here
 * rather than turned down to zero: a gain of zero is still a running graph
 * pulling packets into a device the native path is also writing to, and one
 * accidental ramp back up is a second copy of the programme at a different
 * offset in somebody's ears mid-match. There is no volume control in this
 * application that can undo a disconnected edge.
 *
 * Four things in here are not obvious and each of them fails *silently* — the
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
 *
 * 4. setSinkId NEEDS A USER GESTURE — on WKWebView, and only there. WebKit
 *    treats choosing an audio output device as something a page may only do
 *    while it has transient activation, so setSinkId rejects with
 *    NotAllowedError from any path that is not directly downstream of a click.
 *    Two of this application's three call sites are exactly that: WP-5b applies
 *    the saved headphone id when the configuration loads and again when a
 *    remote seat pushes a new one (frontend/src/ui/app.js), and neither has a
 *    gesture behind it. Left alone the commentator's chosen headphones are
 *    silently ignored and the return plays on whatever the system default is —
 *    on a commentary position that is usually the laptop speakers, sitting
 *    open in front of a live microphone.
 *
 *    So the gesture listener from note 3 does double duty: a setSinkId refused
 *    for want of activation arms it and is retried on the next interaction. It
 *    is armed by an actual NotAllowedError and nothing else, which is why there
 *    is no platform test anywhere in this file: WebView2 has no such
 *    requirement, never throws it, and therefore never arms anything. The retry
 *    is allowed ONE attempt per requested device — a second NotAllowedError
 *    from inside a genuine gesture is not a missing gesture, it is a refusal,
 *    and it is reported as SINK_ID_FAILED rather than promising a click that
 *    will never help.
 */

import { computeGain, clampGainDb, clampLevel, GAIN_RAMP_SECONDS } from './gain.js';
import { MonitorError, MonitorErrorCode, toMonitorError } from './errors.js';
import { channelRouting, normaliseChannelMode, describeChannelMode } from './channels.js';

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
 * @param {string} [args.channelMode] 'stereo' | 'left' | 'right'; see channels.js
 * @param {boolean} [args.muted] start with the graph severed from the output —
 *        used when the commentator's return source is SRT and this path must
 *        not be audible at all
 * @param {(err: Error) => void} args.onError
 * @returns {ReturnAudio}
 */
export function createReturnAudio({
  audioEl,
  gainDb,
  level = 1,
  sinkId = '',
  channelMode,
  muted = false,
  onError,
}) {
  if (!audioEl || typeof audioEl.play !== 'function') {
    throw new MonitorError(
      MonitorErrorCode.BAD_OPTIONS,
      'createReturnAudio: audioEl must be an HTMLAudioElement',
    );
  }

  let currentGainDb = clampGainDb(gainDb);
  let currentLevel = clampLevel(level);
  let requestedSinkId = typeof sinkId === 'string' ? sinkId : '';
  let currentChannelMode = normaliseChannelMode(channelMode);
  /** True while this path is deliberately severed from the output. */
  let currentMuted = !!muted;
  /** The sink actually applied to the element; '' means the system default. */
  let appliedSinkId = null;
  /** True while a setSinkId is queued behind the gesture listener. See note 4. */
  let sinkAwaitingGesture = false;
  /**
   * True once the gesture retry has been spent on the current requestedSinkId.
   * Reset when a different device is asked for, and when one is successfully
   * applied. Without it a device that will never be permitted would promise
   * "on the next click" on every click, forever, and never say what is wrong.
   */
  let sinkGestureSpent = false;

  /** @type {AudioContext|null} */
  let ctx = null;
  /** @type {ChannelSplitterNode|null} */
  let splitter = null;
  /** @type {ChannelMergerNode|null} */
  let merger = null;
  /** @type {GainNode|null} */
  let gainNode = null;
  /** Whether gainNode is currently connected to dest. See applyMute. */
  let outputConnected = false;
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

    // The channel selector. Two outputs and two inputs is the whole of it; the
    // interesting part is which is wired to which, and that is channels.js.
    //
    // Both nodes are built ONCE and outlive every attach(), so switching bus
    // does not disturb the channel choice and switching channel does not
    // disturb the routed track.
    splitter = ctx.createChannelSplitter(2);
    merger = ctx.createChannelMerger(2);

    gainNode = ctx.createGain();
    gainNode.gain.value = computeGain(currentGainDb, currentLevel);
    merger.connect(gainNode);
    applyChannelRouting();

    dest = ctx.createMediaStreamDestination();
    outputConnected = false;
    applyMute();

    audioEl.srcObject = dest.stream;
    audioEl.autoplay = true;
    // NOTE: audioEl.muted is deliberately NOT the mute. The element's own mute
    // leaves the graph running and is one property assignment away from being
    // undone by anything that touches the element; applyMute severs the edge.
    audioEl.muted = false;
    audioEl.volume = 1;
  }

  /**
   * applyChannelRouting rewires the splitter to the merger for the current mode.
   *
   * splitter.disconnect() with no arguments is safe here and only here: the
   * splitter's ONLY destination is the merger, so there is nothing else to lose.
   * If that ever stops being true this must become the three-argument form.
   */
  function applyChannelRouting() {
    if (!splitter || !merger) return;
    try {
      splitter.disconnect();
    } catch {
      /* nothing connected yet */
    }
    for (const { from, to } of channelRouting(currentChannelMode)) {
      splitter.connect(merger, from, to);
    }
  }

  /**
   * applyMute connects or severs the gain -> destination edge.
   *
   * Severed is genuinely silent: no packets reach the element, and the element
   * is paused as well so the WebView is not holding an output device open that
   * the native SRT path wants. Reconnecting restarts playback, because a paused
   * element does not resume on its own when its stream comes back.
   */
  function applyMute() {
    if (!gainNode || !dest) return;

    if (currentMuted) {
      if (outputConnected) {
        try {
          gainNode.disconnect(dest);
        } catch {
          /* already disconnected */
        }
        outputConnected = false;
      }
      try {
        audioEl.pause();
      } catch {
        /* not playing */
      }
      return;
    }

    if (!outputConnected) {
      try {
        gainNode.connect(dest);
        outputConnected = true;
      } catch (err) {
        report(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'un-muting the return', err));
        return;
      }
    }
    startPlayback().catch(() => {
      /* startPlayback reports its own failures */
    });
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
      sinkAwaitingGesture = false;
      sinkGestureSpent = false;
      return;
    } catch (err) {
      appliedSinkId = null;

      // Note 4: WKWebView refuses this without transient activation. Queue it
      // behind the same one-shot listener autoplay uses. The condition is the
      // error alone — WebView2 does not impose the requirement, so on Windows
      // this branch is unreachable and the Windows behaviour is byte-for-byte
      // what it was.
      //
      // ================= AND IT SAYS NOTHING, DELIBERATELY ==================
      //
      // This branch used to report SINK_ID_DEFERRED: "the browser will not
      // change the audio output device until someone interacts with the window —
      // the chosen headphone output will be applied on the next click." Every
      // word of that is true, and the operator screenshotted it as a thing that
      // alarmed him.
      //
      // It is an explanation of a state that is about to end. hookGesture arms a
      // one-shot listener on the next interaction ANYWHERE in the document —
      // pointerdown, keydown, touchstart — and re-applies the sink from inside
      // it, so in the ordinary case the operator would be reading an alarm about
      // something that fixed itself while they read it. An alert that fires when
      // everything is fine trains people to ignore the surface it appears on,
      // and that cost is paid by the NEXT message on that surface, which is
      // real.
      //
      // What is NOT silenced is the case where the retry also fails: the fall
      // through below reports SINK_ID_FAILED, loudly, because a device that is
      // refused from inside a gesture is a device that will never be permitted
      // and the return really is on the wrong output. sinkGestureSpent is what
      // separates the two, and it is unchanged — one silent retry, then the
      // truth.
      //
      // The residual risk, stated: if nobody ever interacts with the window at
      // all, the return plays on the system default device and nothing says so.
      // It is small and it is bounded — the SAME gesture is what autoplay is
      // waiting on, and a monitor that has never been clicked is a monitor
      // playing nothing at all, which is louder than any banner — and it is the
      // price of not crying wolf on every configuration load.
      //
      // MonitorErrorCode.SINK_ID_DEFERRED is kept in errors.js, and alerts.js
      // still classifies it as a NOTE, so a future caller that chooses to
      // surface it does not have to re-derive how loud it should be.
      if (!closed && err && err.name === 'NotAllowedError' && !sinkGestureSpent) {
        sinkGestureSpent = true;
        sinkAwaitingGesture = true;
        hookGesture();
        return;
      }

      sinkAwaitingGesture = false;
      // The wording names what the commentator will actually experience and what
      // to do about it. "could not route the return to output device
      // {guid}" described the call that failed; this describes the consequence,
      // which is the thing the person reading it has to act on.
      report(
        toMonitorError(
          MonitorErrorCode.SINK_ID_FAILED,
          'the return is playing on the system default output, not the headphones chosen ' +
            `on this screen — the browser refused device "${requestedSinkId}". Choose a ` +
            'different output, or set it in the operating system.',
          err,
        ),
      );
    }
  }

  /**
   * hookGesture installs a one-shot listener that retries, on the next user
   * interaction, everything the browser refused for want of one: playback
   * (note 3) and the output-device choice (note 4). Removed as soon as it fires
   * or the module closes, so a long match does not accumulate listeners.
   *
   * Calling it twice is a no-op rather than a second listener, which matters
   * because both notes can be waiting on the same click.
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

    // THE SINK GOES FIRST WHEN THE SINK IS WHAT IS WAITING. Transient
    // activation is spent, not held: WebKit gives a page roughly five seconds
    // from the gesture, and ctx.resume() and play() are both awaited round
    // trips into the audio engine on a thread we do not control. Retrying the
    // refused setSinkId before spending any of that budget is the difference
    // between the commentator's headphone choice landing on this click and
    // landing on the one after it. When nothing is queued the order is the
    // original one, playback first, because that is the one an AUTOPLAY_BLOCKED
    // banner is asking about.
    const sinkFirst = sinkAwaitingGesture;
    if (sinkFirst) await applySinkId(true);

    // A muted path has nothing to resume. Retrying playback on it would be the
    // one place a "silenced" WebRTC return could start making noise again.
    if (!currentMuted) await startPlayback();
    if (!sinkFirst) await applySinkId(true);
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
        // Into the SPLITTER, not the gain: the channel selector sits between
        // the track and everything else.
        source.connect(splitter);
      } catch (err) {
        report(toMonitorError(MonitorErrorCode.AUDIO_FAILED, 'createMediaStreamSource', err));
        return;
      }

      applyGain();
      // A muted path stays paused. Starting playback here would attach the
      // WebView to the output device the native SRT return is using, for a
      // stream that is severed from the graph and therefore silent anyway.
      if (!currentMuted) await startPlayback();
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
     * setChannelMode picks which SOURCE channel reaches which ear.
     *
     * Live: it rewires the splitter to the merger and touches nothing else — not
     * the AudioContext, not the source node, not the element, not the peer
     * connection. Switching channel is exactly as cheap as switching bus, which
     * is the point: on this event CLN carries effects hard left and comms hard
     * right, so choosing a channel is part of choosing what to listen to.
     *
     * @param {string} mode 'stereo' | 'left' | 'right'
     * @returns {string} the mode actually in force
     */
    setChannelMode(mode) {
      currentChannelMode = normaliseChannelMode(mode, currentChannelMode);
      applyChannelRouting();
      return currentChannelMode;
    },

    /**
     * setMuted severs or restores the gain -> destination edge.
     *
     * This is what the return SOURCE control uses to make sure only one path is
     * audible. Selecting SRT mutes this one; selecting WebRTC un-mutes it. It is
     * not a user volume control and there is no UI for it on its own.
     *
     * @param {boolean} m
     * @returns {boolean} the mute state in force
     */
    setMuted(m) {
      const next = !!m;
      if (next === currentMuted) return currentMuted;
      currentMuted = next;
      applyMute();
      return currentMuted;
    },

    /**
     * setSinkId asks for an output device. Remembered and re-applied whenever
     * the element gets a stream — see note 2 in the module comment.
     * @param {string} deviceId '' for the system default
     * @returns {Promise<void>}
     */
    async setSinkId(deviceId) {
      const next = typeof deviceId === 'string' ? deviceId : '';
      // A NEW device gets a fresh gesture retry. The one-shot budget in note 4
      // exists to stop one un-grantable device nagging forever, not to punish
      // the next choice the commentator makes.
      if (next !== requestedSinkId) sinkGestureSpent = false;
      requestedSinkId = next;
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
        // requestedSinkId set, appliedSinkId null and this true is the WKWebView
        // signature of note 4, and the one thing that tells a support log
        // "the operator picked headphones and nobody has clicked yet" apart from
        // "the device is gone".
        sinkAwaitingGesture,
        routed: !!source,
        channelMode: currentChannelMode,
        channelRouting: describeChannelMode(currentChannelMode),
        muted: currentMuted,
        // The honest one: whether anything can actually reach the element. A
        // support log that says muted:false but outputConnected:false is a bug
        // in applyMute, and the two are printed separately so it is visible.
        outputConnected,
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
      for (const node of [splitter, merger, gainNode]) {
        if (!node) continue;
        try {
          node.disconnect();
        } catch {
          /* already disconnected */
        }
      }
      splitter = null;
      merger = null;
      gainNode = null;
      outputConnected = false;
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
      sinkAwaitingGesture = false;
      sinkGestureSpent = false;
    },
  };
}
