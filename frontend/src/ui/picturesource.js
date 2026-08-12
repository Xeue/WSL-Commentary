/**
 * The PICTURE source control: where the commentator's picture comes from.
 *
 * Owner: WP-5b.
 *
 * ===================== THIS REPLACES A CONTROL THAT WAS BACKWARDS ============
 *
 * There used to be a "Return Source" control here — WebRTC or SRT — and what it
 * switched was the HEADPHONES. That was the wrong way round. The operator asked
 * for one thing, in these words:
 *
 *   "I want DIRTY PICTURES. I want PGM high res pictures in what the
 *    commentator sees. With the audio coming from Kinesis."
 *
 * So SRT is a PICTURE path, not an audio path, and the audio never moves:
 *
 *     PICTURE   SRT, M2L-X Output 1, src=pgm, port 40501,
 *               H.265/HEVC 1920x1080 50p, ~15000 kbps, encrypted=false
 *     AUDIO     Kinesis/WebRTC, unchanged, with its existing bus (PGM/CLN/MON/
 *               MIC1-3/PFL) and its existing stereo/left/right selection
 *
 * The SRT stream carries its own AAC audio. IT IS DISCARDED. Playing it would
 * put the same programme in the commentator's ears twice at two different
 * offsets, which is the exact failure the old control's whole safety argument
 * was built around — and it would arrive by the back door, as a side effect of
 * choosing a picture.
 *
 * ========================= WHY THERE IS STILL A CHOICE ======================
 *
 * Because a commentator must never be looking at black.
 *
 * SRT is a real network stream to a native decoder. It takes a moment to dial,
 * it can be refused, and the M2L-X output it wants can be switched off by
 * somebody else. When it is not delivering, the KVS multiviewer mosaic — soft,
 * CSS-cropped to the PGM tile, and already arriving on a connection that is up
 * for the audio anyway — is shown instead. A soft picture beats no picture.
 *
 * That is a FALLBACK, not a preference, and the two are not the same thing: the
 * fallback happens by itself and says so on screen. The control below chooses
 * what the application should be TRYING for, and "Mosaic" is there for the case
 * where the operator wants the SRT fan-out slot back.
 *
 * ============================ WHAT IS NOT HERE ==============================
 *
 * Nothing about audio. Not a mute, not a device, not a bus. Grep this file for
 * a word about the headphones and you will not find one, and picturesource.test.js
 * asserts that by reading this file's text — because the last time a control on
 * this screen touched two things at once, selecting it silenced the operator.
 */

/** The high-quality path: a native SRT receiver painting over the page. */
export const PICTURE_SOURCE_SRT = 'srt';
/** The fallback: the KVS multiviewer mosaic, cropped to the PGM tile. */
export const PICTURE_SOURCE_MOSAIC = 'mosaic';

/**
 * DEFAULT_PICTURE_SOURCE is SRT — the thing that was asked for. The mosaic is
 * what you get when SRT is not delivering, which is a state the application
 * reaches on its own; it is not the state it starts in by choice.
 */
export const DEFAULT_PICTURE_SOURCE = PICTURE_SOURCE_SRT;

/** The config key the choice is persisted under. */
export const PICTURE_SOURCE_KEY = 'pictureSource';

/**
 * The four states the native picture receiver reports on the "picture" event.
 *
 * THEY ARE LOWERCASE, and that is not a style choice — it is internal/gst's
 * PictureState, spelled exactly as Go spells it. gst.ReturnState is uppercase
 * and these are not; the two enums are neighbours, they look alike, and a copy
 * made from the wrong one would compare unequal to every event that arrives, so
 * the status line would sit for ever on whatever was last recognised.
 * picturesource.test.js asserts the four strings against picture.go itself.
 *
 * Everything that compares against them normalises case first anyway, because a
 * wire value is not something to be brittle about; the constants are exact so
 * that the CONTRACT is checkable, not so that the comparison can be sloppy.
 */
export const PICTURE_STATE_STOPPED = 'stopped';
export const PICTURE_STATE_CONNECTING = 'connecting';
export const PICTURE_STATE_SHOWING = 'showing';
export const PICTURE_STATE_BACKOFF = 'backoff';

/**
 * PICTURE_STATE_WORDS glosses each state in the words of somebody about to
 * commentate, not of a broadcast engineer. "backoff" tells an engineer that an
 * attempt failed and another is coming; it tells a commentator nothing.
 */
export const PICTURE_STATE_WORDS = Object.freeze({
  [PICTURE_STATE_STOPPED]: 'not running',
  [PICTURE_STATE_CONNECTING]: 'dialling M2L-X',
  [PICTURE_STATE_SHOWING]: 'high-quality picture is arriving',
  [PICTURE_STATE_BACKOFF]: 'the last attempt failed; retrying',
});

/**
 * PICTURE_BACKOFF_ERROR is the banner raised when an SRT reconnect EPISODE
 * begins — once per episode, not once per retry; see errorlog.js's
 * createBackoffEpisode. It replaced an inline red status line under the
 * controls, at the operator's request: reconnect failures now speak through
 * the same banner as every other error.
 *
 * The second sentence is there because the first, alone, invites the wrong
 * action: the fallback is automatic and the commentator has a picture the
 * whole time. Nothing needs doing, and the message says so.
 */
export const PICTURE_BACKOFF_ERROR =
  'SRT picture: the last attempt failed — retrying automatically. ' +
  'You are watching the mosaic meanwhile.';

/**
 * normalisePictureState folds a state from the wire to the spelling above.
 * Anything unrecognised comes back as the empty string, which every caller reads
 * as "not showing" — the safe direction, because the alternative is painting an
 * opaque window on the strength of a word nobody recognises.
 *
 * @param {unknown} state
 * @returns {string}
 */
export function normalisePictureState(state) {
  const s = String(state == null ? '' : state).toLowerCase();
  return Object.prototype.hasOwnProperty.call(PICTURE_STATE_WORDS, s) ? s : '';
}

/**
 * PICTURE_SOURCES is the control, in the order it is drawn.
 *
 * `cost` is stated because the better option is not free: it is a real
 * 15 Mbit/s stream and one of the four fan-out slots M2L-X has. The operator
 * cannot weigh that against a mosaic they are already receiving unless it is
 * written down.
 *
 * @type {ReadonlyArray<{value: string, label: string, summary: string, cost: string}>}
 */
export const PICTURE_SOURCES = Object.freeze([
  Object.freeze({
    value: PICTURE_SOURCE_SRT,
    label: 'High quality (SRT)',
    summary:
      'The M2L-X programme output at 1920x1080 50p, decoded natively and painted over this window. ' +
      'This is the dirty PGM feed — what the director is putting to air.',
    cost:
      'A real 15 Mbit/s SRT stream from M2L-X Output 1 and one of its fan-out slots. ' +
      'Falls back to the mosaic on its own if it is not delivering.',
  }),
  Object.freeze({
    value: PICTURE_SOURCE_MOSAIC,
    label: 'Mosaic (WebRTC)',
    summary:
      'The multiviewer mosaic already arriving with the return audio, cropped to the PGM tile. ' +
      'Soft — it is one tile of a 2240x1440 grid scaled up — but always available.',
    cost: 'No extra stream and no fan-out slot: this picture is already being received.',
  }),
]);

/**
 * PICTURE_NOTE is fixed text under the control. It says the one thing the
 * commentator cannot work out by looking: that this control does not touch what
 * they can hear.
 *
 * It is not conditional. A line that appears on only one option reads as a
 * difference between the options, and "the audio is unaffected" is true of both.
 */
export const PICTURE_NOTE =
  'Audio always comes from Kinesis and is not affected by this control. ' +
  'The SRT stream carries its own audio; it is discarded.';

/**
 * isValidPictureSource reports whether source names one of the two pictures.
 * @param {unknown} source
 * @returns {boolean}
 */
export function isValidPictureSource(source) {
  return PICTURE_SOURCES.some((s) => s.value === source);
}

/**
 * normalisePictureSource coerces anything to a picture that exists.
 *
 * A config with a third value in it must not leave the commentator with no
 * picture selected at all — and unlike the audio control this once replaced,
 * getting it wrong here costs a black rectangle rather than silence, which is
 * still not something to leave to chance ten seconds before kick-off.
 *
 * @param {unknown} source
 * @param {string} [fallback]
 * @returns {string}
 */
export function normalisePictureSource(source, fallback = DEFAULT_PICTURE_SOURCE) {
  if (isValidPictureSource(source)) return String(source);
  return isValidPictureSource(fallback) ? String(fallback) : DEFAULT_PICTURE_SOURCE;
}

/**
 * derivePictureSourceEffects is THE statement of what a picture selection means.
 *
 * Everything the application does about the picture is read off this object, so
 * that "the mosaic shows whenever SRT is not on screen" is one invariant in one
 * place rather than a pair of call sites kept opposite by hand.
 *
 * It is deliberately total: for every input, valid or not, exactly one of
 * `showingSRT` and `showingMosaic` is true. THERE IS NO STATE IN WHICH NEITHER
 * IS SHOWING — a commentator staring at black, wondering whether the match has
 * started, is the outcome this function exists to make unreachable.
 *
 * It says NOTHING about audio, and must never grow a field that does.
 *
 * @param {unknown} source
 * @param {unknown} [state] the last "picture" event, a PICTURE_STATE_* string
 * @returns {{
 *   source: string,
 *   wantSRT: boolean,
 *   connected: boolean,
 *   showingSRT: boolean,
 *   showingMosaic: boolean,
 *   fallenBack: boolean,
 * }}
 */
export function derivePictureSourceEffects(source, state) {
  const s = normalisePictureSource(source);
  const wantSRT = s === PICTURE_SOURCE_SRT;
  const connected = normalisePictureState(state) === PICTURE_STATE_SHOWING;
  const showingSRT = wantSRT && connected;
  return Object.freeze({
    source: s,
    // Whether the native receiver should be RUNNING. Not the same as whether it
    // is on screen: it is running throughout CONNECTING and BACKOFF, holding a
    // fan-out slot, while the mosaic is what the commentator is looking at.
    wantSRT,
    connected,
    showingSRT,
    // The mosaic is the picture whenever SRT is not, which is the whole
    // invariant. Written as the negation rather than as its own condition so
    // that no future edit can make both false.
    showingMosaic: !showingSRT,
    // True only when SRT was ASKED FOR and is not delivering. That is the state
    // the badge has to call out; "mosaic because you chose mosaic" is not a
    // fallback and must not be dressed as one.
    fallenBack: wantSRT && !connected,
  });
}

/**
 * pictureSourceLabel is the button text. Never blank.
 * @param {string} source
 * @returns {string}
 */
export function pictureSourceLabel(source) {
  const found = PICTURE_SOURCES.find((s) => s.value === source);
  return found ? found.label : `${String(source)} — (not one of the two pictures)`;
}

/**
 * describePictureSource is the sentence under the control: what the selected
 * picture is, and what it costs.
 *
 * @param {string} source
 * @returns {string}
 */
export function describePictureSource(source) {
  const s = normalisePictureSource(source);
  const found = PICTURE_SOURCES.find((x) => x.value === s);
  return `${found.summary} ${found.cost}`;
}

/**
 * describePictureShowing is the BADGE: which picture is on screen right now, in
 * four words, permanently, over the picture itself.
 *
 * It is not a status line among other status lines. The two pictures look alike
 * enough at a glance — the same framing of the same match — and different enough
 * in quality that "is this the good one?" is a question somebody will ask out
 * loud during a match if nothing on screen answers it.
 *
 * @param {ReturnType<typeof derivePictureSourceEffects>} effects
 * @param {unknown} [state]
 * @returns {{text: string, detail: string, good: boolean}}
 */
export function describePictureShowing(effects, state) {
  if (effects.showingSRT) {
    return {
      text: 'SRT 1080p',
      detail: 'The high-quality M2L-X programme picture.',
      good: true,
    };
  }
  if (effects.fallenBack) {
    const name = normalisePictureState(state);
    const words = PICTURE_STATE_WORDS[name] || 'in a state this build does not recognise';
    return {
      text: 'MOSAIC — SRT not up',
      detail: `Showing the soft multiviewer tile while the SRT picture is ${words}.`,
      good: false,
    };
  }
  return {
    text: 'MOSAIC',
    detail: 'The soft multiviewer tile, selected. The SRT picture is not running.',
    good: true,
  };
}
