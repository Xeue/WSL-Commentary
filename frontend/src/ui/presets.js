// The pure model behind the M2L-X instance picker on the Settings screen:
// what changes if this preset is applied, which keys a hand-edited file
// carries that will be ignored, and the wording for both.
//
// Owner: WP-5b (the presets work package).
//
// NO DOM, NO BACKEND IMPORT. This is the house pattern — model.js/drawer.js,
// overlay.js/app.js, validate.js/settings.js — and it is the only way these
// functions get driven tests under node:test, because package.json is frozen
// and there is no jsdom to fake a form with.
//
// ================== WHAT THIS MODULE MUST NEVER KNOW =========================
//
// The four MACHINE field tags — the two device-id keys, the endpoint-id key
// and the slate path — are deliberately NOT SPELLED anywhere in this file, and
// presets.test.js reads this file's source to prove it. The Go whitelist is
// the real guarantee that a preset cannot carry a device id; this is the same
// guarantee on the JS side of the boundary, made structural: a module that
// cannot name a machine field cannot diff one, copy one, or quietly
// reintroduce one with a well-meaning `{...currentConfig, ...fields}`.
// Everything here iterates INSTANCE_FIELD_LABELS — the whitelist — and keys
// outside it fall through to `ignored`, whatever they are called.
//
// The preset NAME is sent to Go and GO derives the id. Nothing here slugifies:
// the id is a filename under %APPDATA% and a Credential Manager target
// segment, and two implementations of that sanitiser that disagree is a preset
// whose file and whose credentials have different names.

/**
 * INSTANCE_FIELD_LABELS is the JS mirror of internal/presets.InstanceFields,
 * in the order the preview draws and lists changes, each with the label the
 * operator sees on the Settings screen. presets.test.js asserts this list
 * against the Go whitelist's json tags, because the two drifting apart is
 * silent: a tag missing here is a change nothing on the form ever mentions —
 * and since the confirm dialog was removed there is no second rendering left to
 * catch it.
 */
export const INSTANCE_FIELD_LABELS = Object.freeze([
  Object.freeze({ tag: 'm2lxHost', label: 'M2L-X host' }),
  Object.freeze({ tag: 'alias', label: 'Alias' }),
  // NO eventId. A preset answers WHICH VENUE; the event id names WHICH MATCH,
  // and the application asks the instance for it (GET /api/events/overview, via
  // App.ListEvents and ui/events.js). It is classified DISCOVERED in
  // internal/presets.DiscoveredFields, and the mirror test below is what keeps
  // that decision from being quietly undone on this side: a tag listed here
  // that Go no longer honours is a diff row — and now a green preview box — for
  // a change the apply will not make.
  Object.freeze({ tag: 'srtPort', label: 'SRT port' }),
  Object.freeze({ tag: 'srtLatencyMs', label: 'SRT latency (ms)' }),
  // The two video-leg fields sit between the circuit and the encryption because
  // that is where internal/presets.InstanceFields puts them, and this list is
  // asserted against that one in order. Both describe the VENUE rather than the
  // PC: the bitrate is how much of that instance's contribution path the feed
  // may take, and the format is what that instance's switcher is configured for
  // — one answer shared by every commentary position at the facility, which is
  // the whole argument for a preset carrying it.
  Object.freeze({ tag: 'videoBitrateKbps', label: 'Video bitrate (kbps)' }),
  Object.freeze({ tag: 'videoFormatOverride', label: 'Video format when the switcher cannot be read' }),
  Object.freeze({ tag: 'pbkeylen', label: 'Passphrase key length' }),
  Object.freeze({ tag: 'statusKey', label: 'Status key' }),
  Object.freeze({ tag: 'srtReturnPort', label: 'SRT return port' }),
  Object.freeze({ tag: 'srtReturnPBKeyLen', label: 'Return key length' }),
  Object.freeze({ tag: 'pictureLatencyMs', label: 'Picture buffer (ms)' }),
  Object.freeze({ tag: 'returnMid', label: 'Return' }),
  Object.freeze({ tag: 'monitorTile', label: 'Monitor tile' }),
  Object.freeze({ tag: 'returnGainDb', label: 'Return gain (dB)' }),
]);

/**
 * MACHINE_FIELD_LABELS is the wording for the permanent "not part of a preset"
 * note: the four MACHINE fields BY THEIR SCREEN LABELS, in the order the
 * Settings screen shows them. LABELS ONLY — no tags, see the header for why
 * this module must not be able to name a machine field. settings.js, which
 * already owns form controls for all four, pairs each label with its current
 * value; the pairing is asserted by settings.test.js.
 */
export const MACHINE_FIELD_LABELS = Object.freeze([
  'Commentary input device ID',
  'Headphone device ID — WebRTC return',
  'Headphone device ID — SRT return',
  'Slate image path',
]);

/**
 * formatValue renders one config value for the "was X, becomes Y" note beside a
 * previewed row. Objects (the monitor tile) become compact JSON; absent values
 * are said to be absent rather than rendered as "undefined", because "(not set)"
 * is a statement and "undefined" is a bug report.
 */
function formatValue(value) {
  if (value === undefined || value === null || value === '') return '(not set)';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

/**
 * diffPreset lists what applying a preset would CHANGE: ordered
 * [{tag, label, from, to}], one entry per whitelisted key the preset carries
 * whose value differs from the live config's.
 *
 * It iterates the WHITELIST, never the preset's own keys — a hand-edited file
 * carrying a machine field produces no diff row for it, because this function
 * cannot see it. Use filterPresetFields to find out what such a file carries.
 *
 * The comparison is on the rendered strings, which is deliberate: what the
 * preview puts on the form and states beside it ARE those strings, and a
 * "difference" that renders identically would be a green box the operator can
 * see no change in — now the only rendering there is, since Apply no longer
 * stops to explain itself in a dialog. One consequence,
 * accepted: a PARTIAL monitorTile in a hand-edited preset (Go merges it
 * field-by-field) renders as the fragment it is, e.g. {"x":1120,"y":720},
 * beside the full live tile — visibly a partial update rather than a lie
 * about the fields it leaves alone.
 *
 * @param {object} currentConfig  the live config (getConfig()'s shape)
 * @param {object} fields         the preset's fields map
 * @param {Array<{tag: string, label: string}>} [labels]
 * @returns {Array<{tag: string, label: string, from: string, to: string}>}
 */
export function diffPreset(currentConfig, fields, labels = INSTANCE_FIELD_LABELS) {
  const cfg = currentConfig || {};
  const preset = fields || {};
  const out = [];
  for (const { tag, label } of labels) {
    if (!Object.prototype.hasOwnProperty.call(preset, tag)) continue;
    const from = formatValue(cfg[tag]);
    const to = formatValue(preset[tag]);
    if (from === to) continue;
    out.push({ tag, label, from, to });
  }
  return out;
}

/**
 * filterPresetFields mirrors internal/presets.Filter for the UI:
 * {kept, ignored} where `kept` holds only whitelisted keys and `ignored` is
 * the sorted list of everything else — the keys Go will drop and report when
 * the preset is applied. Read on SELECTION and stated in the preset preview's
 * summary line, so the operator is told what will actually happen rather than
 * what the file claims — before they commit, rather than from a banner after.
 *
 * The fake backend uses this too, so a dev session in the browser drops the
 * same keys a real apply would.
 *
 * @param {object} fields
 * @returns {{kept: object, ignored: string[]}}
 */
export function filterPresetFields(fields) {
  const kept = {};
  const ignored = [];
  const whitelist = new Set(INSTANCE_FIELD_LABELS.map((f) => f.tag));
  for (const key of Object.keys(fields || {})) {
    if (whitelist.has(key)) {
      kept[key] = fields[key];
    } else {
      ignored.push(key);
    }
  }
  ignored.sort();
  return { kept, ignored };
}

/**
 * describeIgnoredKeys words the "this file carries keys a preset does not
 * honour" message. Empty input is the normal case and produces the empty
 * string, so callers can `if (msg)` rather than parse.
 *
 * @param {string[]} ignored
 * @returns {string}
 */
export function describeIgnoredKeys(ignored) {
  const list = Array.isArray(ignored) ? ignored.filter(Boolean) : [];
  if (list.length === 0) return '';
  const keys = list.join(', ');
  return (
    `This preset file also carries ${list.length === 1 ? 'a key' : `${list.length} keys`} that ` +
    `${list.length === 1 ? 'is' : 'are'} not part of a preset and will be ignored: ${keys}. ` +
    'Device ids, paths and live monitoring choices belong to this PC and are left untouched.'
  );
}
