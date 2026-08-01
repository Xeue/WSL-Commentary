import * as backend from './backend.js';

// The mixer drawer. Imported STATICALLY, not behind a dynamic import, for two
// reasons: it must be in the production bundle unconditionally — nothing
// imported it before this line existed, so none of it shipped — and this is the
// import that pulls frontend/src/ui/mixer/mixer.css into the stylesheet, since
// index.js is the only module allowed to import it.
import { createMixerDrawer } from './mixer/index.js';

/**
 * The host side of the mixer drawer: construction, the 1 Hz refresh, and the
 * mirror of the drawer's write permission onto the Go arm gate.
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THIS IS ITS OWN MODULE =========================
 *
 * It used to live inside settings.js, because the drawer used to live behind
 * Settings. It does not any more: the operator asked for it beside Settings on
 * the MAIN screen, so that an engineer setting the desk up does not have to go
 * through a configuration form to see what is in the clean feed.
 *
 * That move is why this is a module and not a few more functions in home.js.
 * home.js builds DOM and exposes setters and knows nothing about backend.js;
 * app.js does the wiring. Putting the mixer's polling and arm-mirroring into
 * home.js would have made the commentary screen the one file that talks to the
 * write path.
 *
 * ======================= AND WHAT MOVING IT DOES NOT CHANGE =================
 *
 * COMMENTATORS SEE THIS BUTTON NOW. Everything that makes that safe is in the
 * drawer and in internal/mixer, and none of it was relaxed to make the move:
 * the drawer opens READ-ONLY every time, closing locks it again, and both this
 * gate and the Go one have to be open before a single byte reaches the desk.
 */

/**
 * MIXER_POLL_MS is how often the host re-reads the mixer while the drawer is
 * open, and it is a cost, not a preference.
 *
 * The switcher_status socket is snapshot-then-delta: a COMPLETE document
 * arrives exactly once per connection, and it is not yet known whether a
 * routing change is pushed as a whole-node state or as a subtree delta
 * (internal/m2lx/wire.go). So there is nothing to subscribe to, and every
 * refresh is App.GetMixerSnapshot opening a fresh connection. That is
 * deliberate — a cached frame would make an old matrix look live, and
 * set_routing REPLACES a strip's whole bus set from whatever matrix the
 * drawer is holding.
 *
 * One second is the rate the drawer's contract names, and it leaves margin
 * against model.js's STALE_AFTER_MS (3.5 s): one missed poll still leaves the
 * view live. It only runs while the drawer is OPEN.
 */
const MIXER_POLL_MS = 1000;

/**
 * createMixerHost owns the drawer and everything behind it.
 *
 * opts = {
 *   mount        the element the drawer renders into. It must NOT be inside a
 *                view the application hides: the drawer paints as a fixed
 *                full-window overlay, but a hidden ancestor takes it with it.
 *   onStatus(message, isError)   optional. Where the host's own failures — not
 *                the drawer's internal ones — are shown. Called with ('', false)
 *                to clear.
 * }
 *
 * Returns { toggle(), open(), close(), isOpen() }.
 */
export function createMixerHost(opts) {
  const o = opts || {};
  const mount = o.mount;
  if (!mount || typeof mount.appendChild !== 'function') {
    throw new Error('wslcomms: createMixerHost needs opts.mount');
  }
  const report = typeof o.onStatus === 'function' ? o.onStatus : () => {};

  let mixerDrawer = null;
  let mixerPollTimer = null;
  let mixerPollInFlight = false;

  function setMixerStatus(message, isError) {
    report(message || '', !!isError);
  }

  /**
   * syncMixerArm mirrors the drawer's write permission onto the Go arm gate.
   *
   * THE TWO GATES ARE INDEPENDENT AND BOTH MUST BE OPEN. The drawer's own gate
   * (createWriteGate in mixer/model.js) is the outer layer; App.ArmMixer opens
   * the inner one, which is where mixer.Controller.Send refuses with
   * ErrDisarmed. This function does not decide anything — it forwards a gesture
   * the operator already made inside the drawer.
   *
   * If arming the Go side FAILS, the drawer is disarmed again rather than left
   * looking armed. A drawer that says WRITES LIVE while the write path is shut
   * sends the operator to a crosspoint to find out — and now that a crosspoint
   * click IS the write, that is a click they believe went through.
   */
  async function syncMixerArm(armed) {
    let failed = false;
    try {
      if (armed) {
        const state = await backend.armMixer();
        const until = state && state.armedUntil ? new Date(state.armedUntil) : null;
        const when = until && !Number.isNaN(until.getTime()) ? until.toLocaleTimeString() : 'shortly';
        setMixerStatus(
          `Write path armed in the application too; the window closes at ${when} on its own. ` +
            'Arming changes nothing on the mixer by itself.',
          false,
        );
      } else {
        await backend.disarmMixer();
        setMixerStatus('Write path locked. The application has released the control socket.', false);
      }
    } catch (err) {
      failed = true;
      console.error('wslcomms: mixer arm/disarm failed', err);
      setMixerStatus(
        `${armed ? 'Could not arm' : 'Could not disarm'} the application's write path: ${err?.message || err}`,
        true,
      );
    }
    // Outside the try, and after the await, so that the resulting
    // onArmedChange(false) runs a clean disarm rather than re-entering this
    // call. setArmed is a no-op when the state already matches.
    if (failed && armed && mixerDrawer) mixerDrawer.setArmed(false);
  }

  function ensureMixerDrawer() {
    if (mixerDrawer) return mixerDrawer;
    mixerDrawer = createMixerDrawer({
      // Written long-hand, not as the shorthand `mount,`: mixerwiring.test.js
      // checks that every required option is supplied by reading this file, and
      // it can only see the ones that are named.
      mount: mount,
      // Read-only, and never cached on the Go side: see App.GetMixerSnapshot,
      // which opens a fresh status connection for every call because that is
      // the only way the protocol yields a complete, current document. The
      // drawer calls this again on EVERY crosspoint click, before it writes.
      getSnapshot: () => backend.getMixerSnapshot(),
      // The single write path. It reaches the mixer only through the armed
      // mixer.Controller; there is deliberately no second binding that could
      // write without the gate.
      sendCommands: (cmds) => backend.sendMixerCommands(cmds),
      // Still supplied, still bound, still required by the contract — and not
      // called by this build of the drawer, whose drift panel has been
      // withdrawn from the GUI. internal/mixer/golden.go and Compare are
      // untouched; restoring the panel is a change to drawer.js alone.
      getGolden: () => backend.getMixerGolden(),
      setGolden: (snapshot) => backend.setMixerGolden(snapshot),
      onArmedChange: (armed) => {
        void syncMixerArm(armed);
      },
      onError: (err, context) => {
        console.error(`wslcomms: ${context}`, err);
        const message = String(err?.message || err);
        // The Go arm window closes on its own after mixer.ArmWindow, without
        // anybody calling Disarm — that is the point of it. When it does, the
        // drawer is still showing WRITES LIVE, because the drawer's own gate is
        // a separate gate and IS still open. Saying so is better than leaving
        // the operator to read "locked" off a drawer that says live.
        if (message.includes('disarmed')) {
          setMixerStatus(
            "Nothing was written: the application's own write window has closed. It closes by " +
              'itself a couple of minutes after arming, so that a drawer left armed cannot reach ' +
              'the desk later. Press "Allow changes" to shut the drawer gate and press it again to reopen both.',
            true,
          );
          return;
        }
        setMixerStatus(`${context}: ${message}`, true);
      },
    });
    return mixerDrawer;
  }

  /**
   * pollMixer feeds the drawer one fresh snapshot.
   *
   * A failure is NOT swallowed and nothing stale is re-fed: the drawer simply
   * stops receiving update(), declares its view STALE after STALE_AFTER_MS and
   * refuses every crosspoint click. That is the correct behaviour for a mixer
   * that cannot presently be read, and it is reached honestly rather than
   * simulated.
   */
  async function pollMixer() {
    if (!mixerDrawer || mixerPollInFlight) return;
    if (!mixerDrawer.isOpen()) {
      // The operator closed the drawer from inside it — its Close button, the
      // scrim or Escape — none of which the host is told about directly.
      stopMixerPolling();
      return;
    }
    mixerPollInFlight = true;
    try {
      const snapshot = await backend.getMixerSnapshot();
      if (mixerDrawer) mixerDrawer.update(snapshot);
      setMixerStatus('', false);
    } catch (err) {
      console.error('wslcomms: could not read the mixer', err);
      setMixerStatus(
        `The mixer could not be read: ${err?.message || err}. The drawer will show its view as ` +
          'STALE and refuse to write until it can be read again.',
        true,
      );
    } finally {
      mixerPollInFlight = false;
    }
  }

  function startMixerPolling() {
    if (mixerPollTimer !== null) return;
    // No immediate call: drawer.open() reads a snapshot itself, and a second
    // read in the same tick would be a second connection for nothing.
    mixerPollTimer = setInterval(() => {
      void pollMixer();
    }, MIXER_POLL_MS);
  }

  function stopMixerPolling() {
    if (mixerPollTimer === null) return;
    clearInterval(mixerPollTimer);
    mixerPollTimer = null;
  }

  /**
   * closeMixer shuts the drawer and everything behind it.
   *
   * The drawer locks itself on close and tells us through onArmedChange, so the
   * Go-side window and the control socket are released by that path rather than
   * by a second, separate call here.
   */
  function closeMixer() {
    stopMixerPolling();
    if (mixerDrawer && mixerDrawer.isOpen()) mixerDrawer.close();
    setMixerStatus('', false);
  }

  function openMixer() {
    const drawer = ensureMixerDrawer();
    if (drawer.isOpen()) return;
    drawer.open();
    startMixerPolling();
  }

  return {
    open: openMixer,
    close: closeMixer,
    isOpen: () => !!mixerDrawer && mixerDrawer.isOpen(),
    toggle() {
      const drawer = ensureMixerDrawer();
      if (drawer.isOpen()) closeMixer();
      else openMixer();
    },
  };
}
