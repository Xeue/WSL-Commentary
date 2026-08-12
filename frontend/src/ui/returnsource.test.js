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

test('returnsource.js no longer claims the picture comes from WebRTC', () => {
  // It used to export PICTURE_NOTE — "The picture comes from WebRTC either way;
  // this control changes audio only." — and home.js rendered it. Both halves of
  // that sentence are now false: the picture comes from SRT and this module
  // drives no control at all.
  //
  // The constant is DELETED rather than corrected. A stale export with the right
  // name is how the wrong sentence gets rendered again by somebody grepping for
  // where the note lives; picturesource.js owns the replacement.
  const src = ui('returnsource.js');
  assert.ok(
    !/^export const PICTURE_NOTE/m.test(src),
    'returnsource.js must not export PICTURE_NOTE any more',
  );
  assert.ok(
    !/^export const CHANNEL_REBUILD_NOTE/m.test(src),
    'nor a warning about a control the GUI no longer has',
  );
  // home.js no longer renders the note AT ALL — the paragraph under the
  // controls went at the operator's request — so the guard is now pure
  // absence: no import of the sentence from either module, so the stale copy
  // in returnsource.js (if it ever came back) would have no reader. The
  // replacement sentence still has exactly one owner, picturesource.js, where
  // the segmented control's tooltips and Settings take their words from.
  const home = ui('home.js');
  const importFrom = (module) => {
    const at = home.indexOf(`from './${module}.js'`);
    if (at < 0) return '';
    const open = home.lastIndexOf('import', at);
    return home.slice(open, at);
  };
  assert.ok(
    !importFrom('returnsource').includes('PICTURE_NOTE'),
    'home.js must not import PICTURE_NOTE from returnsource.js',
  );
  assert.ok(
    !importFrom('picturesource').includes('PICTURE_NOTE'),
    'home.js no longer renders the note; the paragraph was removed by the operator',
  );
  assert.match(
    ui('picturesource.js'),
    /^export const PICTURE_NOTE/m,
    'picturesource.js still owns the sentence, for the tooltips and Settings',
  );
});

// --------------------------------------------------------------------------
// The wiring: silence first, then start
// --------------------------------------------------------------------------

// The ordering used to be asserted here, as three lines of app.js read as text,
// because driving app.js would need a DOM shim and a Wails runtime. It is not
// asserted that way any more, and the reason is not that the shim got easier:
// the ordering MOVED. It lives in returnpath.js, which takes its effects as
// arguments and can therefore be driven for real — see returnpath.test.js, where
// "exactly one path is audible" is checked over every entry point crossed with
// every combination of which call fails and how.
//
// What is left here is the property that makes that worth anything: THE ORDERING
// EXISTS IN EXACTLY ONE PLACE. A second stop/start/mute sequence written into a
// handler would be a second ordering to get right, and the one that gets it
// wrong will be the one written in a hurry twenty minutes before kick-off.

/**
 * codeOnly strips comments, so that a note DISCUSSING a call cannot satisfy — or
 * break — a guard that counts call sites. Same reasoning as
 * internal/gst/gst_stub_test.go's parseSource, which parses without comments.
 */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

test('nothing outside the machine can make a return path AUDIBLE', () => {
  // Stopping and muting are safe from anywhere: they can only ever take audio
  // away. STARTING and UN-MUTING are the dangerous direction, because getting
  // either one out of order with the other is both paths in the commentator's
  // ears — so both live behind the one seam, and this is what says so.
  const src = codeOnly(ui('app.js'));
  const seamStart = src.indexOf('const returnPath = createReturnPath({');
  const seamEnd = src.indexOf('\n  });', seamStart);
  assert.ok(seamStart > 0 && seamEnd > seamStart, 'app.js must build the return path machine');

  for (const [call, why] of [
    ['backend.startReturn(', 'starting the SRT return'],
    ['setAudioEnabled(', 'silencing or un-silencing the WebRTC return'],
  ]) {
    let at = src.indexOf(call);
    assert.ok(at > -1, `app.js must reach ${call} somewhere`);
    while (at > -1) {
      assert.ok(
        at > seamStart && at < seamEnd,
        `app.js calls ${call} outside the returnpath.js seam: ${why} is the machine's job, ` +
          'and a second call site is a second ordering to get right',
      );
      at = src.indexOf(call, at + 1);
    }
  }
});

test('every transition in app.js goes through the one machine', () => {
  const src = codeOnly(ui('app.js'));
  // `select` is deliberately NOT in this list any more. Nothing in the GUI
  // switches the audio path: audio always comes from Kinesis, so there is no
  // caller left. It stays in returnpath.js, tested there, because the machinery
  // is kept as a capability — but a call site here would mean a control
  // somewhere that can silence the commentator, and there is not one.
  for (const entry of ['applyOption', 'adoptSaved']) {
    assert.match(
      src,
      new RegExp(`returnPath\\s*\\.\\s*${entry}\\(`),
      `app.js must reach the machine through returnPath.${entry}()`,
    );
  }
  assert.ok(
    !/returnPath\s*\.\s*select\(/.test(src),
    'nothing in the UI may switch the audio path any more — SRT is the picture, ' +
      'Kinesis is the audio, and a select() call site is the bug this work removed',
  );
  assert.ok(
    !src.includes('sourceSwitchInFlight'),
    'the in-flight guard belongs to the machine, which is the only thing that can hold it ' +
      'across a channel change AND a headphone change',
  );
});

test('startup drives the audio to Kinesis and corrects a saved SRT audio path', () => {
  // THE FAILURE THIS EXISTS FOR: a config.json written by the previous revision
  // holds returnSource: "srt". Read and honoured, it silences the commentator on
  // every launch, and there is no longer a control on any screen that would undo
  // it. Overruling it is the only route out.
  const src = codeOnly(ui('app.js'));
  assert.match(
    src,
    /returnPath\.adoptSaved\(RETURN_SOURCE_WEBRTC\)/,
    'startup must adopt WebRTC explicitly, not whatever is on disk',
  );
  assert.match(
    src,
    /persistConfig\(\{ returnSource: RETURN_SOURCE_WEBRTC \}\)/,
    'and write the correction back, so the next launch does not repeat it',
  );
  assert.ok(
    !/normaliseReturnSource\(currentConfig\.returnSource\)/.test(src),
    'the saved audio path must not be read back into force',
  );
});

test('the machine silences the outgoing path before starting the incoming one', () => {
  // Asserted against returnpath.js's source as well as its behaviour, because
  // the structural statement is the one that survives a refactor: the ONLY route
  // to setWebRTCAudible(true) is goWebRTC, and goWebRTC awaits stopReturn first.
  const src = ui('returnpath.js');

  const unmutes = [...codeOnly(src).matchAll(/io\.setWebRTCAudible\(true\)/g)];
  assert.equal(
    unmutes.length,
    1,
    'there must be exactly ONE place WebRTC is made audible; each extra one is an ' +
      'ordering that has to be got right again',
  );

  const fn = src.slice(src.indexOf('async function goWebRTC()'));
  const body = fn.slice(0, fn.indexOf('\n  }'));
  assert.ok(
    body.includes('io.setWebRTCAudible(true)'),
    'the single un-mute must be inside goWebRTC',
  );
  assert.ok(
    body.indexOf('await stopSRT()') < body.indexOf('io.setWebRTCAudible(true)'),
    'goWebRTC must stop the SRT return BEFORE it un-mutes WebRTC',
  );

  const startFn = src.slice(src.indexOf('async function startSRT()'));
  const startBody = startFn.slice(0, startFn.indexOf('\n  }'));
  assert.ok(
    startBody.indexOf('muteWebRTC()') < startBody.indexOf('io.startReturn()'),
    'startSRT must silence WebRTC BEFORE it starts the SRT return',
  );
});

test('errReturnAlreadyRunning is stop-then-start, not a failed start', () => {
  // It says a monitor IS running. Treated as a failure the caller falls back to
  // WebRTC and un-mutes it over the top of an orphaned GStreamer pipeline that
  // is still writing CLN to the same headphones.
  const src = ui('returnpath.js');
  const startFn = src.slice(src.indexOf('async function startSRT()'));
  const body = startFn.slice(0, startFn.indexOf('\n  }'));
  assert.match(body, /isAlreadyRunning\(err\)/, 'startSRT must recognise the refusal');
  assert.ok(
    body.indexOf('await stopSRT()') > body.indexOf('isAlreadyRunning(err)'),
    'and answer it by stopping what is running before starting again',
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

  // The ordering is in returnpath.js's select(), on the branch that adopts SRT.
  const src = ui('returnpath.js');
  const fn = src.slice(src.indexOf('async function select(next)'));
  const body = fn.slice(0, fn.indexOf('\n  async function restore'));
  const save = body.indexOf('await io.saveSource(wanted)');
  const start = body.indexOf('await startSRT()');
  assert.ok(save > -1, 'the switch must await the save');
  assert.ok(start > -1, 'and start the return');
  assert.ok(save < start, 'and the save must land before StartReturn reads it');

  // The same ordering on the rebuild path, where it is the difference between a
  // channel change taking effect and the pipeline coming back up on the old one
  // while reporting success.
  const rebuild = src.slice(src.indexOf('async function rebuildSRT('));
  const rebuildBody = rebuild.slice(0, rebuild.indexOf('\n  }'));
  assert.ok(
    rebuildBody.indexOf('await save()') < rebuildBody.indexOf('await startSRT()'),
    'a rebuild must save before it starts, or the return comes back on the previous settings',
  );
});

test('app_return.go still answers which path owns the headphones', () => {
  // IsSRTReturnSelected exists so that ONE place decides. The frontend no longer
  // asks — it does not switch audio paths at all any more, so there is nothing
  // to disagree about — but the binding stays wired in backend.js, because the
  // Go-side capability stays and srtReturnAvailable() counts it.
  assert.match(ui('backend.js'), /export async function isSRTReturnSelected\(/);
  assert.match(read(repoRoot, 'app_return.go'), /func \(a \*App\) IsSRTReturnSelected\(/);
});

test('a build without the bindings says so instead of failing opaquely', () => {
  const js = ui('backend.js');
  assert.match(js, /class BindingMissingError extends Error/, 'the missing-method case is named');
  assert.match(js, /export function srtReturnAvailable\(\)/, 'the audio capability can be asked about');
  // The question the UI actually asks now is about the PICTURE, because that is
  // the only one of the two it offers.
  assert.match(js, /export function pictureAvailable\(\)/, 'and so can the picture one');
  assert.match(ui('app.js'), /backend\.pictureAvailable\(\)/, 'and app.js does ask');
  assert.match(ui('home.js'), /setPictureAvailable/, 'and the option can be disabled with a reason');
});

test('srtReturnAvailable checks EVERY binding the SRT path calls, not just two', () => {
  // Availability decides whether the option is offered at all, and every one of
  // the five is called on a path that has already assumed it:
  //
  //   GetReturnState       straight after startReturn(), where a throw used to
  //                        land in the recovery WITH A MONITOR RUNNING
  //   IsSRTReturnSelected  when switching back to WebRTC
  //   ListOutputDevices    to fill the Headphones dropdown for the SRT path
  //
  // A build with StartReturn and StopReturn but not GetReturnState would offer
  // the option, start the return, and then fail on the next line — which is how
  // a missing binding turns into a wrong recovery. It is also what makes it safe
  // for returnpath.js to treat a BindingMissingError from StopReturn as proof
  // that nothing is running: no StopReturn means no StartReturn means nothing
  // was ever started.
  const js = ui('backend.js');

  const tableStart = js.indexOf('const RETURN_METHODS');
  const table = js.slice(tableStart, js.indexOf('});', tableStart));
  const named = [...table.matchAll(/'([A-Z][A-Za-z]+)'/g)].map((m) => m[1]);
  assert.equal(named.length, 5, `RETURN_METHODS should name five Go methods, found ${named}`);

  const fn = js.slice(js.indexOf('export function srtReturnAvailable()'));
  const body = fn.slice(0, fn.indexOf('\n}'));

  // It must consult the whole table rather than a hand-picked subset. Either it
  // iterates every value, or it names all five — anything else is a subset that
  // will be wrong the next time a binding is added.
  const iterates = /RETURN_METHOD_NAMES\.every\(|Object\.values\(RETURN_METHODS\)\.every\(/.test(
    body,
  );
  if (!iterates) {
    for (const method of named) {
      assert.ok(
        body.includes(method),
        `srtReturnAvailable does not check ${method}. It is called on a path that assumes ` +
          'the option was offered, so a build missing it fails after the return has started.',
      );
    }
  }

  // And the list it iterates has to be the whole table, not a copy.
  if (iterates) {
    assert.match(
      js,
      /RETURN_METHOD_NAMES = Object\.freeze\(Object\.values\(RETURN_METHODS\)\)/,
      'RETURN_METHOD_NAMES must be derived from RETURN_METHODS, not listed again',
    );
  }
});

test('a Settings save reaches a RUNNING SRT return, not just the controls', () => {
  // gst.ReturnOpts is read once, in Play. Nothing about a running pipeline
  // changes when the configuration does — so without this the operator sets
  // "Left only", presses Save, every control on screen agrees, and comms is
  // still in their right ear. Deterministic, not a race, and silent.
  const src = ui('app.js');

  const start = src.indexOf('function onConfigSaved(config)');
  assert.ok(start > 0, 'app.js must handle a Settings save');
  const body = src.slice(start, src.indexOf('\n  function applyReturnOptionsFromConfig', start));
  assert.match(
    body,
    /applyReturnOptionsFromConfig\(\)/,
    'a Settings save must apply the saved return options to a running SRT return',
  );

  const applyStart = src.indexOf('function applyReturnOptionsFromConfig()');
  const apply = src.slice(applyStart, src.indexOf('\n  }', applyStart));
  assert.match(
    apply,
    /returnOptsFingerprint\(currentConfig\)/,
    'and it must decide on what the pipeline was BUILT from, not on every save: ' +
      'rebuilding because somebody corrected a typo takes the return away for a second',
  );
  assert.match(apply, /returnPath\s*[\s\S]*\.applyOption\(/, 'through the one machine');
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

test('the Headphones dropdown is plainly labelled, and never swaps lists', () => {
  // It used to swap between the browser's mediaDeviceIds and Windows' WASAPI
  // endpoint ids as the return-source control moved. That control is gone, so
  // the list is always the browser's. The label used to SAY so — "Headphones —
  // browser (enumerateDevices)" — and the operator removed the qualifier: it
  // was engineering trivia on the screen of somebody about to commentate. The
  // invariant it advertised is still enforced here: one list, never the WASAPI
  // one, whose field lives on the Settings screen.
  const src = ui('home.js');
  assert.match(
    src,
    /headphoneLabel\.textContent = 'Headphones\/output'/,
    'the label is the plain "Headphones/output" the operator asked for',
  );
  assert.ok(
    !/DEVICE_SOURCE_SRT/.test(src),
    'home.js must never put the WASAPI list behind this dropdown again',
  );
  assert.ok(
    !/DEVICE_SOURCE_WEBRTC/.test(src),
    'the qualifier is gone entirely: no import left over to drift back in',
  );
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
  // The port the return dials is NOT carried any more, and must not become
  // carried again. Carrying it is what made it uneditable: an operator whose
  // config.json held 40503 — the encrypted clean feed — had no control to
  // correct it with, so the monitor could never connect and the screen said
  // nothing. It is collected from its own numeric field now.
  assert.match(
    src,
    /srtReturnPort: Number\(fields\.srtReturnPort\.input\.value\)/,
    'the return port must be collected from its control, not carried',
  );
  assert.equal(
    /carriedSRTReturnPort/.test(src),
    false,
    'the return port is carried again; a carried field is a field an operator cannot fix',
  );
  assert.match(src, /returnChannel: normaliseChannelMode\(/, 'and the channel is collected from its control');
  assert.match(src, /\[DEVICE_KEY_SRT\]:/, 'and the WASAPI endpoint id is not dropped either');
});
