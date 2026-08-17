/**
 * The channel map: the routing grid, the meter beside every input channel, and
 * — on a card seat — the video-signal lamp that explains a silent one.
 *
 * Owner: WP-5b. The MODEL is internal/gst/channelmap.go's — this is the screen.
 *
 * ===================== THIS IS NOT A DECKLINK SCREEN =======================
 *
 * It was, and the belief is worth killing by name because every second reader
 * arrives with it. A Blackmagic card presents its embedded SDI audio as ONE
 * capture stream of sixteen UNPOSITIONED channels (channel-mask=0x0, measured
 * on the UltraStudio 4K Mini) — but so does every other multichannel capture
 * device on this platform. gstosxcoreaudio.c:886-889 sets `layout = NULL` for
 * every source unconditionally, so no CoreAudio device of any width can emit a
 * positioned mask: a real 3-channel aggregate negotiated channels=3,mask=0x0
 * and a 16-in interface negotiated channels=16,mask=0x0, identical in shape to
 * the card's. A Focusrite or an RME is byte-for-byte the same problem.
 *
 * And the two narrow widths are routing decisions too, which is why the grid is
 * drawn at EVERY width the pad negotiates rather than only above two: a stereo
 * pair arriving the wrong way round is fixed by {Left<-2, Right<-1}, and a mono
 * commentator reaches both ears by {Left<-1, Right<-1} — which is exactly what
 * gst.DefaultChannelMap already produces for a one-channel device.
 *
 * The commentary feed is stereo. Between the input and it sits an audioconvert
 * mix-matrix, and this file is the operator's end of it: which input channel
 * arrives on which of the two, and how far each contribution is turned down.
 *
 * ===================== NOTHING HERE IS A SECOND MODEL ======================
 *
 * internal/gst/channelmap.go decides what a map means, what is legal, and what
 * the matrix looks like. This file draws it. Every rule below is the same rule
 * with the same reason, written where the operator's end of it lives — and the
 * places where a number is genuinely mirrored (the sixteen-channel bound, the
 * unity clamp, the flap threshold) say so and are asserted against that package's
 * source in channelmap.test.js, so a drift fails a test instead of shipping.
 *
 * A MAP IS A LIST OF CONTRIBUTIONS, not a dense grid: [{output, input, gain}],
 * input counted from ZERO as gst.ChannelContribution counts it, gain linear in
 * [-1, 1]. The grid on screen is a rendering of that list — which is why the
 * list cannot come back out of this screen with two contributions naming one
 * cell, a thing Go refuses by name.
 *
 * ===================== WHAT THIS SCREEN IS NOT ==============================
 *
 * IT IS A ROUTER WITH ATTENUATION, NOT A MIXER, and the difference is measured
 * rather than stylistic. audioconvert HARD-CLAMPS a coefficient to [-1, 1]:
 * 1.0 is accepted, 1.0000001 is refused — and the refusal is SILENT, leaving
 * the previous matrix in force with the meter not moving by so much as 0.0000 dB
 * (measured on the card, 2026-08-16). So there is no make-up gain and no output
 * trim below the matrix; the only headroom this design has is the headroom the
 * operator leaves in the map. Two channels at unity into one output sum, and
 * with correlated material that is +6.02 dB.
 *
 * That is stated up front — ROUTER_CAVEAT, drawn as permanent prose above the
 * grid — and quantified under it, per output, as the map is built. It is not a
 * tooltip: a tooltip is found only by somebody who already suspected there was
 * something to find. Summing is ALLOWED (two commentators into a mono leg is
 * the commonest correct map there is, and gst's OutputGain argues the case at
 * length); what is not allowed is finding out afterwards.
 *
 * ===================== WHY THE GRID IS SIZED THE WAY IT IS ==================
 *
 * A matrix whose width does not match what the capture pad NEGOTIATED kills the
 * pipeline instantly — "streaming stopped, reason error (-5)", the capture chain
 * dead before the next level message. So the grid is built from
 * gst.Pipeline.InputChannels(), the count read from the pad's own caps, and from
 * nothing else: not from the device's advertised max-channels, not from the
 * width of whatever was saved last time, and not from a constant in here.
 * MAX_INPUT_CHANNELS is a sanity ceiling on the report, never a size. With no
 * count there is no grid at all, and the screen says so — an empty panel that
 * explains itself beats a plausible sixteen-row grid whose sixteenth row stops
 * the feed.
 *
 * ===================== AND WHY IT IS SIZED TO A DEVICE ======================
 *
 * The width is not enough on its own, because capture is always live and the
 * device under it can change while this screen is open. Selecting an interface
 * while the card is open does not renegotiate instantly, and for the length of
 * that reopen the last width anybody published is the CARD's sixteen. A grid
 * that believed it still offered sixteen crosspoints over a two-channel pad
 * writes a 2x16 matrix on the first press.
 *
 * So every pad report is STAMPED WITH THE DEVICE it is about (`deviceKey`,
 * "<capture kind>:<device id>" — the same string audioinput.js encodes into an
 * <option> value and internal/config.AudioDeviceKey builds in Go), the grid
 * rebuilds on a change of EITHER half of that pair, and collect() refuses to
 * speak for a device whose map it is not holding. See setPad and collect.
 *
 * ===================== THE HALVES, AND WHY THEY ARE SPLIT ===================
 *
 * Everything above createChannelMapView is pure — plain data in, plain data out,
 * no DOM and no browser API — so `node --test` drives the arithmetic, the
 * wording and the lamps for real with nothing installed (channelmap.test.js).
 * The view below it is the only part that touches the document, and it is
 * deliberately thin: it draws what the pure half computes. That is lamps.js's
 * shape and it is here for lamps.js's reason.
 *
 * What is given up: the view itself is proved only by the source guards in
 * channelmap.test.js, exactly as settings.js and home.js are. There is no jsdom
 * in this repository and a shim widened until a test passes stops being evidence.
 */

// The lamp vocabulary, imported rather than restated: the same four levels, the
// same glyph-plus-text-plus-colour rendering, so a lamp on this screen and a
// lamp on the main screen are the same instrument. lamps.js is another work
// package's file and nothing here asks it to change.
import { LEVEL, createLampRow } from './lamps.js';
// The meter idiom — the mixer's -60..0 dBFS scale, its -18/-6 zone boundaries
// and its peak-hold — imported from the input meters rather than reimplemented
// horizontally. Three meters in one application that disagree about where amber
// starts is the two-tables bug this repository has already paid for twice.
import {
  meterZones,
  zoneFills,
  dbToFraction,
  createPeakHold,
  isSilentFrame,
  frameChannels,
  LEVELS_FLOOR_DB,
} from './meters.js';

/* ========================================================================== */
/* 1. Gains                                                                   */
/* ========================================================================== */

/**
 * MAX_INPUT_CHANNELS mirrors gst.MaxInputChannels: a CEILING on a reported
 * channel count, not a size. The grid's width always comes from the pad; this
 * only stops a garbled report — an older build, a corrupted config, a caps query
 * against a pad that renegotiated underneath us — asking the browser to lay out
 * thousands of rows.
 *
 * IT MOVES WHEN GO'S MOVES AND ONLY THEN. Go refuses a matrix wider than its own
 * bound by name, so a ceiling raised here alone would draw crosspoints whose
 * every press is refused; a ceiling raised there alone silently CLAMPS a wider
 * pad to a narrower grid, which is the same wrong-width write in the other
 * direction. channelmap.test.js reads gst.MaxInputChannels out of that package's
 * source and asserts this equals it, so the pair is a two-line change and never
 * a one-line one.
 *
 * IT IS 32 AND IT USED TO BE 16, with the measurement on the Go side: a 2x32
 * mix-matrix passes audio and `level` reports 32 rms entries per message. The
 * layout follows without a breakpoint — the grid is four cells per row at every
 * width and scrolls itself past about sixteen rows, with the header row sticky
 * so the L and R columns stay labelled at channel 28. See main.css.
 */
export const MAX_INPUT_CHANNELS = 32;

/**
 * captureKindOf reads the KIND half out of a device key — "decklink" from
 * "decklink:2747401380", "native" from "native:BF568F24-…".
 *
 * SPLIT ON THE FIRST COLON AND THE FIRST COLON ONLY, which is the rule Go's
 * AudioDeviceKeyFor and audioinput.js's encodeAudioInput both follow: a device
 * id may itself contain colons, and an id carried verbatim is the only spelling
 * three languages can agree on. Anything unrecognised reads as "native", again
 * matching both, so a key from a newer build cannot make this screen draw a
 * card's furniture over a microphone.
 *
 * @param {unknown} deviceKey
 * @returns {string}
 */
export function captureKindOf(deviceKey) {
  if (typeof deviceKey !== 'string') return 'native';
  const at = deviceKey.indexOf(':');
  return deviceKey.slice(0, at < 0 ? 0 : at) === 'decklink' ? 'decklink' : 'native';
}

/**
 * The two output rows, by the names an operator uses. The indices ARE
 * gst.OutputLeft and gst.OutputRight, and they are row indices of the matrix —
 * rows are outputs, columns are inputs, and that is the one thing in this design
 * that fails silently when it is the wrong way round.
 *
 * ===================== A TRANSPOSE IS INVISIBLE AT 2x2 =====================
 *
 * Which is why this is written down rather than left to be read off the code. At
 * the width this application ran at for its whole life — two in, two out — the
 * matrix is square, a transposed one is a legal matrix, GStreamer accepts it
 * without a word and the only symptom is that left and right are swapped. It
 * negotiates, it meters, it goes to air.
 *
 * The grid is drawn at every width now, so the first NON-SQUARE one — eight in
 * and two out, on an interface — is where anybody who has the orientation
 * backwards finds out, and there it does not fail politely: a 8x2 matrix written
 * where a 2x8 belongs is refused, and audioconvert's refusal is SILENT and
 * leaves the PREVIOUS matrix in force. So: OUTPUT ROWS, INPUT COLUMNS,
 * everywhere, and `cells[output][input]` in this file without exception.
 *
 * `short` heads the grid's column; `label` is what the trim panel and every
 * aria-label spell out, because "L" on its own is not an answer to "where does
 * this commentator end up".
 */
export const OUTPUT_LEFT = 0;
export const OUTPUT_RIGHT = 1;
export const OUTPUTS = Object.freeze([
  Object.freeze({ index: OUTPUT_LEFT, short: 'L', label: 'Left' }),
  Object.freeze({ index: OUTPUT_RIGHT, short: 'R', label: 'Right' }),
]);

/** GAIN_LIMIT mirrors gst.ChannelGainLimit. It is GStreamer's clamp, not a policy. */
export const GAIN_LIMIT = 1;

/**
 * GAIN_PRECISION is how many decimal places a gain keeps.
 *
 * It exists because of the rejection rule: a coefficient outside the clamp is
 * refused and the PREVIOUS matrix stays, silently. dB arithmetic produces things
 * like 1.0000000000000002 for 0 dB, so every gain that leaves this file is
 * rounded before it is clamped — six places is far finer than anything audible
 * (the last place is ~0.00001 dB at unity) and cannot round to 1.0000001.
 */
const GAIN_PRECISION = 1e6;

/**
 * clampGain turns anything into a gain Go will accept: finite, within
 * [-GAIN_LIMIT, GAIN_LIMIT], rounded to GAIN_PRECISION. Non-numeric input
 * becomes 0 — silence — because the one thing a routing coefficient must never
 * do on bad input is come out loud.
 *
 * @param {unknown} value
 * @returns {number}
 */
export function clampGain(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return 0;
  const clamped = Math.max(-GAIN_LIMIT, Math.min(GAIN_LIMIT, n));
  return Math.round(clamped * GAIN_PRECISION) / GAIN_PRECISION;
}

/**
 * TRIM_FLOOR_DB is the bottom of the trim control. -40 dB is 1% of full scale:
 * below it a contribution is inaudible under anything else on the same output,
 * so a finer bottom end would only be a slower way of reaching Off — which is
 * its own control, not the bottom of this slider. There is no top end to choose:
 * 0 dB is the clamp.
 */
export const TRIM_FLOOR_DB = -40;

/**
 * gainToDb reports a contribution's level in dB, or null when it is not routed
 * at all. The MAGNITUDE only: polarity is a separate question with its own
 * control, because a gain of -0.5 means "half, inverted" and rendering that as
 * "-6 dB" with the minus sign somewhere else entirely is how an inverted leg
 * goes unnoticed.
 *
 * Rounded to 0.1 dB — the resolution the readout shows — so the number on screen
 * and the number in the map cannot disagree at the eleventh decimal place.
 *
 * @param {unknown} gain
 * @returns {number|null} dB, floored at TRIM_FLOOR_DB, or null for "not routed"
 */
export function gainToDb(gain) {
  const magnitude = Math.abs(clampGain(gain));
  if (magnitude <= 0) return null;
  const db = 20 * Math.log10(magnitude);
  if (db <= TRIM_FLOOR_DB) return TRIM_FLOOR_DB;
  return Math.round(db * 10) / 10;
}

/**
 * dbToGain is the inverse: a dB value and a polarity into a gain Go accepts.
 * Above 0 dB is not an error to report, it is a value that cannot exist — the
 * clamp is GStreamer's — so it is pulled to unity rather than refused, and the
 * control that feeds this simply has no travel above 0.
 *
 * @param {unknown} db
 * @param {boolean} [inverted] true for the ø (polarity-reversed) contribution
 * @returns {number}
 */
export function dbToGain(db, inverted = false) {
  const n = Number(db);
  if (!Number.isFinite(n)) return 0;
  const magnitude = n >= 0 ? 1 : Math.pow(10, n / 20);
  return clampGain(inverted ? -magnitude : magnitude);
}

/** isInverted reports whether a contribution is polarity-reversed. */
export function isInverted(gain) {
  return clampGain(gain) < 0;
}

/**
 * describeGain is what a crosspoint cell says. Never a bare colour: the cell is
 * the primary reading of the routing, and colour is never the only signal on
 * this screen any more than it is on the lamps.
 *
 *   0      -> '—'          (not routed; an em dash, not an empty cell)
 *   1      -> '0.0 dB'
 *   0.5    -> '-6.0 dB'
 *   -0.5   -> 'ø -6.0 dB'  (inverted)
 *
 * @param {unknown} gain
 * @returns {string}
 */
export function describeGain(gain) {
  const db = gainToDb(gain);
  if (db === null) return '—';
  const text = `${db.toFixed(1)} dB`;
  return isInverted(gain) ? `ø ${text}` : text;
}

/**
 * crosspointLabel names one crosspoint in the words the operator reads
 * everywhere else. THE +1 IS HERE, and it is here exactly once: gst counts
 * inputs from zero because the matrix does, the operator's SDI embedder counts
 * from one, and a conversion that lived in two places would eventually be done
 * twice.
 *
 * @param {number} input zero-based channel index
 * @param {number} output zero-based output index
 * @returns {string}
 */
export function crosspointLabel(input, output) {
  const out = OUTPUTS[output];
  return `Channel ${input + 1} → ${out ? out.label : `Output ${output + 1}`}`;
}

/* ========================================================================== */
/* 2. The map                                                                 */
/* ========================================================================== */

/**
 * inputChannelCount reads the negotiated width out of whatever GetChannelMap
 * reported, clamped to 0..MAX_INPUT_CHANNELS.
 *
 * ZERO IS A NORMAL ANSWER and it is the answer before Start: InputChannels() is
 * read from the capture pad's own caps, and there are no caps until something
 * has negotiated. The view draws no grid for it. That is the whole safety
 * property — with no authoritative width there is no width to guess, and a
 * guessed width is the pipeline kill.
 *
 * @param {{inputChannels?: unknown}|null|undefined} state
 * @returns {number}
 */
export function inputChannelCount(state) {
  const n = Number(state?.inputChannels);
  if (!Number.isFinite(n) || n <= 0) return 0;
  return Math.min(MAX_INPUT_CHANNELS, Math.floor(n));
}

/**
 * deviceKeyOf reads WHICH DEVICE a pad report is about, '' for a report that
 * names none.
 *
 * THE EMPTY STRING IS A REAL ANSWER AND IT MUST NOT BE FILLED IN. It is what
 * getChannelMap answers when it could not ask at all (an older build, a failed
 * call), and it matches no selection anywhere — every configured seat keys as at
 * least "native:", including one with no device chosen. So a screen that draws a
 * grid only while the key it holds is the key of the device on screen draws
 * nothing for it, which is the correct reading of "we could not ask": inventing
 * the caller's own key here would turn that into "this device negotiated zero
 * channels", which is a claim about hardware nobody measured.
 *
 * @param {{deviceKey?: unknown}|null|undefined} state
 * @returns {string}
 */
export function deviceKeyOf(state) {
  const key = state?.deviceKey;
  return typeof key === 'string' ? key : '';
}

/**
 * isNeverMapped reports whether a stored map is the ZERO VALUE — gst's
 * IsDefault. It matters on screen and not only in Go: an empty map means
 * "nobody has chosen yet" and resolves to the default routing, so the grid draws
 * channel 1 left and channel 2 right either way. Without this the screen would
 * imply somebody chose that, and the first question after a wrong feed is always
 * whether anyone set it.
 *
 * @param {unknown} map
 * @returns {boolean}
 */
export function isNeverMapped(map) {
  return !Array.isArray(map) || map.length === 0;
}

/**
 * defaultChannelMap mirrors gst.DefaultChannelMap: input 1 to the left, input 2
 * to the right, at unity — and for one channel, that channel to both.
 *
 * It is not merely a sensible default, it is BIT-FOR-BIT THE PREVIOUS
 * BEHAVIOUR: decklinkaudiosrc channels=2 negotiated a positioned stereo pair
 * that audioconvert passed through unchanged, and this map is the identity on
 * those two channels. A seat that never opens this screen hears exactly what it
 * heard before the screen existed. Restated here rather than fetched because the
 * grid has to DRAW it before any pipeline has been asked anything, and
 * channelmap.test.js asserts it against the Go source so the two cannot drift.
 *
 * @param {number} channels
 * @returns {Array<{output: number, input: number, gain: number}>}
 */
export function defaultChannelMap(channels) {
  const width = inputChannelCount({ inputChannels: channels });
  if (width <= 0) return [];
  if (width === 1) {
    return [
      { output: OUTPUT_LEFT, input: 0, gain: 1 },
      { output: OUTPUT_RIGHT, input: 0, gain: 1 },
    ];
  }
  return [
    { output: OUTPUT_LEFT, input: 0, gain: 1 },
    { output: OUTPUT_RIGHT, input: 1, gain: 1 },
  ];
}

/**
 * emptyGrid is the dense working form the SCREEN uses: one row per output, one
 * column per negotiated input, all silent. It is never saved and never sent —
 * mapFromGrid turns it back into the list — it exists because a grid of buttons
 * is a grid, and because a dense cell is what makes two contributions to one
 * cell impossible to express.
 *
 * @param {number} channels
 * @returns {number[][]}
 */
export function emptyGrid(channels) {
  const width = inputChannelCount({ inputChannels: channels });
  return OUTPUTS.map(() => new Array(width).fill(0));
}

/**
 * gridFromMap renders a stored map onto a grid of the pad's width, and reports
 * the channel NUMBERS whose routing had to be dropped to fit.
 *
 * The dropped list is why this returns a pair. A map saved when the pad
 * negotiated sixteen, loaded on a day it negotiates eight, loses half its
 * columns — and the operator has to be TOLD, or the commentator who was on
 * channel 11 is simply not in the feed and nothing on screen ever mentioned
 * them. Go refuses such a map outright, by name; the screen's job is to show
 * what CAN be sent and to say what was lost getting there.
 *
 * An EMPTY map draws the default, because that is what an empty map means
 * (gst.ChannelMap's zero value). A map that is not a list at all draws the
 * default too: a value this screen cannot read is not a licence to take a
 * working seat off air, and the default is the routing that was already there.
 *
 * Two contributions naming one cell — which Go refuses — resolve to the LAST of
 * them here. It cannot arrive from this screen (a grid cell holds one number),
 * only from a hand-edited file, and drawing one of the two beats drawing nothing:
 * the operator can then see the routing and correct it, and what leaves this
 * screen afterwards is a map Go will accept.
 *
 * @param {unknown} map
 * @param {number} channels
 * @returns {{grid: number[][], dropped: number[]}}
 */
export function gridFromMap(map, channels) {
  const grid = emptyGrid(channels);
  const width = grid[0].length;
  const list = isNeverMapped(map) ? defaultChannelMap(width) : map;
  const dropped = new Set();
  for (const c of list) {
    if (!c || typeof c !== 'object') continue;
    const output = Number(c.output);
    const input = Number(c.input);
    if (!OUTPUTS.some((o) => o.index === output)) continue;
    if (!Number.isInteger(input) || input < 0) continue;
    const gain = clampGain(c.gain);
    if (input >= width) {
      // Only a contribution that was CARRYING something counts as a loss.
      if (gain !== 0) dropped.add(input + 1);
      continue;
    }
    grid[output][input] = gain;
  }
  return { grid, dropped: [...dropped].sort((a, b) => a - b) };
}

/**
 * mapFromGrid turns the grid back into the list that is saved and sent.
 *
 * ONLY NON-ZERO CELLS BECOME CONTRIBUTIONS. A cell at zero is not a
 * contribution that happens to be silent, it is the absence of one — Go would
 * accept it (0 is a legal coefficient) and it would make every saved map
 * thirty-two entries long, most of them meaningless, and a diff of two maps
 * unreadable.
 *
 * By construction the result names each cell at most once, which is the refusal
 * gst.MixMatrix makes by name: one cell holds one coefficient, and two
 * contributions to it would have to be summed into a gain nobody chose.
 *
 * @param {number[][]} grid
 * @returns {Array<{output: number, input: number, gain: number}>}
 */
export function mapFromGrid(grid) {
  const map = [];
  for (const out of OUTPUTS) {
    const row = Array.isArray(grid) && Array.isArray(grid[out.index]) ? grid[out.index] : [];
    for (let input = 0; input < row.length; input += 1) {
      const gain = clampGain(row[input]);
      if (gain !== 0) map.push({ output: out.index, input, gain });
    }
  }
  return map;
}

/**
 * describeDropped is what the view says about gridFromMap's second return value.
 * Empty string when nothing was dropped — never an empty flourish on the normal
 * case.
 *
 * @param {number[]} dropped 1-based channel numbers
 * @param {number} channels the width they were dropped to
 * @returns {string}
 */
export function describeDropped(dropped, channels) {
  if (!Array.isArray(dropped) || dropped.length === 0) return '';
  const which = dropped.length === 1 ? 'Channel' : 'Channels';
  return (
    `${which} ${joinNumbers(dropped)} carried routing in the saved map, but this capture only ` +
    `negotiated ${channels} channel${channels === 1 ? '' : 's'} — that routing has been dropped ` +
    'rather than sent, because a map naming a channel the stream does not have does not degrade ' +
    'the feed, it stops it. Re-route anyone who was on those channels.'
  );
}

/**
 * describePad is the line under the lamps that says where the grid came from,
 * and at width 1 it is the line that says what a two-row, one-column grid IS.
 *
 * THE MONO CASE NEEDS A SENTENCE OF ITS OWN. A 2x1 grid is two buttons in a
 * column, and two buttons in a column do not look like a routing decision — they
 * look like a screen that failed to draw. What they actually offer is the choice
 * between dual mono (both pressed, which is the default and what every mono seat
 * already had) and a single-sided feed, and half of an audience cannot hear the
 * second one. So it is stated rather than inferred.
 *
 * WIDTH 0 IS NOT "PRESS START" ANY MORE, and the copy that said so is gone with
 * the design it described. Capture is built at launch and held to quit; a width
 * of zero now means this input has not negotiated yet — it is opening, or it
 * failed to open — and pressing START would not change that by one buffer. The
 * fault itself is named under the picker (renderAudioInputNote) and on the
 * capture state, which are the places that can say WHICH thing went wrong.
 *
 * @param {number} channels the negotiated width
 * @param {boolean} neverMapped gst.ChannelMap.IsDefault: nobody has chosen yet
 * @returns {string}
 */
export function describePad(channels, neverMapped) {
  const width = inputChannelCount({ inputChannels: channels });
  if (width <= 0) {
    return 'This input has not negotiated a channel count yet, so there is nothing to size a grid from.';
  }
  const sized =
    width === 1
      ? 'One input channel: route it to both sides to be heard by everyone, or to one for a single-sided feed.'
      : `Sized from the ${width} channels this capture negotiated.`;
  if (!neverMapped) return sized;
  // "Nobody has chosen yet" is a different statement from the routing it draws,
  // and the difference is the first question asked after a wrong feed. gst's
  // IsDefault exists for exactly this clause.
  const untouched =
    width === 1
      ? 'Nothing routed yet — the default sends this channel to both sides.'
      : 'Nothing routed yet — the default is channel 1 left, channel 2 right.';
  return `${sized} ${untouched}`;
}

/**
 * describeRoutingHeading names the routing group from the NEGOTIATED WIDTH and
 * the device it belongs to, which is what the heading used to assert instead.
 *
 * It used to read "DeckLink channel routing" on every seat that could see it,
 * because only a card seat ever could. Both halves of that are gone: any
 * multichannel device gets this grid, and a heading naming the wrong hardware is
 * worse than a generic one — an operator on a Focusrite reads "DeckLink" and
 * concludes the panel is not about them.
 *
 * The width is IN THE HEADING because it is the one number that decides whether
 * the grid below is the routing they expected. Sixteen crosspoints where two
 * were expected is a device that opened the wrong way, and it is visible from
 * across the room in the heading and not in the grid.
 *
 * @param {number} channels the negotiated width
 * @param {string} [deviceName] the picker's own label for the device, if known
 * @returns {string}
 */
export function describeRoutingHeading(channels, deviceName = '') {
  const width = inputChannelCount({ inputChannels: channels });
  if (width <= 0) return 'Channel routing';
  const count = `${width} channel${width === 1 ? '' : 's'}`;
  const name = typeof deviceName === 'string' ? deviceName.trim() : '';
  return name === '' ? `Channel routing — ${count}` : `Channel routing — ${name}, ${count}`;
}

/** joinNumbers renders channel numbers the way a person says them: "3, 7 and 11". */
function joinNumbers(numbers) {
  const list = numbers.map(String);
  if (list.length <= 1) return list.join('');
  return `${list.slice(0, -1).join(', ')} and ${list[list.length - 1]}`;
}

/* ========================================================================== */
/* 3. What summing costs, stated before it is discovered                      */
/* ========================================================================== */

/**
 * ROUTER_CAVEAT is the permanent statement above the grid. It is prose in the
 * flow of the screen and it is NOT a tooltip or a title attribute: the fact it
 * carries — that this path has no headroom and two channels at unity will clip —
 * is one an operator needs before they route the second channel.
 *
 * Every clause is a measurement. The clamp is GStreamer's, verified at 1.0
 * accepted and 1.0000001 refused with the meter not moving by 0.0000 dB; the
 * matrix is the last gain stage before the meter and the encoder, so there is
 * nothing downstream to absorb a sum; and the live write took 119 µs with the
 * pipeline staying PLAYING, which is why this is a real-time control rather than
 * an apply-and-restart form.
 */
// The four facts an operator cannot recover from the grid itself, in one line:
// what this is, where the ceiling is, what sharing an output costs, and that it
// is live. The instruction that followed them ("turn a contribution down rather
// than expecting the feed to take it") is what the third fact means, and the
// reason there is no headroom to absorb it — no gain stage after this one — is
// in this constant's own doc comment above, which is where somebody proposing to
// allow boost will read it.
export const ROUTER_CAVEAT =
  'A ROUTER, not a mixer: 0 dB is the maximum, two channels into one output ADD TOGETHER and clip, ' +
  'and every change here is live immediately.';

/**
 * outputSummary is the arithmetic behind that caveat, per output: which channels
 * feed it, what the worst case sums to, and whether that clips. It is the
 * screen's twin of gst.ChannelMap.OutputGain, computed from the grid the
 * operator is looking at so the number moves as they drag.
 *
 * THE SUM IS OF MAGNITUDES, and that is deliberately the pessimistic reading.
 * Two contributions only reach their arithmetic sum when they are correlated —
 * the same commentator on two channels, or a microphone and its own spill — and
 * that is exactly what this grid gets used for, so the honest bound assumes it.
 * Uncorrelated sources will usually peak lower; nothing here promises they will
 * not peak here.
 *
 * @param {number[][]} grid
 * @param {number} output zero-based output index
 * @returns {{output: number, sources: number[], sum: number, clips: boolean}}
 */
export function outputSummary(grid, output) {
  const row = Array.isArray(grid) && Array.isArray(grid[output]) ? grid[output] : [];
  const sources = [];
  let sum = 0;
  for (let i = 0; i < row.length; i += 1) {
    const magnitude = Math.abs(clampGain(row[i]));
    if (magnitude > 0) {
      sources.push(i + 1);
      sum += magnitude;
    }
  }
  // ROUNDED TO THE RESOLUTION IT IS SHOWN AT, and the verdict is taken from the
  // rounded number rather than the raw one. That is the property that makes the
  // warning checkable: an operator reading "worst case 1.00 of full scale — WILL
  // CLIP" cannot act on it, because the number they can see says it does not.
  // Two gains rounded to six places, or a -6 dB trim that is really 0.501187,
  // sum to a hair over unity — at most 0.04 dB at this resolution, which is
  // below audibility and below everything else on this screen.
  const rounded = Math.round(sum * 100) / 100;
  return { output, sources, sum: rounded, clips: rounded > 1 };
}

/**
 * describeOutputSummary is the sentence under the grid, one per output. Three
 * cases, because they need three different things said:
 *
 *   nothing routed  — a silent leg of the feed, which is worth saying plainly:
 *                     it is the state a half-finished route leaves behind.
 *   sum <= 1        — the number, so the margin is visible while it still is one.
 *   sum > 1         — what will happen and what to do. Never "invalid": the map
 *                     is legal, Go will build it and the feed will carry it. It
 *                     is the AUDIO that clips, on loud passages, which is a
 *                     different claim and the only true one.
 *
 * @param {{output: number, sources: number[], sum: number, clips: boolean}} summary
 * @returns {string}
 */
export function describeOutputSummary(summary) {
  const out = OUTPUTS[summary.output];
  const name = out ? out.label : `Output ${summary.output + 1}`;
  if (summary.sources.length === 0) {
    return `${name}: nothing routed — this side of the feed is silent.`;
  }
  const which = summary.sources.length === 1 ? 'channel' : 'channels';
  const head = `${name}: ${which} ${joinNumbers(summary.sources)}, worst case ${summary.sum.toFixed(2)} of full scale`;
  if (!summary.clips) return `${head}.`;
  return (
    `${head} — LOUD PASSAGES WILL CLIP. Turn one of these contributions down; there is no gain ` +
    'stage after this one to absorb it.'
  );
}

/* ========================================================================== */
/* 4. The two lamps: no camera and no audio are different faults              */
/* ========================================================================== */

/**
 * The lamp names. They are separate lamps rather than one "card" lamp because
 * the two faults have NOTHING in common at the desk: no video signal is a cable,
 * a router crosspoint or a source that is off, and it is fixed in the machine
 * room; no audio on any channel is an embedder, a mute or a microphone, and it
 * is fixed at the position. One lamp covering both would send whoever reads it
 * to the wrong place half the time.
 *
 * LAMP_AUDIO LOST THE WORD "CARD" because the grid did: it now reports the
 * channels of whatever device commentary is captured from, and a lamp reading
 * CARD AUDIO beside a Focusrite's eight meters is a lamp about a machine that is
 * not in the path.
 *
 * LAMP_VIDEO KEPT IT, and is drawn only on a card seat — see the view's
 * setPad. It is genuinely about the card and it is genuinely about audio: a
 * decklinkaudiosrc cannot preroll without a decklinkvideosrc in the same
 * pipeline, so on a card seat "no video signal" is the explanation for "no audio
 * on any channel", and the two lamps are read together. On a microphone seat
 * there is no such coupling and the lamp would be a permanent NOT MEASURED next
 * to a working input — furniture that teaches an operator to ignore a grey lamp.
 */
export const LAMP_VIDEO = 'CARD VIDEO';
export const LAMP_AUDIO = 'INPUT AUDIO';

/**
 * SIGNAL_STATE mirrors gst.SignalState EXACTLY, and it is UPPERCASE. The three
 * strings are the contract with app.go's signalPayload; channelmap.test.js
 * asserts them against internal/gst/signalwatch.go itself, because a copy made
 * from the wrong neighbouring enum would compare unequal to every event that
 * arrives and leave the lamp sitting on whatever was last recognised.
 */
export const SIGNAL_STATE = Object.freeze({
  UNKNOWN: 'UNKNOWN',
  OK: 'OK',
  LOST: 'LOST',
});

/**
 * SIGNAL_FLAP_ALERT mirrors internal/gst's signalFlapAlert: how many raw
 * transitions since the last report force one out of the watchdog.
 *
 * It is needed HERE because `flaps` is not a boolean. A perfectly ordinary
 * signal acquisition reports OK with a flap or two — the transition itself is a
 * flap — and a lamp that read "unstable" on any non-zero count would sit amber
 * for the rest of the match after one clean lock. A count at or above the alert
 * threshold is the report the watchdog was FORCED to send, which is the one that
 * means a marginal input.
 */
export const SIGNAL_FLAP_ALERT = 4;

/**
 * deriveSignalLamp turns one "signal" event payload — {state, flaps} — into the
 * {level, text} shape every lamp in this application takes.
 *
 *   UNKNOWN         grey  'NOT MEASURED'
 *   OK              green 'SIGNAL'
 *   OK + flapping   amber 'UNSTABLE (n)'
 *   LOST            red   'NO SIGNAL'
 *
 * UNKNOWN IS NOT A FAULT and must never be drawn as one. It is the state of
 * every machine with no capture card in it — which is every machine running this
 * application today — and of every session before the first measurement. It
 * means this application cannot tell, which is a different thing from a card
 * telling us there is nothing there, and the difference is the whole reason
 * app.go's payload has three states rather than a boolean.
 *
 * THERE IS NO HYSTERESIS HERE, deliberately. The debounce is internal/gst's,
 * with asymmetric hold-offs measured against the real card, and a second one on
 * this side would be two filters in series that nobody could reason about — the
 * lamp would lag a real loss by however long both took to agree.
 *
 * The AMBER case is the one place this file makes a judgement of its own, and it
 * is worth stating: app.go offers `flaps` as an "unstable" qualifier beside an
 * otherwise green lamp. It is drawn amber instead, because green on this screen
 * means "nothing to do here" and an input that has dropped lock four times since
 * the last report is something to do — before it drops lock during the match
 * rather than during the line-up. The count is in the text so the claim is
 * checkable rather than atmospheric.
 *
 * @param {{state?: unknown, flaps?: unknown}|null|undefined} payload
 * @returns {{level: string, text: string}}
 */
export function deriveSignalLamp(payload) {
  const state = payload && typeof payload === 'object' ? payload.state : undefined;
  const flaps = Number(payload?.flaps);
  switch (state) {
    case SIGNAL_STATE.LOST:
      return { level: LEVEL.RED, text: 'NO SIGNAL' };
    case SIGNAL_STATE.OK:
      if (Number.isFinite(flaps) && flaps >= SIGNAL_FLAP_ALERT) {
        return { level: LEVEL.AMBER, text: `UNSTABLE (${Math.floor(flaps)})` };
      }
      return { level: LEVEL.GREEN, text: 'SIGNAL' };
    default:
      // UNKNOWN, an empty state, a payload from a build that sends something
      // else: all of them mean "this application cannot tell". Grey, and never
      // green — a lamp that reads good on a payload it did not understand is
      // the one failure a status lamp may not have.
      return { level: LEVEL.GREY, text: 'NOT MEASURED' };
  }
}

/**
 * deriveAudioLamp turns one per-channel levels frame into the INPUT AUDIO lamp.
 *
 *   no frame           grey  'NO LEVELS'   nothing is being captured yet
 *   every channel dead amber 'NO AUDIO ON ANY CHANNEL'
 *   otherwise          green 'AUDIO ON n OF m'
 *
 * AMBER, NOT RED, for the silent case, and the choice is not cosmetic. Total
 * silence is what a rehearsal gap looks like, and what a capture being torn down
 * and rebuilt looks like (the zero-frame, which is now a DEVICE CHANGE rather
 * than the end of a session — capture outlives STOP); red would be spent on a
 * state that resolves itself when somebody speaks, and a red lamp that goes
 * green on its own teaches the desk to ignore red. It is a different COLOUR, a
 * different GLYPH and a different NAME from the video lamp beside it, which is
 * the point: no camera and no audio must never be mistaken for one another.
 *
 * "Live" is measured at the meter's own floor (-60 dBFS, from meters.js) rather
 * than at the digital-silence clamp: a channel sitting at -80 dBFS of noise
 * floor is not a channel with a commentator on it, and counting it would make
 * this lamp green on an input whose channels are connected to nothing.
 *
 * @param {{peak?: unknown}|null|undefined} frame
 * @param {number} channels the negotiated count, for the "of m"
 * @returns {{level: string, text: string}}
 */
export function deriveAudioLamp(frame, channels) {
  if (!frame || !Array.isArray(frame.peak) || frame.peak.length === 0) {
    return { level: LEVEL.GREY, text: 'NO LEVELS' };
  }
  if (isSilentFrame(frame)) {
    return { level: LEVEL.AMBER, text: 'NO AUDIO ON ANY CHANNEL' };
  }
  const live = frame.peak.filter((db) => typeof db === 'number' && db > LEVELS_FLOOR_DB).length;
  if (live === 0) {
    // Above the digital-silence clamp but below the meter's floor: a noise
    // floor, not a commentator. Said the same way as total silence, because the
    // fix is the same one.
    return { level: LEVEL.AMBER, text: 'NO AUDIO ON ANY CHANNEL' };
  }
  const total = channels > 0 ? channels : frame.peak.length;
  return { level: LEVEL.GREEN, text: `AUDIO ON ${live} OF ${total}` };
}

/* ========================================================================== */
/* 5. The view                                                                */
/* ========================================================================== */

/**
 * createChannelMapView builds the routing screen and returns the handles
 * settings.js drives it through.
 *
 * handlers = { onChange(map) }, called with the WHOLE map every time the
 * operator changes anything. Every time, with no debounce and no apply button:
 * the live write was measured at 119 µs with no renegotiation and the pipeline
 * staying PLAYING, so the honest rendering of this control is one that acts as
 * it is touched. An Apply button here would describe a restriction that does not
 * exist.
 *
 * The returned object:
 *   el                   the element to append into a settings group
 *   setPad(state)        GetChannelMap's report: sizes the grid to a WIDTH AND A
 *                        DEVICE, or removes it
 *   setMap(v, deviceKey) the stored map and the device it belongs to
 *   collect()            the map for collectConfig()
 *   setLevels(f)         one per-channel levels frame; paints the meters and the lamp
 *   setSignal(p)         one "signal" payload; paints the video lamp
 *
 * BOTH THE PAD AND THE MAP CARRY A DEVICE KEY, and they are two different
 * questions: the pad's is which device NEGOTIATED, the map's is which device the
 * routing on screen was LOADED for. They agree in the steady state and disagree
 * for exactly as long as a device change takes to renegotiate — see collect(),
 * which is the one place that disagreement can do damage.
 */
export function createChannelMapView(handlers = {}) {
  const el = document.createElement('div');
  el.className = 'channelmap';

  // THE CAVEAT COMES FIRST, ABOVE THE GRID. Not beside it, not under it, and
  // not on a hover: it is what stops the second route being made wrongly, so it
  // is read before the first one is made.
  const caveat = document.createElement('p');
  caveat.className = 'channelmap-caveat';
  caveat.textContent = ROUTER_CAVEAT;

  // The two lamps, in the row idiom the main screen uses. Built through lamps.js
  // so a lamp here is the same object a lamp there is.
  const lampRow = createLampRow([LAMP_VIDEO, LAMP_AUDIO]);
  lampRow.el.classList.add('channelmap-lamps');

  // What is on screen INSTEAD of the grid when there is no negotiated width, and
  // the "never mapped" line when there is one.
  const padNote = document.createElement('p');
  padNote.className = 'field-hint channelmap-pad-note';

  // The dropped-routing warning: visible only when a saved map named a channel
  // the pad did not negotiate. See gridFromMap.
  const droppedNote = document.createElement('p');
  droppedNote.className = 'channelmap-dropped';
  droppedNote.hidden = true;

  const grid = document.createElement('div');
  grid.className = 'channelmap-grid';
  grid.setAttribute('role', 'group');
  grid.setAttribute('aria-label', 'Input channel routing');

  const sums = document.createElement('div');
  sums.className = 'channelmap-sums';
  const sumLines = OUTPUTS.map(() => {
    const p = document.createElement('p');
    p.className = 'channelmap-sum';
    sums.appendChild(p);
    return p;
  });

  const trim = createTrimPanel();

  el.append(caveat, lampRow.el, padNote, droppedNote, grid, sums, trim.el);

  // --- state ---------------------------------------------------------------
  //
  // `stored` is the map as it will be saved and sent — the list. `cells` is the
  // dense rendering of it at the pad's width, which is what the buttons read and
  // write. Every operator edit rewrites `stored` from `cells`, so re-rendering
  // after a width change never restores a value the operator has since replaced.
  /** @type {unknown} the last map handed in by populate, or produced by an edit */
  let stored = null;
  /** @type {number[][]} OUTPUTS.length x padChannels; empty when there is no grid */
  let cells = [];
  let padChannels = 0;
  /**
   * padKey is the device the WIDTH is about; storedKey the device the MAP is
   * about. Both are "" until something says otherwise, and "" matches no
   * configured seat — see deviceKeyOf.
   */
  let padKey = '';
  let storedKey = '';
  /** True while the stored map is the zero value: the default, chosen by nobody. */
  let neverMapped = true;
  /** @type {Array<{meter: object, buttons: object[]}>} one per input channel */
  let rows = [];
  /** @type {{input: number, output: number}|null} the crosspoint the trim edits */
  let selected = null;

  const peakHold = createPeakHold();

  // --- the grid ------------------------------------------------------------

  /**
   * buildGrid rebuilds the rows for the current pad width. Called only when the
   * width or the DEVICE changes: a rebuild drops the operator's selection and
   * every meter's DOM node, so doing it on every state change would make the
   * meters flicker at the levels event's rate.
   *
   * ON SCREEN IT IS THE OTHER WAY ROUND FROM THE MODEL, deliberately, and this
   * is the one place the two orientations meet. `cells` is OUTPUT ROWS x INPUT
   * COLUMNS because the matrix is; the DOM below is one row PER INPUT, with the
   * two outputs as columns, because an operator reads down a list of channels
   * looking for the one somebody is talking on — and at 32 channels a grid the
   * model's way round would be 32 columns wide and unreadable. Every cell is
   * still addressed `cells[output][input]`; see OUTPUTS on why getting that
   * backwards is invisible at 2x2 and fatal at 2x8.
   */
  function buildGrid() {
    grid.textContent = '';
    rows = [];
    selected = null;
    grid.hidden = padChannels === 0;
    if (padChannels === 0) return;

    // The header row. Plain elements rather than a <table>: the row is a CSS
    // grid so the meter column can take the slack at any window width, and the
    // accessible naming is carried by each button's own aria-label — which has
    // to spell the crosspoint out in full anyway, because a screen-reader user
    // cannot see which column a cell is in.
    grid.appendChild(headerCell('Input'));
    grid.appendChild(headerCell('Level'));
    for (const out of OUTPUTS) grid.appendChild(headerCell(out.label));

    for (let input = 0; input < padChannels; input += 1) {
      const name = document.createElement('span');
      name.className = 'channelmap-name';
      name.textContent = `Ch ${input + 1}`;
      grid.appendChild(name);

      const meter = buildMeter();
      grid.appendChild(meter.el);

      const buttons = OUTPUTS.map((out) => {
        const button = buildCell(input, out.index);
        grid.appendChild(button.el);
        return button;
      });

      rows.push({ meter, buttons });
    }
  }

  function headerCell(text) {
    const cell = document.createElement('span');
    cell.className = 'channelmap-head';
    cell.textContent = text;
    return cell;
  }

  /**
   * buildMeter is one channel's horizontal meter. The ZONES and the SCALE come
   * from meters.js — the mixer's, by import — and only the geometry differs from
   * the vertical pair beside the picture: segments are fixed slices of the bar's
   * WIDTH here rather than its height. The colours are the input meters' own
   * classes, so one rule decides what amber looks like.
   */
  function buildMeter() {
    const bar = document.createElement('div');
    bar.className = 'channelmap-meter';
    const fills = meterZones().map(({ zone, from, to }) => {
      const seg = document.createElement('div');
      seg.className = 'channelmap-meter-seg';
      seg.style.flexBasis = `${((to - from) * 100).toFixed(1)}%`;
      const fill = document.createElement('div');
      fill.className = `channelmap-meter-fill input-meter-fill--${zone}`;
      seg.appendChild(fill);
      bar.appendChild(seg);
      return fill;
    });
    const peakMark = document.createElement('div');
    peakMark.className = 'channelmap-meter-peak';
    peakMark.hidden = true;
    bar.appendChild(peakMark);
    return { el: bar, fills, peakMark };
  }

  /**
   * buildCell is one crosspoint. A button, because that is what it is: pressing
   * it routes or unroutes the channel at unity, which is the whole interaction
   * for the overwhelming majority of seats. Pressing it also aims the trim panel
   * at it, so the fine adjustment is one control on the screen rather than
   * thirty-two.
   */
  function buildCell(input, output) {
    const btn = document.createElement('button');
    btn.type = 'button'; // never submit: this lives inside the settings <form>
    btn.className = 'channelmap-cell';
    btn.dataset.input = String(input);
    btn.dataset.output = String(output);
    btn.addEventListener('click', () => {
      // One press does both. Aiming without toggling would need a second gesture
      // nobody would guess; toggling without aiming would leave the trim pointing
      // at whatever was touched before, which is how a trim lands on the wrong
      // commentator.
      const current = clampGain(cells[output][input]);
      setGain(input, output, current === 0 ? 1 : 0);
      select(input, output);
    });
    return { el: btn, input, output };
  }

  /** renderCells repaints every crosspoint from the working grid. */
  function renderCells() {
    for (let input = 0; input < rows.length; input += 1) {
      for (const button of rows[input].buttons) {
        const gain = clampGain(cells[button.output][input]);
        const routed = gain !== 0;
        button.el.textContent = describeGain(gain);
        button.el.classList.toggle('channelmap-cell--routed', routed);
        button.el.classList.toggle('channelmap-cell--selected', isSelected(input, button.output));
        button.el.setAttribute('aria-pressed', String(routed));
        button.el.setAttribute(
          'aria-label',
          `${crosspointLabel(input, button.output)}: ${routed ? describeGain(gain) : 'not routed'}`,
        );
      }
    }
  }

  function isSelected(input, output) {
    return selected !== null && selected.input === input && selected.output === output;
  }

  /** renderSums redraws the per-output arithmetic under the grid. */
  function renderSums() {
    OUTPUTS.forEach((out, i) => {
      if (padChannels === 0) {
        sumLines[i].textContent = '';
        sumLines[i].classList.remove('channelmap-sum--clip');
        return;
      }
      const summary = outputSummary(cells, out.index);
      sumLines[i].textContent = describeOutputSummary(summary);
      // The clip case is marked as well as worded: the sentence says what will
      // happen, the mark is what makes it findable from across the room. Never
      // the mark alone — see the caveat's own reasoning.
      sumLines[i].classList.toggle('channelmap-sum--clip', summary.clips);
    });
    sums.hidden = padChannels === 0;
  }

  // --- the trim panel ------------------------------------------------------

  /**
   * createTrimPanel is the ONE fine-adjustment control on the screen, aimed at
   * whichever crosspoint was last touched.
   *
   * Thirty-two sliders would be the obvious alternative and it is the wrong one:
   * at sixteen rows they would be too small to hit, and the screen would read as
   * a mixing desk — which is exactly the thing this path is not. One trim, named
   * with the crosspoint it points at, keeps the grid readable as a router.
   *
   * The slider's TOP END IS 0 dB and there is no travel above it. That is the
   * clamp made visible: a control that could ask for +3 dB would be a control
   * whose request GStreamer discards in silence, leaving the previous matrix in
   * force with nothing on screen or in the meter to show it.
   */
  function createTrimPanel() {
    const panel = document.createElement('div');
    panel.className = 'channelmap-trim';

    const title = document.createElement('p');
    title.className = 'channelmap-trim-title';

    const controls = document.createElement('div');
    controls.className = 'channelmap-trim-controls';

    const slider = document.createElement('input');
    slider.type = 'range';
    slider.min = String(TRIM_FLOOR_DB);
    slider.max = '0';
    slider.step = '0.5';
    slider.className = 'channelmap-trim-slider';

    const readout = document.createElement('span');
    readout.className = 'channelmap-trim-readout';

    const offBtn = document.createElement('button');
    offBtn.type = 'button';
    offBtn.className = 'btn btn-ghost btn-small';
    offBtn.textContent = 'Off';

    const unityBtn = document.createElement('button');
    unityBtn.type = 'button';
    unityBtn.className = 'btn btn-ghost btn-small';
    unityBtn.textContent = '0 dB';

    // POLARITY. It is here because the map can express it — a negative gain is a
    // real thing to want on a desk that has sent a leg out of phase — and
    // therefore the operator has to be able to see and undo it. A screen that
    // could only set positive gains would silently flip a mis-wired pair back the
    // first time anybody dragged its trim.
    const invertLabel = document.createElement('label');
    invertLabel.className = 'channelmap-trim-invert';
    const invert = document.createElement('input');
    invert.type = 'checkbox';
    invertLabel.append(invert, document.createTextNode(' Invert polarity (ø)'));

    controls.append(slider, readout, offBtn, unityBtn, invertLabel);
    panel.append(title, controls);

    return { el: panel, title, slider, readout, offBtn, unityBtn, invert };
  }

  /** select aims the trim panel at one crosspoint and redraws both. */
  function select(input, output) {
    selected = { input, output };
    renderCells();
    renderTrim();
  }

  /** renderTrim redraws the trim panel from the selected crosspoint. */
  function renderTrim() {
    const active = selected !== null && padChannels > 0;
    trim.el.hidden = padChannels === 0;
    for (const control of [trim.slider, trim.offBtn, trim.unityBtn, trim.invert]) {
      control.disabled = !active;
    }
    if (!active) {
      trim.title.textContent = 'Select a crosspoint above to set its level.';
      trim.readout.textContent = '';
      return;
    }
    const gain = clampGain(cells[selected.output][selected.input]);
    const db = gainToDb(gain);
    trim.title.textContent = crosspointLabel(selected.input, selected.output);
    // An unrouted crosspoint parks the slider at the floor rather than leaving it
    // wherever the last one was: the control must not read as a level the
    // contribution does not have.
    trim.slider.value = String(db === null ? TRIM_FLOOR_DB : db);
    trim.readout.textContent = db === null ? 'not routed' : `${db.toFixed(1)} dB`;
    trim.invert.checked = isInverted(gain);
  }

  trim.slider.addEventListener('input', () => {
    if (selected === null) return;
    setGain(selected.input, selected.output, dbToGain(Number(trim.slider.value), trim.invert.checked));
  });
  trim.offBtn.addEventListener('click', () => {
    if (selected === null) return;
    setGain(selected.input, selected.output, 0);
  });
  trim.unityBtn.addEventListener('click', () => {
    if (selected === null) return;
    setGain(selected.input, selected.output, dbToGain(0, trim.invert.checked));
  });
  trim.invert.addEventListener('change', () => {
    if (selected === null) return;
    const gain = clampGain(cells[selected.output][selected.input]);
    // Flipping polarity on an unrouted crosspoint must not route it: the tick box
    // describes HOW a contribution arrives, not whether it does.
    if (gain === 0) {
      renderTrim();
      return;
    }
    setGain(selected.input, selected.output, trim.invert.checked ? -Math.abs(gain) : Math.abs(gain));
  });

  // --- edits ---------------------------------------------------------------

  /**
   * setGain is the ONE write path. Every control funnels through it, so there is
   * exactly one place that clamps, one that rebuilds the map from the grid, and
   * one that tells the caller — which is what keeps the live routing and the
   * saved document from ever describing different maps.
   *
   * The first edit on a never-mapped seat turns the DEFAULT into an explicit map,
   * because the grid it is computed from was drawn from the default. That is the
   * intended reading: touching the routing is choosing it, and from then on the
   * screen stops saying nobody has.
   */
  function setGain(input, output, gain) {
    if (padChannels === 0) return;
    cells[output][input] = clampGain(gain);
    stored = mapFromGrid(cells);
    neverMapped = false;
    renderCells();
    renderSums();
    renderTrim();
    renderPad();
    if (typeof handlers.onChange === 'function') handlers.onChange(mapFromGrid(cells));
  }

  /**
   * adoptStored re-renders the working grid at the current pad width and reports
   * anything that would not fit. It does NOT call onChange: this runs on load and
   * on a width change, neither of which is an operator edit, and telling the Go
   * side to apply a map it has just told us about is a loop.
   */
  function adoptStored() {
    neverMapped = isNeverMapped(stored);
    if (padChannels === 0) {
      cells = [];
      droppedNote.hidden = true;
      return;
    }
    const { grid: rendered, dropped } = gridFromMap(stored, padChannels);
    cells = rendered;
    const text = describeDropped(dropped, padChannels);
    droppedNote.textContent = text;
    droppedNote.hidden = text === '';
  }

  /**
   * renderPad draws the line that says where the grid came from, and at width 1
   * the line that says what the mono case is. The wording is describePad's, up
   * in the pure half, so `node --test` drives it for real — including the two
   * narrow widths, which are the ones with the newest copy on them and the least
   * exercise at a desk.
   */
  function renderPad() {
    padNote.textContent = describePad(padChannels, neverMapped);
  }

  // --- meters --------------------------------------------------------------

  /**
   * setLevels paints every channel's meter from one per-channel levels frame and
   * moves the audio lamp with it.
   *
   * This is what makes the grid usable: nobody knows which of sixteen embedded
   * channels a commentator is on, and the way they find out is to have that
   * person talk and watch which bar moves. The frames are the CAPTURE's own
   * channels, measured upstream of the mix down to two — array index i is input
   * channel i, and it stays index i for the life of the pipeline (measured:
   * sixteen sources at 3 dB steps came back in input order). The stereo meters on
   * the main screen are the other side of the matrix and cannot answer the
   * question at all.
   *
   * The frame is normalised to exactly padChannels entries (meters.js's
   * frameChannels), so a frame that arrives with the wrong number of channels
   * paints silence on the rest rather than leaving stale bars standing.
   */
  function setLevels(frame) {
    lampRow.lamps[LAMP_AUDIO].update(deriveAudioLamp(frame, padChannels));
    if (rows.length === 0) return;
    const silent = isSilentFrame(frame);
    grid.classList.toggle('channelmap-grid--idle', silent);
    if (silent) {
      peakHold.reset();
      for (const row of rows) {
        row.meter.fills.forEach((fill) => {
          fill.style.width = '0%';
        });
        row.meter.peakMark.hidden = true;
      }
      return;
    }

    const { peak, rms } = frameChannels(frame, rows.length);
    const marks = peakHold.update(peak);
    rows.forEach((row, i) => {
      const fills = zoneFills(rms[i]);
      row.meter.fills.forEach((fill, z) => {
        // Rounded to 0.1% for the reason home.js rounds: a repaint ten times a
        // second must not rewrite the style attribute with a seventeen-digit
        // float.
        fill.style.width = `${(Math.round(fills[z] * 1000) / 10).toFixed(1)}%`;
      });
      const frac = dbToFraction(marks[i]);
      row.meter.peakMark.hidden = !(frac > 0);
      row.meter.peakMark.style.left = `${(Math.round(frac * 1000) / 10).toFixed(1)}%`;
    });
  }

  // --- the handles settings.js drives --------------------------------------

  /**
   * setPad takes GetChannelMap's report and sizes the grid to the width AND the
   * device in it. A report that changes NEITHER does not rebuild — it arrives on
   * an event as well as on open, and rebuilding would drop the operator's
   * selection while they were using it.
   *
   * THE DEVICE IS HALF THE KEY AND THE HALF THAT WAS MISSING. Two devices can
   * negotiate the same width — a stereo microphone and a stereo interface both
   * report 2 — and a grid that rebuilt only on the number would carry the first
   * one's routing straight onto the second, silently, at a width where it fits
   * perfectly and so goes unnoticed. `channels !== padChannels || key !== padKey`
   * and never just the first.
   */
  function setPad(state) {
    const channels = inputChannelCount(state);
    const key = deviceKeyOf(state);
    if (channels !== padChannels || key !== padKey) {
      padChannels = channels;
      padKey = key;
      adoptStored();
      buildGrid();
    }
    // The card lamp is furniture on a microphone seat — see LAMP_VIDEO. Driven
    // from the pad's own key rather than from a kind passed in beside it,
    // because the key is what the width came stamped with and a second opinion
    // about which device is open is the drift this stamp exists to remove.
    lampRow.lamps[LAMP_VIDEO].el.hidden = captureKindOf(padKey) !== 'decklink';
    renderPad();
    renderCells();
    renderSums();
    renderTrim();
  }

  /**
   * setMap adopts the stored map and the device it is the routing OF, from
   * populate() and from the "channelMap" event. It never calls onChange: loading
   * a configuration is not an operator changing the routing.
   *
   * The key is not decoration — collect() reads it. A caller that omitted it
   * would be saying "this map belongs to no device", which is exactly the state
   * collect() refuses to speak for.
   */
  function setMap(map, deviceKey) {
    stored = map ?? null;
    storedKey = deviceKeyOf({ deviceKey });
    adoptStored();
    renderPad();
    renderCells();
    renderSums();
    renderTrim();
  }

  /**
   * collect is what collectConfig() saves.
   *
   * WITH A GRID FOR THE DEVICE WHOSE MAP IT IS HOLDING it returns the map the
   * grid is showing — so a save writes the routing the operator can see and
   * nothing else, including the narrowing a shrunken pad forced. Otherwise it
   * returns the loaded value untouched: a seat whose device has never been opened
   * must not have its saved map deleted by a Save pressed on an unrelated field,
   * which is exactly what collectConfig does to any field it fails to restate.
   *
   * ===================== THE NOT-MY-DEVICE GUARD =============================
   *
   * `padKey === storedKey` is the second half of that, and it is the half a
   * refusal in Go cannot help with at all. Capture is always live and the device
   * under it can change while this screen is open; selecting an interface does
   * not renegotiate instantly, so there is a window in which the grid is still
   * sized to the PREVIOUS device. A Save landing in that window would write the
   * old device's routing — narrowed or widened to the old width — under the new
   * device's key, silently, with a commentator's channel assignment in it.
   * gst.SetChannelMap validates a LIVE write against InputChannels() and would
   * refuse it, but nothing validates a config.json.
   *
   * When the two disagree the answer is the map as it was loaded, unmodified:
   * the routing this device already had is always a safe thing to write back,
   * and it is the only thing on hand that is certainly about it.
   */
  function collect() {
    if (padChannels > 0 && padKey === storedKey) return neverMapped ? [] : mapFromGrid(cells);
    return Array.isArray(stored) ? stored.map((c) => ({ ...c })) : [];
  }

  /** setSignal paints the video lamp from one "signal" payload. */
  function setSignal(payload) {
    lampRow.lamps[LAMP_VIDEO].update(deriveSignalLamp(payload));
  }

  // Draw once at construction so the group is never blank before the first
  // report arrives. The card lamp starts HIDDEN because padKey starts empty:
  // showing it and taking it away again on the first report would be a lamp
  // flashing onto a microphone seat's screen once per open.
  lampRow.lamps[LAMP_VIDEO].el.hidden = true;
  renderPad();
  renderSums();
  renderTrim();

  return { el, setPad, setMap, collect, setLevels, setSignal };
}
