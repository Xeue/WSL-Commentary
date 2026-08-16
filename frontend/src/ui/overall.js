/**
 * The ONE indicator: an honest reduction of the six lamps to a single state.
 *
 * Owner: WP-5b.
 *
 * The operator: "During the match we only really need an overall status, single
 * green/red indicator and the cough mute buttons. The rest can live in some form
 * of settings tray or something like that."
 *
 * So the main area keeps the picture, this, and the mute controls. The six lamps
 * themselves are not deleted and not hidden behind a click — they move into the
 * side column, permanently visible — because this indicator is a SUMMARY and a
 * summary whose detail cannot be reached is a verdict.
 *
 * Nothing here touches the DOM. home.js renders it; app.js supplies the lamps it
 * already computes for the row, so the indicator and the row can never disagree
 * about what is happening: there is one derivation of each lamp, and this is a
 * pure function of its output.
 *
 * ======================= THE RULE, WRITTEN DOWN =============================
 *
 * "The worst of them" is the obvious answer and it is right for red and amber.
 * It is WRONG for grey, and getting that wrong is the whole difficulty:
 *
 *   worst-of over {red, amber, green, grey} makes this indicator grey on every
 *   position shipping today, for ever, because CAMERA reads grey SLATE on a
 *   slate seat and that is the CORRECT, CONFIGURED, HEALTHY state.
 *
 * Grey is not a rank. It is the absence of one — "not started", "not known",
 * "not applicable" — and three different facts are wearing it:
 *
 *   1. NOTHING IS RUNNING. Every lamp is grey because there is no session. That
 *      is not a fault and must not look like one; it must also never look like
 *      "fine", because nothing is going to air. It is its own state: STANDBY.
 *   2. A SETTLED ANSWER THAT IS NOT GREEN. CAMERA/SLATE is the only one today:
 *      the seat is contributing a slate, on purpose, and the lamp says so. It is
 *      as good as this seat gets, so it does not stop the indicator being GOOD.
 *   3. GENUINELY NOT KNOWN, mid-session. The status socket has gone quiet, or
 *      the monitor module never loaded. Claiming GOOD over that would be the one
 *      failure a status indicator may not have — spec section 8's "never show
 *      stale green" — so it reads WORKING, which is the honest word for "this
 *      application cannot presently see all of it".
 *
 * The four states below are the whole vocabulary, and the reduction is:
 *
 *   any lamp RED                            -> FAULT     (red)
 *   else any lamp AMBER                     -> WORKING   (amber)
 *   else every lamp GREEN                   -> GOOD      (green)
 *   else no session running                 -> STANDBY   (grey)
 *   else every remaining grey is SETTLED    -> GOOD      (green)
 *   else                                    -> WORKING   (amber)
 *
 * ===================== COLOUR IS NEVER THE ONLY SIGNAL ======================
 *
 * The lamp row already encodes level three ways — glyph SHAPE, state TEXT,
 * colour last — for a colourblind commentator and for a monochrome monitor a
 * metre away. This indicator is read from further away than any of them, so it
 * carries the same three and adds a fourth: the WORD is a different length and
 * shape in each state ("GOOD", "WORKING", "FAULT", "STANDBY"), and the glyph is
 * the lamp row's own, so somebody who has learnt ● ▲ ✕ ○ on the lamps has
 * already learnt this.
 *
 * ========================= AND IT NAMES ITS REASON ==========================
 *
 * `detail` is the lamp the verdict came from, in the row's own words:
 * "AUDIO: NO AUDIO (DROPPED?)". A single indicator that reduces six lamps to a
 * colour and then cannot say which one it was is a summary the operator has to
 * distrust — and distrusting the one indicator on the screen is how it stops
 * being looked at. It is empty when everything is green, because there is
 * nothing to name.
 */

import { LEVEL } from './lamps.js';

/**
 * The levels this reduction knows how to weigh. Anything else is treated as
 * unknown rather than trusted — see the mapping in deriveOverallStatus.
 *
 * Built from LEVEL rather than written out, so a fifth level added to the lamp
 * vocabulary cannot silently become "not recognised, therefore fine" here.
 */
const KNOWN_LEVELS = new Set(Object.values(LEVEL));

/**
 * OVERALL is the indicator's four states. The values are the lamp LEVELs, so
 * home.js can paint this with the same class vocabulary and the same glyph table
 * as the row — one visual language, not two.
 */
export const OVERALL = Object.freeze({
  GOOD: 'good',
  WORKING: 'working',
  FAULT: 'fault',
  STANDBY: 'standby',
});

/**
 * OVERALL_WORDS is what each state SAYS. Deliberately four words of four
 * different lengths and no shared prefix: at the distance this is read from,
 * word shape arrives before the letters do.
 */
export const OVERALL_WORDS = Object.freeze({
  [OVERALL.GOOD]: 'GOOD',
  [OVERALL.WORKING]: 'WORKING',
  [OVERALL.FAULT]: 'FAULT',
  // "STANDING BY" rather than "STANDBY", and the reason is the constraint above
  // rather than taste: STANDBY is seven letters and so is WORKING, and two
  // states of the same length and weight are two states that look alike at the
  // distance this is read from. It is also the broadcast word for the thing it
  // describes, which is worth more than the four characters it costs.
  [OVERALL.STANDBY]: 'STANDING BY',
});

/** OVERALL_LEVELS maps each state onto the lamp LEVEL that paints it. */
export const OVERALL_LEVELS = Object.freeze({
  [OVERALL.GOOD]: LEVEL.GREEN,
  [OVERALL.WORKING]: LEVEL.AMBER,
  [OVERALL.FAULT]: LEVEL.RED,
  [OVERALL.STANDBY]: LEVEL.GREY,
});

/**
 * SETTLED_GREY_TEXTS are the grey lamp states that are a real ANSWER rather than
 * an absence of one — case 2 in the header.
 *
 * There is exactly one today and it is deliberately a whitelist of TEXTS rather
 * than of lamp names: what makes CAMERA/SLATE settled is that the seat is
 * configured to send a slate and is sending one, not that the lamp is called
 * CAMERA. A CAMERA lamp reading grey 'NOT MEASURED' — a DeckLink position whose
 * signal event has not arrived — is genuinely not known and must not be waved
 * through by a rule written about its neighbour.
 *
 * Adding to this list is adding a state in which this indicator will read GOOD.
 * That is the whole cost of an entry and it is why the list is short.
 */
export const SETTLED_GREY_TEXTS = Object.freeze(['SLATE']);

const RANK = { [LEVEL.RED]: 3, [LEVEL.AMBER]: 2, [LEVEL.GREEN]: 1, [LEVEL.GREY]: 0 };

/**
 * deriveOverallStatus reduces the lamp row to one state.
 *
 * @param {Array<{name: string, lamp: {level?: string, text?: string}}>} entries
 *        the lamps IN ROW ORDER. Order matters only for `detail`: when two lamps
 *        are equally bad the leftmost is named, which is the one the operator's
 *        eye reaches first on the row this summarises.
 * @param {{running?: boolean}} [session] whether a send session is up. Supplied
 *        by the caller from the same sender state that flips the START/STOP
 *        button, so "STANDBY" and "the button says START" cannot disagree.
 * @returns {{state: string, level: string, text: string, detail: string}}
 */
export function deriveOverallStatus(entries, session) {
  const lamps = (Array.isArray(entries) ? entries : [])
    .filter((e) => e && e.lamp)
    .map((e) => ({
      name: String(e.name ?? ''),
      // AN UNRECOGNISED LEVEL IS NOT A GOOD ONE. `|| LEVEL.GREY` catches the
      // empty and undefined cases, and used to let everything else through
      // verbatim — so a lamp carrying 'RED' (wrong case) or 'critical' matched
      // none of the filters below, fell out of the greys count, and the whole
      // indicator returned GOOD. One lamp screaming, the summary green.
      //
      // This is the single control a commentator glances at to decide whether
      // anything needs their attention, so its one unbreakable property is that
      // it may fail towards concern and never away from it. Anything not in the
      // known vocabulary is now GREY, which reads as "not known" and is handled
      // by the cases below rather than short-circuiting past them.
      level: KNOWN_LEVELS.has(e.lamp.level) ? e.lamp.level : LEVEL.GREY,
      text: String(e.lamp.text ?? ''),
    }));

  // No lamps at all is not "everything is fine". It is the earliest moment of
  // startup, before anything has been derived, and it reads as STANDBY for the
  // same reason case 1 does: nothing claims to be wrong, and nothing is on air.
  if (lamps.length === 0) return build(OVERALL.STANDBY, '');

  const worstFirst = (level) => lamps.find((l) => l.level === level);

  const red = worstFirst(LEVEL.RED);
  if (red) return build(OVERALL.FAULT, `${red.name}: ${red.text}`);

  const amber = worstFirst(LEVEL.AMBER);
  if (amber) return build(OVERALL.WORKING, `${amber.name}: ${amber.text}`);

  const greys = lamps.filter((l) => l.level === LEVEL.GREY);
  if (greys.length === 0) return build(OVERALL.GOOD, '');

  // Case 1: nothing is running. Every grey is "not started", whatever it says.
  if (!session || session.running !== true) {
    return build(OVERALL.STANDBY, '');
  }

  // Cases 2 and 3: a session IS up, so a grey lamp is either a settled answer or
  // a thing this application cannot presently see.
  const unknown = greys.filter((l) => !SETTLED_GREY_TEXTS.includes(l.text));
  if (unknown.length === 0) return build(OVERALL.GOOD, '');
  return build(OVERALL.WORKING, `${unknown[0].name}: ${unknown[0].text}`);
}

function build(state, detail) {
  return {
    state,
    level: OVERALL_LEVELS[state],
    text: OVERALL_WORDS[state],
    detail,
  };
}

/**
 * describeOverall is the indicator's hover/aria text: the word, and the lamp the
 * word came from when there is one.
 *
 * The sentence after the reason is fixed and is the reason this function exists
 * rather than a template at the call site: the indicator summarises SIX lamps
 * and the operator has to know, without being told twice, that the six are one
 * scroll away rather than gone.
 */
export function describeOverall(overall) {
  const word = overall?.text || OVERALL_WORDS[OVERALL.STANDBY];
  const detail = overall?.detail || '';
  const where = 'Every individual lamp is in the column beside the picture.';
  return detail ? `${word} — ${detail}. ${where}` : `${word}. ${where}`;
}

/**
 * rankOf exposes the ordering used above, for callers that need to compare two
 * lamps rather than reduce a row. Grey is 0 and that is not "best": it is
 * OUTSIDE the order, which is exactly why deriveOverallStatus handles it
 * separately rather than by taking a maximum.
 */
export function rankOf(level) {
  return RANK[level] ?? 0;
}
