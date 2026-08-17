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
// by the fake backend: a table of plausible input devices between them
// presenting 1, 2, 3, 8, 16 and 32 channels (the five that mirror
// internal/gst/gst_stub.go's defaultStubDevices, plus four that reach the
// widths those five cannot), a config seeded from internal/config's documented
// defaults (Defaults(), spec section 9), and secrets that report "not set"
// until this module's own setSecret fake has been called in the running
// session.
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
// # THE FAKE CAPTURE IS LIVE FROM MODULE LOAD, AND THAT IS THE POINT
//
// Capture is no longer built at START. The real application builds its picture
// and commentary capture pipelines when the window is ready and holds them to
// quit, so the meters move, the preview paints, the pad has negotiated a width
// and the signal watchdog is reporting before anybody presses anything — and
// all five survive STOP. A fake that stood its card up inside start() and took
// it down inside stop() modelled the OLD lifetime, which means every screen
// built against it would be developed against a state machine the product no
// longer has: the routing grid would only ever be seen while sending, and
// "does this survive STOP" — the one question this whole change exists to
// answer yes to — would be untestable in the dev loop. So fakeCaptureUp() runs
// at the foot of this file, not inside fakeStart(), and fakeStop() takes
// nothing away.
//
// It starts only in a BROWSER. A `node --test` run imports this module with no
// `window`, and a ticker started there keeps the process alive for ever; the
// suite would hang rather than fail, which is the worst way to find out. See
// fakeBrowserSession.
//
// For manual poking from the browser devtools console while running against
// the fakes, the fake event bus is exposed as `window.__wslcommsFake` with
// `emitStatus(status)`, `emitSender(state)`, `emitError(message)`,
// `setDevices(list)`, `setDeviceError(message|null)`, `setSignal(state, flaps)`
// and `selectInput(kind, id)`. It is never created when a real Wails runtime is
// detected.

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
//
// The UltraStudio pair is ONE PHYSICAL BOX seen twice: enumerated through
// CoreAudio and through the DeckLink driver, the same name from both, exactly
// as it was measured. They are in the fake because that pair is the case
// ui/devices.js's labelDevices exists for, and a dev session in which the list
// never collides is a dev session in which the labelling can be broken without
// anybody noticing. The CoreAudio twin is the SILENT one (-96 dBFS on all
// sixteen channels with the mic live).
//
// ===================== `channels` IS NEW, AND IT IS NOT DECORATION ==========
//
// The routing panel is drawn for EVERY negotiated width, not only for a card —
// "we could have a multitrack system device. For example a focusrite or RME
// interface", and at the other end "on a mono you may want to route it to be
// dual mono". So the widths this table can present ARE the dev loop's coverage
// of the routing screen, and the five mirrored entries can only present 2 and
// 16 between them. The four entries below the rule are here to reach 1, 3, 8
// and 32 — every width the plan measured a matrix working at, and 32 is
// gst.MaxInputChannels itself.
//
// This field is the JS fake's own; the Go stub does not publish it yet. When it
// does (as gst.Device.Channels, read from structure 0 of the enumerated device's
// caps) the two tables must agree, and the mirror claim above becomes checkable
// rather than a promise.
const FAKE_DEVICES = [
  {
    id: '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}',
    name: 'DVS Receive  1-2 (Dante Virtual Soundcard)',
    kind: 'native',
    channels: 2,
  },
  {
    id: '{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}',
    name: 'DVS Receive  3-4 (Dante Virtual Soundcard)',
    kind: 'native',
    channels: 2,
  },
  {
    id: '{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}',
    name: 'Microphone (Focusrite Scarlett 2i2 USB)',
    kind: 'native',
    channels: 2,
  },
  {
    id: '{0.0.1.00000000}.{4b1e77a2-0004-438e-9003-51a46e13b7e0}',
    name: 'Blackmagic UltraStudio 4K Mini',
    kind: 'native',
    channels: 16,
  },
  {
    // A BARE PERSISTENT-ID, not a WASAPI endpoint GUID, because that is what
    // GStreamer's decklink provider actually publishes: a gint64 rendered as
    // decimal, with no prefix and no braces, and 2747401380 is the value
    // measured off the real UltraStudio 4K Mini. An endpoint-shaped id here
    // would teach a dev session that the two families look alike, which is the
    // one thing about them that is not true — and it is why the endpoint
    // classifier identifies POSITIVELY in both directions rather than refusing
    // whatever it does not recognise.
    id: '2747401380',
    name: 'Blackmagic UltraStudio 4K Mini',
    kind: 'decklink',
    channels: 16,
  },

  // --- the widths the mirrored five cannot reach -----------------------------

  {
    // FAKE_DEFAULT_INPUT_ID: what an unconfigured seat gets. One channel, so the
    // dev loop OPENS on the mono case — the width with the newest copy on it
    // (dual mono is DefaultChannelMap's answer for a one-channel device) and the
    // one nothing exercised while the routing screen was a card screen.
    id: '{0.0.1.00000000}.{1c7d40ef-0004-438e-9003-51a46e13c2f4}',
    name: 'Built-in Microphone',
    kind: 'native',
    channels: 1,
  },
  {
    // Three, and the count is not academic: a real 3-channel CoreAudio device
    // probed 3 and negotiated 3 with channel-mask=0x0 on the measured machine,
    // which is the evidence that an ordinary system device is unpositioned in
    // exactly the way the card is. It is also the only ODD width here, and an
    // odd width is where a grid that assumes pairs falls over.
    id: '{0.0.1.00000000}.{7e05b3a9-0004-438e-9003-51a46e13d5b6}',
    name: 'Aggregate Device (mic + loopback)',
    kind: 'native',
    channels: 3,
  },
  {
    // The operator's own example of why the panel cannot be a DeckLink panel.
    id: '{0.0.1.00000000}.{a248c61d-0004-438e-9003-51a46e13e8c7}',
    name: 'Focusrite Scarlett 18i8 USB',
    kind: 'native',
    channels: 8,
  },
  {
    // 32 = gst.MaxInputChannels. Anything wider is refused BY NAME at selection,
    // off air, rather than by a Start that fails. Note for whoever raises
    // channelmap.js's MAX_INPUT_CHANNELS from 16: until that lands this device
    // draws a 16-wide grid over a 32-wide pad, which is the clamp doing its job
    // and not the fake lying.
    id: '{0.0.1.00000000}.{d9b12f70-0004-438e-9003-51a46e13f9d8}',
    name: 'DVS Receive  1-32 (Dante Virtual Soundcard)',
    kind: 'native',
    channels: 32,
  },
];

/**
 * FAKE_DEFAULT_INPUT_ID is which device an EMPTY native audioDeviceId opens.
 *
 * Empty is not "no device". osxaudiosrc and wasapi2src with no `device` property
 * open the PLATFORM DEFAULT INPUT, which is why config.Validate requires an id
 * for a native seat but the pipeline still comes up without one — and why the
 * fake must resolve it to a real entry rather than reporting nothing negotiated.
 * A fake that treated "" as "no capture" would show a first-run dev session an
 * empty screen where the product shows a live mono meter.
 */
const FAKE_DEFAULT_INPUT_ID = '{0.0.1.00000000}.{1c7d40ef-0004-438e-9003-51a46e13c2f4}';

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
    // config.DefaultAudioSourceKind. Spelled out rather than left absent because
    // the fake capture opens from these three fields at module load, and "the
    // field is missing" and "the field says native" have to be the same thing
    // here for the same reason EffectiveAudioSourceKind makes them the same
    // thing in Go.
    audioSourceKind: 'native',
    decklinkPersistentId: '',
    videoSource: 'slate', // config.DefaultVideoSource — the still slate, as ever
    decklinkPreviewEnabled: false, // config's default: the operator's monitor starts off
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

// The fake switcher's conform target, answered by getConformTarget(). 1080p50
// with the provenance of a raster derived off one running node, because that is
// the shape a healthy instance actually produces — a `npm run dev` session then
// shows the DERIVED path agreeing with the fake status events, rather than a
// green lamp that is really the fallback in disguise and would stay green
// however badly the derivation were wired.
//
// From the console: window.__wslcommsFake.setConformTarget({width: 1280,
// height: 720, frameRate: 50}) for the 720p50 instance the old constant got
// wrong, or null for the not-known case.
//
// source is "session" and NOT "switcher", which it used to say. app.go's
// GetConformTarget can only ever stamp "session" or "override" — the third
// constant, "switcher", belongs to GetSwitcherFormat and that method's doc says
// in terms that GetConformTarget never returns it. A fake answering with a
// provenance the real binding cannot produce is a dev session in which every
// readout that reads `source` is exercised down a branch the product does not
// have; videoformat.js's describeConformTarget reads exactly that field, so the
// dev session would have shown "not read yet (press START)" for ever while the
// shipped build showed the raster, or the reverse. Fake below, real fake for the
// switcher's own setting.
let fakeConformTarget = {
  width: 1920,
  height: 1080,
  frameRate: 50,
  source: 'session',
  node: 'cam4',
  agreeing: 1,
  disagreeing: [],
  raw: 'codec="h264" width=1920 height=1080 frame_rate="50" scan_type="P"',
};

// The fake instance's OWN CONFIGURED FORMAT, answered by getSwitcherFormat().
//
// It is a separate variable from fakeConformTarget on purpose, and the two are
// deliberately allowed to disagree: that disagreement is the only way a dev
// session can see the divergence readout the Settings screen exists to show.
// Set it to null from the console for the unreachable-instance case, which is
// what every seat sees before it has signed in.
//
// From the console: window.__wslcommsFake.setSwitcherFormat({width: 1280,
// height: 720, frameRate: 50}) or setSwitcherFormat(null).
let fakeSwitcherFormat = {
  width: 1920,
  height: 1080,
  frameRate: 50,
  source: 'switcher',
  raw: '1920x1080p50',
};

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
// over six seconds at 20 frames a second, RMS 8 dB under the peak. Mirrored by
// value rather than imported from anywhere because there is nowhere to import
// it from — the Go stub is the other side of the boundary — and matching it
// means a dev session in the browser and a Gate A session in the app show the
// same moving meters. The final all-silence frame mirrors app.go's zero-frame:
// -100 dBFS per channel, the clamped floor, so the meters fall rather than
// freeze. It is emitted when CAPTURE goes down — a device change, a restart, a
// quit — and no longer when the session stops, because the session stopping no
// longer takes the microphone away.
//
// ===================== THE PROGRAMME METER IS COMPUTED, NOT INVENTED ========
//
// alevel sits BELOW the mix matrix in the real pipeline, so the stereo pair it
// meters is the routing grid's own output. The fake reproduces that: the
// per-channel frame is generated first, and the programme frame is the channel
// frame REDUCED THROUGH THE MAP IN FORCE.
//
// That is what makes a wrong matrix visible in a dev session. Rows are outputs
// and columns are inputs, a transpose is invisible at 2x2, and a fake whose
// channels all moved together — or whose programme meter was its own
// independent waveform — would show a flipped stereo pair, a dual mono and a
// correct routing as three identical pictures. Here, flipping L and R swaps the
// two programme bars, dual mono makes them equal, and routing to a silent input
// drops one to the floor, live, in the next frame. The real thing does the same
// thing in 119 us, which is why the screen that writes the map has no Apply
// button.
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

/**
 * startFakeLevels drives the PROGRAMME meter, at the real alevel's 50 ms
 * interval, off whatever the per-channel generator is producing this step and
 * whatever routing the grid has written.
 *
 * It shares fakeLevelsStep with the per-channel ticker rather than counting its
 * own: two counters would let the two meters disagree about the same instant,
 * and "the input meter moved but the programme meter did not" is a real fault
 * this fake would then be unable to tell from a rounding difference.
 */
function startFakeLevels() {
  if (fakeLevelsInterval || !fakeBrowserSession) return;
  fakeLevelsStep = 0;
  fakeLevelsInterval = setInterval(() => {
    const peak = fakeProgrammePeaksAt(fakeLevelsStep);
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
  // The zero-frame, exactly as app.go emits one when a capture pipeline goes
  // down: the meters must fall to silence, not freeze at the last level.
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
  // START TOUCHES NOTHING ABOUT CAPTURE, and the absence is the model.
  //
  // The meters are already moving, the pad has already negotiated, the signal
  // is already being reported and the preview is already painting — capture came
  // up at module load, an hour before this in a real seat. What START builds is
  // the send pipeline alone. Anything here that started a meter would be
  // reintroducing the lifetime the split removed, and the first symptom would be
  // a dev session in which the routing grid still needs a START to size itself.
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
  // AND STOP TAKES NOTHING AWAY. Meters, per-channel meters, the negotiated
  // width, the routing, the signal lamp and the cough mute all stand. That is
  // the operator-visible half of the change — "everything you were watching
  // stays" — and it is the assertion the dev loop is here to make available:
  // a fake that silenced the meters on STOP would agree with the old product
  // and disagree with this one, and nobody would notice until the rig.
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
    // Drive the VIDEO OK lamp's comparison target: an object for a switcher
    // configured that way, null for "not known" so the lamp falls back to
    // lamps.js's DEFAULT_CONFORM_TARGET. Pair it with emitStatus to see a
    // 720p50 instance read green on a 720p50 feed, which the old constant made
    // impossible.
    setConformTarget: (format) => {
      fakeConformTarget = format && typeof format === 'object' ? { ...format } : null;
    },
    // Drive the Settings screen's SWITCHER readout and its divergence marking,
    // which is a different question from the lamp's and has its own binding.
    // setSwitcherFormat(null) is the state every seat is in before it has
    // signed in, and the one the readout has to be honest about.
    setSwitcherFormat: (format) => {
      fakeSwitcherFormat = format && typeof format === 'object' ? { ...format } : null;
    },
    // Drive the DeckLink routing screen without a card.
    //
    // setSignal('LOST') is the one worth reaching for: a real signal loss is
    // debounced in Go and reported once in a ninety-minute match, so the red and
    // the flapping-input amber are otherwise unreachable in a dev session.
    // setSignal('OK', 6) is the marginal cable — an OK report the watchdog was
    // FORCED to send because the raw reading rattled.
    //
    // setChannelCount(8) is the other: a pad that negotiates fewer channels than
    // the saved map was written against is what the dropped-routing warning
    // exists for, and 0 is the "press START once" state.
    setSignal: (state, flaps) => {
      fakeSignal = { state: String(state || SIGNAL_STATE.UNKNOWN), flaps: Number(flaps) || 0 };
      fakeEmit(EVENT_SIGNAL, { ...fakeSignal });
    },
    // setChannelCount(8) forces a width the device table does not offer, for the
    // narrower question of what a saved map wider than the pad does. It is a LIE
    // to the fake — the device on screen still says what it says — so reach for
    // selectInput first and this only when no device has the width you want.
    setChannelCount: (channels) => {
      fakeCommentaryWidth = fakeClampWidth(channels);
      fakeEmit(EVENT_CHANNEL_MAP, fakeChannelMapState());
    },
    // selectInput('native', id) re-points the fake commentary capture the way
    // the Settings picker does, WITHOUT saving — which is the interaction R2
    // exists for: the grid must re-size before anybody presses Save. Called with
    // no id it takes the platform default input.
    selectInput: (kind, id) => selectCommentaryInput(kind, id, ''),
    // restartCapture() from the console is the cable-fault recovery: the card is
    // held from launch to quit, so this is the only way back short of a reload.
    restartCapture: () => restartCapture(),
    captureState: () => ({ ...fakeCapture }),
  };
}

// ---------------------------------------------------------------------------
// Public API — every export tries the real Wails runtime first and falls
// back to the fake above. Callers never need to know which one answered.
// ---------------------------------------------------------------------------

/** True once, at load time, if this session is running against the fakes. */
export const usingFakeBackend = !hasWails();

/**
 * fakeBrowserSession is usingFakeBackend AND a real browser — the only
 * condition under which the fake may start a ticker.
 *
 * `node --test` imports this module with no `window` at all, so every fake is
 * reachable from the suite; but setInterval there holds the event loop open and
 * the run never exits. It does not fail either. It HANGS, which is the failure
 * mode that costs an afternoon rather than a minute, and it would arrive the
 * moment capture became live at module load. So the tickers ask this, not
 * usingFakeBackend, and a source-reading test that imports this file gets the
 * fake's data and none of its clocks.
 */
const fakeBrowserSession = usingFakeBackend && typeof window !== 'undefined';

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

/**
 * Returns the audio capture endpoints for the commentary input dropdown:
 * [{id, name, kind}], mirroring internal/gst.Device.
 *
 * `kind` is "native" (the platform's own audio system) or "decklink" (a capture
 * card's embedded audio), and it matters because ONE PHYSICAL BOX CAN APPEAR
 * TWICE under the same name — measured on an UltraStudio 4K Mini, where the
 * DeckLink entry carries the microphone and the CoreAudio entry carries -96 dBFS
 * of nothing. ui/devices.js's labelDevices is what turns that field into
 * something an operator can choose between; nothing may infer it from the id,
 * which internal/gst/gst.go documents as opaque.
 *
 * An older Go build omits the field entirely, and the labelling degrades to the
 * bare device name rather than guessing.
 */
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
  const before = fakeConfig;
  fakeConfig = JSON.parse(JSON.stringify(config));
  // A save that changes what capture is BUILT FROM rebuilds it, which is what
  // app.go does through rebuildCapture. Only these fields; a save is a whole
  // document and rebuilding on every one of them would blank the picture every
  // time somebody corrects a typo in the alias.
  const captureFields = [
    'videoSource',
    'decklinkPreviewEnabled',
    'audioSourceKind',
    'audioDeviceId',
    'decklinkPersistentId',
  ];
  if (captureFields.some((k) => before[k] !== fakeConfig[k])) {
    // The save is the moment the configuration becomes the truth again, so a
    // live selection made without one is superseded rather than carried: the
    // form that was just saved contains the device, and rebuilding from a stale
    // live selection would ignore the save that named a different one.
    fakeCommentarySelection = null;
    fakeCaptureDown();
    fakeCaptureUp();
  }
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

// App.CredentialStoreName: what THIS platform's credential vault is called, so
// a dialog can name it instead of guessing. See the bound method's own comment
// in app.go for why the answer is resolved in Go rather than switched on in
// JavaScript.

/** CredentialStoreName's bound method name. One place, so a rename is one edit. */
const CREDENTIAL_STORE_NAME_METHOD = 'CredentialStoreName';

/**
 * The name of the OS credential store, for use inside a sentence: it is always
 * the object of a preposition ("delete the stored passwords from %s"), so the
 * whole phrase comes back article and all, and the two platforms differ —
 * "Windows Credential Manager" takes none, "the macOS login Keychain" does.
 *
 * Never throws and never returns empty. A build without the binding is an older
 * one, which by definition is Windows-only, so the Windows phrase is the right
 * fallback rather than a blank that would leave a dangling "from ." in the
 * dialog. A `npm run dev` session gets the same string, since the fake backend
 * has no platform of its own.
 *
 * @returns {Promise<string>}
 */
export async function credentialStoreName() {
  if (hasWails()) {
    try {
      const got = await callGoBound(CREDENTIAL_STORE_NAME_METHOD);
      if (typeof got === 'string' && got.trim() !== '') return got;
    } catch {
      // BindingMissingError, or anything else: fall through to the default.
    }
  }
  return 'Windows Credential Manager';
}

// ---------------------------------------------------------------------------
// The conform target: what format this instance is configured for
// ---------------------------------------------------------------------------
//
// One binding, App.GetConformTarget, for the question the VIDEO OK lamp used to
// answer with a constant.
//
// # The defect this closes
//
// lamps.js compared the detected format against a hard-coded
// {h264, 1920, 1080, 50}, written when every instance anyone had seen was
// 1080p50. M2L-X's raster is a per-instance CONFIGURATION and every source
// feeding it must match, so on a correctly configured 720p50 facility that lamp
// read RED on a feed arriving perfectly — and the only remedy available at the
// desk was to learn to ignore a red lamp, which is the habit the whole row
// exists to prevent. lamps.js keeps the constant as its FALLBACK and takes the
// raster from here whenever it is known. See DEFAULT_CONFORM_TARGET there.
//
// # Where Go gets the answer, and why this side does not care
//
// The resolution is internal/m2lx's (ConformFormat) and internal/config's
// (VideoFormatOverride), and it is deliberately not restated here: it is a
// question about the switcher, and duplicating it in JavaScript is how the two
// answers start to disagree about which lamp is telling the truth. What this
// side is entitled to assume is only the shape below, and that a null means
// nothing is known.
//
// # A measurement worth having, recorded here because it was made here
//
// MEASURED on the live matchH instance, 2026-08-15, signed in as `matchh`:
//
//	GET /api/input/router/list/{eventId}
//	{"id":3,"name":"SLATE","type":"SRT","width":1920,"height":1080,
//	 "codec":"h265","frame_rate":50,"scan_type":"progressive","port":40003, ...}
//	{"id":4,"name":"COMMS","type":"SRT","width":1920,"height":1080,
//	 "codec":"h264","frame_rate":50,"scan_type":"progressive","port":40004, ...}
//
// Router input 4 is OUR input — port 40004 is config.srtPort on matchH, and its
// node is cam4 — and those width/height/frame_rate/codec ARE the configured
// format, stated by the switcher, available while every node's detected format
// is still JSON null because nothing has come up yet. That is exactly the case
// config.VideoFormatOverride was added to cover by hand.
//
// This does NOT breach docs/architecture.md's ban on REST format. That ban is on
// reading REST as DETECTION — width/height/frame_rate there will cheerfully
// report 1080p50 over a 720p25 stream — and detection still comes from the
// switcher_status socket alone. The field everyone was warned off as evidence is
// the right answer to a different question: what is this input CONFIGURED for.
//
// Two traps in that sample, for whoever wires it up. frame_rate is a NUMBER here
// while switcher_status renders the same quantity as the STRING "50"
// (internal/m2lx/format.go documents that); nothing may assume either, which is
// why normaliseConformTarget coerces. And EVERY router input answers with a
// plausible raster whether or not it is ours — unconfigured input 5 reports
// 1920x1080/h265/50, the defaults — so the match must be exact, on `port`
// against config.srtPort. A believable raster read off somebody else's input is
// worse than none: it turns a green lamp into a lie, where no answer at all
// merely leaves the honest fallback in place.

/** GetConformTarget's bound method name. One place, so a rename is one edit. */
const CONFORM_TARGET_METHOD = 'GetConformTarget';

/**
 * The video format every source feeding this M2L-X instance must be produced
 * in, or null when it is not known.
 *
 * Only width, height and frameRate are load-bearing — they are the three fields
 * lamps.js compares against, and normaliseConformTarget ignores everything else.
 * The rest of the object is PROVENANCE, carried so that a readout can one day
 * say where the number came from without any of this having to change: `source`
 * distinguishes an operator's videoFormatOverride from a raster derived off the
 * switcher, `node` names the switcher_status node it was read from, and
 * `agreeing`/`disagreeing` are how much the derivation had to go on. A lamp that
 * judges against a number is one thing; a lamp that judges against a number
 * nobody can trace is another, and the second is not worth having.
 *
 * NULL IS A NORMAL ANSWER and it is what this returns for every way of not
 * knowing: no Wails runtime, an older build without the binding, no host, not
 * signed in, nothing running on the switcher to derive from, or a Go-side
 * failure of any kind. The caller's response is the same to all of them — use
 * lamps.js's DEFAULT_CONFORM_TARGET — and there is deliberately no second copy
 * of that constant here. Two fallbacks for one question is how they drift, and
 * the drift would be silent: the lamp would start judging against a raster
 * nothing on screen names.
 *
 * NEVER THROWS, for the same reason credentialStoreName does not. This is a
 * refinement of a lamp, called on the page's startup path; a rejected promise
 * here must not be able to take out the status row it exists to improve.
 *
 * @returns {Promise<{width: number, height: number, frameRate: number,
 *   source?: string, node?: string, agreeing?: number, disagreeing?: string[],
 *   raw?: string}|null>}
 */
export async function getConformTarget() {
  if (hasWails()) {
    try {
      const got = await callGoBound(CONFORM_TARGET_METHOD);
      return got && typeof got === 'object' ? got : null;
    } catch {
      // BindingMissingError, a signed-out client, a REST failure: all of them
      // mean "not known", and the caller falls back for all of them alike.
      return null;
    }
  }
  return fakeConformTarget ? { ...fakeConformTarget } : null;
}

/** GetSwitcherFormat's bound method name. One place, so a rename is one edit. */
const SWITCHER_FORMAT_METHOD = 'GetSwitcherFormat';

/**
 * The video format THE M2L-X INSTANCE IS CONFIGURED FOR, read live from the
 * instance, or null when that cannot be established.
 *
 * ===================== WHY THIS IS NOT getConformTarget =====================
 *
 * They answer two different questions and the Settings screen wants both at
 * once, side by side:
 *
 *   getConformTarget    what will WE produce?   (the running pipeline's target,
 *                                                or the operator's declaration)
 *   getSwitcherFormat   what does the SWITCHER  (the instance's own setting,
 *                       require?                 read over REST)
 *
 * The whole value of showing them together is that a DIVERGENCE is visible: an
 * override typed for last month's venue against a switcher configured for this
 * one. getConformTarget cannot stand in for this, and the reason is not
 * squeamishness — with no session it reports the operator's own
 * videoFormatOverride back, stamped source="override". A readout built on that
 * would quote the operator's own setting back to them under the heading "what
 * M2L-X is configured for", which is the screen inventing a confirmation, and a
 * divergence warning that can invent a confirmation is worse than none.
 *
 * IT NEEDS NO SESSION, which is the entire point: the Settings screen is opened
 * an hour before kick-off with nothing running, and that is exactly when the
 * operator wants to see whether their override disagrees with the facility.
 *
 * NULL IS A NORMAL ANSWER, for every way of not knowing — no Wails runtime, an
 * older build without the binding, no host, not signed in, an instance that is
 * not up, a raster this build cannot render. The caller renders nothing rather
 * than a wrong number.
 *
 * NEVER THROWS, for the reason getConformTarget does not.
 *
 * @returns {Promise<{width: number, height: number, frameRate: number,
 *   source?: string, raw?: string}|null>}
 */
export async function getSwitcherFormat() {
  if (hasWails()) {
    try {
      const got = await callGoBound(SWITCHER_FORMAT_METHOD);
      return got && typeof got === 'object' ? got : null;
    } catch {
      // BindingMissingError, a signed-out client, a REST failure: all of them
      // mean "not known", and the caller renders nothing for all of them alike.
      return null;
    }
  }
  return fakeSwitcherFormat ? { ...fakeSwitcherFormat } : null;
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

// ---------------------------------------------------------------------------
// The channel map, its per-channel meters, and the card's video signal
// ---------------------------------------------------------------------------
//
// Two bindings and three events, for the three questions the routing screen
// (ui/channelmap.js, drawn inside Settings) has to answer:
//
//   HOW WIDE IS THE GRID, AND WHAT IS ROUTED   GetChannelMap / "channelMap"
//   WHICH CHANNEL IS THE COMMENTATOR ON        "channelLevels"
//   IS THE CARD SEEING A PICTURE               "signal"
//
// ===================== IT IS NOT A DECKLINK SCREEN ANY MORE =================
//
// It was, and every name in this block used to say so. The operator did not
// agree: "The channel selector only existing for decklink sources isn't right.
// We could have a multitrack system device. For example a focusrite or RME
// interface." And at the narrow end, where this side had proposed to hide it:
// "I think we always show it. You may want to flip the channels on a stereo
// source, on a mono you may want to route it to be dual mono etc."
//
// The evidence agrees with him and it is not a matter of taste. GStreamer's
// gstosxcoreaudio.c sets `layout = NULL; /* no supported for sources */`
// unconditionally for every source, so NO CoreAudio device of any width can emit
// a positioned channel mask above two channels — a Focusrite or an RME is
// byte-for-byte the same unpositioned problem as the card. Corroborated on this
// machine: a real 3-channel CoreAudio device negotiated channels=3,
// channel-mask=0x0, and a 16-in CoreAudio device negotiated channels=16,
// channel-mask=0x0, identical in shape to decklinkaudiosrc's 16.
//
// So the matrix is uniform at every width including 1 and 2, both cases are
// real routing decisions, and both are already expressible in the model: a
// stereo flip is {Left<-in 2, Right<-in 1}, and dual mono is
// {Left<-in 1, Right<-in 1} — which DefaultChannelMap already produces for a
// one-channel device. The "signal" event stays card-only, because it is a
// question about a DeckLink's video input and nothing else has one.
//
// The MODEL is internal/gst/channelmap.go's and nothing here restates it. A map
// is a LIST of contributions — [{output, input, gain}], input counted from ZERO,
// gain linear in [-1, 1] — because that is the shape the operator's intent has
// and because a list survives a change of input width without silently meaning
// something else. A dense 2x16 grid saved and reloaded against a device
// presenting eight channels has to be truncated by somebody; a list is refused
// by name.
//
// ===================== WHY THE WIDTH IS A BINDING AND NOT A CONSTANT ========
//
// A matrix whose width does not match what the capture pad NEGOTIATED kills the
// pipeline instantly — measured on the card: writing a 2x8 matrix to a pipeline
// running 2x16 gave "Internal data stream error ... streaming stopped, reason
// error (-5)", with the capture chain dead before the next level message and
// every coefficient in the matrix perfectly legal. And what a device ADVERTISES
// is not what it negotiates: the DeckLink structure publishes max-channels=16
// whatever the element is configured to produce. So the count crosses this
// boundary as the capture pipeline's InputChannels(), read from the pad's own
// caps, and ui/channelmap.js's MAX_INPUT_CHANNELS is a ceiling on that report
// rather than a size.
//
// ZERO NO LONGER MEANS "PRESS START". Capture is built at launch and the pad
// negotiates within about a tenth of a second of it — measured on the fitted
// card with no consumer, no encoder and no SRT anywhere in the process:
// aconv:sink NEGOTIATED channels=16 at t=0.1176 s, send pipeline exists: NO.
// Zero is now only ever the reopen window of a device change, or a capture that
// failed and said why on the "capture" event. It still draws no grid.
//
// ===================== THE LEVELS HERE ARE NOT THE LEVELS ON THE MAIN SCREEN =
//
// There are TWO level elements in the commentary capture pipeline and they
// answer different questions. alevel meters the stereo pair that goes on to be
// encoded and sent (EVENT_LEVELS, 50 ms) — the meter that goes quiet when the
// wrong device is selected, and the meter the cough mute flattens, because the
// mute sits immediately above it. chlevel meters the CAPTURE's own channels
// upstream of the mix down to two (EVENT_CHANNEL_LEVELS, 100 ms) — the meter
// that answers "which of these sixteen moves when I talk", which is the entire
// reason the mapping UI is usable, and it stays ABOVE the mute so a coughing
// commentator can still be located on the grid.
//
// BOTH ARE LIVE FROM LAUNCH AND BOTH SURVIVE STOP. What used to be "the send
// pipeline's own measurement" is now the capture pipeline's, taken immediately
// upstream of the proxysink rather than immediately upstream of the AAC
// encoder. In normal operation those are the same buffers. During a send-side
// stall longer than about a second they are not: the capture-side queue in
// front of each proxysink is leaky=downstream, so the far end LOSES that audio
// while these meters go on moving. That is a deliberate policy — measured, a
// non-leaky queue dragged the preview to 7.2 fps and the meters to 7.2 msg/s
// and made the card itself drop packets — and the promise that no meter can
// move while silence goes to air therefore holds in normal operation and NOT
// during a stall. A stall that long is already a reconnect-class event and the
// SENDING lamp is the thing that reports it.
//
// KEEPING THEM APART IS LOAD-BEARING, and it was measured: every level element
// in the process posts a GstStructure named "level", so a handler matching on the
// name alone sees 39 messages a second from both and would feed one meter a
// two-entry frame and a sixteen-entry frame alternately, twenty times a second
// each. internal/gst routes on msg.Source(); this side keeps the two events
// apart for the same reason and must never treat one as a fallback for the other.
//
// ARRAY INDEX i IS INPUT CHANNEL i, for the life of the pipeline — measured with
// sixteen sources at 3 dB steps, which came back in input order. Nothing on this
// side re-derives it per frame.
//
// ===================== THE SIGNAL EVENT IS ALREADY DEBOUNCED ================
//
// It exists because NOTHING ELSE IN THIS APPLICATION CAN TELL: a DeckLink that
// loses signal goes on emitting black frames at full rate forever — no error, no
// EOS, the muxer never starves — so the sender stays CONNECTED, the switcher
// reports a healthy correctly-formatted feed, the audio meters keep moving, and
// the picture going out is black with every lamp green.
//
// The hysteresis is internal/gst's, with asymmetric hold-offs measured against
// the real card, and THIS SIDE MUST NOT ADD A SECOND ONE — two filters in series
// lag a real loss by however long both take to agree. What the frontend does with
// the payload is decide what the three states LOOK like; see
// channelmap.js's deriveSignalLamp, including why UNKNOWN is never a fault.

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const CHANNEL_MAP_METHODS = Object.freeze({
  state: 'GetChannelMap',
  set: 'SetChannelMap',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const CHANNEL_MAP_METHOD_NAMES = Object.freeze(Object.values(CHANNEL_MAP_METHODS));

// EventChannelMap: {inputChannels, map, isDefault, deviceKey} — what the capture
// pad negotiated, the routing in force, and WHICH DEVICE both of those are
// about. Emitted when the pad negotiates, which is now when the commentary
// capture is built or rebuilt: at launch, and again on every device change. A
// Settings screen left open across either one re-sizes its grid without being
// reopened, and a screen opened before anything happened is already sized.
//
// ===================== deviceKey IS NOT OPTIONAL ============================
//
// It is `${kind}:${id}` — the same two fields, in the same order, that
// ui/audioinput.js encodes into an <option> value — and without it there is a
// window with no honest way through it.
//
// IT IS THE CONFIGURED PAIR, VERBATIM, NEVER THE RESOLVED DEVICE. An empty
// native id means the platform default input and an empty decklinkPersistentId
// means the only card, and both of those RESOLVE to some particular box — but
// the key must stay `native:` and `decklink:`, because the only thing it is ever
// compared against is the picker's own <option> value, which is built from the
// configuration and not from what opened. Stamping the resolved id instead would
// mean an unconfigured seat's key matched no option on the screen, and the
// routing panel would never appear at all on the one seat that has not been set
// up yet.
//
// Selecting a Focusrite while a card is open does not re-negotiate instantly.
// For the length of that reopen the last width anybody published is the CARD's
// 16, and a grid that believes it offers sixteen crosspoints over a
// two-channel pad. Press one and the map written is 2x16. The measurement on
// the real card: "Internal data stream error ... streaming stopped, reason
// error (-5)", the capture chain dead before the next level message, every
// coefficient in the matrix perfectly legal. Go's SetChannelMap validates the
// map against InputChannels() and refuses one that does not fit, so the write
// itself is safe — but a refusal is not a good answer to a button that should
// never have been on screen, and refusal cannot help the OTHER half of the
// problem at all: a narrowed grid collected at Save writes a 2-wide routing
// over the card's saved 16-wide one, silently, with a commentator's channel
// assignment in it.
//
// So the width and the identity travel together, in one payload, and the screen
// draws a grid only while the key it holds is the key of the device the picker
// is showing. That is also why the saved routing is per-device
// (config.ChannelMaps, keyed the same way): a single slot cannot say which
// device its contents belong to.
export const EVENT_CHANNEL_MAP = 'channelMap';

// EventChannelLevels: {peak: number[], rms: number[]}, one entry per NEGOTIATED
// capture channel, dBFS, silence clamped to -100 — the same wire shape as
// EVENT_LEVELS on purpose, because both meters are drawn by the same model
// (ui/meters.js) and a second frame shape would be a second scale waiting to
// happen. Ten frames a second, not twenty: speech syllables run 150-250 ms, so a
// talker is unmistakable at that rate, and the traffic is linear in channels
// TIMES rate on a bridge that also carries the programme meter.
export const EVENT_CHANNEL_LEVELS = 'channelLevels';

// EventSignal: {state, flaps}, mirroring app.go's signalPayload. state is one of
// SIGNAL_STATE below; flaps is how many times the RAW property reading changed
// between the previous report and this one, which is how a marginal input shows
// through a hysteresis that deliberately hides it.
export const EVENT_SIGNAL = 'signal';

/**
 * SIGNAL_STATE mirrors gst.SignalState EXACTLY, and it is UPPERCASE.
 *
 * UNKNOWN IS NOT A FAULT. It is the state of every machine with no capture card
 * in it — which is every machine running this application today — and of every
 * session before the first measurement. It means this application cannot tell,
 * which is a different claim from a card telling us there is nothing there, and
 * the difference is why the payload carries three states rather than a boolean.
 */
export const SIGNAL_STATE = Object.freeze({
  UNKNOWN: 'UNKNOWN',
  OK: 'OK',
  LOST: 'LOST',
});

/**
 * channelMapAvailable reports whether this build can route the card's channels
 * at all.
 *
 * BOTH, for the reason presetsAvailable and pictureAvailable spell out: reading
 * the pad's width and writing a map are two halves of one control. A build that
 * could report sixteen channels but not set a map would draw a full grid of
 * buttons that throw on the first press — a routing screen that cannot route,
 * offered to somebody looking for the commentator they cannot hear.
 */
export function channelMapAvailable() {
  return CHANNEL_MAP_METHOD_NAMES.every(hasBinding);
}

// --- the fake pad, its per-channel meters, and the saved routing ------------
//
// A `npm run dev` session has no devices at all, so the fake negotiates a pad
// for whichever entry of FAKE_DEVICES the commentary capture is pointed at, and
// emits per-channel frames in which only SOME channels carry audio and no two
// of them move together.
//
// BOTH HALVES OF THAT ASYMMETRY ARE LOAD-BEARING and they answer different
// failures. Only-some-channels-live is the find-the-commentator interaction: a
// fake where all sixteen moved would let it break without anybody noticing in
// the dev loop, the same trap internal/gst's stubChannelLevelsAt documents when
// it refuses to reuse the stereo stub's quarter-period phase rule. And
// no-two-alike is the matrix itself: rows are outputs, columns are inputs, a
// transpose is invisible at 2x2, and identical channels would make a flipped
// map, a dual mono and a correct routing look the same on the programme meter
// they all feed.

/**
 * Which fake input channels carry audio on a device with more than two of them.
 * Zero-based, so these draw as Ch 3, Ch 8 and Ch 24 — never Ch 1 and Ch 2, so
 * the default routing is deliberately NOT the answer and the operator has to
 * use the grid. Trimmed to the negotiated width by fakeLiveChannelsFor.
 */
const FAKE_LIVE_CHANNELS = [2, 7, 23];

/**
 * FAKE_CHANNEL_PHASE_STEPS is how far apart in the triangle two adjacent input
 * channels sit. 13 is coprime with FAKE_LEVELS_PERIOD's 120, so no two of the
 * 32 channels this fake can present are ever in step — which is the property
 * the transpose argument above rests on, and it is why this is not the stereo
 * fake's quarter-period offset (that repeats every four channels).
 */
const FAKE_CHANNEL_PHASE_STEPS = 13;

/**
 * FAKE_MAX_INPUT_CHANNELS mirrors gst.MaxInputChannels, raised from 16 to 32
 * with the measurement beside it: a 2x32 mix-matrix passes audio and `level`
 * reports 32 rms entries per message, verified on the measured machine, and
 * levelMaxChannels is already 64. Wider than this is a NAMED REFUSAL of that
 * device at selection time, off air — never a Start that refuses.
 */
const FAKE_MAX_INPUT_CHANNELS = 32;

/**
 * fakeCommentaryWidth is what the fake commentary pad has negotiated, and
 * fakeCommentaryKey is WHICH DEVICE negotiated it. They move together, always,
 * for the reason EVENT_CHANNEL_MAP's comment gives at length: a width without
 * an identity is a grid that can be pressed against the wrong pad.
 *
 * Zero is the reopen window of a device change and the state of a capture that
 * failed — no longer "nobody has pressed START".
 */
let fakeCommentaryWidth = 0;
let fakeCommentaryKey = '';

/**
 * fakeChannelMaps is the saved routing, PER DEVICE, keyed `${kind}:${id}` —
 * mirroring config.ChannelMaps, which replaced the single decklinkChannelMap
 * slot for a reason this fake has to be able to demonstrate.
 *
 * With always-live capture, selecting a 2-channel microphone to check something
 * and then pressing Save on an unrelated field would, with one slot, overwrite
 * the card's 16-channel routing with a 2-wide one — silently, with a
 * commentator's channel assignment in it. A dev session against a single-slot
 * fake would show that as working.
 */
const fakeChannelMaps = new Map();

let fakeChannelLevelsInterval = null;
let fakeSignal = { state: SIGNAL_STATE.UNKNOWN, flaps: 0 };

/**
 * The device key: `${kind}:${id}`, trimmed, with an unrecognised kind reading as
 * native.
 *
 * THREE PLACES SPELL THIS AND ALL THREE MUST AGREE — audioinput.js's
 * encodeAudioInput (which builds it for every <option> the picker lists),
 * config.AudioDeviceKeyFor in Go (which files the saved routing under it), and
 * this. A key spelled two ways is a routing filed where nothing will look for it
 * again: no error, no refusal, just a grid that comes up empty the next morning
 * with the operator's channels still in the file. The trim is Go's; without it
 * an id with trailing whitespace files under a key the other two cannot build.
 */
function fakeDeviceKey(kind, id) {
  const k = kind === 'decklink' ? 'decklink' : 'native';
  return `${k}:${typeof id === 'string' ? id.trim() : ''}`;
}

/** A reported width, clamped the way gst.MaxInputChannels clamps one. */
function fakeClampWidth(channels) {
  const n = Math.floor(Number(channels));
  if (!Number.isFinite(n) || n <= 0) return 0;
  return Math.min(n, FAKE_MAX_INPUT_CHANNELS);
}

/**
 * fakeLiveChannelsFor decides which inputs carry audio at a given width.
 *
 * At 1 and 2 the device IS the commentator — a mono mic, or a stereo pair off a
 * desk — so every channel it has carries audio, and the question the grid
 * answers there is not WHICH channel but which way round they come out. Above
 * two, the commentator is on some pair among many and finding it is the whole
 * job.
 */
function fakeLiveChannelsFor(width) {
  if (width <= 0) return [];
  if (width <= 2) return Array.from({ length: width }, (_, i) => i);
  return FAKE_LIVE_CHANNELS.filter((i) => i < width);
}

/** One per-channel peak frame, in dBFS, at the shared 50 ms step. */
function fakeChannelPeaksAt(step, width) {
  const live = fakeLiveChannelsFor(width);
  const peak = [];
  for (let i = 0; i < width; i += 1) {
    // The silent channels emit the clamped floor rather than nothing at all: an
    // absent entry and a silent one must look different, and only the second is
    // what a live device with nothing plugged into that pair actually sends.
    peak.push(
      live.includes(i)
        ? fakeLevelAt(step + i * FAKE_CHANNEL_PHASE_STEPS, 0)
        : FAKE_LEVELS_SILENCE_DB,
    );
  }
  return peak;
}

/**
 * fakeDefaultChannelMap is gst.DefaultChannelMap's answer, restated here
 * because the fake has no Go side to ask: input 1 to Left, input 2 to Right —
 * and on a ONE-CHANNEL device, input 1 to BOTH. That last case is dual mono,
 * it is what an operator gets without touching anything, and it is one of the
 * two narrow cases the operator ruled the panel must be drawn for.
 */
function fakeDefaultChannelMap(width) {
  if (width <= 0) return [];
  return [
    { output: 0, input: 0, gain: 1 },
    { output: 1, input: width >= 2 ? 1 : 0, gain: 1 },
  ];
}

/** The routing actually in force for the current device: saved, or the default. */
function fakeEffectiveChannelMap(width) {
  const saved = fakeChannelMaps.get(fakeCommentaryKey);
  return Array.isArray(saved) && saved.length > 0 ? saved : fakeDefaultChannelMap(width);
}

/**
 * fakeProgrammePeaksAt reduces this step's per-channel frame through the map in
 * force, which is what alevel is measuring in the real pipeline.
 *
 * The reduction is a MAX over the contributions rather than a sum. Two
 * correlated signals summed would need a phase-accurate model this fake has no
 * business having, and every question the meter is asked here — did the flip
 * take, is this dual mono, is that input the silent one — is answered the same
 * way by either. Gain is applied in dB, so a zero gain lands on the floor.
 */
function fakeProgrammePeaksAt(step) {
  const width = fakeCommentaryWidth;
  if (width <= 0) return [FAKE_LEVELS_SILENCE_DB, FAKE_LEVELS_SILENCE_DB];
  const inputs = fakeChannelPeaksAt(step, width);
  const map = fakeEffectiveChannelMap(width);
  return [0, 1].map((output) => {
    let peak = FAKE_LEVELS_SILENCE_DB;
    for (const c of map) {
      if (!c || Number(c.output) !== output) continue;
      const from = inputs[Number(c.input)];
      const gain = Math.abs(Number(c.gain));
      if (typeof from !== 'number' || !Number.isFinite(gain)) continue;
      peak = Math.max(peak, from + 20 * Math.log10(gain));
    }
    return Math.max(FAKE_LEVELS_SILENCE_DB, Math.min(0, peak));
  });
}

function fakeChannelMapState() {
  const saved = fakeChannelMaps.get(fakeCommentaryKey);
  const map = Array.isArray(saved) ? saved : [];
  return {
    inputChannels: fakeCommentaryWidth,
    map: map.map((c) => ({ ...c })),
    // gst.ChannelMap.IsDefault: an empty map means nobody has chosen, and the Go
    // side resolves it to channel 1 left / channel 2 right (or dual mono at
    // width 1) — see fakeDefaultChannelMap.
    isDefault: map.length === 0,
    // Which device the two fields above are ABOUT. Never omitted, not even when
    // the width is zero: "nothing negotiated, and it was this device that failed
    // to" is a different sentence from "nothing negotiated".
    deviceKey: fakeCommentaryKey,
  };
}

/**
 * startFakeChannelLevels drives the per-channel meters at the real chlevel's
 * 100 ms interval, off the same step counter the programme meter uses so the
 * two can never disagree about one instant.
 */
function startFakeChannelLevels() {
  if (fakeChannelLevelsInterval || !fakeBrowserSession) return;
  fakeChannelLevelsInterval = setInterval(() => {
    const peak = fakeChannelPeaksAt(fakeLevelsStep, fakeCommentaryWidth);
    fakeEmit(EVENT_CHANNEL_LEVELS, {
      peak,
      rms: peak.map((p) => Math.max(FAKE_LEVELS_SILENCE_DB, p - FAKE_LEVELS_RMS_BELOW_PEAK_DB)),
    });
  }, 100);
}

function stopFakeChannelLevels() {
  if (fakeChannelLevelsInterval) {
    clearInterval(fakeChannelLevelsInterval);
    fakeChannelLevelsInterval = null;
  }
  const silence = new Array(Math.max(fakeCommentaryWidth, 0)).fill(FAKE_LEVELS_SILENCE_DB);
  fakeEmit(EVENT_CHANNEL_LEVELS, { peak: silence, rms: silence.slice() });
}

/**
 * Reads what the capture pad negotiated and the routing in force:
 * {inputChannels, map, isDefault, deviceKey}. inputChannels is the width every
 * map sent back must fit inside; map is the list of contributions, empty when
 * nobody has chosen; isDefault says which of those two an empty list is; and
 * deviceKey is `${kind}:${id}` for the device the other three are about.
 *
 * NEVER THROWS, for the reason getConformTarget does not: this is called on the
 * Settings screen's open path, and an older build without the binding must leave
 * the rest of the screen intact. Zero channels is the honest answer to every way
 * of not knowing, and it is the answer that draws no grid.
 *
 * THE FALLBACK'S EMPTY deviceKey IS DELIBERATE and it must never be filled in
 * with the device the caller happens to have selected. An empty key matches no
 * selection, so a screen that gates on "the key I hold is the key of the device
 * on screen" draws nothing — which is the right outcome for an older build,
 * a failed call and a signed-out one alike. Inventing the key here would turn
 * "we could not ask" into "this device negotiated zero channels".
 */
export async function getChannelMap() {
  if (hasWails()) {
    const nothing = { inputChannels: 0, map: [], isDefault: true, deviceKey: '' };
    try {
      const got = await callGoBound(CHANNEL_MAP_METHODS.state);
      return got && typeof got === 'object' ? got : nothing;
    } catch {
      return nothing;
    }
  }
  return fakeChannelMapState();
}

/**
 * Sets the routing LIVE: a list of {output, input, gain}, input counted from
 * zero. Measured on the real card at 119 µs, with the pipeline staying PLAYING,
 * nothing pending, and the change audible in the very next level message — which
 * is why the screen that calls this has no Apply button.
 *
 * REFUSAL IS GO'S JOB AND IT MATTERS THAT IT HAPPENS BEFORE THE WRITE.
 * audioconvert rejects an out-of-range coefficient SILENTLY, leaving the previous
 * matrix in force, so a rejected write and a successful one are indistinguishable
 * at the property level and the application could never learn afterwards which
 * matrix is running. gst.SetChannelMap validates against InputChannels() first
 * and writes nothing if the map does not fit; a caller that swallowed the error
 * would be showing a routing that is not the one in force.
 *
 * IT WRITES TO WHICHEVER DEVICE CAPTURE IS ON, and takes no device argument.
 * That is Go's shape and it is the right one: there is exactly one commentary
 * capture, the map is a property of the element inside it, and a key passed here
 * could only ever be a second opinion about which device that is. What stops a
 * map being written against the wrong pad is upstream of this call — the grid is
 * drawn only while the "channelMap" event's deviceKey matches the device on
 * screen — plus Go's own refusal of a map that does not fit InputChannels().
 *
 * @param {Array<{output: number, input: number, gain: number}>} map
 */
export async function setChannelMap(map) {
  if (hasWails()) return callGoBound(CHANNEL_MAP_METHODS.set, map);
  const written = Array.isArray(map) ? map.map((c) => ({ ...c })) : [];
  // Stored under the CURRENT device's key, which is what makes a dev session
  // able to show the thing config.ChannelMaps exists for: route the card, pick
  // a stereo microphone, route that, come back, and the card's sixteen are
  // still there.
  fakeChannelMaps.set(fakeCommentaryKey, written);
  fakeEmit(EVENT_CHANNEL_MAP, fakeChannelMapState());
}

/** Subscribes to the "channelMap" event. Returns an unsubscribe function. */
export function onChannelMap(cb) {
  return subscribe(EVENT_CHANNEL_MAP, cb);
}

/** Subscribes to the "channelLevels" event. Returns an unsubscribe function. */
export function onChannelLevels(cb) {
  return subscribe(EVENT_CHANNEL_LEVELS, cb);
}

/** Subscribes to the "signal" event. Returns an unsubscribe function. */
export function onSignal(cb) {
  return subscribe(EVENT_SIGNAL, cb);
}

// ---------------------------------------------------------------------------
// The DeckLink confidence preview surface
// ---------------------------------------------------------------------------
//
// Two bindings, for the operator's own small picture of what the card is
// capturing. They are appended at the END of this file on purpose, alongside the
// channel-map block above: several work packages are editing this module at
// once, and a section that depends on nothing but hasBinding/callGoBound can sit
// here without colliding with any of them.
//
// ===================== IT IS THE SAME MECHANISM AS THE SRT PICTURE ==========
//
// A native child window, positioned by this page, painted over it. It is NOT a
// <video> element and there is nothing here that could be one: the frames never
// leave the Go process, and a browser element cannot be handed a GStreamer sink.
// So the frontend's whole job is the same one overlay.js already does for the
// return picture — reserve a rectangle, describe it in CSS pixels with the ratio
// it was measured at, and say when it must be hidden — and app.js drives BOTH
// surfaces through one createOverlay each rather than inventing a second
// mechanism for the second window.
//
// ===================== BUT THERE IS NO START AND NO STOP ====================
//
// And that asymmetry with the picture is the whole shape of this feature. The
// preview is a BRANCH OF THE CONTRIBUTION PIPELINE — a tee off the one capture,
// because the card is exclusive and a second decklinkvideosrc is impossible —
// so it exists exactly when a session that was started with it exists. It is
// built at Start from the saved configuration and it cannot be added or removed
// afterwards: MEASURED, a set_state(NULL) inside a blocking pad probe took the
// ON-AIR leg from 50 fps to 0 permanently, with the pipeline still reporting
// PLAYING. There is therefore nothing for a StartPreview binding to do that
// would not be a way of doing that.
//
// It also means there is deliberately NO "preview" state event to mirror the
// "picture" one. The page cannot learn from Go whether a branch was built, and
// it does not need to: the native window is opaque and on top, so the caption
// this page draws inside the reserved box is visible exactly when there is no
// picture over it. See ui/videosource.js's describePreviewBox.

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const PREVIEW_METHODS = Object.freeze({
  rect: 'SetPreviewRect',
  visible: 'SetPreviewVisible',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const PREVIEW_METHOD_NAMES = Object.freeze(Object.values(PREVIEW_METHODS));

/**
 * previewAvailable reports whether this build can position the preview surface
 * at all.
 *
 * BOTH, all-or-nothing, for the reason pictureAvailable spells out at length: a
 * build with SetPreviewRect but not SetPreviewVisible would reserve a box, paint
 * a native window into it, and then have no way to take it off the Settings
 * screen or the mixer drawer — an opaque rectangle over a form the operator is
 * trying to read, which is worse than no preview at all.
 *
 * It is FALSE on a remote client, and that is correct rather than incidental:
 * these are host-only, the surface is drawn by the commentary PC's own graphics
 * hardware, and no rectangle measured in a browser on the LAN describes anything
 * that exists. A remote seat reserves no box.
 */
export function previewAvailable() {
  return PREVIEW_METHOD_NAMES.every(hasBinding);
}

let fakePreviewRect = null;
let fakePreviewVisible = false;

/**
 * Positions the preview surface.
 *
 * CSS PIXELS AND THE RATIO, IN ONE CALL — identical to setPictureRect, and for
 * the identical reason: gst.ScaleRect multiplies on the Go side because the
 * factor Go could read for itself is a different number measured at a different
 * moment, and the two travel together so a rectangle can never be paired with a
 * ratio from before or after it. See setPictureRect's comment; there is one
 * conversion rule in this application and overlay.js owns it.
 *
 * @param {{x: number, y: number, width: number, height: number}} cssRect
 * @param {number} devicePixelRatio  window.devicePixelRatio, as measured
 */
export async function setPreviewRect(cssRect, devicePixelRatio) {
  const { x, y, width, height } = cssRect || {};
  if (hasWails()) {
    return callGoBound(PREVIEW_METHODS.rect, x, y, width, height, devicePixelRatio);
  }
  fakePreviewRect = { x, y, width, height, devicePixelRatio };
}

/**
 * Shows or hides the preview surface without touching the pipeline branch that
 * feeds it.
 *
 * Hiding is what Settings and the mixer drawer need. It must never be answered
 * by tearing the branch down instead: that is the set_state(NULL) that stops the
 * feed going to air, and it would be reached by nothing more exotic than opening
 * Settings mid-match.
 *
 * @param {boolean} visible
 */
export async function setPreviewVisible(visible) {
  if (hasWails()) return callGoBound(PREVIEW_METHODS.visible, visible === true);
  fakePreviewVisible = visible === true;
}

/**
 * The fake preview surface's last known geometry, for a dev session in the
 * browser where there is no native window to look at. Diagnostics only; nothing
 * reads it. Mirrors fakePictureOverlay.
 */
export function fakePreviewSurface() {
  return { rect: fakePreviewRect, visible: fakePreviewVisible };
}

// ---------------------------------------------------------------------------
// The cough mute
// ---------------------------------------------------------------------------
//
// The operator: "I also want a cough mute buttons, with a push to mute style and
// a latch mute mode."
//
// ===================== THE MUTE IS AT THE SEND PATH =========================
//
// These bindings mute the COMMENTARY GOING TO THE SWITCHER — app.go writes a
// property on a volume element inside the running contribution pipeline, between
// the resampler and the programme meter. Nothing on this side may substitute for
// them: muting the monitor element, the Web Audio gain, or the return path makes
// this desk quieter while the cough goes to air, with a green light on it.
//
// ==================== THE NAMES, AND THE ORDERING STAMP =====================
//
// MUTE_METHODS is the ONLY place these strings appear in the frontend, so
// aligning with the Go side is one edit in one object rather than a grep. Both
// are in app_remote.go's remoteAllowlist: GetCommentaryMute as a read, and
// SetCommentaryMute as MUTATING and deliberately reachable — a remote seat may
// mute, and the payload's By/ByAddr are the price of allowing it. This side must
// therefore NOT gate the controls on isRemoteClient(); availability is the
// payload's own answer.
//
// SetCommentaryMute takes a SEQ, and it is not optional bookkeeping. Wails runs
// each bound call on its own goroutine with no ordering between them, so the
// key-down and the key-up of one press can arrive in either order — and a
// key-up applied before its own key-down leaves the feed muted with nobody
// holding a key, which is a dead microphone on air that nothing will ever clear.
// app.go drops a request older than the last one applied and returns the state
// actually in force, so the caller is still told the truth. nextMuteSeq below is
// what this side owes that mechanism.
const MUTE_METHODS = Object.freeze({
  set: 'SetCommentaryMute',
  get: 'GetCommentaryMute',
});

/**
 * EVENT_MUTE is the AUTHORITATIVE report of the send path's mute state,
 * mirroring app.go's EventMute constant exactly. The payload is a mutePayload:
 *
 *   {muted, available, reason, by, byAddr, seq}
 *
 * It exists because the mute can change without this page asking: a second seat,
 * a session ending (which takes the mute with it), or a reconciliation finding
 * the pipeline disagreeing with what was last published. A control drawn from
 * its own last click is right almost always and catastrophically wrong in
 * exactly those cases.
 */
export const EVENT_MUTE = 'mute';

/**
 * MUTE_UNAVAILABLE is the payload this side invents when there is nothing to
 * ask — no Wails runtime, or a build without the bindings.
 *
 * It is UNAVAILABLE and it says why. The alternative, an available-and-unmuted
 * payload, would draw a live green control that does nothing, and the operator
 * would trust it. Every field the real payload has is present, so no caller has
 * to branch on which kind of answer it got.
 */
export const MUTE_UNAVAILABLE = Object.freeze({
  muted: false,
  available: false,
  reason:
    'this build has no cough mute binding, so nothing on this screen can mute the commentary',
  by: '',
  byAddr: '',
  seq: 0,
});

/**
 * coughMuteAvailable reports whether this build can mute the commentary at all.
 *
 * BOTH methods, not just the setter, for the reason srtReturnAvailable's comment
 * records: the getter is called on a path that has already assumed availability
 * — the adopt-on-startup that stops the control disagreeing with the pipeline —
 * and a build with one and not the other throws from inside the recovery rather
 * than from the check.
 *
 * This is the BUILD's answer, not the session's. "There is no session to mute"
 * is a different fact with a different sentence and it comes back on the
 * payload's `available` and `reason`; conflating the two would tell an operator
 * who has simply not pressed START that their application cannot mute.
 */
export function coughMuteAvailable() {
  return Object.values(MUTE_METHODS).every(hasBinding);
}

/**
 * lastMuteSeq is the high-water mark of the stamps this page has issued.
 *
 * Date.now() is what app.go asks for — it survives a reload, unlike a counter —
 * but it is not strictly increasing: two calls inside the same millisecond get
 * the same value, and a push-to-mute's down and up genuinely can land in one.
 * Equal stamps would let the pair be reordered, which is the failure the stamp
 * exists to prevent, so each stamp is at least one more than the last.
 */
let lastMuteSeq = 0;

function nextMuteSeq() {
  lastMuteSeq = Math.max(lastMuteSeq + 1, Date.now());
  return lastMuteSeq;
}

/**
 * setCommentaryMute mutes or unmutes the commentary at the send path and returns
 * the state IN FORCE — which is not always what was asked for: a request older
 * than the last one applied is dropped, and dropped is not an error.
 *
 * Rejects rather than resolving when there is no runtime: cough.js turns a
 * rejection into a red MUTE FAILED, which is the truth, whereas a resolved
 * promise would paint a calm MUTED over a live microphone.
 *
 * @param {boolean} muted
 * @returns {Promise<{muted: boolean, available: boolean, reason: string, by: string, byAddr: string, seq: number}>}
 */
export async function setCommentaryMute(muted) {
  if (!hasWails()) {
    // The fake backend does NOT pretend to mute. There is no send path in a
    // browser tab, and a dev session that showed MUTED would be teaching the
    // wrong reflex against the one control whose whole risk is being believed.
    throw new Error(
      'wslcomms: there is no send path on the fake backend, so nothing can be muted. ' +
        'The cough mute works only against a real build.',
    );
  }
  return callGoBound(MUTE_METHODS.set, muted === true, nextMuteSeq());
}

/**
 * getCommentaryMute reads the send path's current mute state.
 *
 * Never throws for the ordinary "this build cannot" case — it answers
 * MUTE_UNAVAILABLE — because the caller is the startup adopt, and a rejection
 * there would be an error banner on every dev session.
 *
 * @returns {Promise<{muted: boolean, available: boolean, reason: string, by: string, byAddr: string, seq: number}>}
 */
export async function getCommentaryMute() {
  if (!coughMuteAvailable()) return MUTE_UNAVAILABLE;
  return callGoBound(MUTE_METHODS.get);
}

/** Subscribes to the "mute" event. Returns an unsubscribe function. */
export function onCommentaryMute(cb) {
  return subscribe(EVENT_MUTE, cb);
}

// ---------------------------------------------------------------------------
// Always-live capture
// ---------------------------------------------------------------------------
//
// Three bindings and one event, for the lifetime change the rest of this module
// now assumes.
//
// ===================== WHAT MOVED, AND WHY THE SEAM IS HERE =================
//
// There used to be ONE pipeline. It was built at START and destroyed at STOP,
// and everything an operator could see came with it: the preview, the input
// meters, the per-channel meters, the negotiated width the routing grid is
// sized from, the card's signal state, the cough mute. Before START there was
// nothing to look at and nothing to configure; after STOP it all went away
// again.
//
// It is now split at a proxysink/proxysrc seam. CAPTURE pipelines — picture and
// commentary — are built when the window is ready and held until the
// application quits. The SEND pipeline is built at START and destroyed at STOP,
// and it contains no device, no preview, no slate, no channel map and no mute.
//
// The three consequences this side has to deliver:
//
//   PICTURE AND METERS WORK FROM LAUNCH, before anybody presses START.
//   THE ROUTING PANEL APPEARS AS SOON AS A DEVICE IS SELECTED, at any width,
//     for any device, sized to what its pad negotiated — "so you can configure
//     it before sending".
//   METERS, PREVIEW, ROUTING WIDTH, SIGNAL AND MUTE ALL SURVIVE STOP.
//
// ===================== THE THREE OPERATOR RULINGS THIS ENCODES ==============
//
// A1. THE DECKLINK IS HELD FROM LAUNCH TO QUIT. There is no Acquire and no
//     Release, deliberately: nothing else on the machine can open the card
//     while this application runs, and that was accepted with its eyes open,
//     because the failure it removes is a cable fault discovered twenty minutes
//     before kick-off rather than at launch. RestartCapture is what makes it
//     survivable — it is the only way back from a contention loss or a pulled
//     cable short of quitting, and it exists precisely because there is no
//     release control.
//
// A2. A COUGH MUTE LATCHED BEFORE START IS CARRIED INTO THE SESSION. The mute
//     element lives in the commentary capture now, so it exists from launch;
//     there is no longer a state in which there is nothing to mute. See the
//     cough mute section above for why that is answered by VISIBILITY rather
//     than by refusal.
//
// A3. DURING A SEND-SIDE STALL THE COMMENTARY IS DROPPED, NOT DELAYED. See the
//     level-events comment above: the queues in front of the proxysinks are
//     leaky=downstream, and the meters can therefore move over audio the far
//     end never receives, for as long as a stall lasts.
//
// ===================== SELECTING A DEVICE IS A CALL, NOT A SAVE =============
//
// SelectCommentaryInput re-points the commentary capture immediately and writes
// nothing to disk. That is what makes the routing panel appear before Save, and
// it is also the only way the panel COULD appear before Save: the width comes
// from a pad, a pad comes from an open device, and nothing on this side can
// negotiate one. The saving is still the Settings form's, on its own Save.
//
// IT IS REFUSED WHILE SENDING, and that is a safety property rather than a UX
// preference. A second proxysrc attaching to a live proxysink silently steals
// the stream and kills the first — measured, A stopped dead at 5.994 s the
// instant B attached at 6.007 s — so re-pointing capture under a running send
// pipeline is a feed that goes quietly dead with every lamp still green. The
// refusal is Go's, for the reason IsSRTReturnSelected's is Go's: a rule about
// what is safe right now must not be written twice in two languages. If it ever
// needs recognising by name rather than showing verbatim, its sentinel belongs
// beside RETURN_ALREADY_RUNNING above.
//
// ===================== AND A DEVICE THAT WILL NOT OPEN IS NOT AN ERROR ======
//
// It is a STATE, reported on the "capture" event with a reason. A capture that
// failed at launch — no card in the machine, a microphone unplugged since the
// configuration was written, the card held by Premiere — must not stop the
// application coming up, and on the picture leg it does not even stop the
// picture: that leg falls back to the slate and the fault goes on the CAMERA
// lamp. Nothing here throws for it.

/** The Go method names this adapter binds to. One place, so a rename is one edit. */
const CAPTURE_METHODS = Object.freeze({
  select: 'SelectCommentaryInput',
  restart: 'RestartCapture',
  state: 'GetCaptureState',
});

/** Derived, not listed again — see RETURN_METHOD_NAMES for the same reasoning. */
const CAPTURE_METHOD_NAMES = Object.freeze(Object.values(CAPTURE_METHODS));

/**
 * EventCapture: {picture, commentary, reason, audioDeviceName}, mirroring
 * app.go's EventCapture constant. The two states are CAPTURE_STATE values;
 * `reason` names the fault when either is "failed" and is empty otherwise;
 * `audioDeviceName` is the display name of the device commentary capture is
 * actually on, which is not always the one the form is showing — the form shows
 * a selection, this shows what opened.
 */
export const EVENT_CAPTURE = 'capture';

/**
 * CAPTURE_STATE mirrors the four states app.go publishes for each leg. They are
 * lowercase, unlike SIGNAL_STATE, because they mirror Go's own spelling.
 *
 *   off       nothing is built — no source configured for this leg
 *   opening   the pipeline is being built and the device taken
 *   live      it is PLAYING and producing
 *   failed    it could not be built or it died; `reason` says why
 *
 * OPENING IS NOT A TRANSIENT WORTH SKIPPING. A card that is held by another
 * application fails in about 100 us, but one that is merely slow to lock can sit
 * here for a second or more, and a screen that drew "failed" over it — or drew
 * nothing at all — would have an operator pulling cables at a pipeline that was
 * about to come up.
 */
export const CAPTURE_STATE = Object.freeze({
  OFF: 'off',
  OPENING: 'opening',
  LIVE: 'live',
  FAILED: 'failed',
});

/**
 * CAPTURE_UNAVAILABLE is what getCaptureState answers when there is nothing to
 * ask — an older build without the bindings, or a call that failed.
 *
 * Every leg is OFF and the reason SAYS which kind of not-knowing this is. The
 * alternative, a live-looking payload, would paint a running capture over a
 * build that has none; and an empty reason would leave a screen with an "off"
 * to explain and nothing to explain it with.
 */
export const CAPTURE_UNAVAILABLE = Object.freeze({
  picture: CAPTURE_STATE.OFF,
  commentary: CAPTURE_STATE.OFF,
  reason: 'this build has no always-live capture, so nothing here can report what is open',
  audioDeviceName: '',
});

/**
 * captureAvailable reports whether this build has always-live capture at all.
 *
 * ALL THREE, all-or-nothing, by channelMapAvailable's argument and one sharper
 * than it. SelectCommentaryInput without GetCaptureState re-points the
 * commentary capture and then has no way to learn that it failed: the operator
 * picks a microphone, the screen shows the selection taking effect, and the
 * meter that never moves again is the only evidence anything went wrong.
 * RestartCapture without the other two is a recovery control for a subsystem
 * this build cannot describe.
 *
 * It is the BUILD's answer, not the machine's. "There is no card in this
 * machine" is a different fact, it arrives on the "capture" event's reason, and
 * conflating the two would tell a slate-and-microphone seat that its
 * application cannot capture.
 */
export function captureAvailable() {
  return CAPTURE_METHOD_NAMES.every(hasBinding);
}

// --- the fake capture ------------------------------------------------------
//
// It comes up at the foot of this file, at MODULE LOAD, which is the whole
// point: see the header comment. Everything below models the one thing a dev
// session could not see before — a device being opened, negotiating a width,
// and being swapped for another one without a session anywhere in sight.

/**
 * How long the fake spends in "opening". The measured figure on the real card is
 * 0.1176 s from process start to a negotiated pad, with no consumer, no encoder
 * and no SRT anywhere in the process. This is longer on purpose: a window that
 * short is not observable in a browser, and the state a dev session cannot see
 * is the state whose rendering is never checked.
 */
const FAKE_CAPTURE_OPEN_MS = 250;

let fakeCapture = {
  picture: CAPTURE_STATE.OFF,
  commentary: CAPTURE_STATE.OFF,
  reason: '',
  audioDeviceName: '',
};
let fakeCaptureTimer = null;

/**
 * fakeCommentarySelection is the device capture is POINTED AT, which is not
 * always the device the configuration names — SelectCommentaryInput does not
 * save, so between picking a Focusrite and pressing Save these two disagree.
 *
 * It exists because RestartCapture has to rebuild the LIVE selection. Rebuilding
 * from the saved configuration instead would produce the worst version of this
 * control: an operator picks an interface, it fails to open because it was
 * asleep, they wake it and press Restart capture, and the application silently
 * goes back to the device they were trying to leave. Null until something has
 * been selected, and cleared by a save, which is the moment the configuration
 * becomes the truth again.
 */
let fakeCommentarySelection = null;

/** The commentary input the SAVED configuration names, as (kind, id). */
function fakeConfiguredCommentaryInput() {
  // Which of the two id fields applies is decided by the kind, exactly as
  // audioinput.js's deriveAudioInputEffects decides it in the other direction:
  // one of them is always cleared, and reading the wrong one is how a seat
  // captures from somewhere other than the box its configuration names.
  const kind = String(fakeConfig.audioSourceKind || 'native') === 'decklink' ? 'decklink' : 'native';
  return {
    kind,
    id:
      kind === 'decklink'
        ? String(fakeConfig.decklinkPersistentId || '')
        : String(fakeConfig.audioDeviceId || ''),
  };
}

function fakeCaptureState() {
  return { ...fakeCapture };
}

function fakePublishCapture(patch) {
  fakeCapture = { ...fakeCapture, ...patch };
  fakeEmit(EVENT_CAPTURE, fakeCaptureState());
}

/**
 * fakeInputDeviceFor resolves (kind, id) against the fake device table the way
 * the real capture element resolves it against the machine.
 *
 * AN EMPTY ID IS NOT "NO DEVICE" in either family, and the two mean different
 * things by it: a native seat with no id opens the PLATFORM DEFAULT INPUT
 * (FAKE_DEFAULT_INPUT_ID), and a decklink seat with no id means THE ONLY CARD,
 * which is config.go's documented reading of an empty decklinkPersistentId.
 *
 * An id that matches nothing returns null, and null is a real answer: it is the
 * saved-device-not-plugged-in case, which is the single most likely fault at a
 * seat whose configuration was copied from another machine.
 */
function fakeInputDeviceFor(kind, id) {
  const decklink = kind === 'decklink';
  const wanted = typeof id === 'string' ? id : '';
  const family = fakeDevices.filter((d) => d && (d.kind === 'decklink') === decklink);
  if (wanted === '') {
    if (decklink) return family.length === 1 ? family[0] : null;
    return family.find((d) => d.id === FAKE_DEFAULT_INPUT_ID) || family[0] || null;
  }
  return family.find((d) => d.id === wanted) || null;
}

/**
 * fakeCommentaryUp opens the commentary capture on (kind, id): OPENING now, then
 * either a negotiated pad or a named failure.
 *
 * The width comes from the device entry, not from the kind. That is the shape of
 * the change: there is no source-kind test anywhere in the routing path any
 * more, a 16-in CoreAudio device and the card are the same problem, and a fake
 * that special-cased "decklink means sixteen" would put back the discriminator
 * this work exists to delete.
 */
function fakeCommentaryUp(kind, id) {
  if (fakeCaptureTimer) {
    clearTimeout(fakeCaptureTimer);
    fakeCaptureTimer = null;
  }
  fakeCommentarySelection = { kind, id };

  // The per-channel ticker stops FIRST, while fakeCommentaryWidth is still the
  // outgoing device's: its parting zero-frame has to be as wide as the meters it
  // is silencing, or a sixteen-row grid is handed a one-entry frame and fifteen
  // rows keep whatever they last showed. app.go's per-channel zero frame is
  // sized the same way and for the same reason.
  stopFakeChannelLevels();

  // Only then does the pad go. The grid must learn that the width is gone BEFORE
  // it learns the new one — otherwise there is a window in which the last width
  // published and the device on screen disagree, which is exactly the window the
  // deviceKey exists to close.
  fakeCommentaryKey = fakeDeviceKey(kind, id);
  fakeCommentaryWidth = 0;
  fakeEmit(EVENT_CHANNEL_MAP, fakeChannelMapState());

  const device = fakeInputDeviceFor(kind, id);
  fakePublishCapture({
    commentary: CAPTURE_STATE.OPENING,
    reason: '',
    audioDeviceName: device ? device.name : '',
  });

  fakeCaptureTimer = setTimeout(() => {
    fakeCaptureTimer = null;
    if (!device) {
      stopFakeLevels();
      fakePublishCapture({
        commentary: CAPTURE_STATE.FAILED,
        reason:
          'that commentary input is not connected to this machine, so nothing can be captured ' +
          'from it. Choose another, or plug it in and use Restart capture.',
        audioDeviceName: '',
      });
      return;
    }
    fakeCommentaryWidth = fakeClampWidth(device.channels);
    fakeEmit(EVENT_CHANNEL_MAP, fakeChannelMapState());
    startFakeChannelLevels();
    startFakeLevels();
    fakePublishCapture({ commentary: CAPTURE_STATE.LIVE, reason: '', audioDeviceName: device.name });
  }, FAKE_CAPTURE_OPEN_MS);
}

/**
 * fakePictureUp opens the picture capture from the saved video source.
 *
 * The SIGNAL follows the picture leg and nothing else. A slate seat reports
 * UNKNOWN — which is not a fault, it is "this application cannot tell", the
 * state of every machine with no card in it — and a card seat reports OK. The
 * old fake reported OK on every fake session regardless, which taught a dev
 * loop that a slate seat has a green camera lamp.
 */
function fakePictureUp() {
  const card = String(fakeConfig.videoSource || '') === 'decklink';
  fakePublishCapture({ picture: CAPTURE_STATE.LIVE });
  fakeSignal = card
    ? { state: SIGNAL_STATE.OK, flaps: 0 }
    : { state: SIGNAL_STATE.UNKNOWN, flaps: 0 };
  fakeEmit(EVENT_SIGNAL, { ...fakeSignal });
}

/**
 * fakeCaptureUp is what the real application does at domReady, and what this
 * module does at load. It is NOT called from fakeStart, and the absence is
 * asserted by channelmap.test.js: a fake that stood capture up inside a session
 * would model the lifetime this change removed.
 */
function fakeCaptureUp() {
  fakePictureUp();
  const { kind, id } = fakeCommentarySelection || fakeConfiguredCommentaryInput();
  fakeCommentaryUp(kind, id);
}

/**
 * fakeCaptureDown is a teardown: a device change, a restart, or the application
 * quitting. It is what emits the zero-frames — the meters must fall to silence
 * rather than freeze at the last level — and it publishes UNKNOWN for the
 * signal, mirroring app.go's forgetSignal.
 *
 * STOP DOES NOT CALL IT, and that is the operator-visible half of this change.
 */
function fakeCaptureDown() {
  if (fakeCaptureTimer) {
    clearTimeout(fakeCaptureTimer);
    fakeCaptureTimer = null;
  }
  stopFakeLevels();
  stopFakeChannelLevels();
  fakeCommentaryWidth = 0;
  fakeEmit(EVENT_CHANNEL_MAP, fakeChannelMapState());
  fakeSignal = { state: SIGNAL_STATE.UNKNOWN, flaps: 0 };
  fakeEmit(EVENT_SIGNAL, { ...fakeSignal });
  fakePublishCapture({
    picture: CAPTURE_STATE.OFF,
    commentary: CAPTURE_STATE.OFF,
    reason: '',
    audioDeviceName: '',
  });
}

/**
 * Re-points the commentary capture at a device, immediately, WITHOUT saving.
 *
 * The three arguments are exactly deriveAudioInputEffects' three fields, in
 * config's own order, and the one that does not apply is ignored rather than
 * having to be omitted — a caller must not have to know which of the two id
 * fields its kind uses, because that is the knowledge the single picker exists
 * to remove.
 *
 * The routing panel appears on the "channelMap" event that follows, stamped
 * with this device's key. It is refused while sending; see the section comment.
 *
 * @param {string} kind "native" or "decklink"
 * @param {string} deviceId the platform endpoint id, for a native seat
 * @param {string} persistentId the card's persistent-id, for a decklink seat
 * @returns {Promise<void>}
 */
export async function selectCommentaryInput(kind, deviceId, persistentId) {
  const k = kind === 'decklink' ? 'decklink' : 'native';
  const id = String((k === 'decklink' ? persistentId : deviceId) ?? '');
  if (hasWails()) {
    return callGoBound(CAPTURE_METHODS.select, k, String(deviceId ?? ''), String(persistentId ?? ''));
  }
  if (fakeSenderRunning) {
    // The fake refuses it too. A dev session in which the device could be
    // changed mid-send would be a dev session in which the screen's handling of
    // the refusal is never once exercised — and the refusal is the guard against
    // a silently dead feed, so its wording matters more than most.
    throw new Error(
      'the commentary input cannot be changed while sending: re-pointing capture under a running ' +
        'send pipeline takes the feed off air without any lamp changing. Press STOP first.',
    );
  }
  fakeCommentaryUp(k, id);
}

/**
 * Tears the capture pipelines down and builds them again.
 *
 * This is the ONLY recovery from a capture fault, because the card is held from
 * launch to quit and there is no release control (A1). It is what an operator
 * reaches for after plugging the SDI cable back in, after quitting whatever else
 * took the card, or after connecting the microphone they had forgotten.
 *
 * It is a rebuild and not a repair: the picture blanks and the meters fall for
 * as long as it takes. Off air that is nothing; while sending it is a device
 * change by another name, and it is refused for the same reason.
 *
 * @returns {Promise<void>}
 */
export async function restartCapture() {
  if (hasWails()) return callGoBound(CAPTURE_METHODS.restart);
  if (fakeSenderRunning) {
    throw new Error(
      'capture cannot be restarted while sending: it would take the feed off air without any ' +
        'lamp changing. Press STOP first.',
    );
  }
  fakeCaptureDown();
  fakeCaptureUp();
}

/**
 * Reads what each capture leg is doing right now:
 * {picture, commentary, reason, audioDeviceName}.
 *
 * NEVER THROWS, for the reason getChannelMap does not: it is called on the
 * startup path, by a screen whose job is to EXPLAIN a fault, and a rejection
 * there would replace the explanation with a stack trace. Every way of not
 * knowing answers CAPTURE_UNAVAILABLE, which says so.
 *
 * @returns {Promise<{picture: string, commentary: string, reason: string, audioDeviceName: string}>}
 */
export async function getCaptureState() {
  if (hasWails()) {
    if (!captureAvailable()) return CAPTURE_UNAVAILABLE;
    try {
      const got = await callGoBound(CAPTURE_METHODS.state);
      return got && typeof got === 'object' ? got : CAPTURE_UNAVAILABLE;
    } catch {
      return CAPTURE_UNAVAILABLE;
    }
  }
  return fakeCaptureState();
}

/** Subscribes to the "capture" event. Returns an unsubscribe function. */
export function onCapture(cb) {
  return subscribe(EVENT_CAPTURE, cb);
}

// ---------------------------------------------------------------------------
// The fake capture comes up HERE, at module load, and this is the last thing
// this module does.
//
// Last, because fakeCaptureUp reaches back through most of the fake — the
// device table, the config, the level generators, the pad, the signal — and
// every one of those is a module-level `let` or `const` that must already be
// initialised. It is not hoisted for the same reason.
//
// Only in a browser: see fakeBrowserSession. A `node --test` import gets the
// fake's data and none of its clocks, and the suite exits.
//
// DEFERRED BY ONE MACROTASK, WHICH IS THIS FAKE'S domReady. The real
// application builds capture from domReady — the earliest moment
// NewOverlaySurface can succeed — which is after main.js has mounted every
// screen and every screen has subscribed. Firing the first "capture",
// "channelMap" and "signal" events synchronously during THIS module's
// evaluation would put all three before a single listener existed: the launch
// state would reach only whoever thought to read it back, and the signal has no
// getter to read it back with. A subscriber registered while main.js runs is in
// place by the time this fires.
// ---------------------------------------------------------------------------
if (fakeBrowserSession) {
  setTimeout(fakeCaptureUp, 0);
}
