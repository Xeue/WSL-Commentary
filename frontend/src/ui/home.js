import { createLampRow } from './lamps.js';
// The dropdowns' pure logic: display order and the saved-but-missing marker.
// It lives in its own module so `node --test` can drive it without a DOM —
// this file is wiring, devices.test.js is where the behaviour is proved.
import { sortDevices, describeDeviceSelection } from './devices.js';
import { effectiveCrop, describeCrop, REFERENCE_MOSAIC } from './tile.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
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
import { createErrorLog, createBackoffEpisode, describeEntry, formatErrorTime } from './errorlog.js';
// The input meters' maths and state: the mixer's own -60..0 scale and
// -18/-6 zone boundaries (imported there, not copied — two meter scales that
// disagree is the two-tables bug), plus the peak-hold. This file only builds
// the bars and paints what meters.js computes; meters.test.js is where the
// behaviour is proved.
import {
  meterZones,
  zoneFills,
  dbToFraction,
  isSilentFrame,
  createPeakHold,
} from './meters.js';
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
 *   setLevels(frame)                    paints the input meters from one
 *                                       "levels" frame {peak:[], rms:[]}; an
 *                                       all-silence frame (or null) dims them
 *   setLevel(fraction)                  positions the level slider, 0..1
 *   setPresets(list)                    fills the header preset indicator with
 *                                       the saved instances; hides it when there
 *                                       are none
 *   setActivePreset(id)                 marks which instance is running, at a
 *                                       glance, in the header indicator
 *   setRunning(running)                 flips the START/STOP button, and gates
 *                                       the preset selector (switching is
 *                                       refused server-side while SENDING)
 *   setBusy(busy)                       disables the button while a call is in flight
 *   setStatusUnavailable(unavailable)   shows/hides the transient banner (Status.Stale)
 *   showError(message) / clearError()   the dismissible error banner. Every
 *                                       message is also kept in a history
 *                                       (errorlog.js) with timestamps and
 *                                       repeat counts, opened from the
 *                                       banner's History button; dismissing
 *                                       hides the banner, not the history
 *
 * handlers = {
 *   onSettings(), onMixer(), onStartStop(),
 *   onInputChange(deviceId), onHeadphoneChange(deviceId),
 *   onReturnChange(mid), onReturnChannelChange(mode), onPictureSourceChange(src),
 *   onLevelChange(fraction), onPresetChange(id),
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

  // WHO ELSE HAS A SEAT. A small persistent indicator of the remote clients
  // connected to the LAN bridge right now, by name. It is hidden when there are
  // none — the normal case — so it costs no attention until it has something to
  // say, and it says exactly one thing: that a person other than the operator at
  // this desk can drive the application. Without it, a remote operator pressing
  // STOP is indistinguishable from a crash.
  //
  // This file knows nothing about the backend — see the header — so it only
  // exposes setRemoteClients(); app.js wires it to the "remote" event.
  const remoteIndicator = document.createElement('span');
  remoteIndicator.className = 'remote-indicator';
  remoteIndicator.hidden = true;
  titleWrap.append(title, devBadge, remoteIndicator);
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

  // WHICH M2L-X INSTANCE IS RUNNING, at a glance. The operator asked to see the
  // active preset on the main page without opening Settings. It is a <select> so
  // the instance can also be SWITCHED from here — but switching is refused
  // server-side while a session is SENDING (ApplyPreset would otherwise leave
  // the feed going to the PREVIOUS instance with every lamp green), so the
  // control is disabled with that reason on it while running (see setRunning).
  //
  // Quiet by design: the whole indicator is hidden until there is at least one
  // saved instance, and when none is applied it shows a disabled "none" rather
  // than an empty box. This file knows nothing about the backend — it exposes
  // setPresets / setActivePreset and raises onPresetChange; app.js wires it to
  // the SAME apply flow the Settings preset UI uses.
  const presetIndicator = document.createElement('div');
  presetIndicator.className = 'preset-indicator';
  presetIndicator.hidden = true;
  const presetIndicatorLabel = document.createElement('span');
  presetIndicatorLabel.className = 'preset-indicator-label';
  presetIndicatorLabel.textContent = 'Preset:';
  const presetIndicatorSelect = document.createElement('select');
  presetIndicatorSelect.id = 'home-preset-select';
  presetIndicatorSelect.addEventListener('change', () => {
    // The empty "none" option is disabled and unselectable, so a change always
    // carries a real preset id — but guard anyway rather than apply "".
    if (presetIndicatorSelect.value) handlers.onPresetChange(presetIndicatorSelect.value);
  });
  presetIndicator.append(presetIndicatorLabel, presetIndicatorSelect);

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
  headerBtns.append(presetIndicator, mixerBtn, settingsBtn);
  header.append(titleWrap, headerBtns);

  function setDevBadge(visible) {
    devBadge.hidden = !visible;
  }

  // --- the active-preset indicator's state ------------------------------
  //
  // presetList is ListPresets' summaries; activePresetId is GetActivePreset's
  // id ('' means none applied); sendingNow mirrors the START/STOP state so the
  // selector can be gated the same way the Settings Apply button is.
  let presetList = [];
  let activePresetId = '';
  let presetSendingNow = false;

  /**
   * renderPresetIndicator draws the header indicator from the three pieces of
   * state. It hides the whole control when there are no saved instances (quiet
   * until it has something to say), shows a disabled "none" when nothing is
   * applied, and disables switching — with the reason on the control — while a
   * session is running.
   */
  function renderPresetIndicator() {
    if (presetList.length === 0) {
      presetIndicator.hidden = true;
      return;
    }
    presetIndicator.hidden = false;
    presetIndicatorSelect.textContent = '';
    // A "none" option ONLY when nothing is applied: it lets the control show the
    // no-preset state honestly without offering "none" as a thing to switch TO.
    if (!activePresetId) {
      const none = document.createElement('option');
      none.value = '';
      none.textContent = 'none';
      none.disabled = true;
      presetIndicatorSelect.appendChild(none);
    }
    for (const p of presetList) {
      const o = document.createElement('option');
      o.value = p.id;
      o.textContent = p.name;
      presetIndicatorSelect.appendChild(o);
    }
    presetIndicatorSelect.value = activePresetId || '';
    // Gated on the sending state, with the reason ON THE CONTROL: ApplyPreset is
    // refused server-side while a session runs, so a switch that would only ever
    // fail must say why it is disabled rather than throw when pressed.
    presetIndicatorSelect.disabled = presetSendingNow;
    presetIndicatorSelect.title = presetSendingNow
      ? 'Disabled while SENDING: stop the feed before switching instance.'
      : 'The M2L-X instance this seat is pointed at. Switching reconnects to the chosen instance.';
  }

  /** setPresets fills the indicator with the saved instances (id-bearing only). */
  function setPresets(list) {
    presetList = Array.isArray(list) ? list.filter((p) => p && p.id) : [];
    renderPresetIndicator();
  }

  /**
   * setActivePreset marks which instance is running. Accepts the id string (what
   * app.js passes from GetActivePreset), tolerating the whole record for safety.
   */
  function setActivePreset(id) {
    activePresetId = typeof id === 'string' ? id : (id && id.id) || '';
    renderPresetIndicator();
  }

  /**
   * setRemoteClients shows the connected remote seats by name, or hides the
   * indicator entirely when there are none. It takes the "remote" event payload
   * shape — an array of {name, addr} — and shows only the names, because the
   * operator's question is WHO, not from where; the address is kept in the title
   * attribute for the rare moment it is wanted. A malformed or empty payload
   * reads as "nobody", which is the safe default: the indicator over-hiding is a
   * missing reassurance, never a false one.
   */
  function setRemoteClients(clients) {
    const list = Array.isArray(clients) ? clients : [];
    if (list.length === 0) {
      remoteIndicator.hidden = true;
      remoteIndicator.textContent = '';
      remoteIndicator.removeAttribute('title');
      return;
    }
    const names = list.map((c) => (c && c.name ? String(c.name) : '?'));
    remoteIndicator.textContent =
      list.length === 1 ? `Remote seat: ${names[0]}` : `Remote seats: ${names.join(', ')}`;
    remoteIndicator.title = list
      .map((c) => `${c && c.name ? c.name : '?'}${c && c.addr ? ` (${c.addr})` : ''}`)
      .join('\n');
    remoteIndicator.hidden = false;
  }

  // --- error banner (dismissible; NOT the honest line) ----------------
  //
  // The banner shows the NEWEST error; the log behind it (./errorlog.js) keeps
  // every one, so a second problem no longer destroys the evidence of the
  // first. When there is more than one row, a History button appears and opens
  // the list — timestamps, messages, repeat counts. Dismissing the banner
  // keeps the history: dismissal means "I have seen this", not "unhappen it".
  const errorLog = createErrorLog();
  const backoffEpisode = createBackoffEpisode();
  let bannerMessage = '';

  const errorBanner = document.createElement('div');
  errorBanner.className = 'error-banner';
  errorBanner.setAttribute('role', 'alert');
  errorBanner.hidden = true;
  const errorText = document.createElement('span');
  const errorHistoryBtn = document.createElement('button');
  errorHistoryBtn.type = 'button';
  errorHistoryBtn.className = 'error-history-toggle';
  errorHistoryBtn.hidden = true;
  errorHistoryBtn.setAttribute('aria-expanded', 'false');
  errorHistoryBtn.addEventListener('click', () => {
    setErrorHistoryOpen(errorHistoryPanel.hidden);
  });
  const errorDismiss = document.createElement('button');
  errorDismiss.type = 'button';
  errorDismiss.className = 'error-dismiss';
  errorDismiss.setAttribute('aria-label', 'Dismiss');
  errorDismiss.textContent = '✕';
  errorDismiss.addEventListener('click', () => clearError());
  errorBanner.append(errorText, errorHistoryBtn, errorDismiss);

  // The history list, drawn directly under the banner when opened.
  const errorHistoryPanel = document.createElement('div');
  errorHistoryPanel.className = 'error-history';
  errorHistoryPanel.hidden = true;
  const errorHistoryList = document.createElement('ul');
  errorHistoryList.className = 'error-history-list';
  const errorHistoryClear = document.createElement('button');
  errorHistoryClear.type = 'button';
  errorHistoryClear.className = 'error-history-clear';
  errorHistoryClear.textContent = 'Clear history';
  errorHistoryClear.addEventListener('click', () => {
    errorLog.clear();
    clearError();
  });
  errorHistoryPanel.append(errorHistoryList, errorHistoryClear);

  function setErrorHistoryOpen(open) {
    errorHistoryPanel.hidden = !open;
    errorHistoryBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) renderErrorHistory();
  }

  function renderErrorHistory() {
    // The History button exists whenever the log holds more than the line the
    // banner is already showing — a second row, or the one row repeating.
    const worthShowing = errorLog.size > 1 || (errorLog.entries[0]?.count ?? 0) > 1;
    errorHistoryBtn.hidden = !worthShowing;
    errorHistoryBtn.textContent = `History (${errorLog.size})`;
    if (!worthShowing) setErrorHistoryOpen(false);

    if (errorHistoryPanel.hidden) return;
    errorHistoryList.textContent = '';
    for (const entry of errorLog.entries) {
      const li = document.createElement('li');
      li.textContent = describeEntry(entry);
      if (entry.count > 1) {
        li.title = `first at ${formatErrorTime(entry.firstAt)}, ${entry.count} times in all`;
      }
      errorHistoryList.appendChild(li);
    }
  }

  function showError(message) {
    const entry = errorLog.record(message);
    bannerMessage = entry.message;
    errorText.textContent = entry.count > 1 ? `${entry.message} (×${entry.count})` : entry.message;
    errorBanner.hidden = false;
    renderErrorHistory();
  }
  function clearError() {
    errorBanner.hidden = true;
    errorText.textContent = '';
    bannerMessage = '';
    setErrorHistoryOpen(false);
    renderErrorHistory();
  }
  /**
   * clearErrorIf takes the banner down only if it is still showing `message`.
   * For errors that RESOLVE — the SRT picture coming back — where clearing
   * unconditionally would eat whatever unrelated error arrived in between.
   */
  function clearErrorIf(message) {
    if (bannerMessage === message) clearError();
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

  // --- the input meters, at the right edge of the picture area -------------
  //
  // A slim vertical stereo pair fed from the SEND pipeline's "levels" event:
  // the level of what is ACTUALLY being encoded and sent, which is the one
  // meter that goes quiet when the wrong device is selected.
  //
  // ============= OUTSIDE .pgm-tile, AND THAT IS LOAD-BEARING =================
  //
  // The native SRT overlay is an OPAQUE CHILD WINDOW painted over exactly the
  // tile's rectangle — measurePictureRect measures .pgm-tile and nothing else —
  // and no z-index in this page reaches above it. Anything drawn inside that
  // rectangle is invisible for as long as the overlay is up, which is exactly
  // when a commentator is mid-match and most needs to see their input. So the
  // meters live in .pgm-stage BESIDE the tile: visible over both pictures,
  // never under either, and the measured rectangle is untouched.
  //
  // The bar is the RMS — the loudness a listener would report — and the thin
  // marker riding above it is the peak-hold, ~1.5 s of the highest recent
  // peak. Zone segments are fixed slices of the scale (green to -18, amber to
  // -6, red above, from meters.js via the mixer's own constants) and only the
  // fill inside each moves, for the reason mixer.css documents: a gradient on
  // a moving fill drags its colour stops with the level.
  const metersEl = document.createElement('div');
  metersEl.className = 'input-meters input-meters-idle';
  metersEl.title =
    'Commentary input level, measured on the encoded feed itself. Green to -18 dBFS, amber to -6, red above.';
  const meterChannels = ['L', 'R'].map((name) => {
    const channel = document.createElement('div');
    channel.className = 'input-meter';
    const bar = document.createElement('div');
    bar.className = 'input-meter-bar';
    // Bottom-first zones drawn bottom-up: the bar is a column-reverse flex, so
    // the first (green) segment sits at the bottom without this file doing
    // coordinate arithmetic.
    const fills = meterZones().map(({ zone, from, to }) => {
      const seg = document.createElement('div');
      seg.className = `input-meter-seg input-meter-seg--${zone}`;
      // The segment's share of the bar comes from the dB boundaries, so the
      // paint cannot drift from the scale it claims to show — the same
      // derive-don't-restate rule as the mixer's meterStageWidths.
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

  pgmStage.append(pgmTile, metersEl);

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
  returnSelect.value = String(DEFAULT_RETURN_MID); // mid 4, MIC1 / "Monitor 1"
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

  // THE NOTES UNDER THE CONTROLS ARE GONE, at the operator's request — the
  // paragraph explaining the selected picture (describePictureSource +
  // PICTURE_NOTE + LATENCY_NOTE) and the separate SRT status line under it.
  // The words themselves are untouched in ./picturesource.js and
  // ./returnsource.js, where Settings and the option tooltips still use them;
  // what changed is that this screen no longer renders a paragraph of them.
  // The one state that mattered from the status line — BACKOFF, the picture
  // failing and retrying — now speaks through the error banner instead, once
  // per episode; see setPictureState below and errorlog.js.

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

  // The Headphones label is plain "Headphones/output", at the operator's
  // request. It used to say whose identifier space the list came from — the
  // browser's — which mattered when a control swapped the dropdown between
  // the browser's list and Windows'. That control is gone — this is always
  // the browser's list, always writing config.headphoneDeviceId — so the
  // qualifier was engineering trivia on the operator's screen. The
  // distinction itself still matters and still lives in ./returnsource.js and
  // on the Settings screen's WASAPI field.
  const headphoneRow = document.createElement('div');
  headphoneRow.className = 'control-group';
  const headphoneLabel = document.createElement('label');
  headphoneLabel.htmlFor = 'headphone-select';
  headphoneLabel.textContent = 'Headphones/output';
  headphoneRow.append(headphoneLabel, headphoneSelect);

  controls.append(
    makeRow('Commentary input', 'input-select', inputSelect),
    headphoneRow,
    returnGroup,
    channelGroup,
    sourceGroup,
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

  el.append(
    header,
    errorBanner,
    errorHistoryPanel,
    pgmStage,
    audioEl,
    controls,
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
    // Display order, not arrival order: real microphones above the NDI/webcam
    // virtual sources, the Dante wall last, numbers compared as numbers. The
    // measured machine offers eight "DVS Receive N-M" pairs, and byte order
    // both interleaves 1-2 with 10-11 and files them above the one Focusrite
    // the commentator actually uses. sortDevices copies; the caller's array
    // (app.js's currentInputDevices) is not reordered under it.
    const ordered = sortDevices(devices);
    for (const d of ordered) {
      const opt = document.createElement('option');
      opt.value = d.id;
      opt.textContent = d.name;
      select.appendChild(opt);
    }
    const selection = describeDeviceSelection(ordered, previousValue);
    if (selection.present) {
      if (selection.savedId !== '') select.value = selection.savedId;
      return;
    }
    // The saved id is not in today's list — a docked USB interface, a stopped
    // Dante Virtual Soundcard, a config.json from another machine. This used
    // to fall through silently, leaving device #1 showing as selected: the
    // operator reads a plausible device on screen while Start refuses the id
    // actually saved, and cannot reconcile the two. Show the truth instead —
    // a marker option that cannot be chosen, selected, with the missing id in
    // it. The control is left VISIBLY WRONG on purpose; picking any real
    // device is the way out, and that gesture overwrites the stale id.
    const missing = document.createElement('option');
    missing.value = selection.savedId;
    missing.textContent = selection.label;
    missing.disabled = true;
    select.insertBefore(missing, select.firstChild);
    select.value = selection.savedId;
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
   * badge over the tile and the segmented control.
   *
   * Both feed it, because neither alone says what is on screen: "SRT selected"
   * with the receiver in BACKOFF is a commentator watching the mosaic, and
   * saying "SRT" over the top of that would be a lie told in large letters.
   *
   * The status LINE it also used to draw is gone; BACKOFF — the only state on
   * it that demanded attention — is raised through the error banner by
   * setPictureState instead, and the badge over the tile already reports which
   * picture is actually showing.
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
   * strings on the "picture" event.
   *
   * It also feeds the backoff-episode tracker: the first failure of an
   * unbroken failing run raises the error banner, the retry cycling inside
   * that run stays silent, and recovery takes the banner down again — but only
   * if the banner is still showing THIS message, because a different error
   * arriving mid-episode must not be cleared by the picture getting better.
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

  // Peak-hold state for the input meters. One instance for the view's life:
  // the session-end zero-frame resets it below, so a new session starts with
  // no ghost of the old one's peaks.
  const inputPeakHold = createPeakHold();

  /**
   * setLevels paints the input meters from one "levels" frame
   * ({peak: number[], rms: number[]}, dBFS per channel).
   *
   * Called on every event, ~20 Hz — no rAF loop, deliberately: at that rate
   * the event IS the frame clock, and the peak-hold ticks with it, so a
   * stopped session (which stops the events after its zero-frame) also stops
   * all meter work rather than leaving a timer painting nothing.
   *
   * The bar is the RMS; the marker is the held peak. An all-silence frame —
   * the zero-frame app.go emits when the session ends — or a null/malformed
   * one dims the whole assembly and resets the hold: empty and dimmed, no
   * text clutter, because "no session" is not a level and must not look like
   * one.
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
        // Rounded to 0.1% for the same reason the mixer's meterPercent
        // rounds: a 20 Hz update must not rewrite the style attribute with a
        // seventeen-digit float.
        fill.style.height = `${(Math.round(fills[z] * 1000) / 10).toFixed(1)}%`;
      });
      const frac = dbToFraction(marks[i]);
      ch.peakMark.hidden = !(frac > 0);
      ch.peakMark.style.bottom = `${(Math.round(frac * 1000) / 10).toFixed(1)}%`;
    });
  }

  function setLevel(fraction) {
    levelSlider.value = String(Math.round(Math.max(0, Math.min(1, fraction)) * 100));
  }

  function setRunning(running) {
    startStopBtn.textContent = running ? 'STOP' : 'START';
    startStopBtn.classList.toggle('btn-stop', running);
    startStopBtn.classList.toggle('btn-primary', !running);
    startStopBtn.setAttribute('aria-pressed', String(running));
    // The preset selector is gated on the SAME running state as the button: the
    // server refuses ApplyPreset while SENDING, so switching instance from the
    // header must be disabled with the reason on it rather than left to fail.
    presetSendingNow = running === true;
    renderPresetIndicator();
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
    setRemoteClients,
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
    setLevels,
    setLevel,
    setPresets,
    setActivePreset,
    setRunning,
    setBusy,
    setStatusUnavailable,
    showError,
    clearError,
  };
}
