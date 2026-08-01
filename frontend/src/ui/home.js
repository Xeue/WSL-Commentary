import { createLampRow, deriveHonestLine } from './lamps.js';
import { effectiveCrop, describeCrop, REFERENCE_MOSAIC } from './tile.js';
import { RETURN_BUSES, RETURN_HINT, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';

// The main screen: the PGM tile, the three device/return controls, the
// START/STOP button, the five lamps and the permanent honest line.
// Specification section 10, verbatim layout.
//
// Owner: WP-5b.
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
 *   setLevel(fraction)                  positions the level slider, 0..1
 *   setRunning(running)                 flips the START/STOP button
 *   setSenderState(state)               updates the honest line's claim
 *   setBusy(busy)                       disables the button while a call is in flight
 *   setStatusUnavailable(unavailable)   shows/hides the transient banner (Status.Stale)
 *   showError(message) / clearError()   the dismissible error banner
 *
 * handlers = {
 *   onSettings(), onMixer(), onStartStop(),
 *   onInputChange(deviceId), onHeadphoneChange(deviceId),
 *   onReturnChange(mid), onLevelChange(fraction),
 * }
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

  // --- PGM tile --------------------------------------------------------
  //
  // The return is the main thing a commentator looks at, so it gets every
  // pixel left after the controls: .pgm-stage is the flex child that grows,
  // and .pgm-tile is sized inside it from the tile's own aspect ratio — height
  // -limited on a wide window, width-limited on a narrow one. See main.css.
  const pgmStage = document.createElement('div');
  pgmStage.className = 'pgm-stage';
  const pgmTile = document.createElement('div');
  pgmTile.className = 'pgm-tile';
  const videoEl = document.createElement('video');
  videoEl.autoplay = true;
  videoEl.playsInline = true;
  videoEl.muted = true; // the mosaic video track carries no audio we want; return audio is separate
  pgmTile.appendChild(videoEl);
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
  returnLabel.textContent = 'Return';
  const returnHint = document.createElement('p');
  returnHint.className = 'control-hint';
  returnHint.textContent = RETURN_HINT;
  returnGroup.append(returnLabel, returnSelect, returnHint);

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

  controls.append(
    makeRow('Commentary input', 'input-select', inputSelect),
    makeRow('Headphones', 'headphone-select', headphoneSelect),
    returnGroup,
    levelGroup,
  );

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

  // The honest line. Permanent, never dismissible, never a tooltip — and its
  // CLAIM tracks the sender state, because it was asserting "your feed is
  // reaching the switcher" while stopped, which was simply untrue. The caveat
  // is in every version of it. See deriveHonestLine in lamps.js.
  const honestLine = document.createElement('p');
  honestLine.className = 'honest-line';
  honestLine.textContent = deriveHonestLine(undefined);

  function setSenderState(state) {
    honestLine.textContent = deriveHonestLine(state);
  }

  el.append(header, errorBanner, pgmStage, audioEl, controls, actionRow, statusUnavailableBanner, honestLine);

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
    lamps,
    setDevBadge,
    setTile,
    setInputDevices,
    setHeadphoneDevices,
    setReturnMid,
    setLevel,
    setRunning,
    setSenderState,
    setBusy,
    setStatusUnavailable,
    showError,
    clearError,
  };
}
