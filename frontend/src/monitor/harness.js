/**
 * Driver for harness.html.
 *
 * Owner: WP-5a. Not part of the shipped app — Vite builds index.html into
 * frontend/dist and nothing here is reachable from it.
 *
 * Two transports, both driving the same real modules:
 *
 *   loopback — a synthetic mosaic and seven synthetic buses published from a
 *              local "master" RTCPeerConnection, consumed through the real
 *              eight-transceiver plan in peer.js and the real video.js /
 *              audio.js. Needs nothing but a browser.
 *
 *   live     — the real createMonitor() against a real KVS channel, with the
 *              credentials form standing in for the Wails-bound
 *              GetKVSCredentials.
 *
 * See the comment at the top of harness.html for how to open it.
 */

import { createMonitor } from './monitor.js';
import { createMosaicView } from './video.js';
import { createReturnAudio } from './audio.js';
import { createViewerPeerConnection, createViewerOffer, applyAnswer, closePeerConnection } from './peer.js';
import { BUS_MAP, VIDEO_MID, AUDIO_MIDS, DEFAULT_RETURN_MID } from './buses.js';
import { DEFAULT_TILE } from './geometry.js';
import { DEFAULT_GAIN_DB } from './gain.js';
import { kvsSdkVersion } from './kvs-sdk.js';
import { createFakeMosaic, createFakeBuses, BUS_TONES } from './fake-source.js';
import {
  SAMPLE_CREDENTIALS,
  createFormCredentialProvider,
  createFailingCredentialProvider,
  createFlakyCredentialProvider,
  loadStoredCredentials,
  storeCredentials,
  clearStoredCredentials,
} from './fake-credentials.js';

const $ = (id) => document.getElementById(id);

const el = {
  video: $('video'),
  audio: $('audio'),
  stageHost: $('stageHost'),
  mode: $('mode'),
  start: $('start'),
  stop: $('stop'),
  lamp: $('lamp'),
  lampText: $('lampText'),
  scale: $('scale'),
  scaleReadout: $('scaleReadout'),
  fitWidth: $('fitWidth'),
  tileX: $('tileX'),
  tileY: $('tileY'),
  tileW: $('tileW'),
  tileH: $('tileH'),
  applyTile: $('applyTile'),
  tilePgm: $('tilePgm'),
  tilePvw: $('tilePvw'),
  tileAll: $('tileAll'),
  returnMid: $('returnMid'),
  gainDb: $('gainDb'),
  level: $('level'),
  levelReadout: $('levelReadout'),
  sink: $('sink'),
  refreshDevices: $('refreshDevices'),
  grantLabels: $('grantLabels'),
  resumeAudio: $('resumeAudio'),
  liveOnly: $('liveOnly'),
  creds: $('creds'),
  saveCreds: $('saveCreds'),
  clearCreds: $('clearCreds'),
  failMode: $('failMode'),
  log: $('log'),
  clearLog: $('clearLog'),
  diag: $('diag'),
};

// ---------------------------------------------------------------- logging --

function log(kind, ...parts) {
  const line = document.createElement('div');
  line.className = kind;
  const t = new Date().toISOString().slice(11, 23);
  line.textContent = `${t}  ${parts.map((p) => (typeof p === 'string' ? p : String(p))).join(' ')}`;
  el.log.appendChild(line);
  el.log.scrollTop = el.log.scrollHeight;
  while (el.log.childElementCount > 800) el.log.removeChild(el.log.firstChild);
}

const LAMP_CLASS = {
  connected: 'good',
  connecting: 'warn',
  new: 'warn',
  disconnected: 'warn',
  failed: 'bad',
  closed: '',
};

function setLamp(state) {
  el.lamp.className = `lamp ${LAMP_CLASS[state] || ''}`;
  el.lampText.textContent = state;
}

// ------------------------------------------------------------ the session --

/**
 * Whatever transport is running. Both shapes expose the same handful of
 * methods so the controls below do not care which is which.
 * @type {null | {kind: string, stop: () => Promise<void>, setReturnMid: Function,
 *                setGainDb: Function, setLevel: Function, setSinkId: Function,
 *                setScale: Function, fitTo: Function, setTile: Function,
 *                getLayout: Function, resumeAudio: Function, getState: Function}}
 */
let session = null;

let currentTile = { ...DEFAULT_TILE };
let currentReturnMid = DEFAULT_RETURN_MID;
let currentGainDb = DEFAULT_GAIN_DB;
let currentLevel = 0.5;
let currentSinkId = '';
let currentScale = 1;

// ----------------------------------------------------------- loopback rig --

/**
 * startLoopback builds a local master/viewer pair and wires the real monitor
 * modules to it.
 *
 * The master offers, in this exact order, one video track and seven audio
 * tracks — the same shape M2L-X's media server offers — so the positional
 * mid-to-bus map in buses.js is exercised for real rather than assumed.
 */
async function startLoopback() {
  const mosaic = createFakeMosaic();
  const buses = createFakeBuses();

  const view = createMosaicView({
    videoEl: el.video,
    tile: currentTile,
    onError: (err) => log('err', 'video:', err.message),
  });
  view.setScale(currentScale);

  const audio = createReturnAudio({
    audioEl: el.audio,
    gainDb: currentGainDb,
    level: currentLevel,
    sinkId: currentSinkId,
    onError: (err) => log('err', `${err.code || 'ERROR'}: ${err.message}`),
  });

  // The viewer is the real thing: createViewerPeerConnection adds the eight
  // recvonly transceivers in plan order. No ICE servers — host candidates are
  // all a loopback needs, which is also what makes this work offline.
  const { pc: viewer, transceivers } = createViewerPeerConnection([]);

  const master = new RTCPeerConnection();
  master.addTransceiver(mosaic.track, { direction: 'sendonly' });
  for (const track of buses.tracks) {
    master.addTransceiver(track, { direction: 'sendonly' });
  }

  /** @type {Map<number, MediaStreamTrack>} */
  const tracks = new Map();

  viewer.ontrack = (ev) => {
    const idx = transceivers.indexOf(ev.transceiver);
    if (idx < 0) {
      log('err', 'a track arrived on a transceiver we did not offer');
      return;
    }
    const bus = BUS_MAP[idx];
    log('ok', `track on mid ${idx} (${bus ? bus.bus : '?'})`);
    if (idx === VIDEO_MID) {
      view.attach(new MediaStream([ev.track]));
      return;
    }
    tracks.set(idx, ev.track);
    if (idx === currentReturnMid) audio.attach(ev.track);
  };

  viewer.onicecandidate = (ev) => {
    if (ev.candidate) master.addIceCandidate(ev.candidate).catch(() => {});
  };
  master.onicecandidate = (ev) => {
    if (ev.candidate) viewer.addIceCandidate(ev.candidate).catch(() => {});
  };
  viewer.onconnectionstatechange = () => {
    setLamp(viewer.connectionState);
    log('st', 'state:', viewer.connectionState);
  };

  setLamp('new');
  log('st', 'state: new');

  const offer = await createViewerOffer(viewer);
  await master.setRemoteDescription(offer);
  const answer = await master.createAnswer();
  await master.setLocalDescription(answer);
  const problems = await applyAnswer(viewer, transceivers, master.localDescription);
  if (problems.length > 0) log('err', 'mid re-map:', problems.join(' | '));
  else log('ok', 'the answer kept the offered mid order; the bus map holds');

  const routeReturn = () => {
    const track = tracks.get(currentReturnMid);
    if (track) audio.attach(track);
  };

  return {
    kind: 'loopback',
    async stop() {
      viewer.ontrack = null;
      closePeerConnection(viewer);
      closePeerConnection(master);
      await audio.close();
      view.destroy();
      mosaic.stop();
      await buses.stop();
      tracks.clear();
      setLamp('closed');
    },
    setReturnMid(n) {
      currentReturnMid = n;
      routeReturn();
    },
    setGainDb: (db) => audio.setGainDb(db),
    setLevel: (l) => audio.setLevel(l),
    setSinkId: (id) => audio.setSinkId(id),
    setScale: (k) => view.setScale(k),
    fitTo: (w, h) => view.fitTo(w, h),
    setTile: (t) => view.setTile(t),
    getLayout: () => view.getLayout(),
    resumeAudio: () => audio.resume(),
    getState: () => ({
      transport: 'loopback',
      connectionState: viewer.connectionState,
      iceConnectionState: viewer.iceConnectionState,
      tracks: [...tracks.keys()].sort((a, b) => a - b),
      returnMid: currentReturnMid,
      audio: audio.getState(),
      layout: view.getLayout(),
    }),
  };
}

// --------------------------------------------------------- live KVS rig ----

/** buildCredentialProvider turns the form plus the fault-injection dropdown into a provider. */
function buildCredentialProvider() {
  const readForm = () => {
    const text = el.creds.value.trim();
    if (!text) throw new Error('no credentials in the form');
    return JSON.parse(text);
  };

  switch (el.failMode.value) {
    case 'always':
      return createFailingCredentialProvider('simulated: M2L-X is not reachable');
    case 'flaky3':
      return createFlakyCredentialProvider(createFormCredentialProvider(readForm), 3);
    case 'garbage':
      return async () => ({ region: '', channelName: '', accessKeyId: '' });
    default:
      return createFormCredentialProvider(readForm);
  }
}

/** startLive runs the real createMonitor. */
async function startLive() {
  const monitor = createMonitor({
    videoEl: el.video,
    audioEl: el.audio,
    getCredentials: buildCredentialProvider(),
    tile: currentTile,
    returnMid: currentReturnMid,
    gainDb: currentGainDb,
    level: currentLevel,
    sinkId: currentSinkId,
  });

  monitor.on('state', (s) => {
    setLamp(s);
    log('st', 'state:', s);
  });
  monitor.on('error', (err) => {
    log('err', `${err.code || 'ERROR'}: ${err.message}`);
  });

  monitor.setScale(currentScale);
  await monitor.start();

  return {
    kind: 'live',
    stop: () => monitor.destroy(),
    setReturnMid: (n) => monitor.setReturnMid(n),
    setGainDb: (db) => monitor.setGainDb(db),
    setLevel: (l) => monitor.setLevel(l),
    setSinkId: (id) => monitor.setSinkId(id),
    setScale: (k) => monitor.setScale(k),
    fitTo: (w, h) => monitor.fitTo(w, h),
    setTile: (t) => monitor.setTile(t),
    getLayout: () => monitor.getLayout(),
    resumeAudio: () => monitor.resumeAudio(),
    getState: () => ({ transport: 'live', ...monitor.getState() }),
  };
}

// --------------------------------------------------------------- controls --

async function start() {
  if (session) return;
  el.start.disabled = true;
  try {
    session = el.mode.value === 'live' ? await startLive() : await startLoopback();
    el.stop.disabled = false;
    log('ok', `${session.kind} transport started`);
  } catch (err) {
    log('err', `could not start: ${err && err.message ? err.message : err}`);
    el.start.disabled = false;
    session = null;
  }
}

async function stop() {
  if (!session) return;
  el.stop.disabled = true;
  const s = session;
  session = null;
  try {
    await s.stop();
    log('ok', 'stopped');
  } catch (err) {
    log('err', `stop failed: ${err && err.message ? err.message : err}`);
  }
  el.start.disabled = false;
  setLamp('closed');
}

function readTile() {
  return {
    x: Number(el.tileX.value),
    y: Number(el.tileY.value),
    w: Number(el.tileW.value),
    h: Number(el.tileH.value),
  };
}

function writeTile(t) {
  el.tileX.value = String(t.x);
  el.tileY.value = String(t.y);
  el.tileW.value = String(t.w);
  el.tileH.value = String(t.h);
}

function applyTile(t) {
  currentTile = t;
  writeTile(t);
  if (session) session.setTile(t);
}

async function refreshDevices() {
  el.sink.innerHTML = '';
  const dflt = document.createElement('option');
  dflt.value = '';
  dflt.textContent = 'System default';
  el.sink.appendChild(dflt);

  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    log('err', 'this browser has no enumerateDevices');
    return;
  }
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    const outputs = devices.filter((d) => d.kind === 'audiooutput');
    for (const d of outputs) {
      const opt = document.createElement('option');
      opt.value = d.deviceId;
      opt.textContent = d.label || `(unlabelled output ${d.deviceId.slice(0, 8)}…)`;
      el.sink.appendChild(opt);
    }
    log('ok', `${outputs.length} audio output device(s)`);
    if (outputs.some((d) => !d.label)) {
      log('err', 'labels are blank — grant microphone permission to see them');
    }
  } catch (err) {
    log('err', `enumerateDevices failed: ${err.message}`);
  }
  el.sink.value = currentSinkId;
}

function refreshDiag() {
  const bits = [`KVS WebRTC SDK ${kvsSdkVersion()}`];
  if (session) {
    try {
      bits.push(JSON.stringify(session.getState(), null, 2));
    } catch (err) {
      bits.push(`getState failed: ${err.message}`);
    }
  } else {
    bits.push('not started');
  }
  el.diag.textContent = bits.join('\n');
}

// ------------------------------------------------------------------- wire --

for (const bus of BUS_MAP) {
  if (!AUDIO_MIDS.includes(bus.mid)) continue;
  const tone = BUS_TONES.find((t) => t.mid === bus.mid);
  const opt = document.createElement('option');
  opt.value = String(bus.mid);
  opt.textContent = `mid ${bus.mid} — ${bus.label} (${bus.bus})${tone ? ` · ${tone.hz.toFixed(0)} Hz in loopback` : ''}`;
  el.returnMid.appendChild(opt);
}
el.returnMid.value = String(currentReturnMid);
el.gainDb.value = String(currentGainDb);
el.level.value = String(currentLevel);
el.levelReadout.textContent = currentLevel.toFixed(2);
writeTile(currentTile);
el.creds.value = JSON.stringify(loadStoredCredentials() || SAMPLE_CREDENTIALS, null, 2);

el.start.addEventListener('click', start);
el.stop.addEventListener('click', stop);

el.mode.addEventListener('change', () => {
  el.liveOnly.style.display = el.mode.value === 'live' ? '' : 'none';
});
el.liveOnly.style.display = 'none';

el.returnMid.addEventListener('change', () => {
  currentReturnMid = Number(el.returnMid.value);
  if (session) session.setReturnMid(currentReturnMid);
  log('ok', `return -> mid ${currentReturnMid}`);
});

el.gainDb.addEventListener('change', () => {
  currentGainDb = Number(el.gainDb.value);
  if (session) session.setGainDb(currentGainDb);
});

el.level.addEventListener('input', () => {
  currentLevel = Number(el.level.value);
  el.levelReadout.textContent = currentLevel.toFixed(2);
  if (session) session.setLevel(currentLevel);
});

el.sink.addEventListener('change', async () => {
  currentSinkId = el.sink.value;
  if (session) await session.setSinkId(currentSinkId);
  log('ok', `sink -> ${currentSinkId || 'system default'}`);
});

el.refreshDevices.addEventListener('click', refreshDevices);

el.grantLabels.addEventListener('click', async () => {
  try {
    const s = await navigator.mediaDevices.getUserMedia({ audio: true });
    for (const t of s.getTracks()) t.stop();
    log('ok', 'microphone permission granted; device labels should appear');
  } catch (err) {
    log('err', `getUserMedia failed: ${err.message}`);
  }
  await refreshDevices();
});

el.resumeAudio.addEventListener('click', async () => {
  if (!session) return;
  await session.resumeAudio();
  log('ok', 'resume requested');
});

el.scale.addEventListener('input', () => {
  currentScale = Number(el.scale.value);
  if (session) currentScale = session.setScale(currentScale);
  el.scaleReadout.textContent = `${currentScale.toFixed(2)}x`;
});

el.fitWidth.addEventListener('click', () => {
  const available = el.stageHost.parentElement.clientWidth - 4;
  if (session) currentScale = session.fitTo(available);
  el.scale.value = String(currentScale);
  el.scaleReadout.textContent = `${currentScale.toFixed(2)}x`;
});

el.applyTile.addEventListener('click', () => applyTile(readTile()));
el.tilePgm.addEventListener('click', () => applyTile({ x: 0, y: 360, w: 640, h: 360 }));
el.tilePvw.addEventListener('click', () => applyTile({ x: 0, y: 0, w: 640, h: 360 }));
el.tileAll.addEventListener('click', () => applyTile({ x: 0, y: 0, w: 2240, h: 1440 }));

el.saveCreds.addEventListener('click', () => {
  try {
    storeCredentials(JSON.parse(el.creds.value));
    log('ok', 'credentials remembered in localStorage');
  } catch (err) {
    log('err', `not valid JSON: ${err.message}`);
  }
});
el.clearCreds.addEventListener('click', () => {
  clearStoredCredentials();
  el.creds.value = JSON.stringify(SAMPLE_CREDENTIALS, null, 2);
  log('ok', 'credentials forgotten');
});

el.clearLog.addEventListener('click', () => {
  el.log.innerHTML = '';
});

// Keep the picture sized to the panel. The shipped app does this from WP-5b's
// resize handling; the monitor module never installs its own listener.
window.addEventListener('resize', () => {
  if (!session) return;
  const available = el.stageHost.parentElement.clientWidth - 4;
  currentScale = session.fitTo(available);
  el.scale.value = String(currentScale);
  el.scaleReadout.textContent = `${currentScale.toFixed(2)}x`;
});

setInterval(refreshDiag, 1000);
setLamp('closed');
refreshDiag();
refreshDevices();
log('ok', 'harness ready — press Start. Loopback mode needs nothing else.');

// Handy from DevTools.
window.harness = {
  get session() {
    return session;
  },
  start,
  stop,
};
