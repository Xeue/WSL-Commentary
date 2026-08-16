/**
 * Tests for the video-format selector and the switcher readout beside it.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURES THESE PREVENT =========================
 *
 * 1. AN OPTION THAT CANNOT BE SAVED. videoFormatOverride was a free-text box
 *    and is a <select> now, so every value an operator can choose is a value
 *    this application wrote down. If one of them does not parse, the operator
 *    picks it, presses Save, and is refused by a validator quoting a field they
 *    did not type into — which is worse than the box, because at least a typo
 *    is the typist's. The whole list is therefore run through validateConfig
 *    below, and through the same bounds internal/config's parser uses.
 *
 * 2. A SAVED RASTER SILENTLY DROPPED. A <select> discards a value it has no
 *    option for, without an error and without a trace: the screen would show
 *    1280x720p50 while config.json said 1920x1080p59.94, and Start would conform
 *    to the file. That is the same screen-and-file disagreement
 *    describeDeviceSelection exists to prevent on the device dropdowns, and it
 *    arrives here through a hand-edited file, a preset from an unusual facility,
 *    or simply a raster Go's parser accepts and this list does not carry.
 *
 * 3. A READOUT THAT CONFIRMS ITSELF. App.GetConformTarget answers from the
 *    RUNNING session when there is one, and otherwise reports the operator's own
 *    videoFormatOverride back, stamped source="override". Comparing an override
 *    against a target DERIVED FROM THAT OVERRIDE always matches — so a readout
 *    that ignored `source` would show a green "matches the switcher" on a seat
 *    that has never spoken to the switcher. That is not a missing feature, it is
 *    an invented confirmation, and it is the one thing a divergence warning must
 *    never do.
 *
 * Everything here is pure and is driven for real. The DOM half — that
 * settings.js actually builds the <select> from these plans — is asserted from
 * settings.js's text, in the manner of settings.test.js's own header: settings.js
 * builds against a real DOM and package.json is frozen, so there is no jsdom.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import {
  FOLLOW_SWITCHER,
  FOLLOW_SWITCHER_LABEL,
  VIDEO_FORMAT_GROUPS,
  CONFORM_SOURCE_SESSION,
  CONFORM_SOURCE_OVERRIDE,
  CONFORM_SOURCE_SWITCHER,
  isIndependentOfTheOperator,
  planVideoFormats,
  formatConformTarget,
  describeConformTarget,
  deriveFormatMatch,
  FORMAT_MATCH,
} from './videoformat.js';
import { validateConfig } from './validate.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

/** Every raster the control offers, flattened. */
const ALL_FORMATS = VIDEO_FORMAT_GROUPS.flatMap((g) => [...g.formats]);

/** A configuration validateConfig accepts, so a case can change one field. */
function validForm(videoFormatOverride) {
  return {
    m2lxHost: 'm2lx.example.com',
    alias: 'wsl-comms-ro',
    eventId: 'dl9-5p5ah0bd-empd',
    srtPort: 40001,
    srtLatencyMs: 120,
    pbkeylen: 0,
    videoBitrateKbps: 2000,
    videoFormatOverride,
    statusKey: '',
    audioDeviceId: '',
    audioSourceKind: 'native',
    decklinkPersistentId: '',
    headphoneDeviceId: '',
    headphoneEndpointId: '',
    returnMid: 2,
    returnChannel: 'stereo',
    returnSource: 'webrtc',
    srtReturnPort: 40501,
    srtReturnPBKeyLen: 0,
    pictureLatencyMs: 120,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
    slatePath: 'slate.png',
  };
}

// ---------------------------------------------------------------------------
// The option list itself
// ---------------------------------------------------------------------------

test('every raster the control offers is one this application can actually save', () => {
  // THE ACCEPTANCE CONDITION for turning the box into a list. An option an
  // operator can select and not save is worse than the free-text field it
  // replaced: the refusal names a field they never typed into, and there is
  // nothing they can do about it from the screen.
  for (const raster of ALL_FORMATS) {
    const { videoFormatOverride } = validateConfig(validForm(raster));
    assert.equal(
      videoFormatOverride,
      undefined,
      `${JSON.stringify(raster)} is an option on the Settings screen that validateConfig refuses`,
    );
  }
  // And blank, which is the default and is the one value that means "derive".
  assert.equal(validateConfig(validForm(FOLLOW_SWITCHER)).videoFormatOverride, undefined);
});

test('the rasters are the canonical spelling, so nothing has to rewrite them', () => {
  // collectConfig sends what is in the control VERBATIM (trimmed), and Go quotes
  // it back if it cannot parse it. A form that had to normalise its own options
  // on the way out would be answering a different question from the one on
  // screen — and the first value it could not rewrite would be the one that
  // mattered.
  const canonical = /^\d+x\d+p\d+(\.\d+)?$/;
  for (const raster of ALL_FORMATS) {
    assert.match(raster, canonical, `${raster} is not in internal/config's canonical form`);
    assert.equal(raster.trim(), raster, `${raster} has whitespace a trim would have to remove`);
  }
  // No duplicates: two identical lines in a dropdown is a choice nobody can make.
  assert.equal(new Set(ALL_FORMATS).size, ALL_FORMATS.length, 'a raster is offered twice');
});

test('the list carries the formats an M2L-X deployment is actually configured for', () => {
  // Not an exhaustive standards table — a longer list is a longer thing to scan,
  // and every entry past the one the operator wants is a cost. These are the
  // ones a missing entry would be a real problem: the example internal/config
  // itself uses, and the NTSC and UHD families a facility outside the UK needs.
  for (const raster of ['1920x1080p50', '1920x1080p59.94', '1280x720p50', '3840x2160p50']) {
    assert.ok(ALL_FORMATS.includes(raster), `the list must offer ${raster}`);
  }
  // The canonical example is Go's own constant, so the two cannot drift into a
  // state where the parser's documented example is not on the screen.
  const parser = read(repoRoot, 'internal', 'config', 'videoformat.go');
  const example = parser.match(/VideoFormatExample = "([^"]+)"/);
  assert.ok(example, 'internal/config must still name a canonical example');
  assert.ok(
    ALL_FORMATS.includes(example[1]),
    `internal/config's example is ${example[1]} and the control does not offer it`,
  );
});

test('following the switcher is the empty string and is NOT one of the rasters', () => {
  // EMPTY IS A SETTING, not an absence: it means "read the format from the
  // switcher", it is what every existing installation holds, and it is right on
  // almost every seat. It is handed to the caller separately from the groups so
  // that it can be rendered ABOVE them — an <optgroup> of one would file the
  // normal answer beside seventeen exceptions as an equal.
  assert.equal(FOLLOW_SWITCHER, '');
  assert.ok(!ALL_FORMATS.includes(FOLLOW_SWITCHER), 'the follow option is not a raster');

  const plan = planVideoFormats('');
  assert.deepEqual(plan.follow, { value: '', label: FOLLOW_SWITCHER_LABEL });
  assert.equal(plan.value, '', 'a blank override selects the follow option');
  for (const group of plan.groups) {
    assert.ok(
      !group.options.some((o) => o.value === ''),
      'the follow option must not also appear inside a group',
    );
  }
  // And the label says what happens rather than what is missing. "None" or
  // "(default)" would read as an unset field on a form where every other blank
  // genuinely is one.
  assert.match(FOLLOW_SWITCHER_LABEL, /switcher/i);
});

// ---------------------------------------------------------------------------
// The way out: a saved raster this build does not list
// ---------------------------------------------------------------------------

test('a saved raster the list does not carry is kept, not dropped', () => {
  // THE ONE WITH TEETH. A <select> silently discards a value it has no option
  // for. The screen would then show a plausible raster while config.json held
  // another, and Start would conform to the file — two statements about one
  // setting that cannot both be true, with nothing on screen to reconcile them.
  const odd = '2048x1080p48';
  assert.ok(!ALL_FORMATS.includes(odd), 'this case is only a case while the list lacks it');

  const plan = planVideoFormats(odd);
  assert.equal(plan.value, odd, 'the saved value must still be the selection');
  const flat = plan.groups.flatMap((g) => g.options.map((o) => o.value));
  assert.ok(flat.includes(odd), 'and it must have an option to be the selection OF');

  // In a group that says where it came from, rather than mixed in among the
  // rasters this application vouches for.
  const home = plan.groups.find((g) => g.options.some((o) => o.value === odd));
  assert.ok(!VIDEO_FORMAT_GROUPS.some((g) => g.label === home.label), 'it gets its own group');
  assert.match(home.label, /saved/i);
});

test('a known raster is not duplicated into the saved group', () => {
  for (const raster of ALL_FORMATS) {
    const plan = planVideoFormats(raster);
    const flat = plan.groups.flatMap((g) => g.options.map((o) => o.value));
    assert.equal(
      flat.filter((v) => v === raster).length,
      1,
      `${raster} appears twice; the operator sees one raster on two lines`,
    );
  }
});

test('the plan survives the values a stored config can actually hold', () => {
  // Whitespace is trimmed on the way in for the same reason collectConfig trims
  // on the way out — a hand-edited file with a trailing space would otherwise
  // land in the "saved on this machine" group as a raster of its own.
  assert.equal(planVideoFormats('  1920x1080p50  ').value, '1920x1080p50');
  for (const blank of [undefined, null, 0, {}, '   ']) {
    assert.equal(planVideoFormats(blank).value, FOLLOW_SWITCHER, `${JSON.stringify(blank)} is follow`);
  }
});

// ---------------------------------------------------------------------------
// The switcher's own format
// ---------------------------------------------------------------------------

test('the conform target renders as the same canonical raster the control offers', () => {
  // The frame rate crosses the Wails boundary as the OPERATOR-FACING DECIMAL —
  // app.go's ConformTargetView.FrameRate is gst.ConformTarget.DisplayFrameRate,
  // so 30000/1001 arrives as 29.97 — and this must not turn 50.0 into "50.0" or
  // 59.94 into "59.9", either of which would read as a divergence from a target
  // that agrees.
  assert.equal(formatConformTarget({ width: 1920, height: 1080, frameRate: 50 }), '1920x1080p50');
  assert.equal(formatConformTarget({ width: 1920, height: 1080, frameRate: 59.94 }), '1920x1080p59.94');
  assert.equal(formatConformTarget({ width: 1280, height: 720, frameRate: 29.97 }), '1280x720p29.97');

  // A PARTIAL TARGET IS NOT RENDERED AT ALL. app.go's conformTargetView already
  // refuses to return one — normaliseConformTarget replaces non-positive fields
  // individually, which would silently mix two sources into a raster neither of
  // them describes — and this is the same refusal on this side.
  for (const bad of [
    null,
    undefined,
    {},
    { width: 1920, height: 0, frameRate: 50 },
    { width: 1920, height: 1080, frameRate: 0 },
    { width: -1920, height: 1080, frameRate: 50 },
    { width: '1920', height: 1080, frameRate: 50 },
    { width: 1920, height: 1080, frameRate: Number.NaN },
  ]) {
    assert.equal(formatConformTarget(bad), '', `${JSON.stringify(bad)} must render as nothing`);
  }
});

test('the readout never presents the operator’s own override as the switcher’s format', () => {
  // THE INVENTED CONFIRMATION. With no session running, App.GetConformTarget
  // reports videoFormatOverride back, stamped source="override" — it does not
  // dial the instance, deliberately, because a UI call that can block for three
  // seconds to refine a readout is a bad trade on the lamps' startup path. A
  // readout that ignored `source` would echo the operator's own setting under
  // the heading "what M2L-X is configured for", which is the screen inventing
  // agreement with itself. This is the assertion that must never be relaxed.
  const own = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_OVERRIDE };
  const line = describeConformTarget(own);
  assert.ok(
    !line.includes('1920x1080p50'),
    'the readout quoted a raster that came from the operator, as though it came from the switcher',
  );

  // AND IT SAYS SO WITHOUT NAMING START ANY MORE. The wording used to be
  // "not read yet (press START)", which was the honest instruction while
  // GetConformTarget was the only binding: before a session there was genuinely
  // nothing to read and Start was what produced it. App.GetSwitcherFormat
  // removed that dead end — it reads the instance's own SETTING over one REST
  // call and needs no session — so the states that remain are no host, not
  // signed in yet, or an instance that is not up, and START addresses none of
  // them. Telling an operator to press START to fix an unreachable instance
  // would send them to put a feed on air to diagnose a configuration problem.
  assert.match(line, /not reachable/i, 'it must say the switcher could not be read');
  assert.doesNotMatch(
    line,
    /START/,
    'it must not send the operator to press START: with GetSwitcherFormat wired, START is no ' +
      'longer what makes the switcher readable',
  );

  // A RUNNING session's target IS the switcher's: Start derived it from the
  // switcher and the pipeline was built to it.
  const live = { width: 1280, height: 720, frameRate: 50, source: CONFORM_SOURCE_SESSION };
  assert.match(describeConformTarget(live), /1280x720p50/);

  // AND SO IS THE INSTANCE'S OWN SETTING, which is the whole point of the second
  // binding: this is the state a Settings screen opened an hour before kick-off
  // is in, and it is exactly when the operator is choosing a format.
  const setting = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SWITCHER };
  assert.match(describeConformTarget(setting), /1920x1080p50/);

  // Not known at all — no binding, not signed in, no host — reads the same as an
  // override, because for the operator it is the same thing: no independent
  // number to check against.
  assert.match(describeConformTarget(null), /not reachable/i);
});

test('a provenance this build does not know about is never quoted as the switcher’s', () => {
  // THE ALLOWLIST, and why isIndependentOfTheOperator is written as one. A
  // fourth provenance added to app.go — or an older frontend meeting a newer Go
  // — must fall SILENT rather than be believed. A denylist ("anything that is
  // not override") would quote the unknown raster as the switcher's, which is
  // the invented confirmation arriving by a different door.
  for (const source of ['override', 'guess', '', undefined, null, 42, {}]) {
    const target = { width: 1920, height: 1080, frameRate: 50, source };
    assert.equal(
      isIndependentOfTheOperator(target),
      false,
      `source ${JSON.stringify(source)} must not count as independent of the operator`,
    );
    assert.ok(
      !describeConformTarget(target).includes('1920x1080p50'),
      `source ${JSON.stringify(source)} was quoted as the switcher's format`,
    );
    assert.equal(
      deriveFormatMatch('1280x720p50', target).diverges,
      false,
      `source ${JSON.stringify(source)} drove a divergence warning off a raster nothing vouches for`,
    );
  }

  // The two that DO count, and nothing else.
  assert.equal(isIndependentOfTheOperator({ source: CONFORM_SOURCE_SESSION }), true);
  assert.equal(isIndependentOfTheOperator({ source: CONFORM_SOURCE_SWITCHER }), true);
  assert.equal(isIndependentOfTheOperator(null), false);
});

test('the switcher’s own setting drives the divergence warning with no session', () => {
  // The state the Settings screen is actually opened in: nothing running, and
  // App.GetSwitcherFormat's answer is the only independent number there is.
  const setting = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SWITCHER };

  const diverging = deriveFormatMatch('1280x720p50', setting);
  assert.equal(diverging.state, FORMAT_MATCH.DIVERGE);
  assert.equal(diverging.diverges, true, 'the caller marks the row off this flag');
  assert.match(diverging.line, /1920x1080p50/, 'it must name what the switcher is set to');

  const agreeing = deriveFormatMatch('1920x1080p50', setting);
  assert.equal(agreeing.state, FORMAT_MATCH.MATCH);
  assert.equal(agreeing.diverges, false);

  // Following the switcher is never a divergence, whatever the switcher says.
  assert.equal(deriveFormatMatch('', setting).state, FORMAT_MATCH.FOLLOW);
});

test('the provenance strings are the ones app.go actually stamps', () => {
  // A drift here does not error: every answer would simply fall through to
  // "not read yet", the divergence warning would never fire, and the control
  // would go back to being a raster with nothing to check it against — silently,
  // and in the safe-looking direction, which is the worst kind.
  const go = read(repoRoot, 'app.go');
  assert.match(
    go,
    new RegExp(`conformSourceSession = "${CONFORM_SOURCE_SESSION}"`),
    'app.go no longer stamps a running session as "session"',
  );
  assert.match(
    go,
    new RegExp(`conformSourceOverride = "${CONFORM_SOURCE_OVERRIDE}"`),
    'app.go no longer stamps the override case as "override"',
  );
  assert.match(
    go,
    new RegExp(`conformSourceSwitcher = "${CONFORM_SOURCE_SWITCHER}"`),
    'app.go no longer stamps GetSwitcherFormat\'s answer as "switcher" — the Settings screen ' +
      'reads that provenance to tell the instance\'s own setting from the operator\'s override',
  );
  assert.match(go, /Source string `json:"source"`/, 'and it must still cross the boundary');

  // THE BINDING ITSELF, because a constant with no method behind it is a
  // provenance nothing ever stamps. app_remote_test.go pins the remote
  // allowlist and dispatch case; this pins that the method exists at all and
  // that it answers the same view type the readout knows how to render.
  assert.match(
    go,
    /func \(a \*App\) GetSwitcherFormat\(\) \*ConformTargetView/,
    'App.GetSwitcherFormat is what lets the Settings screen show the switcher with no session',
  );

  // AND THAT THE FRONTEND ACTUALLY CALLS IT. The binding existed for a whole
  // revision with no wrapper on this side, and the readout said "not read yet
  // (press START)" the entire time — the failure this test is written against.
  const backendJs = read(repoRoot, 'frontend', 'src', 'ui', 'backend.js');
  assert.match(backendJs, /export async function getSwitcherFormat\(\)/);
  assert.match(
    backendJs,
    /const SWITCHER_FORMAT_METHOD = 'GetSwitcherFormat'/,
    'the bound method name must be spelled once, and match app.go',
  );
  const settingsJs = read(repoRoot, 'frontend', 'src', 'ui', 'settings.js');
  assert.match(
    settingsJs,
    /conformTarget = await backend\.getSwitcherFormat\(\)/,
    'the Settings screen must ask for the SWITCHER\'S OWN SETTING first: it is the only one of ' +
      'the two bindings that answers with no session, which is the state the screen is opened in',
  );
});

// ---------------------------------------------------------------------------
// Divergence
// ---------------------------------------------------------------------------

test('following the switcher says nothing at all, because there is nothing to say', () => {
  // The normal seat. Never an empty flourish under a control that is already
  // right — and "you are following the switcher" under an option that says
  // "Follow the switcher" is the emptiest flourish there is.
  const live = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SESSION };
  for (const target of [live, null, { source: CONFORM_SOURCE_OVERRIDE }]) {
    const match = deriveFormatMatch(FOLLOW_SWITCHER, target);
    assert.equal(match.state, FORMAT_MATCH.FOLLOW);
    assert.equal(match.line, '');
    assert.equal(match.diverges, false);
  }
});

test('an override that disagrees with the running feed is called a divergence', () => {
  // Every source feeding an instance must be produced in the instance's format;
  // one that is not is REFUSED. So this is not a preference the operator has
  // expressed, it is a feed that will not be accepted — and they have to be able
  // to see that without knowing the rule.
  const live = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SESSION };
  const match = deriveFormatMatch('1280x720p50', live);
  assert.equal(match.state, FORMAT_MATCH.DIVERGE);
  assert.equal(match.diverges, true, 'the caller marks the row off this flag');
  assert.match(match.line, /DIVERGES/, 'and the word is the first thing on the line');
  assert.match(match.line, /1920x1080p50/, 'it must name what the switcher actually is');
  assert.match(
    match.line,
    /will not be accepted/i,
    'and the consequence, because "diverges" alone is a fact with no stake attached',
  );
  // Short enough to be read at a glance. The rule it is derived from is argued
  // in videoformat.js; this is the alarm, not the explanation.
  assert.ok(match.line.length < 120, `the divergence line is ${match.line.length} characters`);
});

test('an override that agrees is confirmed, and marks nothing', () => {
  const live = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SESSION };
  const match = deriveFormatMatch('1920x1080p50', live);
  assert.equal(match.state, FORMAT_MATCH.MATCH);
  assert.equal(match.diverges, false, 'a matching override is not a fault and must not be marked');
  assert.match(match.line, /1920x1080p50/);
});

test('an override with nothing to check it against is neither confirmed nor blamed', () => {
  // Before START there is no independent number — see describeConformTarget.
  // Claiming a match would be the invented confirmation; claiming a divergence
  // would be a red mark on a correctly configured seat an hour before kick-off.
  // The honest third answer is that it is overriding and nothing has been read.
  for (const target of [
    null,
    { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_OVERRIDE },
    { width: 0, height: 0, frameRate: 0, source: CONFORM_SOURCE_SESSION },
  ]) {
    const match = deriveFormatMatch('1280x720p50', target);
    assert.equal(match.state, FORMAT_MATCH.UNKNOWN, `${JSON.stringify(target)} is not evidence`);
    assert.equal(match.diverges, false);
    assert.match(match.line, /Overriding/i);
  }
});

test('whitespace around an override cannot fake a divergence', () => {
  // collectConfig trims, so the stored value never has any — but the CONTROL is
  // read directly by the renderer, and a value that arrived from a preset or a
  // hand-edited file passes through the select untouched.
  const live = { width: 1920, height: 1080, frameRate: 50, source: CONFORM_SOURCE_SESSION };
  assert.equal(deriveFormatMatch('  1920x1080p50 ', live).state, FORMAT_MATCH.MATCH);
});

// ---------------------------------------------------------------------------
// The form's half
// ---------------------------------------------------------------------------

test('the Settings form draws the readout and marks the row when it diverges', () => {
  // DIVERGENCE IS MARKED AS WELL AS WORDED, for the reason the unstartable
  // video-source case is: a sentence on a form two screenfuls deep is not
  // findable, and the operator this is for does not know there is a rule to look
  // for. The line says what is wrong; the class is what makes them look.
  const js = ui('settings.js');
  const render = js.slice(js.indexOf('function renderVideoFormat()'));
  const body = render.slice(0, render.indexOf('\n  }'));
  assert.ok(body.length > 0, 'settings.js must define renderVideoFormat');
  assert.match(body, /deriveFormatMatch\(videoFormatSelect\.value, conformTarget\)/);
  assert.match(body, /describeConformTarget\(conformTarget\)/, 'the switcher must be shown always');
  assert.match(body, /classList\.toggle\('field--diverges', match\.diverges\)/);

  // Redrawn from the control's own event AND from populate, because assigning a
  // <select>'s value from script fires neither 'input' nor 'change'.
  assert.match(js, /videoFormatSelect\.addEventListener\('change', renderVideoFormat\)/);
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('function refreshSecretBadges'));
  assert.match(populate, /renderVideoFormat\(\)/);

  // And the target is actually fetched, on open, through the binding that
  // already exists. It never throws — null is its answer for every way of not
  // knowing — so a try/catch here would be guarding nothing.
  assert.match(js, /conformTarget = await backend\.getConformTarget\(\)/);
  const open = js.slice(js.indexOf('async function open()'));
  assert.match(open, /await refreshConformTarget\(\)/);

  // main.css has to be able to draw both marks, or the classes are no-ops.
  const sheet = read(here, '..', 'styles', 'main.css');
  for (const selector of ['.video-format-note', '.field--diverges']) {
    assert.ok(sheet.includes(selector), `main.css must style ${selector}`);
  }
});
