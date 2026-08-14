// What SELECTING an instance preset shows on the Settings form, before anybody
// presses Apply: one decoration per field the preset would change.
//
// Owner: WP-5b (the presets work package).
//
// NO DOM. This is the house pattern — presets.js/settings.js, validate.js/
// settings.js, events.js/settings.js — and it is the only way this gets driven
// tests: package.json is frozen, there is no jsdom, and a hand-rolled element
// shim widened until a test passes is not evidence (settings.test.js says so at
// length). settings.js owns the elements; this owns the decisions.
//
// ===================== WHY A PREVIEW EXISTS AT ALL ==========================
//
// Applying a preset rewrites most of the form and restarts every monitor. The
// only place an operator could read what it would change used to be inside the
// confirm() dialog — a modal listing thirteen possible rows, read once, in a
// hurry, with the form it describes hidden behind it. The change is legible on
// the FORM now: the box shows the value the preset would put there and the old
// value is stated beside it.
//
// The dialog has since gone entirely (the owner: "we don't need the confirm
// popup now we have the green text"), which is why describePresetPreview's line
// ends with the consequence the dialog used to carry. Nothing that was in that
// modal is now unsaid; it is said without a modal.
//
// ============== THE TWO CASES THAT BREAK A NAIVE VERSION ====================
//
//   - A <select> cannot show two values. pbkeylen, the return key length and
//     the return bus are dropdowns: the box can only be MOVED to the preset's
//     option, so the old value has to be somewhere else — hence a note per row
//     rather than a "from -> to" crammed into the control. And a preset value
//     the control does not offer (a hand-edited pbkeylen of 24) must not be
//     written into the box at all: a <select> silently deselects everything
//     when set to an option it does not have, which would read as "this preset
//     blanks the field" when what it means is "this value is not valid here".
//
//   - A new value that is EMPTY. A preset that clears statusKey is a real
//     change — three lamps go to NO STATUS — and an empty box with nothing else
//     on the row is indistinguishable from a box the preset never mentioned.
//     `cleared` is that case, flagged rather than left for the caller to
//     rediscover by comparing strings.
//
// This module iterates the DIFF, which presets.js built from the whitelist, so
// a hand-edited preset file carrying a device id produces no row here for the
// same structural reason it produces no diff row there: the tags never arrive.

/**
 * boxText renders a preset's RAW field value as a control's value string.
 *
 * Deliberately NOT presets.js's formatValue, which renders "(not set)" for an
 * empty value: that string is prose for a human reading a diff, and writing it
 * into an input would put the words "(not set)" in the operator's box as if
 * they had typed them. The two renderings differ on exactly the case that
 * matters, so they stay separate functions.
 *
 * @param {unknown} raw
 * @returns {string}
 */
function boxText(raw) {
  if (raw === undefined || raw === null) return '';
  if (typeof raw === 'object') return JSON.stringify(raw);
  return String(raw);
}

/**
 * planPresetPreview turns a preset diff into the decorations the form should
 * carry: ordered, one per changed field, each saying what its control should
 * show and what to say beside it.
 *
 * @param {Array<{tag: string, label: string, from: string, to: string}>} diff
 *        diffPreset's output — the WHITELISTED fields that differ, in whitelist
 *        order. The empty array is the normal, quiet case: the active preset,
 *        or one that happens to match the form.
 * @param {object} fields  the preset's raw fields map, read ONLY at tags the
 *        diff already named, so nothing outside the whitelist can be reached
 *        through it.
 * @param {object} controls  tag -> what settings.js has on screen for it:
 *        { box: 'input'|'select'|'none', options?: string[], format?: fn }.
 *        A tag with no entry is treated as 'none' — a whitelisted field that
 *        grew no control must still be ANNOUNCED, because the alternative is a
 *        preset that silently changes something the screen never mentions.
 * @returns {Array<{tag: string, label: string, from: string, to: string,
 *          note: string, boxValue: string|null, cleared: boolean}>}
 *          boxValue is null when nothing may be written into a control:
 *          there is no control, or the control cannot express the value.
 */
export function planPresetPreview(diff, fields, controls) {
  const rows = Array.isArray(diff) ? diff : [];
  const values = fields || {};
  const byTag = controls || {};
  const out = [];

  for (const row of rows) {
    const control = byTag[row.tag] || { box: 'none' };
    const kind = control.box === 'select' || control.box === 'input' ? control.box : 'none';
    const text = typeof control.format === 'function'
      ? control.format(values[row.tag])
      : boxText(values[row.tag]);

    // A <select> that cannot offer the value keeps the value it has; the note
    // then has to say why the box did not move, or the row reads as a preview
    // that failed to draw.
    const offered = kind !== 'select' || (Array.isArray(control.options) && control.options.includes(text));
    const boxValue = kind === 'none' || !offered ? null : text;

    let note = `Preset preview: ${row.from} → ${row.to}`;
    if (kind === 'select' && !offered) {
      note += ' — this control does not offer that value';
    } else if (boxValue === null) {
      // No box of its own: the row still states the change, and the summary
      // line names the field, but nothing on screen will hold the new value
      // until the apply happens. Saying so is cheaper than an operator hunting
      // for a green box that is not there.
      note += ' — applied, but not shown in a box';
    }

    out.push({
      tag: row.tag,
      label: row.label,
      from: row.from,
      to: row.to,
      note,
      boxValue,
      // The empty-new-value case, flagged: an empty box is otherwise
      // indistinguishable from a box the preset never touched.
      cleared: boxValue === '',
    });
  }

  return out;
}

/**
 * describePresetPreview words the one line in the instance card that tells the
 * operator a preview is on screen, WHERE to look, and what pressing Apply costs.
 *
 * It names every changed field, including the ones with no box of their own,
 * because the form is about two screenfuls deep: green boxes below the fold are
 * only findable if something says which fields to go and find. It returns ''
 * for no changes so the caller can `if (msg)` — selecting the active preset, or
 * one that differs in nothing, must leave the screen exactly as it was rather
 * than announce an empty result.
 *
 * ================== THE CLOSING SENTENCE IS NOT DECORATION ==================
 *
 * It is what the confirm dialog used to say, moved here when the dialog was
 * removed — and it is the half of the dialog the diff could not replace. The
 * green boxes say what changes; nothing on the form says that committing them
 * tears down and re-dials the KVS monitor and the SRT picture, which is several
 * seconds of black picture and silence, or that the device fields the operator
 * can see on the main screen are deliberately NOT part of a preset and will not
 * move. An operator who is told the first and not the second reads the black
 * picture as a fault. Brief, because this is a line read at a glance, and last,
 * because it is the consequence rather than the content.
 *
 * @param {string} name  the preset's name, as the operator sees it in the picker
 * @param {Array<{label: string}>} rows  planPresetPreview's output
 * @returns {string}
 */
export function describePresetPreview(name, rows) {
  const list = Array.isArray(rows) ? rows : [];
  if (list.length === 0) return '';
  const labels = list.map((r) => r.label).join(', ');
  return (
    `Applying "${name}" would change ${list.length === 1 ? '1 setting' : `${list.length} settings`}, ` +
    `marked below: ${labels}. Nothing has changed yet — press Apply to commit: the monitor and ` +
    'the picture then reconnect (a few seconds of black and silence), and your input and ' +
    'headphone devices are not part of a preset and stay as they are.'
  );
}
