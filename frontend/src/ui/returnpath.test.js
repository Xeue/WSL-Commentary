/**
 * Tests for the return path state machine (returnpath.js).
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE PROPERTY, NOT THE EXAMPLES =====================
 *
 * The rule is "exactly one return path is audible, in every reachable state
 * including every failure path". Three examples cannot establish that. What
 * establishes it is an io double that models the two paths INDEPENDENTLY of the
 * module's own bookkeeping, driven through every entry point under every
 * combination of which call fails and how, asserting at the moment of each call:
 *
 *   SAFETY    WebRTC is never made audible while the Go-side monitor is running,
 *             and the monitor never starts while WebRTC is audible.
 *   LIVENESS  When an operation has finished, something is audible. Silence is
 *             fine DURING a transition and is never where one is allowed to end.
 *   HONESTY   The selected path, as the UI would render it, names the path that
 *             is actually making sound.
 *
 * The double is the model. It says "srt is running" because startReturn resolved,
 * not because returnpath.js thinks so — which is the whole point, since the bug
 * this replaces was returnpath's predecessor believing SRT had not started when
 * it had.
 *
 * The named scenarios at the bottom are the two reported failures, written out
 * so that a regression reads as the incident rather than as a matrix cell.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  createReturnPath,
  returnOptsFingerprint,
  RETURN_OPTS_CONFIG_KEYS,
  AUDIBLE_BOTH,
  AUDIBLE_NONE,
} from './returnpath.js';
import { RETURN_SOURCE_WEBRTC, RETURN_SOURCE_SRT } from './returnsource.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');

// ---------------------------------------------------------------------------
// The model
// ---------------------------------------------------------------------------

/** The text of app_return.go's two sentinels, as they cross the Wails boundary. */
const ALREADY_RUNNING = 'wslcomms: the return monitor is already running';
const NOT_RUNNING = 'wslcomms: the return monitor is not running';

/**
 * createBackendDouble models the Go side and the browser's audio graph as two
 * independent booleans, and records a violation the instant they are both true.
 *
 * The failure modes are the ones that actually occur:
 *
 *   start 'fail'            StartReturn refuses — bad config, no bindings, gst init
 *   start 'failString'      Wails rejecting with a bare string, which it does
 *   start 'failAfterFirst'  works once and then stops working, which is what a
 *                           mid-match M2L-X restart looks like to a rebuild
 *   stop  'fail'            StopReturn reports a failure but HAS dropped its
 *                           session first — app_return.go sets a.ret = nil and
 *                           only then calls mon.Stop(), so a reported failure
 *                           still means no monitor is being managed
 *   stop  'wedged'          the pipeline would not reach NULL and audio may
 *                           genuinely still be flowing. The one case the module
 *                           knowingly accepts a risk of overlap over certain
 *                           silence — asserted separately, and it must SAY SO
 *   stop  'bindingMissing'  this build has no StopReturn, so it has no
 *                           StartReturn either
 *   orphan                  a monitor left running by a page that reloaded
 *                           before its beforeunload stopReturn() completed
 */
function createBackendDouble({
  startMode = 'ok',
  stopMode = 'ok',
  saveMode = 'ok',
  orphan = false,
} = {}) {
  // A BUILD WITH NO StopReturn HAS NO StartReturn EITHER, so it can have no
  // monitor to orphan. That is not a convenience: srtReturnAvailable() requires
  // ALL FIVE bindings before the SRT option is offered at all (see
  // returnsource.test.js, "srtReturnAvailable checks every binding..."), which is
  // what makes it true. A double that allowed the split world would be asserting
  // a property over a state the application cannot reach, and the only way to
  // satisfy it would be to keep a commentator silent in a state that never
  // happens.
  const bindingsMissing = stopMode === 'bindingMissing';
  if (bindingsMissing) orphan = false;

  const model = {
    srt: orphan, // is a Go-side monitor running and playing into the headphones
    webrtc: false, // is the Web Audio graph connected to the output
    violations: [],
    errors: [],
    logs: [],
    sourceRenders: [],
    saved: null,
    startCalls: 0,
    stopCalls: 0,
    saveCalls: 0,
  };

  function check(where) {
    if (model.srt && model.webrtc) {
      model.violations.push(`${where}: both return paths audible at once`);
    }
  }

  const io = {
    setWebRTCAudible(on) {
      model.webrtc = !!on;
      if (on) check('setWebRTCAudible(true)');
    },

    async startReturn() {
      model.startCalls += 1;
      if (bindingsMissing) {
        const err = new Error('wslcomms: this build has no App.StartReturn');
        err.name = 'BindingMissingError';
        throw err;
      }
      // The Go side refuses a second monitor whatever else the test asked for.
      // This is app_return.go's errReturnAlreadyRunning and it is not optional.
      if (model.srt) throw new Error(ALREADY_RUNNING);
      if (startMode === 'fail') {
        throw new Error('wslcomms: starting the SRT return: no such headphone endpoint');
      }
      if (startMode === 'failString') {
        // Wails surfaces a Go error as a bare string on some runtime versions.
        throw 'wslcomms: starting the SRT return: srtsrc: Connection timeout (16)';
      }
      if (startMode === 'failAfterFirst' && model.startCalls > 1) {
        throw new Error('wslcomms: starting the SRT return: the endpoint went away');
      }
      model.srt = true;
      check('startReturn resolved');
    },

    async stopReturn() {
      model.stopCalls += 1;
      if (stopMode === 'bindingMissing') {
        const err = new Error('wslcomms: this build has no App.StopReturn');
        err.name = 'BindingMissingError';
        throw err;
      }
      if (stopMode === 'wedged') {
        // Session dropped, pipeline not at NULL: audio may still be flowing and
        // nothing can find out from here.
        throw new Error('wslcomms: stopping the SRT return: pipeline would not go to NULL');
      }
      const wasRunning = model.srt;
      // Dropped BEFORE any failure is reported, exactly as app_return.go does.
      model.srt = false;
      if (!wasRunning) throw new Error(NOT_RUNNING);
      if (stopMode === 'fail') {
        throw new Error('wslcomms: stopping the SRT return: the endpoint was already gone');
      }
    },

    async saveSource(source) {
      model.saveCalls += 1;
      if (saveMode === 'fail') throw new Error('wslcomms: config.json is read-only');
      model.saved = source;
    },

    isAlreadyRunning: (err) =>
      String(err?.message ?? err)
        .toLowerCase()
        .includes('the return monitor is already running'),
    isNotRunning: (err) =>
      String(err?.message ?? err)
        .toLowerCase()
        .includes('the return monitor is not running'),

    showError: (m) => model.errors.push(m),
    log: (m) => model.logs.push(m),
    onSource: (s) => model.sourceRenders.push(s),
  };

  return { io, model, check };
}

/** rendered is the path the UI would be showing: the last onSource call. */
function rendered(model) {
  return model.sourceRenders[model.sourceRenders.length - 1];
}

/** modelAudible reduces the DOUBLE's state, not the module's belief. */
function modelAudible(model) {
  if (model.srt && model.webrtc) return AUDIBLE_BOTH;
  if (model.srt) return RETURN_SOURCE_SRT;
  if (model.webrtc) return RETURN_SOURCE_WEBRTC;
  return AUDIBLE_NONE;
}

// ---------------------------------------------------------------------------
// The property
// ---------------------------------------------------------------------------

const START_MODES = ['ok', 'fail', 'failString', 'failAfterFirst'];
// 'wedged' is excluded here and asserted on its own below: it is the one mode in
// which audio may genuinely survive a stop, so overlap is possible by
// construction and no code in this module can prevent it.
const STOP_MODES = ['ok', 'fail', 'bindingMissing'];
const SAVE_MODES = ['ok', 'fail'];
const ORPHANS = [false, true];

/** Every way the operator can reach this module. */
const SCENARIOS = [
  {
    name: 'startup on the saved SRT path',
    run: (p) => p.adoptSaved(RETURN_SOURCE_SRT),
  },
  {
    name: 'startup on the saved WebRTC path',
    run: (p) => p.adoptSaved(RETURN_SOURCE_WEBRTC),
  },
  {
    name: 'switch WebRTC -> SRT',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_WEBRTC);
      return p.select(RETURN_SOURCE_SRT);
    },
  },
  {
    name: 'switch SRT -> WebRTC',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return p.select(RETURN_SOURCE_WEBRTC);
    },
  },
  {
    name: 'switch there and back',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_WEBRTC);
      await p.select(RETURN_SOURCE_SRT);
      return p.select(RETURN_SOURCE_WEBRTC);
    },
  },
  {
    name: 'return channel changed while on SRT',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return p.applyOption({ what: 'return channel', save: async () => {} });
    },
  },
  {
    name: 'headphone endpoint changed while on SRT',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return p.applyOption({ what: 'SRT headphone device', save: async () => {} });
    },
  },
  {
    name: 'an option change whose own save fails',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return p.applyOption({
        what: 'return channel',
        save: async () => {
          throw new Error('wslcomms: config.json is read-only');
        },
      });
    },
  },
  {
    name: 'two switches fired at once',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_WEBRTC);
      return Promise.all([p.select(RETURN_SOURCE_SRT), p.select(RETURN_SOURCE_WEBRTC)]);
    },
  },
  {
    name: 'a switch and an option change fired at once',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_WEBRTC);
      return Promise.all([
        p.select(RETURN_SOURCE_SRT),
        p.applyOption({ what: 'return channel', save: async () => {} }),
      ]);
    },
  },
  {
    // BOTH ORDERINGS, because they are not the same test. The guard that
    // catches "option change, then switch" is the switch's; the guard that
    // catches "switch, then option change" is the option change's, and it is the
    // one that ends with a commentator on the path they just asked to leave
    // while the control says they left it.
    name: 'a switch away from SRT with an option change fired at once',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return Promise.all([
        p.select(RETURN_SOURCE_WEBRTC),
        p.applyOption({ what: 'return channel', save: async () => {} }),
      ]);
    },
  },
  {
    name: 'an option change fired during a switch to SRT',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_WEBRTC);
      const switching = p.select(RETURN_SOURCE_SRT);
      const option = p.applyOption({ what: 'SRT headphone device', save: async () => {} });
      return Promise.all([switching, option]);
    },
  },
  {
    name: 'two option changes fired at once',
    async run(p) {
      await p.adoptSaved(RETURN_SOURCE_SRT);
      return Promise.all([
        p.applyOption({ what: 'return channel', save: async () => {} }),
        p.applyOption({ what: 'SRT headphone device', save: async () => {} }),
      ]);
    },
  },
];

test('EXACTLY ONE PATH IS AUDIBLE, in every reachable state and on every failure path', async () => {
  let cases = 0;

  for (const scenario of SCENARIOS) {
    for (const startMode of START_MODES) {
      for (const stopMode of STOP_MODES) {
        for (const saveMode of SAVE_MODES) {
          for (const orphan of ORPHANS) {
            const where =
              `${scenario.name} [start=${startMode} stop=${stopMode} ` +
              `save=${saveMode} orphan=${orphan}]`;
            const { io, model } = createBackendDouble({
              startMode,
              stopMode,
              saveMode,
              orphan,
            });
            const path = createReturnPath(io);

            await scenario.run(path);
            cases += 1;

            // SAFETY. Recorded at the moment of each call, not inferred after.
            assert.deepEqual(
              model.violations,
              [],
              `${where}: both paths were audible at once — ${model.violations.join('; ')}`,
            );

            // LIVENESS. A transition may pass through silence; it may not stop
            // there. The commentator has to be able to hear SOMETHING.
            assert.notEqual(
              modelAudible(model),
              AUDIBLE_NONE,
              `${where}: the operation finished with no audio on either path`,
            );

            // HONESTY. What the UI is showing has to be the path making sound.
            assert.equal(
              modelAudible(model),
              rendered(model),
              `${where}: the UI says "${rendered(model)}" and "${modelAudible(model)}" is audible`,
            );
            assert.equal(
              path.source,
              rendered(model),
              `${where}: the module's own source disagrees with what it rendered`,
            );

            // And nothing may be left holding the guard.
            assert.equal(path.busy, false, `${where}: the in-flight guard was left raised`);
          }
        }
      }
    }
  }

  // A guard on the guard: if a refactor collapses the matrix, this notices.
  assert.ok(cases >= 500, `the property was only exercised over ${cases} cases`);
});

test('WebRTC is never un-muted before the SRT return has been asked to stop', async () => {
  // The structural half of the safety property, stated as an ordering over the
  // calls rather than over the model: every setWebRTCAudible(true) must be
  // preceded by a stopReturn, with no startReturn in between.
  const calls = [];
  const { io } = createBackendDouble();
  const spy = {
    ...io,
    setWebRTCAudible(on) {
      calls.push(on ? 'unmute' : 'mute');
      io.setWebRTCAudible(on);
    },
    async startReturn() {
      calls.push('start');
      return io.startReturn();
    },
    async stopReturn() {
      calls.push('stop');
      return io.stopReturn();
    },
  };

  const path = createReturnPath(spy);
  await path.adoptSaved(RETURN_SOURCE_SRT);
  await path.select(RETURN_SOURCE_WEBRTC);
  await path.select(RETURN_SOURCE_SRT);
  await path.select(RETURN_SOURCE_WEBRTC);

  for (let i = 0; i < calls.length; i += 1) {
    if (calls[i] !== 'unmute') continue;
    const before = calls.slice(0, i);
    const lastStop = before.lastIndexOf('stop');
    const lastStart = before.lastIndexOf('start');
    assert.ok(lastStop > -1, `un-muted WebRTC at step ${i} with no stopReturn before it ever`);
    assert.ok(
      lastStop > lastStart,
      `un-muted WebRTC at step ${i} after a startReturn with no stopReturn since: ${calls.join(' ')}`,
    );
  }
});

test('a stop that leaves the pipeline wedged un-mutes anyway, and SAYS SO', async () => {
  // The single case where overlap is possible: StopReturn reported that the
  // pipeline would not reach NULL, so audio may still be flowing and nothing
  // here can find out. Leaving WebRTC muted would mean certain silence with no
  // way out — the only control that could recover it also calls stopReturn.
  // So the module un-mutes and tells the operator what to listen for.
  const { io, model } = createBackendDouble({ stopMode: 'wedged', orphan: true });
  const path = createReturnPath(io);

  await path.adoptSaved(RETURN_SOURCE_WEBRTC);

  assert.equal(model.webrtc, true, 'the commentator is not left in silence');
  assert.ok(
    model.errors.some((m) => /hear the match twice|restart the application/i.test(m)),
    `the operator must be told the return may not have stopped: ${JSON.stringify(model.errors)}`,
  );
});

// ---------------------------------------------------------------------------
// The two reported failures, written out as incidents
// ---------------------------------------------------------------------------

test('INCIDENT: the WebView reloads mid-match while the SRT return is up', async () => {
  // beforeunload fires stopReturn() fire-and-forget and the context dies before
  // the IPC completes, so the Go-side monitor is still running. The new page
  // reads returnSource: "srt" and its StartReturn is refused with
  // errReturnAlreadyRunning.
  //
  // Treating that as a failed start and falling back to WebRTC is the worst
  // outcome this application has: the orphaned GStreamer pipeline is still
  // writing CLN to the same headphones, so the commentator hears the match
  // twice, a few hundred milliseconds apart, while trying to talk over it — and
  // the banner claims the opposite.
  const { io, model } = createBackendDouble({ orphan: true });
  const path = createReturnPath(io);

  const result = await path.adoptSaved(RETURN_SOURCE_SRT);

  assert.deepEqual(model.violations, [], 'the commentator must not hear the match twice');
  assert.equal(result.applied, true, 'the SRT return is the path that ends up running');
  assert.equal(path.source, RETURN_SOURCE_SRT);
  assert.equal(model.srt, true, 'a monitor is running');
  assert.equal(model.webrtc, false, 'and the WebRTC return is silent');
  assert.ok(
    model.stopCalls >= 1,
    'the orphan must be stopped, not adopted: it was built from whatever was saved when the ' +
      'previous page started it, and nothing on screen would agree with it',
  );
  assert.ok(
    !model.errors.some((m) => /falling back to the webrtc return/i.test(m)),
    `no banner may claim a fallback that did not happen: ${JSON.stringify(model.errors)}`,
  );
});

test('INCIDENT: a throw AFTER StartReturn resolved does not leave both paths up', async () => {
  // The recovery used to assume any throw meant the SRT monitor had not
  // started. Everything raised after startReturn() resolves makes that false —
  // getReturnState() hitting a BindingMissingError being the reported case, and
  // a second startReturn inside a rebuild being the one that can be driven here.
  const { io, model } = createBackendDouble({ startMode: 'failAfterFirst' });
  const path = createReturnPath(io);

  await path.adoptSaved(RETURN_SOURCE_SRT);
  assert.equal(model.srt, true, 'the return started the first time');

  await path.applyOption({ what: 'return channel', save: async () => {} });

  assert.deepEqual(model.violations, [], 'the failure after a successful start must not overlap');
  assert.equal(model.srt, false, 'the SRT return is stopped');
  assert.equal(model.webrtc, true, 'and the commentator is on WebRTC rather than in silence');
  assert.equal(path.source, RETURN_SOURCE_WEBRTC, 'and the control says so');
});

test('INCIDENT: a channel change whose restart fails leaves audio, not silence', async () => {
  // Save -> stop -> start behind one .catch that only showed a banner. If
  // startReturn() failed after stopReturn() succeeded the commentator had NO
  // AUDIO AT ALL: SRT stopped, WebRTC still muted, and the control still
  // reading "srt".
  const { io, model } = createBackendDouble({ startMode: 'failAfterFirst' });
  const path = createReturnPath(io);

  await path.adoptSaved(RETURN_SOURCE_SRT);
  const result = await path.applyOption({ what: 'return channel', save: async () => {} });

  assert.equal(result.applied, false);
  assert.equal(modelAudible(model), RETURN_SOURCE_WEBRTC, 'something must be audible');
  assert.equal(path.source, RETURN_SOURCE_WEBRTC, 'and the control must not still claim SRT');
  assert.ok(
    model.errors.some((m) => /silence|webrtc/i.test(m)),
    'and the operator must be told which path they are on now',
  );
});

test('an option change cannot interleave with a source switch, in EITHER order', async () => {
  // Neither used to take the in-flight guard, so a stop from one could land
  // between the other's stop and start. The two orders fail differently and
  // therefore have to be tested separately:
  //
  //   option first, then switch   the switch's guard catches it
  //   switch first, then option   the OPTION's guard is the only thing that can,
  //                               and without it the operator ends up back on
  //                               SRT — the path they just asked to leave —
  //                               while the control reads WEBRTC
  for (const optionFirst of [true, false]) {
    const { io, model } = createBackendDouble();
    const path = createReturnPath(io);
    await path.adoptSaved(RETURN_SOURCE_SRT);

    const option = () => path.applyOption({ what: 'return channel', save: async () => {} });
    const switching = () => path.select(RETURN_SOURCE_WEBRTC);
    const [a, b] = optionFirst ? [option(), switching()] : [switching(), option()];
    const [first, second] = await Promise.all([a, b]);

    const where = optionFirst ? 'option change first' : 'source switch first';
    assert.deepEqual(model.violations, [], where);
    assert.ok(
      [first, second].some((r) => r.applied === false),
      `${where}: the second operation must be refused rather than interleaved`,
    );
    const refused = [first, second].find((r) => r.applied === false);
    assert.match(refused.reason || '', /still changing/i, `${where}: the refusal must say why`);
    assert.equal(
      modelAudible(model),
      rendered(model),
      `${where}: the control must name the path that is actually making sound`,
    );
  }
});

test('re-selecting the path already in force is a no-op, not a rebuild', async () => {
  const { io, model } = createBackendDouble();
  const path = createReturnPath(io);
  await path.adoptSaved(RETURN_SOURCE_SRT);
  const startsAfterAdopt = model.startCalls;

  const result = await path.select(RETURN_SOURCE_SRT);

  assert.equal(result.applied, true);
  assert.equal(model.startCalls, startsAfterAdopt, 'the running return must not be restarted');
});

test('startup on WebRTC still asks the Go side to stop, because it may not be idle', async () => {
  // "The saved path is WebRTC" does not prove nothing is running. A page that
  // reloaded after a failed switch can find an orphaned monitor playing into the
  // same headphones, and nothing else in the application would ever stop it.
  const { io, model } = createBackendDouble({ orphan: true });
  const path = createReturnPath(io);

  await path.adoptSaved(RETURN_SOURCE_WEBRTC);

  assert.equal(model.stopCalls, 1, 'the orphan is asked to stop');
  assert.equal(model.srt, false);
  assert.equal(model.webrtc, true);
  assert.deepEqual(model.violations, []);
});

test('a build with no SRT bindings starts on WebRTC without a banner', async () => {
  const { io, model } = createBackendDouble({ stopMode: 'bindingMissing' });
  const path = createReturnPath(io);

  await path.adoptSaved(RETURN_SOURCE_WEBRTC);

  assert.equal(model.webrtc, true);
  assert.deepEqual(model.errors, [], 'a missing binding on a path nobody chose is not an error');
  assert.ok(model.logs.length > 0, 'but it is worth a console line');
});

// ---------------------------------------------------------------------------
// Which settings need a rebuild
// ---------------------------------------------------------------------------

test('returnOptsFingerprint changes for every field gst.ReturnOpts is built from', () => {
  const base = {
    m2lxHost: 'm2lx.example',
    srtReturnPort: 40501,
    srtLatencyMs: 120,
    srtReturnPBKeyLen: 0,
    returnChannel: 'stereo',
    headphoneEndpointId: '{0.0.0.00000000}.{aaaa}',
    // Fields that must NOT force a rebuild: nothing in gst.ReturnOpts reads them.
    returnGainDb: 18,
    returnMid: 2,
    audioDeviceId: '{0.0.1.00000000}.{bbbb}',
    headphoneDeviceId: 'browser-id',
    // The SEND path's key length. app_return.go stopped reading it when the two
    // paths were given separate encryption settings, so changing it must no
    // longer tear down a working monitor.
    pbkeylen: 0,
  };
  const baseline = returnOptsFingerprint(base);

  for (const key of RETURN_OPTS_CONFIG_KEYS) {
    const changed = { ...base, [key]: `${base[key]}-changed` };
    assert.notEqual(
      returnOptsFingerprint(changed),
      baseline,
      `changing ${key} must be visible to the rebuild decision`,
    );
  }

  for (const key of ['returnGainDb', 'returnMid', 'audioDeviceId', 'headphoneDeviceId', 'pbkeylen']) {
    const changed = { ...base, [key]: `${base[key]}-changed` };
    assert.equal(
      returnOptsFingerprint(changed),
      baseline,
      `changing ${key} must NOT stop and restart a working return`,
    );
  }
});

test('RETURN_OPTS_CONFIG_KEYS covers every config field app_return.go reads', () => {
  // The cross-language contract that no Go test and no JS test sees on its own.
  // A field added to gst.ReturnOpts and read from the config in returnOpts is a
  // field a Settings save can change under a running pipeline — silently, since
  // Play reads the options once and never looks again.
  const go = read(repoRoot, 'app_return.go');
  const start = go.indexOf('func (a *App) returnOpts(');
  assert.ok(start > 0, 'app_return.go must still have returnOpts');
  const body = go.slice(start, go.indexOf('\n}\n', start));

  // Every cfg.<Something> the function reads, mapped to its config.json key.
  const goToJSON = {
    EffectiveSRTHost: ['m2lxHost'],
    EffectiveSRTReturnPort: ['srtReturnPort'],
    SRTLatencyMs: ['srtLatencyMs'],
    // The RETURN path's key length, not the send path's PBKeyLen. If
    // app_return.go ever reads cfg.PBKeyLen again this table has no entry for
    // it and the assertion below fails, which is the point: the two paths dial
    // endpoints M2L-X encrypts independently.
    SRTReturnPBKeyLen: ['srtReturnPBKeyLen'],
    EffectiveReturnChannel: ['returnChannel'],
    HeadphoneEndpointID: ['headphoneEndpointId'],
  };

  const reads = [...body.matchAll(/\bcfg\.([A-Za-z]+)/g)].map((m) => m[1]);
  assert.ok(reads.length > 0, 'returnOpts must read something from the config');

  for (const name of new Set(reads)) {
    const keys = goToJSON[name];
    assert.ok(
      keys,
      `app_return.go's returnOpts now reads cfg.${name}, which returnpath.js does not know ` +
        'about. Add it to RETURN_OPTS_CONFIG_KEYS and to this table, or a Settings save will ' +
        'change it under a running pipeline with nothing on screen disagreeing.',
    );
    for (const key of keys) {
      assert.ok(
        RETURN_OPTS_CONFIG_KEYS.includes(key),
        `RETURN_OPTS_CONFIG_KEYS is missing ${key} (from cfg.${name})`,
      );
    }
  }
});

test('the already-running sentinel is spelled the same way on both sides', () => {
  // Wails flattens a Go error to its message, so the sentinel does not survive
  // the boundary as anything else. If the two drift apart the frontend stops
  // recognising "already running", treats it as a failed start, and takes the
  // recovery path with a monitor running — which is the first incident above.
  const js = read(here, 'backend.js');
  const go = read(repoRoot, 'app_return.go');

  const marker = js.match(/export const RETURN_ALREADY_RUNNING = '([^']+)'/);
  assert.ok(marker, 'backend.js must name the sentinel text in one place');
  assert.ok(
    go.includes(`errReturnAlreadyRunning = errors.New("wslcomms: ${marker[1]}")`),
    `app_return.go no longer spells errReturnAlreadyRunning "wslcomms: ${marker[1]}"`,
  );

  assert.ok(
    go.includes('errReturnNotRunning = errors.New("wslcomms: the return monitor is not running")'),
    'app_return.go no longer spells errReturnNotRunning the way backend.js matches',
  );
  assert.match(js, /export function isReturnNotRunningError\(/);
});
