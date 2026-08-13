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

function isBlank(v) {
  return typeof v !== 'string' || v.trim().length === 0;
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
