/**
 * The VIDEO SOURCE: what this position actually sends to the switcher, the
 * confidence preview beside it, and the camera lamp that says whether the card
 * is seeing anything.
 *
 * Owner: WP-DECKLINK tier 3, frontend half. Pure — no DOM, no browser API, no
 * backend — so `node --test` drives every rule below for real with nothing
 * installed (videosource.test.js). settings.js, home.js and app.js are the
 * wiring; this file is what they are wiring.
 *
 * ===================== WHY THIS IS A CONTROL AND NOT A CONSTANT =============
 *
 * Until now the video leg was ONE still PNG through imagefreeze, on every seat,
 * for the whole match. That is what "slate" means here and it is still the
 * default: a position that never opens this control sends exactly what it sent
 * before the control existed, byte for byte (internal/gst's PipelineOpts.
 * VideoCaptureID: "the zero value is the slate").
 *
 * The card is the other answer. It is not an experiment and it is not the
 * expensive one — MEASURED, the whole capture-to-encode graph costs 9.3-14.6 %
 * of one core against the shipping slate leg's 18.5-23.9 %, because a still
 * picture is re-encoded from nothing fifty times a second while a real one
 * compresses. There is no processor argument against this feature in either
 * direction; the argument is only ever about what belongs on air.
 *
 * ===================== WHICH ONE IS GOING TO AIR MUST BE OBVIOUS =============
 *
 * That is why this module carries WORDING as well as booleans. Nothing else in
 * this application can answer the question:
 *
 *   - the SENDING lamp is green either way, because the socket is fine;
 *   - the switcher's own three lamps are green either way, because it really is
 *     receiving a healthy, correctly formatted, correctly bitrated H.264 feed;
 *   - the audio meters move either way, because commentary is a different leg;
 *   - and the picture on the main screen is the RETURN — what M2L-X is putting
 *     to air — which says nothing whatever about what THIS SEAT contributes.
 *
 * So a position sending a slate when somebody expected a camera, or a camera
 * that has lost its input and is sending black, is invisible from every
 * indicator this application already has. describeToAir is the sentence under
 * the Settings control and deriveCameraLamp is the lamp on the main screen, and
 * between them one glance answers it.
 *
 * ===================== AND AN UNSTARTABLE CHOICE IS SAID UP FRONT ===========
 *
 * MEASURED: a DeckLink capture on a machine with no card — or with a busy one —
 * comes back as not-negotiated (-4) in about 100 microseconds, naming neither
 * the device nor the cause. It is a fault an operator can neither read nor act
 * on. So the card is only OFFERED when one has actually been enumerated
 * (gst.Device.Kind is "decklink" for it), and a configuration that already names
 * the card on a machine that has none says so at the field.
 *
 * It is a WARNING and not a refusal, deliberately, for validate.js's own
 * standing reason: an engineer must be able to configure a position before the
 * hardware is patched into it. What must not happen is that they find out
 * twenty minutes before kick-off. Go's own pre-flight refuses at START, naming
 * this field and the card; this is the same fact, earlier.
 */

// The two device kinds, from the one module that mirrors internal/gst's
// Device.Kind. Imported rather than restated: this file's whole card-present
// test is a comparison against that string, and a second spelling of it would
// answer "no card" on a machine with one in it.
import { DEVICE_KIND } from './devices.js';
// The lamp levels, for the one state this file adds to the watchdog's own.
import { LEVEL } from './lamps.js';
// THE SIGNAL WATCHDOG'S RENDERING, imported and never reimplemented. The
// hysteresis is internal/gst's, its three states already have wording that has
// been reviewed, and the Settings routing screen paints its CARD VIDEO lamp from
// this same function. Two renderings of one watchdog would be two lamps that can
// disagree about one card.
import { deriveSignalLamp } from './channelmap.js';

/**
 * The two video-leg sources, spelled exactly as internal/config spells them
 * (config.VideoSourceSlate / config.VideoSourceDeckLink). They are the <option>
 * values and the values collectConfig sends, so a drift here is the silent kind:
 * config.EffectiveVideoSource substitutes "slate" for anything empty, and a kind
 * that differed only in case would save cleanly and quietly put the slate back
 * on air.
 */
export const VIDEO_SOURCE_SLATE = 'slate';
export const VIDEO_SOURCE_DECKLINK = 'decklink';

/**
 * DEFAULT_VIDEO_SOURCE is the slate, and that is the safe direction rather than
 * a preference: a machine whose configuration cannot be read, or which was
 * written before this field existed, must come up sending what it sent
 * yesterday. config.DefaultVideoSource says the same thing on the other side,
 * and its comment is blunt about why it is the worst default in the table to
 * change.
 */
export const DEFAULT_VIDEO_SOURCE = VIDEO_SOURCE_SLATE;

/**
 * The two config keys this module is about, mirroring config.Config's json tags.
 *
 * They are exported so nothing has to spell them from memory — but settings.js's
 * collectConfig and populate write them as LITERALS all the same, because
 * settings.test.js reflects over internal/config's json tags and requires each
 * one to appear in both functions by name. That test is the data-loss guard, and
 * the loss it guards against is not hypothetical here: collectConfig replaces the
 * whole stored document, so a Save that failed to restate videoSource would put
 * a live position's camera back on the slate because somebody corrected a typo
 * in the event id.
 */
export const VIDEO_SOURCE_KEY = 'videoSource';
export const PREVIEW_KEY = 'decklinkPreviewEnabled';

/** The camera lamp's name in the main screen's lamp row. */
export const LAMP_CAMERA = 'CAMERA';

/**
 * VIDEO_SOURCES is the control, in the order it is drawn.
 *
 * `cost` is stated for the reason picturesource.js states one: an operator
 * cannot weigh an option they are told nothing about. Here it earns its place
 * chiefly because the intuition is BACKWARDS — the card is the cheaper leg —
 * and an engineer who assumes otherwise will leave a position on the slate to
 * "protect the machine".
 *
 * @type {ReadonlyArray<{value: string, label: string, summary: string, cost: string}>}
 */
export const VIDEO_SOURCES = Object.freeze([
  Object.freeze({
    value: VIDEO_SOURCE_SLATE,
    label: 'Slate — a still image',
    summary:
      'The still picture named under Slate below is sent, frozen, for the whole match. ' +
      'This is what every commentary position has sent until now.',
    cost:
      'Needs no capture hardware. It costs MORE processor than the card, not less — 18.5-23.9 % ' +
      'of one core measured — because a still picture is re-encoded from nothing fifty times a ' +
      'second.',
  }),
  Object.freeze({
    value: VIDEO_SOURCE_DECKLINK,
    label: 'The DeckLink card’s video input',
    summary:
      'Whatever is on the card’s SDI or HDMI input is sent, conformed to the format the switcher ' +
      'is configured for. The slate is not sent at all.',
    cost:
      'Measured at 9.3-14.6 % of one core, less than the slate. It needs a card in this machine ' +
      'with a picture on its input.',
  }),
]);

/**
 * normaliseVideoSource maps anything to one of the two sources, defaulting to
 * the slate.
 *
 * It is applied on the way IN as well as on the way out, and the way in is the
 * one that bites: assigning an unrecognised value to a <select> leaves it
 * showing nothing selected, and the next Save would write that nothing back over
 * whatever the file held. settings.js's normaliseAudioSourceKind is used the
 * same way for the same reason.
 *
 * @param {unknown} value
 * @returns {string}
 */
export function normaliseVideoSource(value) {
  return value === VIDEO_SOURCE_DECKLINK ? VIDEO_SOURCE_DECKLINK : VIDEO_SOURCE_SLATE;
}

/**
 * normalisePreviewEnabled reads the preview flag. Anything that is not exactly
 * `true` is OFF, which is the safe direction twice over: the preview is an extra
 * branch of a pipeline that is going to air, and it is a window that appears on
 * somebody's screen over whatever they were looking at. "The config was a shape
 * we did not recognise" is not a reason to do either.
 *
 * @param {unknown} value
 * @returns {boolean}
 */
export function normalisePreviewEnabled(value) {
  return value === true;
}

/**
 * countDeckLinkDevices reports how many entries of a device list are a DeckLink
 * card.
 *
 * The list is App.ListInputDevices' — [{id, name, kind}] — and the KIND is the
 * only thing that may be read. NOTHING HERE PARSES AN ID: internal/gst documents
 * Device.ID as opaque and per-platform, and the kind is a field precisely so
 * that nobody has to infer it from a string shape.
 *
 * That an AUDIO listing answers a question about VIDEO is not a shortcut, it is
 * a measured property of the hardware: one persistent-id names the CARD and
 * serves both entries — the Audio/Source and Video/Source devices for the fitted
 * UltraStudio 4K Mini both publish 2747401380 — which is the same fact
 * PipelineOpts.VideoCaptureID is written around.
 *
 * @param {Array<{kind?: string}>|null|undefined} devices
 * @returns {number}
 */
export function countDeckLinkDevices(devices) {
  if (!Array.isArray(devices)) return 0;
  return devices.filter((d) => d && d.kind === DEVICE_KIND.DECKLINK).length;
}

/**
 * deriveVideoSourceEffects is THE statement of what a video-source selection
 * means, so that "which one is going to air" is one expression in one place
 * rather than three call sites kept in step by hand.
 *
 * `devices` is App.ListInputDevices' answer, or null/undefined for NOT KNOWN —
 * and those are two different states, not one. A machine whose device listing
 * failed has not been shown to have no card, and a control that said "no card in
 * this machine" on the strength of a failed listing would send an engineer to
 * look for hardware that is sitting in the slot. So `startable` stays true when
 * nothing is known: a warning nobody can act on is worse than no warning.
 *
 * @param {unknown} source the saved videoSource
 * @param {Array<{kind?: string}>|null|undefined} devices ListInputDevices, or null
 * @returns {{
 *   source: string,
 *   wantCard: boolean,
 *   cardKnown: boolean,
 *   cardPresent: boolean,
 *   cardCount: number,
 *   startable: boolean,
 *   toAir: string,
 * }}
 */
export function deriveVideoSourceEffects(source, devices) {
  const s = normaliseVideoSource(source);
  const wantCard = s === VIDEO_SOURCE_DECKLINK;
  const cardKnown = Array.isArray(devices);
  const cardCount = countDeckLinkDevices(devices);
  const cardPresent = cardCount > 0;
  const startable = !wantCard || !cardKnown || cardPresent;
  return Object.freeze({
    source: s,
    wantCard,
    cardKnown,
    cardPresent,
    cardCount,
    startable,
    // THREE states rather than two, because "the slate goes to air" and "nothing
    // goes to air and START will fail" are different sentences and only the
    // second one is something somebody has to act on before kick-off.
    toAir: !wantCard ? VIDEO_SOURCE_SLATE : startable ? VIDEO_SOURCE_DECKLINK : 'nothing',
  });
}

/**
 * describeToAir is the sentence under the control: WHAT THIS SEAT SENDS.
 *
 * It is not a restatement of the selected option's label. It is the consequence,
 * in the words of somebody whose question is whether a camera is on air — and it
 * is the answer to requirement "it must be obvious which is going to air", which
 * a <select> showing one of two similar phrases does not by itself give.
 *
 * @param {ReturnType<typeof deriveVideoSourceEffects>} effects
 * @returns {string}
 */
export function describeToAir(effects) {
  if (effects.toAir === VIDEO_SOURCE_SLATE) {
    return (
      'GOING TO AIR: THE SLATE. The still image is sent for the whole match, and nothing from a ' +
      'card is sent whatever is plugged into one.'
    );
  }
  if (effects.toAir === VIDEO_SOURCE_DECKLINK) {
    return (
      'GOING TO AIR: THE CARD. Whatever is on the card’s video input is sent, and the slate is ' +
      'not sent at all. Watch the CAMERA lamp on the main screen — nothing else in this ' +
      'application can tell a live picture from black.'
    );
  }
  return (
    'NOTHING WOULD GO TO AIR. The card is selected and no DeckLink card was found in this ' +
    'machine, so START will be refused. Choose the slate, or fit the card and re-open Settings.'
  );
}

/**
 * describeCardAvailability is the note about the HARDWARE, as opposed to the
 * selection. Empty string when there is nothing worth saying — which is the
 * ordinary case on both a slate seat with no card and a card seat with one, so
 * this is never an empty flourish.
 *
 * The three non-empty cases need three different actions:
 *
 *   card wanted, none found     the START failure, said before it happens;
 *   card not wanted, one fitted one sentence, because an engineer standing in
 *                               front of a rack with a card in it and a slate on
 *                               air should be told which of the two the
 *                               application is doing;
 *   list unread                 say it is unread. NEVER that it is empty.
 *
 * @param {ReturnType<typeof deriveVideoSourceEffects>} effects
 * @returns {string}
 */
export function describeCardAvailability(effects) {
  if (!effects.cardKnown) {
    return (
      'The capture devices on this machine could not be listed, so whether a DeckLink card is ' +
      'fitted is unknown. The choice above is left exactly as it is rather than second-guessed.'
    );
  }
  if (effects.wantCard && !effects.cardPresent) {
    return (
      'NO DECKLINK CARD WAS FOUND IN THIS MACHINE — nothing was enumerated. A capture from a card ' +
      'that is not there fails in about a ten-thousandth of a second and names neither the device ' +
      'nor the cause, so it is said here instead of there.'
    );
  }
  if (!effects.wantCard && effects.cardPresent) {
    return effects.cardCount === 1
      ? 'A DeckLink card IS fitted to this machine and is not being used for video: the slate is ' +
          'what goes to air.'
      : `${effects.cardCount} DeckLink cards are fitted to this machine and none is being used ` +
          'for video: the slate is what goes to air.';
  }
  return '';
}

/**
 * PREVIEW_AT_START_CAVEAT is the permanent statement beside the preview toggle.
 * It has to be read before the toggle is used rather than discovered after it.
 *
 * IT IS A MEASUREMENT, NOT A DESIGN PREFERENCE, and saying so is the point:
 * "applied at Start" reads like an unfinished feature unless the reason is
 * beside it. Building or tearing the preview branch down on a running pipeline
 * means a set_state(NULL) inside a blocking pad probe, and that was measured to
 * take the ON-AIR leg from 50 fps to 0 — PERMANENTLY, with the pipeline still
 * reporting PLAYING and every lamp in this application still green. There is no
 * version of a live toggle worth that, so the preview is built once, at Start,
 * from the saved configuration.
 *
 * The wording is deliberately in the operator's terms — what to press — rather
 * than the pipeline's. "Applied at Start" is engineering; "press STOP, change
 * it, then START" is an instruction. That order is not arbitrary either: the
 * control is DISABLED while a feed is up, so stopping is genuinely the first
 * thing to do, and Go's own refusal (errPreviewChangeWhileSending) says it in
 * exactly these words.
 */
export const PREVIEW_AT_START_CAVEAT =
  'Off by default. It puts a small confidence picture of what the card is capturing beside the ' +
  'return picture on the main screen; it changes nothing about what is transmitted. IT ONLY ' +
  'TAKES EFFECT WHEN YOU PRESS START: switching it during a live feed was measured to stop the ' +
  'picture going to air altogether — permanently, with everything still reporting success — so ' +
  'this application will not do it. Press STOP, change it, then START.';

/**
 * VIDEO_SOURCE_AT_START_CAVEAT is the same rule for the source above the
 * preview, and it is here rather than in the option table because it is true of
 * both options and of neither in particular.
 *
 * The measurement is the one PREVIEW_AT_START_CAVEAT records; what differs is
 * the stake. Changing the preview under a running feed would cost the operator's
 * own window; changing the SOURCE under one would cost the picture the switcher
 * is receiving. Go refuses both — errVideoSourceWhileSending — and this screen
 * disables both rather than offering a control whose refusal the operator would
 * have to discover by pressing it.
 */
export const VIDEO_SOURCE_AT_START_CAVEAT =
  'The video leg is built when you press START, so changing this does not move what is on air ' +
  'now: press STOP, change it, then START. This application will not swap it under a running ' +
  'feed — doing so was measured to stop the picture going to air permanently, with everything ' +
  'still reporting success.';

/**
 * VIDEO_LEG_WHILE_SENDING is the reason both controls carry while a session is
 * up. It is one string for both because it is one fact about both, and because
 * it has to agree with the refusals Go would make (errVideoSourceWhileSending
 * and errPreviewChangeWhileSending) — which say "Press STOP, change it, then
 * START" in those words.
 *
 * The gate itself is GO'S, not this line's. A screen that disabled these
 * controls but let a Save carry the change anyway would be a screen making a
 * promise it does not keep; a screen that offered them and let the operator find
 * the refusal by pressing would be worse. This is renderPresetButtons' pattern
 * exactly: the refusal is the platform's, and the honest rendering of it is on
 * the control.
 */
export const VIDEO_LEG_WHILE_SENDING =
  'Disabled while SENDING: the video leg is built at START. Press STOP, change it, then START.';

/**
 * describePreviewBox is the caption drawn INSIDE the reserved preview box.
 *
 * ===================== THE CAPTION NEEDS NO VISIBILITY RULE =================
 *
 * The preview is painted by an OPAQUE NATIVE CHILD WINDOW on top of this page,
 * exactly as the SRT return picture is (see overlay.js). So the caption is
 * visible precisely when there is no picture over it, by physics rather than by
 * a flag this side has to keep in step — which is worth having, because the one
 * thing the page genuinely cannot know is whether the Go side built a preview
 * branch this session. A box showing a caption is a box explaining itself; a box
 * showing a picture needs no caption.
 *
 * @param {boolean} running whether a session is up
 * @returns {string}
 */
export function describePreviewBox(running) {
  return running
    ? 'PREVIEW — not in this session. It is built at START, so STOP and START to see it.'
    : 'PREVIEW — press START.';
}

/**
 * deriveCameraLamp turns the video source and the signal watchdog's last report
 * into the main screen's CAMERA lamp.
 *
 * ===================== WHY THIS LAMP EXISTS AT ALL ==========================
 *
 * Because NOTHING ELSE IN THIS APPLICATION CAN TELL. MEASURED: a DeckLink that
 * loses its input goes on emitting black frames at full rate FOR EVER — no
 * error, no EOS, and the muxer never starves. The sender stays CONNECTED, the
 * switcher reports a healthy correctly-formatted feed, the audio meters keep
 * moving, and what is going to air is black with every other lamp on this row
 * green. This is the lamp that is not.
 *
 * ===================== AND WHY IT IS NOT THE AUDIO LAMP =====================
 *
 * "No camera" and "no audio" sit next to each other on one row and must never be
 * mistaken for one another, because they send whoever reads them to different
 * places with different urgency. No video signal is a cable, a router crosspoint
 * or a source that is switched off; it is fixed in the machine room, by somebody
 * who is not at this desk, and until it is, black is going to air. No audio is
 * an embedder, a mute or a microphone; it is fixed at the position, now, by the
 * person reading the lamp. They differ here in NAME, in TEXT, in GLYPH and in
 * COLOUR — a red cross against an amber triangle — which is four signals rather
 * than one, so neither colour blindness nor a washed-out gallery projector
 * collapses them into each other.
 *
 * The three card states are deriveSignalLamp's, imported and not restated.
 *
 * The FOURTH state is this function's own and is the reason it exists:
 *
 *   slate           grey 'SLATE'
 *
 * A LAMP THAT DISAPPEARS IS NOT AN ANSWER. Hiding this on a slate seat is
 * tempting and wrong twice over: an absent lamp reads as "this build has not got
 * one" rather than as "the slate is going to air", and a lamp that comes and
 * goes with a setting is a lamp nobody learns to look at. Grey/SLATE is the
 * exact truth on every position shipping today, and it is the at-a-glance answer
 * to what this seat is contributing — which nothing else on the main screen
 * gives at all.
 *
 *   deriveCameraLamp('slate', anything)         -> grey  'SLATE'
 *   deriveCameraLamp('decklink', undefined)     -> grey  'NOT MEASURED'
 *   deriveCameraLamp('decklink', {state:'OK'})  -> green 'SIGNAL'
 *   deriveCameraLamp('decklink', {state:'OK', flaps:4})
 *                                               -> amber 'UNSTABLE (4)'
 *   deriveCameraLamp('decklink', {state:'LOST'})
 *                                               -> red   'NO SIGNAL'
 *
 * @param {unknown} source the saved videoSource
 * @param {{state?: unknown, flaps?: unknown}|null|undefined} signal the "signal" event
 * @returns {{level: string, text: string}}
 */
export function deriveCameraLamp(source, signal) {
  if (normaliseVideoSource(source) !== VIDEO_SOURCE_DECKLINK) {
    return { level: LEVEL.GREY, text: 'SLATE' };
  }
  return deriveSignalLamp(signal);
}
