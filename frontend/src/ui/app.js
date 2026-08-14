import * as backend from './backend.js';
import { deriveSenderLamp, deriveStatusLamps, deriveMonitorLamp } from './lamps.js';
import { createHomeView } from './home.js';
import { createSettingsView } from './settings.js';
// The mixer host. Imported STATICALLY: it is the only module that imports
// ./mixer/index.js, which is the only module that imports mixer.css, so this
// line is what puts the whole drawer and its stylesheet in the bundle. A
// dynamic import would build and would ship nothing until a code path ran.
import { createMixerHost } from './mixerhost.js';
import {
  RETURN_SOURCE_SRT,
  RETURN_SOURCE_WEBRTC,
  DEFAULT_RETURN_SOURCE,
  deriveReturnSourceEffects,
} from './returnsource.js';
// The PICTURE source. Separate module, separate control, and deliberately
// nothing to do with the one above: SRT carries the picture and Kinesis carries
// the audio, and the whole reason this pair of modules is two modules is that
// they were once one and the operator lost their audio to it.
import {
  PICTURE_SOURCE_SRT,
  PICTURE_SOURCE_MOSAIC,
  PICTURE_SOURCE_KEY,
  DEFAULT_PICTURE_SOURCE,
  PICTURE_STATE_STOPPED,
  normalisePictureSource,
  derivePictureSourceEffects,
} from './picturesource.js';
// The native overlay's geometry and its visibility rules.
import { createOverlay, BLOCK_SETTINGS, BLOCK_MIXER, BLOCK_HIDDEN } from './overlay.js';
// The return path state machine. Every stop, start, mute and un-mute of either
// return path goes through it — this file no longer calls startReturn,
// stopReturn or setAudioEnabled itself, and must not start again. See the header
// of returnpath.js for why the ordering is a module rather than a convention.
import { createReturnPath, returnOptsFingerprint } from './returnpath.js';
import { DEFAULT_CHANNEL_MODE, normaliseChannelMode } from '../monitor/channels.js';

// The frontend seam (fixed by CONTRACT.md, not negotiable by either side):
// WP-5a's module exports one factory, createMonitor(opts) -> Monitor, with
// start/stop/setReturnMid/setGainDb/setLevel/setSinkId/on. This file is the
// only place WP-5b reaches into it, and only through that factory — never a
// named export or an internal of frontend/src/monitor/.
//
// WP-5a may still be writing that module while this file is developed
// against it in parallel, so every call into `monitor` below is wrapped so a
// throw or a rejection greys the MONITOR lamp and logs to the console
// instead of taking the rest of the UI down with it (see setUpMonitor).
import { createMonitor } from '../monitor/monitor.js';

// Top-level orchestration: owns the one piece of state everything else reads
// (the current Config), wires backend.js's events to the two views' lamps
// and dropdowns, and is the only module that talks to the monitor factory.
//
// Owner: WP-5b.

// LOCAL_CLIENT_ID mirrors app_remote.go's localClientID: the fixed id the Go
// side stamps on the "config" event for a save made by the LOCAL WebView2 seat
// (which does not go through the remote dispatcher). It is a string contract,
// spelled the same on both sides; a page uses it to recognise the echo of its
// own save. A remote page instead knows its own id from the shim
// (window.__wslcommsRemote.client), so ownClientId prefers that when present.
const LOCAL_CLIENT_ID = 'local-webview2';

/**
 * ownClientId returns the id the "config" event carries as `origin` when THIS
 * page is the one that saved. For a remote browser that is the connection id the
 * shim recorded on the hello frame; for the local window it is LOCAL_CLIENT_ID.
 * The comparison against it is the whole mechanism that stops a page reacting to
 * the echo of its own save.
 */
function ownClientId() {
  const remote = typeof window !== 'undefined' ? window.__wslcommsRemote : null;
  return (remote && remote.client) || LOCAL_CLIENT_ID;
}

/** Mounts the whole application into root (the #app div from index.html). */
export function mountApp(root) {
  let currentConfig = null;
  let currentSenderState = undefined;
  let currentStatus = undefined;
  let monitor = null;
  // Caches of the last device lists fetched, kept only so a Settings save
  // can re-render the dropdowns' selection without a second device fetch.
  // currentConfig, not these, is the source of truth for which id is chosen.
  let currentInputDevices = [];
  // TWO headphone lists, never merged. The browser's list is mediaDeviceIds
  // from enumerateDevices; the Windows list is WASAPI endpoint ids from Go.
  // They name the same hardware in two identifier spaces, and a value from one
  // put into the other's field does not fail — it plays on the default device,
  // silently, which is the failure mode that ends a match with a commentator
  // hearing nothing and every lamp green.
  let currentHeadphoneDevices = [];
  let currentOutputDevices = [];
  /**
   * 'webrtc' | 'srt'. The path currently feeding the commentator's ears.
   *
   * It is a MIRROR of returnPath.source, kept in step by the onSource callback
   * below. It is assigned in exactly one other place — init, before the monitor
   * is constructed, because createMonitor has to be told whether to build its
   * audio silent and returnPath.adoptSaved cannot run until it exists. Every
   * other read is read-only. Assigning to it from a handler would put the
   * decision back in two places, which is the shape of the bug returnpath.js
   * exists to remove.
   */
  let currentReturnSource = DEFAULT_RETURN_SOURCE;

  /**
   * The gst.ReturnOpts fingerprint the RUNNING SRT return was started with.
   *
   * gst.ReturnOpts is read once, in Play. Nothing about a running pipeline
   * changes when the configuration does, so a Settings save that alters the
   * channel or the WASAPI endpoint leaves every control on screen agreeing with
   * a pipeline that disagrees with all of them — deterministically, not as a
   * race. Comparing this against the saved config is how a save that matters is
   * told from one that does not.
   */
  let runningReturnOpts = '';

  const settings = createSettingsView({
    onBack: showHome,
    onSaved: onConfigSaved,
    // The instance-preset apply. Settings owns the preview drawn on the form
    // and the button that commits it; this file owns the sequence — because the
    // sequence has to reach the monitor, the mixer host and the picture, none of
    // which Settings can see.
    //
    // It ends where it started. Applying a preset used to go through
    // onConfigSaved, which ends in showHome(), so pressing Apply on the Settings
    // screen threw the operator back to the main screen — see applyConfigLive
    // for why the re-application and the navigation are now two things.
    onApplyPreset: applyPresetAndRefresh,
  });

  const home = createHomeView({
    onSettings: showSettings,
    onMixer: () => toggleMixer(),
    onStartStop: onStartStopClick,
    onInputChange: onInputChange,
    onHeadphoneChange: onHeadphoneChange,
    onReturnChange: onReturnChange,
    onReturnChannelChange: onReturnChannelChange,
    onPictureSourceChange: onPictureSourceChange,
    onLevelChange: onLevelChange,
    // Switching instance from the header indicator. It reuses the SAME apply
    // sequence the Settings preset UI does — no second apply path — see
    // onHomePresetChange.
    onPresetChange: onHomePresetChange,
  });

  // The drawer mounts at the TOP LEVEL, not inside home.el.
  //
  // It paints as a fixed full-window overlay, but `hidden` on an ancestor takes
  // it with it: mounted inside the home view it would vanish the moment
  // somebody opened Settings behind it. Mounted here it survives a view swap —
  // and showSettings closes it anyway, so an armed write path cannot be left
  // running behind a screen nobody is looking at.
  const mixerMount = document.createElement('div');
  mixerMount.className = 'mixer-mount';

  root.textContent = '';
  root.append(home.el, settings.el, mixerMount);
  home.setDevBadge(backend.usingFakeBackend);

  // --- the picture ----------------------------------------------------------
  //
  // ===================== THE PICTURE COMES FROM SRT ==========================
  //
  // A native child window, decoding H.265 from M2L-X Output 1 (src=pgm, port
  // 40501, 1920x1080 50p), painted OVER this page. The frontend's whole job is
  // to reserve a rectangle, describe it in physical pixels, and say when it must
  // be hidden.
  //
  // ===================== THE AUDIO COMES FROM KINESIS ========================
  //
  // Unchanged, and NOT REACHABLE FROM THE PICTURE CONTROL. Every function in
  // this section may be read with that in mind: none of them calls into
  // returnPath, none of them touches the monitor's audio, and none of them
  // writes returnSource. The previous revision made SRT an audio path, which is
  // the opposite of what was asked for, and selecting it silenced the operator.
  //
  /** 'srt' | 'mosaic' — which picture the operator has asked for. */
  let currentPictureSource = DEFAULT_PICTURE_SOURCE;
  /** The last "picture" event, or null before one has arrived. */
  let currentPictureState = null;
  /** Whether this build has the five native picture bindings at all. */
  let pictureBindingsPresent = false;

  /**
   * pictureFault reports a failure of the native picture ONCE, to the console,
   * and not as a banner.
   *
   * The mosaic is still on screen and the commentator still has a picture, so
   * this is a degradation and not an outage. A red banner for every failed
   * SetPictureRect against a build that has no such binding is how people learn
   * to ignore the banner that matters.
   */
  let lastPictureFault = '';
  function pictureFault(err) {
    const message = String(err?.message || err);
    if (message === lastPictureFault) return;
    lastPictureFault = message;
    console.info('wslcomms: the native picture overlay is not answering:', message);
  }

  /**
   * overlay owns the rectangle and the visibility rule. It is pure; everything
   * effectful is here.
   *
   * setRect and setVisible are fire-and-forget with a catch. They cannot be
   * awaited: they are driven from a ResizeObserver and a resize listener, and a
   * chain of awaited IPC calls behind a resize is how a window drag turns into a
   * queue of stale rectangles arriving after the user has stopped moving.
   */
  const overlay = createOverlay({
    measure: () => home.measurePictureRect(),
    // Read FRESH on every sync rather than captured: a DPI change alters this
    // number without moving the CSS box by a pixel, and a captured ratio would
    // keep reporting the old geometry as correct.
    dpr: () => (typeof window !== 'undefined' && window.devicePixelRatio) || 1,
    // CSS pixels and the ratio, together, because that is what
    // App.SetPictureRect takes: gst.ScaleRect multiplies on the Go side, since
    // the ratio Go could read for itself — GetDpiForWindow — is the monitor's
    // scale factor and not the WebView's, and Ctrl+scroll moves one without the
    // other. The physical rectangle is computed here too, but only to decide
    // whether to send and what to log.
    setRect: (css, dpr) => {
      Promise.resolve()
        .then(() => backend.setPictureRect(css, dpr))
        .catch(pictureFault);
    },
    setVisible: (on) => {
      Promise.resolve()
        .then(() => backend.setPictureVisible(on))
        .catch(pictureFault);
    },
    // THE MOSAIC FOLLOWS THE WINDOW, NOT THE SOURCE SELECTION.
    //
    // home.js suppresses the mosaic <video> while the native overlay covers it,
    // and this is the only thing that tells it to. It used to be driven from
    // `effects.showingSRT` inside home.renderPicture, which is a different fact:
    // the overlay is hidden whenever something must appear above it — the mixer
    // drawer, Settings, a modal — and none of those change the source. Opening
    // the drawer therefore took the native picture away and left the mosaic
    // suppressed underneath it, and the commentator got BLACK.
    //
    // Wired here because overlay.js is where visibility is DECIDED — the same
    // expression that drives setVisible above — so the page cannot hold a
    // different opinion about what is on screen.
    onVisible: (on) => home.setPictureOverlaid(on),
    log: (message) => console.info(message),
  });

  const mixerHost = createMixerHost({
    mount: mixerMount,
    // THE DRAWER MUST NEVER BE UNDER THE PICTURE OVERLAY. The overlay is a
    // native child window: it is opaque, it is outside the page's stacking
    // context, and no z-index in mixer.css reaches it. A drawer opened
    // underneath it is a routing matrix an operator can read two thirds of and
    // click all of.
    //
    // This fires for a drawer closed from inside itself as well — its Close
    // button, the scrim, Escape — none of which app.js is otherwise told about.
    onOpenChange: (open) => {
      if (open) overlay.block(BLOCK_MIXER);
      else overlay.unblock(BLOCK_MIXER);
    },
    onStatus: (message, isError) => {
      if (!message) return;
      // The host's own failures — arming, polling — reach the operator through
      // the home screen's error banner. The drawer shows its own refusals in
      // the cell that was clicked; these are the ones it cannot know about.
      if (isError) home.showError(`Mixer: ${message}`);
      else console.info(`wslcomms: mixer: ${message}`);
    },
  });

  // --- the return path ------------------------------------------------------

  /**
   * returnPath owns every transition between the two return paths.
   *
   * It is given the effects and nothing else — no DOM, no configuration — which
   * is what lets "exactly one path is audible, in every reachable state
   * including every failure path" be tested as a property rather than as three
   * examples. See returnpath.test.js.
   *
   * setWebRTCAudible goes through safeMonitorCall, so it cannot throw. That
   * matters: the machine treats it as infallible, and a monitor that is missing
   * or broken is a MONITOR lamp problem, not a reason to abandon a transition
   * half-done.
   */
  const returnPath = createReturnPath({
    startReturn: () => backend.startReturn(),
    stopReturn: () => backend.stopReturn(),
    setWebRTCAudible: (on) => safeMonitorCall((m) => m.setAudioEnabled(on)),
    saveSource: (source) => persistConfigAndWait({ returnSource: source }),
    isAlreadyRunning: backend.isReturnAlreadyRunningError,
    isNotRunning: backend.isReturnNotRunningError,
    showError: (message) => home.showError(message),
    log: (message) => console.info(message),
    // There is no control on screen to keep in step any more: the audio path is
    // fixed at Kinesis and the picture control cannot reach this machine. What
    // is left is the mirror the rest of this file reads, and the Headphones
    // list, which is chosen by identifier space rather than by preference.
    onSource: (source) => {
      currentReturnSource = source;
      renderHeadphoneList();
    },
  });

  /**
   * afterReturnOperation records what the running return was built from, and
   * reports a refusal.
   *
   * A refusal is the in-flight guard doing its job — two of these sequences
   * running at once is how a stop from one lands between the other's stop and
   * start — so it is said out loud and the control is put back to what the
   * configuration actually holds, rather than left showing a choice that was
   * never applied.
   */
  function afterReturnOperation(result, revert) {
    runningReturnOpts =
      returnPath.source === RETURN_SOURCE_SRT ? returnOptsFingerprint(currentConfig) : '';
    if (result && result.applied === false && result.reason) {
      home.showError(`That change was not applied: ${result.reason}.`);
      if (revert) revert();
    }
    return result;
  }

  function showSettings() {
    // Settings must not be opened behind a live mixer drawer: the drawer is
    // modal, and one that outlived the screen it was opened from is a write
    // path nobody can see.
    mixerHost.close();
    // AND IT MUST NOT BE OPENED BEHIND THE PICTURE OVERLAY EITHER. Two separate
    // reasons are raised, and both have to be released before the picture comes
    // back: BLOCK_SETTINGS because Settings is on screen, BLOCK_HIDDEN because
    // the home view — and with it the rectangle the overlay was measured
    // against — is not. Hiding on one reason and showing on the other's release
    // is precisely the bug the reason SET exists to make unwritable.
    overlay.block(BLOCK_SETTINGS);
    overlay.block(BLOCK_HIDDEN);
    home.el.hidden = true;
    settings.el.hidden = false;
    settings.open();
  }

  function showHome() {
    settings.el.hidden = true;
    home.el.hidden = false;
    overlay.unblock(BLOCK_SETTINGS);
    overlay.unblock(BLOCK_HIDDEN);
    // Re-measure before painting: the window may have been resized while
    // Settings was up, and an overlay restored at yesterday's rectangle is a
    // picture in the wrong place for as long as nothing else moves.
    overlay.sync();
  }

  /**
   * toggleMixer opens or closes the drawer. The overlay is hidden and restored
   * by the host's onOpenChange rather than from here, because the drawer can
   * also close itself — Escape, the scrim, its own Close button — and a hide
   * written at the call site would never see that.
   */
  function toggleMixer() {
    mixerHost.toggle();
  }

  // --- lamps -------------------------------------------------------------

  function renderSenderLamp() {
    home.lamps.SENDING.update(deriveSenderLamp(currentSenderState));
    home.setRunning(!!currentSenderState && currentSenderState !== backend.SENDER_STATE.STOPPED);
    // The Settings screen's preset gate tracks the SAME derivation of the same
    // state home.setRunning does, from the same place, so the Apply button and
    // the START/STOP button can never disagree about whether a session is up.
    // The gate itself is Go's — ApplyPreset refuses while sending — this is
    // the honest rendering of it on the control.
    settings.setSending(!!currentSenderState && currentSenderState !== backend.SENDER_STATE.STOPPED);
    // The honest line used to be updated from here as well, so that its claim
    // and the lamp could never disagree about whether anything was being sent.
    // It is no longer rendered — see the header of home.js — and the SENDING
    // lamp is now the only thing this state reaches.
  }

  function renderStatusLamps() {
    const { switcher, video, audio, unavailable } = deriveStatusLamps(currentStatus);
    home.lamps['SWITCHER SEES FEED'].update(switcher);
    home.lamps.VIDEO.update(video);
    home.lamps.AUDIO.update(audio);
    home.setStatusUnavailable(unavailable);
  }

  renderSenderLamp();
  renderStatusLamps();
  home.lamps.MONITOR.update(deriveMonitorLamp(undefined));

  backend.onSender((state) => {
    currentSenderState = state;
    renderSenderLamp();
  });

  backend.onStatus((status) => {
    currentStatus = status;
    renderStatusLamps();
  });

  backend.onError((message) => {
    console.error('wslcomms: backend error event', message);
    home.showError(String(message));
  });

  // The input meters beside the picture: the SEND pipeline's own peak/RMS
  // measurement of what is being encoded and sent, at most 20 frames a second.
  // Pure forwarding — the scale, zones and peak-hold all live in ui/meters.js
  // and the painting in home.js — and no session-state bookkeeping here: the
  // Go side ends every session with an all-silence zero-frame, which is what
  // dims the meters, so this wire never has to know whether a session is
  // running.
  backend.onLevels((frame) => home.setLevels(frame));

  // The native PICTURE receiver's own state. This is what drives the fallback:
  // the moment it stops SHOWING, the overlay is hidden and the mosaic
  // underneath is what the commentator is looking at — with the badge over the
  // picture saying so. Nothing about the audio changes at any point in that.
  backend.onPicture((state) => {
    currentPictureState = state ? String(state) : null;
    renderPicture();
  });

  // The native AUDIO return's state. It is kept as a capability on the Go side
  // and is never selected by this UI any more — audio always comes from
  // Kinesis — so an event from it means something started it that this page did
  // not, which is worth a console line and nothing on screen.
  backend.onReturn((state) => {
    console.info('wslcomms: the native SRT audio return reported', String(state));
  });

  // --- a SECOND controller's save --------------------------------------------
  //
  // config is written as a WHOLE OBJECT from a per-page cache (persistConfig
  // below, and settings.js's collectConfig), and App.SaveConfig applies it
  // verbatim with no field-level merge. With two controllers — the local
  // WebView2 and a remote browser on the LAN bridge — one page's save would
  // otherwise clobber the other's edits until something forced a reload. The
  // "config" event closes that to one round trip: after ANY seat saves, every
  // OTHER seat adopts the result.
  //
  // It is emitted to this page too on its OWN saves; those are ignored by
  // origin, because this page has already applied its own change through the
  // ordinary save path and re-applying would be at best redundant and at worst —
  // for a remote operator mid-edit — a Settings screen yanked out from under
  // them (onConfigSaved ends in showHome()). So this handler deliberately does
  // NOT call onConfigSaved(): it re-applies only the live-safe parts and leaves
  // the current view exactly where it is.
  backend.onConfig((payload) => {
    if (!payload || !payload.config) return;
    // Our own save, echoed back. We already applied it locally; do nothing —
    // and above all do not showHome() on a page that may be mid-edit.
    if (payload.origin === ownClientId()) return;
    applyRemoteConfig(payload.config);
    // Another seat's save may have applied a different instance — the Settings
    // screen or a remote client can change the active preset — so refresh the
    // header indicator from the authority rather than trusting the config event
    // to carry preset state (it does not).
    refreshHomePresets();
  });

  /**
   * applyRemoteConfig adopts a config another controller saved and re-applies
   * only the parts that are safe to change under a page that did not ask for it:
   * the monitor tile, the return bus/channel/gain, the headphone selection and
   * the input dropdown. It is a strict subset of applyConfigLive, on purpose —
   * smaller even than that, because this page did not ask for any of it:
   *
   *   - NO showHome() — the whole reason this is not onConfigSaved(). A remote
   *     operator editing Settings must not be ejected because somebody at the
   *     desk pressed Save.
   *   - NO picture-source or return-source switch. Neither is a Settings field,
   *     and flipping what this operator is watching or hearing from another
   *     page's save is exactly the silent side effect the source controls were
   *     split apart to prevent.
   *   - NO SRT-return rebuild. The audio path is fixed at Kinesis in this build
   *     (currentReturnSource is always webrtc), so there is nothing to rebuild;
   *     re-dialling it for a remote save would only risk a glitch for no gain.
   */
  function applyRemoteConfig(config) {
    currentConfig = config;
    home.setTile(config.monitorTile);
    home.setReturnMid(config.returnMid);
    home.setInputDevices(currentInputDevices, config.audioDeviceId);
    const channel = normaliseChannelMode(config.returnChannel);
    home.setReturnChannel(channel);
    renderHeadphoneList();
    safeMonitorCall((m) => {
      m.setReturnMid(config.returnMid);
      m.setChannelMode(channel);
      m.setGainDb(config.returnGainDb);
      const sink = selectedHeadphoneId();
      if (sink && currentReturnSource !== RETURN_SOURCE_SRT) m.setSinkId(sink);
    });
  }

  // The connected remote seats, straight to the home-screen indicator. The
  // local WebView2 is never in this list (it does not connect to the listener),
  // so the indicator shows only OTHER seats — which is the question the operator
  // at the desk is asking: is someone else driving this? home.js knows nothing
  // about the backend; app.js is the wire, as everywhere else.
  backend.onRemote((clients) => {
    home.setRemoteClients(Array.isArray(clients) ? clients : []);
  });

  // --- start / stop --------------------------------------------------------

  async function onStartStopClick() {
    const running = !!currentSenderState && currentSenderState !== backend.SENDER_STATE.STOPPED;
    home.setBusy(true);
    home.clearError();
    try {
      if (running) {
        await backend.stop();
      } else {
        await backend.start();
      }
    } catch (err) {
      home.showError(`${running ? 'Stop' : 'Start'} failed: ${err.message}`);
    } finally {
      home.setBusy(false);
    }
  }

  // --- device dropdowns ----------------------------------------------------

  async function loadInputDevices() {
    try {
      currentInputDevices = await backend.listInputDevices();
      home.setInputDevices(currentInputDevices, currentConfig?.audioDeviceId);
    } catch (err) {
      currentInputDevices = [];
      home.setInputDevices([], null);
      home.showError(`Could not list commentary input devices: ${err.message}`);
    }
  }

  /**
   * selectedHeadphoneId reads the id belonging to the CURRENT path out of the
   * config. deriveReturnSourceEffects owns which key that is, so there is no
   * second place where "webrtc means headphoneDeviceId" is written down.
   */
  function selectedHeadphoneId(source = currentReturnSource) {
    const key = deriveReturnSourceEffects(source).deviceKey;
    return currentConfig?.[key] ?? '';
  }

  /**
   * renderHeadphoneList puts the SELECTED path's device list in the dropdown.
   * Called on a source switch and after either list is refetched. The list and
   * the selection always come from the same path — this is the one function
   * where the two identifier spaces could get crossed, so it is the only one
   * allowed to touch the dropdown.
   */
  function renderHeadphoneList() {
    const list =
      currentReturnSource === RETURN_SOURCE_SRT ? currentOutputDevices : currentHeadphoneDevices;
    home.setHeadphoneDevices(list, selectedHeadphoneId());
  }

  /** loadOutputDevices fetches the WASAPI render endpoints for the SRT path. */
  async function loadOutputDevices() {
    try {
      currentOutputDevices = await backend.listOutputDevices();
    } catch (err) {
      currentOutputDevices = [];
      // Not an error banner unless the SRT path is the one selected: against a
      // build without the binding this fails every time, and a permanent red
      // banner about a path nobody chose trains people to ignore the banner.
      if (currentReturnSource === RETURN_SOURCE_SRT) {
        home.showError(`Could not list Windows output devices: ${err.message}`);
      } else {
        console.info('wslcomms: no Windows output device list available:', err.message);
      }
    }
    if (currentReturnSource === RETURN_SOURCE_SRT) renderHeadphoneList();
  }

  async function loadHeadphoneDevices() {
    if (!navigator.mediaDevices?.enumerateDevices) {
      currentHeadphoneDevices = [];
      renderHeadphoneList();
      return;
    }
    try {
      // A silent getUserMedia grant is what makes enumerateDevices() return
      // labels instead of blanks (spec section 7's
      // WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS auto-accepts the prompt this
      // raises; outside the packaged app this may simply fail, which is
      // fine — the fallback below still lists the devices, unlabelled).
      try {
        const probe = await navigator.mediaDevices.getUserMedia({ audio: true });
        probe.getTracks().forEach((t) => t.stop());
      } catch {
        /* no input device, or permission denied — enumerateDevices still works */
      }
      const all = await navigator.mediaDevices.enumerateDevices();
      currentHeadphoneDevices = all
        .filter((d) => d.kind === 'audiooutput')
        .map((d, i) => ({ id: d.deviceId, name: d.label || `Output device ${i + 1}` }));
      renderHeadphoneList();
    } catch (err) {
      currentHeadphoneDevices = [];
      renderHeadphoneList();
      home.showError(`Could not list headphone devices: ${err.message}`);
    }
  }

  if (navigator.mediaDevices) {
    navigator.mediaDevices.ondevicechange = () => {
      loadHeadphoneDevices();
    };
  }

  /**
   * persistConfig merges a patch and saves, reporting a failure and not
   * throwing. Fire-and-forget, which is right for a dropdown that has already
   * been applied to a running object.
   *
   * It is NOT right for anything App.StartReturn will read — see
   * persistConfigAndWait.
   */
  function persistConfig(patch) {
    if (!currentConfig) return;
    currentConfig = { ...currentConfig, ...patch };
    backend.saveConfig(currentConfig).catch((err) => {
      home.showError(`Could not save configuration: ${err.message}`);
    });
  }

  /**
   * persistConfigAndWait is persistConfig for the case where the Go side is
   * about to READ the configuration back.
   *
   * App.StartReturn takes no arguments: host, port, latency, channel and the
   * WASAPI endpoint all come from the saved config, and it refuses outright
   * unless returnSource is already "srt". So a start that races its own save
   * either fails with a message about a setting the operator can see is
   * correct, or — worse — succeeds against the previous configuration and plays
   * the wrong thing while reporting success.
   *
   * It rejects, rather than swallowing, so the caller can decide not to start.
   */
  async function persistConfigAndWait(patch) {
    if (!currentConfig) return;
    currentConfig = { ...currentConfig, ...patch };
    await backend.saveConfig(currentConfig);
  }

  function onInputChange(deviceId) {
    persistConfig({ audioDeviceId: deviceId });
  }

  /**
   * onHeadphoneChange writes the chosen id into the field belonging to the
   * CURRENT path and applies it to that path only.
   *
   * The other path's id is left exactly as it was. Both are persisted, so
   * switching back and forth does not lose either selection, and neither id is
   * ever written into the other's field — they are different identifier spaces
   * for the same headphones and mixing them fails silently.
   */
  function onHeadphoneChange(deviceId) {
    const effects = deriveReturnSourceEffects(currentReturnSource);

    if (effects.srtRunning) {
      // wasapi2sink's device is fixed when the pipeline is built, so retargeting
      // means restarting the monitor. returnPath.applyOption owns that sequence:
      // it saves first (StartReturn reads the endpoint out of the SAVED config,
      // so a start racing its own save comes back up on the old endpoint and
      // reports success), it stops before it starts (two monitors dialled into
      // the same M2L-X output would hold two of its four fan-out slots), it
      // holds the same in-flight guard the source switch does, and — the part
      // that used to be missing — it puts the commentator on WebRTC rather than
      // leaving them in silence if the restart fails.
      returnPath
        .applyOption({
          what: 'SRT headphone device',
          save: () => persistConfigAndWait({ [effects.deviceKey]: deviceId }),
        })
        .then((result) => afterReturnOperation(result, renderHeadphoneList));
      return;
    }

    persistConfig({ [effects.deviceKey]: deviceId });
    safeMonitorCall((m) => m.setSinkId(deviceId));
  }

  function onReturnChange(mid) {
    persistConfig({ returnMid: mid });
    safeMonitorCall((m) => m.setReturnMid(mid));
  }

  /**
   * onReturnChannelChange picks which SOURCE channel reaches both ears. It
   * applies to BOTH return paths, by two different mechanisms:
   *
   *   WebRTC  a ChannelSplitter rewired to a ChannelMerger. Live and instant;
   *           the peer connection is not dropped, exactly as switching bus does
   *           not drop it.
   *   SRT     an audiomixmatrix property set when the GStreamer pipeline is
   *           built (gst.MixMatrix). There is no live setter, so it means a
   *           stop and a start.
   *
   * The two produce the same routing. gst.MixMatrix("left") is [[1,0],[1,0]] —
   * source channel 0 into both outputs — which is what
   * monitor/channels.js's sourceChannelsForOutputs('left') returns as [0, 0].
   * If one of them is ever changed the other has to change with it, or the same
   * control means two different things depending on which path is selected.
   */
  function onReturnChannelChange(mode) {
    const channel = normaliseChannelMode(mode);
    // Applied to the WebRTC graph immediately and unconditionally, so that
    // switching back to WebRTC later finds the choice already in force.
    safeMonitorCall((m) => m.setChannelMode(channel));

    if (currentReturnSource !== RETURN_SOURCE_SRT) {
      persistConfig({ returnChannel: channel });
      return;
    }

    // On the SRT path the channel is a MIX MATRIX baked into the GStreamer
    // pipeline when it is built (gst.ReturnOpts.Channel), and there is no live
    // setter for it. Changing it means saving and rebuilding the pipeline —
    // which is a second or two of silence, and the honest alternative is a
    // control that appears to do nothing until the next restart.
    returnPath
      .applyOption({
        what: 'return channel',
        save: () => persistConfigAndWait({ returnChannel: channel }),
      })
      .then((result) =>
        afterReturnOperation(result, () =>
          home.setReturnChannel(normaliseChannelMode(currentConfig?.returnChannel)),
        ),
      );
  }

  // --- the picture source ---------------------------------------------------

  /**
   * renderPicture pushes the current selection and the current receiver state
   * everywhere they are shown, and moves the overlay to match.
   *
   * It is the only writer of overlay.setWanted, and what it passes is
   * `showingSRT` — the selection AND the receiver actually delivering. Not the
   * selection alone: a receiver in CONNECTING or BACKOFF is running and holding
   * a fan-out slot, and painting an opaque native window over the page for it
   * would replace a soft picture of the match with a black rectangle.
   */
  function renderPicture() {
    const effects = derivePictureSourceEffects(currentPictureSource, currentPictureState);
    home.setPictureSource(effects.source);
    home.setPictureState(effects.wantSRT ? currentPictureState : null);
    overlay.setWanted(effects.showingSRT);
    overlay.sync();
  }

  /**
   * onPictureSourceChange switches which picture the commentator is looking at.
   *
   * ============ IT DOES NOT TOUCH THE AUDIO, AND MUST NEVER LEARN HOW ========
   *
   * There is no call to returnPath here, no setAudioEnabled, no setSinkId, no
   * write to returnSource. The commentator hears Kinesis before this function
   * runs and hears Kinesis after it, whichever way it goes and whether or not it
   * fails. That is the entire correction this work exists to make: the control
   * that used to sit in this position switched the HEADPHONES, and selecting
   * "SRT" on it took the operator's audio away.
   *
   * A failure to start the picture leaves the selection on SRT and the mosaic on
   * screen, which is exactly what the fallback is for — the badge over the
   * picture says so, and the status line says why.
   */
  async function onPictureSourceChange(source) {
    const next = normalisePictureSource(source);
    if (next === currentPictureSource) return;
    home.clearError();

    currentPictureSource = next;
    // Drawn before the IPC, not after: the mosaic is on screen either way and
    // the control must follow the press immediately, or an operator who is not
    // sure whether the click landed presses it again.
    renderPicture();
    persistConfig({ [PICTURE_SOURCE_KEY]: next });

    try {
      if (next === PICTURE_SOURCE_SRT) {
        await backend.startPicture();
        // Resolving means the reconnect loop is running, not that a picture has
        // arrived. Read the state so the status line is right before the first
        // event lands.
        currentPictureState = await backend.getPictureState();
      } else {
        await backend.stopPicture();
        currentPictureState = PICTURE_STATE_STOPPED;
      }
    } catch (err) {
      currentPictureState = null;
      home.showError(
        `Could not switch the picture to ${next.toUpperCase()}: ${err?.message || err}. ` +
          'You are watching the multiviewer mosaic; your audio is unaffected.',
      );
    }
    renderPicture();
  }

  /**
   * watchPictureRect reports the reserved rectangle whenever it moves.
   *
   * Three sources, and all three are needed:
   *
   *   ResizeObserver   the box itself changing size — which happens without a
   *                    window resize, because the controls above it wrap.
   *   window resize    the window changing size, and, in WebView2, a DPI change
   *                    as well.
   *   a dppx media query  the DPI change on its own, for the case where the
   *                    window is dragged to a differently-scaled monitor and the
   *                    CSS box does not move by a single pixel while every
   *                    physical coordinate in it changes.
   *
   * There is deliberately NO window-move report. The overlay is a child of the
   * same top-level window and its rectangle is relative to the client area, so a
   * drag moves both together — which is fortunate, because a page cannot observe
   * its own window being dragged.
   */
  function watchPictureRect() {
    if (typeof window === 'undefined') return;
    const sync = () => overlay.sync();

    window.addEventListener('resize', sync);

    if (typeof ResizeObserver === 'function' && home.pictureEl) {
      try {
        new ResizeObserver(sync).observe(home.pictureEl);
      } catch (err) {
        console.info('wslcomms: could not observe the picture box for resizes:', err?.message || err);
      }
    }

    watchDevicePixelRatio(sync);
    sync();
  }

  /**
   * watchDevicePixelRatio fires onChange when the display scaling changes.
   *
   * There is no event for it. The standard construction is a media query pinned
   * to the CURRENT ratio, which stops matching the moment the ratio moves; it
   * has to be re-armed against the new value each time, which is what the
   * recursion below does.
   */
  function watchDevicePixelRatio(onChange) {
    if (typeof window.matchMedia !== 'function') return;
    const arm = () => {
      const dpr = window.devicePixelRatio || 1;
      let mq;
      try {
        mq = window.matchMedia(`(resolution: ${dpr}dppx)`);
      } catch {
        return; // an engine without the resolution feature; resize still fires
      }
      const handler = () => {
        onChange();
        arm();
      };
      if (typeof mq.addEventListener === 'function') {
        mq.addEventListener('change', handler, { once: true });
      } else if (typeof mq.addListener === 'function') {
        // Legacy MediaQueryList. No `once`, so it is removed by hand before the
        // re-arm, or every DPI change would leave another listener behind.
        const legacy = () => {
          mq.removeListener(legacy);
          handler();
        };
        mq.addListener(legacy);
      }
    };
    arm();
  }

  function onLevelChange(fraction) {
    safeMonitorCall((m) => m.setLevel(fraction));
  }

  /**
   * applyConfigLive re-applies a whole saved configuration to everything on this
   * page that was built from one: the monitor tile, the return bus, the channel,
   * the gain, the device dropdowns — and a RUNNING SRT return, which none of
   * those reach.
   *
   * =============== IT DOES NOT DECIDE WHICH VIEW IS ON SCREEN ================
   *
   * That is the whole reason it exists apart from onConfigSaved, and the two
   * callers differ in exactly that one thing:
   *
   *   Save settings   the operator has finished with the form. onConfigSaved is
   *                   this plus showHome(): you press Save, you are done, you go
   *                   back. That is the button's contract, not an accident.
   *   Apply preset    the operator pressed a button ON the Settings screen and
   *                   asked for an instance switch, nothing else. Applying used
   *                   to run through onConfigSaved, so it inherited the
   *                   showHome() and ejected them to the main screen — losing
   *                   the scroll position and the focus of somebody who was
   *                   part-way down a two-screenful form. That is the defect the
   *                   owner reported after using the build.
   *
   * applyRemoteConfig above is the third member of this family and stays a
   * separate, SMALLER subset: another seat's save must not rebuild this page's
   * return path, so it is not expressible as "this minus something".
   */
  function applyConfigLive(config) {
    currentConfig = config;
    home.setTile(config.monitorTile);
    home.setReturnMid(config.returnMid);
    home.setInputDevices(currentInputDevices, config.audioDeviceId);
    // The RETURN SOURCE is deliberately not re-applied from a Settings save.
    // Settings has no control for it; taking it from the saved config here
    // would silently switch a commentator's audio path because somebody
    // pressed Save on an unrelated screen. The channel is safe to re-apply
    // because it cannot silence anything.
    //
    // Nor is the PICTURE source, and for a smaller version of the same reason:
    // Settings has no control for it either, and a Save that switched the
    // picture out from under somebody would at best cost them a few seconds of
    // reconnect. The tile geometry above IS re-applied, because changing it is
    // the whole point of that field — and it can change the reserved box's
    // aspect ratio, which the ResizeObserver in watchPictureRect will notice.
    const channel = normaliseChannelMode(config.returnChannel);
    home.setReturnChannel(channel);
    renderHeadphoneList();
    safeMonitorCall((m) => {
      m.setReturnMid(config.returnMid);
      m.setChannelMode(channel);
      m.setGainDb(config.returnGainDb);
      const sink = selectedHeadphoneId();
      if (sink && currentReturnSource !== RETURN_SOURCE_SRT) m.setSinkId(sink);
    });

    // AND APPLY IT TO A RUNNING SRT RETURN, which the lines above do not.
    //
    // Everything they touch is live: the Web Audio graph, the crop, the
    // dropdowns. gst.ReturnOpts is not. It is read once, in Play, so a channel
    // or endpoint saved here reaches a pipeline that will never look at it
    // again — the operator sets "Left only", saves, every control on screen
    // agrees, and comms is still in their right ear. Deterministic, not a race,
    // and silent.
    //
    // Only when something the pipeline was actually BUILT from has changed:
    // rebuilding on every Save would take the return away for a second or two
    // because somebody corrected a typo in the event id.
    applyReturnOptionsFromConfig();
  }

  /**
   * onConfigSaved is what the SAVE SETTINGS button gets: the live re-application
   * above, and then back to the main screen.
   *
   * The showHome() is the entire difference between the two, and it is load
   * bearing for Save — an operator who presses Save and stays on the form has to
   * find the Back button to see whether anything happened. Anything that wants
   * the re-application WITHOUT the navigation calls applyConfigLive directly;
   * the way to get one is never to delete the line from the other.
   */
  function onConfigSaved(config) {
    applyConfigLive(config);
    showHome();
  }

  /**
   * applyReturnOptionsFromConfig rebuilds a running SRT return when the saved
   * configuration no longer matches what it was started with.
   *
   * The comparison is over returnOptsFingerprint — every field app_return.go's
   * returnOpts reads, and nothing else. returnpath.test.js asserts that list is
   * still the same list by reading returnOpts itself, because a field added
   * there and forgotten here is exactly this bug again.
   */
  function applyReturnOptionsFromConfig() {
    if (returnPath.source !== RETURN_SOURCE_SRT) {
      runningReturnOpts = '';
      return;
    }
    if (returnOptsFingerprint(currentConfig) === runningReturnOpts) return;

    returnPath
      // The save has already happened — Settings wrote the whole config — so
      // this only needs the rebuild. applyOption still owns the ordering, the
      // in-flight guard and the fallback to WebRTC if the restart fails.
      .applyOption({ what: 'saved return settings', save: async () => {} })
      .then((result) => afterReturnOperation(result));
  }

  // --- applying an instance preset ------------------------------------------

  /**
   * applyPresetAndRefresh is the whole apply sequence: Go merges and returns,
   * this page ADOPTS the returned config, and then every connection that was
   * built from the old instance is rebuilt — unconditionally.
   *
   * ============ currentConfig = THE RETURNED CONFIG, NEVER A LOCAL MERGE =====
   *
   * The assignment below is a correctness contract. This file re-writes the
   * WHOLE currentConfig on the next dropdown change (persistConfig), so any
   * version of this that kept the old object — or rebuilt it locally by
   * spreading preset fields over it — would have the stale cache clobber the
   * preset on the very next control the operator touched. Go's ApplyPreset
   * returns the merged document for exactly this reason, and presets.test.js
   * reads this function's source to prove the assignment is still here and
   * that no field is spread by hand.
   *
   * ============ EVERYTHING THAT IS A MONITOR IS RESTARTED ====================
   *
   * Not "only when the port changed": the credential SCOPE changed with the
   * preset, so the return passphrase and the KVS sign-in are different even
   * when every numeric field is the same.
   *
   *   - the mixer drawer is closed (and with it disarmed on the Go side by
   *     ApplyPreset itself): an open arm window pointed at a different desk
   *     is a write gate nobody can see;
   *   - applyConfigLive reuses the whole Settings-save pathway — tile, mid,
   *     channel, gain, dropdowns, a running SRT audio return — WITHOUT its
   *     showHome(). Not onConfigSaved, which is that plus the navigation: both
   *     callers of this function want the operator left exactly where they are.
   *     From the Settings screen that means the same scroll position on the same
   *     form (settings.js restores it around the redraw), and from the header
   *     picker it means home, which is where they already are;
   *   - the KVS monitor is torn down and REBUILT, which applyConfigLive does
   *     not do: setUpMonitor is otherwise called exactly once, at init, and
   *     the peer connection holds credentials fetched for the OLD event —
   *     without this the commentator keeps hearing the previous event until
   *     the connection happens to drop;
   *   - the picture, if selected, is stopped and started: StartPicture reads
   *     the config and the SRT-return passphrase ONCE, at start.
   *
   * The several seconds of black picture and silence this costs are
   * affordable only because ApplyPreset refuses while SENDING — the refusal
   * gate is load-bearing, not a courtesy.
   */
  async function applyPresetAndRefresh(id) {
    const merged = await backend.applyPreset(id);
    currentConfig = merged;

    mixerHost.close();
    applyConfigLive(merged);

    // The KVS monitor rebuild. Stop is best-effort — a monitor that never
    // started still needs the new one built over it.
    safeMonitorCall((m) => m.stop());
    monitor = null;
    setUpMonitor(merged);

    // The picture, only if it is the selected source: stop-then-start against
    // the new instance. "Not running" rejections are the normal case when the
    // receiver was in backoff and are not worth a banner.
    if (currentPictureSource === PICTURE_SOURCE_SRT && pictureBindingsPresent) {
      try {
        await backend.stopPicture();
      } catch {
        /* nothing was running; the start below is still wanted */
      }
      try {
        await backend.startPicture();
        currentPictureState = await backend.getPictureState();
      } catch (err) {
        currentPictureState = null;
        home.showError(
          `The picture did not restart on the new instance: ${err?.message || err}. ` +
            'You are watching the multiviewer mosaic; your audio is unaffected.',
        );
      }
      renderPicture();
    }

    // Missing credentials, surfaced in the banner NOW rather than as a grey
    // lamp later: a scope that was never written to on this machine looks
    // exactly like a wrong password unless somebody says which it is. Only
    // the credentials this configuration actually needs are named.
    try {
      const status = await backend.getPresetCredentialStatus();
      const missing = [];
      if (!status.m2lx) missing.push('the M2L-X password');
      if (!status.srt && merged.pbkeylen) missing.push('the SRT passphrase');
      if (!status.srtreturn && merged.srtReturnPBKeyLen) missing.push('the SRT return passphrase');
      if (missing.length > 0) {
        home.showError(
          `This instance has no stored ${missing.join(' or ')} on this PC yet — enter ` +
            `${missing.length === 1 ? 'it' : 'them'} on the Settings screen before starting.`,
        );
      }
    } catch (err) {
      console.error('wslcomms: could not check the credential status after the apply', err);
    }

    // Ignored keys from a hand-edited preset arrive on the "error" event from
    // Go and reach the banner through the onError wiring above; nothing to do
    // here, and this function must never inspect preset fields itself.

    // The header indicator follows the apply, so switching from Settings and
    // switching from the header both leave the same at-a-glance state behind.
    await refreshHomePresets();
    return merged;
  }

  /**
   * onHomePresetChange applies the instance chosen from the HEADER indicator. It
   * reuses applyPresetAndRefresh — the exact sequence the Settings preset UI
   * runs through onApplyPreset — rather than writing a second apply path: there
   * is one place that calls backend.applyPreset, adopts the returned config and
   * rebuilds the monitor and picture, and this is a second caller of it, not a
   * second copy of it.
   *
   * The header selector is disabled while SENDING (home.setRunning gates it), so
   * a refused-mid-send apply should not arrive here; the catch is the honest
   * fallback if it does, and refreshHomePresets in the finally snaps the
   * indicator back to whatever is ACTUALLY active when the apply did not land.
   */
  async function onHomePresetChange(id) {
    try {
      await applyPresetAndRefresh(id);
    } catch (err) {
      home.showError(`Could not switch instance: ${err?.message || err}`);
      await refreshHomePresets();
    }
  }

  /**
   * refreshHomePresets reads the saved instances and the active one and pushes
   * them to the header indicator. Called on startup, after any apply, and when a
   * "config" event says another seat changed things. It degrades quietly: a
   * build without the preset bindings (or a failure to read them) just leaves
   * the indicator as it was — the header must never show an error over a status
   * ornament.
   */
  async function refreshHomePresets() {
    if (!(backend.usingFakeBackend || backend.presetsAvailable())) return;
    try {
      const [list, active] = await Promise.all([backend.listPresets(), backend.getActivePreset()]);
      home.setPresets(Array.isArray(list) ? list : []);
      home.setActivePreset(active && active.id ? active.id : '');
    } catch (err) {
      console.info('wslcomms: could not refresh the header preset indicator', err?.message || err);
    }
  }

  // --- monitor -------------------------------------------------------------

  function safeMonitorCall(fn) {
    if (!monitor) return;
    try {
      const result = fn(monitor);
      if (result && typeof result.catch === 'function') {
        result.catch((err) => console.error('wslcomms: monitor call failed', err));
      }
    } catch (err) {
      console.error('wslcomms: monitor call threw', err);
    }
  }

  function setUpMonitor(config) {
    try {
      monitor = createMonitor({
        videoEl: home.videoEl,
        audioEl: home.audioEl,
        getCredentials: () => backend.getKVSCredentials(),
        tile: config.monitorTile,
        returnMid: config.returnMid,
        channelMode: normaliseChannelMode(config.returnChannel),
        // If the saved return source is SRT, the monitor's audio must be built
        // ALREADY SILENT. Building it audible and muting it a tick later is a
        // tick of both paths at once in somebody's ears.
        audioEnabled: currentReturnSource !== RETURN_SOURCE_SRT,
        // The saved headphone id, applied at construction. createReturnAudio
        // remembers it and re-applies it every time the element gets a stream,
        // which is the only way it survives the first attach — see note 2 at
        // the top of frontend/src/monitor/audio.js. Only ever the WebRTC field:
        // a WASAPI endpoint id passed to setSinkId does not fail, it plays on
        // the default device.
        sinkId: currentReturnSource === RETURN_SOURCE_SRT ? '' : selectedHeadphoneId(),
        gainDb: config.returnGainDb,
        // The PAGE owns the crop, not the monitor. Both implement it — the
        // monitor with inline pixel sizes and a scale factor, home.js with
        // percentages of the tile box — and running both is what put the
        // picture in the top-left corner of a large black rectangle: the
        // monitor's wrappers pinned the <video> at 2240x1440 inside a 640x360
        // crop box at scale 1, and nothing ever called setScale or fitTo, so
        // the picture stayed 640x360 however large the window was.
        //
        // The page's version is the one that can be responsive without being
        // driven, so it wins and the monitor is told to touch no layout. See
        // the "OPTING OUT" note at the top of frontend/src/monitor/video.js.
        manageVideoLayout: false,
      });
    } catch (err) {
      console.error('wslcomms: createMonitor threw — the monitor module is unavailable', err);
      monitor = null;
      home.lamps.MONITOR.update(deriveMonitorLamp('unavailable'));
      return;
    }

    try {
      monitor.on('state', (state) => home.lamps.MONITOR.update(deriveMonitorLamp(state)));
      monitor.on('error', (err) => {
        console.error('wslcomms: monitor error event', err);
        home.showError(`Monitor: ${err?.message || err}`);
      });
    } catch (err) {
      console.error('wslcomms: monitor.on() threw — MONITOR lamp will not update', err);
    }

    Promise.resolve()
      .then(() => monitor.start())
      .catch((err) => {
        console.error('wslcomms: monitor.start() failed', err);
        home.lamps.MONITOR.update(deriveMonitorLamp('failed'));
        home.showError(`Could not start the monitor: ${err?.message || err}`);
      });
  }

  window.addEventListener('beforeunload', () => {
    // The monitor is this page's own Web Audio graph and WebRTC connection, so
    // stopping it on unload is always correct — for the local window AND a
    // remote tab, each of which owns its own monitor.
    safeMonitorCall((m) => m.stop());

    // ===================== A REMOTE TAB MUST NOT DO THIS =======================
    //
    // stopReturn() and stopPicture() reach the HOST's native SRT return and
    // picture — the commentator's audio and the operator's picture. On the local
    // window that is the right cleanup on unload. On a REMOTE tab it would be a
    // closing browser somewhere on the LAN silencing the commentator and blacking
    // the operator's picture, which is the single highest-consequence hazard in
    // the whole remote-access plan.
    //
    // The shim already neutralises it by omission — StopReturn/StopPicture are
    // host-only and are simply not installed on a remote client, so these calls
    // land in backend.js as a handled BindingMissingError — but the guard makes
    // the intent explicit rather than emergent, so a future change to the shim's
    // method list cannot quietly re-arm it.
    if (backend.isRemoteClient()) return;

    // An SRT caller left dialled in holds one of M2L-X's four fan-out slots
    // after this window is gone. Best-effort — beforeunload cannot await — and
    // app.go's teardown stops it as well; this is the earlier of the two, not
    // the only one.
    //
    // UNCONDITIONAL (on the local window), not "only if the selected path is
    // SRT". StopReturn rejects when nothing was running and that rejection is
    // free, whereas the case this covers is not hypothetical: it is precisely a
    // page whose belief about what is running has come apart from the Go side
    // that leaves a monitor behind, and that is the page least able to tell.
    //
    // It does not go through returnPath, and does not need to: stopping can only
    // ever take audio away. The machine owns the two directions that can make
    // something audible.
    backend.stopReturn().catch(() => {});
    // And the picture receiver, for the same reason and with the same
    // best-effort caveat: it holds an M2L-X fan-out slot and a native window
    // that would otherwise outlive the page that asked for them.
    backend.stopPicture().catch(() => {});
  });

  // --- startup ---------------------------------------------------------

  (async function init() {
    try {
      currentConfig = await backend.getConfig();
    } catch (err) {
      home.showError(`Could not load configuration: ${err.message}`);
      currentConfig = {
        monitorTile: { x: 0, y: 360, w: 640, h: 360 },
        returnMid: 2,
        returnChannel: DEFAULT_CHANNEL_MODE,
        returnSource: DEFAULT_RETURN_SOURCE,
        returnGainDb: 18,
        audioDeviceId: '',
        headphoneDeviceId: '',
        headphoneEndpointId: '',
      };
    }

    home.setTile(currentConfig.monitorTile || { x: 0, y: 360, w: 640, h: 360 });
    home.setReturnMid(currentConfig.returnMid || 2);
    home.setReturnChannel(normaliseChannelMode(currentConfig.returnChannel));
    home.setLevel(1);

    // The header preset indicator, at startup: which instance is running, shown
    // at a glance. Fire-and-forget — it must not delay putting the commentator
    // on air — and it degrades quietly against a build without the presets.
    refreshHomePresets();

    // Whether the SRT PICTURE can be offered at all.
    //
    // Against the fake backend it IS offered, because the fake drives its own
    // state machine and the point of the fake is that the UI runs without Go.
    // Against a REAL Wails build the option is disabled unless all five
    // bindings are there, with the reason on the control: a build without them
    // is a real possibility while the native side lands, and an option that
    // always fails when pressed is worse than one that says why it cannot be.
    pictureBindingsPresent = backend.usingFakeBackend || backend.pictureAvailable();
    // The reason the SRT option is unavailable is DIFFERENT on a remote client,
    // and the difference matters. On the local build a missing option means the
    // native bindings are absent — a build problem. On a remote browser the
    // option is absent because it is host-only and pruned, and the true reason is
    // physics, not a build: the picture is a native window painted by the
    // commentary PC's GPU and cannot travel over the network. Saying "this build
    // has no native SRT picture" to a remote operator would send an engineer
    // hunting a build fault that is not there.
    const pictureUnavailableReason = backend.isRemoteClient()
      ? "The SRT picture is drawn by the commentary PC's graphics card and cannot be sent " +
        'over the network. You are watching the multiviewer mosaic; audio is unaffected.'
      : 'This build has no native SRT picture. You are watching the multiviewer mosaic; ' +
        'your audio is unaffected either way.';
    home.setPictureAvailable(
      pictureBindingsPresent,
      pictureBindingsPresent ? '' : pictureUnavailableReason,
    );

    // ================= THE AUDIO PATH IS NOW FIXED AT KINESIS =================
    //
    // It is not read from the saved config any more, and this is not a
    // simplification — it is the correction.
    //
    // The native SRT AUDIO return still exists on the Go side and is deliberately
    // kept there as a capability. What has gone is the UI offering it, because
    // the control that did so was a misreading of what was asked for: SRT is the
    // PICTURE and Kinesis is the AUDIO. A config file written by the previous
    // revision can still hold returnSource: "srt", and left alone it would
    // silence the commentator on every launch with no control on screen to undo
    // it — which is the failure this whole change removes, arriving by the one
    // route that outlives a rebuild.
    //
    // So the saved value is overruled, once, and written back so the next launch
    // does not have to do it again.
    currentReturnSource = RETURN_SOURCE_WEBRTC;
    const savedAudioSource = currentConfig.returnSource;

    await Promise.all([loadInputDevices(), loadHeadphoneDevices(), loadOutputDevices()]);
    renderHeadphoneList();

    // The monitor is built BEFORE the SRT return is started, and built silent
    // if SRT is the saved path, so there is no window in which both are live.
    setUpMonitor(currentConfig);

    // Put the commentator on Kinesis, through the one machine that is allowed to
    // make a path audible.
    //
    // adoptSaved is NOT a no-op on the WebRTC side and never was: it asks the Go
    // side to stop before it un-mutes, because "this page is on WebRTC" does not
    // prove nothing else is running. A page that reloaded after a failed switch —
    // or one whose beforeunload stopReturn() did not complete before the context
    // died — can find an orphaned SRT audio monitor playing into the same
    // headphones, and nothing else in the application would ever stop it. That
    // matters MORE now, not less: the UI no longer has a control that would stop
    // one, so this is the only thing that ever will.
    const adopted = await returnPath.adoptSaved(RETURN_SOURCE_WEBRTC);
    afterReturnOperation(adopted);

    // Written back only if it was wrong, so that a config already correct is not
    // rewritten on every launch. The audio is already right by this point; this
    // is the record of it catching up.
    if (savedAudioSource !== undefined && savedAudioSource !== RETURN_SOURCE_WEBRTC) {
      console.info(
        `wslcomms: the saved audio return source was "${savedAudioSource}". Audio now always ` +
          'comes from Kinesis — SRT is the picture — so it has been corrected to webrtc.',
      );
      persistConfig({ returnSource: RETURN_SOURCE_WEBRTC });
    }

    // --- and now the picture -------------------------------------------------
    //
    // LAST, and deliberately so. Everything above is audio, and the commentator
    // being able to hear the match outranks the commentator being able to see it
    // at full resolution — the mosaic is on screen throughout all of this.
    currentPictureSource = pictureBindingsPresent
      ? normalisePictureSource(currentConfig[PICTURE_SOURCE_KEY])
      : PICTURE_SOURCE_MOSAIC;

    // Begin reporting the rectangle before anything is started, so the native
    // window is never painted at a default position it was not told to be at.
    //
    // showHome releases BLOCK_HIDDEN, which the overlay is constructed holding.
    // It starts blocked on purpose: an overlay that has not yet been told where
    // to be, painted wherever a native default puts it, is an opaque box over an
    // application somebody is trying to read.
    showHome();
    watchPictureRect();
    renderPicture();

    if (currentPictureSource === PICTURE_SOURCE_SRT) {
      try {
        await backend.startPicture();
        currentPictureState = await backend.getPictureState();
      } catch (err) {
        // The mosaic is already on screen and the audio is already up. This is a
        // degraded picture, not an outage, and it is said once in the banner
        // because the operator asked for the good one and is not getting it.
        currentPictureState = null;
        home.showError(
          `The high-quality SRT picture did not start: ${err?.message || err}. ` +
            'You are watching the multiviewer mosaic; your audio is unaffected.',
        );
      }
      renderPicture();
    }
  })();
}
