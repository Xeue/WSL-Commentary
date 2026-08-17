import * as backend from './backend.js';
import { validateConfig } from './validate.js';
// The address field's parser and its formatter, and they are deliberately not
// symmetrical. parseM2LXAddress takes EITHER the instance's base URL (the event
// id then comes from the events API) or a full live-operation URL (which still
// fills both fields); formatM2LXAddress writes back the BASE ADDRESS ONLY —
// see its comment for why the app must never hand back a longer URL than the
// one that was pasted.
import { parseM2LXAddress, formatM2LXAddress } from './liveurl.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
import { normaliseReturnSource, DEVICE_KEY_SRT } from './returnsource.js';
// The instance-preset model: the diff the preview on the form is built from,
// and the whitelist filter that mirrors Go's — which is what tells the operator
// that a hand-edited or foreign preset file carries keys the apply will drop.
// Both are read by renderPresetPreview now; the confirm dialog that used to
// read them is gone, see handleApplyPreset.
import { diffPreset, describeIgnoredKeys, filterPresetFields } from './presets.js';
// What SELECTING a preset draws on this form, decided in a pure module and
// applied to the controls down at renderPresetPreview. See presetpreview.js for
// the two cases that break a naive version of this (a <select>, and a new value
// that is empty).
import { planPresetPreview, describePresetPreview } from './presetpreview.js';
// The pure event auto-select / picker rule. It decides, from the instance's
// event list and the URL-derived id, whether to auto-select a lone event or
// offer a picker; this file turns that answer into the hidden field, a note or a
// <select>. Pure and DOM-free — see events.js and events.test.js.
import { chooseEvent } from './events.js';
// The channel table, from the module that enforces it in the Web Audio graph.
// See the note beside the same import in home.js.
import { CHANNEL_MODES, normaliseChannelMode } from '../monitor/channels.js';
// The routing grid, its per-channel meters and — on a card seat — the video
// lamp that explains a silent one. It is a screen inside this screen: see the
// "Channel routing" group below for why it is a settings group and not a view of
// its own, and channelmap.js's header for why the grid is sized to a WIDTH AND A
// DEVICE and never to a device kind.
//
// inputChannelCount and describeRoutingHeading are imported beside the view
// because the GATE and the HEADING are this screen's half of that: the group is
// shown from the width in the last pad report, and the heading is built from it.
import {
  createChannelMapView,
  inputChannelCount,
  describeRoutingHeading,
} from './channelmap.js';
// WHAT THIS POSITION SENDS, and the operator's confidence picture of it. Pure
// and DOM-free — the wording, the card-present rule and the "which one is going
// to air" sentence all live there and are driven for real in
// videosource.test.js; this file is only the controls they are drawn into.
import {
  VIDEO_SOURCES,
  VIDEO_SOURCE_DECKLINK,
  normaliseVideoSource,
  normalisePreviewEnabled,
  deriveVideoSourceEffects,
  describeToAir,
  describeCardAvailability,
  describeCardOptionRefusal,
  PREVIEW_AT_START_CAVEAT,
  VIDEO_LEG_WHILE_SENDING,
} from './videosource.js';
// THE ONE COMMENTARY-INPUT PICKER. The kind constants, the grouping rule and —
// the part that used to be the operator's job — what SELECTING an entry means
// for all three config fields. Pure and DOM-free; see audioinput.js's header for
// the two-controls-one-question failure it removes.
// AUDIO_SOURCE_NATIVE is deliberately NOT imported: nothing on this screen
// compares against it any more. The one place that used to — the capture-kind
// <select>'s option list — is gone, and the one place that still names a kind
// (currentAudioDeviceKey) asks which of the two ID FIELDS this kind selects,
// which is a different question from what may be shown.
//
// NOTHING ON THIS SCREEN GATES ANYTHING ON THE KIND ANY MORE, and that is the
// change rather than an accident of imports. The routing group used to be
// reachable only from audioSourceKind = decklink; it is now shown for every
// device that negotiates a width, because a stereo pair arriving the wrong way
// round and a mono commentator needing both ears are routing decisions too. See
// renderChannelMapGroup.
import {
  AUDIO_SOURCE_DECKLINK,
  normaliseAudioSourceKind,
  planAudioInputs,
  deriveAudioInputEffects,
  encodeAudioInput,
} from './audioinput.js';
// THE VIDEO FORMAT, as a list of rasters rather than a box to type one into,
// and the switcher's own format to judge it against. Pure and DOM-free — the
// canonical spellings, the "a saved value is never dropped" rule and the
// divergence wording all live there and are driven for real in
// videoformat.test.js.
import {
  planVideoFormats,
  describeConformTarget,
  deriveFormatMatch,
} from './videoformat.js';

// THE MIXER DRAWER IS NOT HERE ANY MORE. It moved to a button beside Settings
// on the main screen, at the operator's request — reaching the clean-feed
// matrix used to mean going through this configuration form. The host side of
// it is its own module now, and app.js is what wires it.
//
// Nothing in this file may build a drawer or reach the mixer write path: one
// write path, one call site, and mixerwiring.test.js proves it by reading this
// file's TEXT — so the forbidden symbols are not named here, even in prose.

// The Settings screen: every field of specification section 9, plus the three
// Credential Manager secrets. Same window, swapped view — there is no
// second window and no menu (spec section 10).
//
// The three secrets are the M2L-X password, the SRT passphrase for the SEND
// path and the SRT passphrase for the RETURN path. The last two are separate
// fields on this form and separate Credential Manager entries, because M2L-X
// sets encryption per endpoint and the two routinely differ — see the note
// beside the "SRT return encryption" group.
//
// Owner: WP-5b.
//
// The device-id fields are REGISTERED but never rendered: device selection
// belongs to the main screen's dropdowns alone (operator's request, and the
// free-text route was how a playback GUID once reached wasapi2src). They live
// in the fields map as hidden inputs so a save round-trips them unchanged —
// collectConfig replaces the whole document, and a field this form does not
// restate is a field a save silently deletes.

// Fallback shape used only if backend.getConfig() itself fails, so the form
// still renders instead of the Settings screen going blank. Mirrors
// internal/config.Defaults().
function blankConfig() {
  return {
    m2lxHost: '',
    alias: '',
    eventId: '',
    srtPort: 0,
    srtLatencyMs: 120,
    pbkeylen: 0,
    videoBitrateKbps: 2000,
    // Empty means DERIVE the format from the switcher, which is the default and
    // the normal case. It is not "unknown" — see the field's hint below.
    videoFormatOverride: '',
    statusKey: '',
    audioDeviceId: '',
    audioSourceKind: 'native',
    decklinkPersistentId: '',
    // ONE ROUTING PER DEVICE, keyed "<capture kind>:<device id>" — exactly the
    // <option> value the commentary-input picker builds (audioinput.js,
    // encodeAudioInput) and exactly the key Go builds
    // (internal/config.AudioDeviceKey). A key spelled two ways is a routing
    // saved where nothing will look for it again.
    //
    // An EMPTY OBJECT, not a default map, and the difference is gst.ChannelMap's:
    // an absent key means "nobody has chosen yet for that device" and RESOLVES
    // to channel 1 left, channel 2 right — bit for bit what every seat carried
    // before this grid existed. Writing the default out here instead would turn
    // "not chosen" into "chosen" on every machine that ever opened Settings,
    // after which nothing could tell an operator whether anyone had set the
    // routing.
    channelMaps: {},
    headphoneDeviceId: '',
    headphoneEndpointId: '',
    returnMid: 4,
    returnChannel: 'stereo',
    returnSource: 'webrtc',
    srtReturnPort: 40501,
    pictureLatencyMs: 120,
    srtReturnPBKeyLen: 0,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
    slatePath: 'slate.png',
  };
}

// AUDIO_SOURCE_NATIVE, AUDIO_SOURCE_DECKLINK and normaliseAudioSourceKind USED
// TO BE DECLARED HERE. They moved to audioinput.js when the two controls that
// asked "which subsystem" and "which device" became one picker: the module that
// decides which kind a selection means is the module that should own the
// spelling of the kinds, and a constant owned by the screen that merely renders
// the answer is a constant two files have to agree about. The cross-language
// pin against internal/config's own constants moved with them.

function row(labelText, id, inputEl, hint) {
  const wrap = document.createElement('div');
  wrap.className = 'field';
  const label = document.createElement('label');
  label.htmlFor = id;
  label.textContent = labelText;
  wrap.appendChild(label);
  wrap.appendChild(inputEl);
  if (hint) {
    const hintEl = document.createElement('p');
    hintEl.className = 'field-hint';
    hintEl.textContent = hint;
    wrap.appendChild(hintEl);
  }
  const errorEl = document.createElement('p');
  errorEl.className = 'field-error';
  errorEl.hidden = true;
  wrap.appendChild(errorEl);
  return { wrap, errorEl };
}

function textInput(id, type = 'text') {
  const input = document.createElement('input');
  input.type = type;
  input.id = id;
  return input;
}

function numberInput(id, step) {
  const input = document.createElement('input');
  input.type = 'number';
  input.id = id;
  if (step) input.step = String(step);
  return input;
}

function selectInput(id, options) {
  const input = document.createElement('select');
  input.id = id;
  for (const opt of options) {
    const o = document.createElement('option');
    o.value = String(opt.value);
    o.textContent = opt.label;
    input.appendChild(o);
  }
  return input;
}

/**
 * fillGroupedSelect rebuilds a <select> from a plan of <optgroup>s and restores
 * the plan's selection. It is the DOM half of planAudioInputs and
 * planVideoFormats, which decide everything about WHAT is in the list; nothing
 * here chooses, orders or words anything.
 *
 * `leading` is an ungrouped option placed above every group. It exists for the
 * video format's "Follow the switcher", which must read as the thing the groups
 * are exceptions to rather than as one entry among seventeen — an <optgroup> of
 * one would file it beside the rasters as an equal.
 *
 * THE SELECTION IS ASSIGNED LAST, after every option exists. A <select> silently
 * discards a value it has no option for, so assigning first — or against a list
 * still being built — is how a saved device ends up showing as device #1.
 *
 * @param {HTMLSelectElement} select
 * @param {{groups: Array<{label: string, options: Array<{value: string, label: string}>}>, value: string}} plan
 * @param {{value: string, label: string}|null} leading
 */
function fillGroupedSelect(select, plan, leading) {
  select.textContent = '';
  const option = (spec) => {
    const o = document.createElement('option');
    o.value = spec.value;
    o.textContent = spec.label;
    return o;
  };
  if (leading) select.appendChild(option(leading));
  for (const group of plan.groups) {
    const g = document.createElement('optgroup');
    g.label = group.label;
    for (const spec of group.options) g.appendChild(option(spec));
    select.appendChild(g);
  }
  select.value = plan.value;
}

/**
 * checkboxInput is the ONE tick box on this form.
 *
 * It exists rather than a two-option <select> because the value it carries is
 * genuinely a yes/no about the operator's own screen, and because collectConfig
 * has to send a real boolean: config.DeckLinkPreviewEnabled is a Go `bool`, and
 * a <select> would hand it the string "false" — which is truthy in JavaScript
 * and would not survive the round trip as anything an operator could then turn
 * off. Read with .checked, never .value.
 */
function checkboxInput(id) {
  const input = document.createElement('input');
  input.type = 'checkbox';
  input.id = id;
  return input;
}

/**
 * createSettingsView builds the Settings screen and returns { el, open() }.
 * open() re-fetches the current configuration and (re)populates the form; it
 * is called every time app.js switches into this view, so the form always
 * reflects whatever was last saved — including changes made from the main
 * screen's device dropdowns.
 *
 * handlers = { onBack(), onSaved(config) }. onSaved is called with the
 * config that was just persisted so app.js can refresh the home screen (tile
 * geometry, the return dropdown, the level default) without a second fetch.
 */
export function createSettingsView(handlers) {
  const el = document.createElement('section');
  el.className = 'view view-settings';
  el.hidden = true;

  const header = document.createElement('header');
  header.className = 'topbar';
  const backBtn = document.createElement('button');
  backBtn.type = 'button';
  backBtn.className = 'btn btn-ghost';
  backBtn.textContent = '‹ Back';
  backBtn.addEventListener('click', () => leaveSettings());
  const title = document.createElement('h1');
  title.textContent = 'Settings';
  header.append(backBtn, title);

  const form = document.createElement('form');
  form.className = 'settings-form';
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    handleSave();
  });

  const fields = {}; // key -> { input, errorEl }

  // Fields with no control on this screen that must survive a Save unchanged.
  // Held between populate() and collectConfig().
  //
  // saveConfig REPLACES the stored object: a field this form does not restate
  // is a field this form DELETES. That is not hypothetical — returnSource
  // decides which path the commentator hears, and silently resetting it to the
  // default because somebody pressed Save on an unrelated screen is a way to
  // change what is in their ears without touching a control that says so.
  let carriedReturnSource = normaliseReturnSource(undefined);

  // EVERY OTHER DEVICE'S CHANNEL ROUTING, carried between populate() and
  // collectConfig() for the same reason returnSource above is: saveConfig
  // replaces the stored object, and this screen only ever holds ONE device's
  // grid — the one the capture pad has negotiated.
  //
  // Without the carry, plugging in a USB microphone to check something and then
  // pressing Save on an unrelated field would delete the card's routing, with a
  // commentator's channel assignment in it, from a screen that was not showing
  // the card at all. collectChannelMaps writes the visible grid OVER this and
  // never instead of it.
  let carriedChannelMaps = {};

  // srtReturnPort USED TO BE CARRIED HERE AND MUST NOT BE AGAIN. It has a real
  // control in the Monitor group below, because carrying it is exactly how it
  // became uneditable: it defaulted to 40503 (src=cln, measured encrypted=true)
  // for one revision, the return dialled it with no passphrase, every handshake
  // was refused in silence — and an operator whose config.json still held 40503
  // had no way to correct it from the application at all. A field with no
  // control is a field nobody can fix.

  // Each h2 opens a <section class="settings-group">: the SECTION is the grid,
  // the form stays a column of sections so the h2 border-top still spans the
  // full measure and still reads as the group separator it already is.
  //
  // Deliberately NOT a startGroup('Monitor') helper, and deliberately not a
  // per-group closure. settings.test.js reads this file's TEXT: three tests
  // match on the literal `addField(\n    '` — end of line, then exactly four
  // spaces — and three more assert the source ORDER of the heading
  // assignments. Wrapping the field calls in anything would re-indent them and
  // break tests that are about SRT encryption, not about layout.
  let currentGroup = form;

  function openGroup(heading, modifier) {
    const section = document.createElement('section');
    section.className = modifier ? `settings-group ${modifier}` : 'settings-group';
    section.appendChild(heading);
    form.appendChild(section);
    currentGroup = section;
    return section;
  }

  function addField(key, labelText, input, hint) {
    const { wrap, errorEl } = row(labelText, input.id, input, hint);
    // The WRAP is kept as well as the input: the preset preview decorates the
    // whole row — a mark on the label's side of the box and a "was X, becomes
    // Y" line under it — and reaching for input.parentElement instead would
    // work for the rows this function builds and quietly return null for the
    // hidden fields, which is the case the preview most needs to detect.
    fields[key] = { input, errorEl, wrap };
    currentGroup.appendChild(wrap);
    return input;
  }

  // --- M2L-X instance (presets) -----------------------------------------
  //
  // FIRST group on the screen, above 'M2L-X connection', because it does not
  // describe the form — it ACTS on it: applying a preset rewrites most of the
  // fields below. main.css's .settings-group--presets card is the contract
  // this hangs off.
  //
  // Availability follows the picture/return pattern: offered against the fake
  // backend (the dev loop is how this UI is iterated on without a Wails
  // build), and against a real build only when ALL SEVEN bindings exist —
  // backend.presetsAvailable() says why a subset is worse than none.
  const presetsSupported = backend.usingFakeBackend || backend.presetsAvailable();

  /** The last config populate() drew, used as the diff baseline for Apply. */
  let lastLoadedConfig = null;
  /** ListPresets' summaries, keyed for the picker. */
  let presetSummaries = [];
  /** GetActivePreset's record, or null before the first refresh. */
  let activePreset = null;
  /** Mirrors app.js's sending state: Apply and Delete are gated on it. */
  let sendingNow = false;

  const presetsHeading = document.createElement('h2');
  presetsHeading.textContent = 'M2L-X instance';
  openGroup(presetsHeading, 'settings-group--presets');

  const presetPicker = document.createElement('div');
  presetPicker.className = 'preset-picker';
  const presetSelect = document.createElement('select');
  presetSelect.id = 'f-preset';
  const presetSelectRow = row('Saved instance', 'f-preset', presetSelect);
  presetPicker.appendChild(presetSelectRow.wrap);

  function presetButton(label, className, onClick) {
    const btn = document.createElement('button');
    btn.type = 'button'; // never submit: this form's submit is Save settings
    btn.className = className;
    btn.textContent = label;
    btn.addEventListener('click', onClick);
    return btn;
  }

  const presetActions = document.createElement('div');
  presetActions.className = 'preset-actions';
  const applyPresetBtn = presetButton('Apply', 'btn btn-primary btn-small', () => handleApplyPreset());
  const savePresetBtn = presetButton('Save current as…', 'btn btn-ghost btn-small', () => handleSavePresetAs());
  const renamePresetBtn = presetButton('Rename', 'btn btn-ghost btn-small', () => handleRenamePreset());
  const deletePresetBtn = presetButton('Delete', 'btn btn-ghost btn-small', () => handleDeletePreset());
  presetActions.append(applyPresetBtn, savePresetBtn, renamePresetBtn, deletePresetBtn);
  presetPicker.appendChild(presetActions);
  currentGroup.appendChild(presetPicker);

  // THE ONE LINE THAT SAYS A PREVIEW IS ON SCREEN, and which fields to go and
  // look at. The form is about two screenfuls deep, so green boxes below the
  // fold are only findable if something names them; this is that something, and
  // describePresetPreview words it.
  //
  // It is also where the confirm dialog's two non-diff facts went when the
  // dialog was removed: the cost of applying (the monitor and the picture
  // reconnect; the device fields are not part of a preset) and the keys a
  // hand-edited file carries that will be dropped. Everything the operator used
  // to have to dismiss a modal to read is now here, in the card, above the
  // fields it points at — see renderPresetPreview.
  //
  // Hidden — never an empty flourish — when the selected preset is the active
  // one, differs in nothing, and carries nothing that will be ignored.
  const presetPreviewLine = document.createElement('p');
  presetPreviewLine.className = 'field-hint preset-preview-summary';
  presetPreviewLine.hidden = true;
  currentGroup.appendChild(presetPreviewLine);

  // The active-instance / credential-scope readout and the "never part of a
  // preset" machine-fields note were both removed at the operator's request —
  // the card is just the picker and its buttons now. The safety they described
  // (a preset can never carry a device id) is still enforced by construction
  // in presets.js and Go's whitelist; it simply no longer narrates itself in
  // the UI.

  function selectedPresetSummary() {
    return presetSummaries.find((p) => p.id === presetSelect.value) || null;
  }

  function renderPresetButtons() {
    if (!presetsSupported) {
      for (const btn of [applyPresetBtn, savePresetBtn, renamePresetBtn, deletePresetBtn]) {
        btn.disabled = true;
        btn.title = 'This build has no instance presets.';
      }
      presetSelect.disabled = true;
      return;
    }
    const none = presetSummaries.length === 0;
    presetSelect.disabled = none;
    savePresetBtn.disabled = false;
    savePresetBtn.title = 'Saves the last SAVED settings as a named instance. Save settings first if you have edited the form.';
    renamePresetBtn.disabled = none;
    renamePresetBtn.title = none ? 'No preset to rename.' : '';
    // Apply and Delete are gated on the sending state, with the reason ON THE
    // CONTROL: applying mid-match would leave the feed going to the previous
    // instance with every lamp green, so Go refuses it — this mirror of that
    // refusal is honesty, not the gate itself.
    const sendingReason = 'Disabled while SENDING: stop the feed before changing instance.';
    applyPresetBtn.disabled = none || sendingNow;
    applyPresetBtn.title = sendingNow ? sendingReason : none ? 'No preset to apply.' : '';
    deletePresetBtn.disabled = none || sendingNow;
    deletePresetBtn.title = sendingNow ? sendingReason : none ? 'No preset to delete.' : '';
  }

  /**
   * refreshPresets re-reads the picker and the active record. Each failure
   * degrades its own line rather than blanking the rest, for the same reason
   * open() loads config and suggestions separately.
   */
  async function refreshPresets() {
    if (!presetsSupported) {
      renderPresetButtons();
      return;
    }
    try {
      const [list, active] = await Promise.all([backend.listPresets(), backend.getActivePreset()]);
      presetSummaries = Array.isArray(list) ? list : [];
      activePreset = active || null;
      const previous = presetSelect.value;
      presetSelect.textContent = '';
      for (const p of presetSummaries) {
        const o = document.createElement('option');
        o.value = p.id;
        o.textContent = p.name;
        presetSelect.appendChild(o);
      }
      // Prefer the active preset; fall back to the previous selection, then
      // the first. The picker showing the applied instance by default is what
      // makes the Rename/Delete buttons act on the thing the operator sees.
      if (activePreset && activePreset.id && presetSummaries.some((p) => p.id === activePreset.id)) {
        presetSelect.value = activePreset.id;
      } else if (previous && presetSummaries.some((p) => p.id === previous)) {
        presetSelect.value = previous;
      }
      // A refresh can move the selection under a preview — deleting or renaming
      // one preset while another is previewed, say. A preview that describes a
      // preset the picker is no longer showing is a set of green boxes with
      // nothing on screen to explain them, so it goes.
      //
      // Read off the LINE and not off presetPreview: the line is the one thing
      // that is always there when anything is (see presetPreviewLineFor), so a
      // preset selected only for its ignored-key warning is covered by the same
      // rule rather than being the case that slips through.
      if (presetPreviewLineFor !== '' && presetPreviewLineFor !== presetSelect.value) {
        clearPresetPreview();
      }
    } catch (err) {
      presetSummaries = [];
      presetSelect.textContent = '';
      setSaveMessage(`Could not list the saved instances: ${err.message}`, true);
    }
    renderPresetButtons();
  }

  async function handleApplyPreset() {
    const preset = selectedPresetSummary();
    if (!preset || typeof handlers.onApplyPreset !== 'function') return;

    // ================ THERE IS NO CONFIRM DIALOG ANY MORE =====================
    //
    // APPLY APPLIES. The modal that stood here asked the question the form now
    // answers by itself, in the owner's words after using the build: "we don't
    // need the confirm popup now we have the green text". It was not merely an
    // extra click — it was drawn OVER the thing it described, so a thirteen-line
    // list read in a hurry was standing in front of the same change marked in
    // green on the very boxes it lands in.
    //
    // THE TWO THINGS IT SAID THAT THE PREVIEW DOES NOT COVER WERE MOVED, NOT
    // DROPPED. Both are on the preset preview summary line in the card above,
    // read at a glance and without dismissing anything:
    //
    //   - the keys a hand-edited or foreign preset FILE carries that a preset
    //     does not honour — filterPresetFields/describeIgnoredKeys, now read by
    //     renderPresetPreview, which is also the only place that can state them
    //     BEFORE the operator commits;
    //   - that applying reconnects the monitor and the picture, and that the
    //     input and headphone devices are not part of a preset and stay as they
    //     are — describePresetPreview's closing sentence.
    //
    // What made the dialog dispensable is that the gate on this action was never
    // the dialog. Go's ApplyPreset REFUSES while SENDING and renderPresetButtons
    // disables this button with that reason on it; off air the cost is a few
    // seconds of black picture and silence, and on air the refusal — not a
    // question nobody reads at speed — is what prevents it.

    // WHERE THE OPERATOR IS, read before anything moves.
    //
    // This is the code that now has to make "Apply leaves you where you were"
    // true, and it could not be judged before: app.js's apply used to end in
    // onConfigSaved, which ends in showHome(), so by the time these lines ran
    // the Settings view was HIDDEN — and a hidden element has no layout, reads
    // scrollTop 0 and takes no assignment. The restore could neither work nor be
    // seen not to. The apply goes through applyConfigLive now and this screen
    // stays on top, so the restore is live.
    //
    // It is still needed with the navigation gone, for two separate reasons:
    // clearing the preview removes its notes, so the form gets SHORTER, and a
    // scroll container whose content shrinks has its scrollTop clamped by the
    // browser — the page jumping under somebody who only pressed a button. And
    // disabling a focused button blurs it, so the focus is put back as well.
    const scrollTop = form.scrollTop;
    const hadFocus = document.activeElement === applyPresetBtn;

    applyPresetBtn.disabled = true;
    try {
      // app.js owns the sequence — apply, adopt the RETURNED config, rebuild
      // the monitors — and hands the merged config back for this form.
      const merged = await handlers.onApplyPreset(preset.id);
      // The preview stops being a preview: these are the values now, so the
      // green goes. Cleared explicitly rather than relying on populate() below,
      // because a handler that returns nothing must still not leave green boxes
      // claiming a change that has already happened.
      //
      // There is now exactly ONE route past this line that leaves the preview
      // standing — a throw, caught below, where nothing moved and the preview is
      // therefore still true. The other one used to be the declined confirm,
      // which returned above it; with the dialog gone, reaching the await at all
      // means the values are committed.
      clearPresetPreview();
      if (merged) populate(merged);
      // The dialog used to warn about this before the fact; the summary line
      // warns about it before the fact now, and this says it AT the fact —
      // several seconds of black picture and silence, on the screen the operator
      // is still standing on, is worth a sentence at the moment it starts.
      setSaveMessage(`Applied "${preset.name}". The monitor and the picture are reconnecting.`, false);

      // THE SCREEN STAYS, SO THE SCREEN HAS TO BE RIGHT.
      //
      // This is the obligation that came with not navigating away. Applying used
      // to end on the main screen, and the operator's next visit to Settings ran
      // open(), which re-lists the instance's events; staying here means open()
      // never runs, and the event picker would go on offering the PREVIOUS
      // instance's events beside a host box that now names a different one — a
      // list an operator can choose from, which is worse than no list.
      //
      // The same ladder a host-changing save uses, and for the same reason:
      // ApplyPreset re-scopes the credentials and re-points the M2L-X client, so
      // sign-in to the new instance is asynchronous and a listing in this turn
      // answers "not signed in" every time. `true` unconditionally — even a
      // preset that leaves the host string alone has changed the credential
      // scope, so the round trip is real either way. Not awaited: the apply is
      // finished and reported, and every failure inside it hides the picker
      // rather than leaving a stale one up.
      void refreshEventsAfterSave(true);
    } catch (err) {
      // Nothing moved, so the preview is still true and still on screen.
      setSaveMessage(`Could not apply "${preset.name}": ${err.message}`, true);
    } finally {
      await refreshPresets();
      // ORDER: refresh first (it re-enables the button this function disabled,
      // and focus() on a disabled button does nothing), focus second, and the
      // scroll LAST — because focusing a control inside a scroll container
      // scrolls it into view, which would undo the restore if it came first.
      if (hadFocus && !applyPresetBtn.disabled) applyPresetBtn.focus();
      form.scrollTop = scrollTop;
    }
  }

  async function handleSavePresetAs() {
    // The NAME goes to Go; GO derives the id — the filename and the
    // credential-target rules exist once, in internal/presets.DeriveID.
    const name = window.prompt(
      'Name this M2L-X instance (what is SAVED is the last saved settings — press Save settings first if you have edited the form):',
    );
    if (!name || !name.trim()) return;
    try {
      const saved = await backend.savePreset(name);
      setSaveMessage(`Saved the current settings as "${saved.name}".`, false);
    } catch (err) {
      setSaveMessage(`Could not save the preset: ${err.message}`, true);
    }
    await refreshPresets();
  }

  async function handleRenamePreset() {
    const preset = selectedPresetSummary();
    if (!preset) return;
    const name = window.prompt(`Rename "${preset.name}" to:`, preset.name);
    if (!name || !name.trim() || name.trim() === preset.name) return;
    try {
      await backend.renamePreset(preset.id, name);
      setSaveMessage(`Renamed to "${name.trim()}". Its stored passwords are unaffected.`, false);
    } catch (err) {
      setSaveMessage(`Could not rename: ${err.message}`, true);
    }
    await refreshPresets();
  }

  async function handleDeletePreset() {
    const preset = selectedPresetSummary();
    if (!preset) return;
    if (!window.confirm(`Delete the instance preset "${preset.name}"? Its file is removed; the current settings do not change.`)) {
      return;
    }
    // Credentials are a SECOND, separate question, and only asked when the
    // preset owns a scope of its own: the legacy scope's entries are the
    // machine's original passwords and Go refuses to delete them from here.
    let alsoCredentials = false;
    if (preset.credentialScope) {
      // The store is NAMED by Go, not written in here: this same dialog runs on
      // Windows and on macOS, and naming the wrong one sends the operator to a
      // control panel their machine does not have. backend.credentialStoreName
      // never throws and never returns empty, so it needs no guard.
      const storeName = await backend.credentialStoreName();
      alsoCredentials = window.confirm(
        `Also delete the stored passwords for "${preset.name}" from ${storeName}? ` +
          'Choose Cancel to keep them (they are reused if you re-create the preset with the same name).',
      );
    }
    try {
      await backend.deletePreset(preset.id, alsoCredentials);
      setSaveMessage(`Deleted "${preset.name}".`, false);
    } catch (err) {
      setSaveMessage(`Could not delete: ${err.message}`, true);
    }
    await refreshPresets();
  }

  /**
   * setSending mirrors the sender state into this screen's gates: Apply and
   * Delete are disabled, with the reason on the control, while the feed is
   * up. app.js calls this from the same place it drives the SENDING lamp, so
   * the two can never disagree. The gate itself is Go's — ApplyPreset refuses
   * while a session runs — this is the honest rendering of it.
   */
  function setSending(sending) {
    sendingNow = sending === true;
    renderPresetButtons();
    // And the preview's "this change is not in the running feed" line, which is
    // true of exactly the same state and must not be able to disagree with the
    // buttons above about whether a session is up.
    renderVideoSource();
  }

  // --- the preset preview -------------------------------------------------
  //
  // SELECTING a preset now SHOWS what applying it would do, on the form itself:
  // every box the preset would change holds the value it would put there, in
  // green, with the value it is replacing stated beside it. Pressing Apply
  // commits it and the green goes, because those values are then simply the
  // values.
  //
  // Before this, the picker's selection did nothing visible at all and the
  // change was described only inside the confirm() dialog — thirteen possible
  // lines, read once, in a hurry, with the form they describe hidden behind the
  // modal.
  //
  // ================== A PREVIEW IS NOT AN EDIT ==============================
  //
  // The preview writes into the REAL controls, because a ghost drawn over a box
  // is a second rendering of a value that can disagree with the first, and
  // because a <select> has nowhere to draw one. That makes exactly one thing
  // dangerous — collectConfig reads those controls — and it is closed in one
  // place: handleSave withdraws the preview before it collects. Every other
  // route out of the preview also restores the boxes it borrowed:
  //
  //   - populate() clears first, so a preview can never outlive the config it
  //     was computed against (it is the diff BASELINE that would otherwise go
  //     stale, silently);
  //   - selecting another preset redraws from scratch rather than layering;
  //   - typing into a previewed box hands that box back to the operator.
  //
  // There is no unsaved-changes guard on this screen to fire — leaveSettings
  // calls handlers.onBack() and nothing else, and the only beforeunload handler
  // in the application (app.js) stops the return and the picture. Checked,
  // because a preview that tripped a "you have unsaved changes" prompt would be
  // the app lying about work the operator did not do.

  /** The decoration currently on the form, or null. See clearPresetPreview. */
  let presetPreview = null;

  /**
   * The preset the summary line in the card is currently describing, or '' when
   * the line is hidden.
   *
   * It is NOT presetPreview.presetId, and the difference is the whole reason it
   * exists: the line can be on screen with NOTHING behind it on the form — a
   * preset that changes no field but whose file carries keys a preset does not
   * honour says exactly that, and says it only here. Anything asking "is there
   * something on screen about a preset" has to read this, or a picker that moves
   * (a delete, a rename) leaves a note about a preset the operator can no longer
   * see selected.
   */
  let presetPreviewLineFor = '';

  /** The class that marks a row as previewed; main.css pairs it with the note. */
  const PREVIEW_CLASS = 'field--preset-preview';

  /**
   * What an emptied box says while it is previewed. GREEN IS NOT THE ONLY
   * SIGNAL — colour-blind operators, and a projector in a gallery — and an
   * empty green box says less than any other kind: the placeholder, the note
   * under the row and the summary line in the card all state the change in
   * words as well.
   */
  const PREVIEW_CLEARED_PLACEHOLDER = 'cleared by this preset';

  /**
   * PREVIEW_SURROGATES: where a change lands when the field has no row of its
   * own. It mirrors ERROR_SURROGATES further down, for the same reason — a
   * hidden field's decoration is attached to nothing and is a change the
   * operator cannot see.
   *
   *   m2lxHost    the address box IS the host's rendering, so the box previews
   *               the base address the preset's host would produce. Never the
   *               live-operation form: that is the shape the write path just
   *               stopped producing, and a preview is a write path.
   *   monitorTile ONE config value spread over four boxes, and Go merges it
   *               field-by-field — a partial tile changes two of the four. Four
   *               green numbers with no old values beside them would claim four
   *               independent changes; one note under the grid states the pair
   *               the model actually compared.
   *
   * Lazily evaluated: these rows are built further down this function.
   */
  const PREVIEW_SURROGATES = Object.freeze({
    m2lxHost: () => ({ wrap: liveURLRow.wrap, input: liveURLInput, format: formatM2LXAddress }),
    monitorTile: () => ({ wrap: tileGrid, input: null }),
  });

  /** previewTargetFor resolves a whitelisted tag to the row that shows it. */
  function previewTargetFor(tag) {
    if (Object.prototype.hasOwnProperty.call(PREVIEW_SURROGATES, tag)) {
      return PREVIEW_SURROGATES[tag]();
    }
    const field = fields[tag];
    // No wrap means a hidden input with no row on screen. It gets no
    // decoration; the summary line still names it, which is the whole reason
    // that line lists every changed field rather than counting them.
    return field && field.wrap ? { wrap: field.wrap, input: field.input } : null;
  }

  /**
   * previewControls describes, for the tags in this diff only, what settings.js
   * has on screen for each — which is all the pure planner needs to decide what
   * may be written into a control. Built per render rather than once, because a
   * <select>'s option list is the authority on what it can express and reading
   * it here means there is no second copy of it to drift.
   */
  function previewControls(diff) {
    const controls = {};
    for (const { tag } of diff) {
      const target = previewTargetFor(tag);
      const input = target && target.input;
      if (!input) {
        controls[tag] = { box: 'none' };
      } else if (input.tagName === 'SELECT') {
        controls[tag] = { box: 'select', options: [...input.options].map((o) => o.value) };
      } else {
        controls[tag] = { box: 'input', format: target.format };
      }
    }
    return controls;
  }

  /**
   * clearPresetPreview puts every borrowed control back exactly as it was and
   * removes the marks. Safe to call when there is no preview, which is what
   * lets populate() and handleSave() call it unconditionally.
   */
  function clearPresetPreview() {
    // THE LINE GOES FIRST, AND UNCONDITIONALLY. It can be on screen with no
    // decoration behind it: a preset that changes nothing but carries keys a
    // preset does not honour says so, and says it in this line alone. Clearing
    // it after the `presetPreview === null` guard below would strand exactly
    // that case — a note about a preset the picker has since moved off.
    presetPreviewLine.hidden = true;
    presetPreviewLine.textContent = '';
    presetPreviewLineFor = '';
    if (presetPreview === null) return;
    for (const box of presetPreview.boxes) {
      box.input.value = box.value;
      box.input.placeholder = box.placeholder;
    }
    for (const wrap of presetPreview.wraps) wrap.classList.remove(PREVIEW_CLASS);
    for (const note of presetPreview.notes) note.remove();
    presetPreview = null;
    // The derived line reads the m2lxHost and eventId BOXES, so restoring them
    // without redrawing it leaves it quoting the preset that has just been
    // taken off the form. See renderPresetPreview's matching call.
    showDerived();
  }

  /**
   * releasePreviewedControl hands a box back to the operator the moment they
   * type in it: their value stands, and the green mark — which claims the box
   * holds the PRESET's value — comes off it.
   *
   * The row's note stays, and deliberately: it says what the preset would do to
   * this field, which is still true, and applying would overwrite their edit
   * along with everything else.
   */
  function releasePreviewedControl(target) {
    if (presetPreview === null) return;
    const i = presetPreview.boxes.findIndex((b) => b.input === target);
    if (i < 0) return;
    const [box] = presetPreview.boxes.splice(i, 1);
    box.input.placeholder = box.placeholder;
    // The mark belongs to the ROW, and a row can hold more than one previewed
    // control (the tile grid is that shape today, the address row was), so it
    // comes off only when nothing previewed is left on it.
    if (!presetPreview.boxes.some((b) => b.wrap === box.wrap)) {
      box.wrap.classList.remove(PREVIEW_CLASS);
    }
  }

  /**
   * renderPresetPreview draws the selected preset's changes onto the form. It
   * CLEARS FIRST, unconditionally: switching the picker from one preset to
   * another must redraw, never layer a second set of notes under the first.
   */
  function renderPresetPreview() {
    clearPresetPreview();
    const preset = selectedPresetSummary();
    // No baseline, no honest diff: before the first populate() every field
    // would read as "(not set) -> x", which is a preview of the wrong thing.
    if (!preset || !lastLoadedConfig) return;

    // The diff is presets.js's, not a second comparison written here. It is
    // also what stops a hand-edited preset file decorating a device-id field:
    // the tags never arrive.
    const diff = diffPreset(lastLoadedConfig, preset.fields);
    const rows = planPresetPreview(diff, preset.fields, previewControls(diff));

    // WHAT THE FILE CARRIES THAT A PRESET DOES NOT HONOUR. This used to be
    // computed in handleApplyPreset and shown inside the confirm dialog; the
    // dialog is gone, and this is the one place that can still state the fact
    // BEFORE the operator commits. It is a real fact about a hand-edited or
    // foreign file — Go drops those keys and reports them — and the diff cannot
    // express it, because diffPreset iterates the whitelist and a key outside it
    // never reaches a row.
    const ignoredNote = describeIgnoredKeys(filterPresetFields(preset.fields).ignored);

    // The active preset, or one that differs in nothing: nothing to say, so
    // nothing is said. A "this preset changes 0 settings" line would be an empty
    // flourish on the one selection that needs no attention at all — unless the
    // file carries junk keys, which is worth saying about a preset that changes
    // nothing at all.
    if (rows.length === 0 && ignoredNote === '') return;

    const state = { presetId: preset.id, boxes: [], wraps: [], notes: [] };
    for (const row of rows) {
      const target = previewTargetFor(row.tag);
      if (!target) continue;

      if (target.input && row.boxValue !== null) {
        state.boxes.push({
          input: target.input,
          wrap: target.wrap,
          value: target.input.value,
          placeholder: target.input.placeholder,
        });
        target.input.value = row.boxValue;
        if (row.cleared) target.input.placeholder = PREVIEW_CLEARED_PLACEHOLDER;
        if (!state.wraps.includes(target.wrap)) {
          target.wrap.classList.add(PREVIEW_CLASS);
          state.wraps.push(target.wrap);
        }
      }

      // The note goes on every changed row, box or no box: it carries the from
      // and the to in words, which is what makes the green readable to someone
      // who cannot see that it is green.
      const note = document.createElement('p');
      note.className = 'field-hint preset-preview-note';
      note.textContent = row.note;
      target.wrap.appendChild(note);
      state.notes.push(note);
    }

    // Only when something was actually drawn on the form. `state` with no rows
    // has no boxes to give back and no notes to remove, and handleSave's "the
    // preview was withdrawn" message reads presetPreview !== null — a state
    // recorded for a summary line and nothing else would make that message a
    // claim about work the operator never saw.
    if (rows.length > 0) presetPreview = state;

    // ONE LINE, TWO JOBS, both inherited from the dialog that used to carry
    // them: what changes and where to look for it (describePresetPreview, which
    // also states the cost of applying), and what the file carries that will be
    // dropped. Joined here rather than inside either module, so each pure module
    // still owns exactly its own sentence.
    presetPreviewLine.textContent = [describePresetPreview(preset.name, rows), ignoredNote]
      .filter(Boolean)
      .join(' ');
    presetPreviewLine.hidden = false;
    presetPreviewLineFor = preset.id;

    // The derived line under the address box is built from the m2lxHost and
    // eventId boxes, and a preset that changes the host has just moved one of
    // them. Without this the row states two different hosts at once — the
    // preset's, in green, in the box, and the machine's, in prose, immediately
    // beneath it — which reads as a bug in the preview rather than as the two
    // halves of one row disagreeing. The line follows the boxes; the green is
    // what marks the whole row as not yet applied.
    showDerived();
  }

  // SELECT previews, APPLY commits. The change event, not the button.
  presetSelect.addEventListener('change', () => renderPresetPreview());

  // Typing into a previewed box takes it back off the preview. Delegated to the
  // form — one listener rather than one per previewed control per redraw — and
  // on BOTH events because a <select> announces itself with 'change' and never
  // with 'input'. Setting .value from script fires neither, so drawing the
  // preview cannot trip this.
  form.addEventListener('input', (e) => releasePreviewedControl(e.target));
  form.addEventListener('change', (e) => releasePreviewedControl(e.target));

  // --- connection -------------------------------------------------------
  const connectionHeading = document.createElement('h2');
  connectionHeading.textContent = 'M2L-X connection';
  openGroup(connectionHeading);

  // THREE BOXES: address, username, password — at the operator's request.
  //
  // The host and the event ID used to be two more editable fields under this
  // one, and they were waffle in field form: both are read out of the address
  // typed here. They still exist as config fields and still travel in presets,
  // so they are kept as HIDDEN inputs (never appended to the DOM) — populate,
  // collectConfig, validation and the preset diff all work unchanged — and the
  // derived line under the address shows the operator what was read.
  //
  // THE ADDRESS IS THE INSTANCE, NOT THE PAGE. This box used to demand a full
  // /live-operation/<event id> URL, on the strength of a probe that concluded
  // no endpoint listed events. That was wrong (see liveurl.js's header and
  // internal/m2lx/events.go), and the operator ran into the consequence:
  // pasting the instance's own address was refused with an error, so they were
  // still made to go and find a live-operation URL for an id the app can ask
  // for itself. Both forms are accepted now — a bare address leaves the event
  // to the picker below, a full URL fills both fields as it always did, which
  // matters because the pasted id is the only source there is before sign-in
  // succeeds.
  const liveURLInput = textInput('f-liveUrl');
  liveURLInput.placeholder = 'https://m2lx-wslstudios-matcht.etapsiota.com';
  liveURLInput.autocomplete = 'off';
  liveURLInput.spellcheck = false;
  const liveURLRow = row(
    'M2L-X address',
    'f-liveUrl',
    liveURLInput,
    // Both halves stop a real mistake and neither can be shortened away. The
    // operator was previously REFUSED when they pasted the instance address, so
    // "the instance address is enough" is the correction; and the live-operation
    // URL is still the only source of an event id before sign-in succeeds, so
    // somebody holding one must be told it still works. The sentence that used
    // to follow — explaining that the box then re-displays the base address on
    // its own — described the field's own behaviour rather than preventing
    // anything, and is now stated in formatM2LXAddress's comment alone.
    'The instance address is enough. A full live-operation URL is also accepted.',
  );
  const liveURLNote = document.createElement('p');
  liveURLNote.className = 'field-hint field-note';
  liveURLNote.hidden = true;
  liveURLRow.wrap.insertBefore(liveURLNote, liveURLRow.errorEl);
  currentGroup.appendChild(liveURLRow.wrap);

  // Hidden, deliberately: see the comment above. addHiddenField registers a
  // field exactly like addField without giving it a row on screen.
  function addHiddenField(key, input) {
    const errorEl = document.createElement('p');
    errorEl.className = 'field-error';
    errorEl.hidden = true;
    fields[key] = { input, errorEl };
    return input;
  }
  addHiddenField('m2lxHost', textInput('f-m2lxHost'));
  addHiddenField('eventId', textInput('f-eventId'));

  // "the alias is literally the username" — so the label says Username. The
  // config field stays `alias` because that is the JSON key M2L-X's sign-in
  // endpoint requires and the name every stored config already uses.
  addField('alias', 'Username', textInput('f-alias'));

  const m2lxPasswordInput = textInput('f-m2lxPassword', 'password');
  m2lxPasswordInput.autocomplete = 'new-password';
  m2lxPasswordInput.placeholder = 'Leave blank to keep the stored password';
  const m2lxPasswordRow = row('M2L-X password', 'f-m2lxPassword', m2lxPasswordInput);
  const m2lxPasswordBadge = document.createElement('span');
  m2lxPasswordBadge.className = 'secret-badge';
  m2lxPasswordRow.wrap.insertBefore(m2lxPasswordBadge, m2lxPasswordRow.errorEl);
  fields.m2lxPassword = { input: m2lxPasswordInput, errorEl: m2lxPasswordRow.errorEl };
  currentGroup.appendChild(m2lxPasswordRow.wrap);

  // The derived line is now the only rendering of host and event, so it is
  // shown whenever they are known rather than only after a paste — populate
  // calls this on open.
  //
  // With no event id it says so in words rather than printing event "": an
  // empty pair of quotes reads as a failure, and on the bare-address form there
  // is no failure — the id simply has not been fetched yet.
  const showDerived = () => {
    const host = fields.m2lxHost.input.value;
    const event = fields.eventId.input.value;
    liveURLNote.hidden = !(host || event);
    liveURLNote.textContent = event
      ? `Host "${host}", event "${event}".`
      : `Host "${host}", event not chosen yet.`;
  };

  // --- the event auto-select / picker -----------------------------------
  //
  // When the app can LIST the instance's events (a signed-in client against a
  // reachable instance), the event id no longer has to come from the pasted
  // URL: a lone event is auto-selected for the operator, and several are offered
  // as a picker. Both write the SAME hidden eventId field the URL path writes,
  // so collectConfig, validation and the preset diff are untouched — this only
  // changes where the value comes FROM. When the app cannot list (not signed in,
  // no host, an older build, any error), neither the picker nor the note shows
  // and the URL-derived behaviour above stands exactly as it was.
  //
  // WHICH INSTANCE GETS LISTED IS NOT THIS SCREEN'S CHOICE. backend.listEvents()
  // takes no host: App.ListEvents uses the API client the control plane built,
  // and that client follows the SAVED configuration — saveConfigFrom restarts
  // the control plane when the host or alias changes (app.go). So an address
  // typed into the box above cannot be enumerated until it has been saved and
  // signed in to, and asking anyway would answer with the PREVIOUS instance's
  // events — which, on the auto-select rule, would quietly write another
  // instance's event id into this form. Hence savedM2LXHost below: the events
  // are only asked for when the address on screen is the one the app is
  // actually signed in to, and the operator is told to save when it is not.
  //
  // The rule itself is in events.js (pure, tested); this is the wiring.
  const eventSelect = document.createElement('select');
  eventSelect.id = 'f-eventSelect';
  // No hint. The row is HIDDEN unless the instance is running more than one
  // event, so its mere presence already says "there is a choice here" — and a
  // labelled picker of named events beside it says the rest. The sentence that
  // stood here explained why the control had appeared, which is a thing the
  // control appearing does by itself.
  const eventSelectRow = row('Event', 'f-eventSelect', eventSelect);
  eventSelectRow.wrap.hidden = true;
  currentGroup.appendChild(eventSelectRow.wrap);

  // The auto-selected line, shown INSTEAD of the picker when there is exactly
  // one event: the operator sees the event was chosen for them rather than
  // wondering why there is no control.
  const eventNote = document.createElement('p');
  eventNote.className = 'field-hint event-note';
  eventNote.hidden = true;
  currentGroup.appendChild(eventNote);

  // Writing the hidden field is the whole point: the picker is just another
  // source for the value collectConfig already reads out of fields.eventId.
  eventSelect.addEventListener('change', () => {
    fields.eventId.input.value = eventSelect.value;
    showDerived();
  });

  /**
   * renderEventChoice applies chooseEvent's decision to the DOM: the picker for
   * a genuine choice, the note for a lone auto-selected event, and neither when
   * there is nothing to improve on (the URL-derived id then stands untouched).
   */
  function renderEventChoice(choice, events) {
    if (choice.needsPicker) {
      eventSelect.textContent = '';
      for (const e of events) {
        const o = document.createElement('option');
        o.value = e.id;
        // Name and status, because an operator choosing between several events
        // tells the live one from the rest by its status.
        o.textContent = e.status ? `${e.name} (${e.status})` : e.name;
        eventSelect.appendChild(o);
      }
      eventSelect.value = choice.selectedId;
      fields.eventId.input.value = choice.selectedId;
      eventSelectRow.wrap.hidden = false;
      eventNote.hidden = true;
      showDerived();
      return;
    }
    eventSelectRow.wrap.hidden = true;
    if (choice.autoSelected) {
      fields.eventId.input.value = choice.selectedId;
      // An auto-selection that differs from what is stored is an unsaved change
      // the operator did not make and cannot see — the field it lands in is
      // hidden. Say so, or they leave Settings believing the event is set and
      // the next launch goes back to the old id.
      const unsaved = choice.selectedId !== savedEventId;
      eventNote.textContent = unsaved
        ? `Event "${choice.selectedName}" (auto-selected) — press Save settings to keep it.`
        : `Event "${choice.selectedName}" (auto-selected).`;
      eventNote.hidden = false;
      showDerived();
      return;
    }
    // 0 events: leave the URL-derived id and its line exactly as they were.
    eventNote.hidden = true;
  }

  /**
   * refreshEvents asks the backend for the instance's events and applies the
   * pure rule. It swallows every failure to the console: not signed in, no host,
   * an older build without the binding, a network error — none of these may
   * break the screen, and all of them mean "fall back to the URL-derived id".
   *
   * @returns {Promise<number>} how many usable events were listed; 0 for every
   *          failure, so a caller can decide whether it is worth asking again
   *          without having to distinguish "empty instance" from "not yet".
   */
  async function refreshEvents() {
    try {
      const events = await backend.listEvents();
      const usable = Array.isArray(events) ? events.filter((e) => e && e.id) : [];
      renderEventChoice(chooseEvent(usable, fields.eventId.input.value), usable);
      // Only a listing that FOUND something counts as done. An empty answer is
      // usually "signed in to nothing yet" rather than "this instance has no
      // events", and recording it as done would mean the address settling, or a
      // save, never asks again.
      if (usable.length > 0) listedHost = savedM2LXHost;
      return usable.length;
    } catch (err) {
      eventSelectRow.wrap.hidden = true;
      eventNote.hidden = true;
      console.info(
        'wslcomms: could not list events; the URL-derived event id stands',
        err?.message || err,
      );
      return 0;
    }
  }

  // WHAT THE BACKEND ACTUALLY HAS, as opposed to what is in the boxes. The API
  // client follows the saved configuration (see the note above), so these two
  // are what decide whether listing events is meaningful and whether an
  // auto-selected event is an unsaved change. They are set by populate and by a
  // successful save — NOT by lastLoadedConfig, which is the Apply-preset diff
  // baseline and deliberately stays at the last POPULATED config.
  let savedM2LXHost = '';
  let savedEventId = '';

  /** The host the last SUCCESSFUL listing was made against; '' when none has. */
  let listedHost = '';

  /**
   * How long the address field must sit still before its host is used to ask
   * for events. applyLiveURL runs on every keystroke ('input', so that a paste
   * lands immediately), and without this an operator TYPING an address would
   * put one HTTP request on the instance per character.
   */
  const ADDRESS_SETTLE_MS = 400;
  let addressSettleTimer = null;

  /**
   * scheduleEventRefresh asks for the instance's events once the address has
   * stopped changing, but only when asking can produce a truthful answer: the
   * address on screen must be the one the app is signed in to, and that host
   * must not already have answered.
   */
  function scheduleEventRefresh(host) {
    if (host === '' || host !== savedM2LXHost || host === listedHost) return;
    if (addressSettleTimer !== null) clearTimeout(addressSettleTimer);
    addressSettleTimer = setTimeout(() => {
      addressSettleTimer = null;
      void refreshEvents();
    }, ADDRESS_SETTLE_MS);
  }

  /**
   * refreshEventsAfterSave is the other half of "the picker must populate after
   * a bare address was entered". Saving a NEW host restarts the control plane,
   * and sign-in to it is asynchronous — a listing attempted in the same turn as
   * the save answers "not signed in to M2L-X yet" every time. So it asks again,
   * twice, spaced far enough apart to cover a sign-in round trip, and then
   * stops: this is a convenience, not a poll, and the operator can always
   * re-open Settings.
   *
   * The ladder keeps running if the operator leaves the screen, and a later
   * open() may overlap it. That is safe rather than merely tolerated: both
   * paths do the same thing — read the list, apply the pure rule, render — so
   * the worst an overlap can do is render the same answer twice.
   */
  async function refreshEventsAfterSave(hostChanged) {
    if ((await refreshEvents()) > 0 || !hostChanged) return;
    for (const delay of [1500, 4000]) {
      await new Promise((resolve) => setTimeout(resolve, delay));
      if ((await refreshEvents()) > 0) return;
    }
  }

  // --- SRT output ---------------------------------------------------------
  const srtHeading = document.createElement('h2');
  srtHeading.textContent = 'SRT output';
  openGroup(srtHeading);

  // There is no SRT host field: the SRT listener is ALWAYS the M2L-X host
  // (internal/config.EffectiveSRTHost derives it), so there is nothing to type
  // and nothing to drift out of step when an instance is switched. Removed at
  // the operator's request.
  addField('srtPort', 'SRT port', numberInput('f-srtPort'));
  addField('srtLatencyMs', 'SRT latency (ms)', numberInput('f-srtLatencyMs'), 'Default 120.');
  addField(
    'pbkeylen',
    'Passphrase key length',
    selectInput('f-pbkeylen', [
      { value: 0, label: 'No passphrase (0)' },
      { value: 16, label: '16' },
      { value: 32, label: '32' },
    ]),
  );

  const srtPassphraseInput = textInput('f-srtPassphrase', 'password');
  srtPassphraseInput.autocomplete = 'new-password';
  srtPassphraseInput.placeholder = 'Leave blank to keep the stored passphrase';
  const srtPassphraseRow = row('SRT passphrase', 'f-srtPassphrase', srtPassphraseInput);
  const srtPassphraseBadge = document.createElement('span');
  srtPassphraseBadge.className = 'secret-badge';
  srtPassphraseRow.wrap.insertBefore(srtPassphraseBadge, srtPassphraseRow.errorEl);
  fields.srtPassphrase = { input: srtPassphraseInput, errorEl: srtPassphraseRow.errorEl };
  currentGroup.appendChild(srtPassphraseRow.wrap);

  const secretsHint = document.createElement('p');
  secretsHint.className = 'field-hint secrets-hint';
  secretsHint.textContent =
    'Passwords are write-only; "set" means saved during this run.';
  currentGroup.appendChild(secretsHint);

  // --- the contribution feed's video leg ----------------------------------
  //
  // Two settings that describe the VIDEO the feed carries, as opposed to the
  // transport above it. They are their own group because they are the two
  // numbers a facility engineer sets once per venue and nobody touches again —
  // and because putting them under "SRT output" would file them beside the port
  // and the latency, which are about the socket rather than the picture.
  //
  // Both travel in an instance preset (internal/presets.InstanceFields): the
  // bitrate is a property of the circuit to that deployment, and the format is a
  // property of that deployment's switcher. Every commentary position at a
  // facility needs the same two answers, which is exactly what the baked-in
  // facility presets are for.
  const videoHeading = document.createElement('h2');
  videoHeading.textContent = 'Contribution video';
  openGroup(videoHeading);

  // --- WHAT THIS POSITION ACTUALLY SENDS ----------------------------------
  //
  // FIRST IN THE GROUP, above the bitrate and the format, because those two
  // describe whatever this one chooses. A bitrate box read before the operator
  // knows whether a camera or a still image is being encoded is a number with
  // nothing to be a number about.
  //
  // IT IS A SETTINGS CONTROL AND NOT A MAIN-SCREEN ONE, and that is a decision
  // rather than a place it happened to land. The video leg is built at START
  // from the saved configuration; a control on the main screen would appear to
  // switch what is on air and would in fact do nothing until the next STOP and
  // START — and the honest alternative, switching it live, is the set_state on a
  // running pipeline that was measured to take the on-air leg to 0 fps
  // permanently. So it sits with the other things that are read once, at Start,
  // and the main screen carries the CAMERA lamp that says what came of it.
  //
  // The values are internal/config's own (VideoSourceSlate / VideoSourceDeckLink)
  // through ./videosource.js, which is also where the wording below comes from.
  //
  // ================== THERE IS NO HINT UNDER THIS CONTROL ANY MORE ===========
  //
  // It used to render both options' `summary` and `cost` plus
  // VIDEO_SOURCE_AT_START_CAVEAT: 933 characters, the single largest block of
  // prose on the screen, and the one the owner named — "SOO much text bellow
  // controbution info that really isn't appropriate to be there".
  //
  // NONE OF THE KNOWLEDGE WAS DELETED. Every sentence of it is still written
  // down, in videosource.js, beside the code it explains: what each option puts
  // on air, the measured processor cost of each (9.3-14.6 % of a core for the
  // card against 18.5-23.9 % for the slate — the intuition is backwards and an
  // engineer needs to know it), and why the leg cannot be swapped under a
  // running feed. What changed is WHO reads it. The measurement belongs to
  // whoever edits this application; the operator picking a source needs the two
  // option labels and the line below that says which one is going to air.
  //
  // The at-START caveat is not lost from the screen either: it is on the
  // control, as the reason it is disabled, at the only moment it can be acted on
  // — see VIDEO_LEG_WHILE_SENDING in renderVideoSource.
  addField(
    'videoSource',
    'Video sent to the switcher',
    selectInput(
      'f-videoSource',
      VIDEO_SOURCES.map((s) => ({ value: s.value, label: s.label })),
    ),
  );

  // WHICH ONE IS GOING TO AIR, in a sentence, under the control that decides it.
  //
  // The <select> alone does not answer that question. Its two options are two
  // similar phrases, and everything else in this application reads green either
  // way — the sender's socket is fine, the switcher really is receiving a
  // healthy correctly-formatted feed, and the picture on the main screen is the
  // RETURN, which says nothing about what this seat contributes. This line and
  // the CAMERA lamp are the whole answer; see videosource.js's header.
  const videoToAirLine = document.createElement('p');
  videoToAirLine.className = 'field-hint video-to-air';
  currentGroup.appendChild(videoToAirLine);

  // AND WHETHER THE HARDWARE FOR IT IS THERE. Hidden when there is nothing worth
  // saying, which is the ordinary case on a working seat of either kind — never
  // an empty flourish under a control that is already correct.
  const videoCardNote = document.createElement('p');
  videoCardNote.className = 'field-hint video-card-note';
  videoCardNote.hidden = true;
  currentGroup.appendChild(videoCardNote);

  // --- the operator's confidence picture ----------------------------------
  //
  // Off by default, and the caveat is the field's hint rather than a tooltip:
  // "it only takes effect at START" reads as an unfinished feature unless the
  // measurement is beside it, and a tooltip is found only by somebody who
  // already suspected there was something to find. PREVIEW_AT_START_CAVEAT
  // carries both halves.
  const previewSupported = backend.usingFakeBackend || backend.previewAvailable();

  addField(
    'decklinkPreviewEnabled',
    'Show me what the card is capturing',
    checkboxInput('f-decklinkPreviewEnabled'),
    PREVIEW_AT_START_CAVEAT,
  );
  // The class is added AFTER the call rather than inside it, so that the
  // addField text keeps the shape settings.test.js and settingslayout.test.js
  // read this file by — four spaces, one argument a line. See the note beside
  // `let currentGroup` for why that matters more than it looks.
  fields.decklinkPreviewEnabled.wrap.classList.add('field--check');

  // WHY BOTH CONTROLS ABOVE ARE DEAD WHILE A FEED IS UP, said once for the pair.
  //
  // The gate is GO'S — App.SetVideoSource and App.SetDeckLinkPreviewEnabled both
  // refuse while a session runs, in those words — and this is the honest
  // rendering of it, exactly as renderPresetButtons is the honest rendering of
  // ApplyPreset's refusal. Disabling rather than offering matters here more than
  // it does there: the video leg is built at START, so a control that accepted
  // the change would appear to put a camera on air and would in fact do nothing
  // at all until the next restart.
  const previewSendingNote = document.createElement('p');
  previewSendingNote.className = 'field-hint preview-sending-note';
  previewSendingNote.textContent = VIDEO_LEG_WHILE_SENDING;
  previewSendingNote.hidden = true;
  currentGroup.appendChild(previewSendingNote);

  /**
   * What ListInputDevices last answered, or null for NOT ASKED / NOT ANSWERED.
   *
   * Null and [] are two different states and the difference is the whole reason
   * this is not initialised to an empty array: an empty list means "this machine
   * has no capture devices at all", and saying "no DeckLink card is fitted" on
   * the strength of a listing that FAILED would send an engineer to look for
   * hardware that is sitting in the slot. deriveVideoSourceEffects reads the
   * difference; nothing here decides it.
   *
   * ONE LISTING SERVES BOTH CONTROLS. It was called videoDevices when only the
   * video-source control read it; the commentary-input picker reads the same
   * answer, because it is the same answer — the card's audio and its video are
   * two entries publishing one persistent-id, so a second call would be a second
   * chance for the two controls to disagree about whether a card is fitted.
   */
  let inputDevices = null;

  /**
   * renderVideoSource redraws everything that depends on the selected source and
   * on whether a card was found: the two lines above, the DeckLink option's own
   * availability, and whether the preview control is offered at all.
   *
   * Called from populate() as well as from the control's own event, because
   * assigning a <select>'s value from script fires neither 'input' nor 'change'
   * — the same trap renderChannelMapGroup documents further down.
   */
  function renderVideoSource() {
    const effects = deriveVideoSourceEffects(fields.videoSource.input.value, inputDevices);

    videoToAirLine.textContent = describeToAir(effects);
    // Marked as well as worded when the configuration cannot start: the sentence
    // says what will happen, the mark is what makes it findable on a form two
    // screenfuls deep.
    videoToAirLine.classList.toggle('video-to-air--unstartable', !effects.startable);

    const availability = describeCardAvailability(effects);
    videoCardNote.textContent = availability;
    videoCardNote.hidden = availability === '';
    videoCardNote.classList.toggle('video-card-note--missing', effects.wantCard && !effects.startable);

    // THE CARD OPTION IS WITHDRAWN WHEN THERE IS NO CARD, rather than left there
    // to be chosen and refused at START. It is disabled and NOT removed, and the
    // difference matters in the one case that matters: a configuration that
    // ALREADY names the card on a machine that has none must still show what it
    // is set to. A removed option would leave the <select> showing the slate
    // while config.json said otherwise — the screen and the file disagreeing,
    // which is the fault describeDeviceSelection exists to prevent on the other
    // dropdown.
    //
    // A disabled <option> that is nonetheless SELECTED still renders, so the
    // operator sees "the card" with the note above explaining why it cannot
    // start, and the only move available to them is the one that fixes it.
    //
    // ============ AND IT IS WITHDRAWN ONLY ON POSITIVE EVIDENCE ==============
    //
    // The owner's report was "The declink input is grayed out in settings?" on a
    // machine with a working UltraStudio in it. The gate is `effects.cardKnown &&
    // !effects.cardPresent` and both halves are deliberate: cardKnown is FALSE
    // when the listing failed or has not happened, so a failure to enumerate can
    // never grey this out — deriveVideoSourceEffects is written around exactly
    // that distinction, and a control disabled on the strength of a call that did
    // not answer is a control nobody can talk out of it.
    //
    // Which leaves one way to reach the greyed state wrongly: a listing that
    // SUCCEEDS and carries no entry whose kind is "decklink" on a machine that
    // has one. That is a data fault upstream of this file — the list is
    // App.ListInputDevices' — and it is not papered over here. Rendering a card
    // as available on the strength of nothing having been enumerated would just
    // move the failure to START, where it costs not-negotiated (-4) in about a
    // ten-thousandth of a second, naming neither the device nor the cause.
    const cardOption = [...fields.videoSource.input.options].find(
      (o) => o.value === VIDEO_SOURCE_DECKLINK,
    );
    if (cardOption) {
      const unavailable = effects.cardKnown && !effects.cardPresent;
      cardOption.disabled = unavailable && !effects.wantCard;
      // ONE SHORT LINE, and only when the option is genuinely refused. It used to
      // read the same whether the option was disabled or merely already chosen.
      cardOption.title = cardOption.disabled ? describeCardOptionRefusal() : '';
    }

    // The preview only means something when there is live video to preview: a
    // confidence monitor of a still PNG is the still PNG. Hidden rather than
    // disabled, for the reason the DeckLink routing group is hidden — a seat
    // that has never had a card must see this screen as it was before any of
    // this existed — and collected either way, for the reason it is collected
    // either way there too.
    fields.decklinkPreviewEnabled.wrap.hidden = !effects.wantCard;
    previewSendingNote.hidden = !sendingNow;

    // --- who may touch these two, and when -------------------------------
    //
    // THREE GATES, IN ORDER OF HOW ABSOLUTE THEY ARE. They are applied to both
    // controls through one expression each, so the pair can never end up in a
    // state where one is offered and the other is not for the same reason.
    //
    // 1. A REMOTE SEAT MAY NOT TOUCH EITHER. What goes ON AIR is not a remote
    //    seat's to decide, and the preview is an opaque window on somebody
    //    else's screen. Both are enforced on the Go side twice over — the
    //    dedicated setters are host-only, and SaveConfig REFUSES a remote save
    //    that changes either field — so this is the visible half rather than
    //    the gate, and its job is to stop a remote operator making an edit that
    //    would then have their whole Save refused.
    //
    // 2. NEITHER MAY CHANGE WHILE SENDING. The video leg is built at START and
    //    cannot be exchanged under a running feed, so a control that accepted
    //    the change would appear to put a camera on air and do nothing until the
    //    next restart. Go refuses both; this is the reason on the control.
    //
    // 3. THE PREVIEW ALSO NEEDS A BUILD THAT CAN DRAW IT. All-or-nothing
    //    availability, the same rule the presets, picture and channel-map groups
    //    follow — offering a tick box that reserves a rectangle nothing will
    //    ever paint is worse than saying the build has not got one.
    //
    // The values are COLLECTED regardless of every one of these. A disabled
    // control keeps its value, and this form replaces the whole document, so a
    // field it fails to restate is a field a Save deletes — which for videoSource
    // means putting a live position back on the slate.
    const remote = backend.isRemoteClient();
    const remoteReason =
      'Set at the commentary position itself, never from a remote seat: this decides what the ' +
      'switcher receives, and the preview is a window on somebody else’s screen.';

    fields.videoSource.input.disabled = remote || sendingNow;
    fields.videoSource.input.title = remote
      ? remoteReason
      : sendingNow
        ? VIDEO_LEG_WHILE_SENDING
        : '';

    fields.decklinkPreviewEnabled.input.disabled = remote || sendingNow || !previewSupported;
    fields.decklinkPreviewEnabled.input.title = remote
      ? remoteReason
      : sendingNow
        ? VIDEO_LEG_WHILE_SENDING
        : previewSupported
          ? ''
          : 'This build cannot draw the preview — it has no preview bindings.';

    // AND THE MICROPHONE PICKER, for exactly the same reason and with a stronger
    // one behind it.
    //
    // App.refuseRemoteCaptureChange guards audioSourceKind, audioDeviceId and
    // decklinkPersistentId as well as the two video-leg fields — without it a
    // remote whole-document SaveConfig could take the desk's microphone away, and
    // on a card seat close and reopen the exclusive DeckLink, from another
    // building. The ONE control that writes all three is this dropdown, and it
    // used to be left enabled: a remote producer who touched it, or who merely
    // had Settings open when the desk changed device, had EVERY subsequent save
    // refused — a port, a status key, anything — naming a field they were never
    // told they may not change.
    //
    // Not gated on sendingNow. SelectCommentaryInput refuses while a feed is
    // running with a sentence of its own that names the reason (a new proxysink
    // orphans the running proxysrc and the feed goes silently dead), and that
    // refusal reaches the operator through applyAudioInputSelection's catch. The
    // video source is different: it is disabled while sending because there is no
    // live setter for it at all.
    audioInputSelect.disabled = remote;
    audioInputSelect.title = remote
      ? 'Chosen at the commentary position itself, never from a remote seat: it decides which ' +
        'microphone the switcher hears.'
      : '';
  }

  /**
   * adoptAudioLeg refreshes the microphone picker from a configuration ANOTHER
   * seat saved, and touches nothing else on the form.
   *
   * It is adoptVideoLeg's twin and exists for the identical reason: this form is
   * a page cache refreshed only by open(), App.SaveConfig refuses a remote save
   * whose audioSourceKind, audioDeviceId or decklinkPersistentId differ from the
   * live ones, and a remote seat that had Settings open when the desk changed
   * microphone would otherwise carry the OLD values in a disabled control — after
   * which its next save of anything at all is refused, naming a field that seat
   * cannot see a way to correct.
   *
   * The three hidden fields are written first and the picker is rebuilt from
   * them, because the picker's value is DERIVED from the three: renderAudioInput
   * is the one function that knows the encoding, and writing the <select> here
   * would be a second copy of it.
   */
  function adoptAudioLeg(config) {
    if (!config) return;
    fields.audioSourceKind.input.value = config.audioSourceKind ?? '';
    fields.audioDeviceId.input.value = config.audioDeviceId ?? '';
    fields.decklinkPersistentId.input.value = config.decklinkPersistentId ?? '';
    // lastLoadedConfig is the preset diff's BASELINE and is deliberately left
    // alone, for adoptVideoLeg's reason: it is what was POPULATED.
    renderAudioInput();
    // The routing panel is gated on the SELECTED device's key matching the one
    // the negotiated width was stamped with, so a device that moved under this
    // seat has to re-run the gate or the grid goes on offering crosspoints over
    // a pad that is not there.
    renderChannelMapGroup();
  }

  /**
   * adoptVideoLeg refreshes the two video-leg controls from a configuration
   * ANOTHER seat saved, and touches nothing else on the form.
   *
   * ===================== WHY A REMOTE SETTINGS SCREEN NEEDS THIS =============
   *
   * App.SaveConfig REFUSES a remote save whose videoSource or
   * decklinkPreviewEnabled differ from the live ones — the enforcement that
   * makes the two host-only setters mean anything. This form is a page cache
   * refreshed only by open(), so a remote seat that had Settings open when the
   * operator at the desk switched to the camera would carry the OLD value in a
   * disabled box, and its next save of anything at all — a port, a status key —
   * would be refused, naming a field that seat is not allowed to change and
   * cannot see a way to correct.
   *
   * It is deliberately NOT populate(). Re-drawing the whole form under somebody
   * mid-edit is the thing app.js's onConfig handler exists to avoid, and these
   * two controls are disabled on a remote seat, so there is no operator edit here
   * to stomp on.
   *
   * THEY ARE NO LONGER THE ONLY CONTROLS THAT CAN REFUSE A SAVE THEY ARE NOT PART
   * OF, and that sentence used to stand here. refuseRemoteCaptureChange now
   * guards audioSourceKind, audioDeviceId and decklinkPersistentId as well; the
   * microphone picker is the one control that writes all three, and adoptAudioLeg
   * above is its half of this. Anything else that joins that guard needs a third.
   *
   * On the LOCAL seat it is a no-op in practice: the only saves that reach it are
   * other seats', and no other seat can change either field.
   */
  function adoptVideoLeg(config) {
    if (!config) return;
    fields.videoSource.input.value = normaliseVideoSource(config.videoSource);
    fields.decklinkPreviewEnabled.input.checked = normalisePreviewEnabled(
      config.decklinkPreviewEnabled,
    );
    // lastLoadedConfig is the preset diff's BASELINE and is deliberately left
    // alone: it is what was POPULATED, and moving two keys under it would make
    // the preview describe a comparison that was never made.
    renderVideoSource();
  }

  fields.videoSource.input.addEventListener('change', renderVideoSource);
  fields.decklinkPreviewEnabled.input.addEventListener('change', renderVideoSource);

  /**
   * refreshInputDevices asks what capture hardware this machine has. It feeds
   * BOTH controls that depend on the answer: the video-source control, which
   * must refuse a configuration that cannot start, and the commentary-input
   * picker, which is a list of these very devices.
   *
   * It swallows every failure to `null` — not to an empty list — because those
   * are different claims and only one of them is true after a failed call. The
   * KINDS are what is read and nothing else: an id is opaque per
   * internal/gst/gst.go, and one persistent-id names the CARD and serves its
   * audio and video entries alike, which is why an input listing answers a
   * question about the video leg at all.
   */
  async function refreshInputDevices() {
    try {
      const devices = await backend.listInputDevices();
      inputDevices = Array.isArray(devices) ? devices : null;
    } catch (err) {
      inputDevices = null;
      console.info(
        'wslcomms: could not list capture devices for the Settings screen',
        err?.message || err,
      );
    }
    renderVideoSource();
    renderAudioInput();
  }

  // WHY THIS IS A CONTROL AND NOT A CONSTANT. It was 2000, and 2000 was chosen
  // for what the video leg used to be — one still PNG through imagefreeze, where
  // there is nothing for a bitrate to be spent on. The operator has ruled it too
  // low for live video and wants nearer 10000. The default stays 2000 so that
  // adding this box changes nothing until somebody types in it.
  // The hint carries the two NUMBERS and nothing else. It used to explain what a
  // kilobit is and which link has to carry it; both are true, neither stops a
  // mistake, and the operator typing in this box already knows what a bitrate
  // does. 2000 is the default and 10000 is the owner's ruling for live video —
  // those two an operator cannot guess, so those two stay.
  addField(
    'videoBitrateKbps',
    'Video bitrate (kbps)',
    numberInput('f-videoBitrateKbps'),
    'Default 2000; live video wants nearer 10000.',
  );

  // --- THE VIDEO FORMAT, AND THE SWITCHER'S OWN FORMAT BESIDE IT ------------
  //
  // A SELECTOR, NOT A BOX. The operator's words: "the video format should be a
  // selector, not a free text field and it should show the M2LX format clearly
  // so it is obvious to the users when you diverge". It was a free-text field
  // whose hint had to teach the grammar — "width x height, then p and the frame
  // rate (50, 25, 59.94)" — which is a four-line lesson in a format nobody
  // should be reciting from memory an hour before kick-off. The rasters are a
  // list now and the lesson is gone with the box that needed it.
  //
  // THE STORED VALUE IS STILL THE STRING, deliberately. CONTRACT.md argues it:
  // a struct merges field-by-field, so a preset carrying {"width":1280} would
  // leave height at 1080 and conform this feed to 1280x1080 — a raster nobody
  // chose. A string cannot half-arrive. This is a UI change and not a schema
  // change; collectConfig still sends the same trimmed string it always did.
  //
  // FOLLOWING THE SWITCHER IS THE DEFAULT AND IS NOT ONE ENTRY AMONG MANY. It
  // is the ungrouped option above every group (fillGroupedSelect's `leading`),
  // because deriving is right on almost every seat and an override is the
  // exception — the app reads the format from the switcher at START, and this
  // field exists for the position that comes up before anything is streaming
  // for it to read.
  const videoFormatSelect = document.createElement('select');
  videoFormatSelect.id = 'f-videoFormatOverride';
  addField('videoFormatOverride', 'Video format', videoFormatSelect);

  // WHAT M2L-X IS ACTUALLY CONFIGURED FOR, and whether this seat agrees with it.
  //
  // Every source feeding an instance must be produced in the instance's format;
  // one that is not is refused. So an override that disagrees is not a
  // preference, it is a feed that will not be accepted — and divergence is both
  // the only reason this control exists and the only way to get it wrong. It is
  // therefore MARKED as well as worded: the line says what is wrong and the
  // class on the row is what makes it findable without reading anything.
  const videoFormatLine = document.createElement('p');
  videoFormatLine.className = 'field-hint video-format-note';
  fields.videoFormatOverride.wrap.insertBefore(videoFormatLine, fields.videoFormatOverride.errorEl);

  /**
   * The INDEPENDENT format this seat's override is judged against, or null for
   * not known — and why describeConformTarget reads the view's `source` rather
   * than trusting any raster it is handed. See refreshConformTarget.
   */
  let conformTarget = null;

  /**
   * renderVideoFormat redraws the readout under the format control: the
   * switcher's own format, and — when this seat is overriding it — whether the
   * two agree.
   *
   * Called from populate() and from refreshConformTarget as well as from the
   * control's own event, because assigning a <select>'s value from script fires
   * neither 'input' nor 'change'.
   */
  function renderVideoFormat() {
    const match = deriveFormatMatch(videoFormatSelect.value, conformTarget);
    const switcher = describeConformTarget(conformTarget);
    videoFormatLine.textContent = match.line === '' ? switcher : `${switcher} · ${match.line}`;
    videoFormatLine.classList.toggle('video-format-note--diverges', match.diverges);
    fields.videoFormatOverride.wrap.classList.toggle('field--diverges', match.diverges);
  }

  videoFormatSelect.addEventListener('change', renderVideoFormat);

  /**
   * refreshConformTarget asks what format the SWITCHER requires.
   *
   * ===================== TWO BINDINGS, IN THIS ORDER =========================
   *
   * getSwitcherFormat is asked FIRST and is the one that matters here, because
   * it reads the INSTANCE'S OWN SETTING over one REST call and needs no session.
   * A Settings screen is opened an hour before kick-off with nothing running,
   * and that is precisely when an operator is choosing a format — so a readout
   * that only worked once a feed was up would be absent for the whole of the
   * time it was wanted.
   *
   * getConformTarget is the FALLBACK and is only ever better in one state: a
   * session is already running, and its target was read back off the pipeline
   * that was actually built rather than off a setting that may have been changed
   * since. It is asked second and only accepted when it is independent of the
   * operator — with no session it hands back the operator's own
   * videoFormatOverride stamped source="override", and quoting that as the
   * switcher's would be the screen inventing a confirmation. That refusal is
   * isIndependentOfTheOperator's, in videoformat.js, so this function does not
   * restate the rule; it only has to not defeat it, which is why the fallback is
   * applied to a NULL first answer rather than merged into it.
   *
   * NEITHER THROWS and both answer null for every way of not knowing — no
   * binding, no host, not signed in, an instance that is not up — so this needs
   * no try/catch and has no failure of its own to degrade.
   */
  async function refreshConformTarget() {
    conformTarget = await backend.getSwitcherFormat();
    if (!conformTarget) conformTarget = await backend.getConformTarget();
    renderVideoFormat();
  }

  // --- status ---------------------------------------------------------
  const statusHeading = document.createElement('h2');
  statusHeading.textContent = 'Status';
  openGroup(statusHeading);
  addField(
    'statusKey',
    'Status key — optional',
    textInput('f-statusKey'),
    'Our router input in switcher_status, e.g. "cam7". Blank = the lamps read NO STATUS.',
  );

  // The suggestions. There is no endpoint that names this node, so the app
  // watches switcher_status across a START and offers whatever changed. It
  // never saves one by itself: another operator's input coming up in the same
  // second is indistinguishable from ours, and a wrong statusKey shows three
  // green lamps for somebody else's feed — which reads as confirmation.
  const suggestions = document.createElement('div');
  suggestions.className = 'suggestions';
  const suggestionsIntro = document.createElement('p');
  suggestionsIntro.className = 'field-hint';
  suggestions.appendChild(suggestionsIntro);
  const suggestionsList = document.createElement('div');
  suggestionsList.className = 'suggestion-list';
  suggestions.appendChild(suggestionsList);
  currentGroup.appendChild(suggestions);

  /**
   * renderSuggestions draws the candidate list. Each entry states what it
   * matched on, because "use cam7" with no evidence is just a different guess.
   */
  function renderSuggestions(candidates) {
    const list = Array.isArray(candidates) ? candidates : [];
    suggestionsList.textContent = '';

    if (list.length === 0) {
      // The INSTRUCTION survives and the explanation of the mechanism does not.
      // An operator has to be told that pressing START is what produces a
      // suggestion — nothing else on the screen implies it — but how the watcher
      // works is for whoever reads app_statuskey.go.
      suggestionsIntro.textContent = 'No suggestion yet. Leave this blank and press START.';
      return;
    }

    if (list.length === 1) {
      suggestionsIntro.textContent = 'One node started streaming as your feed came up:';
    } else {
      // The AMBIGUITY has to be stated — several nodes came up together and the
      // app genuinely cannot tell which is this seat's, and a wrong statusKey
      // shows three green lamps for somebody else's feed, which reads as
      // confirmation. What went is the advice on how to break the tie; the
      // evidence line under each candidate carries the video format that breaks
      // it, which is more use than a sentence saying it might.
      suggestionsIntro.textContent =
        `${list.length} nodes started streaming at once — the app cannot tell which is yours:`;
    }

    for (const c of list) {
      const item = document.createElement('div');
      item.className = 'suggestion';

      const use = document.createElement('button');
      use.type = 'button';
      use.className = 'btn btn-ghost btn-small';
      use.textContent = `Use "${c.key}"`;
      use.addEventListener('click', () => {
        fields.statusKey.input.value = c.key;
        fields.statusKey.input.focus();
        setSaveMessage(`Status key set to "${c.key}". Press Save settings to keep it.`, false);
      });

      const detail = document.createElement('span');
      detail.className = 'suggestion-detail';
      detail.textContent = describeCandidate(c);

      item.append(use, detail);
      suggestionsList.appendChild(item);
    }
  }

  /** describeCandidate is the evidence line: what changed, when, and to what. */
  function describeCandidate(c) {
    const was = c.was === 'absent' ? 'was not in the first snapshot' : `was "${c.was}"`;
    const when = typeof c.afterSeconds === 'number' ? `${c.afterSeconds}s after START` : 'after START';
    const parts = [`${was}, now "${c.now}" — ${when}`];
    if (c.video) parts.push(c.video);
    if (typeof c.audioCount === 'number') {
      parts.push(c.audioCount === 1 ? '1 audio stream' : `${c.audioCount} audio streams`);
    }
    return parts.join(' · ');
  }

  // Live updates while the screen is open: a discovery runs for up to ninety
  // seconds after START, so an operator who opens Settings during it should see
  // the candidate appear rather than have to leave and come back.
  backend.onStatusKeyCandidates((candidates) => renderSuggestions(candidates));

  // --- devices: THE GROUP IS GONE ---------------------------------------
  //
  // Device selection is the main screen's job — the operator's words: "drop
  // the devices ID selection entirely from that page, that should be solely
  // done from the main page". The free-text device fields were also the entry
  // route for the render-endpoint fault fixed in 6bd5d8a: a GUID pasted here
  // bypassed every dropdown filter. The three fields stay REGISTERED as
  // hidden inputs because collectConfig replaces the whole document — a field
  // this form does not restate is a field a save silently deletes — and
  // because the loaded values must round-trip untouched. Note the SRT
  // return's output device (headphoneEndpointId) now has NO editable control
  // anywhere; an empty value falls back to the default output device
  // (return_cgo.go documents the fallback), and a value set by an older build
  // is carried through unchanged.
  addHiddenField('audioDeviceId', textInput('f-audioDeviceId'));
  addHiddenField('headphoneDeviceId', textInput('f-headphoneDeviceId'));
  addHiddenField(DEVICE_KEY_SRT, textInput('f-headphoneEndpointId'));

  // --- where the commentary is captured from ------------------------------
  //
  // WHY THIS GROUP EXISTS AT ALL, when the group above it was deleted for being
  // device selection on the wrong screen. It is not a device picker: it is the
  // question of which SUBSYSTEM the commentary comes through, and the main
  // screen's dropdown cannot ask it — that dropdown lists the platform's audio
  // endpoints, and the whole point of the DeckLink route is that the microphone
  // is NOT on one of them.
  //
  // The operator's original bug, in one sentence: the CoreAudio device a
  // Blackmagic card publishes ("Blackmagic UltraStudio 4K Mini") measured
  // -96 dBFS on all 16 channels with the mic live, while the device that does
  // carry the mic publishes no unique-id and is therefore hidden by the
  // dropdown's own filter. So the input list offers the silent one and hides the
  // real one, and no amount of choosing from it can be right.
  //
  // Both fields are MACHINE state (internal/presets.MachineFields): they answer
  // "what is plugged into THIS PC", and a preset carrying either would deliver
  // another machine's hardware by post — the audioDeviceId fault, with a card
  // instead of an endpoint, and a worse error when it lands.
  const captureHeading = document.createElement('h2');
  captureHeading.textContent = 'Commentary input';
  openGroup(captureHeading);

  // ================== ONE PICKER, TWO SUBSYSTEMS, NO RAW IDS ================
  //
  // THREE CONTROLS BECAME ONE. There used to be a "Capture from" <select>
  // choosing the SUBSYSTEM, a free-text "DeckLink card ID" box, and — on another
  // screen entirely — the dropdown that chose the computer's own endpoint. An
  // operator wanting the microphone on the SDI input had to know that picking a
  // microphone implies a capture subsystem, and had to set two halves
  // consistently by hand, on two screens, with a persistent-id typed from
  // memory into a box whose own comment admitted it was a box only because
  // nothing enumerated the cards yet.
  //
  // Something enumerates them now: App.ListInputDevices returns both kinds, and
  // the DeckLink entry's id IS the persistent-id — it names the CARD and serves
  // audio and video alike (measured: the fitted UltraStudio's Audio/Source and
  // Video/Source entries both publish 2747401380). So the box is a dropdown and
  // the subsystem question is gone: SELECTING AN ENTRY DOES THE WHOLE SETUP.
  //
  // The three config fields are still three fields and are still collected and
  // validated exactly as they were — they are HIDDEN inputs below, written by
  // this control. Keeping them as fields rather than deriving them at collect
  // time is what leaves populate(), collectConfig(), validateConfig() and the
  // preset diff untouched by any of this.
  const audioInputSelect = document.createElement('select');
  audioInputSelect.id = 'f-commentaryInput';
  const audioInputRow = row('Microphone', 'f-commentaryInput', audioInputSelect);
  currentGroup.appendChild(audioInputRow.wrap);

  // THE ONLY LINE UNDER IT, and it is shown only when there is something wrong
  // to say — a saved device that is not plugged in today. Hidden otherwise:
  // never an empty flourish under a control that is already correct.
  const audioInputNote = document.createElement('p');
  audioInputNote.className = 'field-hint audio-input-note';
  audioInputNote.hidden = true;
  audioInputRow.wrap.insertBefore(audioInputNote, audioInputRow.errorEl);

  // The three fields the picker writes. Registered exactly as any other field —
  // populate reads them, collectConfig restates them, the validator lights them
  // — but with no row of their own, because the picker above IS their row.
  //
  // audioDeviceId is registered further down with the other device ids; these
  // two are registered here, beside the control that decides them.
  addHiddenField('audioSourceKind', textInput('f-audioSourceKind'));
  addHiddenField('decklinkPersistentId', textInput('f-decklinkPersistentId'));

  // THE COUGH MUTE MODE IS NOT A SETTING ON THIS FORM, but it must survive one
  // being saved.
  //
  // It is chosen on the MAIN SCREEN, in the column beside the picture, because
  // it is a match-time preference and not a configuration: the operator decides
  // between a held cough key and a latched one where they are sitting when they
  // decide it. Putting a second copy of the control here would give one value
  // two owners.
  //
  // Registered hidden for the reason the device ids above are: saveConfig
  // REPLACES the whole document, so a field this form does not restate is a
  // field this form DELETES. An operator who set latch and then saved a port
  // number would find their cough button back on push — discovered mid-match, by
  // pressing it. Go carries the field forward when a page omits it as well
  // (app.go's carryForwardUIOnlyFields), and that belt is not a reason to drop
  // this brace: the two guards fail in different directions.
  addHiddenField('coughMuteMode', textInput('f-coughMuteMode'));

  // Whether this build can re-point capture at all. The same all-or-nothing
  // build axis as channelMapSupported below, and asked for the same reason: an
  // older build has none of the three bindings, and calling one that is not
  // there rejects with a message about a missing method rather than about a
  // microphone. backend.captureAvailable() is the BUILD's answer — "there is no
  // card in this machine" is a different fact and arrives on the capture event.
  const captureSupported = backend.usingFakeBackend || backend.captureAvailable();

  /**
   * applyAudioInputSelection is the "and then do the correct setup based on what
   * is selected" half. All three fields move together, with the one that no
   * longer applies CLEARED — see deriveAudioInputEffects for why clearing
   * matters as much as setting.
   */
  function applyAudioInputSelection() {
    const effects = deriveAudioInputEffects(audioInputSelect.value);
    fields.audioSourceKind.input.value = effects.audioSourceKind;
    fields.audioDeviceId.input.value = effects.audioDeviceId;
    fields.decklinkPersistentId.input.value = effects.decklinkPersistentId;
    // The note is cleared in the same breath, with `false` for absent: a
    // selection the operator has just made came from the list, so it is by
    // construction present, and any "not connected" line left over from the
    // PREVIOUS selection is now about a device nobody has chosen. The absent
    // state is only ever reached by LOADING a configuration, which is
    // renderAudioInput's path.
    renderAudioInputNote(false);
    // The routing grid follows the DEVICE, so it is re-rendered here rather than
    // listened for: these are hidden inputs, and assigning one from script fires
    // nothing at all. It will hide immediately — the width this screen holds
    // belongs to the device being left — and come back when the new device's
    // report arrives. See renderChannelMapGroup.
    renderChannelMapGroup();
    // AND CAPTURE IS RE-POINTED NOW, WITHOUT A SAVE. This is what makes the
    // routing panel appear as soon as the device is selected rather than at the
    // next launch: the width can only come from a pad that has negotiated, and
    // nothing negotiates until the capture is pointed at the device.
    //
    // No save, deliberately. Selecting is not committing — the operator can pick
    // an interface, watch the meters, see it is the wrong one and pick again,
    // and config.json is untouched throughout. It is Save that commits.
    if (!captureSupported) return;
    backend
      .selectCommentaryInput(
        effects.audioSourceKind,
        effects.audioDeviceId,
        effects.decklinkPersistentId,
      )
      // REPORTED, NEVER SWALLOWED, for setChannelMap's reason and one more: the
      // refusal an operator will actually meet is "not while sending", and a
      // selection that was refused leaves this form showing a device the
      // commentary is not coming from.
      .catch((err) => {
        setSaveMessage(`Could not switch the commentary input: ${err.message}`, true);
      });
  }

  audioInputSelect.addEventListener('change', applyAudioInputSelection);

  /**
   * renderAudioInput rebuilds the picker from today's device list and the three
   * saved fields, and says so when the saved selection is not among them.
   *
   * It does NOT write the fields back: rebuilding a list is not the operator
   * choosing from it, and a saved-but-absent device must stay saved. That is the
   * whole point of showing it as absent rather than dropping it — a dropdown
   * that silently moved to device #1 would leave the screen and config.json
   * disagreeing, which is the fault describeDeviceSelection exists to prevent.
   */
  function renderAudioInput() {
    const plan = planAudioInputs(inputDevices, {
      audioSourceKind: fields.audioSourceKind.input.value,
      audioDeviceId: fields.audioDeviceId.input.value,
      decklinkPersistentId: fields.decklinkPersistentId.input.value,
    });
    fillGroupedSelect(audioInputSelect, plan, null);
    renderAudioInputNote(plan.absent);
  }

  /**
   * renderAudioInputNote is the ONE LINE under the picker.
   *
   * It is separate from renderAudioInput because it is called from the picker's
   * own change handler as well, and rebuilding a <select> from inside its own
   * 'change' listener is a thing that works until it does not. Only the note
   * depends on the selection; the list does not.
   *
   * ONE THING CAN BE WRONG HERE NOW, and that is the point.
   *
   * There used to be two, and the second was DECKLINK_AUDIO_NOT_BUILT: a line
   * saying that a DeckLink commentary input would be refused at START because
   * the capture leg for it did not exist. IT EXISTS. decklinkaudiosrc opens the
   * card, a decklinkvideosrc clocks it, the mix matrix routes the commentator's
   * channels into the feed, and a seat set to the card starts and goes on air —
   * so the line, the constant and the test that pinned its presence all went
   * with the gate in app.go's preflightCapture that they were about.
   *
   * What is left is about THIS SELECTION and nothing else: the saved device is
   * not plugged in today. That is the one the operator can fix by choosing
   * again, which is why it is worth a line under the control they would choose
   * with.
   */
  function renderAudioInputNote(absent) {
    const note = absent ? 'This device is not connected. Choose another, or plug it back in.' : '';
    audioInputNote.hidden = note === '';
    audioInputNote.textContent = note;
    // Marked as well as worded for the reason the format row is: a sentence says
    // what is wrong and the mark is what makes it findable on a form two
    // screenfuls deep.
    audioInputRow.wrap.classList.toggle('field--absent', absent);
  }

  // --- the routing grid ----------------------------------------------------
  //
  // WHY THIS IS A GROUP ON THIS SCREEN AND NOT A VIEW OF ITS OWN. It is the
  // second half of the question the group above asks: having said which device
  // the commentary arrives on, the operator has to say which of that device's
  // channels the commentary IS. Splitting the two across screens would put the
  // answer somewhere the question does not lead.
  //
  // ============ IT IS NOT GATED ON THE KIND, AND THAT IS THE CHANGE =========
  //
  // This group used to be reachable only from audioSourceKind = decklink, and
  // the comment that stood here said so as a RULE. It was wrong twice over.
  //
  // Wrong about the hardware: a card is not the only device that presents
  // unpositioned multichannel audio. gstosxcoreaudio.c:886-889 sets the layout
  // to NULL for every CoreAudio source unconditionally, so a Focusrite, an RME
  // or an aggregate is the same problem in the same shape — measured on this
  // machine at channels=3,mask=0x0 and channels=16,mask=0x0, identical to the
  // card's sixteen.
  //
  // And wrong about what routing is FOR: the operator's ruling is that the grid
  // is shown at every width, including 1 and 2. "You may want to flip the
  // channels on a stereo source, on a mono you may want to route it to be dual
  // mono etc." Both are already expressible — a flip is {Left<-2, Right<-1} and
  // dual mono is {Left<-1, Right<-1}, which is what gst.DefaultChannelMap
  // already produces for a one-channel device — so refusing to draw them was
  // hiding a control that already worked.
  //
  // What the group IS gated on is a negotiated width FOR THE DEVICE THIS FORM IS
  // SHOWING. See renderChannelMapGroup: that is one `hidden` on the whole
  // section ([hidden] is !important in main.css, so it beats .settings-group's
  // display:grid), and it never comes from the device list — a machine can have
  // a card in it and still capture from a USB microphone.
  //
  // THE MAP IS STILL COLLECTED WHILE THE GROUP IS HIDDEN. collectConfig
  // replaces the whole document, so a seat pressing Save while its device is
  // still opening would otherwise DELETE that device's routing — the same silent
  // data loss the hidden device-id fields exist to prevent, with a commentator's
  // channel assignment in it. What stops the opposite fault — the grid speaking
  // for a device it is not sized to — is channelMapView.collect()'s own guard.
  const channelMapSupported = backend.usingFakeBackend || backend.channelMapAvailable();

  const channelMapHeading = document.createElement('h2');
  // Replaced by renderChannelMapGroup with the negotiated width and the device,
  // and never left saying DeckLink: an operator on an interface who reads
  // "DeckLink channel routing" above their own eight channels concludes the
  // panel is about somebody else's hardware and stops reading it.
  channelMapHeading.textContent = describeRoutingHeading(0);
  const channelMapGroup = openGroup(channelMapHeading, 'settings-group--channelmap');
  // HIDDEN UNTIL A WIDTH ARRIVES. renderChannelMapGroup runs on populate() and
  // on every pad report, both of which are at least one await after this screen
  // is built, and a group that defaulted to visible would flash an empty routing
  // grid onto every seat's screen once per open.
  channelMapGroup.hidden = true;

  // The half of "live" that the grid itself cannot say. channelmap.js's caveat
  // states that a change reaches the running feed immediately; this states what
  // that does NOT include — the next launch reads config.json, so a routing
  // nobody saved is a routing that lasts until the app is closed.
  const channelMapSaveHint = document.createElement('p');
  channelMapSaveHint.className = 'field-hint channelmap-save-hint';
  // Both halves are the operator's business and neither is an explanation: a
  // change here IS live, and a change nobody saved lasts only until the app is
  // closed. Said in one line instead of two sentences.
  channelMapSaveHint.textContent = 'Live immediately. Save settings to keep it for the next launch.';
  currentGroup.appendChild(channelMapSaveHint);

  const channelMapView = createChannelMapView({
    // LIVE, on every change, because the change IS live: the property write was
    // measured at 119 µs with no renegotiation, the pipeline staying PLAYING and
    // the change audible in the very next level message.
    //
    // A FAILURE IS REPORTED, NEVER SWALLOWED. Go validates the map against the
    // negotiated width and writes nothing if it does not fit; a caller that
    // ignored that would leave the grid showing a routing which is not the one
    // in force — which is precisely the trap the whole design exists to avoid,
    // since audioconvert's own refusals are silent and unreadable afterwards.
    onChange: (map) => {
      if (!channelMapSupported) return;
      backend.setChannelMap(map).catch((err) => {
        setSaveMessage(`Could not apply the channel routing: ${err.message}`, true);
      });
    },
  });
  currentGroup.appendChild(channelMapView.el);

  // A build with no channel-map bindings says so, rather than showing a grid
  // whose every press is refused. Same all-or-nothing rule as the presets and
  // picture groups.
  //
  // ============ WHY THIS IS NOW A BELT ON A BRACE, AND KEPT ANYWAY ==========
  //
  // The old gate was the capture KIND, so a card seat on a bindings-less build
  // opened this group and read this line. The new gate is a negotiated WIDTH,
  // and a build with no channel-map bindings has no GetChannelMap to answer one:
  // it takes backend.getChannelMap's fallback, whose deviceKey is empty and
  // whose width is zero, for ever. So the group never opens and this line is
  // never read.
  //
  // It is kept rather than deleted, and the alternative is written down because
  // somebody will propose it: showing the group on an unsupported build so the
  // line IS read would put a grey paragraph on every seat of that build,
  // including the slate-and-microphone seats that have no routing question at
  // all — the "a routing grid flashing onto a seat that never had a card"
  // fault in a new costume. The three `hidden` writes below stay for the same
  // reason: they are what stops a future gate change resurrecting a grid whose
  // every press is refused, and each one costs a line.
  const channelMapUnsupported = document.createElement('p');
  channelMapUnsupported.className = 'field-hint';
  // What the build cannot do, and what it does instead. The reason it cannot —
  // no channel-map bindings — is a fact about how this copy was compiled and is
  // no use to somebody at a desk; it stays in this comment. De-carded with the
  // rest of this group: the channels are the INPUT's, whatever it is.
  channelMapUnsupported.textContent =
    'This build cannot route this input’s channels. The capture takes the first two.';
  channelMapUnsupported.hidden = channelMapSupported;
  channelMapView.el.hidden = !channelMapSupported;
  channelMapSaveHint.hidden = !channelMapSupported;
  currentGroup.appendChild(channelMapUnsupported);

  /**
   * currentAudioDeviceKey is the channelMaps key of the device this form would
   * save right now: the capture kind, and the id that kind selects.
   *
   * It is encodeAudioInput because that IS the spelling — the picker already
   * builds this exact string for every device it lists, and Go builds the same
   * one in internal/config.AudioDeviceKey. Three places, one grammar, because a
   * key spelled two ways is a routing filed where nothing will look for it
   * again: no error, no refusal, just a grid that comes up empty the next
   * morning with the operator's channels still in the file.
   *
   * Built from the three FIELDS collectConfig itself writes, never from the
   * picker's <select>. A saved-but-absent device stays in those fields while the
   * <select> has nothing to show for it (see renderAudioInput), and the key must
   * follow the document being saved rather than the control.
   *
   * @returns {string}
   */
  function currentAudioDeviceKey() {
    const kind = normaliseAudioSourceKind(fields.audioSourceKind.input.value);
    const id =
      kind === AUDIO_SOURCE_DECKLINK
        ? fields.decklinkPersistentId.input.value.trim()
        : fields.audioDeviceId.input.value.trim();
    return encodeAudioInput(kind, id);
  }

  /**
   * adoptChannelMaps normalises whatever config.json offers into a plain object
   * of arrays. Anything else — null, a list, a string from a hand edit — reads
   * as "no routing saved anywhere", which is the state every seat starts in and
   * the only reading that cannot lose one.
   *
   * @param {unknown} value
   * @returns {Record<string, unknown[]>}
   */
  function adoptChannelMaps(value) {
    /** @type {Record<string, unknown[]>} */
    const out = {};
    if (!value || typeof value !== 'object' || Array.isArray(value)) return out;
    for (const [key, map] of Object.entries(value)) {
      if (Array.isArray(map)) out[key] = map;
    }
    return out;
  }

  /**
   * collectChannelMaps writes the grid that is on screen into the carried store,
   * under the key of the device it was drawn for — over every other device's
   * routing, never instead of it.
   *
   * An EMPTY grid REMOVES the key rather than storing []. Absent means "nobody
   * has chosen" and resolves to the first two input channels; an explicit empty
   * list says the same thing in a spelling nothing else uses, and would grow a
   * key in config.json for every device anybody ever glanced at.
   *
   * @returns {Record<string, unknown[]>}
   */
  function collectChannelMaps() {
    const maps = { ...carriedChannelMaps };
    const key = currentAudioDeviceKey();
    const map = channelMapView.collect();
    if (map.length > 0) maps[key] = map;
    else delete maps[key];
    return maps;
  }

  /**
   * channelMapState is the last pad report this screen has seen: a WIDTH and the
   * DEVICE it belongs to. Held rather than read back, because the gate below has
   * to answer between reports and getChannelMap is an await.
   *
   * Both halves start empty, which is the honest state of a screen that has not
   * been told anything: the width is zero and the key matches no device, so the
   * group is hidden and no grid is drawn.
   */
  let channelMapState = { inputChannels: 0, deviceKey: '' };

  /**
   * renderChannelMapGroup shows or hides the whole group, and names it.
   *
   * ===================== THE GATE, AND WHY EACH HALF IS THERE ================
   *
   *   channelMapSupported   the BUILD can route at all. Nothing on a machine
   *                         changes this; a missing binding is not a missing
   *                         device.
   *   known                 the width this screen holds is for the device this
   *                         FORM is showing. Without it there is a window
   *                         between selecting an interface and its pad
   *                         negotiating in which the grid still holds the
   *                         previous device's sixteen, and a crosspoint pressed
   *                         in that window writes a 2x16 matrix onto a
   *                         two-channel pad — measured on the real card as
   *                         "streaming stopped, reason error (-5)", the capture
   *                         chain dead before the next level message.
   *   width >= 1            something actually negotiated. NOT `> 2`: the
   *                         operator's ruling is that a stereo flip and a dual
   *                         mono are routing decisions like any other, so every
   *                         width the pad reports gets a grid.
   *
   * Called from populate(), from the picker's own handler and from every pad
   * report — the three ways either half can move. It is never called from the
   * device LIST: a machine can have a card in it and still capture from a USB
   * microphone.
   */
  function renderChannelMapGroup() {
    const known = channelMapState.deviceKey === currentAudioDeviceKey();
    const width = known ? inputChannelCount(channelMapState) : 0;
    channelMapGroup.hidden = !(channelMapSupported && known && width >= 1);
    // The heading is the width and the device, so the one number that says
    // whether the right thing opened is readable without counting rows. The
    // device's name is the picker's own label rather than a second spelling of
    // it — an id, or a name this screen made up, would be a third name for a
    // device the operator has already been shown twice.
    channelMapHeading.textContent = describeRoutingHeading(
      width,
      audioInputSelect.selectedOptions?.[0]?.textContent || '',
    );
  }

  // NO LISTENER ON THE KIND FIELD, and its absence is the point. audioSourceKind
  // is a hidden input — the commentary-input picker writes it — and assigning an
  // input's value from script fires neither 'input' nor 'change', so a listener
  // here would be a group that never opened. The picker calls this directly
  // instead; see applyAudioInputSelection.

  /**
   * adoptChannelMapState takes GetChannelMap's report, or the "channelMap"
   * event's, and applies it in the order that matters: the WIDTH first, then the
   * map, so the map is rendered against the width it will actually be sent at.
   *
   * THE REPORT NAMES ITS DEVICE, and a change of device moves the map as well as
   * the width. The store is per device (config.ChannelMaps), so when capture
   * comes up on something else the routing under the grid has to come from that
   * device's entry — otherwise the previous device's map is drawn against the
   * new device's width, which is a routing nobody chose displayed as if they
   * had.
   *
   * A NON-EMPTY map in the report is the one IN FORCE, and it wins over the one
   * config.json holds. They are the same on a normal launch; when they differ it
   * is because somebody routed and did not save, and showing the saved one would
   * be showing a routing that is not what the commentator is hearing. An EMPTY
   * map is not adopted, because empty means "nobody has chosen" — it would
   * overwrite a saved map with the absence of one.
   */
  function adoptChannelMapState(state) {
    const previousKey = channelMapState.deviceKey;
    const deviceKey = typeof state?.deviceKey === 'string' ? state.deviceKey : '';
    channelMapState = { inputChannels: state?.inputChannels, deviceKey };
    channelMapView.setPad(state);
    if (deviceKey !== previousKey) {
      channelMapView.setMap(carriedChannelMaps[deviceKey], deviceKey);
    }
    if (state && Array.isArray(state.map) && state.map.length > 0) {
      channelMapView.setMap(state.map, deviceKey);
    }
    // The group follows the report, which is the half populate() cannot do: a
    // device opening, failing or being swapped happens long after this screen
    // was drawn, and R2's whole promise is that the panel appears as soon as the
    // device is selected rather than at the next launch.
    renderChannelMapGroup();
  }

  // The pad negotiates while this screen is open — at launch, and again every
  // time the device changes — so the grid follows the event as well as the
  // open() call. It used to say "it does so at START", which was true of a
  // capture pipeline built by START; capture is built at launch and held to quit
  // now, and START negotiates nothing. Subscribed here, in the same place and
  // for the same reason as onStatusKeyCandidates above: the screen that needs
  // the value is the one that asks for it.
  //
  // SUBSCRIBED AT BUILD, NOT AT OPEN, and that is load-bearing rather than
  // incidental: the report that matters most arrives while Settings is CLOSED —
  // the operator picks a device, goes back to the main screen and comes back —
  // and a subscription made in open() would miss it and show a stale group until
  // the next report.
  //
  // onChannelLevels, NEVER onLevels: they are two different level elements
  // measuring two different points, and the main screen's pair is downstream of
  // the matrix this grid controls. backend.js's section header has the
  // measurement that makes mixing them up a flickering meter rather than a
  // harmless duplication.
  backend.onChannelMap((state) => adoptChannelMapState(state));
  backend.onChannelLevels((frame) => channelMapView.setLevels(frame));
  backend.onSignal((payload) => channelMapView.setSignal(payload));

  // --- monitor / return ---------------------------------------------------
  const monitorHeading = document.createElement('h2');
  monitorHeading.textContent = 'Monitor';
  openGroup(monitorHeading);
  // All seven audio tracks, from the one shared table in ./returns.js — the
  // same object the main screen's dropdown iterates, not a copy of it. The
  // monitor subscribes to every track regardless; this only picks which one is
  // routed to the headphones.
  addField(
    'returnMid',
    'Return',
    selectInput(
      'f-returnMid',
      RETURN_BUSES.map((b) => ({ value: b.mid, label: b.label })),
    )
  );
  // Which SOURCE channel of that bus reaches the ears. It is here as well as on
  // the main screen because it is part of the saved configuration and because
  // CLN on this event carries effects hard left and comms hard right — a
  // commentator who wants effects alone needs one channel, not one bus.
  //
  // "Left only" means the LEFT SOURCE CHANNEL IN BOTH EARS.
  addField(
    'returnChannel',
    'Return channel',
    selectInput(
      'f-returnChannel',
      CHANNEL_MODES.map((m) => ({ value: m.value, label: m.label })),
    ),
    '"Left only" = the left source channel in both ears.',
  );
  // Beside the bus and channel controls it trims, NOT below the encryption
  // heading it used to sit under: this is a monitor-wide gain, and a field that
  // renders among the encryption controls reads as one of them — under the
  // grid, literally on the same row as the key length and the passphrase.
  addField(
    'returnGainDb',
    'Return gain (dB)',
    numberInput('f-returnGainDb', 0.1),
    'Default 18 dB (measured).',
  );
  // The return SOURCE — WebRTC or the native SRT path — has NO control here on
  // purpose. It decides what the commentator can hear right now, and a
  // configuration screen that changes it as a side effect of pressing Save is a
  // way to silence somebody mid-match from a screen they are not looking at. It
  // lives on the main screen only; this form carries the saved value through
  // untouched — see collectConfig.

  // WHICH M2L-X OUTPUT THE SRT RETURN DIALS. A real control, not a carried
  // value, and that is the whole point of it: it was carried through this form
  // untouched for one revision, so a config.json written while the default was
  // 40503 could not be corrected from the application at all. The operator had
  // an encrypted clean feed in a field nothing on screen would show them.
  //
  // It sits with the return settings and immediately above the return
  // encryption controls because the two are one decision: the port chooses the
  // output, and encryption is set PER OUTPUT on M2L-X, so changing this may
  // well mean changing the two controls below it as well.
  addField(
    'srtReturnPort',
    'SRT return port',
    numberInput('f-srtReturnPort'),
    // THE MENU STAYS. There is no endpoint that lists the M2L-X outputs — this
    // table was measured by dialling each one — so a bare five-digit box is a
    // number with no way to find out what to type. The closing sentence about
    // encrypted outputs needing the key below went: the two controls that supply
    // it are the next two on the screen, under a heading that says so.
    '40501 pgm · 40502 pvw (encrypted) · 40503 cln (encrypted) · 40504+ relays.',
  );

  // HOW MUCH SRT BUFFER THE COMMENTATOR'S PICTURE CARRIES. A real control, for
  // the same reason the port above it is one: it had no field at all until the
  // operator reported the picture running about a second behind the main feed,
  // and a number nobody can reach is a number nobody can correct.
  //
  // It sits immediately below the port because they describe the same session —
  // the port picks which M2L-X output the picture comes from, this picks how
  // much delay it is buffered with — and because the hint below has to be read
  // next to the output it applies to.
  //
  // The hint names the far end's floor. An operator who drops this, sees no
  // change and is told nothing will conclude the control is broken, when the
  // truth may be the opposite: the control works and the far end is overriding
  // it. It is phrased as a thing that CAN happen rather than a thing that
  // definitely does, because that is as far as the measurement goes — see the
  // note on the floor at internal/config.Config.PictureLatencyMs.
  addField(
    'pictureLatencyMs',
    'Picture buffer (ms)',
    numberInput('f-pictureLatencyMs'),
    // THE MOST IMPORTANT STRING ON THIS SCREEN, and it is short because it can
    // be. SRT negotiates the LARGER of the two ends and this M2L-X output is set
    // to 300 ms, so an operator who drops this to 40, sees no change and is told
    // nothing concludes the control is broken — when in fact it works and the far
    // end is overriding it. All three facts survive; the instruction that
    // followed them ("change it on M2L-X") is what an operator does with them.
    'Default 120. SRT takes the larger end, and this M2L-X output sets 300.',
  );

  // --- SRT return encryption ---------------------------------------------
  //
  // Grouped with the return settings above rather than with the SRT OUTPUT
  // fields, because that is what they belong to: they are the key to the M2L-X
  // output this app DIALS, not to the input it SENDS to.
  //
  // They are separate controls from "Passphrase key length" and "SRT
  // passphrase" in the SRT output section for a measured reason. M2L-X sets
  // encryption per output:
  //
  //     Output 1  src=pgm  port 40501  encrypted=false
  //     Output 2  src=pvw  port 40502  encrypted=true
  //     Output 3  src=cln  port 40503  encrypted=true
  //
  // so the send path and the return path routinely need different answers, and
  // one pair of controls for both cannot express that. Setting the key that
  // makes the monitor work would change the key the feed goes out with.
  const returnEncryptionHeading = document.createElement('h3');
  returnEncryptionHeading.textContent = 'SRT return encryption';
  currentGroup.appendChild(returnEncryptionHeading);

  addField(
    'srtReturnPBKeyLen',
    'Return key length',
    selectInput('f-srtReturnPBKeyLen', [
      { value: 0, label: 'No passphrase (0)' },
      { value: 16, label: '16' },
      { value: 32, label: '32' },
    ]),
    'Match the M2L-X output; 0 unless it is encrypted.',
  );

  const srtReturnPassphraseInput = textInput('f-srtReturnPassphrase', 'password');
  srtReturnPassphraseInput.autocomplete = 'new-password';
  srtReturnPassphraseInput.placeholder = 'Leave blank to keep the stored passphrase';
  const srtReturnPassphraseRow = row(
    'SRT return passphrase',
    'f-srtReturnPassphrase',
    srtReturnPassphraseInput,
    'Not the SRT passphrase above — this is the key to the return output.',
  );
  const srtReturnPassphraseBadge = document.createElement('span');
  srtReturnPassphraseBadge.className = 'secret-badge';
  srtReturnPassphraseRow.wrap.insertBefore(srtReturnPassphraseBadge, srtReturnPassphraseRow.errorEl);
  fields.srtReturnPassphrase = {
    input: srtReturnPassphraseInput,
    errorEl: srtReturnPassphraseRow.errorEl,
  };
  currentGroup.appendChild(srtReturnPassphraseRow.wrap);

  const tileHeading = document.createElement('h3');
  tileHeading.textContent = 'Monitor tile (in the 2240x1440 mosaic)';
  currentGroup.appendChild(tileHeading);
  const tileGrid = document.createElement('div');
  tileGrid.className = 'tile-grid';
  const tileX = numberInput('f-tileX');
  const tileY = numberInput('f-tileY');
  const tileW = numberInput('f-tileW');
  const tileH = numberInput('f-tileH');
  for (const [label, input, key] of [
    ['X', tileX, 'monitorTile.x'],
    ['Y', tileY, 'monitorTile.y'],
    ['W', tileW, 'monitorTile.w'],
    ['H', tileH, 'monitorTile.h'],
  ]) {
    const { wrap, errorEl } = row(label, input.id, input);
    fields[key] = { input, errorEl };
    tileGrid.appendChild(wrap);
  }
  currentGroup.appendChild(tileGrid);

  // --- slate ---------------------------------------------------------
  const slateHeading = document.createElement('h2');
  slateHeading.textContent = 'Slate';
  openGroup(slateHeading);
  addField('slatePath', 'Slate image path', textInput('f-slatePath'), 'Blank = the bundled slate.png.');

  // --- remote access -----------------------------------------------------
  //
  // THE LAST GROUP, and deliberately STATUS-ONLY. The listener is unauthenticated
  // and ON by default, bound to all interfaces, by the owner's decision — the
  // dedicated private facility network is the access control (docs/remote-access.md).
  // There is no login, no client accounts and no capability tiers to manage, so
  // this group manages nothing: it is a read-only readout of whether the listener
  // is running and which HTTP/HTTPS ports it is bound on, from GetRemoteState.
  //
  // Deliberately NOT a warning, a secure-context note or any explanatory prose:
  // the owner asked that the app UI show only the bound-port status, and that the
  // risk write-up live in docs/remote-access.md alone. Nothing here is part of the
  // config document either — the listener's own state lives in a Go-owned file
  // (internal/remote), reached through the host-only GetRemoteState.
  //
  // The WHOLE GROUP IS HIDDEN ON A REMOTE CLIENT. GetRemoteState is host-only —
  // pruned from a remote browser's window.go.main.App — so a remote seat cannot
  // read it anyway; hiding the group is the visible half of that.
  const isRemoteView = backend.isRemoteClient();
  const remoteSupported = backend.usingFakeBackend || backend.remoteAvailable();

  const remoteHeading = document.createElement('h2');
  remoteHeading.textContent = 'Remote access';
  const remoteGroup = openGroup(remoteHeading, 'settings-group--remote');
  remoteGroup.hidden = isRemoteView;

  // Two read-only lines: on/off, and — when the listener is actually bound — the
  // HTTP and HTTPS addresses to reach it on. Selectable, monospaced; see
  // .remote-readout in main.css.
  const remoteStatusLine = document.createElement('p');
  remoteStatusLine.className = 'remote-readout';
  currentGroup.appendChild(remoteStatusLine);
  const remoteBoundLine = document.createElement('p');
  remoteBoundLine.className = 'remote-readout';
  currentGroup.appendChild(remoteBoundLine);

  // --- actions ---------------------------------------------------------
  const saveMessage = document.createElement('p');
  saveMessage.className = 'save-message';
  saveMessage.hidden = true;

  const actions = document.createElement('div');
  actions.className = 'settings-actions';
  const saveBtn = document.createElement('button');
  saveBtn.type = 'submit';
  saveBtn.className = 'btn btn-primary';
  saveBtn.textContent = 'Save settings';
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-ghost';
  cancelBtn.textContent = 'Cancel';
  cancelBtn.addEventListener('click', () => leaveSettings());
  actions.append(saveBtn, cancelBtn);

  // The action bar STAYS ON SCREEN. It used to sit at the foot of ~3,500px of
  // form: displayErrors() focuses the first bad field and the operator then had
  // to go and find Save. Buttons first, message beside them, so the primary
  // action does not move when the message is long.
  const footer = document.createElement('div');
  footer.className = 'settings-footer';
  footer.append(actions, saveMessage);
  form.appendChild(footer);

  el.append(header, form);

  /** leaveSettings is the only way out of this screen. */
  function leaveSettings() {
    handlers.onBack();
  }

  // --- the pasted address --------------------------------------------

  function hideLiveURLMessages() {
    liveURLNote.hidden = true;
    liveURLNote.textContent = '';
    liveURLRow.errorEl.hidden = true;
    liveURLRow.errorEl.textContent = '';
  }

  /**
   * applyLiveURL parses whatever is in the address field and fills the hidden
   * fields below it. It reports what it read, so a paste is visibly a paste
   * and not a hope, and it refuses clearly rather than silently doing nothing.
   *
   * Either form is accepted. A bare instance address sets the host and LEAVES
   * THE EVENT ID ALONE — it does not clear it, because the stored id is the
   * fallback for every case where the instance cannot be enumerated, and
   * blanking it on a keystroke would throw away the one value that works
   * offline. The picker below overwrites it when the instance answers.
   */
  function applyLiveURL() {
    const raw = liveURLInput.value.trim();
    if (raw === '') {
      hideLiveURLMessages();
      return;
    }

    const parsed = parseM2LXAddress(raw);
    if (!parsed.ok) {
      liveURLNote.hidden = true;
      liveURLRow.errorEl.textContent = parsed.error;
      liveURLRow.errorEl.hidden = false;
      return;
    }

    liveURLRow.errorEl.hidden = true;
    fields.m2lxHost.input.value = parsed.host;
    if (parsed.eventId !== '') fields.eventId.input.value = parsed.eventId;
    showDerived();

    // Then either ask the instance what its events are, or say plainly why it
    // cannot be asked yet. Saying nothing is what the old field did, and it is
    // how an operator ends up looking at a screen that has quietly decided
    // nothing. The prompt is skipped when the address itself supplied an id:
    // that id belongs to the host beside it and needs no instance to confirm it.
    if (parsed.host === savedM2LXHost) {
      scheduleEventRefresh(parsed.host);
    } else if (parsed.eventId === '') {
      liveURLNote.textContent +=
        ' Press Save settings to sign in to this instance; its events are listed once it answers.';
    }
  }

  // 'input' rather than 'change': a paste should fill the fields the moment it
  // lands, not when focus leaves.
  liveURLInput.addEventListener('input', applyLiveURL);
  liveURLInput.addEventListener('change', applyLiveURL);

  // --- populate / collect --------------------------------------------

  function populate(config) {
    // FIRST, before a single box is written. A preview holds the operator's own
    // values so it can give them back, and it is a diff against the config
    // below — so a preview that outlived a populate() would restore stale
    // values over fresh ones and describe a comparison that no longer exists.
    // One line here covers every caller: open(), a failed load, and the apply.
    clearPresetPreview();
    // The diff baseline for Apply. Recorded BEFORE the form gets a chance to be
    // edited: the confirm dialog compares a preset against what is SAVED, not
    // against keystrokes that were never saved.
    lastLoadedConfig = config;
    // What the backend is configured with, which is what its API client is
    // signed in to. Kept beside the fields rather than read back out of them,
    // because the fields are about to be edited and these two must not follow.
    savedM2LXHost = (config.m2lxHost || '').trim();
    savedEventId = (config.eventId || '').trim();
    fields.m2lxHost.input.value = config.m2lxHost || '';
    fields.alias.input.value = config.alias || '';
    fields.eventId.input.value = config.eventId || '';
    fields.srtPort.input.value = String(config.srtPort ?? 0);
    fields.srtLatencyMs.input.value = String(config.srtLatencyMs ?? 120);
    fields.pbkeylen.input.value = String(config.pbkeylen ?? 0);
    // `||`, not `??`, for the reason given at srtReturnPort below: 0 is what
    // internal/config.EffectiveVideoBitrateKbps substitutes the default FOR, and
    // a form showing 0 would be showing a bitrate the encoder never uses.
    fields.videoBitrateKbps.input.value = String(
      config.videoBitrateKbps || blankConfig().videoBitrateKbps,
    );
    // No default substituted, and that is not an oversight: EMPTY IS A SETTING
    // here. It means "read the format from the switcher", it is what every
    // existing installation holds, and writing 1920x1080p50 into the box on the
    // operator's behalf would turn the derivation off on every machine that
    // opened Settings once.
    // EMPTY IS A SETTING here — it means "read the format from the switcher" —
    // so no default is substituted, and planVideoFormats keeps a saved raster
    // this build's list does not carry rather than letting the <select> discard
    // it. The list is rebuilt on every populate for that reason alone: which
    // options exist depends on what is saved.
    fillGroupedSelect(
      fields.videoFormatOverride.input,
      planVideoFormats(config.videoFormatOverride || ''),
      planVideoFormats('').follow,
    );
    renderVideoFormat();
    fields.statusKey.input.value = config.statusKey || '';
    // WHAT GOES TO AIR, normalised on the way IN as well as out. An unrecognised
    // value assigned to a <select> selects nothing at all, and the next Save
    // would write that nothing back — which for this field means an empty
    // videoSource, which config.EffectiveVideoSource then reads as the slate. A
    // camera taken off air by a form that could not read its own file.
    fields.videoSource.input.value = normaliseVideoSource(config.videoSource);
    // Read with .checked, and normalised, because anything that is not exactly
    // `true` is off: this is a branch added to a pipeline that is going to air,
    // and "the file held a shape we did not recognise" is not a reason to add
    // one. renderVideoSource follows both, because assigning either control from
    // script fires no event.
    fields.decklinkPreviewEnabled.input.checked = normalisePreviewEnabled(
      config.decklinkPreviewEnabled,
    );
    renderVideoSource();
    fields.audioDeviceId.input.value = config.audioDeviceId || '';
    // Normalised on the way in: an unrecognised kind would otherwise reach
    // planAudioInputs, which would file the selection under the wrong group and
    // then find it absent from it.
    fields.audioSourceKind.input.value = normaliseAudioSourceKind(config.audioSourceKind);
    fields.decklinkPersistentId.input.value = config.decklinkPersistentId || '';
    // The picker is drawn from those three fields, never the other way round, so
    // this must come after all three. It does not write them back — see
    // renderAudioInput on why a saved-but-absent device stays saved.
    renderAudioInput();
    // The routing map is held by the grid, not by a field: it is a LIST of
    // contributions and there is no box to put one in. It goes through the same
    // populate/collect pair as everything else all the same — see collectConfig,
    // where NOT restating it would delete a commentator's channel assignment on
    // the next Save. renderChannelMapGroup follows it because assigning the
    // picker's <select> above fires neither 'input' nor 'change'; it will hide
    // the group until a pad report for this device arrives, which is the
    // honest state of a screen that has just been handed a configuration and
    // has not yet been told what negotiated.
    //
    // ONE DEVICE'S ROUTING IS DRAWN AND THE REST ARE CARRIED. The grid can only
    // ever show the device this seat is capturing from — it is sized to the
    // width that device's pad negotiated — so every other key in the store is
    // held untouched between here and collectChannelMaps. This must come after
    // the three device fields above, because the key is built from them.
    //
    // THE KEY GOES IN WITH THE MAP, and not as decoration: collect() will not
    // write a grid back under a device whose map it is not holding, and the only
    // way it can know which that is, is to be told when the map arrives.
    carriedChannelMaps = adoptChannelMaps(config.channelMaps);
    const savedRoutingKey = currentAudioDeviceKey();
    channelMapView.setMap(carriedChannelMaps[savedRoutingKey], savedRoutingKey);
    renderChannelMapGroup();
    fields.headphoneDeviceId.input.value = config.headphoneDeviceId || '';
    fields[DEVICE_KEY_SRT].input.value = config[DEVICE_KEY_SRT] || '';
    // The cough mute's mode: read so that it can be restated, never normalised
    // here. An EMPTY value is documented on the Go side as "not chosen" and is
    // what carryForwardUIOnlyFields keys off; substituting the default in this
    // form would turn "the operator has not chosen" into "the operator chose
    // push", which is a preference this screen has no business inventing.
    fields.coughMuteMode.input.value = config.coughMuteMode || '';
    fields.returnMid.input.value = String(
      isValidReturnMid(config.returnMid) ? config.returnMid : DEFAULT_RETURN_MID,
    );
    fields.returnChannel.input.value = normaliseChannelMode(config.returnChannel);
    fields.srtReturnPBKeyLen.input.value = String(config.srtReturnPBKeyLen ?? 0);
    // `||`, not `??`: a stored 0 must show as the default, because 0 is what
    // internal/config.EffectiveSRTReturnPort substitutes the default FOR. A
    // form that showed 0 would be showing a port the return never dials, and
    // the validator below would then refuse to save the screen it just drew.
    fields.srtReturnPort.input.value = String(config.srtReturnPort || blankConfig().srtReturnPort);
    // `||`, not `??`, for the reason above: 0 is what
    // internal/config.EffectivePictureLatencyMs substitutes the default FOR, and
    // every config.json written before this field existed holds 0. Showing 0
    // would show a latency the monitor never dials with, and the validator
    // below would then refuse to save the screen it had just drawn.
    fields.pictureLatencyMs.input.value = String(
      config.pictureLatencyMs || blankConfig().pictureLatencyMs,
    );
    // Held, not shown. See the note beside the returnChannel field.
    carriedReturnSource = normaliseReturnSource(config.returnSource);
    fields.returnGainDb.input.value = String(config.returnGainDb ?? 18);
    const tile = config.monitorTile || { x: 0, y: 360, w: 640, h: 360 };
    fields['monitorTile.x'].input.value = String(tile.x ?? 0);
    fields['monitorTile.y'].input.value = String(tile.y ?? 360);
    fields['monitorTile.w'].input.value = String(tile.w ?? 640);
    fields['monitorTile.h'].input.value = String(tile.h ?? 360);
    fields.slatePath.input.value = config.slatePath || 'slate.png';
    fields.m2lxPassword.input.value = '';
    fields.srtPassphrase.input.value = '';
    fields.srtReturnPassphrase.input.value = '';
    // THE BASE ADDRESS, never a live-operation URL. This line used to pass the
    // event id as well, and formatM2LXAddress then re-synthesised
    // https://<host>/live-operation/<id> — so an operator who pasted the
    // instance's address got a longer one back on the next load. The event id
    // is not lost by this: it is in fields.eventId, on the derived line below
    // and on the event picker's own row.
    liveURLInput.value = formatM2LXAddress(config.m2lxHost);
    // ORDER MATTERS, and it was the wrong way round: hideLiveURLMessages()
    // clears the derived line as well as the error, so calling it after
    // showDerived() blanked the one thing on this row that says which host and
    // event the screen is holding — on a screen where both fields are hidden.
    // Clear first, then draw.
    hideLiveURLMessages();
    showDerived();
    refreshSecretBadges();
    clearAllErrors();
    saveMessage.hidden = true;
  }

  function refreshSecretBadges() {
    const m2lxSet = backend.isSecretSetThisSession(backend.SECRET_KEY_M2LX);
    const srtSet = backend.isSecretSetThisSession(backend.SECRET_KEY_SRT);
    const srtReturnSet = backend.isSecretSetThisSession(backend.SECRET_KEY_SRT_RETURN);
    m2lxPasswordBadge.textContent = m2lxSet ? 'set' : 'not set';
    m2lxPasswordBadge.classList.toggle('secret-badge-set', m2lxSet);
    srtPassphraseBadge.textContent = srtSet ? 'set' : 'not set';
    srtPassphraseBadge.classList.toggle('secret-badge-set', srtSet);
    // Read through the same write-only signal as the other two: "set" means
    // this field was saved during THIS run of the app, never that Credential
    // Manager was consulted. There is no getter for a secret anywhere.
    srtReturnPassphraseBadge.textContent = srtReturnSet ? 'set' : 'not set';
    srtReturnPassphraseBadge.classList.toggle('secret-badge-set', srtReturnSet);
  }

  function collectConfig() {
    return {
      m2lxHost: fields.m2lxHost.input.value.trim(),
      alias: fields.alias.input.value.trim(),
      eventId: fields.eventId.input.value.trim(),
      srtPort: Number(fields.srtPort.input.value),
      srtLatencyMs: Number(fields.srtLatencyMs.input.value),
      pbkeylen: Number(fields.pbkeylen.input.value),
      videoBitrateKbps: Number(fields.videoBitrateKbps.input.value),
      // Trimmed and otherwise sent VERBATIM — never normalised into the
      // canonical spelling on the way through. What the operator typed is what
      // Go parses and what Go quotes back if it cannot: a form that silently
      // rewrote "1920X1080P50" would be answering a different question from the
      // one on screen, and the first value it could not rewrite would be the one
      // that mattered.
      videoFormatOverride: fields.videoFormatOverride.input.value.trim(),
      statusKey: fields.statusKey.input.value.trim(),
      // THE FIELD THAT DECIDES WHAT THE SWITCHER RECEIVES, and therefore the one
      // on this form whose omission would cost the most: saveConfig REPLACES the
      // stored document, so a collectConfig that failed to restate videoSource
      // would put a live position's camera back on the slate because somebody
      // corrected a typo in the event id — silently, with every lamp green.
      // Normalised, for the reason populate normalises it.
      videoSource: normaliseVideoSource(fields.videoSource.input.value),
      // A REAL BOOLEAN. .checked, never .value — a checkbox's .value is the
      // string "on" whether or not it is ticked, and Go's field is a bool.
      decklinkPreviewEnabled: fields.decklinkPreviewEnabled.input.checked === true,
      audioDeviceId: fields.audioDeviceId.input.value.trim(),
      audioSourceKind: normaliseAudioSourceKind(fields.audioSourceKind.input.value),
      decklinkPersistentId: fields.decklinkPersistentId.input.value.trim(),
      // One routing per device, keyed by the device. Each is a list of
      // {output, input, gain} naming only channels the capture pad negotiated,
      // each cell at most once — the grid is incapable of producing anything
      // else, which is the point of it, because both of those are maps
      // internal/gst refuses by name.
      //
      // Collected even while the group is hidden, and every OTHER device's
      // routing is carried through: this form replaces the whole document, so a
      // seat that has plugged in a USB microphone would otherwise delete the
      // routing of the card it uses next week — and would do it with the card's
      // grid nowhere on screen. See collectChannelMaps.
      channelMaps: collectChannelMaps(),
      headphoneDeviceId: fields.headphoneDeviceId.input.value.trim(),
      [DEVICE_KEY_SRT]: fields[DEVICE_KEY_SRT].input.value.trim(),
      // The cough mute's mode. Chosen on the MAIN SCREEN, restated here, for the
      // reason every hidden field on this form exists: saveConfig REPLACES the
      // stored document, so a field this form does not restate is a field this
      // form deletes. An operator who set latch and then saved a port number
      // would find their cough button back on push — mid-match, discovered by
      // pressing it.
      coughMuteMode: fields.coughMuteMode.input.value.trim(),
      returnMid: Number(fields.returnMid.input.value),
      returnChannel: normaliseChannelMode(fields.returnChannel.input.value),
      srtReturnPort: Number(fields.srtReturnPort.input.value),
      pictureLatencyMs: Number(fields.pictureLatencyMs.input.value),
      srtReturnPBKeyLen: Number(fields.srtReturnPBKeyLen.input.value),
      // Carried through from the loaded config, not collected from a control.
      // See the declaration above for why. It is the LAST such field; every
      // other one on this screen now has a control.
      returnSource: carriedReturnSource,
      monitorTile: {
        x: Number(fields['monitorTile.x'].input.value),
        y: Number(fields['monitorTile.y'].input.value),
        w: Number(fields['monitorTile.w'].input.value),
        h: Number(fields['monitorTile.h'].input.value),
      },
      returnGainDb: Number(fields.returnGainDb.input.value),
      slatePath: fields.slatePath.input.value.trim(),
    };
  }

  function clearAllErrors() {
    for (const { errorEl } of Object.values(fields)) {
      errorEl.hidden = true;
      errorEl.textContent = '';
    }
  }

  // Errors against fields with no row on screen surface where the operator can
  // act on them. m2lxHost and eventId are hidden inputs derived from the
  // address, so their errors belong on the address row; a hidden field's own
  // errorEl is attached to nothing and an error written there is a save that
  // fails with no visible reason.
  //
  // THE THREE COMMENTARY-INPUT FIELDS ARE HERE FOR THE SAME REASON, and they
  // joined this table the moment the one picker replaced their controls. All
  // three are hidden inputs written by that picker, and validateConfig can still
  // refuse two of them on a value the picker itself would never produce:
  //
  //   decklinkPersistentId  a stored "0" — the small integer Blackmagic's own
  //                         tools show beside a card, which the free-text box
  //                         this control replaced was perfectly happy to accept.
  //                         It is an enumeration index and names a different
  //                         card after a replug, so validate.js refuses it.
  //   audioDeviceId         a stored Windows RENDER (playback) endpoint GUID —
  //                         the operator's measured failure, saved before the
  //                         dropdown filter existed to prevent it.
  //
  // Both arrive from a config.json written by an older build or edited by hand,
  // and both render on the picker as "NOT PRESENT — <id>". Without this table
  // their errors would be written to an errorEl attached to nothing: a Save that
  // fails, a message saying to fix the highlighted fields, and no highlighted
  // field anywhere on the screen. Choosing any device clears the offending value
  // outright, so the row the error lands on is also the row that fixes it.
  const ERROR_SURROGATES = Object.freeze({
    m2lxHost: () => ({ errorEl: liveURLRow.errorEl, input: liveURLInput }),
    eventId: () => ({ errorEl: liveURLRow.errorEl, input: liveURLInput }),
    audioSourceKind: () => ({ errorEl: audioInputRow.errorEl, input: audioInputSelect }),
    audioDeviceId: () => ({ errorEl: audioInputRow.errorEl, input: audioInputSelect }),
    decklinkPersistentId: () => ({ errorEl: audioInputRow.errorEl, input: audioInputSelect }),
  });

  function displayErrors(errors) {
    clearAllErrors();
    // The surrogate rows are cleared BY HAND, because clearAllErrors walks
    // `fields` and neither of these rows is a field — they are the visible homes
    // of fields that have none. Missing one here is not a missing clear, it is a
    // stale error: last save's message left standing under a row the operator
    // has since corrected.
    for (const row of [liveURLRow, audioInputRow]) {
      row.errorEl.hidden = true;
      row.errorEl.textContent = '';
    }
    let first = null;
    for (const [key, message] of Object.entries(errors)) {
      const field = ERROR_SURROGATES[key] ? ERROR_SURROGATES[key]() : fields[key];
      if (!field) continue;
      // Two derived errors share the address row; the second appends rather
      // than silently replacing the first.
      field.errorEl.textContent = field.errorEl.textContent
        ? `${field.errorEl.textContent} ${message}`
        : message;
      field.errorEl.hidden = false;
      if (!first) first = field.input;
    }
    if (first) first.focus();
  }

  function setSaveMessage(message, isError) {
    saveMessage.textContent = message;
    saveMessage.hidden = false;
    saveMessage.classList.toggle('save-message-error', isError);
    saveMessage.classList.toggle('save-message-ok', !isError);
  }

  async function handleSave() {
    saveMessage.hidden = true;
    // THE LINE THAT MAKES "A PREVIEW IS NOT AN EDIT" TRUE. The preview writes
    // the preset's values into the real controls, so collectConfig would
    // otherwise save them — half-applying a preset from a button that says Save
    // settings, with no confirmation, no credential scope and no monitor
    // rebuild, which is the worst possible way for an instance switch to
    // happen. Withdrawn first; the save writes what the form actually holds.
    const hadPreview = presetPreview !== null;
    clearPresetPreview();
    const config = collectConfig();
    const errors = validateConfig(config);
    if (Object.keys(errors).length > 0) {
      displayErrors(errors);
      setSaveMessage('Fix the highlighted fields before saving.', true);
      return;
    }
    clearAllErrors();
    saveBtn.disabled = true;
    // Whether this save moves the app to a different instance decides how hard
    // it is worth trying to list events afterwards; read before the save, used
    // after it.
    const hostChanged = config.m2lxHost !== savedM2LXHost;
    let saved = false;
    try {
      await backend.saveConfig(config);
      const m2lxPassword = fields.m2lxPassword.input.value;
      const srtPassphrase = fields.srtPassphrase.input.value;
      const srtReturnPassphrase = fields.srtReturnPassphrase.input.value;
      if (m2lxPassword.length > 0) {
        await backend.setSecret(backend.SECRET_KEY_M2LX, m2lxPassword);
        fields.m2lxPassword.input.value = '';
      }
      if (srtPassphrase.length > 0) {
        await backend.setSecret(backend.SECRET_KEY_SRT, srtPassphrase);
        fields.srtPassphrase.input.value = '';
      }
      // Its own key, its own Credential Manager entry. Writing this must not
      // touch SECRET_KEY_SRT: a working feed is not something to break while
      // fixing a pair of headphones.
      if (srtReturnPassphrase.length > 0) {
        await backend.setSecret(backend.SECRET_KEY_SRT_RETURN, srtReturnPassphrase);
        fields.srtReturnPassphrase.input.value = '';
      }
      refreshSecretBadges();
      savedM2LXHost = config.m2lxHost;
      savedEventId = config.eventId;
      saved = true;
      // Say that the green went, and why. An operator who had a preview on
      // screen just watched several boxes revert as they pressed Save, and
      // silence there reads as the save having gone wrong.
      setSaveMessage(
        hadPreview
          ? 'Settings saved. The preset preview was withdrawn — it was never part of the form; ' +
              'press Apply to switch to that instance.'
          : 'Settings saved.',
        false,
      );
      handlers.onSaved(config);
    } catch (err) {
      setSaveMessage(`Could not save settings: ${err.message}`, true);
    } finally {
      saveBtn.disabled = false;
    }

    // Saving a bare instance address is the moment the app can first sign in to
    // it, so it is the moment its events become listable — and the operator who
    // typed that address has nothing yet in the event field. Deliberately NOT
    // awaited: the save is finished and reported, and a picker that fills in a
    // second later must not hold the Save button down while it waits.
    if (saved) void refreshEventsAfterSave(hostChanged);
  }

  // --- remote access status ---------------------------------------------

  /**
   * renderRemoteState paints the two read-only status lines from GetRemoteState.
   * There is nothing to configure here — the listener is unauthenticated and its
   * state is Go-owned — so this only reports. "Running" is derived from a
   * non-empty certFingerprint (there is no running field); when running, the
   * ports and URLs are the ACTUALLY bound values, so an 80/443 that fell back to
   * 8080/8443 shows the port a browser must actually use.
   */
  function renderRemoteState(state) {
    const s = state || {};
    if (s.enabled !== true) {
      remoteStatusLine.textContent = 'Remote access: off.';
      remoteBoundLine.textContent = '';
      return;
    }
    const running = typeof s.certFingerprint === 'string' && s.certFingerprint !== '';
    if (!running) {
      remoteStatusLine.textContent = 'Remote access: on, listener not bound.';
      remoteBoundLine.textContent = '';
      return;
    }
    remoteStatusLine.textContent = 'Remote access: on.';
    const http = s.httpURL || (s.httpPort ? `port ${s.httpPort}` : '');
    const https = s.httpsURL || (s.httpsPort ? `port ${s.httpsPort}` : '');
    remoteBoundLine.textContent = `Bound on HTTP ${http} · HTTPS ${https}`;
  }

  /**
   * refreshRemote reads the remote-access state and renders it. It is a no-op on
   * a remote client (the group is hidden and GetRemoteState is not even bound
   * there), and degrades to a single line against a build that has no
   * remote-access bindings — the same all-or-nothing availability rule the
   * presets and picture controls use.
   */
  async function refreshRemote() {
    if (isRemoteView) return;
    if (!remoteSupported) {
      remoteStatusLine.textContent = 'This build has no remote access.';
      remoteBoundLine.textContent = '';
      return;
    }
    try {
      renderRemoteState(await backend.getRemoteState());
    } catch (err) {
      remoteStatusLine.textContent = `Could not read the remote-access status: ${err.message}`;
      remoteBoundLine.textContent = '';
    }
  }

  async function open() {
    saveBtn.disabled = true;
    renderSuggestions([]);
    try {
      const config = await backend.getConfig();
      populate(config);
    } catch (err) {
      populate(blankConfig());
      setSaveMessage(`Could not load the current configuration: ${err.message}`, true);
    } finally {
      saveBtn.disabled = false;
    }

    // WHAT CAPTURE HARDWARE THIS MACHINE HAS, so the video-source control can
    // refuse a configuration that cannot start. After the config load, because
    // it renders against the source that load put in the box; separately from
    // it, and swallowing its own failure, because a device listing that cannot
    // be made must leave the rest of the screen intact — it decides a WARNING,
    // and a warning is not worth a blank form.
    await refreshInputDevices();

    // AND WHAT FORMAT THE FEED IS BEING PRODUCED IN, so the format control can
    // show the operator when their override disagrees with it. After the config
    // load, because it renders against the override that load put in the box.
    // backend.getConformTarget never throws — null is its answer for every way
    // of not knowing — so it needs no try/catch of its own.
    await refreshConformTarget();

    // The capture pad's width, after the config load so the saved map is in
    // place to be conformed against it, and before everything else because the
    // routing grid is the one thing on this screen that draws nothing at all
    // until it arrives. getChannelMap never throws — it answers "no channels"
    // for every way of not knowing — so it needs no try/catch of its own.
    adoptChannelMapState(await backend.getChannelMap());

    // Separately from the config load, and after it, so a failure to reach one
    // does not blank the other.
    try {
      renderSuggestions(await backend.getStatusKeyCandidates());
    } catch (err) {
      console.error('wslcomms: could not fetch statusKey suggestions', err);
    }

    // The instance's events, after the config load so the URL-derived id is in
    // place as the fallback and the picker's default. Its own try/catch inside —
    // an instance the app cannot enumerate must leave the URL-derived behaviour
    // untouched, never blank the screen.
    await refreshEvents();

    // And the instance picker, separately again and last: a preset listing
    // failure must not stop the operator editing the fields it would have
    // filled in.
    await refreshPresets();

    // The remote-access group, last and separately again for the same reason: a
    // failure to read it must not blank the rest of the screen. A no-op on a
    // remote client (the group is hidden there).
    await refreshRemote();
  }

  return { el, open, setSending, adoptVideoLeg, adoptAudioLeg };
}
