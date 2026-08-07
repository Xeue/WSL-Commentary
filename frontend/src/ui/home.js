import { createLampRow } from './lamps.js';
import { effectiveCrop, describeCrop, REFERENCE_MOSAIC } from './tile.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
// The headphone list is ALWAYS the browser's. Audio always comes from Kinesis
// and is no longer switchable, so this is a constant now rather than a function
// of a control — but it is still imported from the module that owns the two
// identifier spaces, so that the WASAPI list and the browser list never come to
// be described by two different sentences written in two different files.
import { DEVICE_SOURCE_WEBRTC, LATENCY_NOTE } from './returnsource.js';
import {
  PICTURE_SOURCES,
  PICTURE_SOURCE_SRT,
  DEFAULT_PICTURE_SOURCE,
  normalisePictureSource,
  describePictureSource,
  derivePictureSourceEffects,
  describePictureShowing,
  PICTURE_NOTE,
  PICTURE_STATE_WORDS,
  PICTURE_STATE_BACKOFF,
  normalisePictureState,
} from './picturesource.js';
// The channel table comes from the monitor module because that is where it is
// ENFORCED — it is the wiring of a ChannelSplitter to a ChannelMerger, and the
// words here have to be the words for that wiring. It is pure data with no
// browser API in it. Writing a second copy of "Stereo / Left only / Right only"
// on this side is precisely the bug ./returns.js exists to record: two tables
// that agree with each other and are both wrong.
import { CHANNEL_MODES, DEFAULT_CHANNEL_MODE, normaliseChannelMode } from '../monitor/channels.js';

// The main screen: the PGM tile, the three device/return controls, the
// START/STOP button and the five lamps. Specification section 10 layout, less
// the honest line.
//
// Owner: WP-5b.
//
// ======================= THE HONEST LINE IS WITHDRAWN =======================
//
// The permanent caveat under the lamps ("Your feed is reaching the switcher.
// This does not confirm you are audible on the broadcast output.") is GONE FROM
// THIS GUI at the operator's request. That is a deliberate change to
// specification section 8, not a tidy-up.
//
// Nothing behind it has been removed or weakened: deriveHonestLine and its
// per-state wording are untouched in ./lamps.js, exactly as the golden/drift
// machinery was kept when the drift panel was withdrawn from the mixer drawer.
// What changed is that this file no longer renders it, so putting it back is a
// change to this file alone.
//
// This module only builds DOM and exposes setters; it holds no backend
// knowledge and calls nothing in backend.js or the monitor module directly.
// app.js wires its handlers callbacks to the backend and its setters to
// backend/monitor events, which keeps this file testable-by-eye in isolation
// and keeps the "what does the UI show" question answered in one place.

// The mosaic size is NOT a constant here any more. config.monitorTile is a
// rectangle in the pixels of the mosaic it was measured against
// (tile.js REFERENCE_MOSAIC, 2240x1440), and the live track is whatever it is;
// the crop is computed from the size that actually arrived. See tile.js for why
// assuming otherwise put the picture in the wrong place.

const LAMP_NAMES = ['SENDING', 'SWITCHER SEES FEED', 'VIDEO', 'AUDIO', 'MONITOR'];

// The Return dropdown offers all seven audio tracks. It used to offer two, CLN
// and PGM. That was fine as long as the documented routing held. It did not:
// the commentator on mid 2 could hear themselves delayed, which means
// commentary IS routed to aux1 on this event. This application cannot fix that;
// what it can do is stop being a two-option dropdown with no way out, and offer
// every track so a clean one can be found by ear in the ten seconds before
// kick-off.
//
// The table and the hint live in ./returns.js and are shared with the Settings
// screen, because they were duplicated here and there and both copies were
// wrong for mids 3 to 7 in the same way. See that file.

/**
 * createHomeView builds the home screen and returns:
 *
 *   el          the root <section>, ready to insert into the document
 *   videoEl     the <video> the monitor mosaic attaches to (opts.videoEl)
 *   audioEl     the <audio> the monitor return plays through (opts.audioEl)
 *   lamps       the five lamps by name, as created by createLampRow
 *   setTile(tile)                       sets config.monitorTile; the crop is
 *                                       computed from it and the live track size
 *   setInputDevices(devices, selected)  populates the commentary input dropdown
 *   setHeadphoneDevices(devices, sel)   populates the headphones dropdown
 *   setReturnMid(mid)                   selects one of the seven buses, 1..7
 *   setReturnChannel(mode)              stereo / left / right
 *   setPictureSource(source)            srt / mosaic — WHICH PICTURE, not audio
 *   setPictureAvailable(available, why) disables the SRT option with a reason
 *   setPictureState(state)              the native receiver's own status
 *   setPictureOverlaid(on)              whether the native window is ON SCREEN
 *                                       over the tile. The ONLY thing that may
 *                                       suppress the mosaic; see the function.
 *   measurePictureRect()                the reserved box, in CSS pixels
 *   setLevel(fraction)                  positions the level slider, 0..1
 *   setRunning(running)                 flips the START/STOP button
 *   setBusy(busy)                       disables the button while a call is in flight
 *   setStatusUnavailable(unavailable)   shows/hides the transient banner (Status.Stale)
 *   showError(message) / clearError()   the dismissible error banner
 *
 * handlers = {
 *   onSettings(), onMixer(), onStartStop(),
 *   onInputChange(deviceId), onHeadphoneChange(deviceId),
 *   onReturnChange(mid), onReturnChannelChange(mode), onPictureSourceChange(src),
 *   onLevelChange(fraction),
 * }
 *
 * ===================== THE PICTURE AREA IS NOW A RESERVATION ================
 *
 * The high-quality picture is decoded in Go and painted by a NATIVE CHILD
 * WINDOW over this page — a browser element cannot play SRT. So `.pgm-tile` is
 * two things at once: it is the mosaic's crop box, exactly as it always was, and
 * it is the RECTANGLE the native overlay is told to occupy. Both pictures use
 * the same box on purpose, so that falling back from one to the other does not
 * move a single control on the screen.
 *
 * This file does not talk to Go and does not know the overlay exists. It exposes
 * measurePictureRect(), and app.js does the rest.
 */
export function createHomeView(handlers) {
  const el = document.createElement('section');
  el.className = 'view view-home';

  // --- header --------------------------------------------------------
  const header = document.createElement('header');
  header.className = 'topbar';
  const titleWrap = document.createElement('div');
  titleWrap.className = 'title-wrap';
  const title = document.createElement('h1');
  title.textContent = 'WSL Commentary';
  const devBadge = document.createElement('span');
  devBadge.className = 'dev-badge';
  devBadge.textContent = 'DEV — fake backend';
  devBadge.hidden = true;
  titleWrap.append(title, devBadge);
  // The two header controls. Mixer sits beside Settings because that is where
  // the operator asked for it: it used to be a section INSIDE Settings, which
  // meant reaching the clean-feed matrix through a configuration form.
  //
  // A commentator sees this button, so the drawer's read-only-until-armed gate
  // matters more, not less. This file does not weaken it and cannot: it neither
  // builds the drawer nor knows what one is. It raises onMixer; app.js decides
  // what that means, and the host module beside it owns everything else.
  //
  // mixerwiring.test.js asserts that by reading this file's TEXT, so do not
  // name the forbidden symbols here even in a comment.
  const headerBtns = document.createElement('div');
  headerBtns.className = 'topbar-actions';
  const mixerBtn = document.createElement('button');
  mixerBtn.type = 'button';
  mixerBtn.className = 'btn btn-ghost';
  mixerBtn.textContent = 'Mixer';
  mixerBtn.title = 'Show which inputs are in the CLEAN FEED the client receives. Opens read-only.';
  mixerBtn.addEventListener('click', () => handlers.onMixer());
  const settingsBtn = document.createElement('button');
  settingsBtn.type = 'button';
  settingsBtn.className = 'btn btn-ghost';
  settingsBtn.textContent = 'Settings';
  settingsBtn.addEventListener('click', () => handlers.onSettings());
  headerBtns.append(mixerBtn, settingsBtn);
  header.append(titleWrap, headerBtns);

  function setDevBadge(visible) {
    devBadge.hidden = !visible;
  }

  // --- error banner (dismissible; NOT the honest line) ----------------
  const errorBanner = document.createElement('div');
  errorBanner.className = 'error-banner';
  errorBanner.setAttribute('role', 'alert');
  errorBanner.hidden = true;
  const errorText = document.createElement('span');
  const errorDismiss = document.createElement('button');
  errorDismiss.type = 'button';
  errorDismiss.className = 'error-dismiss';
  errorDismiss.setAttribute('aria-label', 'Dismiss');
  errorDismiss.textContent = '✕';
  errorDismiss.addEventListener('click', () => clearError());
  errorBanner.append(errorText, errorDismiss);

  function showError(message) {
    errorText.textContent = message;
    errorBanner.hidden = false;
  }
  function clearError() {
    errorBanner.hidden = true;
    errorText.textContent = '';
  }

  // --- the picture area -------------------------------------------------
  //
  // The picture is the main thing a commentator looks at, so it gets every
  // pixel left after the controls: .pgm-stage is the flex child that grows,
  // and .pgm-tile is sized inside it from the tile's own aspect ratio — height
  // -limited on a wide window, width-limited on a narrow one. See main.css.
  //
  // TWO PICTURES, ONE BOX. The <video> inside it is the WebRTC mosaic, cropped
  // to the PGM tile; the native SRT overlay is painted over the same rectangle
  // from outside the page entirely. Sharing the box is what makes the fallback
  // invisible as a layout event: when SRT drops, the mosaic underneath is
  // already the right size and in the right place.
  const pgmStage = document.createElement('div');
  pgmStage.className = 'pgm-stage';
  const pgmTile = document.createElement('div');
  pgmTile.className = 'pgm-tile';
  const videoEl = document.createElement('video');
  videoEl.autoplay = true;
  videoEl.playsInline = true;
  videoEl.muted = true; // the mosaic video track carries no audio we want; return audio is separate
  pgmTile.appendChild(videoEl);

  // WHICH PICTURE IS ON SCREEN, said permanently, over the picture.
  //
  // The two look alike at a glance — the same framing of the same match — and
  // differ enough in quality that somebody will ask out loud during a match
  // whether they are looking at the good one. It is drawn INSIDE the tile so
  // that it is over the mosaic; while the SRT overlay is up the overlay covers
  // it, which is correct, because the overlay is only ever up when the answer
  // is "SRT" and there is a second copy of the answer beside the control.
  const pictureBadge = document.createElement('div');
  pictureBadge.className = 'picture-badge';
  pgmTile.appendChild(pictureBadge);

  pgmStage.appendChild(pgmTile);

  const audioEl = document.createElement('audio');
  audioEl.autoplay = true;
  audioEl.hidden = true;

  // The tile as configured, and the mosaic as it actually arrived. Neither is
  // authoritative on its own: the crop is what they produce together.
  let configuredTile = { x: 0, y: 360, w: 640, h: 360 };
  let liveMosaic = null;
  let lastDescription = '';

  function applyCrop() {
    const crop = effectiveCrop(configuredTile, liveMosaic, REFERENCE_MOSAIC);

    pgmTile.style.setProperty('--mosaic-w', String(crop.mosaic.w));
    pgmTile.style.setProperty('--mosaic-h', String(crop.mosaic.h));
    pgmTile.style.setProperty('--tile-x', String(crop.tile.x));
    pgmTile.style.setProperty('--tile-y', String(crop.tile.y));
    pgmTile.style.setProperty('--tile-w', String(crop.tile.w));
    pgmTile.style.setProperty('--tile-h', String(crop.tile.h));
    // Two forms of the same ratio: `aspect-ratio` wants "640 / 360", and the
    // width calculation against the container's height wants a bare number.
    pgmTile.style.setProperty('--tile-ar', `${crop.tile.w} / ${crop.tile.h}`);
    pgmTile.style.setProperty('--tile-ar-num', String(crop.aspect));

    // Logged, not swallowed: "the picture is in the wrong place" is
    // undiagnosable from a screenshot without both mosaic sizes, and this is
    // the only place both are known. Only on a change, so a track that renews
    // its metadata every few seconds does not fill the console.
    const line = describeCrop(crop, configuredTile);
    if (line !== lastDescription) {
      lastDescription = line;
      console.info(line);
    }
  }

  /**
   * readMosaic takes the intrinsic size off the element. A <video> with no
   * track yet reports 0x0, which tile.js reads as "not known" rather than as a
   * mosaic of zero size.
   */
  function readMosaic() {
    const w = videoEl.videoWidth;
    const h = videoEl.videoHeight;
    if (!(w > 0 && h > 0)) return;
    if (liveMosaic && liveMosaic.w === w && liveMosaic.h === h) return;
    liveMosaic = { w, h };
    applyCrop();
  }

  // loadedmetadata fires when the first track's size is known; resize fires if
  // it CHANGES mid-session, which is what a mid-match renegotiation or a
  // reconnect to a differently-configured multiviewer looks like. Both are
  // needed: with only the first, a track that changes size leaves the crop
  // pointing at the old geometry for the rest of the match.
  videoEl.addEventListener('loadedmetadata', readMosaic);
  videoEl.addEventListener('resize', readMosaic);

  // Write the custom properties once at construction, so the tile has a shape
  // before any config has loaded and a failed getConfig cannot leave it at
  // whatever the stylesheet's fallbacks happen to be.
  applyCrop();

  function setTile(tile) {
    if (tile && typeof tile === 'object') configuredTile = tile;
    // The element may already have a track — a Settings save mid-session does
    // not restart the monitor — so re-read rather than waiting for an event
    // that has already happened.
    readMosaic();
    applyCrop();
  }

  // --- controls ----------------------------------------------------------
  //
  // One compact row that wraps, not three stacked full-width rows: every
  // vertical pixel these do not use goes to the picture above them.
  const controls = document.createElement('div');
  controls.className = 'controls';

  function makeRow(labelText, id, control) {
    const row = document.createElement('div');
    row.className = 'control-group';
    const label = document.createElement('label');
    label.htmlFor = id;
    label.textContent = labelText;
    row.append(label, control);
    return row;
  }

  /**
   * makeSegmented builds a radio group drawn as a row of buttons.
   *
   * Radios rather than a <select> because both of these controls have two or
   * three options that are chosen mid-match, sometimes in the ten seconds before
   * kick-off, and every option being visible without opening anything is worth
   * the width. Radios rather than <button>s because a radio group is what a
   * screen reader and a keyboard already understand, and because exactly-one-
   * selected is then enforced by the platform rather than by this file.
   *
   * Returns { el, set(value), setOptionEnabled(value, enabled, reason) }.
   */
  function makeSegmented(name, options, onChange) {
    const group = document.createElement('div');
    group.className = 'segmented';
    group.setAttribute('role', 'radiogroup');
    const inputs = new Map();

    for (const opt of options) {
      const id = `${name}-${opt.value}`;
      const label = document.createElement('label');
      label.className = 'segment';
      label.htmlFor = id;
      const input = document.createElement('input');
      input.type = 'radio';
      input.name = name;
      input.id = id;
      input.value = opt.value;
      if (opt.hint) label.title = opt.hint;
      input.addEventListener('change', () => {
        paint();
        if (input.checked) onChange(input.value);
      });
      const text = document.createElement('span');
      text.textContent = opt.label;
      label.append(input, text);
      group.appendChild(label);
      inputs.set(opt.value, { input, label });
    }

    // The selected segment is marked with a CLASS as well as being styled from
    // :has(:checked). :has() is a recent selector and this is the difference
    // between a control that shows which option is live and one that looks
    // like nothing is selected — which, on a control that decides what the
    // commentator hears, is worth not depending on a browser version for.
    function paint() {
      for (const { input, label } of inputs.values()) {
        label.classList.toggle('segment-checked', input.checked);
      }
    }

    return {
      el: group,
      set(value) {
        const entry = inputs.get(value);
        if (entry) entry.input.checked = true;
        paint();
      },
      setOptionEnabled(value, enabled, reason) {
        const entry = inputs.get(value);
        if (!entry) return;
        entry.input.disabled = !enabled;
        entry.label.classList.toggle('segment-disabled', !enabled);
        // The reason goes on the control itself. A disabled option with no
        // explanation is read as "this is broken", and the most likely reason —
        // the binding is not in this build — is not something anyone can fix by
        // clicking harder.
        if (!enabled && reason) entry.label.title = reason;
      },
    };
  }

  const inputSelect = document.createElement('select');
  inputSelect.id = 'input-select';
  inputSelect.addEventListener('change', () => handlers.onInputChange(inputSelect.value));

  const headphoneSelect = document.createElement('select');
  headphoneSelect.id = 'headphone-select';
  headphoneSelect.addEventListener('change', () => handlers.onHeadphoneChange(headphoneSelect.value));

  // All seven audio transceivers, honestly labelled. The monitor already
  // subscribes to every one of them (~8.4 kbps idle for the lot), so switching
  // is a Web Audio source swap: immediate, and the peer connection — and the
  // programme picture riding on it — is untouched.
  const returnSelect = document.createElement('select');
  returnSelect.id = 'return-select';
  for (const bus of RETURN_BUSES) {
    const opt = document.createElement('option');
    opt.value = String(bus.mid);
    opt.textContent = bus.label;
    returnSelect.appendChild(opt);
  }
  returnSelect.value = String(DEFAULT_RETURN_MID); // mid 2, CLN, stays the default
  returnSelect.addEventListener('change', () => handlers.onReturnChange(Number(returnSelect.value)));

  const returnGroup = document.createElement('div');
  returnGroup.className = 'control-group control-group-return';
  const returnLabel = document.createElement('label');
  returnLabel.htmlFor = 'return-select';
  returnLabel.textContent = 'Return Audio';
  returnGroup.append(returnLabel, returnSelect);

  // --- return CHANNEL, beside the bus -------------------------------------
  //
  // Choosing a bus is no longer enough. FX and comms are mixed on PGM but hard-
  // panned left and right on CLN — M2L-X pans per input strip, not per bus, so
  // the operator got there with double router inputs — and a commentator who
  // wants the effects on their own needs one CHANNEL of that bus.
  //
  // "Left only" is the LEFT SOURCE CHANNEL IN BOTH EARS. It is not "audio in
  // the left ear": half the commentators are wearing one-ear cans and this
  // application does not know which ear. That is enforced in the Web Audio
  // graph, not here — see frontend/src/monitor/channels.js.
  const channelSegmented = makeSegmented(
    'return-channel',
    CHANNEL_MODES.map((m) => ({ value: m.value, label: m.label, hint: m.hint })),
    (mode) => handlers.onReturnChannelChange(mode),
  );
  channelSegmented.set(DEFAULT_CHANNEL_MODE);

  const channelGroup = document.createElement('div');
  channelGroup.className = 'control-group control-group-channel';
  const channelLabel = document.createElement('span');
  channelLabel.className = 'control-label';
  channelLabel.textContent = 'Return Channel';
  channelGroup.append(channelLabel, channelSegmented.el);

  // --- PICTURE source: which picture the commentator is looking at ---------
  //
  // THIS IS NOT THE CONTROL THAT USED TO BE HERE. There was a "Return Source"
  // segmented control in this position that switched the HEADPHONES between
  // WebRTC and SRT, and it was backwards: the operator asked for SRT PICTURES
  // with the audio staying on Kinesis, and selecting "SRT" silenced them.
  //
  // Audio is not switchable from this screen any more. The bus dropdown and the
  // channel selector above are the audio controls, and they are unchanged.
  const sourceSegmented = makeSegmented(
    'picture-source',
    PICTURE_SOURCES.map((s) => ({ value: s.value, label: s.label, hint: s.summary })),
    (source) => handlers.onPictureSourceChange(source),
  );
  sourceSegmented.set(DEFAULT_PICTURE_SOURCE);

  const sourceGroup = document.createElement('div');
  sourceGroup.className = 'control-group control-group-source';
  const sourceLabel = document.createElement('span');
  sourceLabel.className = 'control-label';
  sourceLabel.textContent = 'Picture';
  sourceGroup.append(sourceLabel, sourceSegmented.el);

  // The note under the control: what the selected picture IS, what it costs,
  // and — fixed, on both options — that the audio does not move.
  const sourceNote = document.createElement('p');
  sourceNote.className = 'picture-note';

  // The native receiver's own status. Separate from the note because it is the
  // only line on this screen that reports something happening rather than
  // something configured, and it must not be mistaken for the WebRTC lamps.
  const srtStatusLine = document.createElement('p');
  srtStatusLine.className = 'picture-status';
  srtStatusLine.hidden = true;

  const levelGroup = document.createElement('div');
  levelGroup.className = 'control-group control-group-level';
  const levelLabel = document.createElement('label');
  levelLabel.htmlFor = 'level-slider';
  levelLabel.textContent = 'Return Level';
  const levelSlider = document.createElement('input');
  levelSlider.type = 'range';
  levelSlider.id = 'level-slider';
  levelSlider.min = '0';
  levelSlider.max = '100';
  levelSlider.step = '1';
  levelSlider.value = '100';
  levelSlider.addEventListener('input', () => handlers.onLevelChange(Number(levelSlider.value) / 100));
  levelGroup.append(levelLabel, levelSlider);

  // The Headphones row is built by hand rather than through makeRow because its
  // label says WHOSE device list is in it.
  //
  // It used to CHANGE, because the return-source control swapped the dropdown
  // between the browser's mediaDeviceIds and Windows' WASAPI endpoint ids. That
  // control is gone: audio always comes from Kinesis, so this is always the
  // browser's list and always writes config.headphoneDeviceId. The label is
  // still drawn because a dropdown of device names gives no clue which
  // identifier space it belongs to, and the WASAPI field still exists on the
  // Settings screen for the native return that is kept as a capability.
  const headphoneRow = document.createElement('div');
  headphoneRow.className = 'control-group';
  const headphoneLabel = document.createElement('label');
  headphoneLabel.htmlFor = 'headphone-select';
  headphoneLabel.textContent = `Headphones — ${DEVICE_SOURCE_WEBRTC}`;
  headphoneRow.append(headphoneLabel, headphoneSelect);

  controls.append(
    makeRow('Commentary input', 'input-select', inputSelect),
    headphoneRow,
    returnGroup,
    channelGroup,
    sourceGroup,
    levelGroup,
  );

  const returnNotes = document.createElement('div');
  returnNotes.className = 'return-notes';
  returnNotes.append(sourceNote, srtStatusLine);

  // --- start/stop + lamps -------------------------------------------------
  const actionRow = document.createElement('div');
  actionRow.className = 'action-row';

  const startStopBtn = document.createElement('button');
  startStopBtn.type = 'button';
  startStopBtn.className = 'btn btn-primary btn-start';
  startStopBtn.textContent = 'START';
  startStopBtn.addEventListener('click', () => handlers.onStartStop());

  const { el: lampsEl, lamps } = createLampRow(LAMP_NAMES);

  actionRow.append(startStopBtn, lampsEl);

  const statusUnavailableBanner = document.createElement('p');
  statusUnavailableBanner.className = 'status-unavailable-banner';
  statusUnavailableBanner.textContent =
    'STATUS UNAVAILABLE — the switcher status feed has been silent for over 15 seconds.';
  statusUnavailableBanner.hidden = true;

  el.append(
    header,
    errorBanner,
    pgmStage,
    audioEl,
    controls,
    returnNotes,
    actionRow,
    statusUnavailableBanner,
  );

  // --- setters --------------------------------------------------------

  function fillDeviceSelect(select, devices, selectedId, emptyLabel) {
    const previousValue = selectedId ?? select.value;
    select.textContent = '';
    if (!devices || devices.length === 0) {
      const opt = document.createElement('option');
      opt.value = '';
      opt.textContent = emptyLabel;
      select.appendChild(opt);
      select.disabled = true;
      return;
    }
    select.disabled = false;
    for (const d of devices) {
      const opt = document.createElement('option');
      opt.value = d.id;
      opt.textContent = d.name;
      select.appendChild(opt);
    }
    if (previousValue && devices.some((d) => d.id === previousValue)) {
      select.value = previousValue;
    }
  }

  function setInputDevices(devices, selectedId) {
    fillDeviceSelect(inputSelect, devices, selectedId, 'No input devices found');
  }

  function setHeadphoneDevices(devices, selectedId) {
    fillDeviceSelect(headphoneSelect, devices, selectedId, 'No output devices found');
  }

  function setReturnMid(mid) {
    // A mid outside 1..7 — a hand-edited config.json, an older file — would
    // leave the <select> showing nothing at all, which reads as "no return" to
    // somebody who is about to commentate. Fall back to the documented default.
    returnSelect.value = isValidReturnMid(mid) ? String(mid) : String(DEFAULT_RETURN_MID);
  }

  function setReturnChannel(mode) {
    channelSegmented.set(normaliseChannelMode(mode));
  }

  let currentPictureSource = DEFAULT_PICTURE_SOURCE;
  let currentPictureState = null;

  /**
   * renderPicture draws what the SELECTION and the RECEIVER'S STATE mean: the
   * badge over the tile, the segmented control, and the status line under it.
   *
   * Both feed it, because neither alone says what is on screen: "SRT selected"
   * with the receiver in BACKOFF is a commentator watching the mosaic, and
   * saying "SRT" over the top of that would be a lie told in large letters.
   *
   * IT DOES NOT DECIDE WHETHER THE MOSAIC IS SUPPRESSED, and it used to. That
   * is setPictureOverlaid's job and it answers a different question — is the
   * native window actually on screen — which the selection cannot answer. See
   * setPictureOverlaid.
   */
  function renderPicture() {
    const effects = derivePictureSourceEffects(currentPictureSource, currentPictureState);
    sourceSegmented.set(effects.source);

    const showing = describePictureShowing(effects, currentPictureState);
    pictureBadge.textContent = showing.text;
    pictureBadge.title = showing.detail;
    pictureBadge.classList.toggle('picture-badge-fallback', !showing.good);

    sourceNote.textContent = [describePictureSource(effects.source), PICTURE_NOTE, LATENCY_NOTE].join(' ');

    // The status line is drawn only while the SRT picture is being ASKED for.
    // A status line for a receiver nobody wants running reads as a fault.
    if (!effects.wantSRT || !currentPictureState) {
      srtStatusLine.hidden = true;
      srtStatusLine.textContent = '';
      return;
    }
    // The wire value is lowercase — it is internal/gst's PictureState — and it
    // is SHOUTED here, beside the uppercase lamp states, because a commentator
    // reading this line at a glance is reading a status word and not a Go
    // identifier. An unrecognised state still prints, with no gloss: a state
    // this build does not know is shown as itself rather than swallowed.
    const name = normalisePictureState(currentPictureState) || String(currentPictureState);
    const words = PICTURE_STATE_WORDS[name];
    srtStatusLine.textContent = `SRT picture: ${name.toUpperCase()}${words ? ` — ${words}` : ''}`;
    // Only BACKOFF is called out. It is the one state that means something went
    // wrong and is retrying; the others are normal parts of coming up.
    //
    // Compared against the CONSTANT, not against the shouted string above: the
    // wire value is lowercase and `name.toUpperCase()` is display only.
    srtStatusLine.classList.toggle('picture-status-bad', name === PICTURE_STATE_BACKOFF);
    srtStatusLine.hidden = false;
  }

  /** setPictureSource selects which picture the application should try for. */
  function setPictureSource(source) {
    currentPictureSource = normalisePictureSource(source);
    renderPicture();
  }

  /**
   * setPictureAvailable disables the SRT option, with the reason on the control.
   * Used when the build has no native picture bindings — a real possibility
   * while the Go side lands — so the option is visibly unavailable rather than
   * silently failing when it is pressed.
   */
  function setPictureAvailable(available, reason) {
    sourceSegmented.setOptionEnabled(PICTURE_SOURCE_SRT, available !== false, reason);
  }

  /**
   * setPictureState records the native receiver's own state, one of the four
   * strings on the "picture" event. Null hides the status line.
   */
  function setPictureState(state) {
    currentPictureState = state ? String(state) : null;
    renderPicture();
  }

  /**
   * setPictureOverlaid says whether the native overlay window is ACTUALLY ON
   * SCREEN over this tile. It is the only thing that may suppress the mosaic.
   *
   * ================== IT IS NOT "SRT IS THE CHOSEN SOURCE" ====================
   *
   * This used to be driven from renderPicture, off `effects.showingSRT`, and
   * that is a different fact. The overlay is a native child window and it is
   * HIDDEN whenever anything must appear above it — the mixer drawer, Settings,
   * a modal — none of which changes the picture source at all. So opening the
   * drawer took the native video away and left the mosaic suppressed
   * underneath, and the commentator got BLACK.
   *
   * The rule is one sentence and it has no exceptions: whenever the overlay is
   * not visible, the mosaic is. The mosaic exists to be the thing underneath,
   * and something underneath that is hidden is not a fallback.
   *
   * app.js is the caller, driven from the overlay controller's own visibility —
   * the same expression that decides the native SetVisible — so the page and the
   * window cannot disagree about what is on screen.
   *
   * The mosaic is MARKED, not removed. It stays in the document and stays
   * decoding: a fallback that has to re-establish itself when it is needed is
   * not one.
   */
  function setPictureOverlaid(overlaid) {
    pgmTile.classList.toggle('pgm-tile-overlaid', overlaid === true);
  }

  /**
   * measurePictureRect reports the reserved box in CSS pixels, relative to the
   * viewport — which is the WebView client area.
   *
   * It is deliberately raw: no rounding, no device pixel ratio, no opinion. CSS
   * pixels and the ratio are what App.SetPictureRect takes — gst.ScaleRect does
   * the multiplication — and ./overlay.js is the only module on this side that
   * has anything to say about the conversion. Returns null when the element has
   * no box to measure, which is what a hidden view looks like.
   *
   * @returns {{x: number, y: number, width: number, height: number}|null}
   */
  function measurePictureRect() {
    if (typeof pgmTile.getBoundingClientRect !== 'function') return null;
    const r = pgmTile.getBoundingClientRect();
    if (!r || !(r.width > 0) || !(r.height > 0)) return null;
    return { x: r.left, y: r.top, width: r.width, height: r.height };
  }

  // Draw the note and the badge once at construction so neither is blank before
  // any config has loaded.
  renderPicture();

  function setLevel(fraction) {
    levelSlider.value = String(Math.round(Math.max(0, Math.min(1, fraction)) * 100));
  }

  function setRunning(running) {
    startStopBtn.textContent = running ? 'STOP' : 'START';
    startStopBtn.classList.toggle('btn-stop', running);
    startStopBtn.classList.toggle('btn-primary', !running);
    startStopBtn.setAttribute('aria-pressed', String(running));
  }

  function setBusy(busy) {
    startStopBtn.disabled = busy;
  }

  function setStatusUnavailable(unavailable) {
    statusUnavailableBanner.hidden = !unavailable;
  }

  return {
    el,
    videoEl,
    audioEl,
    // The element whose box the native overlay is told to occupy. app.js
    // observes it; nothing here knows what it is for.
    pictureEl: pgmTile,
    lamps,
    setDevBadge,
    setTile,
    setInputDevices,
    setHeadphoneDevices,
    setReturnMid,
    setReturnChannel,
    setPictureSource,
    setPictureAvailable,
    setPictureState,
    setPictureOverlaid,
    measurePictureRect,
    setLevel,
    setRunning,
    setBusy,
    setStatusUnavailable,
    showError,
    clearError,
  };
}
