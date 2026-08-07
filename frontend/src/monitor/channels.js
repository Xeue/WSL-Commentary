/**
 * Which SOURCE channel of the return reaches which OUTPUT channel of the
 * headphones.
 *
 * Owner: WP-5a (it is enforced inside the Web Audio graph in audio.js), but it
 * is also part of the seam — see "THE SEAM" in monitor.js. It is pure data and
 * pure arithmetic: no DOM, no Web Audio, no browser. That is what lets the home
 * screen import it for its labels instead of writing a second copy of the same
 * three words, and what lets the routing be tested in `node --test`.
 *
 * ======================= WHY THIS EXISTS ====================================
 *
 * The operator needed FX and comms mixed on PGM but hard-panned L and R on CLN.
 * M2L-X's pan is PER INPUT STRIP, not per bus — `pan_balance` lives at
 * `state.effect.<strip>` and there is nothing pan-like on `state.outputs` — so a
 * strip's pan applies to every bus that strip feeds. The way out was DOUBLE
 * ROUTER INPUTS: each source arrives twice, one copy panned centre and routed to
 * master, the other panned hard L or R and routed to aux1.
 *
 * The consequence lands here. The CLN bus now carries:
 *
 *     LEFT  = the effects bed
 *     RIGHT = comms
 *
 * so choosing a BUS is no longer enough to choose what the commentator hears.
 * They have to be able to choose a CHANNEL of that bus as well.
 *
 * ======================= WHY "LEFT ONLY" IS BOTH EARS =======================
 *
 * "Left only" means the LEFT SOURCE CHANNEL IN BOTH EARS. It does not mean
 * "audio in the left ear".
 *
 * A commentator with one dead ear is not monitoring. Half of them will be
 * wearing single-ear cans, and which ear that is is not something this
 * application knows. So the chosen source channel is duplicated to both outputs,
 * which is a ChannelSplitter with the same output wired to both merger inputs.
 *
 * Zeroing a gain node on one side would look identical on a level meter and be
 * wrong in exactly the way that matters: it silences an EAR, not a source. That
 * is why the routing below is expressed as source-to-output pairs and why
 * channels.test.js asserts on the pairs rather than on a gain value.
 *
 * ======================= THE MONO CAVEAT ====================================
 *
 * A ChannelSplitter is `channelInterpretation: 'discrete'`, so a MONO track
 * up-mixed to two channels gets the audio on channel 0 and DIGITAL SILENCE on
 * channel 1. If the selected bus ever arrives mono, "Right only" is silence —
 * not a bug in this file, but a real way for a commentator to end up with
 * nothing, and there is no Web Audio API that reports the true channel count of
 * a remote WebRTC track, so it cannot be detected here and turned into a
 * warning. It is stated in the UI hint for the Right option instead.
 */

/** Both source channels straight through: L to left, R to right. */
export const CHANNEL_STEREO = 'stereo';
/** The LEFT source channel, in BOTH ears. */
export const CHANNEL_LEFT = 'left';
/** The RIGHT source channel, in BOTH ears. */
export const CHANNEL_RIGHT = 'right';

/**
 * DEFAULT_CHANNEL_MODE is stereo. Unchanged behaviour for anyone who does not
 * touch the control: before this existed, both channels went through.
 */
export const DEFAULT_CHANNEL_MODE = CHANNEL_STEREO;

/**
 * CHANNEL_MODES is the whole control, in the order it is drawn.
 *
 * `hint` is a full sentence because the reason to pick Left is a fact about this
 * event's routing, not a property of the word "left", and a commentator who has
 * never been told about the double-router-input arrangement has no way to guess
 * it.
 *
 * @type {ReadonlyArray<{value: string, label: string, hint: string}>}
 */
export const CHANNEL_MODES = Object.freeze([
  Object.freeze({
    value: CHANNEL_STEREO,
    label: 'Stereo',
    hint: 'Both channels as sent. On CLN that is effects and comms together, hard left and hard right.',
  }),
  Object.freeze({
    value: CHANNEL_LEFT,
    label: 'Left only',
    hint: 'The left source channel in BOTH ears. On CLN that is the effects bed on its own.',
  }),
  Object.freeze({
    value: CHANNEL_RIGHT,
    label: 'Right only',
    hint:
      'The right source channel in BOTH ears. On CLN that is comms on its own. ' +
      'If the selected bus is mono this is silence.',
  }),
]);

/**
 * isValidChannelMode reports whether mode names one of the three.
 * @param {unknown} mode
 * @returns {boolean}
 */
export function isValidChannelMode(mode) {
  return CHANNEL_MODES.some((m) => m.value === mode);
}

/**
 * normaliseChannelMode coerces anything — a hand-edited config.json, an older
 * file, undefined — to a mode that exists. It never returns a mode that the
 * routing table has no entry for, because a missing entry means the splitter
 * gets wired to nothing and the return goes silent.
 *
 * @param {unknown} mode
 * @param {string} [fallback]
 * @returns {string}
 */
export function normaliseChannelMode(mode, fallback = DEFAULT_CHANNEL_MODE) {
  if (isValidChannelMode(mode)) return String(mode);
  return isValidChannelMode(fallback) ? String(fallback) : DEFAULT_CHANNEL_MODE;
}

/**
 * sourceChannelsForOutputs is THE primitive. Everything else in this file is
 * derived from it, so there is exactly one statement of the routing anywhere in
 * the application.
 *
 * It answers: for output channel 0 (the left ear) and output channel 1 (the
 * right ear), which SOURCE channel feeds it?
 *
 *   stereo -> [0, 1]   left source to left ear, right source to right ear
 *   left   -> [0, 0]   the LEFT source in both ears
 *   right  -> [1, 1]   the RIGHT source in both ears
 *
 * @param {string} mode
 * @returns {[number, number]} [sourceForLeftOutput, sourceForRightOutput]
 */
export function sourceChannelsForOutputs(mode) {
  switch (normaliseChannelMode(mode)) {
    case CHANNEL_LEFT:
      return [0, 0];
    case CHANNEL_RIGHT:
      return [1, 1];
    default:
      return [0, 1];
  }
}

/**
 * channelRouting is sourceChannelsForOutputs in the shape the Web Audio graph
 * wants: one entry per splitter.connect(merger, from, to) call.
 *
 * @param {string} mode
 * @returns {ReadonlyArray<{from: number, to: number}>}
 */
export function channelRouting(mode) {
  const sources = sourceChannelsForOutputs(mode);
  return Object.freeze(sources.map((from, to) => Object.freeze({ from, to })));
}

/**
 * describeChannelMode is one line for a diagnostics dump or a log. It states the
 * routing rather than repeating the label, so a support log says what was
 * actually wired.
 *
 * @param {string} mode
 * @returns {string}
 */
export function describeChannelMode(mode) {
  const m = normaliseChannelMode(mode);
  const [l, r] = sourceChannelsForOutputs(m);
  const name = (n) => (n === 0 ? 'L' : 'R');
  return `${m}: source ${name(l)} -> left ear, source ${name(r)} -> right ear`;
}

/**
 * channelModeLabel is the button text. Never blank: a segmented control with an
 * empty segment reads as a broken control.
 *
 * @param {string} mode
 * @returns {string}
 */
export function channelModeLabel(mode) {
  const found = CHANNEL_MODES.find((m) => m.value === mode);
  return found ? found.label : `${String(mode)} — (not one of the three channel modes)`;
}
