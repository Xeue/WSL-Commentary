/**
 * THE VIDEO FORMAT THIS SEAT PRODUCES, and the switcher's own format beside it.
 *
 * Owner: WP-5b. Pure — no DOM, no browser API, no backend — so `node --test`
 * drives every rule below for real with nothing installed (videoformat.test.js).
 * settings.js is the wiring; this file is what it wires.
 *
 * ===================== WHY THIS IS A SELECTOR AND NOT A BOX =================
 *
 * videoFormatOverride was a free-text field. The operator's words: "the video
 * format should be a selector, not a free text field and it should show the
 * M2LX format clearly so it is obvious to the users when you diverge". Both
 * halves are failures of the same kind — a raster is not something anybody
 * should be typing from memory twenty minutes before kick-off, and a raster
 * typed correctly but DIFFERENTLY from the switcher's is refused by M2L-X with
 * nothing on this screen to have warned them.
 *
 * ===================== THE STRING STAYS A STRING ============================
 *
 * The config field is still the string "1920x1080p50" and that is deliberate,
 * argued at length in CONTRACT.md: a struct merges field-by-field, so a preset
 * carrying {"width":1280} would leave height at 1080 and conform the feed to
 * 1280x1080 — a raster nobody chose and no switcher accepts. A STRING CANNOT
 * HALF-ARRIVE. This module changes what the operator is shown; it changes
 * nothing about what is stored, and internal/config.ParseVideoFormat is still
 * the authority on what parses.
 *
 * So every value below is a CANONICAL SPELLING that both validate.js and
 * ParseVideoFormat accept, and videoformat.test.js runs the whole list through
 * validateConfig to prove it — an option an operator can select but not save
 * would be worse than the free-text box it replaced.
 *
 * ===================== EMPTY IS THE ANSWER, NOT AN ABSENCE ==================
 *
 * The default is FOLLOW_SWITCHER — the empty string — and it is the right
 * answer on almost every seat: the format is derived from the switcher at
 * START, which is the same derivation every position has always used. An
 * override is the exception, wanted only when the app has nothing to derive
 * from. That is why the follow option is kept OUT of the raster groups and
 * handed to the caller separately: it is not one entry among seventeen, it is
 * the thing the other seventeen are exceptions to.
 */

/**
 * FOLLOW_SWITCHER is the empty override: derive the format from the switcher.
 * It is a named constant because '' reads as "unset" everywhere else in this
 * application and here it is a decision.
 */
export const FOLLOW_SWITCHER = '';

/** The label on the follow option. It says what happens, not what is absent. */
export const FOLLOW_SWITCHER_LABEL = 'Follow the switcher (normal)';

/**
 * The rasters offered, grouped by picture size.
 *
 * WHY THESE AND NOT MORE. The list is the broadcast rasters an M2L-X deployment
 * is actually configured for; a longer list is a longer thing to scan and every
 * entry past the one the operator wants is a cost. Anything genuinely missing
 * still reaches config.json — a value already saved survives (planVideoFormats
 * keeps it), and the field remains a string, so nothing here can make a raster
 * unreachable for ever.
 *
 * The NTSC family is written as the decimals broadcasters write (59.94, not
 * 60000/1001) because that is what ParseVideoFormat accepts and what M2L-X
 * reports; internal/config/videoformat.go's parseVideoFrameRate resolves them
 * to the exact fractions within a 0.005 tolerance.
 */
export const VIDEO_FORMAT_GROUPS = Object.freeze([
  Object.freeze({
    label: '720',
    formats: Object.freeze(['1280x720p50', '1280x720p59.94', '1280x720p60']),
  }),
  Object.freeze({
    label: '1080',
    formats: Object.freeze([
      '1920x1080p23.98',
      '1920x1080p24',
      '1920x1080p25',
      '1920x1080p29.97',
      '1920x1080p30',
      '1920x1080p50',
      '1920x1080p59.94',
      '1920x1080p60',
    ]),
  }),
  Object.freeze({
    label: '2160 (UHD)',
    formats: Object.freeze([
      '3840x2160p25',
      '3840x2160p29.97',
      '3840x2160p30',
      '3840x2160p50',
      '3840x2160p59.94',
      '3840x2160p60',
    ]),
  }),
]);

/**
 * The provenance strings app.go stamps into ConformTargetView.Source. They are
 * the contract; videoformat.test.js pins them against app.go's own constants so
 * the two sides cannot drift into a state where every answer reads as unknown.
 */
export const CONFORM_SOURCE_SESSION = 'session';
export const CONFORM_SOURCE_OVERRIDE = 'override';
/**
 * The INSTANCE'S OWN SETTING, read live over REST by App.GetSwitcherFormat and
 * needing no session. GetConformTarget never stamps it — app.go says so in
 * terms — so a target carrying this source is always the independent number,
 * and that is what makes it usable here.
 */
export const CONFORM_SOURCE_SWITCHER = 'switcher';

/**
 * isIndependentOfTheOperator reports whether a target's raster came from
 * anywhere other than the operator's own videoFormatOverride.
 *
 * ===================== THE ONE RULE BOTH READOUTS OBEY ======================
 *
 * Both functions below exist to let an operator see that their override
 * disagrees with the facility. That comparison is worth nothing unless the
 * number on the other side of it is INDEPENDENT of the override, so this is the
 * single gate both go through, and it is written as an allowlist of the two
 * provenances that qualify rather than as "not override".
 *
 * An allowlist, because the failure modes are not symmetrical. A new provenance
 * that this build does not know about reads as NOT independent, so the readout
 * falls silent — the operator is told nothing, which is recoverable. A denylist
 * would read the same unknown provenance as independent and quote its raster as
 * the switcher's, which is the screen inventing a confirmation and is the one
 * thing a divergence warning must never do.
 *
 * @param {{source?: unknown}|null|undefined} target
 * @returns {boolean}
 */
export function isIndependentOfTheOperator(target) {
  if (!target || typeof target !== 'object') return false;
  return target.source === CONFORM_SOURCE_SESSION || target.source === CONFORM_SOURCE_SWITCHER;
}

/**
 * planVideoFormats decides what the <select> holds and which entry is selected.
 *
 * ===================== A SAVED VALUE IS NEVER DROPPED =======================
 *
 * `saved` may be a raster this list does not carry: a hand-edited config.json, a
 * preset from a facility running something unusual, or simply a format added to
 * Go's parser before it was added here. It is kept, in a group of its own that
 * says where it came from, because the alternative is a <select> that shows
 * 1280x720p50 while config.json says otherwise — the screen and the file
 * disagreeing, which is the fault describeDeviceSelection exists to prevent on
 * the device dropdowns and is no better here.
 *
 * @param {unknown} saved the stored videoFormatOverride
 * @returns {{follow: {value: string, label: string},
 *            groups: Array<{label: string, options: Array<{value: string, label: string}>}>,
 *            value: string}}
 */
export function planVideoFormats(saved) {
  const value = typeof saved === 'string' ? saved.trim() : '';

  const groups = VIDEO_FORMAT_GROUPS.map((g) => ({
    label: g.label,
    options: g.formats.map((f) => ({ value: f, label: f })),
  }));

  const known = value === FOLLOW_SWITCHER || VIDEO_FORMAT_GROUPS.some((g) => g.formats.includes(value));
  if (!known) {
    groups.push({
      label: 'Saved on this machine',
      options: [{ value, label: value }],
    });
  }

  return {
    follow: { value: FOLLOW_SWITCHER, label: FOLLOW_SWITCHER_LABEL },
    groups,
    value,
  };
}

/**
 * formatConformTarget renders a ConformTargetView as the canonical raster
 * string, or '' when there is nothing usable to render.
 *
 * The frame rate crosses the Wails boundary as the OPERATOR-FACING DECIMAL
 * already — app.go's ConformTargetView.FrameRate is gst.ConformTarget's
 * DisplayFrameRate, so 30000/1001 arrives as 29.97 — and String(Number(x)) is
 * what turns 50.0 back into "50" without turning 59.94 into "59.9".
 *
 * @param {{width?: unknown, height?: unknown, frameRate?: unknown}|null|undefined} target
 * @returns {string}
 */
export function formatConformTarget(target) {
  if (!target || typeof target !== 'object') return '';
  const w = target.width;
  const h = target.height;
  const r = target.frameRate;
  const positive = (n) => typeof n === 'number' && Number.isFinite(n) && n > 0;
  if (!positive(w) || !positive(h) || !positive(r)) return '';
  return `${w}x${h}p${String(Number(r))}`;
}

/**
 * describeConformTarget is what the operator is shown about the SWITCHER,
 * beside the control that can disagree with it.
 *
 * ===================== WHAT THE BINDING CAN AND CANNOT ANSWER ===============
 *
 * TWO BINDINGS CAN ANSWER THIS, and this function takes whichever the caller
 * has. App.GetSwitcherFormat reads the INSTANCE'S OWN SETTING over one REST call
 * and needs no session, which is what makes the readout work on a Settings
 * screen opened an hour before kick-off — the state the operator is actually in
 * when they are choosing a format. App.GetConformTarget answers from the RUNNING
 * session when there is one, and that answer is the switcher's too, because
 * Start derived it from the switcher.
 *
 * WHAT IS REFUSED is GetConformTarget's no-session answer, which is the
 * operator's own videoFormatOverride handed back stamped source="override".
 * Echoing an operator's own setting back to them under the heading "what M2L-X
 * is configured for" would be the screen inventing a confirmation, which is
 * worse than saying nothing: the whole point of the readout is to be the
 * INDEPENDENT number the override can be wrong against. That is why this
 * function reads `source` at all, and why the gate is
 * isIndependentOfTheOperator rather than a truthiness check on the raster.
 *
 * THE NOT-KNOWN LINE says the instance could not be read rather than "press
 * START", because pressing START is no longer what fixes it: with
 * GetSwitcherFormat wired, the remaining reasons are no host, not signed in yet,
 * or an instance that is not up — none of which START addresses, and all of
 * which resolve on their own once the seat is configured and reachable.
 *
 * @param {{source?: unknown}|null|undefined} target
 * @returns {string}
 */
export function describeConformTarget(target) {
  const raster = formatConformTarget(target);
  if (raster !== '' && isIndependentOfTheOperator(target)) {
    return `Switcher: ${raster}`;
  }
  return 'Switcher: not reachable yet';
}

/**
 * The four states of "does the override agree with the switcher".
 *
 *   follow    no override: the format is derived, which is right almost always
 *   unknown   an override is set and there is nothing independent to check it
 *             against — see describeConformTarget for why that is the state
 *             before START rather than a failure
 *   match     an override is set and it agrees with the running feed's target
 *   diverge   an override is set and it does NOT agree, which means the
 *             switcher will refuse this feed
 */
export const FORMAT_MATCH = Object.freeze({
  FOLLOW: 'follow',
  UNKNOWN: 'unknown',
  MATCH: 'match',
  DIVERGE: 'diverge',
});

/**
 * deriveFormatMatch compares the override on the form against the switcher's
 * own format and says, in one word and one short line, what that means.
 *
 * ===================== DIVERGENCE IS THE WHOLE POINT ========================
 *
 * Every source feeding an M2L-X instance must be produced in the instance's
 * format; one that is not is refused. So an override that disagrees is not a
 * preference, it is a feed that will not be accepted — and the operator has to
 * be able to see that WITHOUT knowing the rule, which is why the caller marks
 * the row as well as printing the line. The line is short on purpose: a
 * paragraph explaining conform rules is for whoever edits the code, and this
 * one has to be readable at a glance by somebody looking for a microphone.
 *
 * @param {unknown} override the videoFormatOverride on the form
 * @param {{source?: unknown}|null|undefined} target App.GetSwitcherFormat's
 *   answer, or App.GetConformTarget's when a session is running
 * @returns {{state: string, line: string, diverges: boolean}}
 */
export function deriveFormatMatch(override, target) {
  const value = typeof override === 'string' ? override.trim() : '';
  const raster = formatConformTarget(target);
  const independent = raster !== '' && isIndependentOfTheOperator(target);

  if (value === FOLLOW_SWITCHER) {
    return { state: FORMAT_MATCH.FOLLOW, line: '', diverges: false };
  }
  if (!independent) {
    return {
      state: FORMAT_MATCH.UNKNOWN,
      line: 'Overriding — the switcher’s format could not be read.',
      diverges: false,
    };
  }
  if (value === raster) {
    return { state: FORMAT_MATCH.MATCH, line: `Matches the switcher (${raster}).`, diverges: false };
  }
  return {
    state: FORMAT_MATCH.DIVERGE,
    line: `DIVERGES: the switcher is ${raster}. This feed will not be accepted.`,
    diverges: true,
  };
}
