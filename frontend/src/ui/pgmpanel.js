/**
 * The PGM PANEL: the commentator's picture, the input meters and the return
 * audio controls, as ONE component that is used in two places.
 *
 * Owner: WP-P.
 *
 * ======================= WHERE THE PANEL LIVES ==============================
 *
 * In the PGM MONITOR WINDOW — a second Wails window, in a second process,
 * launched by the application (app_monitor.go / monitor_app.go). That is where
 * the mosaic's WebRTC connection, the return audio it carries, the SRT picture
 * pipeline and the meters all live now, so that a frozen picture or a distorted
 * return is fixed by the Refresh button in that window, and a monitor that will
 * not answer at all is fixed by "Restart monitor" in the application's window —
 * killed and reopened — with the contribution feed untouched throughout.
 * monitorview.js mounts this panel there.
 *
 * And INLINE on a REMOTE seat's page. A browser on the LAN loads the same
 * frontend over the bridge and cannot open a window on the host, so home.js
 * keeps the panel in its main area for remote clients: the same mosaic, the
 * same meters, the same controls, minus the SRT picture (which is physics,
 * not a build option — see docs/remote-access.md).
 *
 * The application's own window has NEITHER any more. It shows a card that says
 * where the picture is and a button that restarts the monitor.
 *
 * ========================= WHAT IS IN IT ===================================
 *
 *   tileEl        the PGM tile: the mosaic <video>, cropped by CSS to the
 *                 configured tile, with the badge saying which picture is on
 *                 screen. In the monitor window the SRT picture is a native
 *                 window painted over EXACTLY this rectangle (measurePictureRect)
 *                 and the mosaic under it is suppressed while it is up
 *                 (setPictureOverlaid).
 *   metersEl      the two-channel input meters, fed by "levels" frames.
 *   pictureGroup  the mosaic/SRT switch, with the Refresh button beside it.
 *   headphoneRow, returnGroup, channelGroup, levelGroup
 *                 the four return-audio controls: which output device, which
 *                 bus, stereo/left/right, and the monitoring level.
 *
 * The panel builds DOM and exposes setters; it does not talk to Go. app.js owns
 * every decision (which picture, when to refresh, what the KVS monitor does)
 * and calls the setters; the panel calls the handlers it was given.
 *
 * handlers = {
 *   onPictureSourceChange(src), onPictureRefresh(),
 *   onHeadphoneChange(deviceId), onReturnChange(mid),
 *   onReturnChannelChange(mode), onLevelChange(fraction),
 * }
 *
 * opts = {
 *   showError(message, severity), clearErrorIf(message)
 *                 where the BACKOFF-episode banner goes: the first failure of
 *                 an unbroken failing run raises it, recovery takes it down.
 *   refreshButton whether the switch row carries its own Refresh button.
 *                 The monitor window puts a large one in its header instead.
 * }
 */

import { effectiveCrop, describeCrop, REFERENCE_MOSAIC } from './tile.js';
import { sortDevices, labelDevices, describeDeviceSelection } from './devices.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
import { CHANNEL_MODES, DEFAULT_CHANNEL_MODE, normaliseChannelMode } from '../monitor/channels.js';
import {
  PICTURE_SOURCES,
  PICTURE_SOURCE_SRT,
  DEFAULT_PICTURE_SOURCE,
  normalisePictureSource,
  derivePictureSourceEffects,
  describePictureShowing,
  PICTURE_BACKOFF_ERROR,
  normalisePictureState,
} from './picturesource.js';
import { createBackoffEpisode } from './errorlog.js';
import { sliderToLevel, levelToSlider, describeLevelPosition, DEFAULT_LEVEL_POSITION } from './levelmap.js';
import { meterZones, zoneFills, dbToFraction, isSilentFrame, createPeakHold } from './meters.js';

/**
 * makeSegmented builds a segmented control from radio inputs, so a screen
 * reader and a keyboard already understand it and exactly-one-selected is
 * enforced by the platform rather than by this file.
 *
 * Returns { el, set(value), setOptionEnabled(value, enabled, reason) }.
 */
export function makeSegmented(name, options, onChange) {
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
  // :has(:checked): :has() is a recent selector and this is the difference
  // between a control that shows which option is live and one that looks like
  // nothing is selected.
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
      if (!enabled && reason) entry.label.title = reason;
    },
  };
}

/**
 * fillDeviceSelect populates a device dropdown, keeping the saved selection
 * even when the device is missing — as a disabled first option that says so —
 * rather than silently snapping to whatever is first in the list.
 */
export function fillDeviceSelect(select, devices, selectedId, emptyLabel) {
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
  const ordered = labelDevices(sortDevices(devices));
  for (const d of ordered) {
    const opt = document.createElement('option');
    opt.value = d.id;
    opt.textContent = d.label;
    select.appendChild(opt);
  }
  const selection = describeDeviceSelection(ordered, previousValue);
  if (selection.present) {
    if (selection.savedId !== '') select.value = selection.savedId;
    return;
  }
  const missing = document.createElement('option');
  missing.value = selection.savedId;
  missing.textContent = selection.label;
  missing.disabled = true;
  select.insertBefore(missing, select.firstChild);
  select.value = selection.savedId;
}

export function createPgmPanel(handlers, opts = {}) {
  const showError = typeof opts.showError === 'function' ? opts.showError : () => {};
  const clearErrorIf = typeof opts.clearErrorIf === 'function' ? opts.clearErrorIf : () => {};
  const withRefreshButton = opts.refreshButton !== false;

  // --- the picture tile ------------------------------------------------------
  //
  // TWO PICTURES, ONE BOX. The <video> inside it is the WebRTC mosaic, cropped
  // to the PGM tile; in the monitor window the native SRT overlay is painted
  // over the same rectangle from outside the page entirely. Sharing the box is
  // what makes the fallback invisible as a layout event: when SRT drops, the
  // mosaic underneath is already the right size and in the right place.
  const tileEl = document.createElement('div');
  tileEl.className = 'pgm-tile';
  const videoEl = document.createElement('video');
  videoEl.autoplay = true;
  videoEl.playsInline = true;
  videoEl.muted = true; // the mosaic video track carries no audio we want; return audio is separate
  tileEl.appendChild(videoEl);

  // WHICH PICTURE IS ON SCREEN, said permanently, over the picture. The two
  // look alike at a glance and differ enough in quality that somebody will ask
  // out loud during a match whether they are looking at the good one.
  const pictureBadge = document.createElement('div');
  pictureBadge.className = 'picture-badge';
  tileEl.appendChild(pictureBadge);

  // --- the crop ----------------------------------------------------------------
  let configuredTile = { x: 0, y: 360, w: 640, h: 360 };
  let liveMosaic = null;
  let lastDescription = '';

  function applyCrop() {
    const crop = effectiveCrop(configuredTile, liveMosaic, REFERENCE_MOSAIC);
    tileEl.style.setProperty('--mosaic-w', String(crop.mosaic.w));
    tileEl.style.setProperty('--mosaic-h', String(crop.mosaic.h));
    tileEl.style.setProperty('--tile-x', String(crop.tile.x));
    tileEl.style.setProperty('--tile-y', String(crop.tile.y));
    tileEl.style.setProperty('--tile-w', String(crop.tile.w));
    tileEl.style.setProperty('--tile-h', String(crop.tile.h));
    tileEl.style.setProperty('--tile-ar', `${crop.tile.w} / ${crop.tile.h}`);
    tileEl.style.setProperty('--tile-ar-num', String(crop.aspect));
    // The stage the tile sits in sizes the tile from this too; it is set on
    // the parent when the panel is placed (see stageAspectTarget).
    if (stageAspectTarget) stageAspectTarget.style.setProperty('--tile-ar-num', String(crop.aspect));
    const line = describeCrop(crop, configuredTile);
    if (line !== lastDescription) {
      lastDescription = line;
      console.info(line);
    }
  }

  let stageAspectTarget = null;

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
  videoEl.addEventListener('loadedmetadata', readMosaic);
  videoEl.addEventListener('resize', readMosaic);
  applyCrop();

  function setTile(tile) {
    if (tile && typeof tile === 'object') configuredTile = tile;
    readMosaic();
    applyCrop();
  }

  /**
   * attachStage tells the panel which element sizes the tile (the .pgm-stage
   * it sits in), so the aspect-ratio variable reaches it.
   */
  function attachStage(stageEl) {
    stageAspectTarget = stageEl;
    applyCrop();
  }

  // --- the input meters --------------------------------------------------------
  //
  // OUTSIDE the tile, beside it: in the monitor window the native SRT overlay
  // covers exactly the tile's rectangle, and anything drawn inside it is
  // invisible for as long as the good picture is up — precisely when a
  // commentator most needs to see their input.
  const metersEl = document.createElement('div');
  metersEl.className = 'input-meters input-meters-idle';
  metersEl.title =
    'Commentary input level, measured where the capture pipeline hands the audio to the encoder — ' +
    'live from launch, whether or not you are sending. Green to -18 dBFS, amber to -6, red above.';
  const meterChannels = ['L', 'R'].map((name) => {
    const channel = document.createElement('div');
    channel.className = 'input-meter';
    const bar = document.createElement('div');
    bar.className = 'input-meter-bar';
    const fills = meterZones().map(({ zone, from, to }) => {
      const seg = document.createElement('div');
      seg.className = `input-meter-seg input-meter-seg--${zone}`;
      seg.style.flexBasis = `${((to - from) * 100).toFixed(1)}%`;
      const fill = document.createElement('div');
      fill.className = `input-meter-fill input-meter-fill--${zone}`;
      seg.appendChild(fill);
      bar.appendChild(seg);
      return fill;
    });
    const peakMark = document.createElement('div');
    peakMark.className = 'input-meter-peak';
    peakMark.hidden = true;
    bar.appendChild(peakMark);
    const label = document.createElement('span');
    label.className = 'input-meter-label';
    label.textContent = name;
    channel.append(bar, label);
    metersEl.appendChild(channel);
    return { fills, peakMark };
  });
  const inputPeakHold = createPeakHold();

  /**
   * setLevels paints the meters from one "levels" frame {peak:[], rms:[]}; an
   * all-silence frame (or null) dims them.
   */
  function setLevels(frame) {
    const silent = isSilentFrame(frame);
    metersEl.classList.toggle('input-meters-idle', silent);
    if (silent) {
      inputPeakHold.reset();
      for (const ch of meterChannels) {
        ch.fills.forEach((fill) => {
          fill.style.height = '0%';
        });
        ch.peakMark.hidden = true;
      }
      return;
    }
    const peaks = Array.isArray(frame.peak) ? frame.peak : [];
    const rms = Array.isArray(frame.rms) ? frame.rms : [];
    const marks = inputPeakHold.update(peaks);
    meterChannels.forEach((ch, i) => {
      const fills = zoneFills(rms[i]);
      ch.fills.forEach((fill, z) => {
        fill.style.height = `${(Math.round(fills[z] * 1000) / 10).toFixed(1)}%`;
      });
      const frac = dbToFraction(marks[i]);
      ch.peakMark.hidden = !(frac > 0);
      ch.peakMark.style.bottom = `${(Math.round(frac * 1000) / 10).toFixed(1)}%`;
    });
  }

  // --- the return-audio controls ---------------------------------------------
  const headphoneSelect = document.createElement('select');
  headphoneSelect.id = 'headphone-select';
  headphoneSelect.addEventListener('change', () => handlers.onHeadphoneChange(headphoneSelect.value));
  const headphoneRow = document.createElement('div');
  headphoneRow.className = 'control-group';
  const headphoneLabel = document.createElement('label');
  headphoneLabel.htmlFor = 'headphone-select';
  headphoneLabel.textContent = 'Headphones/output';
  headphoneRow.append(headphoneLabel, headphoneSelect);

  const returnSelect = document.createElement('select');
  returnSelect.id = 'return-select';
  for (const bus of RETURN_BUSES) {
    const opt = document.createElement('option');
    opt.value = String(bus.mid);
    opt.textContent = bus.label;
    returnSelect.appendChild(opt);
  }
  returnSelect.value = String(DEFAULT_RETURN_MID);
  returnSelect.addEventListener('change', () => handlers.onReturnChange(Number(returnSelect.value)));
  const returnGroup = document.createElement('div');
  returnGroup.className = 'control-group control-group-return';
  const returnLabel = document.createElement('label');
  returnLabel.htmlFor = 'return-select';
  returnLabel.textContent = 'Return Audio';
  returnGroup.append(returnLabel, returnSelect);

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

  // THE LEVEL SLIDER READS IN DECIBELS — levelmap.js. It used to be linear
  // into the gain multiplier, which with +18 dB of make-up gain put a
  // comfortable level at 5–10 % of its travel. The readout beside the label
  // says what the position means; what leaves here is still the 0..1 linear
  // multiplier the monitor takes. Where it STARTS is app.js's business (the
  // remembered level); this default is only the shape of a never-set slider.
  const levelGroup = document.createElement('div');
  levelGroup.className = 'control-group control-group-level';
  const levelHead = document.createElement('div');
  levelHead.className = 'level-head';
  const levelLabel = document.createElement('label');
  levelLabel.htmlFor = 'level-slider';
  levelLabel.textContent = 'Return Level';
  const levelReadout = document.createElement('span');
  levelReadout.className = 'level-readout';
  levelReadout.setAttribute('aria-live', 'off');
  levelHead.append(levelLabel, levelReadout);
  const levelSlider = document.createElement('input');
  levelSlider.type = 'range';
  levelSlider.id = 'level-slider';
  levelSlider.min = '0';
  levelSlider.max = '100';
  levelSlider.step = '1';
  levelSlider.value = String(DEFAULT_LEVEL_POSITION);
  function paintLevel() {
    levelReadout.textContent = describeLevelPosition(Number(levelSlider.value));
  }
  levelSlider.addEventListener('input', () => {
    paintLevel();
    handlers.onLevelChange(sliderToLevel(Number(levelSlider.value)));
  });
  paintLevel();
  levelGroup.append(levelHead, levelSlider);

  // --- the picture switch and Refresh ----------------------------------------
  //
  // THIS IS NOT AN AUDIO CONTROL. It chooses WHICH PICTURE the commentator is
  // looking at; the audio always comes from Kinesis, whatever it says.
  //
  // THE NOTES UNDER THE CONTROLS ARE GONE, at the operator's request — the
  // paragraph explaining the selected picture (describePictureSource +
  // PICTURE_NOTE + LATENCY_NOTE) and the separate SRT status line under it.
  // The words are untouched in ./picturesource.js, where Settings and the
  // option tooltips still use them. The one state that mattered from the
  // status line — BACKOFF — speaks through the alerts instead, once per
  // episode; see setPictureState below.
  const sourceSegmented = makeSegmented(
    'picture-source',
    PICTURE_SOURCES.map((s) => ({ value: s.value, label: s.label, hint: s.summary })),
    (source) => handlers.onPictureSourceChange(source),
  );
  sourceSegmented.set(DEFAULT_PICTURE_SOURCE);

  // REFRESH: the picture has frozen, or the return sounds wrong. It gives
  // WHICHEVER PICTURE IS ACTIVE a kick from nothing — app.js decides which
  // half; see onPictureRefresh there — and never touches the feed.
  const refreshBtn = document.createElement('button');
  refreshBtn.type = 'button';
  refreshBtn.className = 'btn btn-ghost btn-small picture-refresh';
  refreshBtn.textContent = 'Refresh';
  refreshBtn.title = REFRESH_TITLE;
  refreshBtn.addEventListener('click', () => handlers.onPictureRefresh());

  const pictureGroup = document.createElement('div');
  pictureGroup.className = 'control-group control-group-source';
  const sourceLabel = document.createElement('span');
  sourceLabel.className = 'control-label';
  sourceLabel.textContent = 'Picture';
  const sourceRow = document.createElement('div');
  sourceRow.className = 'picture-source-row';
  sourceRow.append(sourceSegmented.el);
  if (withRefreshButton) sourceRow.append(refreshBtn);
  pictureGroup.append(sourceLabel, sourceRow);

  // --- picture state -------------------------------------------------------------
  const backoffEpisode = createBackoffEpisode();
  let currentPictureSource = DEFAULT_PICTURE_SOURCE;
  let currentPictureState = null;

  /**
   * renderPicture draws what the SELECTION and the RECEIVER'S STATE mean: the
   * badge over the tile and the segmented control. Both feed it, because
   * neither alone says what is on screen: "SRT selected" with the receiver in
   * BACKOFF is a commentator watching the mosaic. IT DOES NOT DECIDE WHETHER
   * THE MOSAIC IS SUPPRESSED: that is setPictureOverlaid's job.
   */
  function renderPicture() {
    const effects = derivePictureSourceEffects(currentPictureSource, currentPictureState);
    sourceSegmented.set(effects.source);
    const showing = describePictureShowing(effects, currentPictureState);
    pictureBadge.textContent = showing.text;
    pictureBadge.title = showing.detail;
    pictureBadge.classList.toggle('picture-badge-fallback', !showing.good);
  }

  function setPictureSource(source) {
    currentPictureSource = normalisePictureSource(source);
    renderPicture();
  }

  function setPictureAvailable(available, reason) {
    sourceSegmented.setOptionEnabled(PICTURE_SOURCE_SRT, available !== false, reason);
  }

  /**
   * setPictureState records the native receiver's state and feeds the
   * backoff-episode tracker: the first failure of an unbroken failing run
   * raises the banner, the retry cycling inside that run stays silent, and
   * recovery takes the banner down again — if it is still showing THIS
   * message.
   */
  function setPictureState(state) {
    currentPictureState = state ? String(state) : null;
    switch (backoffEpisode.track(normalisePictureState(currentPictureState))) {
      case 'raise':
        showError(PICTURE_BACKOFF_ERROR);
        break;
      case 'clear':
        clearErrorIf(PICTURE_BACKOFF_ERROR);
        break;
    }
    renderPicture();
  }

  /**
   * setPictureOverlaid says whether the native overlay window is ACTUALLY ON
   * SCREEN over this tile. It is the only thing that may suppress the mosaic,
   * and it is NOT "SRT is the chosen source": the overlay is hidden whenever
   * anything must appear above it — a modal, a drawer — none of which changes
   * the source, and suppressing the mosaic then leaves the commentator with
   * black. Whenever the overlay is not visible, the mosaic is. The mosaic is
   * MARKED, not removed: a fallback that has to re-establish itself is not one.
   */
  function setPictureOverlaid(overlaid) {
    tileEl.classList.toggle('pgm-tile-overlaid', overlaid === true);
  }

  /**
   * measurePictureRect reports the tile's box in CSS pixels, relative to the
   * viewport — the WebView client area. Raw: no rounding, no device pixel
   * ratio; overlay.js owns the conversion. Null when the element has no box,
   * which is what a hidden view looks like.
   */
  function measurePictureRect() {
    if (typeof tileEl.getBoundingClientRect !== 'function') return null;
    const r = tileEl.getBoundingClientRect();
    if (!r || !(r.width > 0) || !(r.height > 0)) return null;
    return { x: r.left, y: r.top, width: r.width, height: r.height };
  }

  function setHeadphoneDevices(devices, selectedId) {
    fillDeviceSelect(headphoneSelect, devices, selectedId, 'No output devices found');
  }
  function setReturnMid(mid) {
    returnSelect.value = isValidReturnMid(mid) ? String(mid) : String(DEFAULT_RETURN_MID);
  }
  function setReturnChannel(mode) {
    channelSegmented.set(normaliseChannelMode(mode));
  }
  function setLevel(fraction) {
    levelSlider.value = String(levelToSlider(fraction));
    paintLevel();
  }
  /** getLevel is the slider's current 0..1 linear multiplier. */
  function getLevel() {
    return sliderToLevel(Number(levelSlider.value));
  }

  renderPicture();

  return {
    tileEl,
    videoEl,
    metersEl,
    pictureGroup,
    headphoneRow,
    returnGroup,
    channelGroup,
    levelGroup,
    attachStage,
    setTile,
    setLevels,
    setPictureSource,
    setPictureAvailable,
    setPictureState,
    setPictureOverlaid,
    measurePictureRect,
    setHeadphoneDevices,
    setReturnMid,
    setReturnChannel,
    setLevel,
    getLevel,
  };
}

/** The Refresh button's tooltip, shared by the panel's button and the monitor window's. */
export const REFRESH_TITLE =
  'Give the picture a kick. Mosaic: reconnects the multiviewer and the return audio with it ' +
  '(a moment of silence). SRT: restarts the SRT picture, dialling M2L-X again. ' +
  'The feed going to air is never touched.';
