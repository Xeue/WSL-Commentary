// Validation for the Settings form, specification section 9.
//
// Owner: WP-5b.
//
// validateConfig is pure: it takes the plain object the Settings form has
// built (the same shape as config.Config, JSON tags and all — see
// internal/config/config.go) and returns a map of field name to error
// message. An empty map means the config may be saved. Settings.js is the
// only caller; it is a separate function so the rules are readable and
// checkable without the DOM.
//
// audioDeviceId and headphoneDeviceId are deliberately NOT required here:
// both are normally written by the two dropdowns on the main screen, and an
// engineer must be able to save the rest of the connection details (host,
// event, SRT target, statusKey) before a Dante patch or a pair of headphones
// exists to choose from. Start() will fail on its own if audioDeviceId is
// still empty when pressed; that is internal/gst.PipelineOpts's job to
// reject, not Settings'.
//
// Not required is not the same as not CHECKED: a pasted id that is positively
// in the WRONG Windows endpoint namespace (a playback id in audioDeviceId, a
// recording id in headphoneEndpointId) is refused at the field, with the
// namespace rule in the message — see the two device-id checks below.

import { CHANNEL_MODES } from '../monitor/channels.js';
// The Windows endpoint-id namespace rule, mirrored from internal/gst/
// device_id.go: capture ids begin {0.0.1.00000000}., render ids begin
// {0.0.0.00000000}., and an id of unrecognised shape is NEITHER — accepted,
// never refused. See the two device-id checks below.
import {
  isCaptureEndpointId,
  isRenderEndpointId,
  CAPTURE_ENDPOINT_PREFIX,
  RENDER_ENDPOINT_PREFIX,
} from './devices.js';
// The routing bounds, from the one module that mirrors internal/gst's own
// constants (channelmap.test.js asserts them against that package's source).
// Imported rather than restated, for the reason RETURN_CHANNEL_VALUES is: a
// validator with its own copy of a bound is how a value becomes savable here and
// refused by the element it was written for.
import { MAX_INPUT_CHANNELS, GAIN_LIMIT, OUTPUTS } from './channelmap.js';
// The two video-leg sources, from the module that owns them. Imported rather
// than restated for RETURN_CHANNEL_VALUES' reason exactly: a validator with its
// own copy of an enum is how a value becomes savable here and unbuildable in the
// pipeline — and on this field that means a position that will not start.
import { VIDEO_SOURCES, VIDEO_SOURCE_SLATE } from './videosource.js';

const PBKEYLEN_VALUES = [0, 16, 32];

// All seven audio transceiver mids, matching internal/config.Validate's 1..7
// and frontend/src/monitor/buses.js's AUDIO_MIDS. It used to be [1, 2], which
// meant the Settings screen refused to save any of the five buses the monitor
// was already subscribed to — no help at all to a commentator who can hear
// themselves on the default one.
const RETURN_MID_VALUES = [1, 2, 3, 4, 5, 6, 7];

// The three channel modes, from the one table that defines them (imported at
// the top of the file). Not restated: a validator with its own copy of the enum
// is how a value becomes valid here and unroutable in the audio graph.
const RETURN_CHANNEL_VALUES = CHANNEL_MODES.map((m) => m.value);

// The video-leg bounds, mirrored from internal/config. MAX_VIDEO_BITRATE_KBPS is
// config.MaxVideoBitrateKbps and exists as a typo guard rather than as a
// judgement about the circuit; DEFAULT_VIDEO_BITRATE_KBPS is what a 0 means.
const MAX_VIDEO_BITRATE_KBPS = 100000;
const DEFAULT_VIDEO_BITRATE_KBPS = 2000;

// The commentary capture subsystems, mirrored from internal/config's
// AudioSourceNative/AudioSourceDeckLink. settings.js owns the <select> that
// produces them; this list is what the form may not save outside of.
const AUDIO_SOURCE_NATIVE = 'native';
const AUDIO_SOURCE_KINDS = [AUDIO_SOURCE_NATIVE, 'decklink'];

// The video-leg sources this form may save, DERIVED from the control's own table
// rather than written out again — so an option added to the screen and forgotten
// here cannot become a value the operator can select and the form then refuses.
const VIDEO_SOURCE_VALUES = VIDEO_SOURCES.map((s) => s.value);

// The video-format grammar, mirrored from internal/config/videoformat.go.
//
// maxVideoDimension and maxVideoFrameRate there; the example is
// config.VideoFormatExample, and it appears in every message on purpose,
// because a refusal that does not show a correct value is a refusal an operator
// has to go and research.
const MAX_VIDEO_DIMENSION = 8192;
const MAX_VIDEO_FRAME_RATE = 1000;
const VIDEO_FORMAT_EXAMPLE = '1920x1080p50';

// How far a typed decimal may sit from an exact n*1000/1001 and still be read
// as that rate. 0.005, the same figure and for the same reason as Go's: 23.98
// is 0.004 from 24000/1001, and no rate anybody means as itself is that close
// to an NTSC one.
const NTSC_TOLERANCE = 0.005;

function isBlank(v) {
  return typeof v !== 'string' || v.trim().length === 0;
}

/**
 * channelMapError mirrors internal/gst's ChannelMap.MixMatrix refusals and
 * returns an operator-facing message, or '' when the routing is one the element
 * will accept.
 *
 * IT IS A MIRROR, NOT A SECOND MODEL, for the same reason videoFormatError is.
 * Go refuses these maps by name before a byte reaches audioconvert; this exists
 * so a hand-edited config.json is caught at the FIELD rather than at START. What
 * makes the mirror worth having HERE rather than only there is that
 * audioconvert's own rejection is SILENT — it leaves the previous matrix in
 * force and there is nothing readable afterwards to say which of the two is
 * running — so a bad coefficient that gets as far as the element is not a fault
 * anybody can diagnose from the outside.
 *
 * The one bound NOT checked is the input channel ceiling against what the pad
 * negotiated, because this function has no pad: MAX_INPUT_CHANNELS is the widest
 * INPUT this application supports at all, and a map naming a channel a
 * particular device did not negotiate is refused by Go at the moment it is
 * written, with the actual width in the message. Checking a guessed width here
 * would refuse a perfectly good sixteen-channel map on a machine whose card, or
 * whose interface, was unplugged — and the store this validates holds routings
 * for devices that are deliberately not connected today.
 *
 * @param {unknown} raw
 * @returns {string} the message, or '' if the routing is acceptable
 */
function channelMapError(raw) {
  if (!Array.isArray(raw)) {
    return 'Channel routing must be a list of contributions, each naming an output, an input ' +
      'channel and a gain. Leave it out entirely to use the input’s first two channels.';
  }

  const seen = new Set();
  for (let i = 0; i < raw.length; i += 1) {
    const c = raw[i];
    const at = `Contribution ${i + 1}`;
    if (!c || typeof c !== 'object' || Array.isArray(c)) {
      return `${at} is not a routing entry. Each one needs an output, an input and a gain.`;
    }
    if (!isInt(c.output) || !OUTPUTS.some((o) => o.index === c.output)) {
      return `${at} routes to output ${c.output}, and this feed has two: ` +
        `${OUTPUTS.map((o) => `${o.index} (${o.label})`).join(' and ')}. The commentary feed is a ` +
        'stereo pair and there is no third channel to send anything to.';
    }
    // Zero-based, as it is on the wire and in Go; the +1 in the message is the
    // operator's numbering, because the number they can check is the one on the
    // embedder.
    if (!isInt(c.input) || c.input < 0 || c.input >= MAX_INPUT_CHANNELS) {
      return `${at} takes input channel ${Number(c.input) + 1}, and this application routes at ` +
        `most ${MAX_INPUT_CHANNELS}. Channels are counted from 1 on the device and from 0 in ` +
        'this file.';
    }
    if (typeof c.gain !== 'number' || !Number.isFinite(c.gain) || Math.abs(c.gain) > GAIN_LIMIT) {
      return `${at} has a gain of ${c.gain}, and the routing accepts −${GAIN_LIMIT} to ` +
        `${GAIN_LIMIT}. This is a router with attenuation, not a mixer: there is no gain above ` +
        'unity anywhere in this path, and a value outside the range is rejected by the audio ' +
        'element without saying so, leaving the previous routing in force.';
    }
    const cell = `${c.output}:${c.input}`;
    if (seen.has(cell)) {
      return `${at} routes input channel ${c.input + 1} to an output it is already routed to. ` +
        'One crosspoint holds one gain; two entries for the same one would have to be added ' +
        'together into a level nobody chose.';
    }
    seen.add(cell);
  }
  return '';
}

/**
 * channelMapsError checks the whole per-device store: an object keyed
 * "<capture kind>:<device id>" whose values are routings channelMapError
 * accepts.
 *
 * THE MESSAGE NAMES THE DEVICE, and that is the reason this wrapper exists
 * rather than a loop at the call site. The store holds routings for devices that
 * are not selected and may not even be plugged in, so "Contribution 3 takes
 * input channel 17…" with no device on it sends the operator to the grid in
 * front of them — which is a different device's, is perfectly valid, and shows
 * nothing wrong. The key is quoted verbatim because it is what a hand-editor
 * would search config.json for.
 *
 * @param {unknown} raw
 * @returns {string} the message, or '' if every device's routing is acceptable
 */
function channelMapsError(raw) {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    return 'Channel routing must be a set of routings, one per capture device. Leave it out ' +
      'entirely to use each device’s first two channels.';
  }
  for (const key of Object.keys(raw)) {
    const map = raw[key];
    // A null routing is ABSENT, not invalid. Go unmarshals it to a nil slice,
    // which is exactly "nobody has chosen for that device" — so refusing it here
    // would make a file unsavable over a value the application it is saved for
    // reads as the normal state.
    if (map === undefined || map === null) continue;
    const message = channelMapError(map);
    if (message) return `Channel routing for "${key}": ${message}`;
  }
  return '';
}

/**
 * videoFormatError mirrors internal/config.ParseVideoFormat and returns an
 * operator-facing message, or '' when the value is a format this application
 * can conform to.
 *
 * IT IS A MIRROR, NOT A SECOND GRAMMAR. Go's parser is the authority — it is
 * what Start actually uses and what a hand-edited config.json meets — and this
 * exists so the operator learns at the FIELD rather than twenty seconds after
 * START, by which time the message is "not-negotiated (-4)" and names nothing.
 * A string these two disagree about is a defect either way round: accepted here
 * and refused there is a form that saves a value Start rejects; refused here and
 * accepted there is a format the application can send and the screen will not
 * let anybody ask for.
 *
 * INTERLACE IS REFUSED BY NAME, which is the only reason the scan letter is
 * read at all. 1080i25 is a real M2L-X configuration and somebody will type it;
 * the video leg is a still image through imagefreeze and there is no interlacer
 * in either bundler's element list, so the refusal has to say that this is a
 * limitation of the application rather than a rejected spelling — otherwise the
 * operator retypes it four ways and concludes the box is broken.
 *
 * @param {string} raw
 * @returns {string} the message, or '' if the value is acceptable
 */
function videoFormatError(raw) {
  const value = String(raw).trim();
  const eg = `, e.g. "${VIDEO_FORMAT_EXAMPLE}"`;

  const m = /^(\d*)x(\d*)([pi])(.*)$/i.exec(value);
  if (!m) {
    return `"${value}" is not a video format; write it as <width>x<height>p<frame rate>${eg}.`;
  }
  const [, widthPart, heightPart, scan, ratePart] = m;

  if (scan.toLowerCase() === 'i') {
    return (
      `"${value}" is interlaced (the "i"); this application can only send progressive video, ` +
      `so an interlaced switcher cannot be conformed to from here. Write a progressive ` +
      `format such as "${VIDEO_FORMAT_EXAMPLE}" if the switcher can be set to one.`
    );
  }

  for (const [part, what] of [
    [widthPart, 'width'],
    [heightPart, 'height'],
  ]) {
    if (part === '') return `"${value}" has no ${what}${eg}.`;
    const n = Number(part);
    if (!Number.isInteger(n) || n < 1 || n > MAX_VIDEO_DIMENSION) {
      return `"${value}" has a ${what} of "${part}"; it must be between 1 and ${MAX_VIDEO_DIMENSION} pixels${eg}.`;
    }
  }

  if (ratePart === '') return `"${value}" has no frame rate after the "p"${eg}.`;

  // Whole numbers are exact: p50 is 50/1. The digits test is explicit rather
  // than left to Number(), which accepts "+50", "5e1" and " 50" — every one of
  // which parses to a plausible rate and means the operator typed something
  // else.
  if (/^\d+$/.test(ratePart)) {
    const n = Number(ratePart);
    if (n < 1 || n > MAX_VIDEO_FRAME_RATE) {
      return `"${value}" has a frame rate of "${ratePart}"; it must be between 1 and ${MAX_VIDEO_FRAME_RATE}${eg}.`;
    }
    return '';
  }

  // The NTSC family, accepted as the decimals broadcasters actually write and
  // meaning the fraction it really is: 59.94 is 60000/1001. Any OTHER decimal
  // is refused rather than rounded — 50.5 is a typo, not a format, and
  // conforming to it would be this form inventing a video standard.
  if (!/^\d+(\.\d+)?$/.test(ratePart)) {
    return `"${value}" has a frame rate of "${ratePart}", which is not a number of frames per second${eg}.`;
  }
  const f = Number(ratePart);
  const n = Math.round((f * 1001) / 1000);
  if (
    f > 0 &&
    n >= 1 &&
    n <= MAX_VIDEO_FRAME_RATE &&
    Math.abs(f - (n * 1000) / 1001) <= NTSC_TOLERANCE
  ) {
    return '';
  }
  return (
    `"${value}" has a frame rate of "${ratePart}"; whole numbers (24, 25, 30, 50, 60) and ` +
    `the NTSC rates (23.98, 29.97, 59.94) are the frame rates there are${eg}.`
  );
}

function hasScheme(v) {
  return typeof v === 'string' && v.includes('://');
}

function isInt(v) {
  return typeof v === 'number' && Number.isFinite(v) && Number.isInteger(v);
}

/**
 * validateConfig checks config against every constraint specification
 * section 9 states or implies (a host has no scheme, a port is in range, a
 * key length is 0/16/32, a tile has positive size). It returns
 * { fieldName: message }; a field name of "monitorTile.x" etc. addresses one
 * of the four tile numbers individually so the form can point at the right
 * input.
 */
export function validateConfig(config) {
  const errors = {};

  if (isBlank(config.m2lxHost)) {
    errors.m2lxHost = 'M2L-X host is required.';
  } else if (hasScheme(config.m2lxHost)) {
    errors.m2lxHost = 'Enter a bare host, e.g. "m2lx.example.com" — no "https://".';
  }

  if (isBlank(config.alias)) {
    errors.alias = 'Alias is required.';
  }

  if (isBlank(config.eventId)) {
    errors.eventId = 'Event ID is required.';
  }

  // There is no srtHost to validate: the SRT host is always derived from
  // m2lxHost (internal/config.EffectiveSRTHost), which strips any scheme itself.

  if (!isInt(config.srtPort) || config.srtPort < 1 || config.srtPort > 65535) {
    errors.srtPort = 'SRT port must be a whole number from 1 to 65535.';
  }

  if (!isInt(config.srtLatencyMs) || config.srtLatencyMs <= 0) {
    errors.srtLatencyMs = 'Latency must be a whole number of milliseconds greater than 0.';
  }

  if (!isInt(config.pbkeylen) || !PBKEYLEN_VALUES.includes(config.pbkeylen)) {
    errors.pbkeylen = 'Key length must be 0 (no passphrase), 16 or 32.';
  }

  // The port of the M2L-X OUTPUT the return dials, as opposed to srtPort above,
  // which is the INPUT the feed is sent to. Same range check, a different
  // endpoint, and the one field on this screen an operator is most likely to
  // have inherited a wrong value for: it had no control at all for a revision,
  // so a config.json written while the default was 40503 — src=cln, measured
  // encrypted=true — could not be corrected from the application.
  //
  // Zero is rejected rather than accepted as "use the default". Go's
  // EffectiveSRTReturnPort does substitute the default for 0, but the form
  // shows the substituted value, so a 0 arriving here means the operator
  // cleared the box — and silently saving a different number from the one on
  // screen is how a field stops meaning what it says.
  if (!isInt(config.srtReturnPort) || config.srtReturnPort < 1 || config.srtReturnPort > 65535) {
    errors.srtReturnPort = 'SRT return port must be a whole number from 1 to 65535 — 40501 is the programme output.';
  }

  // The PICTURE monitor's SRT buffer, in milliseconds. A different endpoint
  // again from srtLatencyMs above: that one is the retransmission budget the
  // contribution feed carries on its way OUT, this one is the budget the
  // commentator's picture window carries on the way IN. They were one field
  // until the picture was measured running about a second behind, and sharing
  // them meant the only way to make the monitor quicker was to thin the
  // protection on the feed going to air.
  //
  // The bounds match internal/config.ValidateReturn exactly — 0 to 8000 — and
  // the two must not drift: a value this form accepts and Go then refuses
  // reaches the operator as a monitor that will not start, with the reason on a
  // screen they are not looking at.
  //
  // Zero is ACCEPTED here, unlike srtReturnPort, and the difference is
  // deliberate. Go's EffectivePictureLatencyMs substitutes the default for 0 and
  // every config.json written before this field existed holds 0, so refusing it
  // would make the Settings screen unsavable on the first launch after an
  // upgrade — the form is populated from a stored 0, and the operator would be
  // told to fix a field they never touched.
  if (!isInt(config.pictureLatencyMs) || config.pictureLatencyMs < 0 || config.pictureLatencyMs > 8000) {
    errors.pictureLatencyMs =
      'Picture buffer must be a whole number of milliseconds from 0 to 8000 — 120 is the default.';
  }

  // --- the contribution video leg -----------------------------------------
  //
  // The two fields below are the form's half of an acceptance condition worth
  // stating in full: IT MUST NOT BE POSSIBLE TO SAVE A VALUE THAT MAKES START
  // FAIL LATER WITH A CAPS ERROR NAMING NO FIELD. Go's SaveConfig deliberately
  // does not validate — a half-filled form on first run has to be savable — so
  // internal/config.Validate runs only at START. That makes it the backstop for
  // a hand-edited file or a preset from a newer build, and makes THIS the thing
  // that stops a bad value reaching config.json at all. The two must not drift:
  // every bound and every accepted spelling below is mirrored from
  // internal/config/videoformat.go, and videoformat_test.go is where the
  // authority lives.

  // Zero means "use the default", which is why 0 is accepted rather than
  // treated as an empty box — the same shape as pictureLatencyMs above and for
  // the same upgrade reason: every config.json written before this field
  // existed holds 0. The ceiling is a typo guard and nothing more; it is not a
  // statement about what the circuit will carry.
  if (
    !isInt(config.videoBitrateKbps) ||
    config.videoBitrateKbps < 0 ||
    config.videoBitrateKbps > MAX_VIDEO_BITRATE_KBPS
  ) {
    errors.videoBitrateKbps =
      `Video bitrate must be a whole number of kbps from 0 to ${MAX_VIDEO_BITRATE_KBPS}. ` +
      `0 means the default, ${DEFAULT_VIDEO_BITRATE_KBPS} — which was sized for a still slate; ` +
      'nearer 10000 is wanted for live video.';
  }

  // BLANK IS VALID and is the default: empty means derive the format from the
  // switcher, which is what happens whenever any node is streaming. The check
  // fires only on a value somebody typed.
  if (!isBlank(config.videoFormatOverride)) {
    const message = videoFormatError(config.videoFormatOverride);
    if (message) errors.videoFormatOverride = message;
  }

  // WHAT THE VIDEO LEG CARRIES. Undefined is ACCEPTED and reads as the slate —
  // that is what every config.json written before this field existed holds, and
  // refusing it would make the Settings screen unsavable on the first launch
  // after an upgrade, over a field the operator has never seen. It is also what
  // config.EffectiveVideoSource does with an empty value, for the same reason
  // and in the same direction: nobody's silence may turn into a live camera.
  //
  // What is NOT checked here is whether the machine has the card the value
  // names. That is deliberate and it is validate.js's standing rule (see the
  // file header on audioDeviceId): an engineer must be able to configure a
  // position before the hardware is patched into it. The absence is a WARNING on
  // the control instead — videosource.js's describeCardAvailability — and a
  // refusal at START, from Go, which is the only place that can be sure.
  if (!VIDEO_SOURCE_VALUES.includes(config.videoSource ?? VIDEO_SOURCE_SLATE)) {
    errors.videoSource =
      'Video sent to the switcher must be either the slate or the DeckLink card’s video input.';
  }

  // decklinkPreviewEnabled is deliberately NOT validated. It is a boolean the
  // form produces with .checked, and every non-boolean a hand-edited file could
  // put there is read as OFF by normalisePreviewEnabled — which is the safe
  // direction and needs no refusal, because there is no value of it that stops
  // this position going on air.

  // --- the commentary input subsystem --------------------------------------

  if (!AUDIO_SOURCE_KINDS.includes(config.audioSourceKind ?? AUDIO_SOURCE_NATIVE)) {
    errors.audioSourceKind =
      'Commentary input must be either the computer sound input or a Blackmagic DeckLink card.';
  }

  // OPTIONAL — empty means "the only card in the machine", which is the normal
  // case. The one check is against the ONE WRONG VALUE a hurried operator will
  // type: the small integer Blackmagic's own tools show beside a card. That is
  // the device-number, an ENUMERATION INDEX that changes when a card is added
  // or moved, and the whole reason this field stores persistent-id instead is
  // that a device-number silently addresses a different card after the next
  // reboot. A one- or two-digit value is refused by name; anything longer is
  // accepted, because a real persistent-id is a large decimal (the measured
  // UltraStudio 4K Mini reports 2747401380) and a positive rule here would turn
  // an unrecognised future id into an unsavable form.
  //
  // THE CLOSING INSTRUCTION CHANGED WITH THE CONTROL. It used to read "Leave
  // this blank if there is only one card", which was the right advice while this
  // was a free-text box the operator typed into. It is a dropdown now — the
  // value can only be a stored one or an enumerated card's own id — so a
  // device-number reaching here came out of a config.json written by an older
  // build or edited by hand, and the operator's move is to pick the card off the
  // list rather than to clear a box they cannot see. An error whose remedy names
  // a control that is not on the screen is an error nobody can act on.
  if (!isBlank(config.decklinkPersistentId) && /^\d{1,2}$/.test(config.decklinkPersistentId.trim())) {
    errors.decklinkPersistentId =
      `"${config.decklinkPersistentId.trim()}" is a device number, not a persistent ID. ` +
      'A device number is a position in the enumeration and addresses a different card once ' +
      'one is added or moved; a persistent ID names the card itself and is a long number ' +
      '(e.g. 2747401380). Choose the card from the microphone list above.';
  }

  // THE CAPTURE DEVICES' CHANNEL ROUTING, one per device. Absent or empty is
  // VALID and is the normal state — an absent key means nobody has chosen for
  // that device, and Go resolves it to the first two input channels, which is
  // exactly what this application sent before the routing screen existed.
  //
  // The grid upstairs is INCAPABLE of producing an invalid map: it is drawn from
  // the negotiated width, its crosspoints are a set, and it cannot express two
  // contributions to one cell. A HAND-EDITED config.json is not, and this is the
  // only thing between such a file and a Save. The bounds are internal/gst's, not
  // a second opinion: two outputs because the AAC encoder is pinned to a stereo
  // pair, and [-1, 1] because audioconvert HARD-CLAMPS the coefficient there —
  // and rejects the whole matrix SILENTLY, leaving the previous one in force, so
  // a value that reaches the element out of range is unrecoverable rather than
  // merely wrong.
  //
  // The map is REFUSED WHOLE and never trimmed, which mirrors gst.MixMatrix's
  // refusals. Dropping the offending entry would save a routing the operator did
  // not write, and the entry most likely to be wrong is the one carrying the
  // commentator. ONE bad device's routing refuses the whole field for the same
  // reason: this form saves the store whole, so trimming one device's entry out
  // of it is deleting that device's routing rather than fixing it.
  if (config.channelMaps !== undefined && config.channelMaps !== null) {
    const message = channelMapsError(config.channelMaps);
    if (message) errors.channelMaps = message;
  }

  // The RETURN path's key length. Same three values, a different endpoint, and
  // deliberately not the same field as pbkeylen above: M2L-X sets encryption
  // per output — Output 1 (pgm, 40501) measured encrypted=false while Outputs 2
  // and 3 measured encrypted=true — so the feed and the monitor routinely need
  // different answers. Sharing one control means whichever way it is set, one
  // of the two paths is wrong, and the failure is a silent handshake refusal.
  if (!isInt(config.srtReturnPBKeyLen) || !PBKEYLEN_VALUES.includes(config.srtReturnPBKeyLen)) {
    errors.srtReturnPBKeyLen = 'Return key length must be 0 (no passphrase), 16 or 32.';
  }

  // statusKey is OPTIONAL. It names the switcher_status node the three
  // WebSocket-derived lamps read; with it empty they say NO STATUS, which is
  // honest, and the feed is unaffected. Requiring it made the app unusable
  // until the operator guessed a value nothing in the M2L-X API will tell them
  // — the app now offers candidates after a START instead.

  // audioDeviceId stays optional (see the file comment), but a value that is
  // POSITIVELY a Windows RENDER (playback) endpoint id is refused at the
  // field. This is the paste route into the defect the dropdown filter fixes:
  // wasapi2 republishes every playback endpoint as a capture "loopback"
  // device, an operator saved one, the pipeline prerolled and then failed
  // ASYNCHRONOUSLY — and the sender blamed the SRT network and retried
  // forever. An engineer pasting a GUID here before the hardware is patched in
  // has only this message to tell the two namespaces apart.
  //
  // Unknown shapes and empty are ACCEPTED, deliberately — the asymmetric rule
  // from internal/gst/device_id.go. Refusing an id merely because it fails to
  // classify as capture would turn a future Windows id-shape change into a
  // Settings screen that cannot be saved, twenty minutes before kick-off.
  if (!isBlank(config.audioDeviceId) && isRenderEndpointId(config.audioDeviceId)) {
    errors.audioDeviceId =
      `This is a Windows PLAYBACK endpoint id — capture ids begin ${CAPTURE_ENDPOINT_PREFIX}, ` +
      `render ids begin ${RENDER_ENDPOINT_PREFIX}. A playback device cannot be recorded from; ` +
      'pick a device in the Commentary input dropdown on the main screen instead.';
  }

  // The same check, pointed the other way, for the SRT return's WASAPI
  // endpoint (headphoneEndpointId): wasapi2sink plays through a RENDER
  // endpoint, so a CAPTURE id pasted here is the mirror-image mistake.
  // headphoneDeviceId is deliberately NOT checked — it is a browser
  // mediaDeviceId, a different identifier space with no namespace to test.
  if (!isBlank(config.headphoneEndpointId) && isCaptureEndpointId(config.headphoneEndpointId)) {
    errors.headphoneEndpointId =
      `This is a Windows CAPTURE (recording) endpoint id — render ids begin ` +
      `${RENDER_ENDPOINT_PREFIX}, capture ids begin ${CAPTURE_ENDPOINT_PREFIX}. Headphones are a ` +
      'playback device; take the id from App.ListOutputDevices, not the input list.';
  }

  if (!isInt(config.returnMid) || !RETURN_MID_VALUES.includes(config.returnMid)) {
    errors.returnMid = 'Return must be one of the seven audio transceivers, mid 1 to 7.';
  }

  // returnChannel picks which SOURCE channel of the selected bus reaches the
  // ears — see frontend/src/monitor/channels.js. Validated because a value with
  // no routing entry leaves the ChannelSplitter wired to nothing, and the
  // symptom of that is silence, which is the symptom of everything else too.
  //
  // returnSource is deliberately NOT validated here: the Settings form has no
  // control for it, so an error keyed to it would light no field and leave
  // "fix the highlighted fields" pointing at nothing highlighted. Settings
  // normalises it on the way through instead.
  if (!RETURN_CHANNEL_VALUES.includes(config.returnChannel)) {
    errors.returnChannel = 'Return channel must be Stereo, Left only or Right only.';
  }

  if (typeof config.returnGainDb !== 'number' || !Number.isFinite(config.returnGainDb)) {
    errors.returnGainDb = 'Return gain must be a number of decibels.';
  }

  const tile = config.monitorTile || {};
  if (!isInt(tile.x) || tile.x < 0) errors['monitorTile.x'] = 'Tile X must be a whole number, 0 or more.';
  if (!isInt(tile.y) || tile.y < 0) errors['monitorTile.y'] = 'Tile Y must be a whole number, 0 or more.';
  if (!isInt(tile.w) || tile.w <= 0) errors['monitorTile.w'] = 'Tile width must be a whole number greater than 0.';
  if (!isInt(tile.h) || tile.h <= 0) errors['monitorTile.h'] = 'Tile height must be a whole number greater than 0.';

  if (isBlank(config.slatePath)) {
    errors.slatePath = 'Slate path is required.';
  }

  return errors;
}

/** True when validateConfig(config) found nothing wrong. */
export function isConfigValid(config) {
  return Object.keys(validateConfig(config)).length === 0;
}
