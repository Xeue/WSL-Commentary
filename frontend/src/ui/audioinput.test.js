/**
 * Tests for the one commentary-input picker: the grouping, what selecting an
 * entry SETS, and what happens to a saved device that is not plugged in today.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURES THESE PREVENT =========================
 *
 * 1. HALF A SETUP. The commentary input used to be two controls asking one
 *    question: a <select> chose the SUBSYSTEM and the device came from
 *    somewhere else — the main screen's dropdown for the computer's endpoints,
 *    a free-text box for the card's persistent-id. Setting one and not the
 *    other saves cleanly and captures from the wrong place. audioSourceKind
 *    "decklink" beside a leftover audioDeviceId is two answers to one question,
 *    and which one wins is a property of internal/gst rather than of anything on
 *    screen. deriveAudioInputEffects is the fix, and the CLEARING is as
 *    load-bearing as the setting.
 *
 * 2. AN ID INFERRED FROM ITS SHAPE. internal/gst/gst.go documents Device.ID as
 *    opaque and per-platform, and Kind is a field precisely so nobody has to
 *    guess. Measured and directly relevant: the fitted UltraStudio's
 *    Audio/Source and Video/Source entries BOTH publish 2747401380, because a
 *    persistent-id names the CARD and not the stream — so an id says less here
 *    than it looks like it does, and a module that grouped by id shape would be
 *    wrong on the very hardware this feature exists for.
 *
 * 3. A SAVED DEVICE SILENTLY DROPPED. A <select> discards a value it has no
 *    option for. The operator then reads a plausible device on screen while
 *    Start refuses the id actually in config.json — two truths they cannot
 *    reconcile from the GUI. This is the same defect describeDeviceSelection
 *    was written for, arriving through a different control, and it is answered
 *    with the same words.
 *
 * Everything here is pure and driven for real. The DOM half — that settings.js
 * builds the <optgroup>s from these plans and writes the three fields — is
 * asserted from settings.js's text, for the reason settings.test.js's header
 * gives at length: settings.js builds against a real DOM and package.json is
 * frozen, so there is no jsdom, and a shim widened until a test passes stops
 * being evidence.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  AUDIO_SOURCE_NATIVE,
  AUDIO_SOURCE_DECKLINK,
  NATIVE_GROUP_LABEL,
  DECKLINK_GROUP_LABEL,
  DEFAULT_INPUT_LABEL,
  ANY_CARD_LABEL,
  normaliseAudioSourceKind,
  encodeAudioInput,
  decodeAudioInput,
  deriveAudioInputEffects,
  planAudioInputs,
} from './audioinput.js';
import { DEVICE_KIND } from './devices.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/**
 * THE MEASURED MACHINE, as small as it can be and still exercise the fault this
 * whole feature exists for: one Blackmagic box enumerating TWICE under the same
 * name, once through the platform's audio stack and once through the DeckLink
 * driver, plus a real microphone and a slab of the Dante wall to sort past.
 *
 * The persistent-id is the fitted UltraStudio 4K Mini's real one.
 */
const CARD_ID = '2747401380';
const CARD_NAME = 'Blackmagic UltraStudio 4K Mini';
const MEASURED = Object.freeze([
  { id: 'dvs-1-2', name: 'DVS Receive  1-2 (Dante Virtual Soundcard)', kind: DEVICE_KIND.NATIVE },
  { id: 'coreaudio-uid-ultrastudio', name: CARD_NAME, kind: DEVICE_KIND.NATIVE },
  { id: 'focusrite-uid', name: 'Microphone (Focusrite Scarlett Solo)', kind: DEVICE_KIND.NATIVE },
  { id: CARD_ID, name: CARD_NAME, kind: DEVICE_KIND.DECKLINK },
]);

/** The config triple a seat holds, so a case can change one part of it. */
function saved(overrides) {
  return {
    audioSourceKind: AUDIO_SOURCE_NATIVE,
    audioDeviceId: '',
    decklinkPersistentId: '',
    ...overrides,
  };
}

/** groupNamed pulls one group out of a plan by its label. */
function groupNamed(plan, label) {
  return plan.groups.find((g) => g.label === label) || null;
}

// ---------------------------------------------------------------------------
// The kinds, across the language boundary
// ---------------------------------------------------------------------------

test('the two kinds are spelled the way internal/config spells them', () => {
  // A drift here is the SILENT kind. Go's Validate refuses an unrecognised kind
  // by name, which is at least loud; a kind that differs only in case would save
  // cleanly and capture from the wrong subsystem.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /AudioSourceNative = "native"/, 'internal/config no longer spells it "native"');
  assert.match(go, /AudioSourceDeckLink = "decklink"/, 'internal/config no longer spells it "decklink"');
  assert.equal(AUDIO_SOURCE_NATIVE, 'native');
  assert.equal(AUDIO_SOURCE_DECKLINK, 'decklink');

  // And they must be the SAME two strings internal/gst tags a device with, or
  // the grouping below files every card under the computer's audio devices.
  assert.equal(AUDIO_SOURCE_NATIVE, DEVICE_KIND.NATIVE);
  assert.equal(AUDIO_SOURCE_DECKLINK, DEVICE_KIND.DECKLINK);
});

test('an unrecognised kind reads as native, which is what the machine did before', () => {
  // Every config.json written before the field existed holds nothing at all, and
  // refusing it would make Settings unsavable on the first launch after an
  // upgrade — over a field the operator never touched.
  for (const value of [undefined, null, '', 'NATIVE', 'blackmagic', 'coreaudio', 2, {}]) {
    assert.equal(
      normaliseAudioSourceKind(value),
      AUDIO_SOURCE_NATIVE,
      `${JSON.stringify(value)} must read as native`,
    );
  }
  assert.equal(normaliseAudioSourceKind('decklink'), AUDIO_SOURCE_DECKLINK);
});

// ---------------------------------------------------------------------------
// The option value: built here, never inferred
// ---------------------------------------------------------------------------

test('an option value round-trips a kind and an OPAQUE id, colons and all', () => {
  // THE RULE THIS DOES NOT BREAK. internal/gst documents Device.ID as opaque and
  // per-platform, and nothing above it may assume a shape. This splits on the
  // FIRST separator only, so the left half is one of two known words — neither
  // of which contains a colon — and everything after it is the id verbatim,
  // however many more colons it has. A WASAPI GUID, a CoreAudio unique-id, a
  // persistent-id and whatever a future provider publishes all survive.
  const ids = [
    '',
    CARD_ID,
    '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}',
    'BF568F24-731B-41DB-932E-AC7E260BC71A',
    'a:b:c:d',
    ':leading-colon',
    'trailing-colon:',
    'BuiltInMicrophoneDevice',
  ];
  for (const kind of [AUDIO_SOURCE_NATIVE, AUDIO_SOURCE_DECKLINK]) {
    for (const id of ids) {
      assert.deepEqual(
        decodeAudioInput(encodeAudioInput(kind, id)),
        { kind, id },
        `${kind}/${JSON.stringify(id)} did not survive the round trip`,
      );
    }
  }
});

test('a value that is not one this module built reads as the default input', () => {
  // A <select> that has lost its selection reports ''. The safe reading of that
  // is what the machine did before any of this existed — never a DeckLink
  // capture, which on a machine with no card fails as not-negotiated (-4).
  for (const value of ['', 'nonsense', undefined, null, 42]) {
    assert.deepEqual(decodeAudioInput(value), { kind: AUDIO_SOURCE_NATIVE, id: '' });
  }
  // And a value naming a kind that does not exist is native too, with its id
  // still carried: the id is not the part that was wrong.
  assert.deepEqual(decodeAudioInput('ndi:some-id'), { kind: AUDIO_SOURCE_NATIVE, id: 'some-id' });
});

test('nothing in this module parses a device id', () => {
  // The id is opaque. It may be carried, compared whole and handed back; it may
  // never be inspected for a prefix, a shape or a number — and least of all
  // here, where the DeckLink card's audio and video entries publish the SAME id.
  const src = codeOnly(ui('audioinput.js'));
  assert.equal(
    /\b(id|savedId)\s*\.\s*(startsWith|endsWith|includes|match|slice|split|indexOf)\s*\(/.test(src),
    false,
    'audioinput.js is taking a device id apart; internal/gst documents it as opaque',
  );
  // The grouping keys on Kind, which is the field that exists so nobody has to
  // guess, and it reads it through the one module that mirrors internal/gst.
  assert.match(src, /d\.kind !== DEVICE_KIND\.DECKLINK/);
  assert.match(src, /d\.kind === DEVICE_KIND\.DECKLINK/);
});

// ---------------------------------------------------------------------------
// Selecting an entry IS the setup
// ---------------------------------------------------------------------------

test('choosing a computer input sets the device and CLEARS the card', () => {
  const effects = deriveAudioInputEffects(encodeAudioInput(AUDIO_SOURCE_NATIVE, 'focusrite-uid'));
  assert.deepEqual(effects, {
    audioSourceKind: AUDIO_SOURCE_NATIVE,
    audioDeviceId: 'focusrite-uid',
    decklinkPersistentId: '',
  });
});

test('choosing the card sets the persistent id and CLEARS the audio device', () => {
  // The one that would otherwise be half-done. An audioDeviceId left over from
  // the computer's audio stack sitting beside audioSourceKind "decklink" is two
  // answers to one question.
  const effects = deriveAudioInputEffects(encodeAudioInput(AUDIO_SOURCE_DECKLINK, CARD_ID));
  assert.deepEqual(effects, {
    audioSourceKind: AUDIO_SOURCE_DECKLINK,
    audioDeviceId: '',
    decklinkPersistentId: CARD_ID,
  });
});

test('the two blank selections are real settings, not absences', () => {
  // Native with no id is the platform's default input, which is what
  // blankConfig holds and every fresh seat starts on. DeckLink with no id is
  // "the card in this machine", which is right on every seat with one card.
  assert.deepEqual(deriveAudioInputEffects(encodeAudioInput(AUDIO_SOURCE_NATIVE, '')), {
    audioSourceKind: AUDIO_SOURCE_NATIVE,
    audioDeviceId: '',
    decklinkPersistentId: '',
  });
  assert.deepEqual(deriveAudioInputEffects(encodeAudioInput(AUDIO_SOURCE_DECKLINK, '')), {
    audioSourceKind: AUDIO_SOURCE_DECKLINK,
    audioDeviceId: '',
    decklinkPersistentId: '',
  });
});

test('every option a plan offers derives a complete, consistent setup', () => {
  // The property that matters, asserted over the whole list rather than over the
  // two cases somebody thought of: there is no option anywhere in this control
  // that leaves both id fields set, or that sets the id belonging to the other
  // subsystem.
  for (const config of [
    saved({}),
    saved({ audioDeviceId: 'focusrite-uid' }),
    saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK, decklinkPersistentId: CARD_ID }),
    saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK }),
  ]) {
    for (const group of planAudioInputs(MEASURED, config).groups) {
      for (const option of group.options) {
        const e = deriveAudioInputEffects(option.value);
        assert.ok(
          e.audioDeviceId === '' || e.decklinkPersistentId === '',
          `${option.label} sets both device ids: ${JSON.stringify(e)}`,
        );
        if (e.audioSourceKind === AUDIO_SOURCE_DECKLINK) {
          assert.equal(e.audioDeviceId, '', `${option.label} is a card option carrying an endpoint id`);
        } else {
          assert.equal(e.decklinkPersistentId, '', `${option.label} is a native option carrying a card id`);
        }
      }
    }
  }
});

// ---------------------------------------------------------------------------
// The groups
// ---------------------------------------------------------------------------

test('the card’s audio and the computer’s audio are separated by KIND', () => {
  // The measured machine's whole point: one physical box, two entries, the same
  // name. They land in different groups because their KIND differs — not because
  // anything looked at the two ids, which on this hardware would not have helped
  // (the card's audio and video publish the same one).
  const plan = planAudioInputs(MEASURED, saved({}));
  const native = groupNamed(plan, NATIVE_GROUP_LABEL);
  const card = groupNamed(plan, DECKLINK_GROUP_LABEL);
  assert.ok(native && card, 'both groups must exist when both kinds are enumerated');

  assert.deepEqual(
    card.options.map((o) => o.label),
    [CARD_NAME],
    'the DeckLink group is the card entry alone',
  );
  assert.equal(card.options[0].value, encodeAudioInput(AUDIO_SOURCE_DECKLINK, CARD_ID));

  // And the CoreAudio twin — the one measured at -96 dBFS on all sixteen
  // channels with the mic live — is in the other group, under the same name.
  const twin = native.options.find((o) => o.label === CARD_NAME);
  assert.ok(twin, 'the native twin must still be offered; it is a real device');
  assert.equal(twin.value, encodeAudioInput(AUDIO_SOURCE_NATIVE, 'coreaudio-uid-ultrastudio'));
});

test('the group headings are in the operator’s language, not the driver’s', () => {
  // "CoreAudio", "WASAPI" and "DeckLink" are the names of software nobody at a
  // commentary position chose or installed. SDI and HDMI are the cables in front
  // of them. The headings have to answer "which one is my microphone" to
  // somebody who knows only where they plugged it in.
  assert.match(NATIVE_GROUP_LABEL, /computer/i);
  assert.match(DECKLINK_GROUP_LABEL, /SDI|HDMI/);
  for (const label of [NATIVE_GROUP_LABEL, DECKLINK_GROUP_LABEL]) {
    assert.equal(
      /CoreAudio|WASAPI|GStreamer|persistent[- ]id/i.test(label),
      false,
      `"${label}" names an implementation the operator did not choose`,
    );
  }
});

test('the names are bare inside the groups, because the heading already said it', () => {
  // devices.js's labelDevices suffixes a DeckLink entry with "SDI/HDMI audio"
  // and its native twin with "computer sound input" — necessary on the main
  // screen, where the two land on adjacent lines of one FLAT list under the same
  // name. Here the <optgroup> has said it once, above them both, and repeating
  // it on every line is the noise that stops labels being read.
  const plan = planAudioInputs(MEASURED, saved({}));
  for (const group of plan.groups) {
    for (const option of group.options) {
      assert.equal(
        /SDI\/HDMI audio|computer sound input/.test(option.label),
        false,
        `"${option.label}" repeats its own group heading`,
      );
    }
  }
});

test('the computer’s inputs are in display order, with the default first', () => {
  // sortDevices' order: real microphones above the virtual wall, numbers
  // compared as numbers. The measured machine files "DVS Receive  1-2" above the
  // one Focusrite the commentator actually uses if this is left alphabetical.
  const plan = planAudioInputs(MEASURED, saved({}));
  const labels = groupNamed(plan, NATIVE_GROUP_LABEL).options.map((o) => o.label);
  assert.equal(labels[0], DEFAULT_INPUT_LABEL, 'the default input is where a seat starts');
  assert.ok(
    labels.indexOf('Microphone (Focusrite Scarlett Solo)') < labels.indexOf(MEASURED[0].name),
    'a real microphone must sort above the Dante wall',
  );
});

test('a machine with no card offers no card group at all', () => {
  // Never an empty flourish. A seat that has never had a card must see this
  // control exactly as it would have looked before any of this existed.
  const plan = planAudioInputs(
    MEASURED.filter((d) => d.kind !== DEVICE_KIND.DECKLINK),
    saved({}),
  );
  assert.equal(groupNamed(plan, DECKLINK_GROUP_LABEL), null);
  assert.equal(plan.absent, false);
});

test('“the card in this machine” is offered only when it is what is saved', () => {
  // An empty decklinkPersistentId means the card in this machine and is the
  // right answer on every one-card seat — but on a machine whose card
  // enumerates, naming it is unambiguous, and offering both would put two
  // entries meaning the same thing on adjacent lines.
  const explicit = planAudioInputs(
    MEASURED,
    saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK, decklinkPersistentId: CARD_ID }),
  );
  assert.deepEqual(
    groupNamed(explicit, DECKLINK_GROUP_LABEL).options.map((o) => o.label),
    [CARD_NAME],
    'the blank-card entry must not appear beside the card it resolves to',
  );

  // And when it IS what is saved, it is offered and selected — a saved setting
  // is never quietly rewritten into a different one, even an equivalent one.
  const blank = planAudioInputs(MEASURED, saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK }));
  const card = groupNamed(blank, DECKLINK_GROUP_LABEL);
  assert.equal(card.options[0].label, ANY_CARD_LABEL);
  assert.equal(blank.value, encodeAudioInput(AUDIO_SOURCE_DECKLINK, ''));
  assert.equal(blank.absent, false, 'it resolves to a card that IS fitted; nothing is missing');
});

// ---------------------------------------------------------------------------
// The way out: a saved device that is not plugged in today
// ---------------------------------------------------------------------------

test('a saved device that is not in today’s list is kept and marked', () => {
  // A docked USB interface, a stopped Dante Virtual Soundcard, a config.json
  // copied from another machine. It used to fall through the dropdown's fill
  // silently, leaving device #1 showing as selected: the operator reads a
  // plausible device while Start refuses the id actually saved.
  const plan = planAudioInputs(MEASURED, saved({ audioDeviceId: 'unplugged-interface' }));
  assert.equal(plan.absent, true);
  assert.equal(plan.value, encodeAudioInput(AUDIO_SOURCE_NATIVE, 'unplugged-interface'));

  const flat = plan.groups.flatMap((g) => g.options);
  const marker = flat.find((o) => o.value === plan.value);
  assert.ok(marker, 'the saved selection must have an option to BE the selection of');
  assert.match(marker.label, /NOT PRESENT/, 'and it must say so');
  assert.ok(
    marker.label.includes('unplugged-interface'),
    'with the id in it — the id is the only handle the operator has on what was saved',
  );
});

test('a saved CARD that is not fitted is kept and marked too', () => {
  // The same rule pointed at the other subsystem: a config.json from a seat with
  // a card, opened on a laptop without one. Go's pre-flight refuses this at
  // START naming the card; this is the same fact, earlier, and on the control.
  const plan = planAudioInputs(
    MEASURED.filter((d) => d.kind !== DEVICE_KIND.DECKLINK),
    saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK, decklinkPersistentId: '9999999' }),
  );
  assert.equal(plan.absent, true);
  const marker = plan.groups.flatMap((g) => g.options).find((o) => o.value === plan.value);
  assert.match(marker.label, /NOT PRESENT/);
  assert.ok(marker.label.includes('9999999'));
});

test('the absence marker is worded the way the main screen words it', () => {
  // One spelling of this in the application. The operator meets it on the main
  // screen's input dropdown and on this picker, about the same device, and two
  // different sentences about one fact reads as two different faults.
  const plan = planAudioInputs([], saved({ audioDeviceId: 'gone' }));
  const marker = plan.groups.flatMap((g) => g.options).find((o) => o.value === plan.value);
  const home = ui('devices.js');
  assert.match(home, /NOT PRESENT — \$\{id\}/, 'devices.js must still own the wording');
  assert.equal(marker.label, 'NOT PRESENT — gone');
});

test('an unread device list still shows what is saved', () => {
  // Null and [] are different claims and only the second says this machine has
  // no capture devices — but neither is a reason to stop showing the operator
  // what their own configuration holds.
  for (const devices of [null, undefined, [], 'not a list']) {
    const plan = planAudioInputs(devices, saved({ audioDeviceId: 'focusrite-uid' }));
    assert.equal(plan.value, encodeAudioInput(AUDIO_SOURCE_NATIVE, 'focusrite-uid'));
    assert.ok(
      plan.groups.flatMap((g) => g.options).some((o) => o.value === plan.value),
      `${JSON.stringify(devices)} must still leave the saved selection selectable`,
    );
  }
});

test('the two id spaces cannot match each other by coincidence', () => {
  // The card's audio and its video publish the SAME persistent-id, and nothing
  // says a CoreAudio unique-id could never equal one. If the presence test were
  // over the whole list rather than over the matching group, a native device
  // sharing an id with a card would report a card as present — which is the
  // exact wrong-subsystem match this module exists to prevent.
  const collision = [{ id: CARD_ID, name: 'Some USB thing', kind: DEVICE_KIND.NATIVE }];
  const plan = planAudioInputs(
    collision,
    saved({ audioSourceKind: AUDIO_SOURCE_DECKLINK, decklinkPersistentId: CARD_ID }),
  );
  assert.equal(
    plan.absent,
    true,
    'a native device with the same id must not satisfy a saved CARD selection',
  );
});

test('the input is never mutated', () => {
  // app.js keeps currentInputDevices, and a plan that reordered or decorated it
  // would be a side effect on somebody else's state — the rule sortDevices and
  // labelDevices already follow.
  const devices = MEASURED.map((d) => ({ ...d }));
  const before = JSON.stringify(devices);
  planAudioInputs(devices, saved({ audioDeviceId: 'focusrite-uid' }));
  assert.equal(JSON.stringify(devices), before, 'planAudioInputs mutated the device list');
});

// ---------------------------------------------------------------------------
// The form's half
// ---------------------------------------------------------------------------

test('the Settings form builds ONE picker and hides the three fields behind it', () => {
  const js = ui('settings.js');
  // One control, and the three config fields it writes have no rows of their
  // own. They are still registered, still populated and still collected — see
  // settings.test.js's data-loss guard, which reflects over config.go's tags.
  assert.match(js, /audioInputSelect\.id = 'f-commentaryInput'/);
  assert.match(js, /addHiddenField\('audioSourceKind',/);
  assert.match(js, /addHiddenField\('decklinkPersistentId',/);
  assert.match(js, /addHiddenField\('audioDeviceId',/);

  // Drawn from the pure plan, into <optgroup>s, with the selection assigned
  // LAST — a <select> silently discards a value it has no option for yet.
  const render = js.slice(js.indexOf('function renderAudioInput()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.ok(body.length > 0, 'settings.js must define renderAudioInput');
  assert.match(body, /planAudioInputs\(inputDevices, \{/);
  assert.match(body, /fillGroupedSelect\(audioInputSelect, plan, null\)/);
  const fill = js.slice(js.indexOf('function fillGroupedSelect('));
  assert.match(fill.slice(0, fill.indexOf('\n}')), /createElement\('optgroup'\)/);

  // AND IT DOES NOT WRITE THE FIELDS BACK. Rebuilding a list is not the operator
  // choosing from it: a saved-but-absent device must stay saved, which is the
  // whole point of showing it as absent rather than dropping it.
  for (const field of ['audioSourceKind', 'audioDeviceId', 'decklinkPersistentId']) {
    assert.equal(
      body.includes(`fields.${field}.input.value =`),
      false,
      `renderAudioInput writes ${field}; redrawing the list would then rewrite the configuration`,
    );
  }
});

test('a refusal on any of the three fields lands on the picker, which is their row', () => {
  // THE TRAP THAT COMES WITH HIDING A FIELD. Two of the three can still be
  // refused by validateConfig on a value the picker itself would never produce,
  // both arriving from a config.json written by an older build or edited by
  // hand: a decklinkPersistentId of "0" — the enumeration index Blackmagic's own
  // tools show, which the free-text box this control replaced accepted happily —
  // and an audioDeviceId holding a Windows RENDER endpoint GUID, the operator's
  // measured failure. Both are real, and both are still refused.
  const validate = ui('validate.js');
  assert.match(validate, /device number/, 'validate.js must still refuse a card device number');

  // Without a surrogate their errors would be written to an errorEl attached to
  // nothing: a Save that fails, "Fix the highlighted fields before saving", and
  // no highlighted field anywhere on the screen. This is the same rule m2lxHost
  // and eventId already follow, applied to the three fields that lost their rows.
  const js = ui('settings.js');
  const surrogates = js.slice(js.indexOf('const ERROR_SURROGATES'), js.indexOf('function displayErrors'));
  for (const field of ['audioSourceKind', 'audioDeviceId', 'decklinkPersistentId']) {
    assert.ok(
      surrogates.includes(`${field}: () => ({ errorEl: audioInputRow.errorEl, input: audioInputSelect })`),
      `${field} has no visible row and no surrogate: its refusal would be invisible`,
    );
  }
  // And the row is CLEARED by hand, because clearAllErrors walks `fields` and
  // this row is not one — a surrogate that is never cleared leaves last save's
  // message under a row the operator has since corrected.
  const display = js.slice(js.indexOf('function displayErrors(errors)'));
  assert.match(
    display.slice(0, display.indexOf('\n  }')),
    /for \(const row of \[liveURLRow, audioInputRow\]\)/,
    'both surrogate rows must be cleared before the new errors are written',
  );
});

test('the picker is redrawn from populate and from the device listing, and never listened for', () => {
  const js = ui('settings.js');
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(populate, /renderAudioInput\(\)/, 'the screen must open showing what is saved');
  // AFTER all three fields, because the plan is built FROM them.
  assert.ok(
    populate.indexOf('renderAudioInput()') > populate.indexOf('fields.decklinkPersistentId.input.value'),
    'the picker is drawn from the three fields, so it must come after all three',
  );
  const refresh = js.slice(js.indexOf('async function refreshInputDevices()'));
  assert.match(refresh.slice(0, refresh.indexOf('\n  }')), /renderAudioInput\(\)/);
});

test('the DeckLink commentary leg is BUILT, and nothing on the row says otherwise', () => {
  // ===================== THE HALF THAT WAS NOT BUILT, AND NOW IS =============
  //
  // This test replaces the one that pinned DECKLINK_AUDIO_NOT_BUILT — a line on
  // the picker row saying a DeckLink commentary input would be refused at START.
  // It was true, and it was said here rather than by greying the group out so
  // that the operator met the refusal at the moment of choosing. The leg exists
  // now, so the line is gone and this pins the new truth in both directions: the
  // Go gate is deleted, and no frontend module renders a sentence about it.
  //
  // THE GO SIDE IS THE SUBJECT and is read out of app.go, exactly as the old
  // test read it. The two must move together or the screen and the application
  // disagree about what pressing START will do.
  const go = read(repoRoot, 'app.go');
  assert.doesNotMatch(
    go,
    /THE DECKLINK AUDIO LEG IS NOT BUILT IN THIS REVISION/,
    'the staging gate is back in app.go; if the audio leg has been reverted, the line on the ' +
      'picker row has to come back with it, or the operator meets the refusal at kick-off',
  );
  assert.doesNotMatch(go, /cannot capture AUDIO from one/, 'and so does its refusal');

  // AND THE PIPELINE REALLY BUILDS IT. Deleting a refusal without building the
  // leg is the one change this test must not be able to pass: an empty
  // audioDeviceId on osxaudiosrc or wasapi2src is the SYSTEM DEFAULT INPUT, so a
  // DeckLink seat that reached the platform element would put the match on air
  // off the laptop's built-in microphone with every lamp green.
  const gst = read(repoRoot, 'internal', 'gst', 'gst_cgo.go');
  assert.match(
    gst,
    /audioCaptureFactory/,
    'internal/gst does not build a DeckLink commentary source, so removing the warning would ' +
      'leave the operator with a seat that silently captures the wrong microphone',
  );
  assert.match(
    read(repoRoot, 'app.go'),
    /AudioCaptureID = plan\.AudioCaptureID/,
    'app.go never hands the resolved card to the pipeline, so audioSourceKind "decklink" would ' +
      'build the platform source with no device — the system default input',
  );

  // NOTHING ON THE SCREEN SAYS THE FEATURE IS MISSING. The constant is gone from
  // the module that owned it and from the screen that rendered it; a leftover
  // sentence about an unbuilt feature outliving the feature is exactly what the
  // old test was written to prevent, in the other direction.
  const js = ui('settings.js');
  assert.doesNotMatch(codeOnly(js), /DECKLINK_AUDIO_NOT_BUILT/, 'settings.js must not render it');
  assert.doesNotMatch(
    codeOnly(ui('audioinput.js')),
    /DECKLINK_AUDIO_NOT_BUILT/,
    'audioinput.js must not export it',
  );

  // THE ONE NOTE LEFT is about THIS SELECTION — a saved device that is not
  // plugged in — and it must still be rendered from both paths: loading a
  // configuration that names an absent device, and choosing now.
  const apply = js.slice(js.indexOf('function applyAudioInputSelection()'));
  assert.match(
    apply.slice(0, apply.indexOf('\n  }')),
    /renderAudioInputNote\(false\)/,
    'choosing an entry must clear a note left over from the previous selection',
  );
  const render = js.slice(js.indexOf('function renderAudioInput()'));
  assert.match(
    render.slice(0, render.indexOf('\n  }')),
    /renderAudioInputNote\(plan\.absent\)/,
    'opening a configuration that names a device this machine does not have must say so',
  );

  // THE OPTION IS STILL OFFERED, and now it is offered because it WORKS.
  // Disabling the group would be the owner's original complaint back again
  // ("The declink input is grayed out in settings?").
  assert.doesNotMatch(
    js,
    /decklinkOptions[\s\S]{0,200}disabled = true/,
    'the DeckLink group must stay selectable: the screen and config.json must not disagree',
  );
});
