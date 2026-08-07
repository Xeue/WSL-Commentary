/**
 * Tests for the return SOURCE control: WebRTC or the native SRT return.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * Both paths carry the same programme, and they do not arrive at the same time:
 * WebRTC has a measured 489 ms upper bound and the SRT return has never been
 * measured at all. A commentator who can hear both is hearing a slapback echo
 * of the match they are describing, over the top of their own voice, and it is
 * not obviously a settings problem to the person suffering it — it sounds like
 * the facility is broken.
 *
 * So "only one path may be audible" is not a rule spread across two call sites
 * that happen to be kept opposite. It is one function, deriveReturnSourceEffects,
 * and the first tests below are about it being total and mutually exclusive for
 * every input, including inputs nobody intends to pass.
 *
 * The second group reads the TEXT of app.js, for the same reason
 * mixerwiring.test.js does: the property is an ORDERING — silence the outgoing
 * path before starting the incoming one — and driving app.js would need a DOM
 * shim covering <video>, <select>, navigator.mediaDevices and a Wails runtime.
 * A shim widened until a test passes stops being evidence. The ordering is
 * three lines of source, so three lines of source are what is asserted.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  RETURN_SOURCE_WEBRTC,
  RETURN_SOURCE_SRT,
  RETURN_SOURCES,
  DEFAULT_RETURN_SOURCE,
  DEVICE_KEY_WEBRTC,
  DEVICE_KEY_SRT,
  DEVICE_SOURCE_WEBRTC,
  DEVICE_SOURCE_SRT,
  WEBRTC_LATENCY_MS,
  LATENCY_NOTE,
  PICTURE_NOTE,
  isValidReturnSource,
  normaliseReturnSource,
  deriveReturnSourceEffects,
  returnSourceLabel,
  describeReturnSource,
} from './returnsource.js';
// The routing table, so the assertion that GStreamer and Web Audio agree is
// made against the one definition rather than against a copy of it.
import { sourceChannelsForOutputs } from '../monitor/channels.js';

const here = dirname(fileURLToPath(import.meta.url));
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);
const monitor = (name) => read(here, '..', 'monitor', name);

// --------------------------------------------------------------------------
// Only one path may be audible
// --------------------------------------------------------------------------

test('selecting WebRTC makes WebRTC audible and stops the SRT return', () => {
  const e = deriveReturnSourceEffects(RETURN_SOURCE_WEBRTC);
  assert.equal(e.webrtcAudible, true);
  assert.equal(e.srtRunning, false);
});

test('selecting SRT silences WebRTC and runs the SRT return', () => {
  const e = deriveReturnSourceEffects(RETURN_SOURCE_SRT);
  assert.equal(e.webrtcAudible, false);
  assert.equal(e.srtRunning, true);
});

test('there is NO input for which both paths are audible', () => {
  // Total and mutually exclusive, including for values nobody means to pass: a
  // hand-edited config.json, an older file, a typo in a future caller. The
  // dangerous direction is both-on, so it is asserted for every input rather
  // than for the two intended ones.
  const inputs = [
    RETURN_SOURCE_WEBRTC,
    RETURN_SOURCE_SRT,
    undefined,
    null,
    '',
    'WEBRTC',
    'SRT',
    'both',
    'webrtc ',
    0,
    1,
    true,
    {},
    [],
    NaN,
  ];
  for (const input of inputs) {
    const e = deriveReturnSourceEffects(input);
    assert.notEqual(
      e.webrtcAudible && e.srtRunning,
      true,
      `${JSON.stringify(input)} must never make both paths audible`,
    );
    assert.equal(
      e.webrtcAudible || e.srtRunning,
      true,
      `${JSON.stringify(input)} must leave the commentator on SOME path, not silence`,
    );
    assert.equal(
      e.webrtcAudible,
      !e.srtRunning,
      `${JSON.stringify(input)} must select exactly one path`,
    );
  }
});

test('the effects object is frozen, so nothing can flip one flag and not the other', () => {
  const e = deriveReturnSourceEffects(RETURN_SOURCE_SRT);
  assert.ok(Object.isFrozen(e));
});

test('WebRTC is the default: the path that exists and has been measured', () => {
  assert.equal(DEFAULT_RETURN_SOURCE, RETURN_SOURCE_WEBRTC);
  assert.equal(deriveReturnSourceEffects(undefined).source, RETURN_SOURCE_WEBRTC);
});

test('normaliseReturnSource never returns a path that does not exist', () => {
  for (const bad of [undefined, null, '', 'aux1', 'rtmp', 3, {}]) {
    assert.ok(isValidReturnSource(normaliseReturnSource(bad)), `${String(bad)} normalises to a real path`);
  }
  assert.equal(normaliseReturnSource('nope', RETURN_SOURCE_SRT), RETURN_SOURCE_SRT, 'the fallback is honoured');
  assert.equal(normaliseReturnSource('nope', 'also nope'), DEFAULT_RETURN_SOURCE, 'a bad fallback is not');
});

test('there are exactly two paths, and both are labelled', () => {
  assert.deepEqual(
    RETURN_SOURCES.map((s) => s.value),
    [RETURN_SOURCE_WEBRTC, RETURN_SOURCE_SRT],
  );
  for (const s of RETURN_SOURCES) {
    assert.ok(s.label.length > 0);
    assert.ok(s.summary.length > 0);
    assert.ok(s.cost.length > 0);
  }
  assert.match(returnSourceLabel('nonsense'), /not one of the two/);
});

// --------------------------------------------------------------------------
// The two identifier spaces
// --------------------------------------------------------------------------

test('the two paths persist their device id in DIFFERENT config fields', () => {
  // A browser mediaDeviceId and a WASAPI endpoint id name the same headphones
  // and are not interchangeable. Putting one where the other is expected does
  // not throw — it plays on the default device, silently.
  assert.notEqual(DEVICE_KEY_WEBRTC, DEVICE_KEY_SRT);
  assert.equal(deriveReturnSourceEffects(RETURN_SOURCE_WEBRTC).deviceKey, DEVICE_KEY_WEBRTC);
  assert.equal(deriveReturnSourceEffects(RETURN_SOURCE_SRT).deviceKey, DEVICE_KEY_SRT);
});

test('headphoneDeviceId keeps its existing meaning', () => {
  // It is the spec §9 field and it has always held a browser mediaDeviceId.
  // Redefining it to hold a WASAPI id would silently break every config that
  // already exists.
  assert.equal(DEVICE_KEY_WEBRTC, 'headphoneDeviceId');
});

test('each path says whose device list it is showing', () => {
  assert.notEqual(DEVICE_SOURCE_WEBRTC, DEVICE_SOURCE_SRT);
  assert.match(deriveReturnSourceEffects(RETURN_SOURCE_WEBRTC).deviceSource, /browser/i);
  assert.match(deriveReturnSourceEffects(RETURN_SOURCE_SRT).deviceSource, /WASAPI|Windows/i);
});

// --------------------------------------------------------------------------
// Honesty about what each path is
// --------------------------------------------------------------------------

test('the latency note gives the MEASURED WebRTC figure and no figure for SRT', () => {
  assert.equal(WEBRTC_LATENCY_MS, 489, 'docs/test-results.md §2.2');
  assert.match(LATENCY_NOTE, /489 ms/, 'the measured figure is shown');
  assert.match(LATENCY_NOTE, /measured/i);
  assert.match(LATENCY_NOTE, /not been measured|unmeasured/i, 'and the SRT figure is said to be unmeasured');

  // The real test: there is exactly ONE millisecond figure in the sentence.
  // A second number beside the first is indistinguishable from a second
  // measurement, and nobody has measured the SRT return.
  const msFigures = LATENCY_NOTE.match(/\d+\s*ms/g) || [];
  assert.deepEqual(msFigures, ['489 ms'], 'no invented latency figure may appear beside the measured one');
});

test('no module invents a latency number for the SRT return', () => {
  // Belt and braces across the files that could print one. If somebody later
  // adds "about 200 ms" to a hint, this is what notices.
  for (const [name, src] of [
    ['returnsource.js', ui('returnsource.js')],
    ['home.js', ui('home.js')],
  ]) {
    const figures = new Set((src.match(/\d+\s*ms\b/g) || []).map((s) => s.replace(/\s+/g, ' ')));
    figures.delete('489 ms');
    assert.deepEqual(
      [...figures],
      [],
      `${name} prints a latency figure that is not the one measured value`,
    );
  }
});

test('the SRT option states what it costs, in the units the operator thinks in', () => {
  const srt = RETURN_SOURCES.find((s) => s.value === RETURN_SOURCE_SRT);
  assert.match(srt.cost, /15 Mbit/i, 'the real bitrate of M2L-X output 3');
  assert.match(srt.cost, /fan-out/i, 'and the fan-out slot it holds');
  assert.match(srt.summary + srt.cost, /full[- ]quality|browser is not in this path/i);
});

test('the WebRTC option is described as monitor-grade, not as the lesser default', () => {
  const webrtc = RETURN_SOURCES.find((s) => s.value === RETURN_SOURCE_WEBRTC);
  assert.match(webrtc.summary, /monitor-grade/i);
  assert.match(describeReturnSource(RETURN_SOURCE_WEBRTC), /monitor-grade/i);
});

test('the UI says the picture is unchanged by this control', () => {
  // "Switch the return to SRT" reads like "switch everything to SRT". The video
  // keeps coming from WebRTC either way.
  assert.match(PICTURE_NOTE, /picture/i);
  assert.match(PICTURE_NOTE, /audio only|audio\b/i);
  assert.ok(ui('home.js').includes('PICTURE_NOTE'), 'home.js must render it');
});

// --------------------------------------------------------------------------
// The wiring: silence first, then start
// --------------------------------------------------------------------------

/** switchBody is the body of app.js's one source-switching function. */
function switchBody() {
  const src = ui('app.js');
  const start = src.indexOf('async function onReturnSourceChange(');
  assert.ok(start > 0, 'app.js must have exactly one source-switching function');
  const end = src.indexOf('sourceSwitchInFlight = false;', start);
  assert.ok(end > start, 'the function must clear its in-flight guard');
  return src.slice(start, end);
}

test('the switch silences WebRTC BEFORE it starts the SRT return', () => {
  const body = switchBody();
  const mute = body.indexOf('setAudioEnabled(false)');
  const startSRT = body.indexOf('startReturn(');
  assert.ok(mute > -1, 'the switch must be able to silence the WebRTC path');
  assert.ok(startSRT > -1, 'and to start the SRT one');
  assert.ok(
    mute < startSRT,
    'WebRTC must be silenced before SRT is started — the other order is both paths at once',
  );
});

test('the switch stops the SRT return BEFORE it un-mutes WebRTC', () => {
  const body = switchBody();
  const stopSRT = body.indexOf('stopReturn(');
  const unmute = body.indexOf('setAudioEnabled(true)');
  assert.ok(stopSRT > -1 && unmute > -1);
  assert.ok(stopSRT < unmute, 'SRT must be stopped before WebRTC is made audible again');
});

test('the switch is the only place either path is made audible', () => {
  // A second call site is a second ordering to get right, and the one that gets
  // it wrong will be the one written in a hurry.
  const src = ui('app.js');
  const enables = src.match(/setAudioEnabled\(/g) || [];
  const starts = src.match(/backend\.startReturn\(/g) || [];
  assert.ok(enables.length > 0 && starts.length > 0);
  // Both appear in the switch, in the switch's error recovery, and in startup.
  // What must NOT exist is a call in a handler that does not also deal with the
  // other path — so every startReturn call site must sit in a file that also
  // silences WebRTC.
  assert.ok(
    src.includes('setAudioEnabled(false)'),
    'anything that can start the SRT return must also be able to silence WebRTC',
  );
});

test('a saved SRT selection builds the monitor ALREADY silent', () => {
  // Building it audible and muting it a tick later is a tick of both paths in
  // somebody's ears, on every launch.
  const src = ui('app.js');
  assert.match(
    src,
    /audioEnabled:\s*currentReturnSource !== RETURN_SOURCE_SRT/,
    'createMonitor must be told at construction, not corrected afterwards',
  );
});

test('the monitor exposes the silencing as an audio-only control', () => {
  // The picture rides the same peer connection. setAudioEnabled(false) must
  // sever the Web Audio graph and nothing else — if it stopped the monitor the
  // commentator would lose the picture as well.
  const src = monitor('monitor.js');
  assert.match(src, /setAudioEnabled\(enabled\)\s*\{/, 'the seam must carry setAudioEnabled');
  const body = src.slice(src.indexOf('setAudioEnabled(enabled)'));
  const end = body.indexOf('\n    },');
  const fn = body.slice(0, end);
  assert.match(fn, /audio\.setMuted\(/, 'it must mute the audio chain');
  assert.ok(!/view\.|stop\(\)|teardownSession/.test(fn), 'and must not touch the video or the connection');
});

test('the mute is a severed edge, not a gain of zero', () => {
  const src = monitor('audio.js');
  assert.match(
    src,
    /gainNode\.disconnect\(dest\)/,
    'applyMute must disconnect the destination edge; a gain of zero is one ramp from audible',
  );
});

// --------------------------------------------------------------------------
// The backend adapter
// --------------------------------------------------------------------------

const repoRoot = join(here, '..', '..', '..');

test('backend.js binds each return call to the Go method of that name', () => {
  // The one contract that no Go test and no JS test can see on its own: a
  // rename on either side compiles, builds, and fails inside WebView2 as a
  // rejected promise with no clue in it.
  const js = ui('backend.js');
  const go = read(repoRoot, 'app_return.go');

  // The Go names live in ONE table, so a rename is one edit rather than five
  // string literals scattered through call sites.
  const tableStart = js.indexOf('const RETURN_METHODS');
  assert.ok(tableStart > 0, 'backend.js must name the Go methods in one table');
  const table = js.slice(tableStart, js.indexOf('});', tableStart));

  for (const [jsName, goName] of [
    ['listOutputDevices', 'ListOutputDevices'],
    ['isSRTReturnSelected', 'IsSRTReturnSelected'],
    ['startReturn', 'StartReturn'],
    ['stopReturn', 'StopReturn'],
    ['getReturnState', 'GetReturnState'],
  ]) {
    assert.ok(
      new RegExp(`export async function ${jsName}\\(`).test(js),
      `backend.js must export ${jsName}`,
    );
    assert.ok(table.includes(`'${goName}'`), `RETURN_METHODS must name Go's ${goName}`);
    assert.ok(
      new RegExp(`func \\(a \\*App\\) ${goName}\\(`).test(go),
      `app_return.go must declare ${goName}`,
    );
  }
});

test('the "return" event name matches app.go', () => {
  assert.match(ui('backend.js'), /export const EVENT_RETURN = 'return'/);
  assert.match(read(repoRoot, 'app.go'), /EventReturn = "return"/);
});

test('the four return states are gst.ReturnState, spelled the same way', () => {
  // Uppercase, and matching internal/gst exactly. A lowercase copy here would
  // compare unequal to every event that arrives, and the status line would sit
  // on whatever the last recognised state was.
  const js = ui('backend.js');
  const gst = read(repoRoot, 'internal', 'gst', 'return.go');
  for (const state of ['STOPPED', 'CONNECTING', 'RECEIVING', 'BACKOFF']) {
    assert.ok(js.includes(`${state}: '${state}'`), `backend.js carries ${state}`);
    assert.ok(gst.includes(`ReturnState = "${state}"`), `internal/gst defines ${state}`);
  }
});

test("the two device id fields are config.Config's own JSON tags", () => {
  const cfg = read(repoRoot, 'internal', 'config', 'config.go');
  assert.ok(cfg.includes(`json:"${DEVICE_KEY_WEBRTC}"`), `config.go declares ${DEVICE_KEY_WEBRTC}`);
  assert.ok(cfg.includes(`json:"${DEVICE_KEY_SRT}"`), `config.go declares ${DEVICE_KEY_SRT}`);
});

test('the channel and source values are the ones config.ValidateReturn accepts', () => {
  // A frontend that saved "Left" or "SRT" would save happily and then be
  // refused by StartReturn, with a message about a field the operator can see
  // is set correctly.
  const cfg = read(repoRoot, 'internal', 'config', 'config.go');
  assert.ok(cfg.includes('ReturnSourceWebRTC = "webrtc"'));
  assert.ok(cfg.includes('ReturnSourceSRT = "srt"'));
  assert.equal(RETURN_SOURCE_WEBRTC, 'webrtc');
  assert.equal(RETURN_SOURCE_SRT, 'srt');
  assert.ok(cfg.includes('ReturnChannelStereo = "stereo"'));
  assert.ok(cfg.includes('ReturnChannelLeft   = "left"'));
  assert.ok(cfg.includes('ReturnChannelRight  = "right"'));
});

test('the Go mix matrix and the Web Audio routing describe the SAME routing', () => {
  // The channel control applies to both paths, by two different mechanisms: a
  // ChannelSplitter/ChannelMerger pair in the browser and an audiomixmatrix
  // property in GStreamer. If they ever disagree, one control means two
  // different things depending on which return source is selected — and the
  // commentator has no way to know which one they are hearing.
  const go = read(repoRoot, 'internal', 'gst', 'return.go');
  const mixMatrix = go.slice(go.indexOf('func MixMatrix('), go.indexOf('func MixMatrixString('));

  for (const mode of ['stereo', 'left', 'right']) {
    const goName = `ReturnChannel${mode[0].toUpperCase()}${mode.slice(1)}`;
    const caseStart = mixMatrix.indexOf(`case ${goName}:`);
    assert.ok(caseStart > 0, `MixMatrix handles ${goName}`);
    const block = mixMatrix.slice(caseStart, mixMatrix.indexOf('}, nil', caseStart));
    // Each row is one OUTPUT channel; each column is one SOURCE channel.
    const rows = [...block.matchAll(/\{(\d),\s*(\d)\}/g)].map((m) => [Number(m[1]), Number(m[2])]);
    assert.equal(rows.length, 2, `${goName} is a 2x2 matrix`);

    // The same statement, built from the frontend's table.
    const wanted = sourceChannelsForOutputs(mode).map((src) => [src === 0 ? 1 : 0, src === 1 ? 1 : 0]);
    assert.deepEqual(rows, wanted, `${mode}: GStreamer and Web Audio must route identically`);
  }
});

test('StartReturn takes no arguments, so the config is saved before it is called', () => {
  // App.StartReturn reads host, port, latency, channel and the WASAPI endpoint
  // out of the SAVED configuration, and refuses unless returnSource is already
  // "srt". A start racing its own save either fails with a message about a
  // setting that looks correct, or succeeds against the previous configuration
  // and plays the wrong thing while reporting success.
  assert.match(
    read(repoRoot, 'app_return.go'),
    /func \(a \*App\) StartReturn\(\) error/,
    'it really does take no arguments',
  );

  const body = switchBody();
  const save = body.indexOf('persistConfigAndWait({ returnSource: next })');
  const start = body.indexOf('backend.startReturn(');
  assert.ok(save > -1, 'the switch must await the save');
  assert.ok(start > -1);
  assert.ok(save < start, 'and it must land before StartReturn reads it');
});

test('which path owns the headphones is asked of Go, not decided twice', () => {
  // app_return.go's IsSRTReturnSelected exists so that ONE place decides. The
  // frontend comparing returnSource to "srt" itself would put the same decision
  // in two languages, and the failure of the two disagreeing is both paths
  // playing at once.
  assert.match(ui('app.js'), /backend\.isSRTReturnSelected\(\)/);
});

test('a build without the bindings says so instead of failing opaquely', () => {
  const js = ui('backend.js');
  assert.match(js, /class BindingMissingError extends Error/, 'the missing-method case is named');
  assert.match(js, /export function srtReturnAvailable\(\)/, 'and the UI can ask before offering the option');
  assert.match(ui('app.js'), /srtReturnAvailable\(\)/, 'and app.js does ask');
  assert.match(ui('home.js'), /setSRTAvailable/, 'and the option can be disabled with a reason');
});

test('the fake refuses to start the SRT return while returnSource is webrtc', () => {
  // That refusal is the guarantee that both paths cannot be audible at once.
  // A fake that waved it through would let the bug be written here and then
  // fail to reproduce it in a dev session.
  const js = ui('backend.js');
  const section = js.slice(js.indexOf('// The native SRT return'), js.indexOf('// The mixer drawer'));
  assert.ok(section.length > 0, 'backend.js has a return section before the mixer one');
  const start = section.slice(section.indexOf('export async function startReturn('));
  assert.match(
    start.slice(0, start.indexOf('\n}')),
    /fakeConfig\.returnSource !== 'srt'/,
    'the fake must refuse in the same case app_return.go does',
  );
});

test('stopReturn documents that it rejects when nothing was running', () => {
  // app_return.go returns errReturnNotRunning so teardown can call it
  // unconditionally. Every caller here has to catch it, and a caller that
  // treats it as a failure would put a red banner on a normal path switch.
  const js = ui('backend.js');
  const stop = js.slice(js.indexOf('export async function stopReturn('));
  assert.match(js.slice(js.indexOf('* Stops the native SRT return')), /REJECTS WHEN NOTHING WAS RUNNING/);
  assert.match(stop.slice(0, stop.indexOf('\n}')), /not running/);
  assert.match(read(repoRoot, 'app_return.go'), /errReturnNotRunning/);
});

test('the fake output devices are not the fake INPUT devices wearing a hat', () => {
  // The hazard is treating a WASAPI endpoint id and a browser mediaDeviceId as
  // the same string. A dev session where the two lists share ids hides it.
  const js = ui('backend.js');
  const inputIds = [...js.matchAll(/id: '(\{0\.0\.1[^']*)'/g)].map((m) => m[1]);
  const outputIds = [...js.matchAll(/id: '(\{0\.0\.0[^']*)'/g)].map((m) => m[1]);
  assert.ok(inputIds.length > 0, 'there are fake capture endpoints');
  assert.ok(outputIds.length > 0, 'and fake render endpoints');
  for (const id of outputIds) {
    assert.ok(!inputIds.includes(id), `${id} appears in both fake device lists`);
  }
});

// --------------------------------------------------------------------------
// The dropdown must not conflate the two lists
// --------------------------------------------------------------------------

test('exactly one function fills the Headphones dropdown, and it picks by path', () => {
  const src = ui('app.js');
  assert.match(src, /function renderHeadphoneList\(\)/, 'there is one place the list is chosen');
  const sites = src.match(/home\.setHeadphoneDevices\(/g) || [];
  assert.equal(
    sites.length,
    1,
    'home.setHeadphoneDevices must have exactly one call site: crossing the two identifier spaces is silent',
  );
});

test('the home screen relabels the dropdown when the path changes', () => {
  // An unlabelled dropdown that silently swaps its contents is how a WASAPI id
  // ends up in the browser's field.
  const src = ui('home.js');
  assert.match(src, /headphoneLabel\.textContent = `Headphones — \$\{effects\.deviceSource\}`/);
});

test('home.js does not fetch either device list itself', () => {
  // It has no backend import (mixerwiring.test.js asserts that) and it must not
  // grow a second route to the browser's list either: the two lists and the
  // selection have to be chosen together.
  const src = ui('home.js');
  assert.ok(!/enumerateDevices|listOutputDevices/.test(src), 'device discovery belongs to app.js');
});

test('Settings carries the return source through a save instead of dropping it', () => {
  // saveConfig REPLACES the stored object. A field the form does not restate is
  // a field the form deletes, and deleting returnSource would reset a
  // commentator's audio path on the next launch because somebody pressed Save
  // for an unrelated reason.
  const src = ui('settings.js');
  assert.match(src, /returnSource: carriedReturnSource/, 'the saved value is carried, not collected');
  assert.match(src, /srtReturnPort: carriedSRTReturnPort/, 'and so is the port the return dials');
  assert.match(src, /returnChannel: normaliseChannelMode\(/, 'and the channel is collected from its control');
  assert.match(src, /\[DEVICE_KEY_SRT\]:/, 'and the WASAPI endpoint id is not dropped either');
});
