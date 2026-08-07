/**
 * The return audio path: which software feeds the commentator's headphones.
 *
 * Owner: WP-5b.
 *
 * ================== THIS IS NO LONGER A CONTROL ON ANY SCREEN ===============
 *
 * IT IS ALWAYS WEBRTC. Audio comes from Kinesis, full stop, and there is nothing
 * in the GUI that will change it.
 *
 * There WAS a control, in the position the picture control now occupies, and it
 * was a misreading of what was asked for. The operator's words were:
 *
 *   "I want DIRTY PICTURES. I want PGM high res pictures in what the
 *    commentator sees. With the audio coming from Kinesis."
 *
 * SRT is the PICTURE. It was built here as an AUDIO path, so selecting "SRT"
 * silenced the operator — the exact opposite of the request. See
 * ./picturesource.js for the control that replaced it.
 *
 * ======================= AND YET NONE OF THIS IS DELETED ====================
 *
 * The native SRT audio return still exists on the Go side, app_return.go is
 * untouched, and everything below still describes it correctly. It is kept as a
 * CAPABILITY: it works, it is tested, and the day somebody needs full-bitrate
 * audio into a WASAPI endpoint the machinery is here rather than in a git
 * history nobody reads.
 *
 * What app.js does with it now is narrow and deliberate: at startup it drives
 * returnpath.js to the WebRTC path — which STOPS any SRT audio monitor left
 * running by an older build or an orphaned page — and corrects a saved
 * returnSource of "srt" back to "webrtc". Without that, a config file written by
 * the previous revision would go on silencing the commentator on every launch,
 * with no control on screen able to undo it.
 *
 * Everything from here down is a description of that capability, and of the
 * invariant that made it safe. Read it as documentation of something the UI can
 * no longer reach, not as a description of a control.
 *
 * ======================= THE TWO PATHS ARE NOT ALIKE ========================
 *
 * This is not a preference between two renderings of the same thing. The two
 * options are different pieces of software, on different sides of the process
 * boundary, carrying different audio at different quality:
 *
 *   WEBRTC   the KVS monitor's audio track, through Web Audio, out of the
 *            WebView with setSinkId. Monitor-grade: it is the audio that rides
 *            along with the multiviewer mosaic. Its control-to-audible latency
 *            has been MEASURED at a 489 ms upper bound.
 *
 *   SRT      a real 15 Mbit/s stream from M2L-X output 3, received by Go and
 *            played through wasapi2sink. THE BROWSER IS NOT IN THIS PATH AT
 *            ALL. Full quality, full bitrate, and its latency has NOT been
 *            measured.
 *
 * Three consequences fall out of that, and all three are the UI's problem:
 *
 * 1. ONLY ONE MAY BE AUDIBLE. Both carry the same programme at different
 *    offsets. A commentator hearing both is hearing a slapback echo of the
 *    match they are describing, which is worse than either path on its own and
 *    is not obviously a settings problem to the person suffering it. So
 *    selecting one silences the other — see deriveReturnSourceEffects, whose
 *    whole job is that there is never a state where both are on.
 *
 * 2. THE DEVICE IDS ARE DIFFERENT IDENTIFIERS FOR THE SAME HARDWARE. WebRTC
 *    routes with a mediaDeviceId from enumerateDevices; SRT routes with a
 *    WASAPI endpoint id from ListOutputDevices. They name the same headphones
 *    and they are not interchangeable — passing one where the other is expected
 *    does not fail loudly, it silently plays on the default device. So the
 *    Headphones dropdown lists the devices of the SELECTED path, both ids are
 *    persisted separately, and neither is ever written into the other's field.
 *
 * 3. THE PICTURE IS UNCHANGED. Video comes from the WebRTC peer connection
 *    whichever audio path is selected. This control does not touch it, and the
 *    UI says so, because "switch to SRT" reads like "switch everything to SRT".
 *
 * ======================= WHAT IS NOT SAID HERE ==============================
 *
 * There is no latency number for SRT. Nobody has measured it. A plausible
 * figure printed beside a measured one is indistinguishable from a second
 * measurement, and the commentator would be choosing between "489 ms" and a
 * guess dressed as a fact. LATENCY_NOTE says the WebRTC figure and says the SRT
 * figure is unmeasured, and that is the entire honest position until somebody
 * measures it.
 */

/** The existing path: the KVS monitor's audio through Web Audio. */
export const RETURN_SOURCE_WEBRTC = 'webrtc';
/** The new path: Go receives SRT and plays it through wasapi2sink. */
export const RETURN_SOURCE_SRT = 'srt';

/**
 * DEFAULT_RETURN_SOURCE is WebRTC — the path that exists, is measured, and
 * needs no extra fan-out slot on M2L-X. SRT is opted into.
 */
export const DEFAULT_RETURN_SOURCE = RETURN_SOURCE_WEBRTC;

/**
 * The config keys the two device ids are persisted under. Both names are
 * config.Config's own JSON tags, not names invented here:
 * `headphoneDeviceId` is HeadphoneDeviceID, the existing spec §9 field, and it
 * keeps its meaning (a browser mediaDeviceId); `headphoneEndpointId` is
 * HeadphoneEndpointID, a WASAPI IMMDevice endpoint GUID, filled from
 * App.ListOutputDevices.
 *
 * They are separate fields on purpose: see consequence 2 above.
 */
export const DEVICE_KEY_WEBRTC = 'headphoneDeviceId';
export const DEVICE_KEY_SRT = 'headphoneEndpointId';

/** Where each path's device list comes from — used in the dropdown's label. */
export const DEVICE_SOURCE_WEBRTC = 'browser (enumerateDevices)';
export const DEVICE_SOURCE_SRT = 'Windows (WASAPI endpoints)';

/**
 * WEBRTC_LATENCY_MS is the MEASURED control-to-audible upper bound for the
 * WebRTC monitor: docs/test-results.md §2.2. It is an upper bound, not a
 * typical figure, and the wording in LATENCY_NOTE says so.
 */
export const WEBRTC_LATENCY_MS = 489;

/**
 * LATENCY_NOTE is the only latency sentence in the UI. One measured number, one
 * plain statement that the other has not been measured. Do not add a figure for
 * SRT here until there is a measurement to cite.
 */
export const LATENCY_NOTE =
  `Return audio latency: a measured ${WEBRTC_LATENCY_MS} ms upper bound, control to audible. ` +
  'The SRT picture has not been measured — no figure is shown because none exists.';

/**
 * RETURN_SOURCES is the control, in the order it is drawn.
 *
 * `cost` is what the option actually costs, in the units the operator thinks
 * in — a fan-out slot and 15 Mbit/s, against a monitor track that is already
 * running. It is shown, not hidden behind a tooltip: the operator asked to A/B
 * these side by side and cannot do that fairly without knowing what the better
 * one costs.
 *
 * @type {ReadonlyArray<{value: string, label: string, summary: string, cost: string}>}
 */
export const RETURN_SOURCES = Object.freeze([
  Object.freeze({
    value: RETURN_SOURCE_WEBRTC,
    label: 'WebRTC',
    summary: 'Monitor-grade audio from the KVS monitor, played by the browser.',
    cost: 'No extra stream: this is the audio already arriving with the picture.',
  }),
  Object.freeze({
    value: RETURN_SOURCE_SRT,
    label: 'SRT',
    summary:
      'Full-quality audio received natively and played through Windows. The browser is not in this path.',
    cost:
      'A real 15 Mbit/s SRT stream from M2L-X and one of its fan-out slots, ' +
      'against WebRTC’s monitor-grade audio.',
  }),
]);

/**
 * isValidReturnSource reports whether source names one of the two paths.
 * @param {unknown} source
 * @returns {boolean}
 */
export function isValidReturnSource(source) {
  return RETURN_SOURCES.some((s) => s.value === source);
}

/**
 * normaliseReturnSource coerces anything to a path that exists. A config with a
 * third value in it must not leave the commentator with no audio path selected
 * at all.
 *
 * @param {unknown} source
 * @param {string} [fallback]
 * @returns {string}
 */
export function normaliseReturnSource(source, fallback = DEFAULT_RETURN_SOURCE) {
  if (isValidReturnSource(source)) return String(source);
  return isValidReturnSource(fallback) ? String(fallback) : DEFAULT_RETURN_SOURCE;
}

/**
 * deriveReturnSourceEffects is THE statement of what selecting a path means.
 * Everything the app does when the control changes is read off this object, so
 * that "only one path may be audible" is one invariant in one place rather than
 * a pair of call sites that have to be kept opposite by hand.
 *
 * It is deliberately total and deliberately mutually exclusive: for every input,
 * valid or not, exactly one of webrtcAudible and srtRunning is true.
 *
 * @param {unknown} source
 * @returns {{
 *   source: string,
 *   webrtcAudible: boolean,
 *   srtRunning: boolean,
 *   deviceKey: string,
 *   deviceSource: string,
 * }}
 */
export function deriveReturnSourceEffects(source) {
  const s = normaliseReturnSource(source);
  const isSRT = s === RETURN_SOURCE_SRT;
  return Object.freeze({
    source: s,
    // The monitor's Web Audio graph is severed from its output element when
    // this is false. The picture is unaffected either way.
    webrtcAudible: !isSRT,
    // The Go-side SRT receiver runs only while this is true. Stopped, not
    // paused: an SRT caller left dialled in holds a fan-out slot M2L-X has only
    // four of.
    srtRunning: isSRT,
    deviceKey: isSRT ? DEVICE_KEY_SRT : DEVICE_KEY_WEBRTC,
    deviceSource: isSRT ? DEVICE_SOURCE_SRT : DEVICE_SOURCE_WEBRTC,
  });
}

/**
 * returnSourceLabel is the button text. Never blank.
 * @param {string} source
 * @returns {string}
 */
export function returnSourceLabel(source) {
  const found = RETURN_SOURCES.find((s) => s.value === source);
  return found ? found.label : `${String(source)} — (not one of the two return paths)`;
}

/**
 * describeReturnSource is the sentence shown under the control for whichever
 * path is selected: what it is, and what it costs.
 *
 * @param {string} source
 * @returns {string}
 */
export function describeReturnSource(source) {
  const s = normaliseReturnSource(source);
  const found = RETURN_SOURCES.find((x) => x.value === s);
  return `${found.summary} ${found.cost}`;
}

// PICTURE_NOTE USED TO BE HERE AND IS GONE, because what it said is now false.
//
// It read: "The picture comes from WebRTC either way; this control changes
// audio only." That was true of the control this module used to drive. It is
// not true of anything now — the picture comes from SRT, the audio comes from
// Kinesis, and the sentence has been replaced by picturesource.js's PICTURE_NOTE,
// which says the opposite thing about the opposite control.
//
// It is deleted rather than corrected on purpose. A stale constant with the
// right name is how the wrong sentence gets rendered again by somebody grepping
// for where the note lives.
//
// CHANNEL_REBUILD_NOTE is gone with it. It warned that changing the return
// channel on the SRT audio path rebuilds the GStreamer pipeline and goes quiet
// for a moment — still true of that path, and no longer reachable from the GUI,
// so a warning about it would be a warning about something nobody can do.
