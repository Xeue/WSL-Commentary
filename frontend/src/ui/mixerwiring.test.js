/**
 * Tests that the mixer drawer is actually wired in.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= WHY THESE READ SOURCE ==============================
 *
 * These assert on the TEXT of the modules rather than by driving them, and that
 * is a deliberate choice with a reason on both sides.
 *
 * It cannot be done by driving them. settings.js imports ./mixer/index.js,
 * which imports ./mixer.css — `node --test` cannot parse a CSS import, which is
 * precisely why index.js exists and why drawer.js does not import the
 * stylesheet itself. And the DOM shim in mixer/testdom.js implements exactly
 * the surface drawer.js uses; settings.js needs <form>, <select>, classList and
 * more, and widening a shim until a test passes is how a shim stops being
 * evidence.
 *
 * And the failure they exist to catch is a TEXTUAL one. internal/mixer and
 * frontend/src/ui/mixer/ were complete, reviewed and committed, and none of it
 * shipped, because no module imported the drawer. `npm run build` succeeded the
 * whole time. The thing that was missing was an import statement, so an import
 * statement is what is asserted.
 *
 * The real proof that the drawer is in the bundle is a grep of frontend/dist
 * for a string only the drawer emits. These tests are what stops it silently
 * falling out again.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');

const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

test('settings.js imports the drawer through its entry point', () => {
  const src = ui('settings.js');
  assert.match(
    src,
    /import \{ createMixerDrawer \} from '\.\/mixer\/index\.js'/,
    'settings.js must import the drawer, or none of it reaches the bundle',
  );
  // index.js is the entry point BECAUSE it is where mixer.css is imported.
  // Importing drawer.js directly would build and would ship the drawer with no
  // stylesheet at all.
  assert.ok(
    !/from '\.\/mixer\/drawer\.js'/.test(src),
    'settings.js must not reach past index.js to drawer.js — that is how the CSS gets left behind',
  );
  // A dynamic import would also work, but only if it is ever evaluated. A
  // static one cannot be code-split away from the bundle by accident.
  assert.ok(
    !/import\(['"]\.\/mixer/.test(src),
    'the drawer must be imported statically, so its presence in the bundle does not depend on a code path running',
  );
});

test('mixer/index.js is what pulls the stylesheet into the bundle', () => {
  const src = read(here, 'mixer', 'index.js');
  assert.match(src, /import '\.\/mixer\.css'/, 'index.js must import mixer.css');
});

test('the drawer is behind Settings, not on the commentary screen', () => {
  // Engineers set the desk up with it before a match; commentators never open
  // it. A control that can change a live clean feed does not belong next to
  // START.
  const src = ui('home.js');
  assert.ok(!/mixer/i.test(src), 'home.js must not reference the mixer drawer');
});

test('settings.js supplies every option the drawer requires', () => {
  const src = ui('settings.js');
  for (const option of ['mount', 'getSnapshot', 'sendCommands', 'getGolden', 'setGolden']) {
    assert.match(src, new RegExp(`\\b${option}:`), `createMixerDrawer needs opts.${option}`);
  }
  // Not required by createMixerDrawer, but required by this host: without
  // onArmedChange the Go arm gate is never opened and Apply always fails, and
  // without onError the drawer's failures go nowhere an operator can see.
  assert.match(src, /\bonArmedChange:/, 'the host must mirror the drawer arm onto the Go gate');
  assert.match(src, /\bonError:/, 'the host must surface the drawer errors');
});

test('arming the drawer arms the Go gate, and disarming disarms it', () => {
  const src = ui('settings.js');
  assert.match(src, /backend\.armMixer\(\)/, 'the drawer arm must reach App.ArmMixer');
  assert.match(src, /backend\.disarmMixer\(\)/, 'the drawer disarm must reach App.DisarmMixer');
});

test('the write path reaches the mixer only through the drawer', () => {
  // The drawer's own gate is the outer of two independent gates. A second call
  // site for sendMixerCommands anywhere in the host would be a way past it, and
  // would look like a convenience.
  const src = ui('settings.js');
  const uses = src.match(/backend\.sendMixerCommands/g) || [];
  assert.equal(uses.length, 1, 'sendMixerCommands must have exactly one call site: the drawer options');
});

test('the host feeds the drawer on a timer while it is open, and stops when it closes', () => {
  const src = ui('settings.js');
  assert.match(src, /setInterval\(/, 'the host must poll: the status socket carries no mixer subscription');
  assert.match(src, /clearInterval\(/, 'the polling must stop when the drawer closes');
  assert.match(src, /mixerDrawer\.update\(snapshot\)/, 'the poll must feed the drawer through update()');
  assert.match(src, /isOpen\(\)/, 'the poll must notice a drawer closed from inside itself');
});

test('backend.js binds each mixer call to the Go method of that name', () => {
  // The one contract that no Go test and no JS test can see on its own: a
  // rename on either side compiles, builds, and fails inside WebView2 as a
  // rejected promise with no clue in it.
  const js = ui('backend.js');
  const go = read(repoRoot, 'app_mixer.go');

  const bindings = [
    ['getMixerSnapshot', 'GetMixerSnapshot'],
    ['armMixer', 'ArmMixer'],
    ['disarmMixer', 'DisarmMixer'],
    ['sendMixerCommands', 'SendMixerCommands'],
    ['getMixerGolden', 'GetMixerGolden'],
    ['setMixerGolden', 'SetMixerGolden'],
  ];

  for (const [jsName, goName] of bindings) {
    assert.match(js, new RegExp(`export async function ${jsName}\\(`), `backend.js exports ${jsName}`);
    assert.match(js, new RegExp(`callGo\\('${goName}'`), `${jsName} calls Go's ${goName}`);
    assert.match(go, new RegExp(`func \\(a \\*App\\) ${goName}\\(`), `app_mixer.go declares ${goName}`);
  }
});

test('no mixer call falls back to a fake', () => {
  // Every other backend export answers from an in-memory fake when there is no
  // Wails runtime. The mixer must not: a fabricated snapshot renders as a
  // routing matrix an operator can read, and the most likely fabrication — an
  // empty one — reads as "nothing is in the clean feed".
  const js = ui('backend.js');
  const mixerSection = js.slice(js.indexOf('// The mixer drawer'));
  assert.ok(mixerSection.length > 0, 'backend.js has a mixer section');
  for (const fn of [
    'getMixerSnapshot',
    'armMixer',
    'disarmMixer',
    'sendMixerCommands',
    'getMixerGolden',
    'setMixerGolden',
  ]) {
    const body = mixerSection.slice(mixerSection.indexOf(`export async function ${fn}(`));
    const end = body.indexOf('\n}');
    assert.match(
      body.slice(0, end),
      /requireWails\(\);/,
      `${fn} must refuse without a real backend rather than invent mixer state`,
    );
  }
});
