/**
 * Tests for the device dropdowns' pure logic: display order, the
 * saved-but-missing marker, and the Windows endpoint-id namespace helpers.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURES THESE PREVENT =========================
 *
 * 1. THE SILENTLY-WRONG SELECTION. A saved audioDeviceId absent from today's
 *    list used to fall through fillDeviceSelect's `if`, leaving device #1
 *    showing as selected. The operator reads a plausible device on screen
 *    while Start refuses the id actually in config.json — two statements
 *    about one setting that cannot both be true, and nothing on screen to
 *    reconcile them. describeDeviceSelection is what makes the absence
 *    VISIBLE, id and all.
 *
 * 2. THE UNREADABLE LIST. The measured dev machine offers eight
 *    "DVS Receive N-M" pairs and NDI virtual audio alongside the one real
 *    microphone. Byte order both interleaves 2 with 10 (the classic
 *    1, 10, 11 … 19, 2 the operator already complained about on the mixer)
 *    and files the virtual wall above the hardware. sortDevices is the fix,
 *    mirroring mixer/model.js's natural-sort idiom.
 *
 * 3. THE WRONG NAMESPACE, PASTED. wasapi2 republishes every playback endpoint
 *    as a capture "loopback" device; an operator selected one and the sender
 *    blamed the SRT network forever. The Settings form's paste fields refuse
 *    a POSITIVELY wrong-namespace GUID via the helpers here, which must match
 *    internal/gst/device_id.go — including its asymmetry: unknown shapes are
 *    NEITHER capture nor render, and are never refused.
 *
 * 4. TWO IDENTICAL LINES, ONE OF THEM SILENT. A Blackmagic UltraStudio
 *    enumerates twice — measured — once through CoreAudio and once through the
 *    DeckLink driver, under the same name. The DeckLink entry carries the
 *    commentator's microphone; the CoreAudio entry reads -96 dBFS on all
 *    sixteen channels with the mic live. labelDevices is what makes the two
 *    tellable apart, and the tests below hold it to two things: it must key on
 *    Device.Kind and NEVER on the id (internal/gst/gst.go documents the id as
 *    opaque, and the DeckLink card's audio and video share theirs anyway), and
 *    it must degrade to the bare name rather than guess.
 *
 * The DOM half — that fillDeviceSelect actually calls these — is asserted
 * from home.js's text, in the manner of returnsource.test.js and
 * overlay.test.js: home.js needs a real DOM, package.json is frozen, and a
 * shim widened until a test passes stops being evidence.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  CAPTURE_ENDPOINT_PREFIX,
  RENDER_ENDPOINT_PREFIX,
  isCaptureEndpointId,
  isRenderEndpointId,
  describeDeviceSelection,
  sortDevices,
  compareDevicesForDisplay,
  labelDevices,
  DEVICE_KIND,
} from './devices.js';
import { validateConfig } from './validate.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);
const repoRoot = join(here, '..', '..', '..');

function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

/** A capture-namespace device, so cases below only vary what they test. */
function dev(id, name) {
  return { id, name };
}

// --------------------------------------------------------------------------
// describeDeviceSelection: the saved-but-missing marker
// --------------------------------------------------------------------------

const MIC = dev('{0.0.1.00000000}.{9f6d2b18-0004-438e-9003-51a46e13a4d5}', 'Microphone (Focusrite Scarlett 2i2 USB)');
const DVS12 = dev('{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}', 'DVS Receive  1-2 (Dante Virtual Soundcard)');

test('a saved id that is in the list needs no marker', () => {
  const s = describeDeviceSelection([MIC, DVS12], MIC.id);
  assert.equal(s.present, true);
  assert.equal(s.savedId, MIC.id);
  assert.equal(s.label, '', 'present means nothing to mark');
});

test('a saved id absent from the list is marked, NAMING the id', () => {
  // The id is the only handle the operator has on what was saved — a docked
  // USB interface, a stopped Dante Virtual Soundcard, a config.json copied
  // from another machine. A marker that just said "missing" would name
  // nothing anybody can go and plug back in.
  const gone = '{0.0.1.00000000}.{11111111-2222-3333-4444-555555555555}';
  const s = describeDeviceSelection([MIC, DVS12], gone);
  assert.equal(s.present, false);
  assert.equal(s.savedId, gone);
  assert.match(s.label, /NOT PRESENT/);
  assert.ok(s.label.includes(gone), 'the marker must quote the missing id');
});

test('no saved id means no marker: there is no selection to be absent', () => {
  for (const blank of ['', null, undefined, 0, {}]) {
    const s = describeDeviceSelection([MIC], blank);
    assert.equal(s.present, true, `${JSON.stringify(blank)} is not a saved id`);
    assert.equal(s.label, '');
  }
});

test('an empty device list with a saved id reports the id as absent', () => {
  // fillDeviceSelect never reaches this case — its empty-list branch returns
  // first, unchanged (asserted from source below) — but the pure function
  // must still tell the truth for any other caller.
  const s = describeDeviceSelection([], MIC.id);
  assert.equal(s.present, false);
  assert.ok(s.label.includes(MIC.id));
});

// --------------------------------------------------------------------------
// sortDevices: numbers as numbers, hardware above the virtual walls
// --------------------------------------------------------------------------

test('embedded numbers compare as numbers: Mic 1, Mic 2, Mic 10', () => {
  // The operator's earlier complaint about the mixer, in their words: "the
  // classic 1, 10, 11 ... 19, 2 alphabetical not numerical sort". Byte order
  // yields Mic 1, Mic 10, Mic 2; this must not.
  const sorted = sortDevices([dev('c', 'Mic 10'), dev('b', 'Mic 2'), dev('a', 'Mic 1')]);
  assert.deepEqual(sorted.map((d) => d.name), ['Mic 1', 'Mic 2', 'Mic 10']);
});

test('a real microphone sorts above NDI/webcam virtual audio, which sorts above DVS', () => {
  const sorted = sortDevices([
    dev('dvs', 'DVS Receive 1-2 (Dante Virtual Soundcard)'),
    dev('ndi', 'Webcam 1 (NDI Webcam Audio)'),
    dev('mic', 'Microphone (Focusrite Scarlett 2i2 USB)'),
  ]);
  assert.deepEqual(sorted.map((d) => d.id), ['mic', 'ndi', 'dvs']);
});

test('the DVS wall itself is in natural order, 2-3 before 10-11', () => {
  // Eight pairs on the measured machine. "DVS Receive  1-2" carries a double
  // space (mirrored in backend.js's fakes on purpose); the comparator must
  // survive it rather than assume single-spaced names.
  const sorted = sortDevices([
    dev('a', 'DVS Receive  10-11 (Dante Virtual Soundcard)'),
    dev('b', 'DVS Receive  2-3 (Dante Virtual Soundcard)'),
    dev('c', 'DVS Receive  1-2 (Dante Virtual Soundcard)'),
  ]);
  assert.deepEqual(sorted.map((d) => d.id), ['c', 'b', 'a']);
});

test('Dante’s mixed-width padding cannot put 11 above 1', () => {
  // THE REGRESSION THE OPERATOR REPORTED: Dante pads for column alignment, so
  // single digits carry a DOUBLE space ("DVS Receive  1-2") while double
  // digits carry a single one ("DVS Receive 11-12") — these are the literal
  // names from the machine's registry. Chunked without normalising, the text
  // chunks differ at the padding and the shorter one wins, so 11-12 sorted
  // ABOVE 1-2 and the previous test never saw it because it padded every name
  // the same way.
  const sorted = sortDevices([
    dev('k', 'DVS Receive 15-16 (Dante Virtual Soundcard)'),
    dev('j', 'DVS Receive 11-12 (Dante Virtual Soundcard)'),
    dev('i', 'DVS Receive  7-8 (Dante Virtual Soundcard)'),
    dev('h', 'DVS Receive  1-2 (Dante Virtual Soundcard)'),
  ]);
  assert.deepEqual(
    sorted.map((d) => d.id),
    ['h', 'i', 'j', 'k'],
    '1-2, 7-8, 11-12, 15-16 — numeric order regardless of alignment padding',
  );
});

test('an unrecognised device is presumed real hardware, rank 0', () => {
  // The measured failure of the mixer's ranking was the useful precedent: a
  // family nobody predicted should surface where the operator will see it,
  // not be filed under the block everyone has learned to ignore.
  const sorted = sortDevices([
    dev('ndi', 'Webcam 1 (NDI Webcam Audio)'),
    dev('new', 'Some Future Interface (XYZ Audio)'),
  ]);
  assert.deepEqual(sorted.map((d) => d.id), ['new', 'ndi']);
});

test('sortDevices returns a NEW array and does not reorder its input', () => {
  // app.js keeps currentInputDevices; a sort that mutated it would be a side
  // effect on somebody else's state.
  const input = [dev('b', 'Mic 2'), dev('a', 'Mic 1')];
  const before = input.slice();
  const sorted = sortDevices(input);
  assert.notEqual(sorted, input);
  assert.deepEqual(input, before, 'the caller\'s array is untouched');
});

test('sortDevices tolerates junk: not-an-array and nameless entries', () => {
  assert.deepEqual(sortDevices(null), []);
  assert.deepEqual(sortDevices(undefined), []);
  const sorted = sortDevices([dev('a', 'Mic 1'), { id: 'x' }]);
  assert.equal(sorted.length, 2, 'a nameless device is kept, not dropped');
});

test('the comparator is total: equal names fall back to the id', () => {
  const a = dev('a', 'Same Name');
  const b = dev('b', 'Same Name');
  assert.ok(compareDevicesForDisplay(a, b) < 0);
  assert.ok(compareDevicesForDisplay(b, a) > 0);
  assert.equal(compareDevicesForDisplay(a, a), 0);
});

// --------------------------------------------------------------------------
// labelDevices: two entries, one physical box
// --------------------------------------------------------------------------

/** The measured pair: one UltraStudio, enumerated by both providers. */
const ULTRASTUDIO_NATIVE = {
  id: 'BF568F24-731B-41DB-932E-AC7E260BC71A',
  name: 'Blackmagic UltraStudio 4K Mini',
  kind: DEVICE_KIND.NATIVE,
};
const ULTRASTUDIO_DECKLINK = {
  id: 'DL-0000000000000001',
  name: 'Blackmagic UltraStudio 4K Mini',
  kind: DEVICE_KIND.DECKLINK,
};

test('the twins are tellable apart, and BOTH lines say which one they are', () => {
  // One labelled line beside one bare line reads as though the bare one were
  // the ordinary choice — which is the wrong conclusion here, because the bare
  // one is the silent one. So in the collision both are named.
  const labelled = labelDevices([ULTRASTUDIO_NATIVE, ULTRASTUDIO_DECKLINK]);
  const byId = Object.fromEntries(labelled.map((d) => [d.id, d.label]));
  assert.match(byId[ULTRASTUDIO_DECKLINK.id], /SDI\/HDMI audio/);
  assert.match(byId[ULTRASTUDIO_NATIVE.id], /computer sound input/);
  assert.notEqual(byId[ULTRASTUDIO_DECKLINK.id], byId[ULTRASTUDIO_NATIVE.id]);
  for (const label of Object.values(byId)) {
    assert.ok(label.startsWith('Blackmagic UltraStudio 4K Mini'), 'the OS name still leads');
    assert.doesNotMatch(label, /DeckLink|CoreAudio|WASAPI/i, 'driver names are not the operator’s');
  }
});

test('a DeckLink entry is labelled even when nothing collides with it', () => {
  // It is the unfamiliar entry, and its audio comes from somewhere its name
  // does not mention: the signal on the card's input, not the computer's sound
  // system. An operator who has never seen one needs that said once.
  const [only] = labelDevices([ULTRASTUDIO_DECKLINK]);
  assert.match(only.label, /SDI\/HDMI audio/);
});

test('an ordinary microphone is NOT suffixed: noise is what stops labels being read', () => {
  const labelled = labelDevices([
    { id: 'a', name: 'Microphone (Focusrite Scarlett 2i2 USB)', kind: DEVICE_KIND.NATIVE },
    { id: 'b', name: 'DVS Receive  1-2 (Dante Virtual Soundcard)', kind: DEVICE_KIND.NATIVE },
  ]);
  assert.deepEqual(
    labelled.map((d) => d.label),
    ['Microphone (Focusrite Scarlett 2i2 USB)', 'DVS Receive  1-2 (Dante Virtual Soundcard)'],
  );
});

test('the twin test survives the padding and casing the OS actually emits', () => {
  // Same normalisation the comparator applies, and for the same measured
  // reason: display names carry alignment padding and inconsistent casing, and
  // a twin test that missed on either would leave the silent entry unlabelled.
  const labelled = labelDevices([
    { id: 'n', name: 'Blackmagic  UltraStudio 4K MINI ', kind: DEVICE_KIND.NATIVE },
    ULTRASTUDIO_DECKLINK,
  ]);
  assert.match(labelled[0].label, /computer sound input/);
  assert.ok(labelled[0].label.startsWith('Blackmagic  UltraStudio 4K MINI '), 'the name is shown as it is');
});

test('no kind means no claim: the label is the bare name', () => {
  // Every output device, every Windows entry today, and any older Go build. The
  // right answer to "I do not know" is to say nothing extra.
  const labelled = labelDevices([
    { id: 'a', name: 'Headphones (Focusrite Scarlett 2i2 USB)' },
    { id: 'b', name: 'Something New', kind: 'a-kind-from-the-future' },
  ]);
  assert.deepEqual(labelled.map((d) => d.label), ['Headphones (Focusrite Scarlett 2i2 USB)', 'Something New']);
});

test('labelDevices reads the KIND and never the id', () => {
  // internal/gst/gst.go documents Device.ID as opaque, and DeckLink audio and
  // video publish the SAME persistent-id because it names the CARD. A label
  // inferred from an id shape would be wrong on the next platform and is
  // unfalsifiable here, so this pins the negative: an id that screams DeckLink
  // gets nothing without the field.
  const [decoy] = labelDevices([{ id: 'decklink-audio-0', name: 'Decoy', kind: DEVICE_KIND.NATIVE }]);
  assert.equal(decoy.label, 'Decoy');
  const src = codeOnly(ui('devices.js'));
  const fn = src.slice(src.indexOf('export function labelDevices('));
  const body = fn.slice(0, fn.indexOf('\n}'));
  assert.doesNotMatch(body, /\.id\b/, 'nothing in labelDevices may look at an id');
});

test('labelDevices copies: neither the array nor its devices are mutated', () => {
  // app.js keeps currentInputDevices. A device that grew a label under it would
  // be a side effect on somebody else's state, exactly as an in-place sort was.
  const input = [ULTRASTUDIO_DECKLINK, ULTRASTUDIO_NATIVE];
  const before = JSON.parse(JSON.stringify(input));
  const labelled = labelDevices(input);
  assert.notEqual(labelled, input);
  assert.notEqual(labelled[0], input[0]);
  assert.deepEqual(input, before);
  assert.equal(Object.prototype.hasOwnProperty.call(input[0], 'label'), false);
});

test('labelDevices preserves order and tolerates junk', () => {
  assert.deepEqual(labelDevices(null), []);
  assert.deepEqual(labelDevices(undefined), []);
  const labelled = labelDevices([{ id: 'b', name: 'Zed' }, null, { id: 'a', name: 'Alpha' }]);
  assert.deepEqual(labelled.map((d) => d.label), ['Zed', '', 'Alpha']);
});

test('between identically named twins, the DeckLink one sorts FIRST', () => {
  // The one carrying the microphone goes on top. Two identical lines in a
  // dropdown, one of them silent, is an invitation to pick the wrong one.
  const sorted = sortDevices([ULTRASTUDIO_NATIVE, ULTRASTUDIO_DECKLINK]);
  assert.deepEqual(sorted.map((d) => d.kind), [DEVICE_KIND.DECKLINK, DEVICE_KIND.NATIVE]);
  assert.ok(compareDevicesForDisplay(ULTRASTUDIO_NATIVE, ULTRASTUDIO_DECKLINK) > 0);
  assert.ok(compareDevicesForDisplay(ULTRASTUDIO_DECKLINK, ULTRASTUDIO_NATIVE) < 0);
});

test('kind decides ORDER only between identical names, never across them', () => {
  // The families rule is unchanged: a DeckLink entry does not jump the queue
  // past the operator's Focusrite, and it does not sink below the Dante wall.
  const sorted = sortDevices([
    { id: 'dvs', name: 'DVS Receive  1-2 (Dante Virtual Soundcard)', kind: DEVICE_KIND.NATIVE },
    ULTRASTUDIO_DECKLINK,
    { id: 'mic', name: 'A Microphone', kind: DEVICE_KIND.NATIVE },
  ]);
  assert.deepEqual(sorted.map((d) => d.id), ['mic', ULTRASTUDIO_DECKLINK.id, 'dvs']);
});

test('the two fake device tables carry the same UltraStudio twin pair', () => {
  // backend.js's FAKE_DEVICES says in its own comment that it mirrors
  // internal/gst/gst_stub.go's defaultStubDevices, and nothing enforced it —
  // which is how the two were found holding DIFFERENT ids for the same fake
  // card, one of them a WASAPI-shaped GUID that no DeckLink device has ever
  // published. Neither table is reachable as an export (one is Go, the other is
  // module-private), so this reads both as source text, the same way the kind
  // strings below are pinned.
  //
  // What must agree is the TWIN PAIR specifically, because that pair is the
  // entire reason either table has a DeckLink entry: it is the case
  // labelDevices exists for, and a dev session or a Gate A run in which the
  // list never collides is one where the labelling can be broken unnoticed.
  const js = read(here, 'backend.js');
  const go = read(repoRoot, 'internal', 'gst', 'gst_stub.go');

  // The card's real persistent-id, measured off the UltraStudio 4K Mini. A bare
  // decimal in both, because that is what the decklink provider publishes.
  for (const [what, src] of [['backend.js', js], ['gst_stub.go', go]]) {
    assert.ok(
      src.includes('2747401380'),
      `${what} must carry the measured DeckLink persistent-id for the fake card`,
    );
    assert.ok(
      !/['"]\{0\.0\.1\.00000000\}\.\{5c2f88b3/.test(src),
      `${what} still gives the DeckLink twin an ENDPOINT-shaped id; a persistent-id has no ` +
        `braces and no prefix, and pretending otherwise teaches the one thing about the two ` +
        `id families that is not true`,
    );
  }

  // Both twins share a NAME. That is what makes it a collision rather than two
  // unrelated devices, and labelDevices suffixes a native entry only when a
  // DeckLink entry shares its name — so twins named differently would exercise
  // nothing.
  const twinName = 'Blackmagic UltraStudio 4K Mini';
  for (const [what, src] of [['backend.js', js], ['gst_stub.go', go]]) {
    const occurrences = src.split(twinName).length - 1;
    assert.ok(
      occurrences >= 2,
      `${what} names ${JSON.stringify(twinName)} ${occurrences} time(s); the twin pair must ` +
        `share one name or the collision the labelling exists for is never exercised`,
    );
  }
});

test('the kind strings are spelled exactly as internal/gst spells them', () => {
  // Two copies of a contract string that drift do not error — every device just
  // falls through to the unlabelled default, and the silent twin goes back to
  // being indistinguishable. Pinned the same way the endpoint prefixes are.
  //
  // THE SKIP IS GONE, AND ITS HISTORY IS THE ARGUMENT FOR REMOVING IT. This
  // test was written while Device.Kind was a sibling's unlanded change, and it
  // opened by skipping when the field was absent so that this JS would not go
  // red over somebody else's mid-flight Go. The guard looked for `Kind string`;
  // the field landed as `Kind DeviceKind`; and the test went on skipping
  // itself, green and inert, for exactly the drift it was written to catch —
  // committed by the test itself.
  //
  // Correcting the regex fixed that instance and left the mechanism in place,
  // which would do the same thing again on the next rename. So the condition is
  // now an ASSERTION: the field has landed, and a build in which it has not, or
  // in which it has been renamed or had its tag changed, is a build where the
  // frontend's DEVICE_KIND no longer matches anything Go sends. That must fail.
  //
  // The field is `Kind DeviceKind `json:"kind,omitempty"`` — a NAMED TYPE, so
  // that no bare string can be assigned to it, and omitempty because the field
  // is not persisted and an absent kind reads as native.
  const go = read(repoRoot, 'internal', 'gst', 'gst.go');
  assert.match(
    go,
    /Kind\s+DeviceKind\s+`json:"kind,omitempty"`/,
    'internal/gst.Device must carry Kind as the named type DeviceKind with the json tag ' +
      '"kind,omitempty" — the frontend reads this field by that name off every device',
  );
  // The constants carry the type in their declaration — `KindNative DeviceKind
  // = "native"` — because they are declared in a const block with no shared
  // type. Matching the whole declaration rather than just the string keeps the
  // pin on the CONTRACT (this value, on this type) instead of on any line that
  // happens to contain the word.
  assert.ok(
    go.includes(`KindNative DeviceKind = "${DEVICE_KIND.NATIVE}"`),
    'gst.go must spell KindNative',
  );
  assert.ok(
    go.includes(`KindDeckLink DeviceKind = "${DEVICE_KIND.DECKLINK}"`),
    'gst.go must spell KindDeckLink',
  );
  assert.equal(DEVICE_KIND.NATIVE, 'native');
  assert.equal(DEVICE_KIND.DECKLINK, 'decklink');
});

// --------------------------------------------------------------------------
// The namespace helpers, against internal/gst/device_id.go
// --------------------------------------------------------------------------

test('capture, render and unknown ids classify correctly, and asymmetrically', () => {
  const capture = '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}';
  const render = '{0.0.0.00000000}.{2e91b4c7-88a0-41d6-bf3c-6a0e5c7d4b02}';
  assert.equal(isCaptureEndpointId(capture), true);
  assert.equal(isRenderEndpointId(capture), false);
  assert.equal(isRenderEndpointId(render), true);
  assert.equal(isCaptureEndpointId(render), false);

  // Unknown shapes are NEITHER — never refused. A refusal keyed on "not
  // capture" would turn a future Windows id-shape change into an unstartable
  // commentary position; the rule is a POSITIVE identification both ways.
  for (const unknown of ['', 'default', 'alsa:hw:0', '{0.0.2.00000000}.{guid}', null, undefined, 42]) {
    assert.equal(isCaptureEndpointId(unknown), false, `${String(unknown)} is not capture`);
    assert.equal(isRenderEndpointId(unknown), false, `${String(unknown)} is not render either`);
  }
});

test("the operator's actual failing id classifies as render", () => {
  // Verbatim from the live failure: wasapi2src's error 1551 echoed the
  // REQUESTED id, and this is the id it echoed.
  const measured = '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}';
  assert.equal(isRenderEndpointId(measured), true);
  assert.equal(isCaptureEndpointId(measured), false);
});

test('classification is case-insensitive: GUID casing is not stable', () => {
  assert.equal(isCaptureEndpointId('{0.0.1.00000000}.{B3F8FA53-0004-438E-9003-51A46E139BFC}'), true);
  assert.equal(isRenderEndpointId('{0.0.0.00000000}.{8678CE58-90C0-4827-8FF7-C9EDD8D074ED}'), true);
});

test('the prefixes are spelled exactly as internal/gst/device_id.go spells them', () => {
  // Two copies of a namespace string that drift do not error — they just stop
  // classifying, and the paste-route check silently accepts everything again.
  const go = read(repoRoot, 'internal', 'gst', 'device_id.go');
  assert.ok(go.includes(`CaptureEndpointPrefix = "${CAPTURE_ENDPOINT_PREFIX}"`));
  assert.ok(go.includes(`RenderEndpointPrefix = "${RENDER_ENDPOINT_PREFIX}"`));
  assert.equal(CAPTURE_ENDPOINT_PREFIX, '{0.0.1.00000000}.');
  assert.equal(RENDER_ENDPOINT_PREFIX, '{0.0.0.00000000}.');
});

test('the default-device pseudo entries classify by their embedded class GUID', () => {
  // The \\?\SWD#MMDEVAPI# path form carries the DEVINTERFACE GUID mid-string,
  // which is why the helpers test containment and not just the prefix — same
  // as Go's IsCaptureEndpointID/IsRenderEndpointID.
  assert.equal(
    isRenderEndpointId('\\\\?\\SWD#MMDEVAPI#{E6327CAD-DCEC-4949-AE8A-991E976A79D2}'),
    true,
    'DEVINTERFACE_AUDIO_RENDER is positively a playback identity wherever it appears',
  );
  assert.equal(
    isCaptureEndpointId('\\\\?\\SWD#MMDEVAPI#{2eef81be-33fa-4800-9670-1cd474972c3f}'),
    true,
    'and DEVINTERFACE_AUDIO_CAPTURE the capture one, case-insensitively',
  );
});

// --------------------------------------------------------------------------
// The wiring: home.js's fillDeviceSelect uses both halves
// --------------------------------------------------------------------------

test('fillDeviceSelect sorts through sortDevices and populates from the sorted list', () => {
  // Matched loosely enough to survive labelDevices being composed around it —
  // the load-bearing facts are that the raw list goes through sortDevices and
  // that the options are built from the RESULT, not from the argument.
  const src = codeOnly(ui('home.js'));
  const fn = src.slice(src.indexOf('function fillDeviceSelect('));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.match(body, /sortDevices\(devices\)/, 'the list must go through sortDevices');
  assert.match(body, /const ordered =/, 'into a local the rest of the function reads');
  assert.match(body, /for \(const d of ordered\)/, 'and the options must be built from the SORTED list');
});

test('fillDeviceSelect shows the LABEL, so the silent twin is not offered as an equal', () => {
  // THIS TEST USED TO SKIP ITSELF, and the skip is gone rather than merely
  // unreachable. It was written while home.js still rendered d.name, and it
  // opened with `if (!body.includes('labelDevices(')) { t.skip(...); return; }`
  // so that an unlanded caller change said so out loud instead of failing.
  //
  // That is a reasonable thing to write while waiting for another work
  // package and a booby trap to leave behind once it has landed, because the
  // condition it skips on is EXACTLY THE REGRESSION IT EXISTS TO CATCH. A
  // home.js that goes back to rendering d.name would not fail this test; it
  // would switch it off, and the suite would report a pass with one more skip
  // in a line nobody reads. The two entries this guards are one Blackmagic
  // card enumerating twice under names an operator cannot tell apart, of which
  // the native twin measures -96 dBFS on all sixteen channels with the
  // microphone live — so the thing that would quietly stop being asserted is
  // the owner's original bug.
  //
  // home.js is still not this file's work package. An assertion that fails is
  // how that gets reported.
  const src = codeOnly(ui('home.js'));
  const fn = src.slice(src.indexOf('function fillDeviceSelect('));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.match(
    body,
    /labelDevices\(sortDevices\(devices\)\)/,
    'label the sorted list, in that order — labelDevices depends on which entries share a name, ' +
      'so it must run after the sort',
  );
  assert.match(
    body,
    /opt\.textContent = d\.label/,
    'and the option text must be the label, not d.name: rendering the name puts the silent ' +
      'CoreAudio twin and the DeckLink entry carrying the microphone on adjacent lines as equals',
  );
});

test('fillDeviceSelect handles the absent-id branch with a disabled marker', () => {
  const src = codeOnly(ui('home.js'));
  const fn = src.slice(src.indexOf('function fillDeviceSelect('));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.match(body, /describeDeviceSelection\(/, 'presence is decided by the pure module, not re-derived');
  assert.match(body, /missing\.disabled = true/, 'the marker must not be choosable');
  assert.match(
    body,
    /select\.insertBefore\(missing, select\.firstChild\)/,
    'and must be PREPENDED, where the selected entry of a closed dropdown is read',
  );
  assert.match(
    body,
    /select\.value = selection\.savedId/,
    'and selected, so the control is visibly wrong rather than showing device #1',
  );
});

test('the empty-list behaviour is unchanged, and runs before any sorting', () => {
  // Empty list: one option carrying the emptyLabel ("No input devices
  // found"), control disabled, early return. The marker logic must never
  // reach it — an empty list already tells the truth.
  const src = codeOnly(ui('home.js'));
  const fn = src.slice(src.indexOf('function fillDeviceSelect('));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  const emptyBranch = body.indexOf('devices.length === 0');
  const sortCall = body.indexOf('sortDevices(');
  assert.ok(emptyBranch > -1, 'the empty branch must still exist');
  assert.match(body, /opt\.textContent = emptyLabel/, 'and still show the emptyLabel option');
  assert.match(body, /select\.disabled = true/, 'and still disable the control');
  assert.ok(sortCall > emptyBranch, 'sorting happens only after the empty branch has returned');
});

// --------------------------------------------------------------------------
// The paste route: validateConfig refuses a positively wrong namespace
// --------------------------------------------------------------------------
// The exhaustive field cases live in settings.test.js beside the other
// validateConfig suites; this is the one cross-module statement — that the
// validator's answers ARE the helpers' answers.

test('validateConfig keys its device-id refusals on the namespace helpers', () => {
  const src = codeOnly(ui('validate.js'));
  assert.match(src, /isRenderEndpointId\(config\.audioDeviceId\)/, 'the input field refuses render ids');
  assert.match(
    src,
    /isCaptureEndpointId\(config\.headphoneEndpointId\)/,
    'the SRT-return headphone field refuses capture ids',
  );
  const render = '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}';
  const errors = validateConfig({ audioDeviceId: render, headphoneEndpointId: render });
  assert.ok(errors.audioDeviceId, 'a render id in audioDeviceId is refused');
  assert.equal(errors.headphoneEndpointId, undefined, 'and accepted where playback is what is wanted');
});
