/**
 * Tests for the Settings screen's LAYOUT: the per-group grids, the sticky
 * action bar, and the CSS<->JS coupling both of them introduce.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * The form used to be a 640px single column pinned to the left of an 1100px
 * box: a 2560px window spent ~1,900px on nothing while the form ran ~3,500px
 * deep, with "‹ Back" and "Save settings" scrolled out of reach. The fix is a
 * grid per <section class="settings-group">, a 1500px view cap, and a sticky
 * footer — and every load-bearing property of that fix lives in CSS, where a
 * regression neither throws nor shows in a diff:
 *
 *   - all six group separators vanishing at once, because the old
 *     `h2:first-of-type` selector is trivially true inside one-section-per-h2;
 *   - the sticky footer going translucent, so the form scrolls visibly
 *     through the Save button (mixer.css:407-421 records that exact trap);
 *   - `auto-fit` collapsing empty tracks, so a field's width depends on how
 *     many siblings its group happens to have;
 *   - a full-width field id renamed in settings.js, silently unhooking the
 *     `.field:has(> #f-…)` rule that gives its ~450-character hint room.
 *
 * ======================= WHY THIS READS SOURCE ==============================
 *
 * package.json is frozen: no jsdom, no computed styles. Asserting stylesheet
 * and module source text is the same class of evidence settings.test.js
 * already uses when it reads internal/config/config.go to prove two languages
 * agree. The regexes tolerate whitespace but deliberately NOT reformatting of
 * the declarations they name — a rule that moves is a rule someone touched.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = (name) => readFileSync(join(here, name), 'utf8');
const css = () => readFileSync(join(here, '..', 'styles', 'main.css'), 'utf8');

const js = ui('settings.js');
const sheet = css();

/** rule(name) returns the declaration block of the first rule whose selector list contains `name`. */
function rule(sheetText, selector) {
  const i = sheetText.indexOf(selector);
  if (i < 0) return '';
  return sheetText.slice(i, sheetText.indexOf('}', i));
}

/** stripComments removes CSS comments, so prose ABOUT a selector is not the selector. */
function stripComments(sheetText) {
  return sheetText.replace(/\/\*[\s\S]*?\*\//g, '');
}

/** mediaBlocks returns every @media block in full, nested braces balanced. */
function mediaBlocks(sheetText) {
  const blocks = [];
  let idx = 0;
  while ((idx = sheetText.indexOf('@media', idx)) !== -1) {
    const open = sheetText.indexOf('{', idx);
    if (open < 0) break;
    let depth = 1;
    let j = open + 1;
    while (depth > 0 && j < sheetText.length) {
      if (sheetText[j] === '{') depth += 1;
      else if (sheetText[j] === '}') depth -= 1;
      j += 1;
    }
    blocks.push(sheetText.slice(idx, j));
    idx = j;
  }
  return blocks;
}

// ---------------------------------------------------------------------------
// The group sections in settings.js
// ---------------------------------------------------------------------------

test('every h2 on the Settings form opens a group section', () => {
  assert.match(js, /openGroup\(connectionHeading\)/);
  assert.match(js, /openGroup\(srtHeading\)/);
  assert.match(js, /openGroup\(statusHeading\)/);
  assert.match(js, /openGroup\(monitorHeading\)/);
  assert.match(js, /openGroup\(slateHeading\)/);
  assert.equal(
    /form\.appendChild\(\w+Heading\)/.test(js),
    false,
    'a heading appended straight to the form has no grid to span, so its group ' +
      'lays out as one column while the others do not',
  );
});

test('the field-construction calls keep the text and the order three other tests read by offset', () => {
  // THE GUARD FOR THE GUARDS.
  const why =
    'settings.test.js reads settings.js by literal text and byte offset. ' +
    'Re-indenting or reordering a field call breaks tests whose names are about ' +
    'SRT encryption and picture latency, so the failure lands three files away ' +
    'from the change that caused it. Layout is done in CSS; the DOM order stays.';
  assert.ok(js.includes("addField(\n    'srtReturnPort',"), why);
  assert.ok(js.includes("addField(\n    'pictureLatencyMs',"), why);

  const monitor = js.indexOf("monitorHeading.textContent = 'Monitor'");
  const port = js.indexOf("addField(\n    'srtReturnPort',");
  const latency = js.indexOf("addField(\n    'pictureLatencyMs',");
  const encryption = js.indexOf("returnEncryptionHeading.textContent = 'SRT return encryption'");
  const slate = js.indexOf("slateHeading.textContent = 'Slate'");
  assert.ok(monitor > 0 && port > 0 && latency > 0 && encryption > 0 && slate > 0, why);
  assert.ok(port > monitor, why);
  assert.ok(port < encryption, why);
  assert.ok(latency > port, why);
  assert.ok(latency < encryption, why);
  assert.ok(encryption > monitor && encryption < slate, why);
});

test('the h3 sub-headings stay inside the Monitor group', () => {
  // An h3 that opened its own section would draw a border between the return
  // port and the encryption controls it decides the answer for.
  assert.match(js, /currentGroup\.appendChild\(returnEncryptionHeading\)/);
  assert.match(js, /currentGroup\.appendChild\(tileHeading\)/);
  assert.equal(
    /openGroup\(returnEncryptionHeading/.test(js),
    false,
    'the SRT return encryption h3 must not open its own section',
  );
  assert.equal(
    /openGroup\(tileHeading/.test(js),
    false,
    'the monitor tile h3 must not open its own section',
  );
});

test('nothing is appended straight to the form except a group or the footer', () => {
  // A field appended to the form sits outside every grid, full width and
  // unaligned with the column it belongs beside.
  const appends = [...js.matchAll(/\bform\.append(?:Child)?\(([^)]*)\)/g)].map((m) => m[1].trim());
  assert.ok(appends.length > 0, 'settings.js must still build the form by appending to it');
  for (const arg of appends) {
    assert.ok(
      arg === 'section' || arg === 'footer',
      `form.append(${arg}): a node appended straight to the form sits outside ` +
        'every grid, full width and unaligned with the column it belongs beside',
    );
  }
});

// ---------------------------------------------------------------------------
// The scroll container and the sticky footer
// ---------------------------------------------------------------------------

test('the action bar is sticky and opaque', () => {
  const f = rule(sheet, '.settings-footer');
  assert.ok(f, 'main.css must style .settings-footer');
  assert.match(f, /position:\s*sticky/);
  assert.match(f, /bottom:\s*0/);
  assert.match(f, /background:\s*var\(--bg\)/);
  assert.equal(
    /rgba\(|transparent/.test(f),
    false,
    'mixer.css:407 records this exact failure: a sticky element with a ' +
      'translucent background is not a header, it is a window, and the form ' +
      'scrolls visibly through it',
  );
});

test('the form is the scroll container, so Back and Save never scroll away', () => {
  const view = rule(sheet, '.view-settings');
  assert.ok(view, 'main.css must style .view-settings');
  assert.match(view, /overflow:\s*hidden/);
  assert.equal(
    /overflow-y:\s*auto/.test(view),
    false,
    'the view scrolling means the topbar and the action bar scroll with it',
  );
  const form = rule(sheet, '.settings-form');
  assert.ok(form, 'main.css must style .settings-form');
  assert.match(form, /overflow-y:\s*auto/);
  assert.match(form, /min-height:\s*0/, 'without min-height: 0 the flex child cannot shrink, and the footer is pushed off the bottom');
  assert.match(form, /flex:\s*1 1 auto/);
});

test('a focused field cannot hide behind the sticky footer', () => {
  const form = rule(sheet, '.settings-form');
  assert.match(
    form,
    /scroll-padding-bottom/,
    'displayErrors focuses the first bad field, and without scroll padding the ' +
      'browser may scroll it flush to the bottom edge, under the footer — a ' +
      'focus ring on a field the operator cannot read',
  );
});

test('the group separators survive the section wrappers', () => {
  assert.ok(
    sheet.includes('.settings-form > .settings-group:first-of-type h2'),
    'the first heading on the form must still be the one without a border',
  );
  // Checked with comments stripped: the trap is documented in prose beside the
  // fixed rule, and prose about the selector is not the selector.
  assert.equal(
    /\.settings-form h2:first-of-type/.test(stripComments(sheet)),
    false,
    'inside one section each, EVERY h2 is :first-of-type among its siblings, ' +
      'so this selector removes all six group separators at once',
  );
});

// ---------------------------------------------------------------------------
// The group grid
// ---------------------------------------------------------------------------

test('a group with one field keeps that field one column wide', () => {
  const g = rule(sheet, '.settings-group');
  assert.ok(g, 'main.css must style .settings-group');
  assert.match(g, /auto-fill/);
  assert.equal(
    /auto-fit/.test(g),
    false,
    'auto-fit collapses the empty tracks, so the Status group\'s single "cam7" ' +
      'box stretches to the full 1460px while the four-field groups show 350px boxes',
  );
});

test('the column count is derived from the window, not from a breakpoint', () => {
  const g = rule(sheet, '.settings-group');
  assert.match(g, /repeat\(auto-fill, minmax\(/);
  for (const block of mediaBlocks(sheet)) {
    assert.equal(
      block.includes('.settings-group'),
      false,
      'a breakpointed column count flickers when the operator drags the window ' +
        'and needs re-tuning every time a field is added',
    );
  }
});

test('no settings item spans a fixed number of tracks', () => {
  assert.equal(
    /grid-column:\s*span/.test(sheet),
    false,
    'a span wider than the track count does not clamp to it, it adds implicit ' +
      'columns — at the 800px minimum window a span-2 item silently makes a ' +
      'two-column grid of 380px tracks',
  );
});

test('the widest fields are named by ids settings.js actually creates', () => {
  // Closes the CSS<->JS coupling introduced by selecting wide fields by id.
  const ids = [...sheet.matchAll(/\.field:has\(> #(f-[A-Za-z0-9]+)\)/g)].map((m) => m[1]);
  assert.ok(ids.length >= 4, 'the full-width field list must still be in main.css');
  // Every constructor settings.js has. The list grew when the video-source
  // control arrived: a <select> whose hint names both options and what each
  // costs, and the one tick box on the form, whose hint is the measured reason
  // the preview cannot be switched live. Both are full-row for their PROSE,
  // exactly as the four text and number fields are, so both belong to this
  // coupling — and leaving the list at two constructors would have meant either
  // a rule that matches nothing or a hint in an 18rem track.
  for (const id of ids) {
    assert.ok(
      js.includes(`textInput('${id}'`) ||
        js.includes(`numberInput('${id}'`) ||
        js.includes(`selectInput(\n      '${id}'`) ||
        js.includes(`selectInput('${id}'`) ||
        js.includes(`checkboxInput('${id}'`),
      `main.css marks #${id} full-width but settings.js never creates it: the ` +
        'rule matches nothing and the long hints silently return to a 350px track',
    );
  }
});

test('the Devices group is gone: device selection is the main screen, only', () => {
  // The group used to exist as three free-text GUID fields with a 27rem track
  // wide enough for a WASAPI endpoint id. The operator removed it: "drop the
  // devices ID selection entirely from that page, that should be solely done
  // from the main page" — and the free-text route was how a playback GUID
  // once reached wasapi2src. The fields survive as HIDDEN inputs so a save
  // round-trips them (collectConfig replaces the whole document), which is
  // what the second and third assertions pin: gone from the SCREEN must never
  // come to mean gone from the SAVE.
  assert.equal(
    /openGroup\(devicesHeading/.test(js),
    false,
    'no Devices group may be opened; device selection belongs to the main screen',
  );
  assert.match(
    js,
    /addHiddenField\('audioDeviceId'/,
    'audioDeviceId must stay registered as a hidden field or a save deletes it',
  );
  assert.match(
    js,
    /addHiddenField\('headphoneDeviceId'/,
    'headphoneDeviceId must stay registered as a hidden field or a save deletes it',
  );
  // Comments stripped: the rule's tombstone in main.css names the selector in
  // prose, and prose about a selector is not the selector.
  assert.equal(
    /settings-group--devices/.test(stripComments(sheet)),
    false,
    'the orphaned 27rem track rule should go with the group it sized',
  );
});

// ---------------------------------------------------------------------------
// The presets card contract
// ---------------------------------------------------------------------------

test('the preset card has a home', () => {
  // The CSS CONTRACT for the picker + its buttons. The .preset-note caution
  // rule is gone with the note itself (removed at the operator's request).
  for (const selector of ['.settings-group--presets', '.preset-picker', '.preset-actions']) {
    assert.ok(sheet.includes(selector), `main.css must declare ${selector} for the presets UI to land into`);
  }
});

test('the preset preview has a home, and every class settings.js adds is styled', () => {
  // The CSS<->JS coupling the preview introduces. A class settings.js adds and
  // main.css does not style is a preview that draws nothing — the picker would
  // go back to doing nothing visible, which is the whole defect.
  for (const selector of ['.field--preset-preview', '.preset-preview-note', '.preset-preview-summary']) {
    assert.ok(sheet.includes(selector), `main.css must style ${selector}`);
    assert.ok(js.includes(selector.replace(/^\./, '')), `settings.js must actually add ${selector}`);
  }
});

test('the preview is marked by more than its colour', () => {
  // Colour-blind operators, and a gallery projector that washes this screen
  // out. The mark on a previewed control is a DASHED border — a shape — so the
  // green is the fastest signal rather than the only one. The other two are the
  // note's "was X → becomes Y" and the summary line, both in settings.js.
  const marked = rule(sheet, '.field--preset-preview input[type=');
  assert.ok(marked, 'main.css must style a previewed control');
  assert.match(
    marked,
    /border:\s*1px dashed/,
    'a colour-only mark is invisible to a colour-blind operator and to a washed-out projector',
  );
});

test('the preview summary owns the whole row of the instance card', () => {
  // It names every changed field, so at a 350px track it would wrap into a
  // column two words wide. Full row via `1 / -1`, with the other composite
  // items — never `span N`, for the reason the rule beside it records.
  const fullRow = rule(sheet, '.settings-group > .preset-preview-summary');
  assert.match(fullRow, /grid-column:\s*1 \/ -1/);
});
