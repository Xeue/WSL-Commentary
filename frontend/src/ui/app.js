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
  DEFAULT_RETURN_SOURCE,
  normaliseReturnSource,
  deriveReturnSourceEffects,
} from './returnsource.js';
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
  });

  const home = createHomeView({
    onSettings: showSettings,
    onMixer: () => mixerHost.toggle(),
    onStartStop: onStartStopClick,
    onInputChange: onInputChange,
    onHeadphoneChange: onHeadphoneChange,
    onReturnChange: onReturnChange,
    onReturnChannelChange: onReturnChannelChange,
    onReturnSourceChange: onReturnSourceChange,
    onLevelChange: onLevelChange,
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

  const mixerHost = createMixerHost({
    mount: mixerMount,
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
    onSource: (source) => {
      currentReturnSource = source;
      home.setReturnSource(source);
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
    home.el.hidden = true;
    settings.el.hidden = false;
    settings.open();
  }

  function showHome() {
    settings.el.hidden = true;
    home.el.hidden = false;
  }

  // --- lamps -------------------------------------------------------------

  function renderSenderLamp() {
    home.lamps.SENDING.update(deriveSenderLamp(currentSenderState));
    home.setRunning(!!currentSenderState && currentSenderState !== backend.SENDER_STATE.STOPPED);
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

  // The native return's own state. Rendered only while SRT is the selected
  // path: a status line for a receiver nobody asked for reads as a fault in the
  // return that IS running.
  backend.onReturn((state) => {
    if (currentReturnSource !== RETURN_SOURCE_SRT) return;
    home.setSRTReturnState(state);
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

  /**
   * onReturnSourceChange is the ONE place that switches audio paths, and the
   * ordering in it is the whole safety property:
   *
   *   SILENCE THE OUTGOING PATH FIRST, THEN START THE INCOMING ONE.
   *
   * Never the other way round. Both paths carry the same programme at different
   * offsets, so an overlap — even a few hundred milliseconds of one while the
   * other spins up — is a slapback echo in the ears of somebody talking over
   * it. Failing to start the new path leaves the commentator in silence, which
   * is recoverable and visible; starting it before the old one stops is not.
   *
   * The PICTURE is untouched throughout. It rides the WebRTC peer connection
   * whichever audio path is selected, and setAudioEnabled(false) severs the Web
   * Audio graph only.
   */
  async function onReturnSourceChange(source) {
    const next = normaliseReturnSource(source);
    home.clearError();

    const result = await returnPath.select(next);
    afterReturnOperation(result, () => home.setReturnSource(returnPath.source));

    // The control follows the machine, not the click. On every path — applied,
    // refused, or recovered onto WebRTC after a failed start — onSource has
    // already put the segmented control and the Headphones list where the audio
    // actually is, and this only fills in the two things the machine cannot
    // know about.
    if (returnPath.source === RETURN_SOURCE_SRT) {
      // The status line for a return that is now running. It is a separate call
      // because it can fail on its own — a build without GetReturnState — and a
      // failure to READ the state must not be mistaken for a failure to start.
      try {
        home.setSRTReturnState(await backend.getReturnState());
      } catch (err) {
        console.info('wslcomms: the SRT return state could not be read:', err.message);
      }
    } else {
      // Asked, not assumed. App.IsSRTReturnSelected is the single place that
      // decides which path owns the headphones; comparing returnSource to a
      // string literal here would put the same decision in two languages.
      //
      // It is a CHECK, not the mechanism: returnPath has already stopped the SRT
      // return and un-muted WebRTC in that order. If Go disagrees with what just
      // happened, that is worth a line in the console and is not a reason to
      // re-mute a commentator who is listening.
      try {
        if (await backend.isSRTReturnSelected()) {
          console.warn(
            'wslcomms: the Go side still reports the SRT return as selected after a switch to WebRTC',
          );
        }
      } catch (err) {
        console.info('wslcomms: could not confirm which path owns the headphones:', err.message);
      }
      safeMonitorCall((m) => m.setSinkId(selectedHeadphoneId(returnPath.source)));
    }
  }

  function onLevelChange(fraction) {
    safeMonitorCall((m) => m.setLevel(fraction));
  }

  function onConfigSaved(config) {
    currentConfig = config;
    home.setTile(config.monitorTile);
    home.setReturnMid(config.returnMid);
    home.setInputDevices(currentInputDevices, config.audioDeviceId);
    // The RETURN SOURCE is deliberately not re-applied from a Settings save.
    // Settings has no control for it; taking it from the saved config here
    // would silently switch a commentator's audio path because somebody
    // pressed Save on an unrelated screen. The channel is safe to re-apply
    // because it cannot silence anything.
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
    showHome();

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
    safeMonitorCall((m) => m.stop());
    // An SRT caller left dialled in holds one of M2L-X's four fan-out slots
    // after this window is gone. Best-effort — beforeunload cannot await — and
    // app.go's teardown stops it as well; this is the earlier of the two, not
    // the only one.
    //
    // UNCONDITIONAL, not "only if the selected path is SRT". StopReturn rejects
    // when nothing was running and that rejection is free, whereas the case this
    // covers is not hypothetical: it is precisely a page whose belief about
    // what is running has come apart from the Go side that leaves a monitor
    // behind, and that is the page least able to tell.
    //
    // It does not go through returnPath, and does not need to: stopping can only
    // ever take audio away. The machine owns the two directions that can make
    // something audible.
    backend.stopReturn().catch(() => {});
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

    // Whether the SRT option can be offered at all.
    //
    // Against the fake backend it IS offered, because the fake drives its own
    // state machine and the point of the fake is that the UI runs without Go —
    // and the fake says "fake backend" in every payload it emits, so nothing on
    // screen claims audio is arriving. Against a REAL Wails build the option is
    // disabled unless the bindings are actually there, with the reason on the
    // control: a build without them is a real possibility while the native side
    // lands, and an option that always fails when pressed is worse than one
    // that says why it cannot be pressed.
    const srtAvailable = backend.usingFakeBackend || backend.srtReturnAvailable();
    home.setSRTAvailable(
      srtAvailable,
      srtAvailable
        ? ''
        : 'This build has no native SRT return. The WebRTC return is unaffected.',
    );

    // The saved path, honoured — but never a path this build cannot run.
    //
    // This is the one assignment to currentReturnSource outside onSource, and it
    // has to be here: createMonitor below is told at CONSTRUCTION whether to
    // build its audio silent, and returnPath.adoptSaved — which is what sets the
    // mirror everywhere else — cannot run before the monitor it drives exists.
    // adoptSaved re-establishes it a few lines later through the same callback
    // every other transition uses.
    const savedSource = normaliseReturnSource(currentConfig.returnSource);
    currentReturnSource =
      savedSource === RETURN_SOURCE_SRT && !srtAvailable ? DEFAULT_RETURN_SOURCE : savedSource;
    home.setReturnSource(currentReturnSource);

    await Promise.all([loadInputDevices(), loadHeadphoneDevices(), loadOutputDevices()]);
    renderHeadphoneList();

    // The monitor is built BEFORE the SRT return is started, and built silent
    // if SRT is the saved path, so there is no window in which both are live.
    setUpMonitor(currentConfig);

    // Bring the saved path into force.
    //
    // This runs even when the saved path is WEBRTC, and it is not a no-op then:
    // adoptSaved asks the Go side to stop before it un-mutes, because "the saved
    // path is WebRTC" does not prove nothing is running. A page that reloaded
    // after a failed switch — or one whose beforeunload stopReturn() did not
    // complete before the context died — can find an orphaned monitor playing
    // into the same headphones, and nothing else in the application would ever
    // stop it. The cost of asking is one StopReturn that usually rejects with
    // errReturnNotRunning; the cost of assuming is a commentator hearing the
    // match twice from the moment the page loads.
    //
    // No save is needed for the SRT case: currentReturnSource came from the
    // saved config, so what StartReturn is about to read is already on disk.
    const adopted = await returnPath.adoptSaved(currentReturnSource);
    afterReturnOperation(adopted);

    if (returnPath.source === RETURN_SOURCE_SRT) {
      try {
        home.setSRTReturnState(await backend.getReturnState());
      } catch (err) {
        // A build with StartReturn but no GetReturnState. The return IS running;
        // only its status line is missing, and this is the throw that used to
        // land in the recovery above and un-mute WebRTC over the top of it.
        console.info('wslcomms: the SRT return state could not be read:', err.message);
      }
    }
  })();
}
