import { createLampRow, GLYPH } from './lamps.js';
// The dropdowns' pure logic: display order and the saved-but-missing marker.
// It lives in its own module so `node --test` can drive it without a DOM —
// this file is wiring, devices.test.js is where the behaviour is proved.
import { createPgmPanel, makeSegmented, fillDeviceSelect } from './pgmpanel.js';
import { createErrorLog, describeEntry, formatErrorTime } from './errorlog.js';
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
  coughMuteKeyDown,
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
// The channel table comes from the monitor module because that is where it is
// ENFORCED — it is the wiring of a ChannelSplitter to a ChannelMerger, and the
// words here have to be the words for that wiring. It is pure data with no
// browser API in it. Writing a second copy of "Stereo / Left only / Right only"
// on this side is precisely the bug ./returns.js exists to record: two tables
// that agree with each other and are both wrong.

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
export const LAMP_NAMES = [
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
 *   setPictureOverlaid(on)              whether the native window covers the
 *                                       tile (inline panel only)
 *   measurePictureRect()                the tile's box, or null when there is
 *                                       no tile in this window
 *   setMonitorState(payload)            the PGM monitor process's state, for
 *                                       the card and the Restart button
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
 *   onPictureRefresh(), onRestartMonitor(), onRefreshDeskPicture(), onLevelChange(fraction),
 *   onPresetChange(id),
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
 * ===================== THE PICTURE IS IN ANOTHER WINDOW =====================
 *
 * The programme picture, the return audio and the input meters live in the PGM
 * MONITOR window — a second process the application launches (app_monitor.go)
 * — and this view shows a CARD in their place: where they went, and a button
 * that restarts that process when it has stopped answering. The pieces
 * themselves are ./pgmpanel.js, which monitorview.js mounts in that window.
 *
 * A REMOTE seat cannot open a window on the host, so for a remote client this
 * view mounts the panel INLINE, where the card would be — the same mosaic,
 * meters and controls as before, minus the SRT picture. viewOpts.pgm says
 * which: 'inline' or 'external' (the default).
 */
export function createHomeView(handlers, viewOpts = {}) {
  const pgmInline = viewOpts.pgm === 'inline';
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
  alertsRegion.append(alertsClear, alertsList);

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

  // --- the picture area, or where it went -----------------------------------
  //
  // The PGM tile, the meters and the return controls are ./pgmpanel.js. A
  // remote seat gets them here, inline; the application's own window gets a
  // card instead, because they are in the PGM Monitor window — and the card
  // carries the one thing that window cannot do for itself: restart it.
  const pgmStage = document.createElement('div');
  pgmStage.className = 'pgm-stage';

  const panel = pgmInline
    ? createPgmPanel(handlers, {
        showError: (message, severity) => showError(message, severity),
        clearErrorIf: (message) => clearErrorIf(message),
      })
    : null;
  if (panel) panel.attachStage(pgmStage);

  const monitorCard = document.createElement('div');
  monitorCard.className = 'monitor-card';
  monitorCard.hidden = pgmInline;
  const monitorCardTitle = document.createElement('div');
  monitorCardTitle.className = 'monitor-card-title';
  monitorCardTitle.textContent = 'PGM MONITOR';
  const monitorCardState = document.createElement('div');
  monitorCardState.className = 'monitor-card-state';
  monitorCardState.setAttribute('role', 'status');
  const monitorCardText = document.createElement('p');
  monitorCardText.className = 'monitor-card-text';
  monitorCardText.textContent =
    'The programme picture, the return audio and the input meters are in the PGM Monitor window. ' +
    'Refresh picture asks that window to refresh — the mosaic or the SRT picture, whichever is ' +
    'showing. If the window has frozen or stopped answering, restart it: it is closed and reopened. ' +
    'The feed going to air is not touched either way.';
  const refreshDeskBtn = document.createElement('button');
  refreshDeskBtn.type = 'button';
  refreshDeskBtn.className = 'btn btn-ghost monitor-refresh-desk';
  refreshDeskBtn.textContent = 'Refresh picture';
  refreshDeskBtn.title = 'The same as the Refresh button in the PGM Monitor window.';
  refreshDeskBtn.addEventListener('click', () => handlers.onRefreshDeskPicture());
  const restartBtn = document.createElement('button');
  restartBtn.type = 'button';
  restartBtn.className = 'btn btn-primary monitor-restart';
  restartBtn.textContent = 'Restart monitor';
  restartBtn.addEventListener('click', () => handlers.onRestartMonitor());
  const monitorCardActions = document.createElement('div');
  monitorCardActions.className = 'monitor-card-actions';
  monitorCardActions.append(refreshDeskBtn, restartBtn);
  monitorCard.append(monitorCardTitle, monitorCardState, monitorCardText, monitorCardActions);

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

  // ONE ROW: PICTURE, METERS, PREVIEW. Left to right, all three side by side.
  //
  // The rework had swept the meters into the settings column, and the operator
  // moved them back — "the meters should still be next to the preview and not
  // in the settings sidebar" — and then corrected the shape of the fix as well,
  // because the first attempt put the preview and the meters in a vertical
  // stack: "The preview of the video was correct before, being next to the main
  // video. Now it's sat above the meter? They should all be next to each other
  // in a line."
  //
  // The order is the operator's too: "it goes big montior, metering, little".
  // The meters sit against the picture they belong to, and the confidence
  // preview — the smallest and least urgent of the three — takes the outside.
  //
  // So they are three flex siblings of .pgm-stage and not a stack, which is what
  // the preview and the meters each had before any of this and what their CSS
  // was written for: the preview is height-led from the stage's own height in
  // cqh units, and the meters stretch to the stage.
  //
  // The meters are here rather than in the column because they are WATCHED,
  // continuously, for the whole match — they answer the question a commentator
  // asks most often, which is whether their microphone is live and at level.
  // The column is for what you go and consult.
  //
  // Both are SIBLINGS of .pgm-tile and never children of it. That rectangle is
  // covered by an opaque native child window, so anything drawn inside it is
  // invisible exactly when a commentator is mid-match, and two native windows
  // told to occupy overlapping rectangles simply erase one another.
  if (panel) pgmStage.append(panel.tileEl, panel.metersEl, previewTile);
  else pgmStage.append(monitorCard, previewTile);

  const audioEl = document.createElement('audio');
  audioEl.autoplay = true;
  audioEl.hidden = true;

  // The tile as configured, and the mosaic as it actually arrived. Neither is
  // authoritative on its own: the crop is what they produce together.
  function setTile(tile) {
    if (panel) panel.setTile(tile);
  }

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

  controls.append(makeRow('Commentary input', 'input-select', inputSelect));
  if (panel) {
    controls.append(panel.headphoneRow, panel.returnGroup, panel.channelGroup, panel.levelGroup);
  }

  // --- the PGM monitor's section in the column ---------------------------------
  //
  // The status line and the restart button again, beside the other controls,
  // for the operator who is looking at the column rather than the main area.
  const monitorGroup = document.createElement('div');
  monitorGroup.className = 'control-group control-group-monitor';
  const monitorGroupState = document.createElement('span');
  monitorGroupState.className = 'control-label monitor-group-state';
  const monitorGroupRefresh = document.createElement('button');
  monitorGroupRefresh.type = 'button';
  monitorGroupRefresh.className = 'btn btn-ghost btn-small monitor-refresh-desk';
  monitorGroupRefresh.textContent = 'Refresh picture';
  monitorGroupRefresh.title = 'The same as the Refresh button in the PGM Monitor window.';
  monitorGroupRefresh.addEventListener('click', () => handlers.onRefreshDeskPicture());
  const monitorGroupBtn = document.createElement('button');
  monitorGroupBtn.type = 'button';
  monitorGroupBtn.className = 'btn btn-ghost btn-small monitor-restart';
  monitorGroupBtn.textContent = 'Restart monitor';
  monitorGroupBtn.addEventListener('click', () => handlers.onRestartMonitor());
  const monitorGroupActions = document.createElement('div');
  monitorGroupActions.className = 'desk-actions';
  monitorGroupActions.append(monitorGroupRefresh, monitorGroupBtn);
  monitorGroup.append(monitorGroupState, monitorGroupActions);

  // THE REMOTE SEAT'S KICK. A seat in another building that sees the desk's
  // picture go wrong can refresh it — App.RefreshMonitorPicture, an event to
  // the PGM Monitor window's page, its own Refresh button pressed from afar —
  // or restart that window outright. Inline mode only: in the application's
  // own window the card and the group above carry the same two buttons.
  const deskGroup = document.createElement('div');
  deskGroup.className = 'control-group control-group-desk';
  const deskLabel = document.createElement('span');
  deskLabel.className = 'control-label';
  deskLabel.textContent = "The desk's PGM monitor";
  const deskRefresh = document.createElement('button');
  deskRefresh.type = 'button';
  deskRefresh.className = 'btn btn-ghost btn-small monitor-refresh-desk';
  deskRefresh.textContent = 'Refresh desk picture';
  deskRefresh.title =
    "Ask the desk's PGM Monitor window to refresh its picture: the same as its own Refresh button. " +
    'The feed going to air is not touched.';
  deskRefresh.addEventListener('click', () => handlers.onRefreshDeskPicture());
  const deskRestart = document.createElement('button');
  deskRestart.type = 'button';
  deskRestart.className = 'btn btn-ghost btn-small monitor-restart';
  deskRestart.textContent = 'Restart desk monitor';
  deskRestart.title =
    "Close and reopen the desk's PGM Monitor window, for when it has frozen or stopped answering. " +
    'The feed going to air is not touched.';
  deskRestart.addEventListener('click', () => handlers.onRestartMonitor());
  const deskActions = document.createElement('div');
  deskActions.className = 'desk-actions';
  deskActions.append(deskRefresh, deskRestart);
  deskGroup.append(deskLabel, deskActions);

  const startStopBtn = document.createElement('button');
  startStopBtn.type = 'button';
  startStopBtn.className = 'btn btn-primary btn-start';
  startStopBtn.textContent = 'START';
  startStopBtn.addEventListener('click', () => handlers.onStartStop());

  const { el: lampsEl, lamps } = createLampRow(LAMP_NAMES);

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
  // START/STOP SITS WITH THE STATUS, and that is the operator's correction:
  // "start should still be in the footer next to the CHeck status".
  //
  // The rework had moved it into the column's Session section on the reasoning
  // that the match bar is for what you WATCH and the column for what you DO.
  // That reasoning fails on this one control, because going on air and knowing
  // whether you are on air are the same question asked twice — the indicator
  // reads CHECK or NOT READY precisely when the operator's next act is to press
  // this — and separating a verdict from the action it prompts puts a head-turn
  // between them at the only moment nobody has one to spare.
  matchBar.append(overallEl, startStopBtn, coughEl);

  // ----- the keyboard bindings -----
  //
  // Document-level and capturing, because the operator's hands are not
  // guaranteed to be anywhere near this button, and because the default action
  // has to be suppressed: Space activates whatever is focused and scrolls.
  //
  // WHAT to do with each keydown — cancel the default? raise a gesture? — is
  // coughMuteKeyDown's decision, kept a pure function so the part that has been
  // got wrong twice is testable without a DOM: the typing-field exclusion, the
  // focused button that keeps its own Space, and the REPEAT. A held Space must
  // keep cancelling its default on EVERY repeat, or the page scrolls and the
  // system key sound loops for as long as the mute is held; the press is raised
  // on the first edge only. This handler just does what it returns.
  //
  // pushKeyHeld records whether THIS binding owns the current Space press. It
  // decides one thing only: whether the keyup may be cancelled. A <button> is
  // activated by Space on the KEYUP, so cancelling that edge unconditionally is
  // what silenced every button in the app — see isSpaceActivated.
  let pushKeyHeld = false;
  function onKeyDown(ev) {
    const { preventDefault, gesture } = coughMuteKeyDown(ev, ev.target === pushBtn);
    if (preventDefault) ev.preventDefault();
    if (gesture === 'press') {
      pushKeyHeld = true;
      handlers.onMutePress();
    } else if (gesture === 'latch') {
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
    makeRailSection('Session', presetIndicator),
    makeRailSection('Status', lampsEl),
    makeRailSection('Audio', controls),
    panel ? makeRailSection('Picture', panel.pictureGroup, deskGroup) : makeRailSection('PGM monitor', monitorGroup),
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

  function setInputDevices(devices, selectedId) {
    fillDeviceSelect(inputSelect, devices, selectedId, 'No input devices found');
  }

  function setHeadphoneDevices(devices, selectedId) {
    if (panel) panel.setHeadphoneDevices(devices, selectedId);
  }
  function setReturnMid(mid) {
    if (panel) panel.setReturnMid(mid);
  }
  function setReturnChannel(mode) {
    if (panel) panel.setReturnChannel(mode);
  }

  function setPictureSource(source) {
    if (panel) panel.setPictureSource(source);
  }
  function setPictureAvailable(available, reason) {
    if (panel) panel.setPictureAvailable(available, reason);
  }
  function setPictureState(state) {
    if (panel) panel.setPictureState(state);
  }
  function setPictureOverlaid(overlaid) {
    if (panel) panel.setPictureOverlaid(overlaid);
  }
  function measurePictureRect() {
    return panel ? panel.measurePictureRect() : null;
  }

  /**
   * setMonitorState paints the card and the column's line from the "monitor"
   * event's payload: whether the PGM monitor process is running, and what its
   * KVS connection is doing. The button reads "Open" when there is nothing to
   * restart.
   */
  function setMonitorState(payload) {
    const process = payload && typeof payload.process === 'string' ? payload.process : 'closed';
    const kvs = payload && typeof payload.kvs === 'string' ? payload.kvs : '';
    let line;
    switch (process) {
      case 'running':
        line = kvs ? `Monitor window open — mosaic ${kvs}` : 'Monitor window open';
        break;
      case 'starting':
        line = 'Monitor window opening…';
        break;
      case 'failed':
        line = 'Monitor window could not be opened — see the alerts';
        break;
      default:
        line = 'Monitor window closed';
    }
    monitorCardState.textContent = line;
    monitorGroupState.textContent = line;
    monitorCard.classList.toggle('monitor-card-down', process !== 'running');
    const label = process === 'running' || process === 'starting' ? 'Restart monitor' : 'Open monitor';
    restartBtn.textContent = label;
    monitorGroupBtn.textContent = label;
  }
  setMonitorState(null);

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
   * viewport — the WebView client area.
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

  // Peak-hold state for the input meters. One instance for the view's life:
  // the zero-frame emitted when capture goes down resets it below, so the next
  // device comes up with no ghost of the last one's peaks.
  function setLevels(frame) {
    if (panel) panel.setLevels(frame);
  }
  function setLevel(fraction) {
    if (panel) panel.setLevel(fraction);
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
    videoEl: panel ? panel.videoEl : null,
    audioEl,
    // The mosaic's box. app.js observes it for resizes, because the preview
    // box beside it is sized from the same stage; nothing here knows that.
    pictureEl: panel ? panel.tileEl : null,
    // The element whose box the native preview surface is told to occupy. It
    // is a separate element from the tile precisely so the two rectangles can
    // never overlap, which would erase the mosaic under the surface and report
    // nothing.
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
    setMonitorState,
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
