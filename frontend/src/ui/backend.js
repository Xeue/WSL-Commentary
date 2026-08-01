// Thin adapter over the Wails-generated JS runtime.
//
// Owner: WP-5b.
//
// Wails v2 injects two globals into the WebView2 page once main.go calls
// wails.Run: `window.go.main.App.<Method>` for each method bound on app.go's
// App struct, and `window.runtime.{EventsOn,EventsOff}` for the Go -> JS
// events app.go declares as EventStatus ("status"), EventSender ("sender")
// and EventError ("error"). Both globals are cgo-only: they exist only in a
// build produced by the Wails CLI (Gate B), never inside a plain browser tab
// such as `npm run dev`.
//
// There is no `frontend/wailsjs/` yet, because nobody has run that CLI. Wails
// v2 also does not require its own generated wrapper files to be imported —
// the generated files are themselves thin callers of `window.go.*` — so
// rather than statically importing bindings that do not exist, every export
// below talks to `window.go` / `window.runtime` directly and falls back to an
// in-memory fake with the same shapes when neither global is present. That
// keeps this module buildable and runnable at Gate A, and it becomes the
// real thing automatically the moment Wails injects its globals: nothing
// here needs to change when Gate B opens.
//
// # Running the UI against the fakes
//
// From frontend/: `npm run dev`, then open the printed localhost URL in any
// browser. `window.go` will not exist there, so every call below is served
// by the fake backend: three plausible input devices (mirroring
// internal/gst/gst_stub.go's defaultStubDevices), a config seeded from
// internal/config's documented defaults (Defaults(), spec section 9), and
// secrets that report "not set" until this module's own setSecret fake has
// been called in the running session.
//
// The fake also drives the three Go -> JS events so the lamps have something
// to react to: start() moves SENDING through CONNECTING to CONNECTED (the
// spec's measured ~1.1 s input lock, section 4) and shortly after moves the
// fake switcher_status through "starting" to "streaming" with a healthy
// video/audio format, the way a real session start looks on the lamps.
// stop() reverses both. Errors can be forced with setDeviceError / by editing
// this file's FAKE_* constants; nothing here is reachable from production
// code once a real Wails runtime is present.
//
// For manual poking from the browser devtools console while running against
// the fakes, the fake event bus is exposed as `window.__wslcommsFake` with
// `emitStatus(status)`, `emitSender(state)`, `emitError(message)`,
// `setDevices(list)` and `setDeviceError(message|null)`. It is never created
// when a real Wails runtime is detected.

// Event names emitted Go -> JS, mirroring app.go's EventStatus / EventSender
// / EventError constants exactly. These strings are the contract; they must
// match app.go.
export const EVENT_STATUS = 'status';
export const EVENT_SENDER = 'sender';
export const EVENT_ERROR = 'error';

// EventStatusKeys: a []m2lx.StatusKeyCandidate, emitted while a statusKey
// discovery is running (app.go, maybeDiscoverStatusKey). Suggestions only —
// nothing is saved until the operator confirms one on the Settings screen.
export const EVENT_STATUS_KEYS = 'statusKeyCandidates';

// Secret keys, mirroring internal/secrets' KeyM2LX / KeySRT constants
// exactly. Passed to setSecret().
export const SECRET_KEY_M2LX = 'm2lx';
export const SECRET_KEY_SRT = 'srt';

// sender.State string values, mirrored here because the fake backend needs
// to emit them and callers need something to compare against without a
// dependency on internal/sender.
export const SENDER_STATE = Object.freeze({
  CONNECTING: 'CONNECTING',
  CONNECTED: 'CONNECTED',
  DRAINING: 'DRAINING',
  BACKOFF: 'BACKOFF',
  STOPPED: 'STOPPED',
});

// m2lx.StreamState* string values, mirrored for the same reason.
export const STREAM_STATE = Object.freeze({
  STREAMING: 'streaming',
  STARTING: 'starting',
  STOPPED: 'stopped',
});

function hasWails() {
  return (
    typeof window !== 'undefined' &&
    window.go &&
    window.go.main &&
    window.go.main.App
  );
}

function hasRuntimeEvents() {
  return (
    typeof window !== 'undefined' &&
    window.runtime &&
    typeof window.runtime.EventsOn === 'function'
  );
}

// callGo invokes a bound method by name with args, converting both a thrown
// exception and a rejected promise into a rejected promise carrying an Error
// with a readable message. Wails surfaces a Go `error` return as a rejected
// promise whose value is sometimes a string and sometimes an Error-like
// object depending on runtime version, so both are normalised here.
async function callGo(method, ...args) {
  const fn = window.go.main.App[method];
  if (typeof fn !== 'function') {
    throw new Error(`wslcomms: App.${method} is not bound`);
  }
  try {
    return await fn(...args);
  } catch (err) {
    if (err instanceof Error) throw err;
    throw new Error(typeof err === 'string' ? err : JSON.stringify(err));
  }
}

// ---------------------------------------------------------------------------
// Fake backend
// ---------------------------------------------------------------------------

// FAKE_DEVICES mirrors internal/gst/gst_stub.go's defaultStubDevices,
// including the double space in the Dante Virtual Soundcard display names —
// a caller that mishandles it should fail in the browser tab, not at the
// facility.
const FAKE_DEVICES = [
  {
    id: '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}',
    name: 'DVS Receive  1-2 (Dante Virtual Soundcard)',
  },
  {
    id: '{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}',
    name: 'DVS Receive  3-4 (Dante Virtual Soundcard)',
  },
  {
    id: '{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}',
    name: 'Microphone (Focusrite Scarlett 2i2 USB)',
  },
];

// defaultFakeConfig mirrors internal/config.Defaults(): every documented
// default from specification section 9, with the fields that have no
// documented default (hosts, alias, event and status keys, device ids) left
// blank so the Settings validation has something to catch during a dev
// session, exactly as a first run would.
function defaultFakeConfig() {
  return {
    m2lxHost: '',
    alias: '',
    eventId: '',
    srtHost: '',
    srtPort: 0,
    srtLatencyMs: 120, // config.DefaultSRTLatencyMs
    pbkeylen: 0,
    statusKey: '',
    audioDeviceId: '',
    headphoneDeviceId: '',
    returnMid: 2, // config.DefaultReturnMid (aux1/CLN)
    monitorTile: { x: 0, y: 360, w: 640, h: 360 }, // config.DefaultMonitorTile
    returnGainDb: 18, // config.DefaultReturnGainDB
    slatePath: 'slate.png', // config.DefaultSlateFilename
  };
}

let fakeConfig = defaultFakeConfig();
// secretSetThisSession tracks, for BOTH the real and the fake backend,
// whether setSecret() has succeeded for a key since the page loaded. It is
// the only "set"/"not set" signal the Settings screen can show, because
// SetSecret is write-only by design (internal/secrets never grows a getter)
// — see setSecret()'s doc comment below.
const secretSetThisSession = { [SECRET_KEY_M2LX]: false, [SECRET_KEY_SRT]: false };
let fakeDevices = FAKE_DEVICES.slice();
let fakeDeviceError = null;

const fakeListeners = new Map(); // event name -> Set<callback>

function fakeOn(event, cb) {
  if (!fakeListeners.has(event)) fakeListeners.set(event, new Set());
  fakeListeners.get(event).add(cb);
  return () => {
    fakeListeners.get(event)?.delete(cb);
  };
}

function fakeEmit(event, payload) {
  const set = fakeListeners.get(event);
  if (!set) return;
  for (const cb of set) {
    try {
      cb(payload);
    } catch (err) {
      console.error(`wslcomms: fake listener for "${event}" threw`, err);
    }
  }
}

function makeFakeStatus({ streamState, healthy }) {
  return {
    streamState,
    video: healthy
      ? { raw: 'h264 1920x1080 50 P', codec: 'h264', width: 1920, height: 1080, frameRate: 50 }
      : { raw: '', codec: '', width: 0, height: 0, frameRate: 0 },
    audio: healthy ? [{ raw: 'aac 48000 2ch', codec: 'aac', sampleRate: 48000, channels: 2 }] : [],
    at: new Date().toISOString(),
    stale: false,
  };
}

let fakeSenderTimers = [];
let fakeSenderRunning = false;

function clearFakeSenderTimers() {
  fakeSenderTimers.forEach(clearTimeout);
  fakeSenderTimers = [];
}

// fakeStart simulates the shape of a real session start: CONNECTING now,
// CONNECTED after the spec's measured ~1.1 s input lock (section 4), then the
// switcher_status echoing "starting" and "streaming" a little afterwards —
// plausible enough to exercise every lamp without a backend.
function fakeStart() {
  if (fakeSenderRunning) {
    return Promise.reject(new Error('sender: already started (fake)'));
  }
  fakeSenderRunning = true;
  clearFakeSenderTimers();
  fakeEmit(EVENT_SENDER, SENDER_STATE.CONNECTING);
  fakeSenderTimers.push(
    setTimeout(() => {
      fakeEmit(EVENT_SENDER, SENDER_STATE.CONNECTED);
      fakeEmit(EVENT_STATUS, makeFakeStatus({ streamState: STREAM_STATE.STARTING, healthy: false }));
    }, 1100),
  );
  fakeSenderTimers.push(
    setTimeout(() => {
      fakeEmit(EVENT_STATUS, makeFakeStatus({ streamState: STREAM_STATE.STREAMING, healthy: true }));
    }, 2600),
  );
  return Promise.resolve();
}

function fakeStop() {
  if (!fakeSenderRunning) {
    return Promise.reject(new Error('sender: not started (fake)'));
  }
  fakeSenderRunning = false;
  clearFakeSenderTimers();
  fakeEmit(EVENT_SENDER, SENDER_STATE.STOPPED);
  fakeEmit(EVENT_STATUS, makeFakeStatus({ streamState: STREAM_STATE.STOPPED, healthy: false }));
  return Promise.resolve();
}

function installFakeConsoleHandle() {
  if (typeof window === 'undefined') return;
  window.__wslcommsFake = {
    emitStatus: (status) => fakeEmit(EVENT_STATUS, status),
    emitSender: (state) => fakeEmit(EVENT_SENDER, state),
    emitError: (message) => fakeEmit(EVENT_ERROR, message),
    setDevices: (list) => {
      fakeDevices = Array.isArray(list) ? list : FAKE_DEVICES.slice();
    },
    setDeviceError: (message) => {
      fakeDeviceError = message || null;
    },
  };
}

// ---------------------------------------------------------------------------
// Public API — every export tries the real Wails runtime first and falls
// back to the fake above. Callers never need to know which one answered.
// ---------------------------------------------------------------------------

/** True once, at load time, if this session is running against the fakes. */
export const usingFakeBackend = !hasWails();

if (usingFakeBackend) {
  installFakeConsoleHandle();
  console.info(
    'wslcomms: no Wails runtime detected — running against the in-memory fake backend. ' +
      'See frontend/src/ui/backend.js for how to drive it from the console (window.__wslcommsFake).',
  );
}

/** Returns the audio capture endpoints for the commentary input dropdown. */
export async function listInputDevices() {
  if (hasWails()) return callGo('ListInputDevices');
  if (fakeDeviceError) throw new Error(fakeDeviceError);
  return fakeDevices.slice();
}

/** Returns the current configuration for the Settings screen. */
export async function getConfig() {
  if (hasWails()) return callGo('GetConfig');
  return JSON.parse(JSON.stringify(fakeConfig));
}

/** Persists the configuration. Does not restart a running session. */
export async function saveConfig(config) {
  if (hasWails()) return callGo('SaveConfig', config);
  fakeConfig = JSON.parse(JSON.stringify(config));
}

/**
 * Writes one of the two Credential Manager secrets. key is
 * SECRET_KEY_M2LX or SECRET_KEY_SRT. There is deliberately no getter: a
 * secret goes in and never comes back out across this boundary, on the real
 * backend or the fake. isSecretSetThisSession is the only trace of it left in
 * this process.
 */
export async function setSecret(key, value) {
  if (key !== SECRET_KEY_M2LX && key !== SECRET_KEY_SRT) {
    throw new Error(`secrets: unknown key "${key}"`);
  }
  if (hasWails()) {
    await callGo('SetSecret', key, value);
  }
  secretSetThisSession[key] = value.length > 0;
}

/** Begins the contribution session. Progress arrives on the "sender" event. */
export async function start() {
  if (hasWails()) return callGo('Start');
  return fakeStart();
}

/** Ends the contribution session. */
export async function stop() {
  if (hasWails()) return callGo('Stop');
  return fakeStop();
}

/** Runs the M2L-X -> Cognito chain and returns credentials for the monitor page. */
export async function getKVSCredentials() {
  if (hasWails()) return callGo('GetKVSCredentials');
  return {
    region: 'eu-west-1',
    channelName: 'webrtc-fake-channel',
    channelArn: '',
    accessKeyId: 'FAKEACCESSKEYID',
    secretKey: 'fake-secret-key',
    sessionToken: 'fake-session-token',
    expiry: new Date(Date.now() + 3600_000).toISOString(),
  };
}

// subscribe wires cb to event, using the real Wails runtime's EventsOn/
// EventsOff when present and the fake bus otherwise. It returns an
// unsubscribe function either way.
function subscribe(event, cb) {
  if (hasRuntimeEvents()) {
    const cancel = window.runtime.EventsOn(event, cb);
    if (typeof cancel === 'function') return cancel;
    return () => window.runtime.EventsOff(event);
  }
  return fakeOn(event, cb);
}

/** Subscribes to the "status" event, an m2lx.Status. Returns an unsubscribe function. */
export function onStatus(cb) {
  return subscribe(EVENT_STATUS, cb);
}

/** Subscribes to the "sender" event, a sender.State string. Returns an unsubscribe function. */
export function onSender(cb) {
  return subscribe(EVENT_SENDER, cb);
}

/** Subscribes to the "error" event, a human-readable string. Returns an unsubscribe function. */
export function onError(cb) {
  return subscribe(EVENT_ERROR, cb);
}

/**
 * Subscribes to the "statusKeyCandidates" event. Returns an unsubscribe
 * function. The payload is an array of {key, was, now, video, audioCount,
 * afterSeconds}.
 */
export function onStatusKeyCandidates(cb) {
  return subscribe(EVENT_STATUS_KEYS, cb);
}

/**
 * Returns the switcher_status nodes that were seen to start streaming while
 * the last discovery was running: suggestions for a statusKey the operator has
 * not set. Always an array.
 *
 * The fake backend has none, and says so by returning an empty list rather
 * than inventing a plausible "cam7" — a fabricated suggestion is exactly the
 * thing that must not reach a Settings screen, in a dev session or anywhere
 * else.
 */
export async function getStatusKeyCandidates() {
  if (hasWails()) {
    const got = await callGo('GetStatusKeyCandidates');
    return Array.isArray(got) ? got : [];
  }
  return [];
}

/**
 * True once setSecret has been called for key with a non-empty value during
 * this session. This is the only "is it set" signal available anywhere in
 * the app: internal/secrets.Store has no getter, by design (see its doc
 * comment), so a secret written in a previous session always reads false
 * here even though it is still in Credential Manager.
 */
export function isSecretSetThisSession(key) {
  return !!secretSetThisSession[key];
}

// ---------------------------------------------------------------------------
// The mixer drawer
// ---------------------------------------------------------------------------
//
// Six bindings, mirroring app_mixer.go. They are thin on purpose: every safety
// property of the mixer path lives in internal/mixer and in the drawer, and a
// helpful adapter in the middle is how one of them gets lost.
//
// THERE IS NO FAKE MIXER. Every function below rejects when there is no Wails
// runtime, rather than answering with plausible-looking state. A fabricated
// mixer snapshot renders as a routing matrix an operator can read, and the most
// likely fabrication — an empty one — reads as "nothing is in the clean feed",
// which is the single most dangerous false statement this application can make.
// A dev session that wants to see the drawer has one:
// frontend/src/ui/mixer/demo.html, which drives it from a captured live frame
// and is explicitly a demo.

const NO_MIXER_IN_FAKE =
  'the mixer is only available against a real M2L-X. This session is running on the in-memory ' +
  'fake backend, which has no mixer state and will not invent any — open ' +
  'src/ui/mixer/demo.html to see the drawer against a captured frame.';

function requireWails() {
  if (!hasWails()) throw new Error(`wslcomms: ${NO_MIXER_IN_FAKE}`);
}

/**
 * Reads the mixer now. NEVER a cached frame: the Go side opens a fresh status
 * connection for every call, because the switcher_status socket only carries a
 * complete document once per connection. The drawer's freshness gate depends on
 * that, so nothing here may cache either.
 *
 * @returns {Promise<import('./mixer/contract.js').MixerSnapshot>}
 */
export async function getMixerSnapshot() {
  requireWails();
  return callGo('GetMixerSnapshot');
}

/**
 * Opens the Go-side write window and returns {armed, armedUntil, windowSeconds}.
 * Arming changes nothing on the mixer; it only permits a later write. The
 * window closes on its own after windowSeconds whatever anybody forgets to do.
 */
export async function armMixer() {
  requireWails();
  return callGo('ArmMixer');
}

/** Shuts the write window and releases the control socket. Idempotent. */
export async function disarmMixer() {
  requireWails();
  return callGo('DisarmMixer');
}

/**
 * THE WRITE PATH TO A LIVE MIXER. Refused on the Go side with ErrDisarmed
 * unless armMixer has opened a window that has not expired — the second of the
 * two independent gates, the first being createWriteGate in mixer/model.js.
 *
 * Resolving means SENT, not applied. Confirmation comes from the next snapshot.
 *
 * @param {import('./mixer/contract.js').MixerCommand[]} cmds
 */
export async function sendMixerCommands(cmds) {
  requireWails();
  return callGo('SendMixerCommands', cmds);
}

/**
 * Loads the saved golden snapshot, or null when none has ever been saved. Null
 * is a normal state and the drawer renders it as "no golden saved" — never as
 * "no differences", which is a claim it cannot make.
 */
export async function getMixerGolden() {
  requireWails();
  return callGo('GetMixerGolden');
}

/** Saves a snapshot as the golden baseline. Writes a local file only. */
export async function setMixerGolden(snapshot) {
  requireWails();
  return callGo('SetMixerGolden', snapshot);
}
