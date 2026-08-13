/**
 * Tests for the instance-preset model (presets.js) and for the JS half of the
 * guarantee the Go whitelist gives: NO DEVICE ID IS EVER WRITTEN BY THE PRESET
 * PATH IN JS.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b (the presets work package).
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * A preset is one M2L-X deployment's coordinates, applied as a merge. The
 * fault it must never cause is the live phantom-endpoint failure — "Failed to
 * open device {0.0.0.00000000}.{8678ce58-…}" — delivered by a config value
 * from ANOTHER machine. internal/presets makes that impossible in Go with an
 * explicit whitelist; these tests make it impossible to reintroduce in JS,
 * where the one-line version of the bug is a well-meaning
 * `currentConfig = { ...currentConfig, ...preset.fields }`.
 *
 * The pure functions are driven for real; the structural claims — which
 * module may name a machine field, and what app.js's apply handler assigns —
 * are asserted on SOURCE TEXT with the house codeOnly() comment-stripper,
 * exactly as mixerwiring.test.js and picturesource.test.js already do,
 * because package.json is frozen and there is no jsdom to drive the form.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  INSTANCE_FIELD_LABELS,
  MACHINE_FIELD_LABELS,
  diffPreset,
  filterPresetFields,
  describeIgnoredKeys,
} from './presets.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

/**
 * codeOnly strips comments, so that a note DISCUSSING a field cannot satisfy —
 * or break — a guard about which code may name it. Same helper as
 * picturesource.test.js:60.
 */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/** The four MACHINE tags. Named HERE, in the test — the thing under test may not. */
const MACHINE_TAGS = ['audioDeviceId', 'headphoneDeviceId', 'headphoneEndpointId', 'slatePath'];

/** A full config in getConfig()'s shape, for diffing against. */
function liveConfig() {
  return {
    m2lxHost: 'wembley.example.com',
    alias: 'wsl-comms-ro',
    eventId: 'dl9-wembley',
    srtPort: 40005,
    srtLatencyMs: 120,
    pbkeylen: 0,
    statusKey: 'cam7',
    audioDeviceId: '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}',
    headphoneDeviceId: 'browser-hash',
    headphoneEndpointId: '{0.0.0.00000000}.{7a2c1f90-4b3e-4c1a-9d55-0d1b3f8e2a11}',
    returnSource: 'webrtc',
    returnChannel: 'stereo',
    srtReturnPort: 40501,
    srtReturnPBKeyLen: 0,
    pictureLatencyMs: 120,
    returnMid: 2,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
    slatePath: 'slate.png',
  };
}

// ---------------------------------------------------------------------------
// The whitelist mirror
// ---------------------------------------------------------------------------

test('INSTANCE_FIELD_LABELS mirrors the Go whitelist exactly', () => {
  // The two lists drifting apart is silent: a tag missing here is a change
  // the confirm dialog never mentions, and a tag here that Go dropped is a
  // diff row for a field the apply will not touch. Extract the Go table with
  // comments stripped, so a tag mentioned in prose is not counted.
  const go = read(repoRoot, 'internal', 'presets', 'fields.go');
  const block = go.slice(go.indexOf('var InstanceFields'), go.indexOf('// MachineFields'));
  const stripped = block.replace(/\/\/.*$/gm, '');
  const goTags = [...stripped.matchAll(/"([A-Za-z0-9]+)"/g)].map((m) => m[1]);
  const jsTags = INSTANCE_FIELD_LABELS.map((f) => f.tag);

  assert.deepEqual(
    [...jsTags].sort(),
    [...goTags].sort(),
    'presets.js INSTANCE_FIELD_LABELS and internal/presets.InstanceFields must list the same tags',
  );
  assert.equal(jsTags.length, 13, 'the whitelist is 13 INSTANCE fields (srtHost was removed); growth is a reviewed decision');
  for (const { label } of INSTANCE_FIELD_LABELS) {
    assert.ok(label && typeof label === 'string', 'every whitelisted tag needs a screen label');
  }
});

test('MACHINE_FIELD_LABELS has one label per machine field, and labels only', () => {
  assert.equal(MACHINE_FIELD_LABELS.length, MACHINE_TAGS.length);
  // Labels, not tags: the module proves by its source that it cannot name a
  // machine field, so the labels must not smuggle the tags in.
  for (const label of MACHINE_FIELD_LABELS) {
    for (const tag of MACHINE_TAGS) {
      assert.ok(!label.includes(tag), `label ${JSON.stringify(label)} contains the tag ${tag}`);
    }
  }
});

// ---------------------------------------------------------------------------
// diffPreset
// ---------------------------------------------------------------------------

test('diffPreset lists only the whitelisted keys that differ, in whitelist order', () => {
  const diff = diffPreset(liveConfig(), {
    m2lxHost: 'twickenham.example.com', // differs
    eventId: 'dl9-wembley', // wait — differs from dl9-wembley? no: equal, skipped
    srtPort: 40005, // equal, skipped
    statusKey: 'cam9', // differs
  });
  assert.deepEqual(
    diff.map((d) => d.tag),
    ['m2lxHost', 'statusKey'],
    'equal values and absent keys must not produce rows',
  );
  assert.equal(diff[0].label, 'M2L-X host');
  assert.equal(diff[0].from, 'wembley.example.com');
  assert.equal(diff[0].to, 'twickenham.example.com');
});

test('diffPreset never produces a row for a machine field, even from a hand-edited file', () => {
  const doctored = {
    m2lxHost: 'twickenham.example.com',
    audioDeviceId: '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}',
    headphoneEndpointId: '{0.0.0.00000000}.{dead}',
    slatePath: 'C:\\someone\\elses\\slate.png',
    returnSource: 'srt',
    madeUpKey: 42,
  };
  const diff = diffPreset(liveConfig(), doctored);
  assert.deepEqual(
    diff.map((d) => d.tag),
    ['m2lxHost'],
    'a diff row for a machine or UI key would present it as something the apply will do',
  );
});

test('diffPreset shows empty and absent values as "(not set)", never "undefined"', () => {
  const diff = diffPreset({ ...liveConfig(), statusKey: '' }, { statusKey: 'cam7' });
  assert.equal(diff.length, 1);
  assert.equal(diff[0].from, '(not set)');
  assert.equal(diff[0].to, 'cam7');
});

test('diffPreset renders a partial monitorTile as the fragment it is', () => {
  // Go merges a partial tile field-by-field; the dialog shows the fragment
  // beside the full live tile — visibly a partial update, not a lie about the
  // fields it leaves alone.
  const diff = diffPreset(liveConfig(), { monitorTile: { x: 1120, y: 720 } });
  assert.equal(diff.length, 1);
  assert.equal(diff[0].tag, 'monitorTile');
  assert.equal(diff[0].from, JSON.stringify({ x: 0, y: 360, w: 640, h: 360 }));
  assert.equal(diff[0].to, JSON.stringify({ x: 1120, y: 720 }));
});

test('diffPreset tolerates a missing config and empty fields', () => {
  assert.deepEqual(diffPreset(null, {}), []);
  assert.deepEqual(diffPreset(undefined, undefined), []);
});

// ---------------------------------------------------------------------------
// filterPresetFields / describeIgnoredKeys
// ---------------------------------------------------------------------------

test('filterPresetFields keeps the whitelist and reports the rest, sorted', () => {
  const { kept, ignored } = filterPresetFields({
    m2lxHost: 'x',
    slatePath: 'y',
    returnSource: 'srt',
    audioDeviceId: 'z',
    zzz: 1,
  });
  assert.deepEqual(Object.keys(kept), ['m2lxHost']);
  assert.deepEqual(ignored, ['audioDeviceId', 'returnSource', 'slatePath', 'zzz']);
});

test('describeIgnoredKeys is empty for nothing and names every key otherwise', () => {
  assert.equal(describeIgnoredKeys([]), '');
  assert.equal(describeIgnoredKeys(undefined), '');

  const one = describeIgnoredKeys(['audioDeviceId']);
  assert.match(one, /audioDeviceId/);
  assert.match(one, /ignored/i);

  const two = describeIgnoredKeys(['audioDeviceId', 'slatePath']);
  assert.match(two, /audioDeviceId, slatePath/);
  assert.match(two, /2 keys/);
  assert.match(two, /belong to this PC/i, 'the message must say WHY, or it reads as data loss');
});

// ---------------------------------------------------------------------------
// The structural guards: who may name a machine field, and what apply assigns
// ---------------------------------------------------------------------------

test('presets.js cannot name a machine field', () => {
  // The module-level version of the whole guarantee: a module that cannot
  // spell audioDeviceId cannot diff it, copy it, or spread it. Comments
  // stripped, so the documentation ABOUT the rule does not violate it.
  const src = codeOnly(ui('presets.js'));
  for (const tag of MACHINE_TAGS) {
    assert.ok(
      !src.includes(tag),
      `presets.js names ${tag}; the machine tags must not be spellable in the preset model`,
    );
  }
});

test("app.js's apply handler adopts the RETURNED config and never touches a machine field", () => {
  const src = codeOnly(ui('app.js'));
  const start = src.indexOf('async function applyPresetAndRefresh(id)');
  assert.ok(start > 0, 'app.js must have the preset apply handler');
  const body = src.slice(start, src.indexOf('\n  }', src.indexOf('return merged', start)));

  // The correctness contract: assign the awaited backend return, whole.
  assert.match(
    body,
    /const merged = await backend\.applyPreset\(id\);/,
    'the apply must go through the backend adapter and await the merged config',
  );
  assert.match(
    body,
    /currentConfig = merged;/,
    'the page must ADOPT the returned config: the next dropdown change re-writes the whole ' +
      'currentConfig, so anything less is the stale cache clobbering the preset',
  );

  // The one-line bug this file exists to make unwritable: a local merge.
  assert.ok(
    !/\.\.\.\s*(preset|fields|merged)/.test(body),
    'the apply handler spreads preset fields itself; Go owns the merge and returns the authority',
  );
  for (const tag of MACHINE_TAGS) {
    assert.ok(!body.includes(tag), `the apply path names ${tag}; it must not know the machine fields exist`);
  }

  // And the rebuild obligations the sequence exists for.
  assert.match(body, /mixerHost\.close\(\)/, 'an armed drawer pointed at a different desk must close');
  assert.match(body, /onConfigSaved\(merged\)/, 'the Settings-save pathway is reused, not restated');
  assert.match(body, /setUpMonitor\(merged\)/, 'the KVS monitor must be REBUILT: its credentials are the old event\'s');
  assert.match(body, /monitor = null/, 'the old monitor must be dropped before the rebuild');
  assert.match(body, /stopPicture\(\)[\s\S]*startPicture\(\)/, 'a running picture is restarted against the new instance');
});

test('backend.js requires all seven preset bindings, derived from one table', () => {
  const js = ui('backend.js');
  assert.match(js, /const PRESET_METHODS = Object\.freeze\(\{/);
  assert.match(
    js,
    /PRESET_METHOD_NAMES = Object\.freeze\(Object\.values\(PRESET_METHODS\)\)/,
    'PRESET_METHOD_NAMES must be derived from PRESET_METHODS, not listed again',
  );
  assert.match(
    js,
    /export function presetsAvailable\(\)\s*\{\s*return PRESET_METHOD_NAMES\.every\(hasBinding\);/,
    'availability must be all-or-nothing: a subset turns a missing binding into a throw ' +
      'on the line AFTER an apply has already half-happened',
  );
});

test('the JS method table spells the methods app_presets.go actually binds', () => {
  // Wails binds by exact name. A drift here is not an error anywhere — the
  // call rejects with "not bound" against a build that has the feature.
  const js = ui('backend.js');
  const table = js.slice(js.indexOf('const PRESET_METHODS'), js.indexOf('PRESET_METHOD_NAMES'));
  const names = [...table.matchAll(/'([A-Za-z]+)'/g)].map((m) => m[1]);
  assert.equal(names.length, 7, 'seven bindings, exactly');

  const go = read(repoRoot, 'app_presets.go');
  for (const name of names) {
    assert.ok(
      new RegExp(`func \\(a \\*App\\) ${name}\\(`).test(go),
      `backend.js binds App.${name}, which app_presets.go does not export`,
    );
  }
});

test('the fake refuses to apply a preset while the fake sender is running', () => {
  // The refusal is the safety story — a mid-match apply leaves the feed going
  // to the previous instance with every lamp green — and a fake that waved it
  // through would let the bug be written in a dev session and then not
  // reproduce. Same reasoning as the fake's return-path refusals.
  const js = ui('backend.js');
  const fn = js.slice(js.indexOf('export async function applyPreset('));
  const body = codeOnly(fn.slice(0, fn.indexOf('\n}')));
  assert.ok(body.includes('fakeSenderRunning'), 'the fake apply must check the fake sender');
  assert.match(body, /SENDING/, 'and refuse with the reason named');
});

test('the preset name goes to Go; production code never slugifies it', () => {
  // The id is a filename under %APPDATA% AND a Credential Manager target
  // segment; the sanitiser exists once, in internal/presets.DeriveID. The
  // fake's stand-in is the fake BEING Go and must only be reachable from the
  // fake branch — asserted by it living behind the hasWails() early return.
  const js = ui('backend.js');
  const save = js.slice(js.indexOf('export async function savePreset('));
  const body = save.slice(0, save.indexOf('\n}'));
  assert.match(
    body,
    /if \(hasWails\(\)\) return callGoBound\(PRESET_METHODS\.save, name\);/,
    'against a real build the NAME must be sent verbatim; Go derives the id',
  );
  // And settings.js sends the prompt result straight through.
  const settings = codeOnly(ui('settings.js'));
  assert.ok(!/slugif|deriveId|toLowerCase\(\)\.replace/.test(settings), 'settings.js must not derive ids');
});
