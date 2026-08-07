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
  /** 'webrtc' | 'srt'. The path currently feeding the commentator's ears. */
  let currentReturnSource = DEFAULT_RETURN_SOURCE;
  /** Guards against two overlapping source switches leaving both paths on. */
  let sourceSwitchInFlight = false;

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
      // means restarting the monitor — and StartReturn reads the endpoint out of
      // the SAVED config, so the save has to land first or the return comes back
      // up on the old endpoint and says it succeeded.
      //
      // Stop before start: two monitors dialled into the same M2L-X output would
      // hold two of its four fan-out slots.
      (async () => {
        await persistConfigAndWait({ [effects.deviceKey]: deviceId });
        await backend.stopReturn().catch((err) => {
          console.info('wslcomms: nothing to stop before moving the SRT return:', err.message);
        });
        await backend.startReturn();
      })().catch((err) => home.showError(`Could not move the SRT return: ${err.message}`));
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
    (async () => {
      await persistConfigAndWait({ returnChannel: channel });
      await backend.stopReturn().catch((err) => {
        console.info('wslcomms: nothing to stop before rebuilding the SRT return:', err.message);
      });
      await backend.startReturn();
    })().catch((err) => home.showError(`Could not change the SRT return channel: ${err.message}`));
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
    if (next === currentReturnSource || sourceSwitchInFlight) {
      home.setReturnSource(currentReturnSource);
      return;
    }
    sourceSwitchInFlight = true;
    const previous = currentReturnSource;
    home.clearError();

    try {
      const effects = deriveReturnSourceEffects(next);

      // 1. Silence whatever is audible now.
      if (!effects.webrtcAudible) safeMonitorCall((m) => m.setAudioEnabled(false));
      if (!effects.srtRunning) {
        await backend.stopReturn().catch((err) => {
          // StopReturn rejects when nothing was running, which is the normal
          // case here and not a failure. A stop that fails for a real reason is
          // logged but must not abort the switch either: the alternative is
          // refusing to leave SRT, which strands the commentator on a path that
          // is already not working.
          console.info('wslcomms: stopping the SRT return:', err.message);
        });
      }

      // 2. Adopt the new path and SAVE IT. StartReturn reads returnSource out
      //    of the saved config and refuses unless it is already "srt", so this
      //    save is not bookkeeping — it is a precondition of step 3.
      currentReturnSource = next;
      await persistConfigAndWait({ returnSource: next });
      home.setReturnSource(next);
      renderHeadphoneList();

      // 3. Start the incoming path.
      if (effects.srtRunning) {
        await backend.startReturn();
        home.setSRTReturnState(await backend.getReturnState());
      } else {
        // Asked, not assumed. App.IsSRTReturnSelected is the single place that
        // decides which path owns the headphones; comparing returnSource to a
        // string literal here would put the same decision in two languages, and
        // the failure of the two disagreeing is both paths playing at once.
        const srtOwnsHeadphones = await backend.isSRTReturnSelected();
        safeMonitorCall((m) => m.setAudioEnabled(!srtOwnsHeadphones));
        safeMonitorCall((m) => m.setSinkId(selectedHeadphoneId(next)));
      }
    } catch (err) {
      // The new path did not start. Go back to the old one rather than leaving
      // the commentator with nothing: the previous path is known to have been
      // working a moment ago.
      home.showError(
        `Could not switch the return to ${next.toUpperCase()}: ${err.message}. ` +
          `Staying on ${previous.toUpperCase()}.`,
      );
      currentReturnSource = previous;
      home.setReturnSource(previous);
      renderHeadphoneList();
      try {
        await persistConfigAndWait({ returnSource: previous });
        if (previous === RETURN_SOURCE_SRT) await backend.startReturn();
        else safeMonitorCall((m) => m.setAudioEnabled(true));
      } catch (e) {
        console.error('wslcomms: could not restore the previous return path', e);
        // Last resort. If even the restore failed, the WebRTC path is the one
        // that needs no backend at all, so it is the one to leave audible
        // rather than leaving the commentator in silence.
        safeMonitorCall((m) => m.setAudioEnabled(true));
      }
    } finally {
      sourceSwitchInFlight = false;
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
    // only when there is something to stop, because StopReturn rejects
    // otherwise. app.go's teardown stops it as well; this is the earlier of the
    // two, not the only one.
    if (currentReturnSource === RETURN_SOURCE_SRT) {
      backend.stopReturn().catch(() => {});
    }
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
    const savedSource = normaliseReturnSource(currentConfig.returnSource);
    currentReturnSource =
      savedSource === RETURN_SOURCE_SRT && !srtAvailable ? DEFAULT_RETURN_SOURCE : savedSource;
    home.setReturnSource(currentReturnSource);

    await Promise.all([loadInputDevices(), loadHeadphoneDevices(), loadOutputDevices()]);
    renderHeadphoneList();

    // The monitor is built BEFORE the SRT return is started, and built silent
    // if SRT is the saved path, so there is no window in which both are live.
    setUpMonitor(currentConfig);

    if (currentReturnSource === RETURN_SOURCE_SRT) {
      try {
        // No save needed: currentReturnSource came from the saved config, so
        // what StartReturn is about to read is already what is on disk.
        await backend.startReturn();
        home.setSRTReturnState(await backend.getReturnState());
      } catch (err) {
        // Falling back to WebRTC rather than leaving the commentator in silence
        // on a path that did not start.
        home.showError(
          `The SRT return did not start: ${err.message}. Falling back to the WebRTC return.`,
        );
        currentReturnSource = DEFAULT_RETURN_SOURCE;
        // Saved, so the Go side's own view of which path owns the headphones
        // agrees with what is actually playing.
        persistConfig({ returnSource: DEFAULT_RETURN_SOURCE });
        home.setReturnSource(currentReturnSource);
        renderHeadphoneList();
        safeMonitorCall((m) => m.setAudioEnabled(true));
      }
    }
  })();
}
