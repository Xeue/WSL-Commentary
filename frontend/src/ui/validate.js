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

import { CHANNEL_MODES } from '../monitor/channels.js';

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

  // srtHost is OPTIONAL: empty means "the same host as M2L-X", which is what it
  // is on every instance seen so far. internal/config.EffectiveSRTHost does the
  // fallback, and it is the only place that does it.
  if (!isBlank(config.srtHost) && hasScheme(config.srtHost)) {
    errors.srtHost = 'Enter a bare host or address — no "srt://".';
  }

  if (!isInt(config.srtPort) || config.srtPort < 1 || config.srtPort > 65535) {
    errors.srtPort = 'SRT port must be a whole number from 1 to 65535.';
  }

  if (!isInt(config.srtLatencyMs) || config.srtLatencyMs <= 0) {
    errors.srtLatencyMs = 'Latency must be a whole number of milliseconds greater than 0.';
  }

  if (!isInt(config.pbkeylen) || !PBKEYLEN_VALUES.includes(config.pbkeylen)) {
    errors.pbkeylen = 'Key length must be 0 (no passphrase), 16 or 32.';
  }

  // statusKey is OPTIONAL. It names the switcher_status node the three
  // WebSocket-derived lamps read; with it empty they say NO STATUS, which is
  // honest, and the feed is unaffected. Requiring it made the app unusable
  // until the operator guessed a value nothing in the M2L-X API will tell them
  // — the app now offers candidates after a START instead.

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
