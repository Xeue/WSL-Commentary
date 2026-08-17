/**
 * Tests for the MAIN SCREEN'S LAYOUT: the two columns, and the one property the
 * whole arrangement exists to hold.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * The operator, verbatim:
 *
 *   "We should move the errors/alerts to not be banners above and bellow that
 *    cause layout shifts. For the comentators watching a live match, anything
 *    casuing their video to move is a massive no."
 *
 * There were two banners, .error-banner above the picture and
 * .status-unavailable-banner below it, both in normal document flow with
 * margins. Every appearance and every dismissal reflowed the page and shoved the
 * programme picture — mid-sentence, while a match was going out.
 *
 * The fix is structural rather than cosmetic: a permanent, fixed-width column
 * beside the picture holds the alerts and the tray, so there is nothing to
 * shift. That is a property of a handful of CSS declarations and of where
 * home.js appends four elements, and a regression in any of them neither throws
 * nor shows up as a broken test elsewhere — it shows up as a commentator losing
 * their picture. So each load-bearing piece is asserted here by name.
 *
 * ======================= WHY THIS READS SOURCE ==============================
 *
 * package.json is frozen: no jsdom, no computed styles, no layout engine. This
 * is the same class of evidence settingslayout.test.js already uses for the
 * Settings form, and the same class settings.test.js uses when it reads
 * internal/config/config.go to prove two languages agree.
 *
 * It is NOT the only evidence. frontend/tools/measure-home.mjs drives a real
 * browser over this exact stylesheet and reports .pgm-tile's measured rectangle
 * with the column empty, with one alert, with ten alerts and with the tray
 * scrolled — the numbers the acceptance for this change asks for. That tool
 * needs a browser on the machine, so it cannot be a unit test; this file is what
 * runs everywhere and what fails in review.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');
/**
 * normaliseCombinators makes selector matching immune to the formatter.
 *
 * Prettier (and the operator's editor) rewrite `.a > .b` to `.a>.b`, which is
 * the same selector and a different string — and every assertion here that
 * looks a selector up by text broke on a reformat that changed no CSS at all.
 * A test that fails when someone runs a formatter is a test that trains people
 * to ignore it, so the sheet is normalised once, here, and the assertions go on
 * being written the readable way.
 */
function normaliseCombinators(text) {
  return (
    text
      .replace(/\s*([>+~])\s*/g, ' $1 ')
      // ...but not immediately after an opening paren. `:has(>#f-x)` would
      // otherwise normalise to `:has( > #f-x)`, and the selectors that use a
      // child combinator inside :has() are matched by their own regex.
      .replace(/\(\s+/g, '(')
  );
}

const sheet = normaliseCombinators(
  readFileSync(join(here, '..', 'styles', 'main.css'), 'utf8'),
);
const home = ui('home.js');
const app = ui('app.js');

/** stripComments removes CSS comments, so prose ABOUT a selector is not the selector. */
function stripComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, '');
}

const bareSheet = stripComments(sheet);

/**
 * rule(selector) returns the declaration block of the first rule whose selector
 * list contains it — searched with COMMENTS STRIPPED, because this stylesheet
 * documents its traps in prose beside the rules that avoid them and prose that
 * names a selector is not that selector.
 */
function rule(selector) {
  const i = bareSheet.indexOf(selector);
  if (i < 0) return '';
  return bareSheet.slice(i, bareSheet.indexOf('}', i));
}

/** codeOnly strips JS comments, so prose about an element is not the element. */
function codeOnly(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

// ---------------------------------------------------------------------------
// The column cannot change width, so it cannot change the picture
// ---------------------------------------------------------------------------

test('the column has a CONSTANT flex-basis and cannot grow or shrink', () => {
  const r = rule('.home-rail {');
  assert.ok(r, 'main.css must style .home-rail');
  assert.match(
    r,
    /flex:\s*0 0 var\(--rail-w\)/,
    'flex-grow AND flex-shrink must both be zero with a named constant basis. ' +
      '`flex: 0 1 auto` or a bare width would let the CONTENT decide the width, and one alert ' +
      'with a long sentence in it would then narrow the picture — the original bug, sideways',
  );
  assert.match(
    stripComments(sheet),
    /--rail-w:\s*\d/,
    ':root must define --rail-w as a length, not derive it from anything',
  );
  assert.equal(
    /--rail-w:\s*(auto|fit-content|max-content|min-content)/.test(stripComments(sheet)),
    false,
    'an intrinsic width is a content-sized column by another name',
  );
});

test('the column scrolls itself, and never horizontally', () => {
  const r = rule('.home-rail {');
  assert.match(r, /overflow-y:\s*auto/, 'the column must take its own scrollbar');
  assert.match(
    r,
    /overflow-x:\s*hidden/,
    'a horizontal scrollbar means something inside is wider than the column, which is the ' +
      'shape of a child that would have pushed if it could',
  );
});

test('long alert text wraps inside the column rather than pushing it', () => {
  const r = rule('.alert-text {');
  assert.ok(r, 'main.css must style .alert-text');
  assert.match(
    r,
    /overflow-wrap:\s*anywhere/,
    'an unbroken token — a device GUID, a URL — sets an intrinsic minimum width. The basis ' +
      'being a constant already stops that reaching the picture; this is the second defence, ' +
      'and both are cheap',
  );
  assert.match(r, /min-width:\s*0/, 'without it a flex child refuses to shrink below its content');
});

test('the main column can shrink, so a wide control cannot push the layout', () => {
  const r = rule('.home-main {');
  assert.ok(r, 'main.css must style .home-main');
  assert.match(r, /flex:\s*1 1 auto/);
  assert.match(
    r,
    /min-width:\s*0/,
    'a <select> holding a long device name would otherwise set the flex base size and push the ' +
      'column off the right-hand edge',
  );
});

// ---------------------------------------------------------------------------
// The column never becomes a banner
// ---------------------------------------------------------------------------

test('the two columns never wrap, at any width', () => {
  const r = rule('.home-body {');
  assert.ok(r, 'main.css must style .home-body');
  assert.match(r, /flex-direction:\s*row/);
  assert.match(
    r,
    /flex-wrap:\s*nowrap/,
    'a column that wraps to a stacked banner at 900px has reintroduced the whole problem on the ' +
      'machine most likely to be a laptop at a venue — and reintroduced it mid-drag',
  );
});

test('no media query turns the column into a row, or hides it', () => {
  // The one breakpoint that exists changes a WIDTH. A rule that changed
  // .home-body's direction, or that hid the column, would be the stacked banner
  // arriving by another route.
  const blocks = [];
  let idx = 0;
  const text = stripComments(sheet);
  while ((idx = text.indexOf('@media', idx)) !== -1) {
    const open = text.indexOf('{', idx);
    if (open < 0) break;
    let depth = 1;
    let j = open + 1;
    while (depth > 0 && j < text.length) {
      if (text[j] === '{') depth += 1;
      else if (text[j] === '}') depth -= 1;
      j += 1;
    }
    blocks.push(text.slice(idx, j));
    idx = j;
  }
  for (const block of blocks) {
    assert.equal(
      /\.home-body[^{]*\{[^}]*flex-direction/.test(block),
      false,
      'a breakpointed flex-direction is how a column becomes a banner',
    );
    assert.equal(
      /\.home-rail[^{]*\{[^}]*display:\s*none/.test(block),
      false,
      'hiding the column at a width hides the alerts with it',
    );
  }
});

test('the collapsed column is a narrow strip, never zero and never absent', () => {
  assert.match(
    stripComments(sheet),
    /--rail-w-collapsed:\s*[0-9.]+[a-z]/,
    'the collapsed width must be a real length: an alert nobody can see because the column was ' +
      'folded away is an alert that did not happen',
  );
  assert.equal(
    /--rail-w-collapsed:\s*0[^.0-9a-z]/.test(stripComments(sheet)),
    false,
    'zero would remove the way back and the attention count with it',
  );
});

// ---------------------------------------------------------------------------
// Nothing that can appear is in the main column
// ---------------------------------------------------------------------------

test('the main column holds the picture stage and the match bar, and nothing else', () => {
  const src = codeOnly(home);
  assert.match(
    src,
    /homeMain\.append\(pgmStage, matchBar\)/,
    'anything else appended to .home-main is something that can change the picture geometry',
  );
  assert.match(src, /homeBody\.append\(homeMain, rail\)/, 'the body is exactly the two columns');
  assert.match(
    src,
    /el\.append\(header, homeBody, audioEl\)/,
    'the view is the topbar, the two columns, and the hidden <audio>. A fourth child in normal ' +
      'flow is a banner whatever it is called',
  );
});

test('the alert feed is built into the column and into nothing else', () => {
  const src = codeOnly(home);
  assert.match(src, /alertsRegion\.className = 'rail-alerts'/);
  assert.match(
    src,
    /rail\.append\(\s*railHeader,\s*alertsRegion,/,
    'the alerts must be a child of the column, above the tray sections',
  );
  assert.equal(
    /homeMain\.append[^)]*alerts/i.test(src),
    false,
    'an alert element in the main column is a banner with a new class name',
  );
});

test('the alert list is always rendered, empty included', () => {
  const src = codeOnly(home);
  assert.match(
    src,
    /alertsList\.appendChild\(alertsEmpty\)/,
    '"No alerts" must be a ROW. An empty state that collapses moves everything below it when the ' +
      'first alert arrives, and the operator learns the column by watching it jump',
  );
});

test('the match bar is a fixed height, and everything in it is one line', () => {
  const r = rule('.match-bar {');
  assert.ok(r, 'main.css must style .match-bar');
  assert.match(
    r,
    /height:\s*var\(--match-bar-h\)/,
    '.pgm-tile is sized from the height left in .pgm-stage, so a bar that can grow a line ' +
      'SHORTENS the picture — the same defect as a banner, wearing a control\'s clothes',
  );
  assert.match(r, /flex-wrap:\s*nowrap/, 'wrapping is how a fixed height becomes a clipped control');
  assert.match(
    stripComments(sheet),
    /--match-bar-h:\s*\d/,
    ':root must define --match-bar-h as a length',
  );

  const detail = rule('.overall-detail {');
  assert.match(
    detail,
    /white-space:\s*nowrap/,
    'the indicator names the lamp its verdict came from, and that line must never wrap to two',
  );
  assert.match(detail, /text-overflow:\s*ellipsis/, 'so it has to ellipsise instead');
});

// ---------------------------------------------------------------------------
// The banners are gone, not merely unused
// ---------------------------------------------------------------------------

test('neither banner element is built any more', () => {
  const src = codeOnly(home);
  for (const gone of ['error-banner', 'status-unavailable-banner', 'error-history']) {
    assert.equal(
      src.includes(gone),
      false,
      `home.js still builds .${gone}. Both banners were in normal document flow with margins; ` +
        'a surviving one is a layout shift waiting for the state that raises it',
    );
  }
  assert.equal(
    /setStatusUnavailable/.test(src),
    false,
    'the setter is withdrawn with the element, so app.js cannot call something that silently does nothing',
  );
});

test('the stylesheet no longer styles either banner', () => {
  const css = stripComments(sheet);
  for (const gone of ['.error-banner', '.status-unavailable-banner', '.error-history']) {
    assert.equal(
      css.includes(gone),
      false,
      `main.css still declares ${gone}: an orphaned rule is how a deleted element comes back ` +
        'looking correct',
    );
  }
});

test('app.js does not raise the switcher-status banner by any route', () => {
  const src = codeOnly(app);
  assert.equal(
    /setStatusUnavailable/.test(src),
    false,
    'the orange banner said a fourth time what the three lamps already say in glyph, text and ' +
      'colour — and it said it in the one form that reflowed the page',
  );
  // The lamps themselves are UNTOUCHED: staleness must still grey all three.
  assert.match(
    src,
    /deriveStatusLamps\(currentStatus, currentConformTarget\)/,
    'the lamp derivation must still run — this change removed a banner, not the honesty',
  );
});

// ---------------------------------------------------------------------------
// Collapsing is an operator action and only that
// ---------------------------------------------------------------------------

test('nothing can collapse the column except the two buttons', () => {
  const src = codeOnly(home);
  const writes = [...src.matchAll(/railCollapsed\s*=\s*(\w+)/g)];
  assert.equal(
    writes.length,
    3,
    'the declaration and exactly two writers. Every additional assignment is another thing that ' +
      'can resize the picture',
  );
  assert.match(src, /let railCollapsed = false;/, 'the first is the declaration, and it starts open');
  // The other two must each be inside a click handler. A collapse reachable
  // from an EVENT — a status arriving, an alert being raised — would be an
  // alert resizing the picture with extra steps.
  for (const w of writes.slice(1)) {
    const before = src.slice(Math.max(0, w.index - 140), w.index);
    assert.match(
      before,
      /addEventListener\('click'/,
      `${w[0]} is not inside a click handler: only the operator's own hand may move the picture`,
    );
  }
  assert.equal(
    /setRailCollapsed|collapseRail|setCollapsed/.test(src),
    false,
    'there must be no setter for it on the returned view: app.js must not be able to reach it',
  );
  const ret = src.slice(src.lastIndexOf('return {'));
  assert.equal(
    /collaps/i.test(ret),
    false,
    'and nothing collapse-shaped may be handed out',
  );
});

// ---------------------------------------------------------------------------
// The tray, and what is left in the main area
// ---------------------------------------------------------------------------

test('what is CONSULTED is in the column', () => {
  // "During the match we only really need an overall status, single green/red
  // indicator and the cough mute buttons. The rest can live in some form of
  // settings tray or something like that."
  //
  // The first cut of this took "the rest" literally and swept the meters and
  // START in with the settings. The operator moved both back on sight, and the
  // line the corrections draw is not main-area-versus-tray but CONSULTED versus
  // WATCHED: the column holds what you go and look at, the main area holds what
  // you keep in your eye.
  const src = codeOnly(home);
  const railAppend = src.slice(src.indexOf('rail.append('), src.indexOf(');', src.indexOf('rail.append(')));
  for (const [what, token] of [
    ['the six lamps', 'lampsEl'],
    ['the device and return controls', 'controls'],
    ['the picture selector', 'sourceGroup'],
    ['the preset picker', 'presetIndicator'],
  ]) {
    assert.ok(railAppend.includes(token), `${what} must be in the column (${token})`);
  }
  for (const [what, token, why] of [
    ['the input meters', 'metersEl',
      'watched continuously for the whole match — "the meters should still be next to the ' +
      'preview and not in the settings sidebar"'],
    ['START/STOP', 'startStopBtn',
      'it belongs beside the verdict that prompts it — "start should still be in the footer ' +
      'next to the CHeck status"'],
  ]) {
    assert.equal(
      railAppend.includes(token),
      false,
      `${what} must NOT be in the column: ${why}`,
    );
  }
});

test('the match bar is the indicator, START, and the cough controls', () => {
  const src = codeOnly(home);
  assert.match(
    src,
    /matchBar\.append\(overallEl, startStopBtn, coughEl\)/,
    'the verdict, the act it prompts, and the mute — in that reading order',
  );
  // And it must still not grow. The bar's fixed height is what stops any of
  // this moving the picture, and START is 64px inside an 84px bar.
  const bar = rule('.match-bar');
  assert.match(bar, /height:\s*var\(--match-bar-h\)/, 'the bar stays a fixed height');
  assert.match(bar, /flex-wrap:\s*nowrap/, 'and never wraps to a second line');
});

test('picture, meters, preview — one row, in that order', () => {
  const src = codeOnly(home);
  assert.match(
    src,
    /pgmStage\.append\(pgmTile, metersEl, previewTile\)/,
    'one stack beside the picture, so the meters are there whether or not the preview is',
  );
  assert.match(src, /pgmStage\.append\(pgmTile, metersEl, previewTile\)/);

  // Neither may take width from the picture on its own initiative.
  const preview = rule('.preview-tile');
  assert.match(preview, /flex:\s*0 0 auto/, 'the preview is fixed to its content');
  const meters = rule('.input-meters');
  assert.ok(meters, 'main.css must style .input-meters');
  assert.match(meters, /flex:\s*0 0 auto/, 'the meters must not grow into the picture');

  // "the metering should match the height of the big monitor" — so the meters
  // compute the SAME height the tile does, from the same two constraints and
  // the same ratio, rather than stretching to the stage and standing a little
  // taller than the picture they sit beside.
  assert.match(
    meters,
    /height:\s*min\(100cqh, calc\(100cqw \/ var\(--tile-ar-num/,
    'the meters must derive the tile height, not the stage height',
  );
  assert.ok(
    !/align-self:\s*stretch/.test(meters),
    'stretching is what made them taller than the picture',
  );

  // And the ratio has to reach them: a custom property set on .pgm-tile is
  // readable by the tile and its descendants only, and the meters are its
  // sibling.
  assert.match(
    codeOnly(home),
    /pgmStage\.style\.setProperty\('--tile-ar-num'/,
    'applyCrop must publish the ratio on the stage, not only on the tile',
  );
});

test('the muted state is drawn with an outline, which costs no layout', () => {
  const r = rule('.view-home.is-muted .pgm-tile');
  assert.ok(r, 'main.css must mark the picture while the commentary is muted');
  assert.match(r, /outline:\s*\d/, 'an outline is not part of the box model');
  assert.equal(
    /border(-\w+)?:\s*\d/.test(r),
    false,
    'a border here would change .pgm-tile\'s content box and move the video by its own width — ' +
      'the one thing this screen may not do',
  );
  assert.equal(
    /box-shadow:[^;]*inset/.test(r),
    false,
    'an inset shadow is inside the rectangle the opaque native SRT window covers, so it would be ' +
      'invisible in exactly the case that matters most',
  );
});
