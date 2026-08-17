import { createLampRow, GLYPH } from './lamps.js';
// The dropdowns' pure logic: display order and the saved-but-missing marker.
// It lives in its own module so `node --test` can drive it without a DOM —
// this file is wiring, devices.test.js is where the behaviour is proved.
import { sortDevices, labelDevices, describeDeviceSelection } from './devices.js';
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
// The severity vocabulary and the judgements about what is worth an operator's
// attention mid-match. This file decides where a message is DRAWN; alerts.js
// decides how loud it is, and records why the switcher-status banner is gone.
import { SEVERITY, normaliseSeverity, describeAttention } from './alerts.js';
// The single indicator's reduction rule. Pure: lamps in, one state out. This
// file feeds it the SAME lamp objects it paints on the row (see wrapLamps), so
// the summary and the detail cannot disagree — there is one derivation of each
// lamp and it happens in app.js, exactly as it did before.
import { deriveOverallStatus, describeOverall, OVERALL_LEVELS } from './overall.js';
// The cough mute's vocabulary: which keys are bound, what they are called on
// screen, and which targets a keystroke means a character rather than a command.
// The STATE MACHINE is not here and must not be — app.js owns it, because the
// mute is a call into Go and this file has no backend knowledge. This file draws
// the readout it is handed and raises the three gestures.
import {
  MUTE_KEY_PUSH,
  MUTE_KEY_LATCH,
  MUTE_MODE,
  DEFAULT_MUTE_MODE,
  describeMuteKey,
  describeMuteMode,
  normaliseMuteMode,
  isTypingTarget,
  isSpaceActivated,
} from './cough.js';
// The CAMERA lamp's name. The lamp's DERIVATION is not here and must not be —
// this file holds no backend knowledge and no state machine — app.js derives it
// from the "signal" event and the saved video source and pushes it into
// lamps.CAMERA, exactly as it does for the four lamps beside it.
import { LAMP_CAMERA } from './videosource.js';
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

// ======================= WHY THERE IS A CAMERA LAMP =========================
//
// It is SECOND, immediately after SENDING, because the row reads outwards from
// this desk: what this position is sending, then what the switcher makes of it.
// CAMERA belongs on our side of that line — it is the only lamp on the row that
// describes what this seat is putting INTO the feed.
//
// It exists because nothing else here can tell. MEASURED: a DeckLink that loses
// its input goes on emitting black frames at full rate for ever, so SENDING
// stays green, all three switcher lamps stay green — the switcher really is
// receiving a healthy, correctly-formatted, correctly-bitrated feed — and the
// audio meters keep moving. Black goes to air with five green lamps above it.
//
// On a slate position, which is every position shipping today, it reads grey
// SLATE. That is not a filler state: it is the at-a-glance answer to "what is
// this seat contributing", which no other lamp on this row gives, and a lamp
// that appeared and disappeared with a setting would be a lamp nobody learns to
// look at. See videosource.js's deriveCameraLamp.
//
// ============= AND WHY TWO OF THEM SAY "SWITCHER" IN THEIR NAME =============
//
// The last three lamps are one fact each about what M2L-X REPORTS RECEIVING,
// read off the switcher's own telemetry socket (lamps.js's deriveStatusLamps).
// Two of them used to be called VIDEO and AUDIO, which was survivable while
// nothing on this screen measured this desk's own video or audio outside a
// session. Both do now: the meters beside the picture are the commentary
// capture's, live from launch, and the CAMERA lamp is the card's, live from
// launch.
//
// So a commentator would sit in front of a MOVING INPUT METER beside a lamp
// reading "AUDIO — NO STATUS" — which is a true statement about a quiet
// telemetry socket and reads, to the person it is in front of, as "this
// application says my microphone is dead". They would go looking for a fault at
// the desk, twenty minutes before kick-off, in a rig that is working. The names
// say whose fact it is, which is the whole cost of the fix and the whole of it.
//
// These strings are the KEYS app.js paints through (home.lamps['SWITCHER
// VIDEO']) and the labels drawn on the pills, so renaming one is one edit here
// and one at its call site; videosource.test.js pins the list and its order.
const LAMP_NAMES = [
  'SENDING',
  LAMP_CAMERA,
  'SWITCHER SEES FEED',
  'SWITCHER VIDEO',
  'SWITCHER AUDIO',
  'MONITOR',
];

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
 *   setPreviewReserved(on)              whether the card's confidence preview
 *                                       box exists in the layout at all
 *   setPreviewCaption(text)             the words drawn inside that box, which
 *                                       the native surface covers when it paints
 *   measurePreviewRect()                that box, in CSS pixels
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
 *   setMuteReadout(readout)             paints the cough mute from ONE object,
 *                                       cough.js's describeMute output. app.js
 *                                       owns the state machine; this file owns
 *                                       no part of whether audio is going out
 *   showError(message, severity)        adds a row to the alert column. Every
 *   showNote(message)                   message is kept with timestamps and
 *   clearError()                        repeat counts (errorlog.js); dismissing
 *   clearErrorIf(message)               one row means "I have seen this".
 *                                       clearErrorIf retires the rows carrying
 *                                       one message, for faults that resolve
 *
 * There is no setStatusUnavailable. The switcher-status banner is withdrawn —
 * see the block above the match bar, and alerts.js for why staleness raises
 * nothing at all.
 *
 * handlers = {
 *   onSettings(), onMixer(), onStartStop(),
 *   onInputChange(deviceId), onHeadphoneChange(deviceId),
 *   onReturnChange(mid), onReturnChannelChange(mode), onPictureSourceChange(src),
 *   onLevelChange(fraction), onPresetChange(id),
 *   onMutePress(), onMuteRelease(), onMuteLatchToggle(),
 * }
 *
 * ================== THE PICTURE'S GEOMETRY IS INDEPENDENT ===================
 *
 * The governing rule of this screen, in the operator's words: "For the
 * comentators watching a live match, anything casuing their video to move is a
 * massive no."
 *
 * So the layout is two columns. .home-main grows and holds the picture and the
 * match bar; .home-rail is a fixed-width flex child with its own scroll and
 * holds the alerts AND the tray. Nothing that can arrive — an alert, ten alerts,
 * a note, a section of settings being read — is in the main column, and the rail
 * cannot change width because its flex-basis is a constant rather than its
 * content. The match bar under the picture is a fixed height for the same
 * reason. The only things that may resize .pgm-tile are the WINDOW changing size
 * and the operator collapsing the column, both of which are that operator's own
 * hand. See main.css and homelayout.test.js.
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
  // The preset indicator is NOT here any more. It moved into the column with the
  // rest of the tray at the operator's request — "The rest can live in some form
  // of settings tray or something like that" — and a preset picker is exactly
  // that: something chosen before kick-off, refused server-side while SENDING,
  // and never touched mid-match. The topbar keeps the two buttons that open the
  // other surfaces, and the column sits directly underneath them.
  headerBtns.append(mixerBtn, settingsBtn);
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

  // --- the alert feed, which lives in the COLUMN and nowhere else ----------
  //
  // ===================== THIS IS NO LONGER A BANNER =========================
  //
  // It was two of them: .error-banner above the picture and
  // .status-unavailable-banner below it, both in normal document flow with
  // margins, so every appearance and every dismissal reflowed the page and
  // shoved the programme picture mid-sentence. The operator, verbatim:
  //
  //   "We should move the errors/alerts to not be banners above and bellow
  //    that cause layout shifts. For the comentators watching a live match,
  //    anything casuing their video to move is a massive no."
  //
  // and, correcting a first attempt that proposed floating overlays instead:
  //
  //   "The solution to not shifting is done make them banners????? We could
  //    look at having a column on the side that has the allerts and all the
  //    settings, so we make more use of vertical space etc"
  //
  // He is right and it is the better design. An overlay is still something that
  // appears; a PERMANENT COLUMN has nothing to shift by construction. This list
  // is drawn inside .home-rail, which is a fixed-width flex child with its own
  // scroll, so the number of rows in it — nought, one, ten — cannot change the
  // width of .home-main and therefore cannot change one pixel of .pgm-tile.
  //
  // The list is ALWAYS RENDERED, including when it is empty: "No alerts" is a
  // row, not an absence. An empty state that collapses is an empty state that
  // moves everything below it when the first alert arrives, and the operator
  // then learns the column by watching it jump.
  //
  // The log behind it (./errorlog.js) keeps every message with timestamps and
  // repeat counts, so a second problem does not destroy the evidence of the
  // first. Dismissing a row means "I have seen this", not "unhappen it": the
  // history stays until it is cleared.
  const errorLog = createErrorLog();
  const backoffEpisode = createBackoffEpisode();

  const alertsRegion = document.createElement('div');
  alertsRegion.className = 'rail-alerts';
  const alertsList = document.createElement('ul');
  alertsList.className = 'alert-list';
  // Polite, not assertive: this region can gain a row while the commentator is
  // mid-sentence, and an assertive live region interrupts a screen reader
  // immediately. The one place that IS urgent is the mute readout, which has
  // its own.
  alertsList.setAttribute('aria-live', 'polite');
  alertsList.setAttribute('aria-label', 'Alerts');
  const alertsEmpty = document.createElement('li');
  alertsEmpty.className = 'alert-empty';
  alertsEmpty.textContent = 'No alerts';
  const alertsClear = document.createElement('button');
  alertsClear.type = 'button';
  alertsClear.className = 'alert-clear';
  alertsClear.textContent = 'Clear all';
  alertsClear.hidden = true;
  alertsClear.addEventListener('click', () => {
    errorLog.clear();
    renderAlerts();
  });
  alertsRegion.append(alertsList, alertsClear);

  // The header's attention marker. It is the ONE thing that has to be legible
  // when the column is collapsed, so it is rendered into both places from the
  // same call.
  const alertsCount = document.createElement('span');
  alertsCount.className = 'rail-attention';
  const railStripCount = document.createElement('span');
  railStripCount.className = 'rail-strip-attention';

  function renderAlerts() {
    const entries = errorLog.entries;
    const attention = describeAttention(entries);

    for (const el2 of [alertsCount, railStripCount]) {
      el2.textContent = attention.attention ? String(attention.alerts) : '';
      el2.hidden = !attention.attention;
      el2.title = attention.label;
    }
    alertsCount.setAttribute('aria-label', attention.label);

    alertsList.textContent = '';
    if (entries.length === 0) {
      alertsList.appendChild(alertsEmpty);
      alertsClear.hidden = true;
      return;
    }
    alertsClear.hidden = false;

    for (const entry of entries) {
      const li = document.createElement('li');
      li.className = `alert-row alert-row--${entry.severity}`;
      const when = document.createElement('span');
      when.className = 'alert-when';
      when.textContent = formatErrorTime(entry.lastAt);
      const text = document.createElement('span');
      text.className = 'alert-text';
      text.textContent = entry.count > 1 ? `${entry.message} (×${entry.count})` : entry.message;
      // describeEntry is the one-line form, kept as the hover text so the row's
      // full sentence is reachable when the column has ellipsised it, together
      // with when the run started.
      li.title =
        entry.count > 1
          ? `${describeEntry(entry)}\nfirst at ${formatErrorTime(entry.firstAt)}, ${entry.count} times in all`
          : describeEntry(entry);
      const dismiss = document.createElement('button');
      dismiss.type = 'button';
      dismiss.className = 'alert-dismiss';
      dismiss.setAttribute('aria-label', `Dismiss: ${entry.message}`);
      dismiss.textContent = '✕';
      // BY IDENTITY, not by index: the list re-renders on every arrival, so an
      // index captured when this row was drawn can point at a different row by
      // the time it is clicked. See errorlog.js's dismiss().
      dismiss.addEventListener('click', () => {
        errorLog.dismiss(entry);
        renderAlerts();
      });
      li.append(when, text, dismiss);
      alertsList.appendChild(li);
    }
  }

  /**
   * showError records a message and draws it in the column.
   *
   * The second argument is the SEVERITY (alerts.js). It defaults to ALERT
   * because a caller that forgets to classify should over-report; NOTE is for
   * the things that explain rather than warn, and the only one today is the
   * deferred output-device switch.
   */
  function showError(message, severity) {
    errorLog.record(message, normaliseSeverity(severity));
    renderAlerts();
  }

  /** showNote is showError at NOTE severity, named so call sites read. */
  function showNote(message) {
    showError(message, SEVERITY.NOTE);
  }

  /** clearError empties the whole feed. The column's "Clear all". */
  function clearError() {
    errorLog.clear();
    renderAlerts();
  }

  /**
   * clearErrorIf takes down every row carrying `message` and nothing else.
   *
   * For errors that RESOLVE — the SRT picture coming back — where clearing
   * unconditionally would eat whatever unrelated error arrived in between. This
   * used to be possible only for the single newest message, because there was
   * one banner and it showed one line; a list can retire the right row wherever
   * it has got to.
   */
  function clearErrorIf(message) {
    if (errorLog.dismissMatching(message) > 0) renderAlerts();
  }

  renderAlerts();

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
  // A slim vertical stereo pair fed from the "levels" event: the level of the
  // commentary as it leaves the capture pipeline, measured immediately before
  // the queue that feeds the encoder. It is the one meter that goes quiet when
  // the wrong device is selected, and it is downstream of the cough mute, so a
  // muted commentator has a flat meter as well as a mute banner.
  //
  // ================= THEY ARE LIVE FROM LAUNCH, NOT FROM START ===============
  //
  // They used to be the SEND pipeline's own measurement, so they began at START
  // and fell to silence at STOP. Capture is a pipeline of its own now, built at
  // launch and held until the application quits, so the meters move while the
  // operator is still setting up and go on moving after STOP. That is the point
  // of the change — a commentator finds out that their microphone is dead while
  // there is still time to fix it — and it is why the note under them exists.
  //
  // ONE PROMISE IS WEAKER THAN IT WAS, and it is written down here because it
  // cannot be seen from the screen: what this meter shows is what reaches the
  // encoder IN NORMAL OPERATION, and not during a send-side stall. The queue in
  // front of the proxysink is leaky=downstream, deliberately (a non-leaky one
  // was measured dragging the preview to 7.2 fps and the meters to 7.2 msg/s and
  // making the card itself drop frames), so a stall of more than about a second
  // DROPS that second of commentary rather than delaying it. The meter can move
  // over audio the far end never receives. A stall that long is already a
  // reconnect-class event and the SENDING lamp is the thing that says so.
  //
  // ============= OUTSIDE .pgm-tile, AND THAT IS LOAD-BEARING =================
  //
  // The native SRT overlay is an OPAQUE CHILD WINDOW painted over exactly the
  // tile's rectangle — measurePictureRect measures .pgm-tile and nothing else —
  // and no z-index in this page reaches above it. Anything drawn inside that
  // rectangle is invisible for as long as the overlay is up, which is exactly
  // when a commentator is mid-match and most needs to see their input. So the
  // meters are never a child of the tile: visible over both pictures, never
  // under either, and the measured rectangle is untouched.
  //
  // THEY NOW LIVE IN THE COLUMN, not beside the tile in .pgm-stage. The operator
  // asked for a main area holding the picture, one overall indicator and the
  // cough controls, with everything else in the tray: "During the match we only
  // really need an overall status, single green/red indicator and the cough mute
  // buttons. The rest can live in some form of settings tray or something like
  // that." The column is outside .pgm-tile exactly as .pgm-stage was, so the
  // reason above is satisfied by the new home as well as by the old one.
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
    'Commentary input level, measured where the capture pipeline hands the audio to the encoder — ' +
    'live from launch, whether or not you are sending. Green to -18 dBFS, amber to -6, red above.';
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

  // The line beside the meters, and it is not a decoration: on a CoreAudio seat
  // the operating system's microphone indicator — the orange dot in the menu bar
  // — is now lit from launch to quit rather than only while sending, because the
  // input really is open the whole time. A commentator who has learnt to read
  // that dot as "I am live" would read it wrong on every seat, all day, and no
  // lamp in this application would contradict them.
  //
  // It is STATIC. It says the same words in every state, so it can never appear,
  // change or clear — the column's rule — and it names the one control that does
  // answer the question it is about.
  const metersNote = document.createElement('p');
  metersNote.className = 'input-meters-note';
  metersNote.textContent =
    'Open from launch: the input is live, and its recording light is on, before anything is sent. ' +
    'The SENDING lamp is what says you are on air.';

  // --- the card's confidence preview, at the right edge --------------------
  //
  // A SECOND RESERVED RECTANGLE, and the same mechanism as the first: the
  // preview is decoded and drawn in Go and painted by a NATIVE CHILD WINDOW
  // over this page, because the frames never leave that process and a <video>
  // element cannot be handed a GStreamer sink. So this file's whole job is
  // identical to its job for the SRT picture — reserve a box, expose a way to
  // measure it — and app.js reports it through a createOverlay of its own.
  //
  // ================ IT IS OUTSIDE .pgm-tile, AND THAT IS LOAD-BEARING ========
  //
  // Two opaque native windows must not be told to occupy overlapping
  // rectangles: whichever is on top simply erases the other, and neither the
  // page nor Go would report anything wrong. .pgm-tile is measured exactly by
  // measurePictureRect, so this sits BESIDE it in .pgm-stage — the same
  // reasoning, and the same place, as the input meters above.
  //
  // ================ THE CAPTION NEEDS NO VISIBILITY FLAG =====================
  //
  // It is drawn INSIDE the reserved box and is never hidden by this file. The
  // native surface is opaque and on top, so the caption is visible exactly when
  // there is no picture over it — which is the one thing this page genuinely
  // cannot learn from Go: the preview branch is built with the picture capture
  // at launch, the build retries without it if the surface will not attach, and
  // no event reports the branch itself. A box showing a caption is a box
  // explaining itself; a box showing a picture needs no caption. app.js supplies
  // the words (videosource.js's describePreviewBox, from the capture state).
  const previewTile = document.createElement('div');
  previewTile.className = 'preview-tile';
  // HIDDEN UNTIL SOMETHING SAYS OTHERWISE, so that a seat which has never
  // turned the preview on — which is every seat today — has the main screen it
  // has always had, to the pixel. An empty reserved box would also move
  // .pgm-tile, which is the commentator's picture.
  previewTile.hidden = true;
  const previewCaption = document.createElement('p');
  previewCaption.className = 'preview-caption';
  previewTile.appendChild(previewCaption);

  // The stage holds the two PICTURES and nothing else. The meters moved to the
  // column (see above); everything that is not a picture is either in the match
  // bar under the stage or in the column beside it.
  pgmStage.append(pgmTile, previewTile);

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

  // The AUDIO controls only. sourceGroup — which PICTURE — gets its own block in
  // the column, because it is not an audio control and grouping it with four of
  // them is how a commentator came to believe the old "Return Source" selector
  // changed what they could see rather than what they could hear.
  controls.append(
    makeRow('Commentary input', 'input-select', inputSelect),
    headphoneRow,
    returnGroup,
    channelGroup,
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

  // ======================= THE STATUS BANNER IS GONE =========================
  //
  // .status-unavailable-banner — "STATUS UNAVAILABLE — the switcher status feed
  // has been silent for over 15 seconds" — is deleted, not re-homed. The
  // operator: "the orrange status banner at the bottom about switcher status is
  // VERY annoying and keeps cauing layout shifts and causing concerns when
  // everything is fine."
  //
  // Two faults, and the column only fixes one. The second is that it was a
  // FOURTH copy of a fact the lamp row already states three times: staleness
  // greys SWITCHER SEES FEED, VIDEO and AUDIO and writes STATUS UNAVAILABLE
  // across all three, in glyph, text and colour. The overall indicator folds the
  // same fact in and can never read GOOD over it. And it never meant what it
  // looked like it meant: the telemetry WebSocket going quiet says nothing about
  // the contribution feed, which is a different socket to a different port and
  // has its own lamp. See alerts.js, which records the decision and why.
  //
  // setStatusUnavailable is withdrawn with it, so app.js cannot call a setter
  // that silently does nothing.

  // --- the match bar: one indicator and the cough controls ------------------
  //
  // Under the picture, in the MAIN area, and the only two things allowed there.
  //
  // ITS HEIGHT IS A CONSTANT, and that is load-bearing. .pgm-tile is sized from
  // the height left in .pgm-stage, so anything under the stage that can grow a
  // line moves the picture — which is the whole defect this work exists to
  // remove, reintroduced by a control instead of by a banner. So the bar is a
  // fixed height in the stylesheet, every text inside it is a single
  // non-wrapping line, and the long forms live in title attributes. See
  // main.css's .match-bar and homelayout.test.js, which asserts it.
  const matchBar = document.createElement('div');
  matchBar.className = 'match-bar';

  // ----- the one overall indicator -----
  const overallEl = document.createElement('div');
  overallEl.className = 'overall';
  overallEl.setAttribute('role', 'status');
  const overallGlyph = document.createElement('span');
  overallGlyph.className = 'overall-glyph';
  overallGlyph.setAttribute('aria-hidden', 'true');
  const overallWords = document.createElement('span');
  overallWords.className = 'overall-words';
  const overallText = document.createElement('span');
  overallText.className = 'overall-text';
  const overallDetail = document.createElement('span');
  overallDetail.className = 'overall-detail';
  overallWords.append(overallText, overallDetail);
  overallEl.append(overallGlyph, overallWords);

  // ----- the cough mute -----
  //
  // The control whose state being misread puts a cough on air or leaves a
  // commentator talking into a dead microphone. Everything about it is drawn
  // from ONE readout object (cough.js's describeMute) handed in by app.js, so
  // the words, the colour and the pressed states cannot disagree about whether
  // audio is going out.
  //
  // NOTHING HERE MUTES ANYTHING. The three gestures raise handlers; app.js calls
  // the Go binding that mutes the SEND path. Muting a monitor element here would
  // make this desk quieter while the cough went to air.
  const coughEl = document.createElement('div');
  coughEl.className = 'cough';

  const coughReadout = document.createElement('div');
  coughReadout.className = 'cough-readout';
  // Assertive, and it is the only assertive region on the screen: this is the
  // one state change a commentator must not miss, and it is the one they cannot
  // check by listening.
  coughReadout.setAttribute('role', 'status');
  coughReadout.setAttribute('aria-live', 'assertive');
  const coughState = document.createElement('span');
  coughState.className = 'cough-state';
  const coughReason = document.createElement('span');
  coughReason.className = 'cough-reason';
  coughReadout.append(coughState, coughReason);

  const pushBtn = document.createElement('button');
  pushBtn.type = 'button';
  pushBtn.className = 'btn cough-btn cough-push';
  const latchBtn = document.createElement('button');
  latchBtn.type = 'button';
  latchBtn.className = 'btn cough-btn cough-latch';

  /**
   * keyCap builds the printed key legend on a button. The bound key has to be
   * OBVIOUS — this control exists for the moment when looking at the screen is
   * what the operator cannot do, and a shortcut nobody can see is a shortcut
   * nobody uses — so it is drawn on the button, not hidden in a tooltip.
   */
  function keyCap(label, code) {
    const wrap = document.createElement('span');
    wrap.className = 'cough-btn-label';
    const text = document.createElement('span');
    text.textContent = label;
    const cap = document.createElement('kbd');
    cap.className = 'keycap';
    cap.textContent = describeMuteKey(code);
    wrap.append(text, cap);
    return wrap;
  }
  pushBtn.append(keyCap('PUSH TO MUTE', MUTE_KEY_PUSH));
  latchBtn.append(keyCap('LATCH MUTE', MUTE_KEY_LATCH));

  // PUSH is pointer-held, and every way of losing the release is covered:
  // pointerup, pointercancel, pointerleave, and the window-level blur and
  // visibilitychange below. A hold whose release is never seen is a dead
  // microphone for the rest of the match.
  pushBtn.addEventListener('pointerdown', (ev) => {
    // Keep the pointer's events coming to this element even if it slides off,
    // where the runtime supports it; the leave/cancel handlers are the belt to
    // that brace, not a substitute for it.
    if (typeof pushBtn.setPointerCapture === 'function' && ev.pointerId !== undefined) {
      try {
        pushBtn.setPointerCapture(ev.pointerId);
      } catch {
        /* not supported; the leave handler still covers it */
      }
    }
    handlers.onMutePress();
  });
  for (const type of ['pointerup', 'pointercancel', 'pointerleave']) {
    pushBtn.addEventListener(type, () => handlers.onMuteRelease());
  }
  latchBtn.addEventListener('click', () => handlers.onMuteLatchToggle());

  coughEl.append(coughReadout, pushBtn, latchBtn);
  matchBar.append(overallEl, coughEl);

  // ----- the keyboard bindings -----
  //
  // Document-level and capturing, because the operator's hands are not
  // guaranteed to be anywhere near this button, and because the default action
  // has to be suppressed: Space activates whatever is focused and scrolls.
  // isTypingTarget keeps a passphrase field in Settings from muting the
  // commentary on every word.
  //
  // repeat is ignored: holding a key fires keydown at the platform's repeat
  // rate, and each one would re-issue a mute that is already held.
  //
  // pushKeyHeld records whether THIS binding owns the current Space press. It
  // decides one thing only: whether the keyup may be cancelled. A <button> is
  // activated by Space on the KEYUP, so cancelling that edge unconditionally is
  // what silenced every button in the app — see isSpaceActivated.
  let pushKeyHeld = false;
  function onKeyDown(ev) {
    if (ev.repeat || ev.altKey || ev.ctrlKey || ev.metaKey) return;
    if (isTypingTarget(ev.target)) return;
    if (ev.code === MUTE_KEY_PUSH) {
      // The focused control already answers to Space: it keeps it. PUSH TO MUTE
      // is the exception, because its activation is this mute, and because a
      // click gives one event where a hold needs a press and a release.
      if (ev.target !== pushBtn && isSpaceActivated(ev.target)) return;
      ev.preventDefault();
      pushKeyHeld = true;
      handlers.onMutePress();
      return;
    }
    if (ev.code === MUTE_KEY_LATCH) {
      ev.preventDefault();
      handlers.onMuteLatchToggle();
    }
  }
  function onKeyUp(ev) {
    if (ev.code !== MUTE_KEY_PUSH) return;
    // The RELEASE is not gated on where the key came up: a keydown that started
    // outside a field and a keyup delivered inside one must still release, and
    // release() is a no-op when nothing is held and never touches the latch. The
    // release path is deliberately harder to block than the press path.
    //
    // The DEFAULT is cancelled only when the press was ours, so a Space that
    // went to a focused button still activates it on this edge.
    if (pushKeyHeld) ev.preventDefault();
    pushKeyHeld = false;
    handlers.onMuteRelease();
  }
  document.addEventListener('keydown', onKeyDown, { capture: true });
  document.addEventListener('keyup', onKeyUp, { capture: true });
  // The window losing focus, or the page being hidden, means the keyup may be
  // delivered somewhere else and never arrive. Release rather than hold: the
  // latch is untouched by both, because a latch is a deliberate choice and
  // dropping it would put a live microphone up without anybody asking.
  // Both also drop pushKeyHeld: the keyup for this press is now never coming, so
  // leaving the flag set would arm a preventDefault on some later, unrelated
  // Space — which is the button-stealing bug arriving by the back door.
  if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
    window.addEventListener('blur', () => {
      pushKeyHeld = false;
      handlers.onMuteRelease();
    });
  }
  if (typeof document.addEventListener === 'function') {
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') {
        pushKeyHeld = false;
        handlers.onMuteRelease();
      }
    });
  }

  // --- the column ----------------------------------------------------------
  //
  // Fixed width, permanently present, its own scroll. It carries the alerts and
  // everything the main area no longer does: START/STOP and the preset picker,
  // the six lamps, the input meters, the device and return controls, the picture
  // selector.
  //
  // WHY THE RIGHT-HAND SIDE.
  //
  //   - DOM order is reading order. The picture and the mute controls come
  //     first, for a screen reader and for a keyboard, and the tray after them.
  //     A left-hand column reverses that on the one screen where the urgent
  //     control must be reached first.
  //   - The two buttons that used to lead to configuration — Mixer and Settings
  //     — are already at the top RIGHT of the topbar. The tray belongs under the
  //     things that used to open it, not across the screen from them.
  //   - The picture is centred in what is left either way, so neither side is
  //     better for the picture; this is decided on where the operator's hand and
  //     eye already are.
  //
  // WHY IT IS FIXED-WIDTH AND NOT INTRINSIC. `flex: 0 0 var(--rail-w)` with the
  // basis a constant. An `auto`-width column would be sized by its CONTENT, so
  // an alert with a long sentence in it would widen the column, narrow the main
  // area and move the picture — the original bug, rebuilt sideways. That is what
  // homelayout.test.js pins, and it is why the alert text is allowed to wrap and
  // to break inside a word rather than to push.
  const rail = document.createElement('aside');
  rail.className = 'home-rail';
  rail.setAttribute('aria-label', 'Alerts and settings');

  const railHeader = document.createElement('div');
  railHeader.className = 'rail-header';
  const railTitle = document.createElement('span');
  railTitle.className = 'rail-title';
  railTitle.textContent = 'ALERTS & SETTINGS';
  const railCollapse = document.createElement('button');
  railCollapse.type = 'button';
  railCollapse.className = 'btn btn-ghost rail-collapse';
  railHeader.append(railTitle, alertsCount, railCollapse);

  // The collapsed strip. It is a THIRD fixed width, never zero: an alert that
  // cannot be seen because the operator folded the column away is an alert that
  // did not happen, so the strip keeps the attention count and the way back.
  const railStrip = document.createElement('div');
  railStrip.className = 'rail-strip';
  const railExpand = document.createElement('button');
  railExpand.type = 'button';
  railExpand.className = 'btn btn-ghost rail-expand';
  railExpand.textContent = '‹';
  railExpand.title = 'Show alerts and settings';
  railExpand.setAttribute('aria-label', 'Show alerts and settings');
  const railStripLabel = document.createElement('span');
  railStripLabel.className = 'rail-strip-label';
  railStripLabel.textContent = 'ALERTS';
  railStrip.append(railExpand, railStripCount, railStripLabel);

  /**
   * makeRailSection is one labelled block in the column.
   *
   * The column is scrolled and read at leisure, unlike the main area, so it can
   * afford headings — and it needs them: six lamps, two meters, five controls
   * and a preset picker with no structure is a list nobody finds anything in.
   */
  function makeRailSection(titleText, ...children) {
    const section = document.createElement('section');
    section.className = 'rail-section';
    const h = document.createElement('h2');
    h.className = 'rail-section-title';
    h.textContent = titleText;
    section.append(h, ...children);
    return section;
  }

  // The MODE selector: which of the two cough behaviours is primary. It is in
  // the column, not in Settings, because it is a match-time preference chosen
  // where the operator is sitting when they choose it — and not in the match
  // bar, because the main area holds the picture, one indicator and the mute
  // controls themselves and nothing else. Both behaviours stay reachable
  // whichever is chosen; see cough.js's MUTE_MODE for why that is not a fudge.
  const coughModeSegmented = makeSegmented(
    'cough-mute-mode',
    [
      {
        value: MUTE_MODE.PUSH,
        label: 'Push',
        hint: `Hold ${describeMuteKey(MUTE_KEY_PUSH)} or the button to mute; release to go live.`,
      },
      {
        value: MUTE_MODE.LATCH,
        label: 'Latch',
        hint: `Press ${describeMuteKey(MUTE_KEY_LATCH)} or the button to mute until pressed again.`,
      },
    ],
    (mode) => handlers.onCoughModeChange(mode),
  );
  coughModeSegmented.set(DEFAULT_MUTE_MODE);

  const coughModeGroup = document.createElement('div');
  coughModeGroup.className = 'control-group control-group-cough-mode';
  const coughModeLabel = document.createElement('span');
  coughModeLabel.className = 'control-label';
  coughModeLabel.textContent = 'Primary cough control';
  coughModeGroup.append(coughModeLabel, coughModeSegmented.el);

  rail.append(
    railHeader,
    alertsRegion,
    makeRailSection('Cough mute', coughModeGroup),
    makeRailSection('Session', actionRow, presetIndicator),
    makeRailSection('Status', lampsEl, metersEl, metersNote),
    makeRailSection('Audio', controls),
    makeRailSection('Picture', sourceGroup),
    railStrip,
  );

  // --- collapsing, which is an OPERATOR ACTION and only that ---------------
  //
  // Collapsing narrows the column to the strip and gives the width to the
  // picture, so it DOES move the picture. That is allowed and nothing else is:
  // an operator who asks for a bigger picture is choosing to resize it, which is
  // categorically different from an alert resizing it for them. Nothing in this
  // file, and nothing app.js can call, collapses or expands the column — there
  // is no setter for it on the returned view, deliberately, so no event can
  // reach it. The only writers are these two buttons.
  //
  // The class goes on the VIEW, not on the column, because the main area's width
  // is what actually changes and CSS has no parent selector.
  let railCollapsed = false;
  function renderRail() {
    el.classList.toggle('home-rail-collapsed', railCollapsed);
    rail.setAttribute('aria-expanded', railCollapsed ? 'false' : 'true');
    railCollapse.textContent = '›';
    railCollapse.title = 'Hide alerts and settings (the picture gets the space)';
    railCollapse.setAttribute('aria-label', 'Hide alerts and settings');
    // app.js re-measures the native overlays off a ResizeObserver on the picture
    // element, so nothing has to be told: .pgm-tile changing size IS the event.
  }
  railCollapse.addEventListener('click', () => {
    railCollapsed = true;
    renderRail();
  });
  railExpand.addEventListener('click', () => {
    railCollapsed = false;
    renderRail();
  });
  renderRail();

  // --- the two-column body -------------------------------------------------
  //
  // .home-main grows and .home-rail does not. min-width: 0 on the main column is
  // what stops a wide child (a long device name in a <select>) from forcing the
  // flex line wider than the window and pushing the column off the edge.
  const homeMain = document.createElement('div');
  homeMain.className = 'home-main';
  homeMain.append(pgmStage, matchBar);

  const homeBody = document.createElement('div');
  homeBody.className = 'home-body';
  homeBody.append(homeMain, rail);

  el.append(header, homeBody, audioEl);

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
    //
    // labelDevices then adds the DISAMBIGUATING SUFFIX, and it has to run after
    // the sort because what it adds depends on which entries share a name. One
    // Blackmagic card enumerates TWICE — once through the platform's audio
    // stack and once through GStreamer's decklink provider — under names an
    // operator cannot tell apart, and the native twin measures -96 dBFS on all
    // sixteen channels with the mic live. Rendering d.name here would put those
    // two on adjacent lines as equals, which is exactly the choice the operator
    // cannot be asked to make. labelDevices copies too, so the caller's array
    // is still untouched, and .id is unchanged on every entry — the option
    // VALUE and describeDeviceSelection below both still read the real id.
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

  /**
   * setPreviewReserved decides whether the preview box exists in the layout at
   * all. app.js is the only caller and the only thing that knows the answer: it
   * takes the saved video source, the saved preview flag and whether this build
   * can position the surface.
   *
   * Reserving is a LAYOUT change — .pgm-tile is sized against what is left in
   * .pgm-stage — so app.js re-syncs both overlays after calling it. That is not
   * this file's business; it neither knows nor may know that either window
   * exists.
   */
  function setPreviewReserved(reserved) {
    previewTile.hidden = reserved !== true;
  }

  /** setPreviewCaption writes the words drawn inside the reserved box. */
  function setPreviewCaption(text) {
    previewCaption.textContent = typeof text === 'string' ? text : '';
  }

  /**
   * measurePreviewRect reports the preview box in CSS pixels, relative to the
   * viewport — the WebView client area — exactly as measurePictureRect does.
   *
   * Deliberately raw: no rounding, no scaling, no opinion. ./overlay.js is the
   * only module on this side allowed one, and there is one conversion rule in
   * this application rather than two. Null when there is no box to measure,
   * which is what a hidden preview and a hidden view both look like — and null
   * means "do not report", which leaves the surface where it was rather than
   * moving it to a corner and shrinking it to nothing.
   *
   * @returns {{x: number, y: number, width: number, height: number}|null}
   */
  function measurePreviewRect() {
    if (previewTile.hidden) return null;
    if (typeof previewTile.getBoundingClientRect !== 'function') return null;
    const r = previewTile.getBoundingClientRect();
    if (!r || !(r.width > 0) || !(r.height > 0)) return null;
    return { x: r.left, y: r.top, width: r.width, height: r.height };
  }

  // Draw the note and the badge once at construction so neither is blank before
  // any config has loaded.
  renderPicture();

  // Peak-hold state for the input meters. One instance for the view's life:
  // the zero-frame emitted when capture goes down resets it below, so the next
  // device comes up with no ghost of the last one's peaks.
  const inputPeakHold = createPeakHold();

  /**
   * setLevels paints the input meters from one "levels" frame
   * ({peak: number[], rms: number[]}, dBFS per channel).
   *
   * Called on every event, ~20 Hz — no rAF loop, deliberately: at that rate
   * the event IS the frame clock, and the peak-hold ticks with it, so capture
   * going down (which stops the events after its zero-frame) also stops all
   * meter work rather than leaving a timer painting nothing.
   *
   * The bar is the RMS; the marker is the held peak. An all-silence frame — the
   * zero-frame app.go emits when a capture pipeline goes away, which is a device
   * change, a restart or the application quitting and NOT the end of a session —
   * or a null/malformed one dims the whole assembly and resets the hold: empty
   * and dimmed, no text clutter, because "nothing is arriving" is not a level
   * and must not look like one.
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
    // And the overall indicator, from the SAME state: "STANDBY" and "the button
    // says START" are the same fact, and they arrive here together so they
    // cannot drift apart. See overall.js, case 1.
    overallRunning = running === true;
    renderOverall();
  }

  function setBusy(busy) {
    startStopBtn.disabled = busy;
  }

  // --- the one overall indicator -------------------------------------------
  //
  // ===================== IT IS NOT A SEVENTH DERIVATION ======================
  //
  // Every lamp on the row is derived in app.js from backend state, exactly as it
  // was before this screen had a summary. This file does not re-derive any of
  // them and holds no backend knowledge, per its header. What it does is
  // REMEMBER what it was told to paint, and reduce that with overall.js's pure
  // rule. So the indicator is a function of the six values the operator can also
  // read on the row two feet away, and the two cannot disagree — which is the
  // only way a summary is worth having.
  const lampState = {};
  for (const name of LAMP_NAMES) lampState[name] = { level: 'grey', text: 'NOT STARTED' };

  let overallRunning = false;

  function renderOverall() {
    const overall = deriveOverallStatus(
      LAMP_NAMES.map((name) => ({ name, lamp: lampState[name] })),
      { running: overallRunning },
    );
    for (const level of Object.values(OVERALL_LEVELS)) {
      overallEl.classList.toggle(`overall-${level}`, level === overall.level);
    }
    overallGlyph.textContent = GLYPH[overall.level];
    overallText.textContent = overall.text;
    // The reason, on ONE line that is allowed to ellipsise. A detail that could
    // wrap to two lines would change the match bar's height, and the match bar's
    // height is what .pgm-tile is sized against. The full sentence is on the
    // title, and the six lamps it came from are in the column.
    overallDetail.textContent = overall.detail;
    const described = describeOverall(overall);
    overallEl.title = described;
    overallEl.setAttribute('aria-label', `Overall status: ${described}`);
  }

  // The lamps handed to app.js are WRAPPERS: they paint the row exactly as
  // before and additionally record what they were told, so renderOverall has
  // something to reduce. The shape is unchanged ({el, update}), so app.js's
  // existing calls — home.lamps.MONITOR.update(...) and the rest — are untouched.
  const wrappedLamps = {};
  for (const name of LAMP_NAMES) {
    const lamp = lamps[name];
    wrappedLamps[name] = {
      el: lamp.el,
      update(value) {
        lampState[name] = value || { level: 'grey', text: '' };
        lamp.update(value);
        renderOverall();
      },
    };
  }

  renderOverall();

  // --- the cough mute readout ----------------------------------------------
  //
  // ONE object in, everything on the screen out. app.js owns the state machine
  // (cough.js) and hands its describeMute() output here; there is no second
  // place in this file where "muted" is decided, and no branch that can leave
  // the buttons saying one thing and the readout another.
  function setMuteReadout(readout) {
    const r = readout || {};
    const muted = r.muted === true;

    coughState.textContent = r.text || '';
    // The reason is drawn only when there is one, and it is a SHAPE as well as
    // words: "MUTED · LATCHED" reads differently at a glance from "MUTED ·
    // HOLDING", and only the first survives letting go of the key.
    coughReason.textContent = r.reason ? `· ${r.reason}` : '';
    coughReadout.title = r.detail || '';

    for (const state of ['live', 'muted', 'muting', 'unmuting', 'failed', 'unavailable']) {
      coughEl.classList.toggle(`cough--${state}`, r.state === state);
    }

    // WHICH BEHAVIOUR IS PRIMARY. Drawn from the readout's own `mode`, so the
    // emphasis on the buttons and the "PUSH MODE" line under the state come from
    // one value. The order is CSS, not DOM: the tab order and the screen
    // reader's order stay push-then-latch whatever the preference, because a
    // control's identity moving with a setting is how a key gets pressed by
    // muscle memory and does the other thing.
    const mode = normaliseMuteMode(r.mode);
    coughEl.classList.toggle('cough--mode-latch', mode === MUTE_MODE.LATCH);
    pushBtn.classList.toggle('cough-btn-primary', mode === MUTE_MODE.PUSH);
    latchBtn.classList.toggle('cough-btn-primary', mode === MUTE_MODE.LATCH);

    // ================== THE MUTED STATE IS UNMISSABLE =======================
    //
    // A class on the VIEW, so the treatment is not confined to a control in the
    // corner: main.css draws a heavy red OUTLINE around the programme picture
    // while the commentary is muted. An outline, specifically — it is painted
    // outside the border box, so it costs no layout at all and it falls OUTSIDE
    // the rectangle the native SRT overlay covers, which means it is visible
    // over the good picture as well as over the mosaic. A border or an inset
    // shadow would fail both of those tests.
    //
    // Colour is not the only signal: the readout says the word MUTED, names
    // which control is holding it, and the outline is a shape change around the
    // one thing the commentator is already looking at.
    el.classList.toggle('is-muted', muted);

    pushBtn.setAttribute('aria-pressed', String(r.held === true));
    latchBtn.setAttribute('aria-pressed', String(r.latched === true));
    pushBtn.classList.toggle('cough-btn-on', r.held === true);
    latchBtn.classList.toggle('cough-btn-on', r.latched === true);

    // A build without the binding must not offer a mute that silently does
    // nothing — that is worse than having no button at all, because the operator
    // would trust it. Disabled, with the reason on the control.
    const unavailable = r.available === false;
    pushBtn.disabled = unavailable;
    latchBtn.disabled = unavailable;
    if (unavailable) {
      pushBtn.title = r.detail || '';
      latchBtn.title = r.detail || '';
    } else {
      pushBtn.title = `Hold to mute the commentary at the send path. Bound to ${describeMuteKey(MUTE_KEY_PUSH)}.`;
      latchBtn.title = `Mute until pressed again. Bound to ${describeMuteKey(MUTE_KEY_LATCH)}.`;
    }
  }

  /**
   * setCoughMode selects the saved preference on the column's control. It does
   * NOT change the readout — the readout's `mode` comes from the model, which
   * app.js has already been told — so there is one owner of the value and this
   * is only the picker agreeing with it.
   */
  function setCoughMode(mode) {
    coughModeSegmented.set(normaliseMuteMode(mode));
  }

  // Drawn once at construction so the control is never blank, and so the first
  // thing it says is the truth: nothing is muted until something mutes it. The
  // mode shown is the documented default until a configuration says otherwise.
  setMuteReadout({
    state: 'live',
    text: 'LIVE',
    muted: false,
    reason: `${describeMuteMode(DEFAULT_MUTE_MODE)} MODE`,
    held: false,
    latched: false,
    mode: DEFAULT_MUTE_MODE,
  });

  return {
    el,
    videoEl,
    audioEl,
    // The element whose box the native overlay is told to occupy. app.js
    // observes it; nothing here knows what it is for.
    pictureEl: pgmTile,
    // The SECOND such element, for the card's confidence preview. Two boxes,
    // two native windows, one mechanism — and they are separate elements
    // precisely so the two rectangles can never overlap, which would erase one
    // window with the other and report nothing.
    previewEl: previewTile,
    // The WRAPPED lamps: same {el, update} shape, same paint, and each update
    // also feeds the one overall indicator. See renderOverall.
    lamps: wrappedLamps,
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
    setPreviewReserved,
    setPreviewCaption,
    measurePreviewRect,
    setLevels,
    setLevel,
    setPresets,
    setActivePreset,
    setRunning,
    setBusy,
    setMuteReadout,
    setCoughMode,
    showError,
    showNote,
    clearError,
    // Exposed for the faults that RESOLVE. The picture receiver's backoff has
    // used it inside this file since the column was built; the capture faults
    // app.js raises are the second family — a card that failed to open at launch
    // and opened on a restart must not leave a row saying it did not, and
    // clearing the whole feed to retire one row would eat everything else the
    // operator has not read yet.
    clearErrorIf,
  };
}
