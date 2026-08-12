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
  const src = codeOnly(ui('home.js'));
  const fn = src.slice(src.indexOf('function fillDeviceSelect('));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.match(body, /const ordered = sortDevices\(devices\)/, 'the list must go through sortDevices');
  assert.match(body, /for \(const d of ordered\)/, 'and the options must be built from the SORTED list');
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
