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

// The preset whitelist's JS mirror, used ONLY by the fake backend below so a
// dev session drops the same non-instance keys a real ApplyPreset would.
// presets.js imports nothing, so this cannot cycle.
import { filterPresetFields } from './presets.js';

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

// EventLevels: the send pipeline's own measurement of the commentary audio,
// mirroring app.go's EventLevels constant exactly. The payload is
// {peak: number[], rms: number[]} — one entry per channel, dBFS, silence
// clamped to -100 (never -Infinity; it does not survive JSON). It arrives at
// most 20 times a second while a session is running, plus one final
// all-silence frame when the session stops, so a meter driven from it falls
// to nothing rather than freezing at the last level.
export const EVENT_LEVELS = 'levels';

// EventConfig: the freshly-saved configuration and the id of the seat that
// saved it, mirroring app_remote.go's EventConfig ("config") exactly. The
// payload is {config: Config, origin: string}. It is emitted after ANY seat's
// SaveConfig — the local WebView2 or a remote browser on the LAN bridge — so a
// SECOND controller can refresh its cache instead of clobbering the other
// page's edits on the next whole-object save. `origin` is the saving seat's id,
// which is how a page tells its own echo (ignore it) from somebody else's save
// (adopt it); see app.js. String contract, mirrored from app_remote.go.
export const EVENT_CONFIG = 'config';

// EventRemote: the remote seats connected right now, mirroring app_remote.go's
// EventRemote ("remote") exactly. The payload is an array of {name, addr}, or an
// empty array when nobody is connected. It drives the home-screen indicator that
// lets the operator at the desk SEE that someone else has a seat — without it, a
// remote operator pressing STOP is indistinguishable from a crash.
export const EVENT_REMOTE = 'remote';

// Secret keys, mirroring internal/secrets' KeyM2LX / KeySRT / KeySRTReturn
// constants exactly. Passed to setSecret().
//
// SECRET_KEY_SRT and SECRET_KEY_SRT_RETURN are two DIFFERENT passphrases, not
// one value stored twice. Encryption on M2L-X is a per-endpoint setting — the
// commentary input this app sends to and the programme output the return dials
// disagree about it on the measured instance — so one field for both means the
// operator cannot describe what is in front of them, and setting the key that
// makes the monitor work changes the key the feed goes out with.
export const SECRET_KEY_M2LX = 'm2lx';
export const SECRET_KEY_SRT = 'srt';
export const SECRET_KEY_SRT_RETURN = 'srtreturn';

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
    srtPort: 0,
    srtLatencyMs: 120, // config.DefaultSRTLatencyMs
    pbkeylen: 0,
    statusKey: '',
    audioDeviceId: '',
    headphoneDeviceId: '', // a browser mediaDeviceId — the WebRTC return path
    headphoneEndpointId: '', // a WASAPI endpoint GUID — the SRT return path
    returnSource: 'webrtc', // config.DefaultReturnSource
    returnChannel: 'stereo', // config.DefaultReturnChannel
    srtReturnPort: 40501, // config.DefaultSRTReturnPort — Output 1, src=pgm (DIRTY programme)
    srtReturnPBKeyLen: 0, // config.DefaultSRTReturnPBKeyLen — Output 1 measured encrypted=false
    pictureLatencyMs: 120, // config.DefaultPictureLatencyMs — the picture window's SRT buffer
    returnMid: 4, // config.DefaultReturnMid (MIC1 / "Monitor 1")
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
const secretSetThisSession = {
  [SECRET_KEY_M2LX]: false,
  [SECRET_KEY_SRT]: false,
  [SECRET_KEY_SRT_RETURN]: false,
};
let fakeDevices = FAKE_DEVICES.slice();
let fakeDeviceError = null;

// FAKE_EVENTS is what a `npm run dev` session lists through listEvents(). ONE
// event by default, shaped exactly like the Go m2lx.Event ({id, name, status}),
// so the dev loop shows the OPERATOR'S DEFAULT — a lone event is auto-selected,
// with no picker (see ui/events.js). Reassign it from the devtools console
// (window.__wslcommsFake.setEvents) to exercise the empty-instance and
// multiple-event paths without a Wails build.
let fakeEvents = [{ id: 'dl9-5p5ah0bd-empd', name: 'MatchT', status: 'Running' }];

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

// --- fake input levels -----------------------------------------------------
//
// The same deterministic waveform internal/gst's stub twin emits (gst_stub.go
// via levels.go's stubLevelsAt): a triangle from -40 up to -6 dBFS and back
// over six seconds at 20 frames a second, right channel a quarter-period
// behind the left, RMS 8 dB under the peak. Mirrored by value rather than
// imported from anywhere because there is nowhere to import it from — the Go
// stub is the other side of the boundary — and matching it means a dev session
// in the browser and a Gate A session in the app show the same moving meters.
// The final all-silence frame on stop mirrors app.go's zero-frame: -100 dBFS
// per channel, the clamped floor, so the meters fall rather than freeze.
const FAKE_LEVELS_LOW_DB = -40;
const FAKE_LEVELS_HIGH_DB = -6;
const FAKE_LEVELS_PERIOD = 120; // 50 ms steps: a six-second sweep
const FAKE_LEVELS_RIGHT_OFFSET = FAKE_LEVELS_PERIOD / 4;
const FAKE_LEVELS_RMS_BELOW_PEAK_DB = 8;
const FAKE_LEVELS_SILENCE_DB = -100;

let fakeLevelsInterval = null;
let fakeLevelsStep = 0;

function fakeLevelAt(step, channel) {
  const phase = (step + channel * FAKE_LEVELS_RIGHT_OFFSET) % FAKE_LEVELS_PERIOD;
  const half = FAKE_LEVELS_PERIOD / 2;
  const span = FAKE_LEVELS_HIGH_DB - FAKE_LEVELS_LOW_DB;
  if (phase < half) return FAKE_LEVELS_LOW_DB + (span * phase) / half;
  return FAKE_LEVELS_HIGH_DB - (span * (phase - half)) / half;
}

function startFakeLevels() {
  if (fakeLevelsInterval) return;
  fakeLevelsStep = 0;
  fakeLevelsInterval = setInterval(() => {
    const peak = [fakeLevelAt(fakeLevelsStep, 0), fakeLevelAt(fakeLevelsStep, 1)];
    fakeEmit(EVENT_LEVELS, {
      peak,
      rms: peak.map((p) => Math.max(FAKE_LEVELS_SILENCE_DB, p - FAKE_LEVELS_RMS_BELOW_PEAK_DB)),
    });
    fakeLevelsStep += 1;
  }, 50);
}

function stopFakeLevels() {
  if (fakeLevelsInterval) {
    clearInterval(fakeLevelsInterval);
    fakeLevelsInterval = null;
  }
  // The zero-frame, exactly as app.go emits one when the session ends: the
  // meters must fall to silence, not freeze at the last level.
  fakeEmit(EVENT_LEVELS, {
    peak: [FAKE_LEVELS_SILENCE_DB, FAKE_LEVELS_SILENCE_DB],
    rms: [FAKE_LEVELS_SILENCE_DB, FAKE_LEVELS_SILENCE_DB],
  });
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
  // Levels begin with the session, not with the connection: the real pipeline
  // is capturing and encoding from Start onwards — the sink comes later — and
  // the level element measures upstream of the sink, so the meters move even
  // while the SRT caller is still dialling. The fake keeps that property.
  startFakeLevels();
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
  stopFakeLevels();
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
    // Drive the event auto-select / picker: [] for an empty instance (the URL
    // falls back in), one for the auto-select path, several for the picker.
    setEvents: (list) => {
      fakeEvents = Array.isArray(list) ? list : [];
    },
  };
}

// ---------------------------------------------------------------------------
// Public API — every export tries the real Wails runtime first and falls
// back to the fake above. Callers never need to know which one answered.
// ---------------------------------------------------------------------------

/** True once, at load time, if this session is running against the fakes. */
export const usingFakeBackend = !hasWails();

/**
 * isRemoteClient reports whether this page is a REMOTE browser reaching the app
 * over the LAN bridge, rather than the local WebView2 window (or a `npm run dev`
 * tab).
 *
 * The transport shim (internal/remote/shim.js) sets window.__wslcommsRemote the
 * moment it loads — from its very first line, before the hello frame arrives —
 * and the local WebView2 and a plain browser tab never do. So the presence of
 * that object IS the signal, and it is readable from the first paint.
 *
 * app.js uses it for two safety decisions: to make the picture-unavailable
 * message honest (the SRT picture is a host GPU surface that cannot cross the
 * network, not a missing build feature), and to SKIP the destructive
 * beforeunload stopReturn()/stopPicture() calls — a closing remote tab must not
 * be able to kill the commentator's audio or picture. It is deliberately NOT a
 * transport call: it reads a global the shim already published.
 */
export function isRemoteClient() {
  return typeof window !== 'undefined' && window.__wslcommsRemote != null;
}

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
 * Writes one of the three Credential Manager secrets. key is SECRET_KEY_M2LX,
 * SECRET_KEY_SRT or SECRET_KEY_SRT_RETURN. There is deliberately no getter: a
 * secret goes in and never comes back out across this boundary, on the real
 * backend or the fake. isSecretSetThisSession is the only trace of it left in
 * this process.
 *
 * The key is checked here as well as in Go. internal/secrets rejects an unknown
 * key, but the fake backend never reaches it, and a typo that silently recorded
 * a badge as "set" against a key nothing reads is exactly the reassurance an
 * operator must not be given about a passphrase.
 */
export async function setSecret(key, value) {
  if (!Object.prototype.hasOwnProperty.call(secretSetThisSession, key)) {
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

// ---------------------------------------------------------------------------
// The M2L-X events list
// ---------------------------------------------------------------------------
//
// One binding, App.ListEvents, mirroring the concrete (*client).ListEvents: a
// GET /api/events/overview through the already-signed-in client, returning
// [{id, name, status}] sorted by name with id-less entries dropped (all on the
// Go side).
//
// It is the mechanism behind "you should not have to know your event id". With
// a signed-in client the app can enumerate the instance's events and, when
// there is exactly one, choose it without asking; when there are several, offer
// a picker (ui/events.js owns that rule). The pasted live-operation URL is still
// the source when the app CANNOT list — not signed in, no host, or an older
// build without this binding — which is why it goes through callGoBound: against
// such a build the caller gets a BindingMissingError it treats the same as an
// empty list, and settings.js wraps the whole call in try/catch so a failure
// leaves today's URL-derived behaviour untouched rather than breaking the
// screen.

/** ListEvents' bound method name. One place, so a rename is one edit. */
const EVENTS_METHOD = 'ListEvents';

/**
 * Lists the M2L-X instance's events: [{id, name, status}]. An empty instance is
 * an empty array and NOT an error — "no events" is a state the caller renders,
 * falling back to the URL-derived id. A non-array answer (or a build without the
 * binding, via BindingMissingError out of callGoBound) is the caller's cue to
 * do the same.
 *
 * @returns {Promise<Array<{id: string, name: string, status: string}>>}
 */
export async function listEvents() {
  if (hasWails()) {
    const got = await callGoBound(EVENTS_METHOD);
    return Array.isArray(got) ? got : [];
  }
  return fakeEvents.map((e) => ({ ...e }));
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
 * Subscribes to the "levels" event: {peak: number[], rms: number[]}, dBFS per
 * channel, at most 20 frames a second while a session runs, one all-silence
 * frame ([-100, ...]) when it stops. Returns an unsubscribe function.
 */
export function onLevels(cb) {
  return subscribe(EVENT_LEVELS, cb);
}

/**
 * Subscribes to the "config" event: {config, origin}. Emitted after ANY seat's
 * SaveConfig so a second controller can refresh; `origin` is the saving seat's
 * id, which lets a page ignore the echo of its own save. Returns an unsubscribe
 * function. Mirrors onStatus/onLevels; the transport is the shim's, not this
 * module's.
 */
export function onConfig(cb) {
  return subscribe(EVENT_CONFIG, cb);
}

/**
 * Subscribes to the "remote" event: an array of {name, addr} of the connected
 * remote seats, for the home-screen indicator. An empty array means nobody is
 * connected. Returns an unsubscribe function.
 */
export function onRemote(cb) {
  return subscribe(EVENT_REMOTE, cb);
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
// The native picture overlay
// ---------------------------------------------------------------------------
//
// Five bindings and one event, for the path a browser cannot be in at all:
// srtsrc ! tsdemux ! h265parse ! d3d11h265dec ! a native child window painted
// over this page. Entirely inside Go.
//
// THE PICTURE IS SRT. THE AUDIO IS KINESIS. Nothing in this section touches
// audio, and the SRT stream's own AAC track is discarded on the Go side. A
// previous revision of this application built SRT as an AUDIO path, which is the
// opposite of what was asked for, and it is why selecting "SRT" used to silence
// the operator.
//
// # SetPictureRect takes CSS PIXELS AND THE DEVICE PIXEL RATIO, in one call
//
// gst.PictureRect is in PHYSICAL pixels, and gst.ScaleRect is what converts —
// on the Go side, from the numbers this call carries. The page cannot send only
// physical pixels and Go cannot read the factor for itself: GetDpiForWindow is
// the monitor's scale factor, which equals the WebView's device pixel ratio only
// at 100% zoom, and Ctrl+scroll on a WebView2 changes one and not the other.
//
// The two travel together so a rectangle can never be paired with a ratio
// measured at a different moment. See setPictureRect below.
//
// The origin is the client area rather than the screen because the native window
// is a child of the same top-level window: dragging the window moves both
// together and needs no report, which is fortunate, because a page cannot
// observe its own window moving.
//
// # SetPictureVisible is not a convenience
//
// The overlay is opaque and it is outside the page's stacking context — no
// z-index reaches it. Anything the page draws over that rectangle is invisible
// until this is called with false. Settings and the mixer drawer both do.

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const PICTURE_METHODS = Object.freeze({
  start: 'StartPicture',
  stop: 'StopPicture',
  rect: 'SetPictureRect',
  visible: 'SetPictureVisible',
  state: 'GetPictureState',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const PICTURE_METHOD_NAMES = Object.freeze(Object.values(PICTURE_METHODS));

// EventPicture: the native picture receiver's state. The payload is one of
// PICTURE_STATE below.
export const EVENT_PICTURE = 'picture';

/**
 * PICTURE_STATE mirrors internal/gst's PictureState EXACTLY, and it is
 * lowercase.
 *
 * gst.ReturnState is uppercase and this is not. The two enums are neighbours,
 * they carry nearly the same four words, and a copy made from the wrong one
 * would compare unequal to every event that arrives — leaving the status line
 * sitting on whatever was last recognised while the picture came and went.
 * picturesource.test.js asserts these four strings against picture.go itself.
 *
 * SHOWING is where RETURN_STATE says RECEIVING, because a picture that is being
 * received and a picture that is on screen are different claims, and only the
 * second one is worth making to a commentator.
 */
export const PICTURE_STATE = Object.freeze({
  STOPPED: 'stopped',
  CONNECTING: 'connecting',
  SHOWING: 'showing',
  BACKOFF: 'backoff',
});

/**
 * pictureAvailable reports whether this build can do the native SRT picture at
 * all.
 *
 * It checks ALL FIVE, for the reason srtReturnAvailable does: every one of them
 * is called on a path that has already assumed the option was offered. A build
 * with StartPicture but no SetPictureRect would start a receiver and then paint
 * it at whatever rectangle a native default puts it at — an opaque box over the
 * application, which is worse than no picture at all.
 */
export function pictureAvailable() {
  return PICTURE_METHOD_NAMES.every(hasBinding);
}

let fakePictureState = PICTURE_STATE.STOPPED;
let fakePictureTimer = null;
let fakePictureRect = null;
let fakePictureVisible = false;

function setFakePictureState(next) {
  fakePictureState = next;
  fakeEmit(EVENT_PICTURE, fakePictureState);
}

/**
 * Starts the native SRT picture receiver.
 *
 * NO ARGUMENTS: like StartReturn it reads the saved configuration — host, port,
 * latency, key length — so a caller that changes any of those must save first.
 *
 * Resolving means the reconnect loop is running, not that a picture is on
 * screen. Watch the "picture" event for that.
 */
export async function startPicture() {
  if (hasWails()) return callGoBound(PICTURE_METHODS.start);
  if (fakePictureState !== PICTURE_STATE.STOPPED) {
    throw new Error('wslcomms: the picture receiver is already running (fake)');
  }
  if (fakePictureTimer) clearTimeout(fakePictureTimer);
  setFakePictureState(PICTURE_STATE.CONNECTING);
  // AND IT NEVER REACHES SHOWING. There is no native decoder and no child
  // window in a browser tab, so a fake that claimed a picture was on screen
  // would hide the mosaic behind an overlay that does not exist — a black
  // rectangle, in the one session where somebody is looking at the layout.
  //
  // BACKOFF is the honest answer and it is also the more useful one: it is the
  // FALLBACK that a dev session needs to be able to see, and it is the state
  // this application spends its worst minutes in.
  fakePictureTimer = setTimeout(() => {
    fakePictureTimer = null;
    setFakePictureState(PICTURE_STATE.BACKOFF);
    console.info(
      'wslcomms: the fake backend cannot decode SRT — there is no native window in a browser ' +
        'tab — so the picture stays in BACKOFF and the mosaic fallback is what you are seeing. ' +
        'That is the intended behaviour of the fake, not a failure of the picture path.',
    );
  }, 1200);
}

/** Stops the receiver, hides the overlay and releases the M2L-X fan-out slot. */
export async function stopPicture() {
  if (hasWails()) return callGoBound(PICTURE_METHODS.stop);
  if (fakePictureTimer) {
    clearTimeout(fakePictureTimer);
    fakePictureTimer = null;
  }
  fakePictureVisible = false;
  setFakePictureState(PICTURE_STATE.STOPPED);
}

/**
 * Positions the overlay.
 *
 * ============ IT SENDS CSS PIXELS AND THE RATIO, IN ONE CALL ================
 *
 * Not physical pixels, and this is gst.PictureRect's contract rather than a
 * convenience. Go multiplies, in gst.ScaleRect, and it does so because the
 * factor cannot be read on the Go side without being a DIFFERENT NUMBER
 * MEASURED AT A DIFFERENT MOMENT: GetDpiForWindow is the monitor's scale
 * factor, which equals the WebView's device pixel ratio only at 100% zoom, and
 * Ctrl+scroll changes one and not the other. The page's own ratio is
 * authoritative because the page's own layout is what the rectangle has to line
 * up with.
 *
 * The ratio travels WITH the rectangle, in the same call, so that a rectangle
 * can never be paired with a ratio measured before or after it. That is the
 * whole reason this is not two bindings.
 *
 * overlay.js still computes the physical rectangle. It does not send it: it uses
 * it to decide WHETHER to send — a DPI change with an unchanged CSS box has to
 * re-report, and only the physical rectangle knows that — and it applies the
 * same edge-rounding rule gst.ScaleRect does, so the number in the console line
 * is the number Go will land on.
 *
 * @param {{x: number, y: number, width: number, height: number}} cssRect
 * @param {number} devicePixelRatio  window.devicePixelRatio, as measured
 */
export async function setPictureRect(cssRect, devicePixelRatio) {
  const { x, y, width, height } = cssRect || {};
  if (hasWails()) {
    return callGoBound(PICTURE_METHODS.rect, x, y, width, height, devicePixelRatio);
  }
  fakePictureRect = { x, y, width, height, devicePixelRatio };
}

/**
 * Shows or hides the overlay without stopping the receiver.
 *
 * Hiding is what Settings and the mixer drawer need: the overlay is opaque and
 * on top of its rectangle whatever the page does, so a screen drawn underneath
 * it is a screen the operator can only read two thirds of. Hiding rather than
 * stopping means coming back from Settings does not re-dial M2L-X.
 *
 * @param {boolean} visible
 */
export async function setPictureVisible(visible) {
  if (hasWails()) return callGoBound(PICTURE_METHODS.visible, visible === true);
  fakePictureVisible = visible === true;
}

/**
 * Reads the picture receiver's state now, for a page that has just loaded and
 * has not yet seen a "picture" event. One of PICTURE_STATE.
 *
 * @returns {Promise<string>}
 */
export async function getPictureState() {
  if (hasWails()) return callGoBound(PICTURE_METHODS.state);
  return fakePictureState;
}

/** Subscribes to the "picture" event. Returns an unsubscribe function. */
export function onPicture(cb) {
  return subscribe(EVENT_PICTURE, cb);
}

/**
 * The fake overlay's last known geometry, for a dev session in the browser where
 * there is no native window to look at. Diagnostics only; nothing reads it.
 */
export function fakePictureOverlay() {
  return { rect: fakePictureRect, visible: fakePictureVisible, state: fakePictureState };
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

// ---------------------------------------------------------------------------
// The M2L-X instance presets
// ---------------------------------------------------------------------------
//
// Seven bindings, mirroring app_presets.go. A preset is one M2L-X deployment's
// coordinates, applied onto the live config as a MERGE on the Go side; the
// whitelist that keeps device ids out of it lives in internal/presets and is
// mirrored (for the fake and the confirm dialog) in ./presets.js.
//
// # applyPreset RETURNS THE MERGED CONFIG, and the caller must ASSIGN it
//
// `currentConfig = await backend.applyPreset(id)` is a correctness contract,
// not a style choice: app.js re-writes the WHOLE currentConfig on the next
// dropdown change, so an apply that did not replace the page's cache would be
// clobbered by the very next control the operator touched — the stale cache
// winning over the preset that was just applied, deterministically.
//
// # The preset NAME goes to Go; GO derives the id
//
// The id is a filename under %APPDATA% and a Credential Manager target
// segment, and the sanitiser that makes it safe exists exactly once, in
// internal/presets.DeriveID. Nothing on this side slugifies (the fake's
// stand-in below is the fake BEING Go, not a second implementation shipped to
// production).

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const PRESET_METHODS = Object.freeze({
  list: 'ListPresets',
  save: 'SavePreset',
  apply: 'ApplyPreset',
  rename: 'RenamePreset',
  delete: 'DeletePreset',
  active: 'GetActivePreset',
  credentials: 'GetPresetCredentialStatus',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const PRESET_METHOD_NAMES = Object.freeze(Object.values(PRESET_METHODS));

/**
 * presetsAvailable reports whether this build has the instance presets at all.
 *
 * It requires ALL SEVEN, for the reason srtReturnAvailable and
 * pictureAvailable spell out at length (backend.js's return section): every
 * one of these is called on a path that has already assumed availability —
 * getActivePreset() straight after applyPreset(), getPresetCredentialStatus()
 * to render the scope line — and deciding on a subset turns a missing binding
 * into a throw on the line AFTER an apply has already half-happened.
 */
export function presetsAvailable() {
  return PRESET_METHOD_NAMES.every(hasBinding);
}

// --- the fake preset store ---------------------------------------------------
//
// In-memory, mirroring the Go behaviour closely enough that the Settings
// group is exercisable under `npm run dev`: the whitelist filter (through
// presets.js, the same module the real UI uses), the merge, the migration
// rule for the first preset's credential scope, and the refusal to apply
// while the fake sender is running — because that refusal is the safety story
// and a fake that waved it through would let the bug be written and then not
// reproduce.

let fakePresets = []; // [{id, name, credentialScope, savedAt, fields}]
let fakeActivePreset = { id: '', credentialScope: '', appliedAt: '' };

/**
 * fakeDeriveId is the fake standing in for Go's DeriveID — NOT a second
 * production slugifier. It exists only so `npm run dev` can mint plausible
 * ids; the real path sends the NAME to Go and Go answers.
 */
function fakeDeriveId(name) {
  const id = String(name)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  if (!id || id.length > 48) {
    throw new Error(`presets: ${JSON.stringify(name)} cannot name a preset (fake)`);
  }
  return id;
}

function fakePresetSummaries() {
  return fakePresets
    .slice()
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .map((p) => JSON.parse(JSON.stringify(p)));
}

/** Lists every saved preset: {id, name, credentialScope, savedAt, fields}[]. */
export async function listPresets() {
  if (hasWails()) return callGoBound(PRESET_METHODS.list);
  return fakePresetSummaries();
}

/**
 * Saves the CURRENT SAVED configuration as a preset named `name`, returns its
 * summary, and points the active record at it. Unsaved form edits are not in
 * it — SavePreset extracts from what SaveConfig last persisted.
 */
export async function savePreset(name) {
  if (hasWails()) return callGoBound(PRESET_METHODS.save, name);

  const id = fakeDeriveId(name);
  const existing = fakePresets.find((p) => p.id === id);
  if (existing && existing.name.toLowerCase() !== String(name).trim().toLowerCase()) {
    throw new Error(
      `presets: the name ${JSON.stringify(name)} would collide with the existing preset ` +
        `${JSON.stringify(existing.name)} (fake)`,
    );
  }
  // The migration rule, mirrored: the FIRST preset on a machine that has never
  // had one keeps the legacy scope "" so nothing has to be retyped.
  const migration = fakePresets.length === 0 && !fakeActivePreset.id;
  const scope = existing ? existing.credentialScope : migration ? '' : id;
  const { kept } = filterPresetFields(JSON.parse(JSON.stringify(fakeConfig)));
  const preset = {
    id,
    name: String(name).trim(),
    credentialScope: scope,
    savedAt: new Date().toISOString(),
    fields: kept,
  };
  fakePresets = fakePresets.filter((p) => p.id !== id).concat(preset);
  fakeActivePreset = { id, credentialScope: scope, appliedAt: preset.savedAt };
  return JSON.parse(JSON.stringify(preset));
}

/**
 * Applies a preset onto the live config and RETURNS THE MERGED CONFIG — see
 * the section header: the caller must assign it over its whole cached config.
 *
 * The fake refuses while the fake sender is running, exactly as Go does,
 * because the refusal is load-bearing: a mid-match apply leaves the feed going
 * to the previous instance with every lamp green.
 */
export async function applyPreset(id) {
  if (hasWails()) return callGoBound(PRESET_METHODS.apply, id);

  if (fakeSenderRunning) {
    throw new Error(
      'wslcomms: a preset cannot be applied while the feed is SENDING — press STOP first (fake)',
    );
  }
  const preset = fakePresets.find((p) => p.id === id);
  if (!preset) throw new Error(`presets: no preset ${JSON.stringify(id)} (fake)`);

  const { kept, ignored } = filterPresetFields(preset.fields);
  const merged = JSON.parse(JSON.stringify(fakeConfig));
  for (const [key, value] of Object.entries(kept)) {
    if (key === 'monitorTile' && value && typeof value === 'object') {
      // Field-by-field, as Go's unmarshal-onto-live-struct merge does: a
      // partial tile in a hand-edited file updates only the fields it names.
      merged.monitorTile = { ...merged.monitorTile, ...value };
    } else {
      merged[key] = JSON.parse(JSON.stringify(value));
    }
  }
  fakeConfig = merged;
  fakeActivePreset = {
    id: preset.id,
    credentialScope: preset.credentialScope,
    appliedAt: new Date().toISOString(),
  };
  if (ignored.length > 0) {
    // The banner surfacing, exactly as Go emits it on the "error" event.
    fakeEmit(
      EVENT_ERROR,
      `wslcomms: preset "${preset.name}" carried keys that are not part of a preset and were ` +
        `ignored: ${ignored.join(', ')} (fake)`,
    );
  }
  return JSON.parse(JSON.stringify(merged));
}

/** Renames a preset. The id and the credential scope never change with it. */
export async function renamePreset(id, name) {
  if (hasWails()) return callGoBound(PRESET_METHODS.rename, id, name);
  const preset = fakePresets.find((p) => p.id === id);
  if (!preset) throw new Error(`presets: no preset ${JSON.stringify(id)} (fake)`);
  const trimmed = String(name).trim();
  if (!trimmed) throw new Error('presets: a preset needs a name (fake)');
  preset.name = trimmed;
}

/**
 * Deletes a preset, refusing the ACTIVE one and refusing to delete the legacy
 * scope's credentials — both refusals mirrored from Go so the dev loop shows
 * the same behaviour the operator gets.
 */
export async function deletePreset(id, alsoDeleteCredentials) {
  if (hasWails()) return callGoBound(PRESET_METHODS.delete, id, alsoDeleteCredentials === true);
  const preset = fakePresets.find((p) => p.id === id);
  if (!preset) throw new Error(`presets: no preset ${JSON.stringify(id)} (fake)`);
  if (fakeActivePreset.id === id) {
    throw new Error('wslcomms: this preset is the ACTIVE one — apply another preset first (fake)');
  }
  if (alsoDeleteCredentials && preset.credentialScope === '') {
    throw new Error(
      "wslcomms: this preset uses the machine's original Credential Manager entries; they are " +
        'not deleted with it (fake)',
    );
  }
  fakePresets = fakePresets.filter((p) => p.id !== id);
}

/**
 * Returns {id, credentialScope, appliedAt}: which preset this PC is pointed
 * at. The zero record (empty id) means none — the legacy vault entries.
 */
export async function getActivePreset() {
  if (hasWails()) return callGoBound(PRESET_METHODS.active);
  return { ...fakeActivePreset };
}

/**
 * Returns {scope, m2lx, srt, srtreturn}: whether each credential EXISTS for
 * the active scope. Booleans, never values — this is the one recorded
 * exception to "no secret crosses the boundary outbound", and it exists
 * because after applying a preset the operator has to know whether to type
 * the passwords, and the "set this session" badge cannot answer for a scope
 * never written to in this run.
 *
 * The fake answers from its session-write flags, which is as much as a
 * browser tab can honestly claim to know.
 */
export async function getPresetCredentialStatus() {
  if (hasWails()) return callGoBound(PRESET_METHODS.credentials);
  return {
    scope: fakeActivePreset.credentialScope,
    m2lx: !!secretSetThisSession[SECRET_KEY_M2LX],
    srt: !!secretSetThisSession[SECRET_KEY_SRT],
    srtreturn: !!secretSetThisSession[SECRET_KEY_SRT_RETURN],
  };
}

// ---------------------------------------------------------------------------
// Remote access administration
// ---------------------------------------------------------------------------
//
// Two bindings, mirroring app_remote.go's host-only remote-admin surface: read
// the listener's state, and set whether it runs and on what address/ports. The
// listener is UNAUTHENTICATED by the owner's decision — it runs on a dedicated
// private facility network and the network is the access control — so there are
// NO client accounts, NO capability tiers and no per-client admin methods. See
// docs/remote-access.md.
//
// ===================== THESE ARE HOST-ONLY, AND THAT SHAPES THIS ============
//
// On the Go side both are HostOnly: the remote dispatcher refuses them from
// every connection, and the hello frame OMITS them, so on a remote browser
// window.go.main.App does not carry them at all — a listener that could be
// reconfigured by one of its own remote clients is one that whoever first gets
// in can widen to the world. So these exist for the LOCAL Settings screen only,
// which is why settings.js hides the whole "Remote access" group when
// isRemoteClient(), and why the calls below go through callGoBound: against a
// remote client (or an older host build without the bindings) they are simply
// absent, and BindingMissingError is the honest report of that.
//
// remoteAvailable() decides whether the Settings group offers itself against a
// real build, all-or-nothing for the reason presetsAvailable/pictureAvailable
// spell out: a build with GetRemoteState but not SetRemoteListener would draw
// the panel and then fail. Against the fake backend the group is offered and
// driven by the in-memory store below, so the dev loop can exercise it without a
// Wails build.

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const REMOTE_METHODS = Object.freeze({
  state: 'GetRemoteState',
  setListener: 'SetRemoteListener',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const REMOTE_METHOD_NAMES = Object.freeze(Object.values(REMOTE_METHODS));

/**
 * remoteAvailable reports whether this build has the remote-access administration
 * bindings at all. False against a remote client (they are pruned host-only) and
 * against an older host build without them; the Settings group uses it to say why
 * rather than offering controls that always fail.
 */
export function remoteAvailable() {
  return REMOTE_METHOD_NAMES.every(hasBinding);
}

// The fake remote-access store, mirroring internal/remote's OPEN defaults
// (enabled, all-interfaces, well-known ports with 8080/8443 fallbacks) so a
// `npm run dev` session shows the same starting posture the real Settings screen
// does. There are no client records: the listener is unauthenticated.
let fakeRemote = { enabled: true, bind: '0.0.0.0', httpPort: 80, httpsPort: 443 };

/** fakeRemoteState mirrors app_remote.go's RemoteState shape for the dev loop. */
function fakeRemoteState() {
  return {
    enabled: fakeRemote.enabled,
    bind: fakeRemote.bind,
    httpPort: fakeRemote.httpPort,
    httpsPort: fakeRemote.httpsPort,
    // The fake never binds a real socket, so it is never "running" (the Settings
    // readout derives running from certFingerprint !== '') and has no bound URLs
    // or certificate to report — an honest answer, not an invented one.
    httpURL: '',
    httpsURL: '',
    certFingerprint: '',
  };
}

/**
 * Reads the remote-access configuration and live status for the Settings screen:
 * {enabled, bind, httpPort, httpsPort, httpURL, httpsURL, certFingerprint}. There
 * is no client list and no running flag — the readout derives "running" from a
 * non-empty certFingerprint, and when running the ports and URLs reflect the
 * ACTUALLY bound values (a fallback moves 80/443 to 8080/8443).
 */
export async function getRemoteState() {
  if (hasWails()) return callGoBound(REMOTE_METHODS.state);
  return fakeRemoteState();
}

/**
 * Enables or disables the listener and sets where it binds and on which HTTP and
 * HTTPS ports, then (on the Go side) validates and restarts it. Validation is
 * Go's: a bind that is not a literal IP, or a port out of 0..65535, is refused
 * with its reason. There is no non-loopback refusal — a wildcard bind is the
 * intended default.
 */
export async function setRemoteListener(enabled, bind, httpPort, httpsPort) {
  if (hasWails()) {
    return callGoBound(
      REMOTE_METHODS.setListener,
      enabled === true,
      String(bind),
      Number(httpPort),
      Number(httpsPort),
    );
  }
  fakeRemote.enabled = enabled === true;
  fakeRemote.bind = String(bind).trim();
  fakeRemote.httpPort = Number(httpPort);
  fakeRemote.httpsPort = Number(httpsPort);
}
