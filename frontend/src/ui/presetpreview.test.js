/**
 * Tests for the preset PREVIEW model (presetpreview.js): what selecting an
 * instance in the picker draws on the Settings form before anybody presses
 * Apply.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b (the presets work package).
 *
 * ======================= THE FAILURES THESE PREVENT =========================
 *
 * Applying a preset rewrites most of the form and restarts every monitor, and
 * until now the picker's selection did nothing visible at all: the change was
 * described only inside a confirm() dialog, read once, in a hurry, with the
 * form it describes hidden behind it. The preview puts it on the form — and a
 * preview is only worth having if it is true, so what these pin is the cases
 * where a naive one lies:
 *
 *   - a <select> asked to show a value it does not offer. Setting .value to an
 *     unknown option silently deselects EVERYTHING, so a hand-edited pbkeylen
 *     of 24 would draw an empty dropdown — reading as "this preset blanks the
 *     field" when it means "this value is not one this control has";
 *   - a new value that is EMPTY. A preset that clears statusKey takes the three
 *     lamps to NO STATUS, and an undecorated empty box is indistinguishable
 *     from a field the preset never mentioned;
 *   - a field with no control on screen, which must still be announced rather
 *     than changed in silence;
 *   - and the quiet case: the ACTIVE preset, or one that differs in nothing,
 *     must produce nothing at all rather than an empty flourish.
 *
 * The DOM half — which row each tag lands on, that a redraw clears first, that
 * Save withdraws the preview before it collects — is asserted on source text in
 * settings.test.js, the same way that file already asserts every other property
 * of a form there is no jsdom to drive.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import { planPresetPreview, describePresetPreview } from './presetpreview.js';
import { diffPreset } from './presets.js';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');

/** Strips comments, so prose ABOUT a tag cannot satisfy a guard about naming it. */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/** The four MACHINE tags. Named HERE, in the test — the thing under test may not. */
const MACHINE_TAGS = ['audioDeviceId', 'headphoneDeviceId', 'headphoneEndpointId', 'slatePath'];

/** A live config in getConfig()'s shape, to diff a preset against. */
function liveConfig() {
  return {
    m2lxHost: 'm2lx-wslstudios-matchg.etapsiota.com',
    alias: 'matchg',
    eventId: 'dl9-5p5ah0bd-empd',
    srtPort: 40004,
    srtLatencyMs: 120,
    pbkeylen: 16,
    statusKey: 'cam4',
    srtReturnPort: 40504,
    srtReturnPBKeyLen: 0,
    pictureLatencyMs: 120,
    returnMid: 4,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
  };
}

/** The controls a text/number field has, as settings.js describes them. */
const TEXT = { box: 'input' };
/** The three SRT key lengths, as the <select> actually offers them. */
const KEYLEN_SELECT = { box: 'select', options: ['0', '16', '32'] };

// ---------------------------------------------------------------------------
// The ordinary case
// ---------------------------------------------------------------------------

test('a changed field is previewed with the new value in the box and the old one beside it', () => {
  const diff = diffPreset(liveConfig(), { statusKey: 'cam7' });
  const rows = planPresetPreview(diff, { statusKey: 'cam7' }, { statusKey: TEXT });

  assert.equal(rows.length, 1);
  assert.equal(rows[0].tag, 'statusKey');
  assert.equal(rows[0].label, 'Status key');
  assert.equal(rows[0].boxValue, 'cam7', 'the BOX shows the value the preset would put there');
  assert.equal(rows[0].cleared, false);
  // And the row says it in words: green cannot be the only signal, and the
  // operator has to be able to read it as "from X to Y".
  assert.match(rows[0].note, /cam4/, 'the note must carry the value being replaced');
  assert.match(rows[0].note, /cam7/, 'and the value replacing it');
});

test('the preview follows the diff, so equal and absent fields draw nothing', () => {
  // The whole point of building on diffPreset rather than a second comparison:
  // a preset that names a field with the value it already has is not a change,
  // and a box marked green for it would be a lie the operator has to check.
  const preset = { m2lxHost: 'm2lx-wslstudios-matchg.etapsiota.com', statusKey: 'cam7' };
  const diff = diffPreset(liveConfig(), preset);
  const rows = planPresetPreview(diff, preset, { statusKey: TEXT, m2lxHost: TEXT });
  assert.deepEqual(rows.map((r) => r.tag), ['statusKey']);
});

test('rows keep the whitelist order the diff arrived in', () => {
  // The summary line reads as a list, and a list that reorders itself between
  // two presets is one the operator has to re-read every time.
  // INSTANCE_FIELD_LABELS order — host, then the SRT port, then the status key
  // — regardless of the order the preset file happens to spell them in.
  const preset = { statusKey: 'cam7', m2lxHost: 'other.example.com', srtPort: 40009 };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, {});
  assert.deepEqual(rows.map((r) => r.tag), ['m2lxHost', 'srtPort', 'statusKey']);
});

// ---------------------------------------------------------------------------
// The quiet case: nothing to say
// ---------------------------------------------------------------------------

test('a preset that differs in nothing previews nothing at all', () => {
  // Selecting the ACTIVE preset is the commonest selection there is — the
  // picker opens on it — and it must leave the screen exactly as it was. An
  // "changes 0 settings" line would be an empty flourish on the one selection
  // that needs no attention.
  const cfg = liveConfig();
  const rows = planPresetPreview(diffPreset(cfg, { statusKey: cfg.statusKey, srtPort: cfg.srtPort }), cfg, {});
  assert.deepEqual(rows, []);
  assert.equal(describePresetPreview('Match G', rows), '', 'and nothing to say in the card either');
});

test('an empty or missing diff is tolerated rather than thrown on', () => {
  assert.deepEqual(planPresetPreview([], {}, {}), []);
  assert.deepEqual(planPresetPreview(undefined, undefined, undefined), []);
  assert.equal(describePresetPreview('Match G', undefined), '');
});

// ---------------------------------------------------------------------------
// The <select> case
// ---------------------------------------------------------------------------

test('a <select> is moved to the preset option, and the old value goes in the note', () => {
  // A dropdown cannot show two values in the box, so the box holds the NEW one
  // and the note carries the pair.
  const preset = { pbkeylen: 32 };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { pbkeylen: KEYLEN_SELECT });
  assert.equal(rows.length, 1);
  assert.equal(rows[0].boxValue, '32', 'the option value is a STRING; a number would match no option');
  assert.match(rows[0].note, /16/, 'the note must still say what it is replacing');
  assert.match(rows[0].note, /32/);
});

test('a value the <select> does not offer is NOT written into the box', () => {
  // THE FAILURE THIS PREVENTS. Setting .value to an option a <select> does not
  // have deselects everything, so a hand-edited pbkeylen of 24 — libsrt accepts
  // 0, 16 and 32 only — would draw an EMPTY dropdown: "this preset blanks the
  // key length", which is not what it means and not what applying it would do.
  const preset = { pbkeylen: 24 };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { pbkeylen: KEYLEN_SELECT });
  assert.equal(rows.length, 1);
  assert.equal(rows[0].boxValue, null, 'nothing may be written into a control that cannot express it');
  assert.equal(rows[0].cleared, false, 'and it is emphatically not a "cleared" row');
  assert.match(rows[0].note, /24/, 'the change is still announced');
  assert.match(rows[0].note, /does not offer/, 'and the box not moving is explained rather than left as a mystery');
});

// ---------------------------------------------------------------------------
// The empty-new-value case
// ---------------------------------------------------------------------------

test('a preset that CLEARS a field reads as a change, not as an untouched box', () => {
  // statusKey blank means the three lamps read NO STATUS. An empty box with
  // nothing else on the row is indistinguishable from a field the preset never
  // mentioned, so the row is flagged: settings.js turns `cleared` into a
  // placeholder that says so in words, on top of the mark and the note.
  const preset = { statusKey: '' };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { statusKey: TEXT });
  assert.equal(rows.length, 1, 'clearing a set field is a change and must produce a row');
  assert.equal(rows[0].boxValue, '', 'the box shows what the preset would leave there: nothing');
  assert.equal(rows[0].cleared, true);
  assert.match(rows[0].note, /cam4/, 'what is being lost must be readable');
  assert.match(rows[0].note, /not set/, 'and the diff already words the empty side as "(not set)"');
});

test('an empty value is written as an empty box, never as the words "(not set)"', () => {
  // presets.js's formatValue renders "(not set)" for a human reading a diff.
  // Writing that string into an input would put the words "(not set)" in the
  // operator's box as though they had typed them — and then SAVE them if the
  // withdrawal in handleSave ever regressed.
  const preset = { statusKey: '' };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { statusKey: TEXT });
  assert.equal(rows[0].boxValue, '');
  assert.ok(!/^\(not set\)$/.test(rows[0].boxValue));
});

// ---------------------------------------------------------------------------
// Fields with no control of their own
// ---------------------------------------------------------------------------

test('a field with no control is still announced, with no box to write into', () => {
  // monitorTile is the standing case: ONE config value spread over four boxes,
  // merged field-by-field by Go, so a partial tile changes two of the four.
  // Four green numbers with no old values beside them would claim four
  // independent changes; one note under the grid states the pair the model
  // actually compared. A whitelisted field that grows no control at all lands
  // here too, and must not become a change the screen never mentions.
  const preset = { monitorTile: { x: 1120, y: 720, w: 640, h: 360 } };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { monitorTile: { box: 'none' } });
  assert.equal(rows.length, 1);
  assert.equal(rows[0].boxValue, null);
  assert.match(rows[0].note, /1120/);
  assert.match(rows[0].note, /not shown in a box/, 'or the operator hunts for a green box that is not there');
});

test('a tag with no control entry at all is treated as having no box', () => {
  // Defensive, and the direction of the defence matters: an unknown control
  // must silence the BOX, never the row. The alternative — assuming a text
  // input — writes a value into whatever happens to be there.
  const preset = { returnGainDb: 24 };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, {});
  assert.equal(rows.length, 1);
  assert.equal(rows[0].boxValue, null);
});

test('a format function decides the box value, so the address box shows an address', () => {
  // m2lxHost has no row of its own: the address box IS its rendering, and what
  // belongs in it is the base address the host produces, not the bare host.
  const preset = { m2lxHost: 'm2lx-wslstudios-matcht.etapsiota.com' };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, {
    m2lxHost: { box: 'input', format: (raw) => `https://${raw}` },
  });
  assert.equal(rows[0].boxValue, 'https://m2lx-wslstudios-matcht.etapsiota.com');
  // The NOTE stays in the diff's own words — the host, as the model compared
  // it — so what is stated is what will actually be applied.
  assert.match(rows[0].note, /m2lx-wslstudios-matchg\.etapsiota\.com/);
});

test('an object value is rendered as compact JSON, matching the diff', () => {
  const preset = { monitorTile: { x: 1120, y: 720 } };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, {
    monitorTile: { box: 'input' },
  });
  assert.equal(rows[0].boxValue, '{"x":1120,"y":720}');
});

// ---------------------------------------------------------------------------
// The summary line
// ---------------------------------------------------------------------------

test('the summary names the preset, the count and every changed field', () => {
  const preset = { m2lxHost: 'other.example.com', statusKey: 'cam7', srtPort: 40009 };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, {});
  const line = describePresetPreview('Match T', rows);
  assert.match(line, /Match T/);
  assert.match(line, /3 settings/);
  for (const label of ['M2L-X host', 'Status key', 'SRT port']) {
    assert.ok(line.includes(label), `the summary must name "${label}" — the form is two screenfuls deep`);
  }
  assert.match(line, /Nothing has changed yet/, 'a preview that reads as an action taken is worse than none');
  assert.match(line, /Apply/, 'and it must say what commits it');
});

test('the summary line carries what the confirm dialog used to say about applying', () => {
  // The dialog that stood in front of this form is gone (settings.js's
  // handleApplyPreset says why). Two of the three things it said were not the
  // diff, and this line is where one and a half of them went — the third, the
  // unhonoured keys a hand-edited file carries, is describeIgnoredKeys and is
  // joined on by settings.js so each pure module owns only its own sentence.
  //
  // Neither of these is expressible in a green box. An operator who is shown
  // what changes but not that committing it re-dials the KVS monitor and the SRT
  // picture reads the several seconds of black picture and silence as a fault —
  // and one who is not told the device fields are excluded goes looking for the
  // headphone selection a preset never touched.
  const preset = { statusKey: 'cam7' };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { statusKey: TEXT });
  const line = describePresetPreview('Match G', rows);
  assert.match(line, /monitor and the picture then reconnect/, 'applying restarts every monitor; say so');
  assert.match(line, /black and silence/, 'and say what that looks like, or it reads as a fault');
  assert.match(
    line,
    /input and headphone devices are not part of a preset and stay as they are/,
    'the machine fields are excluded BY CONSTRUCTION; an operator who is not told hunts for them',
  );

  // Still one line to read at a glance, not a paragraph. The dialog got away
  // with thirteen rows because it stopped everything; this does not stop
  // anything, so it has to stay readable at speed.
  assert.ok(
    line.length < 400,
    `the summary line is ${line.length} characters: it is read at a glance, in a card above the ` +
      'fields it points at, not settled down with',
  );

  // And the quiet case stays quiet. A preset that changes nothing says nothing —
  // the consequence is a consequence OF the changes, so it must not appear on
  // its own and turn the active selection into an announcement.
  assert.equal(describePresetPreview('Match G', []), '');
});

test('one change is "1 setting", not "1 settings"', () => {
  const preset = { statusKey: 'cam7' };
  const rows = planPresetPreview(diffPreset(liveConfig(), preset), preset, { statusKey: TEXT });
  assert.match(describePresetPreview('Match G', rows), /1 setting\b/);
});

// ---------------------------------------------------------------------------
// The structural guard, same as presets.js carries
// ---------------------------------------------------------------------------

test('presetpreview.js cannot name a machine field', () => {
  // The module-level version of the guarantee: a module that cannot spell
  // audioDeviceId cannot decorate a box with one. It reads the preset's raw
  // fields, so this is not idle — it is the only place in the preview path
  // where an unwhitelisted key could be reached, and it is reached only at tags
  // the diff already named.
  const src = codeOnly(ui('presetpreview.js'));
  for (const tag of MACHINE_TAGS) {
    assert.ok(!src.includes(tag), `presetpreview.js names ${tag}`);
  }
});

test('the preview model has no DOM in it', () => {
  // The house pattern, and the reason these tests can drive it for real:
  // package.json is frozen, there is no jsdom, and a module that reaches for
  // document is a module that can only be tested by a shim widened until it
  // passes.
  const src = codeOnly(ui('presetpreview.js'));
  for (const forbidden of ['document', 'window', 'classList', 'appendChild']) {
    assert.ok(!src.includes(forbidden), `presetpreview.js touches ${forbidden}; the DOM half belongs to settings.js`);
  }
});
