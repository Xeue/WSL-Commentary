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
// The DeckLink routing grid, its per-channel meters and the card's video lamp.
// It is a screen inside this screen — see the "DeckLink channel routing" group
// below for why it is a settings group and not a view of its own, and
// channelmap.js's header for why the grid is sized the way it is.
import { createChannelMapView } from './channelmap.js';

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
    // An EMPTY list, not a default map, and the difference is gst.ChannelMap's:
    // the zero value means "nobody has chosen yet" and RESOLVES to channel 1
    // left, channel 2 right — bit for bit what every seat carried before this
    // grid existed. Writing the default out here instead would turn "not chosen"
    // into "chosen" on every machine that ever opened Settings, after which
    // nothing could tell an operator whether anyone had set the routing.
    decklinkChannelMap: [],
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

// The two commentary capture kinds, spelled exactly as internal/config spells
// them (config.AudioSourceNative / config.AudioSourceDeckLink). They are the
// <option> values and the value collectConfig sends, so a drift here is the
// silent kind: Go's Validate refuses an unrecognised kind by name, which is at
// least loud, but a kind that differs only in case would save cleanly and
// capture from the wrong subsystem.
//
// They live at module scope in this file rather than in a shared module because
// this screen is the only thing that asks the question today. If a second screen
// ever needs them, they belong in a pure module of their own — the house pattern
// is returnsource.js and channels.js — not copied.
const AUDIO_SOURCE_NATIVE = 'native';
const AUDIO_SOURCE_DECKLINK = 'decklink';

/**
 * normaliseAudioSourceKind maps anything to one of the two kinds, defaulting to
 * native — which is what every machine did before the field existed.
 *
 * It is applied on the way IN as well as on the way out, and the way in is the
 * one that would otherwise bite: assigning an unrecognised value to a <select>
 * leaves it showing '' — no option selected — and collectConfig would then save
 * an empty kind over whatever the file actually held. normaliseChannelMode is
 * used the same way, three lines further down populate, for the same reason.
 */
function normaliseAudioSourceKind(value) {
  return value === AUDIO_SOURCE_DECKLINK ? AUDIO_SOURCE_DECKLINK : AUDIO_SOURCE_NATIVE;
}

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
    'The instance address is enough — the event is found from the instance and chosen below. ' +
      'A full live-operation URL is also accepted, and fills the event in from its own path; ' +
      'this box then shows the instance address on its own, which is all it needs.',
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
  const eventSelectRow = row(
    'Event',
    'f-eventSelect',
    eventSelect,
    'This instance is running more than one event — choose which one this seat controls.',
  );
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

  // WHY THIS IS A CONTROL AND NOT A CONSTANT. It was 2000, and 2000 was chosen
  // for what the video leg used to be — one still PNG through imagefreeze, where
  // there is nothing for a bitrate to be spent on. The operator has ruled it too
  // low for live video and wants nearer 10000. The default stays 2000 so that
  // adding this box changes nothing until somebody types in it.
  addField(
    'videoBitrateKbps',
    'Video bitrate (kbps)',
    numberInput('f-videoBitrateKbps'),
    'Kilobits per second. Default 2000, which suits a still slate; live video wants nearer 10000. ' +
      'It is the uplink from this seat to the switcher that has to carry it.',
  );

  // THE FALLBACK CONFORM TARGET, and the hint has to say what "fallback" means
  // or the box reads as "force the format to this".
  //
  // The app derives the format from the switcher whenever any node is streaming.
  // MEASURED on the live instance: when nothing is streaming every node reports
  // no format at all, and no REST endpoint states the instance's configured
  // format either — so a position that comes up first, which is the normal case
  // an hour before kick-off, has nothing to derive from. This is what it falls
  // back to. Blank leaves the derivation to do its job.
  addField(
    'videoFormatOverride',
    'Video format when the switcher cannot be read',
    textInput('f-videoFormatOverride'),
    'Blank = read the format from the switcher, which works whenever anything is streaming into it. ' +
      'Fill this in for the case where nothing is: write it as 1920x1080p50 — width x height, then ' +
      'p and the frame rate (50, 25, 59.94). It describes how the SWITCHER is set up, so every ' +
      'position at a venue has the same answer.',
  );

  // --- status ---------------------------------------------------------
  const statusHeading = document.createElement('h2');
  statusHeading.textContent = 'Status';
  openGroup(statusHeading);
  addField(
    'statusKey',
    'Status key — optional',
    textInput('f-statusKey'),
    'Our router input in switcher_status, e.g. "cam7". Blank = the three lamps read NO STATUS.',
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
      suggestionsIntro.textContent =
        'No suggestion yet. Leave this blank, go back and press START: the app watches every ' +
        'switcher_status node and offers the one that starts streaming as your feed comes up.';
      return;
    }

    if (list.length === 1) {
      suggestionsIntro.textContent = 'One node started streaming as your feed came up:';
    } else {
      suggestionsIntro.textContent =
        `${list.length} nodes started streaming at the same time, so the app cannot tell which is ` +
        'yours — another operator may have started too. Choose by the video format if one matches ' +
        'your feed, or try again when nobody else is coming up:';
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

  addField(
    'audioSourceKind',
    'Capture from',
    selectInput('f-audioSourceKind', [
      { value: AUDIO_SOURCE_NATIVE, label: 'This computer’s audio devices' },
      { value: AUDIO_SOURCE_DECKLINK, label: 'A Blackmagic DeckLink card' },
    ]),
    'Leave this on the computer’s audio devices unless the microphone arrives on an SDI card. ' +
      'The card’s own audio does not appear in the input list on the main screen, which is why ' +
      'it is chosen here instead.',
  );

  // A FREE-TEXT DEVICE FIELD, WHICH THIS SCREEN OTHERWISE HAS NONE OF — so the
  // reason it is allowed to be one is worth stating.
  //
  // The fields that were removed were removed because a pasted value bypassed
  // the dropdown's filter and failed SILENTLY: a playback endpoint id in
  // audioDeviceId prerolled, then failed asynchronously, and the sender blamed
  // the network and retried for ever. This one cannot fail that way. A card id
  // that names nothing on this machine fails at START, immediately, with the id
  // in the message — and the ordinary answer is to leave the box EMPTY, which
  // means "the card in this machine" and is right on every seat that has one.
  //
  // It is a text box today because nothing enumerates the cards across the Wails
  // boundary yet. When something does, this becomes a dropdown and the box goes.
  addField(
    'decklinkPersistentId',
    'DeckLink card ID — optional',
    textInput('f-decklinkPersistentId'),
    'Blank = the card in this machine, which is the usual answer. Fill it in only on a machine ' +
      'with more than one card. It is the card’s persistent ID, never its device number: a device ' +
      'number is a position in this boot’s enumeration order and means something different after ' +
      'a replug.',
  );

  // --- the DeckLink routing grid ------------------------------------------
  //
  // WHY THIS IS A GROUP ON THIS SCREEN AND NOT A VIEW OF ITS OWN. It is the
  // second half of the question the group above asks: having said the
  // commentary arrives on a card, the operator has to say WHICH of the card's
  // sixteen embedded channels is the commentary. Splitting the two across
  // screens would put the answer somewhere the question does not lead.
  //
  // IT IS ONLY EVER REACHABLE FROM audioSourceKind = decklink, and a seat on a
  // native device must see this screen exactly as it was before the group
  // existed. That is one `hidden` on the whole section — [hidden] is
  // !important in main.css, so it beats .settings-group's display:grid — driven
  // from the control above and from populate(), never from the device list: a
  // machine can have a card in it and still capture from a USB microphone.
  //
  // THE MAP IS STILL COLLECTED WHILE THE GROUP IS HIDDEN. collectConfig
  // replaces the whole document, so a native seat pressing Save on an unrelated
  // field would otherwise DELETE the routing of a card it simply is not using
  // today — the same silent data loss the hidden device-id fields exist to
  // prevent, with a commentator's channel assignment in it.
  const channelMapSupported = backend.usingFakeBackend || backend.channelMapAvailable();

  const channelMapHeading = document.createElement('h2');
  channelMapHeading.textContent = 'DeckLink channel routing';
  const channelMapGroup = openGroup(channelMapHeading, 'settings-group--channelmap');
  // HIDDEN UNTIL A CONFIG SAYS OTHERWISE. renderChannelMapGroup runs on
  // populate(), which is one await after this screen is built, and a group that
  // defaulted to visible would flash a DeckLink routing grid onto the screen of
  // every seat that has never had a card in it.
  channelMapGroup.hidden = true;

  // The half of "live" that the grid itself cannot say. channelmap.js's caveat
  // states that a change reaches the running feed immediately; this states what
  // that does NOT include — the next launch reads config.json, so a routing
  // nobody saved is a routing that lasts until the app is closed.
  const channelMapSaveHint = document.createElement('p');
  channelMapSaveHint.className = 'field-hint channelmap-save-hint';
  channelMapSaveHint.textContent =
    'Changes here reach the running feed the moment you make them. Press Save settings to keep ' +
    'them for the next launch as well.';
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

  // A build with no channel-map bindings says so, rather than showing the
  // grid's "press START once" line — which would be a lie, because no amount of
  // starting will make a binding appear. Same all-or-nothing rule as the presets
  // and picture groups.
  const channelMapUnsupported = document.createElement('p');
  channelMapUnsupported.className = 'field-hint';
  channelMapUnsupported.textContent =
    'This build cannot route the card’s channels — it has no channel-map bindings. The capture ' +
    'takes the card’s first two embedded channels.';
  channelMapUnsupported.hidden = channelMapSupported;
  channelMapView.el.hidden = !channelMapSupported;
  channelMapSaveHint.hidden = !channelMapSupported;
  currentGroup.appendChild(channelMapUnsupported);

  /**
   * renderChannelMapGroup shows or hides the whole group from the capture-kind
   * control. Called from populate() as well as from the control's own event:
   * assigning a <select>'s value from script fires neither 'input' nor 'change',
   * so a screen that only listened would open showing the wrong thing.
   */
  function renderChannelMapGroup() {
    const decklink =
      normaliseAudioSourceKind(fields.audioSourceKind.input.value) === AUDIO_SOURCE_DECKLINK;
    channelMapGroup.hidden = !decklink;
  }

  fields.audioSourceKind.input.addEventListener('change', renderChannelMapGroup);

  /**
   * adoptChannelMapState takes GetChannelMap's report, or the "channelMap"
   * event's, and applies it in the order that matters: the WIDTH first, then the
   * map, so the map is rendered against the width it will actually be sent at.
   *
   * A NON-EMPTY map in the report is the one IN FORCE, and it wins over the one
   * populate() drew from config.json. They are the same on a normal launch; when
   * they differ it is because somebody routed and did not save, and showing the
   * saved one would be showing a routing that is not what the commentator is
   * hearing. An EMPTY map is not adopted, because empty means "nobody has
   * chosen" — it would overwrite a saved map with the absence of one.
   */
  function adoptChannelMapState(state) {
    channelMapView.setPad(state);
    if (state && Array.isArray(state.map) && state.map.length > 0) {
      channelMapView.setMap(state.map);
    }
  }

  // The pad can negotiate while this screen is open — it does so at START — so
  // the grid follows the event as well as the open() call. Subscribed here, in
  // the same place and for the same reason as onStatusKeyCandidates above: the
  // screen that needs the value is the one that asks for it.
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
    '40501 pgm (dirty, the default) / 40502 pvw (encrypted) / 40503 cln (encrypted) / 40504+ relays. ' +
      'Encrypted outputs need the key and passphrase below.',
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
    'Default 120. SRT uses the larger end: this M2L-X output sets 300 ms, so below that, ' +
      'change it on M2L-X.',
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
    fields.videoFormatOverride.input.value = config.videoFormatOverride || '';
    fields.statusKey.input.value = config.statusKey || '';
    fields.audioDeviceId.input.value = config.audioDeviceId || '';
    // Normalised on the way in: an unrecognised kind assigned to a <select>
    // shows nothing at all, and a save would then write that nothing back.
    fields.audioSourceKind.input.value = normaliseAudioSourceKind(config.audioSourceKind);
    fields.decklinkPersistentId.input.value = config.decklinkPersistentId || '';
    // The routing map is held by the grid, not by a field: it is a LIST of
    // contributions and there is no box to put one in. It goes through the same
    // populate/collect pair as everything else all the same — see collectConfig,
    // where NOT restating it would delete a commentator's channel assignment on
    // the next Save. renderChannelMapGroup follows it because assigning the
    // capture-kind <select> above fires neither 'input' nor 'change'.
    channelMapView.setMap(config.decklinkChannelMap);
    renderChannelMapGroup();
    fields.headphoneDeviceId.input.value = config.headphoneDeviceId || '';
    fields[DEVICE_KEY_SRT].input.value = config[DEVICE_KEY_SRT] || '';
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
      audioDeviceId: fields.audioDeviceId.input.value.trim(),
      audioSourceKind: normaliseAudioSourceKind(fields.audioSourceKind.input.value),
      decklinkPersistentId: fields.decklinkPersistentId.input.value.trim(),
      // A list of {output, input, gain} naming only channels the capture pad
      // negotiated, each cell at most once — the grid is incapable of producing
      // anything else, which is the point of it, because both of those are maps
      // internal/gst refuses by name. Collected even while the group is hidden:
      // this form replaces the whole document, so a native-input seat pressing
      // Save would otherwise delete the routing of the card it uses next week.
      decklinkChannelMap: channelMapView.collect(),
      headphoneDeviceId: fields.headphoneDeviceId.input.value.trim(),
      [DEVICE_KEY_SRT]: fields[DEVICE_KEY_SRT].input.value.trim(),
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
  const ERROR_SURROGATES = Object.freeze({
    m2lxHost: () => ({ errorEl: liveURLRow.errorEl, input: liveURLInput }),
    eventId: () => ({ errorEl: liveURLRow.errorEl, input: liveURLInput }),
  });

  function displayErrors(errors) {
    clearAllErrors();
    liveURLRow.errorEl.hidden = true;
    liveURLRow.errorEl.textContent = '';
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

  return { el, open, setSending };
}
