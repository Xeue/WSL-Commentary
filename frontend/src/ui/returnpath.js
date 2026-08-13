/**
 * The return path state machine: which of the two return paths is audible, and
 * every transition between them.
 *
 * Owner: WP-5b.
 *
 * ==================== WHY THIS IS A MODULE AND NOT A HANDLER =================
 *
 * `deriveReturnSourceEffects` (returnsource.js) says what a selection MEANS —
 * that exactly one of webrtcAudible and srtRunning is ever true. That is a pure
 * function over a string and it is easy to keep correct.
 *
 * The hard part is the other half: DRIVING two pieces of software to that state
 * across a sequence of awaits, any of which can fail, while a second transition
 * may be trying to start. That half used to live in app.js as three separate
 * async handlers, each with its own recovery, and the recoveries did not agree:
 *
 *   - Startup and the source switch both assumed that a throw meant the SRT
 *     monitor had not started, and re-enabled WebRTC WITHOUT stopping SRT. That
 *     is false for every throw raised after StartReturn resolves, and it is
 *     false for the one refusal that means the opposite — errReturnAlreadyRunning,
 *     which says a monitor IS running. The result was both paths in the
 *     commentator's ears, a few hundred milliseconds apart, with a banner
 *     announcing the WebRTC return as if the other one had stopped.
 *
 *   - The channel and headphone changes stopped the SRT return and started it
 *     again behind a single .catch that only showed a banner. A start that
 *     failed after the stop succeeded left NO audio at all: SRT stopped, WebRTC
 *     still muted, and the selected path still reading "srt".
 *
 * So the transitions are here, in one object, with one rule enforced
 * structurally rather than by review:
 *
 *   ===========================================================
 *   THE ONLY ROUTE TO setWebRTCAudible(true) IS goWebRTC(),
 *   AND goWebRTC() AWAITS stopReturn() FIRST. ALWAYS.
 *   ===========================================================
 *
 * Not "when we think SRT is running" — always. The belief can be wrong in both
 * directions: a monitor left running by a page that reloaded before its
 * beforeunload stopReturn() completed, or a start that threw after the Go side
 * had already begun. Being wrong in the direction of "stop something that was
 * not running" costs one rejected IPC that this module swallows. Being wrong in
 * the other direction is the failure above.
 *
 * ============================ THE TWO PROPERTIES ============================
 *
 * SAFETY   Both paths are never audible at once. Enforced by the rule above and
 *          by starting SRT only from a state where WebRTC has been silenced.
 *
 * LIVENESS Once an operation has finished, SOMETHING is audible. Silence is an
 *          acceptable state DURING a transition — silencing the outgoing path
 *          before starting the incoming one is deliberate, because an overlap is
 *          worse than a gap — but it is never where an operation is allowed to
 *          stop. Every failure path ends at WebRTC, which needs no backend at
 *          all, and says so.
 *
 * Both are tested exhaustively in returnpath.test.js against an io double that
 * models the two paths independently of this module's own bookkeeping, over
 * every combination of which call fails and how.
 *
 * ======================= WHAT THIS MODULE DOES NOT KNOW =====================
 *
 * It has no imports from backend.js, no DOM, and no configuration. Everything
 * effectful arrives as `io`, which is what lets the properties above be tested
 * as properties rather than as a handful of examples. app.js owns the wiring.
 */

import {
  RETURN_SOURCE_WEBRTC,
  RETURN_SOURCE_SRT,
  DEFAULT_RETURN_SOURCE,
  normaliseReturnSource,
} from './returnsource.js';

/** What is currently making sound. AUDIBLE_BOTH must never be observed. */
export const AUDIBLE_WEBRTC = 'webrtc';
export const AUDIBLE_SRT = 'srt';
export const AUDIBLE_NONE = 'none';
export const AUDIBLE_BOTH = 'both';

/**
 * @typedef {object} ReturnPathIO
 * @property {() => Promise<void>} startReturn      App.StartReturn. Reads the SAVED config.
 * @property {() => Promise<void>} stopReturn       App.StopReturn. Rejects when nothing runs.
 * @property {(on: boolean) => void} setWebRTCAudible  Must not throw; see safeMonitorCall.
 * @property {(source: string) => Promise<void>} saveSource  Persist returnSource and wait.
 * @property {(err: unknown) => boolean} [isAlreadyRunning]  errReturnAlreadyRunning?
 * @property {(err: unknown) => boolean} [isNotRunning]      errReturnNotRunning?
 * @property {(message: string) => void} [showError] The operator-visible banner.
 * @property {(message: string) => void} [log]       Console only.
 * @property {(source: string) => void} [onSource]   Called whenever the selection changes.
 */

/**
 * createReturnPath builds the state machine. It starts believing WebRTC is
 * audible and nothing is running, which is what a freshly loaded page looks like
 * before adoptSaved has been called.
 *
 * @param {ReturnPathIO} io
 */
export function createReturnPath(io) {
  const isAlreadyRunning = io.isAlreadyRunning || (() => false);
  const isNotRunning = io.isNotRunning || (() => false);
  const showError = io.showError || (() => {});
  const log = io.log || (() => {});
  const onSource = io.onSource || (() => {});

  let source = DEFAULT_RETURN_SOURCE;
  let webrtcAudible = true;
  let srtRunning = false;
  let busy = false;

  function audible() {
    if (webrtcAudible && srtRunning) return AUDIBLE_BOTH;
    if (webrtcAudible) return AUDIBLE_WEBRTC;
    if (srtRunning) return AUDIBLE_SRT;
    return AUDIBLE_NONE;
  }

  function message(err) {
    if (err == null) return 'unknown error';
    return String(err.message ?? err);
  }

  function setSource(next) {
    source = normaliseReturnSource(next);
    onSource(source);
  }

  // -------------------------------------------------------------------------
  // The two primitives. Everything else is built from these.
  // -------------------------------------------------------------------------

  /**
   * muteWebRTC severs the Web Audio graph. The picture is untouched — the video
   * rides the same peer connection whichever audio path is selected.
   */
  function muteWebRTC() {
    io.setWebRTCAudible(false);
    webrtcAudible = false;
  }

  /**
   * stopSRT stops the Go-side monitor and NEVER throws.
   *
   * Every way it can reject leaves the Go side without a running monitor:
   *
   *   errReturnNotRunning  the normal case — there was nothing to stop.
   *   BindingMissingError  this build has no StopReturn, so it has no
   *                        StartReturn either and nothing here started one.
   *   anything else        App.StopReturn clears its session BEFORE it can fail
   *                        (app_return.go sets a.ret = nil and only then calls
   *                        mon.Stop()), so a reported failure means the pipeline
   *                        did not reach NULL cleanly, not that a monitor is
   *                        still being managed. That last case is the only one
   *                        where audio could conceivably survive the call, it is
   *                        shown to the operator, and the alternative — refusing
   *                        to ever re-enable WebRTC — is a commentator left in
   *                        silence for certain rather than in overlap by chance.
   */
  async function stopSRT() {
    try {
      await io.stopReturn();
    } catch (err) {
      if (isNotRunning(err) || err?.name === 'BindingMissingError') {
        log(`wslcomms: nothing to stop on the SRT return: ${message(err)}`);
      } else {
        showError(
          `The SRT return did not stop cleanly: ${message(err)}. ` +
            'If you can hear the match twice, restart the application.',
        );
      }
    }
    srtRunning = false;
  }

  /**
   * goWebRTC is THE ONLY WAY WebRTC is ever made audible, and it stops the SRT
   * return first, unconditionally. Do not add a second caller of
   * io.setWebRTCAudible(true); that is the whole safety argument.
   */
  async function goWebRTC() {
    await stopSRT();
    io.setWebRTCAudible(true);
    webrtcAudible = true;
  }

  /**
   * startSRT starts the Go-side monitor, silencing WebRTC first, and treats
   * errReturnAlreadyRunning as stop-then-start rather than as a failure.
   *
   * The already-running case is not exotic. beforeunload fires stopReturn()
   * fire-and-forget and the page dies before the IPC lands, so a WebView reload
   * mid-match leaves a monitor up. Restarting it — rather than adopting it — is
   * deliberate: StartReturn reads host, port, latency, channel and endpoint out
   * of the saved config, and the running one was built from whatever was saved
   * when the previous page started it. Adopting would leave the pipeline
   * disagreeing with every control on screen, which is finding 2 by another
   * route.
   *
   * It throws if the return will not start. The caller decides what to do about
   * that; every caller here ends up at goWebRTC.
   */
  async function startSRT() {
    muteWebRTC();
    try {
      await io.startReturn();
    } catch (err) {
      if (!isAlreadyRunning(err)) throw err;
      log('wslcomms: a return monitor was already running; restarting it against the saved configuration');
      await stopSRT();
      await io.startReturn();
    }
    srtRunning = true;
  }

  /**
   * fallBackToWebRTC is the end of every failure path: audible on WebRTC, the
   * selection updated to match, and the saved config agreeing with both so that
   * the next page load does not try the failed path again on the operator's
   * behalf.
   */
  async function fallBackToWebRTC() {
    await goWebRTC();
    setSource(RETURN_SOURCE_WEBRTC);
    try {
      await io.saveSource(RETURN_SOURCE_WEBRTC);
    } catch (err) {
      // The audio is already right; only the record of it is not. Worth a line
      // in the console and not worth a second banner on top of the one that
      // brought us here.
      log(`wslcomms: could not save the fallback to the WebRTC return: ${message(err)}`);
    }
  }

  // -------------------------------------------------------------------------
  // The three transitions
  // -------------------------------------------------------------------------

  /**
   * adoptSaved brings the saved selection into force at startup.
   *
   * It routes the WebRTC case through goWebRTC rather than doing nothing,
   * because "the saved path is WebRTC" does not prove nothing is running: a page
   * that reloaded after a failed switch can find an orphaned Go-side monitor
   * playing into the same headphones. The cost of asking is one StopReturn that
   * usually rejects with errReturnNotRunning; the cost of assuming is a
   * commentator hearing the match twice from the moment the page loads.
   *
   * @param {string} saved  the returnSource read from the config
   * @returns {Promise<{applied: boolean, source: string}>}
   */
  async function adoptSaved(saved) {
    const next = normaliseReturnSource(saved);
    if (busy) return { applied: false, source };
    busy = true;
    try {
      setSource(next);
      if (next !== RETURN_SOURCE_SRT) {
        await goWebRTC();
        return { applied: true, source };
      }
      try {
        await startSRT();
        return { applied: true, source };
      } catch (err) {
        showError(
          `The SRT return did not start: ${message(err)}. Falling back to the WebRTC return.`,
        );
        await fallBackToWebRTC();
        return { applied: false, source };
      }
    } finally {
      busy = false;
    }
  }

  /**
   * select switches the audible path.
   *
   * SILENCE THE OUTGOING PATH FIRST, THEN START THE INCOMING ONE. Never the
   * other way round: both paths carry the same programme at different offsets,
   * so even a few hundred milliseconds of overlap is a slapback echo in the ears
   * of somebody talking over it. Failing to start the new path leaves silence,
   * which is recoverable and visible; starting it before the old one stops is
   * not.
   *
   * @param {string} next
   * @returns {Promise<{applied: boolean, source: string, reason?: string}>}
   */
  async function select(next) {
    const wanted = normaliseReturnSource(next);
    if (wanted === source) return { applied: true, source };
    if (busy) {
      return {
        applied: false,
        source,
        reason: 'the return is still changing — try again in a moment',
      };
    }
    busy = true;
    const previous = source;
    try {
      if (wanted === RETURN_SOURCE_SRT) {
        // Silence the outgoing path first, and before the save, so that a save
        // which hangs cannot leave both paths live while it does.
        muteWebRTC();
        // Then save, and only then start: StartReturn takes no arguments and
        // reads returnSource, host, port, latency, channel and the endpoint out
        // of the saved configuration. It also REFUSES unless returnSource is
        // already "srt", so this save is a precondition and not bookkeeping.
        await io.saveSource(wanted);
        setSource(wanted);
        await startSRT();
      } else {
        // Stopping SRT is inside goWebRTC, before the un-mute, so nothing here
        // can strand the commentator with both paths up.
        await goWebRTC();
        setSource(wanted);
        // The save comes LAST on this direction and its failure is NOT a reason
        // to bounce back to SRT. Nothing on the WebRTC path reads the saved
        // config — the operator asked for WebRTC and is now on it, so a disk
        // failure must not override that. It is reported, because the next
        // launch will start on the path that is still written down.
        try {
          await io.saveSource(wanted);
        } catch (err) {
          showError(
            `Switched the return to WEBRTC, but could not save the choice: ${message(err)}. ` +
              'It will not survive a restart.',
          );
        }
      }
      return { applied: true, source };
    } catch (err) {
      showError(
        `Could not switch the return to ${wanted.toUpperCase()}: ${message(err)}. ` +
          `Staying on ${previous.toUpperCase()}.`,
      );
      await restore(previous);
      return { applied: false, source };
    } finally {
      busy = false;
    }
  }

  /**
   * restore puts the previous path back after a failed switch, and falls all the
   * way back to WebRTC if even that fails. It never leaves the selection
   * claiming a path that is not the one making sound — the banner and the status
   * line are read by somebody deciding whether their headphones are broken.
   */
  async function restore(previous) {
    if (previous === RETURN_SOURCE_SRT) {
      try {
        setSource(RETURN_SOURCE_SRT);
        await io.saveSource(RETURN_SOURCE_SRT);
        await startSRT();
        return;
      } catch (err) {
        log(`wslcomms: could not restore the SRT return: ${message(err)}`);
      }
    }
    await fallBackToWebRTC();
  }

  /**
   * applyOption saves a setting and, when the SRT return is the running path,
   * rebuilds it so the setting actually takes effect.
   *
   * The rebuild exists because gst.ReturnOpts is read ONCE, in Play. The channel
   * is a mix matrix baked into the pipeline when it is built and the headphone
   * endpoint is fixed when wasapi2sink is created, so neither has a live setter
   * and both mean a stop and a start. `save` runs first and is awaited, because
   * StartReturn takes no arguments and reads the saved configuration — a start
   * that races its own save comes back up on the previous settings and reports
   * success, which is the worst kind of success.
   *
   * On the WebRTC path it only saves: the browser's controls are live and the
   * caller has already applied the change to the Web Audio graph.
   *
   * It takes the same in-flight guard as select, so a channel change, a
   * headphone change and a source switch cannot interleave with each other. They
   * are three sequences that all stop and start the same monitor, and two of
   * them running at once is how a stop from one lands between the other's stop
   * and start.
   *
   * @param {{what: string, save: () => Promise<void>}} opts
   * @returns {Promise<{applied: boolean, source: string, reason?: string}>}
   */
  async function applyOption({ what, save }) {
    if (busy) {
      return {
        applied: false,
        source,
        reason: 'the return is still changing — try again in a moment',
      };
    }
    busy = true;
    try {
      if (source !== RETURN_SOURCE_SRT) {
        try {
          await save();
          return { applied: true, source };
        } catch (err) {
          showError(`Could not save the ${what}: ${message(err)}`);
          return { applied: false, source };
        }
      }
      return await rebuildSRT(what, save);
    } finally {
      busy = false;
    }
  }

  /** rebuildSRT is applyOption's SRT half. busy is already held. */
  async function rebuildSRT(what, save) {
    try {
      await save();
      await stopSRT();
      await startSRT();
      return { applied: true, source };
    } catch (err) {
      // THE FAILURE THIS BRANCH EXISTS FOR: the stop succeeded and the start did
      // not. Without a fallback the commentator has no audio at all — SRT
      // stopped, WebRTC still muted, and the control still reading "SRT".
      showError(
        `Could not apply the ${what} to the SRT return: ${message(err)}. ` +
          'Falling back to the WebRTC return so you are not left with silence.',
      );
      await fallBackToWebRTC();
      return { applied: false, source };
    }
  }

  return {
    get source() {
      return source;
    },
    get audible() {
      return audible();
    },
    get busy() {
      return busy;
    },
    adoptSaved,
    select,
    applyOption,
  };
}

/**
 * RETURN_OPTS_CONFIG_KEYS is every configuration field gst.ReturnOpts is built
 * from — that is, every field whose change requires a rebuild rather than
 * merely a save.
 *
 * It mirrors app_return.go's returnOpts, and returnpath.test.js asserts that it
 * still does by reading that function. The mirror is the point: a Settings save
 * that changes one of these while the SRT return is running leaves a pipeline
 * disagreeing with every control on screen, and the disagreement is silent —
 * "Left only" saved, acknowledged, shown, and comms still in the right ear.
 *
 * The SRT RETURN PASSPHRASE is deliberately absent. It is not in the config; it
 * comes from Credential Manager through App.SetSecret, and this module cannot
 * see it change. Changing the passphrase mid-match therefore still needs a
 * manual source switch, which is a real gap and is stated here rather than
 * papered over. The key LENGTH beside it is not a secret, is in the config, and
 * IS here — so an operator who switches the return between an encrypted and an
 * unencrypted M2L-X output gets the rebuild that change needs.
 *
 * `pbkeylen` — the SEND path's key length — is deliberately NOT here any more.
 * app_return.go used to read it and now reads `srtReturnPBKeyLen` instead,
 * because encryption on M2L-X is a per-endpoint setting and the feed's input
 * and the monitor's output disagree about it on the measured instance. Leaving
 * it in this list would stop and rebuild a perfectly good monitor every time
 * somebody adjusted the contribution feed's encryption.
 */
export const RETURN_OPTS_CONFIG_KEYS = Object.freeze([
  'm2lxHost', // the SRT host is always derived from it — EffectiveSRTHost
  'srtReturnPort',
  'srtLatencyMs',
  'srtReturnPBKeyLen',
  'returnChannel',
  'headphoneEndpointId',
]);

/**
 * returnOptsFingerprint reduces a configuration to the part the running SRT
 * pipeline was built from. Two configs with the same fingerprint need no
 * rebuild; two with different ones do.
 *
 * It is a string rather than a deep compare so that the caller can hold one
 * across an await without holding a reference to a config object that may have
 * been replaced underneath it.
 *
 * @param {Record<string, unknown> | null | undefined} config
 * @returns {string}
 */
export function returnOptsFingerprint(config) {
  if (!config) return '';
  return RETURN_OPTS_CONFIG_KEYS.map((key) => `${key}=${String(config[key] ?? '')}`).join(' ');
}

export { RETURN_SOURCE_WEBRTC, RETURN_SOURCE_SRT };
