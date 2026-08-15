// The five status lamps, specification section 8.
//
// Owner: WP-5b.
//
// Two halves, kept separate on purpose: the derivation functions below are
// pure (state in, {level, text} out) so the logic that decides what a lamp
// says can be read and reasoned about without a DOM; createLamp/createLampRow
// are the only part that touches the document.
//
// The derivation functions are written as small, dependency-free functions of
// plain data — no DOM, no imports — which is what lets `node --test` drive them
// with nothing installed (lamps.test.js). That shape was originally chosen
// because there was no test runner at all and each doc comment had to enumerate
// its own cases for review by inspection; the doc comments have been kept in
// that register anyway, because they are also what the operator-facing wording
// is reviewed from. They are additionally exercised by hand against backend.js's
// fakes; see that file's doc comment for how to run the UI against them.
//
// The DOM half (createLamp, createLampRow) is still untested: there is no jsdom
// here and a shim widened until a test passes stops being evidence.

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

// ======================= WITHDRAWN FROM THE GUI, NOT DELETED ================
//
// The honest line is NO LONGER RENDERED. home.js stopped appending it at the
// operator's request, which is a deliberate change to specification section 8
// rather than a tidy-up.
//
// deriveHonestLine and its wording stay here, complete and tested in
// lamps.test.js, exactly as internal/mixer's golden/Compare machinery stayed
// when the drift panel was withdrawn from the mixer drawer. It has no caller in
// this build. That is expected: putting the line back is a change to home.js
// alone, and nothing here needs to be rewritten first.
//
// The description below is what it says WHEN SHOWN, and is unchanged.
//
// The honest line, specification section 10. It is permanent, not dismissible
// and not a tooltip — and it must not assert something that is not true.
//
// The caveat NEVER changes: nothing this application can see proves the
// commentator is audible on the broadcast output. The switcher accepting a feed
// is not the same as the gallery having it faded up, and the app has no way to
// tell the difference. What changes is the sentence in front of the caveat,
// because "your feed is reaching the switcher" was being shown while STOPPED,
// when it was simply false — and a permanent honest line that is sometimes a
// lie is worse than no line at all.
const HONEST_CAVEAT = 'This does not confirm you are audible on the broadcast output.';

/**
 * deriveHonestLine turns a sender.State string into the honest line's text.
 *
 *   deriveHonestLine('CONNECTED')  -> the feed IS reaching the switcher (+ caveat)
 *   deriveHonestLine('CONNECTING') -> not reaching it YET (+ caveat)
 *   deriveHonestLine('DRAINING')   -> as CONNECTING
 *   deriveHonestLine('BACKOFF')    -> reconnecting; nothing is arriving (+ caveat)
 *   deriveHonestLine('STOPPED')    -> nothing is being sent (+ caveat)
 *   deriveHonestLine(undefined)    -> as STOPPED (no event has arrived yet)
 *
 * The claim tracks the SENDER, not the switcher lamps, because the sender is
 * the half this process actually knows: it is our socket and our pipeline. The
 * three WebSocket lamps report what M2L-X says, which is a different question
 * and has its own row.
 */
export function deriveHonestLine(state) {
  switch (state) {
    case 'CONNECTED':
      return `Your feed is reaching the switcher. ${HONEST_CAVEAT}`;
    case 'CONNECTING':
    case 'DRAINING':
      return `Connecting: your feed is not reaching the switcher yet. ${HONEST_CAVEAT}`;
    case 'BACKOFF':
      return `Reconnecting: nothing is reaching the switcher at the moment. ${HONEST_CAVEAT}`;
    default:
      // STOPPED, and every state this build does not recognise. Claiming less
      // than is true is the only safe direction to be wrong in here.
      return `You are not sending: nothing is reaching the switcher. When you are, ${
        HONEST_CAVEAT.charAt(0).toLowerCase() + HONEST_CAVEAT.slice(1)
      }`;
  }
}

// ===================== WHAT "GOOD VIDEO" IS COMPARED AGAINST =================
//
// It used to be a constant: {codec:'h264', width:1920, height:1080,
// frameRate:50}, written when every instance anyone had seen was 1080p50. That
// was a live defect. M2L-X's raster is a per-instance CONFIGURATION — every
// source feeding it must match whatever it is set to — so on a correctly
// configured 720p50 facility this lamp went RED on a perfectly good feed, and
// the operator's only remedy was to ignore a red lamp, which is the habit this
// row exists to prevent.
//
// So the RASTER half of the comparison is now supplied by the caller, out of
// the switcher's own reported configuration, and the constant below is only
// what is used when nothing is known. The CODEC half stays absolute; the two
// paragraphs after the constants say why each is the way it is.

/**
 * DEFAULT_CONFORM_TARGET is the raster used when the caller supplies nothing:
 * the 1080p50 this function has always assumed, kept because it is still the
 * measured configuration of every facility instance seen so far and because
 * gst_cgo.go's slate capsfilter is pinned to it.
 *
 * It is a FALLBACK and it is deliberately not treated as authoritative. "The
 * app has not learned the switcher's format yet" and "the switcher is 1080p50"
 * are different states, and this constant is what makes the first one behave
 * exactly as this application did before the target became derived — no worse,
 * on any instance, at any moment during startup.
 *
 * @type {Readonly<{width: number, height: number, frameRate: number}>}
 */
export const DEFAULT_CONFORM_TARGET = Object.freeze({ width: 1920, height: 1080, frameRate: 50 });

/**
 * CONFORM_CODEC is NOT derived, and that is a decision rather than an
 * oversight.
 *
 * The raster is the operator's to configure and this application follows it.
 * The codec is not: H.264 is the only thing this build can produce — mfh264enc
 * on Windows, vtenc_h264 on macOS, with no second encoder anywhere in
 * gst_cgo.go — so "what codec should M2L-X be seeing from us" has exactly one
 * answer whatever the switcher is set to. Comparing against our own encoder
 * makes this half of the lamp a statement about OUR pipeline, which is the half
 * this process actually knows.
 *
 * The derived source does carry a codec, and it is ignored on purpose. MEASURED
 * on the live matchH instance, 2026-08-15, GET /api/input/router/list: router
 * input 3 (SLATE) is configured "h265" and input 4 (COMMS, port 40004 — ours)
 * is configured "h264". Had the codec been derived from that field, an instance
 * whose commentary input was left on the h265 default would light this lamp RED
 * on a feed that is arriving correctly and that the operator cannot change:
 * there is no way to make this application send H.265. A red lamp nobody can
 * act on is worse than no lamp.
 *
 * What that costs, honestly: if a facility really does configure the commentary
 * input for H.265 and expects H.265, this lamp stays green while the input is
 * misconfigured. That misconfiguration is visible where it belongs — the
 * SWITCHER SEES FEED lamp and the input's own state — and it is a provisioning
 * error, not something the commentator can be asked to notice.
 */
const CONFORM_CODEC = 'h264';

const GOOD_AUDIO = { codec: 'aac', sampleRate: 48000, channels: 2 };

/**
 * normaliseConformTarget turns whatever the app layer supplies into a complete
 * {width, height, frameRate}, falling back to DEFAULT_CONFORM_TARGET.
 *
 * FIELD BY FIELD, not whole-object. A source that knows the raster but reports
 * a zero frame rate would otherwise make the comparison unsatisfiable — no real
 * format has frameRate 0, so the lamp would sit red forever with nothing on
 * screen explaining why. Every field that is not a finite positive number is
 * replaced individually, so the worst case is the behaviour this application
 * had before the target was derived at all.
 *
 * Values are coerced with Number() because the two places a format comes from
 * disagree about the type and always have: M2L-X's switcher_status renders
 * frame_rate as the STRING "50" while width and height beside it are NUMBERS
 * (internal/m2lx/format.go documents that trap), whereas REST's
 * /api/input/router/list renders frame_rate as the number 50 (measured, matchH,
 * 2026-08-15). Neither shape may be assumed here.
 *
 * @param {{width?: unknown, height?: unknown, frameRate?: unknown}|null|undefined} target
 * @returns {{width: number, height: number, frameRate: number}}
 */
export function normaliseConformTarget(target) {
  const t = target || {};
  return {
    width: positiveOr(t.width, DEFAULT_CONFORM_TARGET.width),
    height: positiveOr(t.height, DEFAULT_CONFORM_TARGET.height),
    frameRate: positiveOr(t.frameRate, DEFAULT_CONFORM_TARGET.frameRate),
  };
}

function positiveOr(value, fallback) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

/**
 * describeConformTarget is the GREEN lamp's text: "1080P50", "720P50", "2160P25"
 * — height, scan letter, frame rate, the vocabulary the operator already reads
 * on the switcher.
 *
 * The P is this application's own and is not read from anywhere. Everything it
 * can send is progressive: the video leg is a still image through imagefreeze,
 * and there is no interlaced path anywhere in gst_cgo.go. So the letter is a
 * true statement about the feed being described, not an unchecked claim about
 * the switcher's configuration — which matters, because the parsed
 * switcher_status format this lamp compares against carries scan_type only
 * inside Raw and the comparison never sees it.
 *
 * @param {{width?: unknown, height?: unknown, frameRate?: unknown}|null|undefined} target
 * @returns {string}
 */
export function describeConformTarget(target) {
  const want = normaliseConformTarget(target);
  return `${want.height}P${want.frameRate}`;
}

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
 * conformTarget is the RASTER the switcher is configured for, supplied by the
 * app layer (app.js, from backend.getConformTarget) and normalised
 * field-by-field against DEFAULT_CONFORM_TARGET. Omitting it is a supported
 * call and reproduces this function's behaviour before the target was derived.
 * See the block above DEFAULT_CONFORM_TARGET for why the raster is derived and
 * the codec is not.
 *
 * WHAT THE RED TEXT IS FOR IS UNCHANGED, and it is the point of the lamp: on a
 * mismatch the operator is shown v.raw — the format M2L-X says it ACTUALLY
 * received, verbatim, with the fields it named — rather than a verdict. The
 * lamp exists to make an unexpected format visible, and "1080P50 expected" tells
 * nobody what arrived instead. Nothing about the comparison target being derived
 * changes that: the target decides the colour, Raw is still what carries the
 * information.
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
 *   the SAME status on an instance configured for 720p25:
 *   deriveStatusLamps(that, {width:1280, height:720, frameRate:25})
 *     -> video green "720P25" (the feed was always fine; the constant was wrong)
 *
 * @param {object|null|undefined} status the "status" event payload
 * @param {{width?: unknown, height?: unknown, frameRate?: unknown}|null} [conformTarget]
 */
export function deriveStatusLamps(status, conformTarget) {
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

  const want = normaliseConformTarget(conformTarget);
  const v = status.video || {};
  const videoGood =
    v.codec === CONFORM_CODEC &&
    v.width === want.width &&
    v.height === want.height &&
    v.frameRate === want.frameRate;
  const video = videoGood
    ? { level: LEVEL.GREEN, text: describeConformTarget(want) }
    : // Raw, verbatim — what M2L-X says it received, not what it should have
      // been. This is the whole reason the lamp carries text at all.
      { level: LEVEL.RED, text: v.raw || 'NO VIDEO' };

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
