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

// EventReturn: the native SRT return monitor's state, mirroring app.go's
// EventReturn constant exactly. The payload is a gst.ReturnState string.
export const EVENT_RETURN = 'return';

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

// RETURN_STATE: gst.ReturnState's four values, mirrored here so the UI has
// something to compare against without a dependency on internal/gst. The Go
// side is authoritative; these strings are the contract and must match it.
//
// These four are not interchangeable to a commentator, which is why the status
// line prints the state rather than a green/red verdict:
//
//   STOPPED     nothing is coming and nothing is trying
//   CONNECTING  a pipeline is being built and the SRT caller is dialling
//   RECEIVING   the handshake succeeded and audio is arriving
//   BACKOFF     an attempt failed; it is waiting to try again
//
// BACKOFF with no audio is a normal state that resolves itself. STOPPED with no
// audio is not.
export const RETURN_STATE = Object.freeze({
  STOPPED: 'STOPPED',
  CONNECTING: 'CONNECTING',
  RECEIVING: 'RECEIVING',
  BACKOFF: 'BACKOFF',
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
    headphoneDeviceId: '', // a browser mediaDeviceId — the WebRTC return path
    headphoneEndpointId: '', // a WASAPI endpoint GUID — the SRT return path
    returnSource: 'webrtc', // config.DefaultReturnSource
    returnChannel: 'stereo', // config.DefaultReturnChannel
    srtReturnPort: 40503, // config.DefaultSRTReturnPort — Output 3, src=cln
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
// The native SRT return
// ---------------------------------------------------------------------------
//
// Five bindings and one event, for the return path the browser is not in:
// srtsrc ! tsdemux ! aacparse ! mfaacdec ! audioconvert ! wasapi2sink, entirely
// inside Go. Mirrors app_return.go.
//
// # StartReturn TAKES NO ARGUMENTS, AND THAT MATTERS
//
// Everything it needs — host, port, latency, passphrase, channel selection and
// the WASAPI endpoint — it reads from the saved configuration. So a caller that
// changes any of those must SAVE FIRST and start second. Calling startReturn()
// with an unsaved change starts the previous configuration, silently and
// successfully, which is the worst kind of success.
//
// It also refuses outright while returnSource is "webrtc". That refusal is on
// the Go side on purpose: it is the guarantee that both paths cannot be up at
// once, and the frontend is exactly where a race between a settings save and a
// page reload would put both of them up.
//
// # isSRTReturnSelected, and why the UI does not just compare a string
//
// app_return.go's IsSRTReturnSelected exists so that ONE place decides which
// path owns the headphones. The frontend could read returnSource out of
// getConfig() and compare it to "srt" itself — and then the same decision would
// be written in two languages, and the failure of the two disagreeing is both
// paths playing at once, which is the one outcome the setting exists to
// prevent. So the WebRTC return is attached on the answer to this question, not
// on a local string comparison.
//
// # A build without the bindings
//
// They are recent. Every call below distinguishes three situations that
// otherwise all arrive as the same opaque rejected promise inside WebView2:
//
//   no Wails runtime at all   -> the fake below answers, honestly labelled
//   Wails, but no such method -> BindingMissingError, which the UI renders as
//                                "this build has no SRT return" rather than as
//                                a failure of the return itself
//   Wails, method present     -> the real thing

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const RETURN_METHODS = Object.freeze({
  list: 'ListOutputDevices',
  start: 'StartReturn',
  stop: 'StopReturn',
  state: 'GetReturnState',
  selected: 'IsSRTReturnSelected',
});

/**
 * RETURN_METHOD_NAMES is every Go method the SRT return path needs, derived from
 * the table above rather than listed again — a sixth binding added there is
 * covered here without a second edit, and srtReturnAvailable is what decides
 * whether the option is offered at all.
 */
const RETURN_METHOD_NAMES = Object.freeze(Object.values(RETURN_METHODS));

/**
 * BindingMissingError means the Wails runtime is real but does not export the
 * method — an older build, or a sibling still mid-edit. Distinct from an
 * ordinary failure because the remedy is different: nothing the operator can do
 * at the desk will fix it.
 */
export class BindingMissingError extends Error {
  constructor(method) {
    super(
      `wslcomms: this build has no App.${method}. The native SRT return is not available in it; ` +
        'the WebRTC return is unaffected.',
    );
    this.name = 'BindingMissingError';
    this.method = method;
  }
}

function hasBinding(method) {
  return hasWails() && typeof window.go.main.App[method] === 'function';
}

/** callGoBound is callGo with the missing-method case named. */
async function callGoBound(method, ...args) {
  if (!hasBinding(method)) throw new BindingMissingError(method);
  return callGo(method, ...args);
}

/**
 * srtReturnAvailable reports whether this build can do the native SRT return at
 * all. False against a Wails build without the bindings; the UI uses it to
 * disable the SRT option with a reason rather than offering a button that always
 * fails.
 *
 * IT CHECKS ALL FIVE, not just start and stop. Every one of them is called on a
 * path that has already assumed availability: getReturnState() straight after
 * startReturn(), isSRTReturnSelected() when switching back to WebRTC, and
 * listOutputDevices() to fill the Headphones dropdown. A build with StartReturn
 * and StopReturn but not GetReturnState offers the option, starts the return,
 * and then throws a BindingMissingError out of the line after the start — which
 * lands in the caller's recovery path WITH A MONITOR ALREADY RUNNING. Deciding
 * availability on a subset is how a missing binding turns into a wrong recovery.
 */
export function srtReturnAvailable() {
  return RETURN_METHOD_NAMES.every(hasBinding);
}

/**
 * RETURN_ALREADY_RUNNING is the text of app_return.go's errReturnAlreadyRunning.
 *
 * It is a STRING CONTRACT and it is mirrored here on purpose: Wails flattens a
 * Go error to its message, so the sentinel does not survive the boundary as
 * anything else. returnsource.test.js asserts that app_return.go still spells it
 * this way, because the cost of the two drifting apart is silent — the caller
 * stops recognising "already running", treats it as a failed start, and takes
 * the recovery path while a monitor is running.
 */
export const RETURN_ALREADY_RUNNING = 'the return monitor is already running';

/**
 * isReturnAlreadyRunningError reports whether err is StartReturn's refusal to
 * open a SECOND monitor.
 *
 * This is not a failure to start; it means one is already up. It happens for one
 * ordinary reason: the WebView reloaded. beforeunload fires stopReturn() and
 * cannot await it, the page dies before the IPC completes, and the Go-side
 * monitor outlives the context that asked for it. The new page then reads
 * returnSource: "srt" and gets this back from its own StartReturn.
 *
 * Treated as a failure it produces the worst outcome this application has: the
 * caller falls back to WebRTC, un-mutes it, and the orphaned GStreamer pipeline
 * is still playing CLN into the same headphones a few hundred milliseconds
 * apart. The right response is stop-then-start, which is what
 * ui/returnpath.js does with this.
 *
 * @param {unknown} err
 * @returns {boolean}
 */
export function isReturnAlreadyRunningError(err) {
  if (!err) return false;
  const message = typeof err === 'string' ? err : String(err.message ?? err);
  return message.toLowerCase().includes(RETURN_ALREADY_RUNNING);
}

/**
 * RETURN_NOT_RUNNING is the text of app_return.go's errReturnNotRunning, under
 * the same string contract as RETURN_ALREADY_RUNNING above.
 */
export const RETURN_NOT_RUNNING = 'the return monitor is not running';

/**
 * isReturnNotRunningError reports whether err is StopReturn's "there was nothing
 * to stop".
 *
 * That is the NORMAL answer on most stop paths — the return is stopped
 * unconditionally before WebRTC is made audible, precisely so that nothing has
 * to be assumed about whether one was running — so it must be distinguishable
 * from a stop that genuinely failed. A red banner on every ordinary path switch
 * trains people to ignore the banner.
 *
 * @param {unknown} err
 * @returns {boolean}
 */
export function isReturnNotRunningError(err) {
  if (!err) return false;
  const message = typeof err === 'string' ? err : String(err.message ?? err);
  return message.toLowerCase().includes(RETURN_NOT_RUNNING);
}

// FAKE_OUTPUT_DEVICES are WASAPI RENDER endpoints. Their ids deliberately do
// NOT match FAKE_DEVICES: those are capture endpoints, and the whole hazard the
// return-source control has to avoid is treating a WASAPI endpoint id and a
// browser mediaDeviceId as the same string. A dev session in which the two
// lists share ids would hide exactly that mistake.
const FAKE_OUTPUT_DEVICES = [
  {
    id: '{0.0.0.00000000}.{7a2c1f90-4b3e-4c1a-9d55-0d1b3f8e2a11}',
    name: 'Headphones (Focusrite Scarlett 2i2 USB)',
  },
  {
    id: '{0.0.0.00000000}.{2e91b4c7-88a0-41d6-bf3c-6a0e5c7d4b02}',
    name: 'DVS Transmit  1-2 (Dante Virtual Soundcard)',
  },
  {
    id: '{0.0.0.00000000}.{c5d0e133-19af-4f2b-a7e4-91b0c2f6d38a}',
    name: 'Speakers (Realtek(R) Audio)',
  },
];

let fakeReturnState = RETURN_STATE.STOPPED;
let fakeReturnTimer = null;

function setFakeReturnState(next) {
  fakeReturnState = next;
  fakeEmit(EVENT_RETURN, fakeReturnState);
}

/**
 * Lists the Windows audio RENDER endpoints the native return can play through.
 *
 * Device.id is a WASAPI IMMDevice endpoint GUID and is what belongs in
 * config.headphoneEndpointId. It is NOT a browser mediaDeviceId and the two
 * cannot be substituted in either direction — see
 * config.Config.HeadphoneEndpointID for what goes wrong when they are, which is
 * nothing visible: audio in the wrong ears and no diagnostic anywhere.
 *
 * @returns {Promise<Array<{id: string, name: string}>>}
 */
export async function listOutputDevices() {
  if (hasWails()) return callGoBound(RETURN_METHODS.list);
  if (fakeDeviceError) throw new Error(fakeDeviceError);
  return FAKE_OUTPUT_DEVICES.slice();
}

/**
 * Reports whether the SRT return is the configured path, according to the Go
 * side. The WebRTC return audio is attached only when this is false.
 *
 * @returns {Promise<boolean>}
 */
export async function isSRTReturnSelected() {
  if (hasWails()) return callGoBound(RETURN_METHODS.selected);
  return fakeConfig.returnSource === 'srt';
}

/**
 * Starts the native SRT return.
 *
 * NO ARGUMENTS: it reads the saved configuration. Save returnSource,
 * headphoneEndpointId and returnChannel BEFORE calling this, or it starts the
 * previous ones and reports success.
 *
 * Resolving means the reconnect loop is running, not that audio is arriving —
 * a connection failure is deliberately not an error here, because the
 * commentator may well press the button before the operator has enabled the
 * output. Watch the "return" event for what is actually happening.
 */
export async function startReturn() {
  if (hasWails()) return callGoBound(RETURN_METHODS.start);

  // The fake refuses in the same case the real one does, because that refusal
  // is the guarantee that both paths cannot be audible at once and a fake that
  // waved it through would let the bug be written and then not reproduce.
  if (fakeConfig.returnSource !== 'srt') {
    throw new Error(
      `wslcomms: cannot start the SRT return: returnSource is "${fakeConfig.returnSource || 'webrtc'}"`,
    );
  }
  // And it refuses a SECOND monitor, exactly as app_return.go's
  // errReturnAlreadyRunning does. A fake that quietly restarted instead would
  // hide the whole stop-then-start recovery: the case is reached by nothing more
  // exotic than reloading the WebView while the SRT return is up, and it is the
  // case in which getting the recovery wrong puts both paths in the
  // commentator's ears.
  if (fakeReturnState !== RETURN_STATE.STOPPED) {
    throw new Error(`wslcomms: ${RETURN_ALREADY_RUNNING}`);
  }
  if (fakeReturnTimer) clearTimeout(fakeReturnTimer);
  setFakeReturnState(RETURN_STATE.CONNECTING);
  fakeReturnTimer = setTimeout(() => {
    fakeReturnTimer = null;
    setFakeReturnState(RETURN_STATE.RECEIVING);
  }, 900);
}

/**
 * Stops the native SRT return and releases the M2L-X fan-out slot.
 *
 * REJECTS WHEN NOTHING WAS RUNNING — app_return.go returns errReturnNotRunning
 * so that teardown can call it unconditionally and still tell "there was
 * nothing to stop" from a real failure. Every caller here catches it. The fake
 * refuses in the same case for the same reason the fake start does.
 */
export async function stopReturn() {
  if (hasWails()) return callGoBound(RETURN_METHODS.stop);
  if (fakeReturnState === RETURN_STATE.STOPPED) {
    throw new Error('wslcomms: the return monitor is not running (fake)');
  }
  if (fakeReturnTimer) {
    clearTimeout(fakeReturnTimer);
    fakeReturnTimer = null;
  }
  setFakeReturnState(RETURN_STATE.STOPPED);
}

/**
 * Reads the SRT return's state now, for a page that has just loaded and has not
 * yet seen a "return" event. One of RETURN_STATE.
 *
 * @returns {Promise<string>}
 */
export async function getReturnState() {
  if (hasWails()) return callGoBound(RETURN_METHODS.state);
  return fakeReturnState;
}

/**
 * Subscribes to the "return" event, a gst.ReturnState string. Returns an
 * unsubscribe function.
 */
export function onReturn(cb) {
  return subscribe(EVENT_RETURN, cb);
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
