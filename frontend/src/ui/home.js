import { createLampRow } from './lamps.js';

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

// MOSAIC_WIDTH/MOSAIC_HEIGHT are the KVS multiviewer mosaic's fixed
// dimensions (specification section 7). They are an M2L-X constant, not
// configuration — unlike the tile rectangle within it, which is
// config.monitorTile and can change without a code change.
const MOSAIC_WIDTH = 2240;
const MOSAIC_HEIGHT = 1440;

const LAMP_NAMES = ['SENDING', 'SWITCHER SEES FEED', 'VIDEO', 'AUDIO', 'MONITOR'];

const HONEST_LINE =
  'Your feed is reaching the switcher. This does not confirm you are audible on the broadcast output.';

/**
 * createHomeView builds the home screen and returns:
 *
 *   el          the root <section>, ready to insert into the document
 *   videoEl     the <video> the monitor mosaic attaches to (opts.videoEl)
 *   audioEl     the <audio> the monitor return plays through (opts.audioEl)
 *   lamps       the five lamps by name, as created by createLampRow
 *   setTile(tile)                       positions the PGM crop from config.monitorTile
 *   setInputDevices(devices, selected)  populates the commentary input dropdown
 *   setHeadphoneDevices(devices, sel)   populates the headphones dropdown
 *   setReturnMid(mid)                   selects CLN(2)/PGM(1) in the return dropdown
 *   setLevel(fraction)                  positions the level slider, 0..1
 *   setRunning(running)                 flips the START/STOP button
 *   setBusy(busy)                       disables the button while a call is in flight
 *   setStatusUnavailable(unavailable)   shows/hides the transient banner (Status.Stale)
 *   showError(message) / clearError()   the dismissible error banner
 *
 * handlers = {
 *   onSettings(), onStartStop(),
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
  const settingsBtn = document.createElement('button');
  settingsBtn.type = 'button';
  settingsBtn.className = 'btn btn-ghost';
  settingsBtn.textContent = 'Settings';
  settingsBtn.addEventListener('click', () => handlers.onSettings());
  header.append(titleWrap, settingsBtn);

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
  const pgmWrap = document.createElement('div');
  pgmWrap.className = 'pgm-wrap';
  const pgmTile = document.createElement('div');
  pgmTile.className = 'pgm-tile';
  const videoEl = document.createElement('video');
  videoEl.autoplay = true;
  videoEl.playsInline = true;
  videoEl.muted = true; // the mosaic video track carries no audio we want; return audio is separate
  pgmTile.appendChild(videoEl);
  pgmWrap.appendChild(pgmTile);

  const audioEl = document.createElement('audio');
  audioEl.autoplay = true;
  audioEl.hidden = true;

  function setTile(tile) {
    pgmTile.style.setProperty('--mosaic-w', String(MOSAIC_WIDTH));
    pgmTile.style.setProperty('--mosaic-h', String(MOSAIC_HEIGHT));
    pgmTile.style.setProperty('--tile-x', String(tile.x));
    pgmTile.style.setProperty('--tile-y', String(tile.y));
    pgmTile.style.setProperty('--tile-w', String(tile.w));
    pgmTile.style.setProperty('--tile-h', String(tile.h));
  }

  // --- controls ----------------------------------------------------------
  const controls = document.createElement('div');
  controls.className = 'controls';

  function makeRow(labelText, id, control) {
    const row = document.createElement('div');
    row.className = 'control-row';
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

  const returnSelect = document.createElement('select');
  returnSelect.id = 'return-select';
  const optCLN = document.createElement('option');
  optCLN.value = '2';
  optCLN.textContent = 'CLN (effects, no commentary)';
  const optPGM = document.createElement('option');
  optPGM.value = '1';
  optPGM.textContent = 'PGM';
  returnSelect.append(optCLN, optPGM);
  returnSelect.addEventListener('change', () => handlers.onReturnChange(Number(returnSelect.value)));

  const returnRow = document.createElement('div');
  returnRow.className = 'control-row';
  const returnLabel = document.createElement('label');
  returnLabel.htmlFor = 'return-select';
  returnLabel.textContent = 'Return';
  const levelLabel = document.createElement('label');
  levelLabel.htmlFor = 'level-slider';
  levelLabel.className = 'level-label';
  levelLabel.textContent = 'Level';
  const levelSlider = document.createElement('input');
  levelSlider.type = 'range';
  levelSlider.id = 'level-slider';
  levelSlider.min = '0';
  levelSlider.max = '100';
  levelSlider.step = '1';
  levelSlider.value = '100';
  levelSlider.addEventListener('input', () => handlers.onLevelChange(Number(levelSlider.value) / 100));
  returnRow.append(returnLabel, returnSelect, levelLabel, levelSlider);

  controls.append(
    makeRow('Commentary input', 'input-select', inputSelect),
    makeRow('Headphones', 'headphone-select', headphoneSelect),
    returnRow,
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

  const honestLine = document.createElement('p');
  honestLine.className = 'honest-line';
  honestLine.textContent = HONEST_LINE;

  el.append(header, errorBanner, pgmWrap, audioEl, controls, actionRow, statusUnavailableBanner, honestLine);

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
    returnSelect.value = String(mid);
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
    setBusy,
    setStatusUnavailable,
    showError,
    clearError,
  };
}
