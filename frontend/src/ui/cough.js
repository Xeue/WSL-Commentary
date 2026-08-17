/**
 * The cough mute: two behaviours, ONE truth about whether audio is going out.
 *
 * Owner: WP-5b.
 *
 * The operator: "I also want a cough mute buttons, with a push to mute style and
 * a latch mute mode."
 *
 * ===================== WHY THIS IS A MODEL AND NOT A BUTTON =================
 *
 * This is the control on the screen whose state being MISREAD puts a cough on
 * air or leaves a commentator talking into a dead microphone. Both failures are
 * silent at this desk — nothing on this machine can hear the broadcast output —
 * so the only defence is that the thing on the screen is derived from one
 * variable and that variable is the one the send path was actually told about.
 *
 * So: nothing here touches the DOM, nothing here knows what a button is, and
 * everything the screen says is the return value of describeMute() applied to
 * this module's state. home.js draws it; app.js supplies `apply`, which is the
 * Go call. There is no second place where "muted" is decided.
 *
 * ==================== THE MUTE IS AT THE SEND PATH, ALWAYS ==================
 *
 * `apply` MUST be the binding that mutes the commentary going to the switcher.
 * Muting a monitor element, a Web Audio gain, or the return path is not a cough
 * mute: it makes this desk quieter while the cough goes to air, which is the
 * exact failure inverted and with a green light on it. This module cannot
 * enforce that — it calls what it is given — so the enforcement is a test that
 * reads app.js and refuses to find a monitor mute behind this handler.
 *
 * ========================== THE TWO BEHAVIOURS ==============================
 *
 * They are NOT a mode switch. Both are live at once, and the muted state is
 * their OR:
 *
 *   held     true while the push control is down (pointer or key)
 *   latched  toggled by the latch control
 *   muted    held || latched
 *
 * A mode switch was the other option and it is worse in the one moment that
 * matters: a commentator who has latched the mute and then instinctively holds
 * the push key does not want the hold to UNDO the latch, and a commentator who
 * is holding the push key does not want a mis-pressed mode switch to strand them
 * muted. With an OR there is no ordering that produces a surprise, and the
 * readout names WHICH of the two is holding it — "MUTED — LATCHED" reads
 * differently from "MUTED — HOLDING", and only the first survives letting go.
 *
 * ======================== FAIL-SAFE ON THE HOLD =============================
 *
 * A hold that is never released is a dead microphone for the rest of the match,
 * and the release is the half of a push-to-mute that a window can miss: the
 * pointer leaves the button, the window loses focus, the operator alt-tabs with
 * the key down and the keyup is delivered somewhere else. So release() is
 * idempotent and the caller is expected to call it from blur and
 * visibilitychange as well as from pointerup/keyup. The LATCH is deliberately
 * NOT released by any of those: it is a deliberate, persistent choice, and
 * dropping it because a window lost focus would put a live microphone up without
 * anybody asking for one.
 *
 * ===================== INTENT, IN FLIGHT, AND CONFIRMED =====================
 *
 * `apply` is an IPC round trip and it can fail. Three variables, not one:
 *
 *   intended   what the controls say (held || latched) — instant
 *   confirmed  what Go last acknowledged — authoritative
 *   failure    the last apply that was refused, if it has not been superseded
 *
 * The readout shows the INTENT while a call is in flight, because a
 * push-to-mute that waits for a round trip before it looks muted is a control
 * nobody will trust and everybody will press twice — but it says so ("MUTING…")
 * rather than claiming the mute has landed. A failure is its own state and it is
 * RED and loud: "MUTE FAILED" over a live microphone is the truth, and the one
 * thing this control may never do is show a calm "MUTED" while audio is going
 * out.
 *
 * Calls are serialised and LATEST-WINS. A push and its release inside one round
 * trip must not leave Go holding the mute: while a call is in flight the newest
 * intent is remembered, and re-issued when the flight lands, until what Go has
 * been told matches what the controls say.
 */

/**
 * MUTE_KEY_PUSH is the KeyboardEvent.code held down to mute.
 *
 * Space, and the choice is not free. It has to be operable without looking,
 * which rules out anything the hand has to find: a commentator's hands are on a
 * desk in a dim box with their eyes on the pitch. Space is the largest key on
 * the board and the only one that can be found by feel with certainty.
 *
 * What it costs: Space is also "activate the focused control" and "scroll". Both
 * are paid for at the listener rather than by choosing a worse key — the handler
 * takes the event before the default, and refuses to act when the operator is
 * typing (see isTypingTarget). A key that needs a glance to find is not a
 * push-to-mute.
 */
export const MUTE_KEY_PUSH = 'Space';

/**
 * MUTE_KEY_LATCH is the KeyboardEvent.code that toggles the latch.
 *
 * M for mute. It is a TOGGLE, so it does not need to be findable by feel under
 * pressure the way the push key does — it is pressed once, deliberately — and a
 * letter key cannot be hit by the heel of a hand resting on the desk, which is
 * exactly the accident a second large key would invite.
 */
export const MUTE_KEY_LATCH = 'KeyM';

/**
 * KEY_CAPS is what each binding is CALLED on the screen. The bound key has to be
 * obvious, which means printed on the control it drives and not in a tooltip:
 * a keyboard shortcut nobody can see is a keyboard shortcut nobody uses, and
 * this one exists precisely for the moment when looking at the screen is what
 * the operator cannot do.
 */
export const KEY_CAPS = Object.freeze({
  [MUTE_KEY_PUSH]: 'Space',
  [MUTE_KEY_LATCH]: 'M',
});

/** describeMuteKey names a bound key for the screen. */
export function describeMuteKey(code) {
  return KEY_CAPS[code] || String(code || '');
}

/**
 * isTypingTarget reports whether a key event came from somewhere a keystroke
 * means a character rather than a command.
 *
 * Without it, Space in the Settings form's SRT passphrase field mutes the
 * commentary — and worse, the keyup that unmutes it arrives only if the field
 * still has focus. Text inputs, textareas, selects and anything contenteditable
 * are excluded.
 *
 * A <button> is NOT excluded here, because a button is not somewhere a keystroke
 * is a character. It is handled by isSpaceActivated below instead, which is a
 * different question with a different answer.
 */
export function isTypingTarget(target) {
  if (!target || typeof target !== 'object') return false;
  if (target.isContentEditable === true) return true;
  const tag = String(target.tagName || '').toUpperCase();
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || tag === 'OPTION';
}

/**
 * SPACE_ACTIVATED_ROLES are the ARIA roles whose controls the platform activates
 * with Space, for elements that are not really <button> underneath.
 */
const SPACE_ACTIVATED_ROLES = new Set([
  'button',
  'checkbox',
  'radio',
  'switch',
  'menuitem',
  'menuitemcheckbox',
  'menuitemradio',
  'option',
  'tab',
]);

/**
 * isSpaceActivated reports whether Space is the target's own ACTIVATION key —
 * whether pressing it there already means "press this control".
 *
 * ===================== WHY THIS IS SEPARATE FROM TYPING =====================
 *
 * The push-to-mute binding is document-level, capturing, and calls
 * preventDefault, because the operator's hands are not guaranteed to be near the
 * button and Space would otherwise scroll the page. All correct — and it stole
 * Space from every button in the application. Space activates a focused
 * <button>, and it does so on the KEYUP; the binding cancelled the default on
 * both edges, so Save in Settings, the Mixer drawer's controls and every other
 * button in the app stopped responding to a keyboard entirely. Somebody working
 * without a mouse could reach a control and never fire it.
 *
 * So the rule is: when the focused thing already answers to Space, it keeps it.
 * Everywhere else — the picture, the lamps, the body, which is where focus
 * actually sits during a match — Space is the mute. The single exception is the
 * PUSH TO MUTE button itself, whose activation IS the mute and which the caller
 * therefore exempts explicitly; a click there would be one event where the press
 * and release need two.
 *
 * Only Space needs this. The latch is bound to M, which activates nothing.
 */
export function isSpaceActivated(target) {
  if (!target || typeof target !== 'object') return false;
  const tag = String(target.tagName || '').toUpperCase();
  if (tag === 'BUTTON' || tag === 'SUMMARY') return true;
  // <input type=checkbox|radio|button|submit|reset> are Space-activated too, but
  // isTypingTarget already excludes every INPUT before this is reached.
  const role =
    typeof target.getAttribute === 'function' ? String(target.getAttribute('role') || '') : '';
  return SPACE_ACTIVATED_ROLES.has(role.toLowerCase());
}

/**
 * MUTE_STATE is the readout's vocabulary — one of these six, never a boolean.
 *
 * FAILED exists because "we could not mute" is not "not muted": the operator has
 * pressed the control and must be told it did not take. UNAVAILABLE is separate
 * from it because "there is nothing to mute yet" is not a failure — it is what
 * every machine reads before START, and painting that red would be an alarm on
 * the ordinary state of the application.
 */
export const MUTE_STATE = Object.freeze({
  LIVE: 'live',
  MUTED: 'muted',
  MUTING: 'muting',
  UNMUTING: 'unmuting',
  FAILED: 'failed',
  UNAVAILABLE: 'unavailable',
});

/**
 * MUTE_BY_REMOTE is app.go's muteSeatRemote, mirrored here as a string contract.
 *
 * Wails flattens the payload to JSON, so the sentinel does not survive the
 * boundary as anything else. app.go's mutePayload comment is explicit that the
 * frontend MUST draw this: "a remote seat silently muting the feed with nobody
 * at the desk knowing is the failure that made this decision arguable in the
 * first place, and this pair is the price of allowing it."
 */
export const MUTE_BY_REMOTE = 'remote';

/**
 * MUTE_MODE mirrors config.CoughMuteModePush / CoughMuteModeLatch — the
 * operator's saved PREFERENCE, a MACHINE-classified UI field in
 * internal/presets/fields.go.
 *
 * ============ WHAT THE MODE DOES, AND WHAT IT DELIBERATELY DOES NOT =========
 *
 * It chooses which behaviour is PRIMARY on the screen: which button is drawn
 * first and larger, and which the readout names when nothing is muted. It does
 * NOT take the other behaviour away, and that is the decision worth arguing.
 *
 * The safety property this control has to hold is that the muted state is a
 * function of the two inputs with no ordering that surprises: muted = held ||
 * latched. A real mode switch breaks it in the one moment that matters — a
 * commentator who has latched and then instinctively holds the key would have
 * the hold UNDO the latch, or a mis-pressed mode switch would strand them muted
 * with the control that would release it no longer on screen. Neither is
 * survivable on a live match, and neither is what "I want a push to mute style
 * and a latch mute mode" asks for: the operator asked for both behaviours.
 *
 * So the mode is a preference about EMPHASIS, the saved value is honoured and
 * shown, and both behaviours remain reachable at all times.
 */
export const MUTE_MODE = Object.freeze({
  PUSH: 'push',
  LATCH: 'latch',
});

/** DEFAULT_MUTE_MODE mirrors config.DefaultCoughMuteMode. */
export const DEFAULT_MUTE_MODE = MUTE_MODE.PUSH;

/**
 * normaliseMuteMode substitutes the default for an empty value AND for an
 * unrecognised one, exactly as config.EffectiveCoughMuteMode does on the Go
 * side. A hand-edited config.json must not produce a control with no primary.
 */
export function normaliseMuteMode(mode) {
  const m = typeof mode === 'string' ? mode.trim() : '';
  return m === MUTE_MODE.LATCH ? MUTE_MODE.LATCH : MUTE_MODE.PUSH;
}

/** describeMuteMode is the mode's name on the screen. */
export function describeMuteMode(mode) {
  return normaliseMuteMode(mode) === MUTE_MODE.LATCH ? 'LATCH' : 'PUSH';
}

/** HOLD_REASON says which of the two behaviours is holding a mute. */
export const HOLD_REASON = Object.freeze({
  NONE: '',
  LATCH: 'LATCHED',
  PUSH: 'HOLDING',
  BOTH: 'LATCHED + HOLDING',
});

/**
 * describeMute turns the model's state into everything the screen shows.
 *
 * @param {{held: boolean, latched: boolean, confirmed: boolean, pending: boolean,
 *          failure: string, available: boolean}} s
 * @returns {{state: string, muted: boolean, text: string, reason: string,
 *            detail: string, level: string}}
 */
export function describeMute(s) {
  const intended = !!(s.held || s.latched);
  const reason =
    s.held && s.latched
      ? HOLD_REASON.BOTH
      : s.latched
        ? HOLD_REASON.LATCH
        : s.held
          ? HOLD_REASON.PUSH
          : HOLD_REASON.NONE;
  // held and latched ride along so the two BUTTONS can show their own pressed
  // state from the same object the readout is drawn from. A button that reads
  // its own variable is a second place the truth lives.
  const controls = {
    held: !!s.held,
    latched: !!s.latched,
    available: s.available !== false,
    mode: normaliseMuteMode(s.mode),
  };

  // WHO DID IT. app.go's payload carries By/ByAddr, and its comment is explicit
  // that the frontend must draw them when a REMOTE seat is holding the mute: a
  // second seat silently muting the feed with nobody at the desk knowing is the
  // failure that made allowing it arguable at all.
  const byRemote = s.by === MUTE_BY_REMOTE;
  const whoDetail = byRemote
    ? ` A REMOTE SEAT muted this${s.byAddr ? ` (${s.byAddr})` : ''}.`
    : '';

  if (s.available === false) {
    return {
      ...controls,
      state: MUTE_STATE.UNAVAILABLE,
      muted: !!s.confirmed,
      text: 'NO MUTE',
      reason: '',
      level: 'grey',
      // The sentence is the Go side's whenever there is one: "nothing is being
      // sent yet, so there is nothing to mute…" is app.go's own wording, and two
      // descriptions of one fact are one description too many.
      detail:
        s.reason ||
        'Nothing on this screen can mute the commentary in this build. Do not rely on it.',
    };
  }

  if (s.failure) {
    return {
      ...controls,
      state: MUTE_STATE.FAILED,
      // Whatever Go last CONFIRMED, never what was asked for. The call was
      // refused, so the send path is in whatever state it was; claiming
      // otherwise is the one lie this control may never tell.
      muted: !!s.confirmed,
      text: 'MUTE FAILED',
      reason,
      level: 'red',
      detail: `The commentary mute was refused: ${s.failure}${whoDetail}`,
    };
  }

  if (s.pending) {
    return intended
      ? {
          ...controls,
          state: MUTE_STATE.MUTING,
          muted: !!s.confirmed,
          text: 'MUTING…',
          reason,
          level: 'amber',
          detail: 'Asking the send path to mute.',
        }
      : {
          ...controls,
          state: MUTE_STATE.UNMUTING,
          muted: !!s.confirmed,
          text: 'OPENING…',
          reason,
          level: 'amber',
          detail: 'Asking the send path to go live again.',
        };
  }

  if (s.confirmed) {
    return {
      ...controls,
      state: MUTE_STATE.MUTED,
      muted: true,
      // A mute nobody at this desk asked for is named on the BADGE, not only in
      // a tooltip nobody hovers mid-match.
      text: byRemote ? 'MUTED (REMOTE)' : 'MUTED',
      reason,
      level: 'red',
      detail:
        'The commentary is muted at the send path. Nothing from this microphone is going out.' +
        whoDetail,
    };
  }

  return {
    ...controls,
    state: MUTE_STATE.LIVE,
    muted: false,
    text: 'LIVE',
    // With nothing holding a mute there is no reason to name, so the line says
    // WHICH MODE IS ACTIVE instead — the operator's saved choice, on the screen,
    // where a preference that only shows itself when it is pressed would not be.
    reason: `${describeMuteMode(controls.mode)} MODE`,
    level: 'green',
    detail: 'The commentary is going out. Hold the push control, or latch, to mute it.',
  };
}

/**
 * createCoughMute builds the model.
 *
 * @param {object} args
 * @param {(muted: boolean) => Promise<object>} args.apply the SEND-PATH mute.
 *        Must be backend.setCommentaryMute; see the header. It resolves with
 *        app.go's mutePayload — the state IN FORCE, which is not always what was
 *        asked for, because a request that lost its race is dropped rather than
 *        applied and the payload says so.
 * @param {(readout: object) => void} [args.onChange] called after every state
 *        change with describeMute's output.
 * @param {boolean} [args.available] false when the build has no bindings.
 * @param {string} [args.mode] the operator's saved coughMuteMode.
 */
export function createCoughMute({ apply, onChange, available = true, mode = DEFAULT_MUTE_MODE }) {
  const state = {
    held: false,
    latched: false,
    confirmed: false,
    pending: false,
    failure: '',
    available: available !== false,
    reason: '',
    by: '',
    byAddr: '',
    mode: normaliseMuteMode(mode),
  };

  let inFlight = false;

  // `sent` is what THIS SEAT last successfully transmitted, and it is the ONLY
  // thing local intent is ever compared against. See pump.
  //
  // It starts false because a seat that has sent nothing has sent nothing —
  // deliberately NOT seeded from Go's first payload, because seeding it from a
  // shared value is precisely the confusion this variable exists to end.
  let sent = false;

  function emit() {
    if (onChange) onChange(describeMute(state));
  }

  /**
   * absorb takes app.go's mutePayload as the TRUTH about the send path.
   *
   * Every field of it, not just `muted`: `available` and `reason` are how a
   * session ending disables the controls with the right sentence on them, and
   * By/ByAddr are how the desk accounts for a mute it did not make.
   */
  function absorb(payload) {
    const p = payload && typeof payload === 'object' ? payload : {};
    state.confirmed = p.muted === true;
    state.available = p.available !== false;
    state.reason = typeof p.reason === 'string' ? p.reason : '';
    state.by = typeof p.by === 'string' ? p.by : '';
    state.byAddr = typeof p.byAddr === 'string' ? p.byAddr : '';
  }

  /**
   * pump drives Go towards the current intent, one call at a time.
   *
   * LATEST-WINS rather than a queue: if the controls changed three times while
   * one call was in flight, the only interesting value is the last one. A queue
   * would replay the intermediate states and could leave the send path muted
   * after the operator has already let go.
   */
  function pump() {
    if (inFlight) return;
    if (!state.available) return;
    const want = !!(state.held || state.latched);
    if (want === state.confirmed) {
      if (state.failure) {
        state.failure = '';
        emit();
      }
      return;
    }

    inFlight = true;
    state.pending = true;
    emit();

    // ISSUED SYNCHRONOUSLY, not scheduled. This is the lowest-latency thing the
    // application does — a cough is under a second and the whole point is that
    // the mute lands before the sound does — so the IPC starts on the key-down
    // itself rather than one microtask later. A synchronous throw from `apply`
    // is normalised into a rejection so the failure path is the same one.
    let flight;
    try {
      flight = Promise.resolve(apply(want));
    } catch (err) {
      flight = Promise.reject(err);
    }

    flight
      .then((payload) => {
        absorb(payload);
        state.failure = '';
      })
      .catch((err) => {
        // `confirmed` is untouched deliberately, so the readout cannot claim a
        // mute that did not land.
        state.failure = String(err?.message || err || 'the call failed');
      })
      .then(() => {
        inFlight = false;
        state.pending = false;
        emit();
        // The intent may have moved on while this was in flight. Re-check rather
        // than assume — this is the push-and-release-inside-one-round-trip case,
        // and getting it wrong strands the microphone muted.
        if (!state.failure && !!(state.held || state.latched) !== state.confirmed) pump();
      });
  }

  return {
    /** press starts a push-to-mute hold. Idempotent. */
    press() {
      if (state.held) return;
      state.held = true;
      state.failure = '';
      emit();
      pump();
    },

    /**
     * release ends a push-to-mute hold. Idempotent, and safe to call from blur,
     * visibilitychange, pointerleave and pointercancel as well as from the
     * release itself — see the fail-safe note in the header.
     */
    release() {
      if (!state.held) return;
      state.held = false;
      emit();
      pump();
    },

    /** toggleLatch flips the latch. Returns the new latch value. */
    toggleLatch() {
      state.latched = !state.latched;
      state.failure = '';
      emit();
      pump();
      return state.latched;
    },

    /** setLatch sets the latch explicitly. */
    setLatch(on) {
      const next = on === true;
      if (next === state.latched) return;
      state.latched = next;
      state.failure = '';
      emit();
      pump();
    },

    /**
     * adopt takes an AUTHORITATIVE mutePayload — Go telling us what the send path
     * actually is, on startup or on the "mute" event.
     *
     * It absorbs the whole payload and then re-pumps, so a send path that came up
     * muted (a page reloaded mid-cough, a second seat) is either corrected to
     * match the controls or, if it already matches, simply believed.
     *
     * ============== A SESSION ENDING DROPS THE HELD MUTE, NOT THE LATCH =======
     *
     * app.go clears the mute at both session boundaries, so an unavailable
     * payload means there is nothing to hold. The HELD state is cleared with it:
     * a key the operator is no longer pressing (because they pressed STOP) must
     * not be remembered. The LATCH is not — it is a deliberate standing choice,
     * and the next session should begin the way the operator left this one,
     * which is also why pump() re-issues it as soon as the payload says a session
     * is available again.
     */
    /**
     * observe takes a payload that ARRIVED FROM GO and updates what this seat
     * DISPLAYS. It never calls.
     *
     * ================= THE INVARIANT, AND WHY IT IS THE WHOLE FIX ============
     *
     * A payload from Go may change what a seat DISPLAYS. It may never, by
     * itself, cause a seat to CALL.
     *
     * This used to end in pump(), and with one seat that was self-correcting:
     * the desk saw a stale value, disagreed, and put it right. With TWO seats it
     * is a fight neither can win, because each seat holds its own intent and
     * treats the shared value as something to be corrected towards it:
     *
     *   A latches   -> Go says muted     -> B observes, disagrees, unmutes
     *   B's unmute  -> Go says unmuted   -> A observes, disagrees, mutes
     *
     * at network speed, for as long as both are open. The operator reported it
     * as "the mute keys just flash on and off at a super high frequency".
     *
     * No amount of echo suppression, debouncing or rate limiting fixes this —
     * measured by the diagnostic that reproduced it, echo suppression alone
     * still oscillated 300 times in 300 turns — because B fighting A is not an
     * echo. The only fix is that an arriving payload cannot cause a call.
     *
     * DO NOT ADD A pump() HERE to "correct a stale payload". That is the bug,
     * and there is a test that fails by name if it comes back.
     */
    observe(payload) {
      absorb(payload);
      state.failure = '';
      // Availability is the exception that proves the rule: it does not send
      // anything, it releases a LOCAL hold that can no longer mean anything,
      // because there is nothing to hold the mute on.
      if (!state.available) {
        state.held = false;
        sent = false;
      }
      emit();
    },

    /**
     * adopt is observe, kept under its old name for the startup read.
     *
     * app.js calls this once with GetCommentaryMute's answer before any event
     * can arrive. It is the same thing as observing an event — absorb and paint
     * — and it is deliberately NOT a place to reconcile either: a page that has
     * just loaded has no intent of its own to assert.
     */
    adopt(payload) {
      this.observe(payload);
    },

    /** setMode records the operator's saved coughMuteMode preference. */
    setMode(next) {
      const m = normaliseMuteMode(next);
      if (m === state.mode) return;
      state.mode = m;
      emit();
    },

    /** readout is describeMute of the current state. */
    get readout() {
      return describeMute(state);
    },

    /** state, read-only, for tests and for the aria text. */
    get snapshot() {
      return { ...state };
    },
  };
}
