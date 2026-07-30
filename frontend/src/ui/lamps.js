// The five status lamps, specification section 8.
//
// Owner: WP-5b.
//
// Two halves, kept separate on purpose: the derivation functions below are
// pure (state in, {level, text} out) so the logic that decides what a lamp
// says can be read and reasoned about without a DOM; createLamp/createLampRow
// are the only part that touches the document.
//
// frontend/package.json is frozen (CONTRACT.md, rule 1) and has no test
// runner in it, so none of this can be exercised with a `go test` equivalent
// from this package. The derivation functions are therefore written as
// small, dependency-free functions of plain data — no DOM, no imports — so
// that whoever adds a JS test runner later can import and test them with no
// rework, and so that in the meantime each function's doc comment enumerates
// its cases for review by inspection. They are also exercised by hand against
// backend.js's fakes; see that file's doc comment for how to run the UI
// against them.

/** The four lamp levels. Colour is never the only signal — see createLamp. */
export const LEVEL = Object.freeze({
  GREEN: 'green',
  AMBER: 'amber',
  RED: 'red',
  GREY: 'grey',
});

// One glyph per level, chosen so the SHAPE differs even with colour removed
// entirely: a filled disc, a triangle, a heavy cross and a hollow ring are
// distinguishable by outline alone, which is what a colourblind commentator
// or a monochrome monitor a metre away is left with (spec section 10 design
// note). The glyph is decorative — screen readers get the text state via
// aria-label, not the glyph.
const GLYPH = Object.freeze({
  [LEVEL.GREEN]: '●', // ● filled circle
  [LEVEL.AMBER]: '▲', // ▲ triangle
  [LEVEL.RED]: '✕', // ✕ heavy cross
  [LEVEL.GREY]: '○', // ○ hollow ring
});

/**
 * deriveSenderLamp turns a sender.State string (the "sender" event payload,
 * see internal/sender.State) into a lamp. CONNECTED is the only good state;
 * CONNECTING/DRAINING/BACKOFF are all "trying", shown amber; STOPPED and
 * anything unrecognised are grey.
 *
 *   deriveSenderLamp(undefined)      -> grey  "NOT STARTED"  (no event yet)
 *   deriveSenderLamp('STOPPED')      -> grey  "STOPPED"
 *   deriveSenderLamp('CONNECTING')   -> amber "CONNECTING"
 *   deriveSenderLamp('DRAINING')     -> amber "DRAINING"
 *   deriveSenderLamp('BACKOFF')      -> amber "RETRYING"
 *   deriveSenderLamp('CONNECTED')    -> green "SENDING"
 */
export function deriveSenderLamp(state) {
  switch (state) {
    case undefined:
    case null:
      return { level: LEVEL.GREY, text: 'NOT STARTED' };
    case 'STOPPED':
      return { level: LEVEL.GREY, text: 'STOPPED' };
    case 'CONNECTING':
      return { level: LEVEL.AMBER, text: 'CONNECTING' };
    case 'DRAINING':
      return { level: LEVEL.AMBER, text: 'DRAINING' };
    case 'BACKOFF':
      return { level: LEVEL.AMBER, text: 'RETRYING' };
    case 'CONNECTED':
      return { level: LEVEL.GREEN, text: 'SENDING' };
    default:
      return { level: LEVEL.GREY, text: String(state) };
  }
}

const GOOD_VIDEO = { codec: 'h264', width: 1920, height: 1080, frameRate: 50 };
const GOOD_AUDIO = { codec: 'aac', sampleRate: 48000, channels: 2 };

/**
 * deriveStatusLamps turns one m2lx.Status (the "status" event payload) into
 * the three WebSocket-derived lamps: SWITCHER SEES FEED, VIDEO OK, AUDIO OK.
 *
 * status may be null/undefined before the first event has ever arrived — all
 * three read grey, "NO STATUS" in that case, exactly as they do when
 * status.stale is true except for the text (spec section 8: "grey the three
 * WebSocket-derived lamps and say STATUS UNAVAILABLE rather than showing
 * stale green" — never show stale green, so this function never returns
 * green when unavailable is true).
 *
 * Video and audio are read directly off the WebSocket's detected formats,
 * independent of streamState, because a format mismatch is informative even
 * while the switcher itself is not (yet) streaming. An EMPTY audio array is
 * read RED, not grey — it is the MP2/AC-3 silent-drop signature (spec
 * section 8) and must not be mistaken for "no data yet".
 *
 * Illustrative cases:
 *
 *   deriveStatusLamps(undefined)
 *     -> switcher/video/audio all grey, "NO STATUS"; unavailable=false
 *   deriveStatusLamps({stale:true, ...})
 *     -> switcher/video/audio all grey, "STATUS UNAVAILABLE"; unavailable=true
 *   deriveStatusLamps({stale:false, streamState:'streaming',
 *                       video:{codec:'h264',width:1920,height:1080,frameRate:50},
 *                       audio:[{codec:'aac',sampleRate:48000,channels:2}]})
 *     -> switcher green "STREAMING"; video green "1080P50"; audio green "AAC 48K STEREO"
 *   deriveStatusLamps({stale:false, streamState:'starting', video:{}, audio:[]})
 *     -> switcher amber "STARTING"; video red "NO VIDEO"; audio red "NO AUDIO (DROPPED?)"
 *   deriveStatusLamps({stale:false, streamState:'stopped', video:{}, audio:[]})
 *     -> switcher red "STOPPED"; video red "NO VIDEO"; audio red "NO AUDIO (DROPPED?)"
 *   deriveStatusLamps({stale:false, streamState:'streaming',
 *                       video:{codec:'h264',width:1280,height:720,frameRate:25,raw:'h264 1280x720 25 P'},
 *                       audio:[{codec:'aac',sampleRate:48000,channels:2}]})
 *     -> video red "h264 1280x720 25 P" (wrong format, shown verbatim via Raw)
 */
export function deriveStatusLamps(status) {
  if (!status) {
    const l = { level: LEVEL.GREY, text: 'NO STATUS' };
    return { switcher: l, video: l, audio: l, unavailable: false };
  }
  if (status.stale) {
    const l = { level: LEVEL.GREY, text: 'STATUS UNAVAILABLE' };
    return { switcher: l, video: l, audio: l, unavailable: true };
  }

  let switcher;
  switch (status.streamState) {
    case 'streaming':
      switcher = { level: LEVEL.GREEN, text: 'STREAMING' };
      break;
    case 'starting':
      switcher = { level: LEVEL.AMBER, text: 'STARTING' };
      break;
    case 'stopped':
      switcher = { level: LEVEL.RED, text: 'STOPPED' };
      break;
    default:
      switcher = { level: LEVEL.GREY, text: status.streamState || 'UNKNOWN' };
  }

  const v = status.video || {};
  const videoGood =
    v.codec === GOOD_VIDEO.codec &&
    v.width === GOOD_VIDEO.width &&
    v.height === GOOD_VIDEO.height &&
    v.frameRate === GOOD_VIDEO.frameRate;
  const video = videoGood
    ? { level: LEVEL.GREEN, text: '1080P50' }
    : { level: LEVEL.RED, text: v.raw || 'NO VIDEO' };

  const audioList = Array.isArray(status.audio) ? status.audio : [];
  let audio;
  if (audioList.length === 0) {
    // The MP2/AC-3 silent-drop signature: video can stay green while this
    // reads red. It must never read grey here — grey is reserved for "no
    // status at all" and "stale", both handled above.
    audio = { level: LEVEL.RED, text: 'NO AUDIO (DROPPED?)' };
  } else {
    const a0 = audioList[0];
    const audioGood =
      a0.codec === GOOD_AUDIO.codec &&
      a0.sampleRate === GOOD_AUDIO.sampleRate &&
      a0.channels === GOOD_AUDIO.channels;
    audio = audioGood
      ? { level: LEVEL.GREEN, text: 'AAC 48K STEREO' }
      : { level: LEVEL.RED, text: a0.raw || 'BAD FORMAT' };
  }

  return { switcher, video, audio, unavailable: false };
}

// Monitor connection states that are transitional rather than terminal.
// RTCPeerConnection.connectionState's own vocabulary (new/connecting) plus
// 'disconnected', which per the WebRTC spec is frequently transient and
// self-recovers, unlike 'failed'.
const MONITOR_TRANSITIONAL = new Set(['new', 'connecting', 'disconnected']);

/**
 * deriveMonitorLamp turns the monitor module's 'state' event (a
 * RTCPeerConnection.connectionState string, per the frontend seam) into the
 * MONITOR lamp. 'connected' is the only good state.
 *
 *   deriveMonitorLamp(undefined)      -> grey  "NOT STARTED"
 *   deriveMonitorLamp('new')          -> amber "CONNECTING"
 *   deriveMonitorLamp('connecting')   -> amber "CONNECTING"
 *   deriveMonitorLamp('disconnected') -> amber "RECONNECTING"
 *   deriveMonitorLamp('connected')    -> green "CONNECTED"
 *   deriveMonitorLamp('failed')       -> red   "DOWN"
 *   deriveMonitorLamp('closed')       -> red   "DOWN"
 *   deriveMonitorLamp('unavailable')  -> grey  "UNAVAILABLE" (module failed to load, see app.js)
 */
export function deriveMonitorLamp(state) {
  if (state === undefined || state === null) {
    return { level: LEVEL.GREY, text: 'NOT STARTED' };
  }
  if (state === 'connected') {
    return { level: LEVEL.GREEN, text: 'CONNECTED' };
  }
  if (state === 'failed' || state === 'closed') {
    return { level: LEVEL.RED, text: 'DOWN' };
  }
  if (state === 'disconnected') {
    return { level: LEVEL.AMBER, text: 'RECONNECTING' };
  }
  if (MONITOR_TRANSITIONAL.has(state)) {
    return { level: LEVEL.AMBER, text: 'CONNECTING' };
  }
  return { level: LEVEL.GREY, text: String(state).toUpperCase() };
}

/**
 * createLamp builds one lamp's DOM node and returns { el, update(lamp) }.
 * name is the fixed label ("SENDING", "VIDEO", ...); lamp is the
 * {level, text} shape every derive* function above returns.
 *
 * Level is encoded three ways at once so no single sense or CSS failure
 * loses it: the glyph's SHAPE, the state TEXT, and colour last — a
 * deliberate answer to the design note that colour alone fails a
 * colourblind commentator.
 */
export function createLamp(name, initial = { level: LEVEL.GREY, text: 'NOT STARTED' }) {
  const el = document.createElement('div');
  el.className = 'lamp';

  const glyph = document.createElement('span');
  glyph.className = 'lamp-glyph';
  glyph.setAttribute('aria-hidden', 'true');

  const textWrap = document.createElement('span');
  textWrap.className = 'lamp-text';

  const nameEl = document.createElement('span');
  nameEl.className = 'lamp-name';
  nameEl.textContent = name;

  const stateEl = document.createElement('span');
  stateEl.className = 'lamp-state';

  textWrap.append(nameEl, stateEl);
  el.append(glyph, textWrap);

  function update(lamp) {
    const level = lamp?.level || LEVEL.GREY;
    const text = lamp?.text || '';
    el.classList.remove('lamp-green', 'lamp-amber', 'lamp-red', 'lamp-grey');
    el.classList.add(`lamp-${level}`);
    glyph.textContent = GLYPH[level] || GLYPH[LEVEL.GREY];
    stateEl.textContent = text;
    el.setAttribute('aria-label', `${name}: ${text}`);
  }

  update(initial);
  return { el, update };
}

/**
 * createLampRow builds a labelled, wrapping row of lamps and an aria-live
 * region so state changes are announced without re-announcing the whole
 * row on every unrelated update. names is an ordered list of lamp names;
 * the returned map's keys match it.
 */
export function createLampRow(names) {
  const el = document.createElement('div');
  el.className = 'lamps';
  el.setAttribute('role', 'group');
  el.setAttribute('aria-label', 'Status indicators');
  el.setAttribute('aria-live', 'polite');

  const lamps = {};
  for (const name of names) {
    const lamp = createLamp(name);
    lamps[name] = lamp;
    el.appendChild(lamp.el);
  }

  return { el, lamps };
}
