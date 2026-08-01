import * as backend from './backend.js';
import { deriveSenderLamp, deriveStatusLamps, deriveMonitorLamp } from './lamps.js';
import { createHomeView } from './home.js';
import { createSettingsView } from './settings.js';
// The mixer host. Imported STATICALLY: it is the only module that imports
// ./mixer/index.js, which is the only module that imports mixer.css, so this
// line is what puts the whole drawer and its stylesheet in the bundle. A
// dynamic import would build and would ship nothing until a code path ran.
import { createMixerHost } from './mixerhost.js';

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
  let currentHeadphoneDevices = [];

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
    // The honest line's claim is derived from the same state as the lamp, so
    // the two can never disagree about whether anything is being sent.
    home.setSenderState(currentSenderState);
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

  async function loadHeadphoneDevices() {
    if (!navigator.mediaDevices?.enumerateDevices) {
      home.setHeadphoneDevices([], null);
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
      home.setHeadphoneDevices(currentHeadphoneDevices, currentConfig?.headphoneDeviceId);
    } catch (err) {
      currentHeadphoneDevices = [];
      home.setHeadphoneDevices([], null);
      home.showError(`Could not list headphone devices: ${err.message}`);
    }
  }

  if (navigator.mediaDevices) {
    navigator.mediaDevices.ondevicechange = () => {
      loadHeadphoneDevices();
    };
  }

  function persistConfig(patch) {
    if (!currentConfig) return;
    currentConfig = { ...currentConfig, ...patch };
    backend.saveConfig(currentConfig).catch((err) => {
      home.showError(`Could not save configuration: ${err.message}`);
    });
  }

  function onInputChange(deviceId) {
    persistConfig({ audioDeviceId: deviceId });
  }

  function onHeadphoneChange(deviceId) {
    persistConfig({ headphoneDeviceId: deviceId });
    safeMonitorCall((m) => m.setSinkId(deviceId));
  }

  function onReturnChange(mid) {
    persistConfig({ returnMid: mid });
    safeMonitorCall((m) => m.setReturnMid(mid));
  }

  function onLevelChange(fraction) {
    safeMonitorCall((m) => m.setLevel(fraction));
  }

  function onConfigSaved(config) {
    currentConfig = config;
    home.setTile(config.monitorTile);
    home.setReturnMid(config.returnMid);
    home.setInputDevices(currentInputDevices, config.audioDeviceId);
    home.setHeadphoneDevices(currentHeadphoneDevices, config.headphoneDeviceId);
    safeMonitorCall((m) => {
      m.setReturnMid(config.returnMid);
      m.setGainDb(config.returnGainDb);
      if (config.headphoneDeviceId) m.setSinkId(config.headphoneDeviceId);
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
        returnGainDb: 18,
        audioDeviceId: '',
        headphoneDeviceId: '',
      };
    }

    home.setTile(currentConfig.monitorTile || { x: 0, y: 360, w: 640, h: 360 });
    home.setReturnMid(currentConfig.returnMid || 2);
    home.setLevel(1);

    await Promise.all([loadInputDevices(), loadHeadphoneDevices()]);

    setUpMonitor(currentConfig);
  })();
}
