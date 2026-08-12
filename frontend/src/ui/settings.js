import * as backend from './backend.js';
import { validateConfig } from './validate.js';
import { parseLiveOperationURL, formatLiveOperationURL, bareHost } from './liveurl.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
import { normaliseReturnSource, DEVICE_KEY_SRT } from './returnsource.js';
// The instance-preset model: the diff for the confirm dialog, the whitelist
// filter that mirrors Go's, and the machine-field LABELS for the permanent
// note. Pure — presets.js cannot name a machine field's tag, by design, so
// the tag/label pairing happens here, where the four fields already have
// controls. See MACHINE_NOTE_TAGS below.
import { diffPreset, describeIgnoredKeys, filterPresetFields, MACHINE_FIELD_LABELS } from './presets.js';
// The channel table, from the module that enforces it in the Web Audio graph.
// See the note beside the same import in home.js.
import { CHANNEL_MODES, normaliseChannelMode } from '../monitor/channels.js';

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
    srtHost: '',
    srtPort: 0,
    srtLatencyMs: 120,
    pbkeylen: 0,
    statusKey: '',
    audioDeviceId: '',
    headphoneDeviceId: '',
    headphoneEndpointId: '',
    returnMid: 2,
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
    fields[key] = { input, errorEl };
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

  // Which Credential Manager scope is live, and which of its three
  // credentials exist — GetPresetCredentialStatus's booleans, never a value.
  const presetScopeLine = document.createElement('p');
  presetScopeLine.className = 'field-hint preset-scope';
  currentGroup.appendChild(presetScopeLine);

  // THE ONE SENTENCE THIS CARD EXISTS TO MAKE UNMISSABLE, permanent and with
  // the current values shown: the operator SEES that their hardware survived
  // an apply, rather than being asked to trust that it did.
  const presetNote = document.createElement('p');
  presetNote.className = 'preset-note';
  currentGroup.appendChild(presetNote);

  // The four MACHINE tags, paired BY POSITION with presets.js's
  // MACHINE_FIELD_LABELS. The tags are spelled HERE and not there on purpose:
  // presets.js proves by its own source text that it cannot name a machine
  // field, and this file already owns form controls for all four, so the
  // pairing lives beside the fields it describes. settings.test.js asserts
  // the two lists are the same length and that the note names every tag.
  const MACHINE_NOTE_TAGS = ['audioDeviceId', 'headphoneDeviceId', 'headphoneEndpointId', 'slatePath'];

  function renderPresetNote(config) {
    const parts = MACHINE_NOTE_TAGS.map((tag, i) => {
      const value = config ? config[tag] : '';
      return `${MACHINE_FIELD_LABELS[i]}: ${value || '(not set)'}`;
    });
    presetNote.textContent =
      "Never part of a preset — this PC's hardware and files stay put when an instance is " +
      'applied: ' + parts.join(' · ');
  }
  renderPresetNote(null);

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

  function renderPresetScopeLine(status) {
    if (!presetsSupported) {
      presetScopeLine.textContent =
        'This build has no instance presets; the fields below still work as before.';
      return;
    }
    const activeName =
      activePreset && activePreset.id
        ? `Active instance: ${
            (presetSummaries.find((p) => p.id === activePreset.id) || { name: activePreset.id }).name
          }.`
        : 'No instance preset applied.';
    if (!status) {
      presetScopeLine.textContent = activeName;
      return;
    }
    const scopeName = status.scope
      ? `"${status.scope}"`
      : "this machine's original entries";
    const cred = (label, exists) => `${label} ${exists ? 'stored' : 'NOT stored'}`;
    presetScopeLine.textContent =
      `${activeName} Credentials scope: ${scopeName} — ` +
      `${cred('M2L-X password', status.m2lx)} · ${cred('SRT passphrase', status.srt)} · ` +
      `${cred('SRT return passphrase', status.srtreturn)}.`;
  }

  /**
   * refreshPresets re-reads the picker, the active record and the credential
   * status. Each failure degrades its own line rather than blanking the rest,
   * for the same reason open() loads config and suggestions separately.
   */
  async function refreshPresets() {
    if (!presetsSupported) {
      renderPresetButtons();
      renderPresetScopeLine(null);
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
    } catch (err) {
      presetSummaries = [];
      presetSelect.textContent = '';
      setSaveMessage(`Could not list the saved instances: ${err.message}`, true);
    }
    let status = null;
    try {
      status = await backend.getPresetCredentialStatus();
    } catch (err) {
      console.error('wslcomms: could not read the credential status', err);
    }
    renderPresetButtons();
    renderPresetScopeLine(status);
  }

  async function handleApplyPreset() {
    const preset = selectedPresetSummary();
    if (!preset || typeof handlers.onApplyPreset !== 'function') return;

    // The confirm dialog: what will CHANGE, and what the file carries that a
    // preset does not honour. Both computed here, before anything moves, so
    // the operator confirms what will actually happen.
    const changes = diffPreset(lastLoadedConfig, preset.fields);
    const { ignored } = filterPresetFields(preset.fields);
    const lines = changes.map((c) => `  ${c.label}: ${c.from} -> ${c.to}`);
    const ignoredNote = describeIgnoredKeys(ignored);
    const text =
      `Apply the instance "${preset.name}"?\n\n` +
      (lines.length ? `This changes:\n${lines.join('\n')}\n\n` : 'No field differs from the current settings.\n\n') +
      (ignoredNote ? `${ignoredNote}\n\n` : '') +
      'The monitor and the picture will reconnect to the new instance. ' +
      'Your input and headphone devices are not part of a preset and stay as they are.';
    if (!window.confirm(text)) return;

    applyPresetBtn.disabled = true;
    try {
      // app.js owns the sequence — apply, adopt the RETURNED config, rebuild
      // the monitors — and hands the merged config back for this form.
      const merged = await handlers.onApplyPreset(preset.id);
      if (merged) populate(merged);
      setSaveMessage(`Applied "${preset.name}".`, false);
    } catch (err) {
      setSaveMessage(`Could not apply "${preset.name}": ${err.message}`, true);
    } finally {
      await refreshPresets();
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
      alsoCredentials = window.confirm(
        `Also delete the stored passwords for "${preset.name}" from Windows Credential Manager? ` +
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

  // --- connection -------------------------------------------------------
  const connectionHeading = document.createElement('h2');
  connectionHeading.textContent = 'M2L-X connection';
  openGroup(connectionHeading);

  // THREE BOXES: address, username, password — at the operator's request.
  //
  // The host and the event ID used to be two more editable fields under this
  // one, and they were waffle in field form: both are read out of the pasted
  // live-operation address, an instance only ever has the one event, and the
  // API has NO way to list events — probed again on 2026-08-12 against the
  // live instance (/api/live_operation/events and friends all 404) and
  // confirmed by reading the SPA bundle, whose only real endpoints are
  // sign-in, refresh, the two KVS calls and the two websockets. So the URL is
  // not merely the convenient source of the event id; it is the ONLY source,
  // and a picker for "multiple events" has nothing to ask. The two derived
  // values still exist as config fields and still travel in presets — they
  // are kept as HIDDEN inputs (never appended to the DOM) so populate,
  // collectConfig, validation and the preset diff all work unchanged, and the
  // derived line under the address shows the operator what was read.
  const liveURLInput = textInput('f-liveUrl');
  liveURLInput.placeholder = 'https://m2lx-…/live-operation/…';
  liveURLInput.autocomplete = 'off';
  liveURLInput.spellcheck = false;
  const liveURLRow = row('M2L-X address', 'f-liveUrl', liveURLInput);
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
  const showDerived = () => {
    const host = fields.m2lxHost.input.value;
    const event = fields.eventId.input.value;
    liveURLNote.hidden = !(host || event);
    liveURLNote.textContent = `Host "${host}", event "${event}".`;
  };

  // --- SRT output ---------------------------------------------------------
  const srtHeading = document.createElement('h2');
  srtHeading.textContent = 'SRT output';
  openGroup(srtHeading);

  // Optional and clearly secondary: on every instance seen so far the SRT
  // listener answers on the same name as the REST API, and the operator should
  // not have to type it twice. internal/config.EffectiveSRTHost owns the
  // fallback; this field is the override for an ingest published elsewhere.
  addField(
    'srtHost',
    'SRT host — optional',
    textInput('f-srtHost'),
    'Blank = same host as M2L-X.',
  );
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
   * applyLiveURL parses whatever is in the address field and fills the two
   * fields below it. It reports what it read, so a paste is visibly a paste
   * and not a hope, and it refuses clearly rather than silently doing nothing.
   */
  function applyLiveURL() {
    const raw = liveURLInput.value.trim();
    if (raw === '') {
      hideLiveURLMessages();
      return;
    }

    const parsed = parseLiveOperationURL(raw);
    if (!parsed.ok) {
      liveURLNote.hidden = true;
      liveURLRow.errorEl.textContent = parsed.error;
      liveURLRow.errorEl.hidden = false;
      return;
    }

    liveURLRow.errorEl.hidden = true;
    fields.m2lxHost.input.value = parsed.host;
    fields.eventId.input.value = parsed.eventId;
    liveURLNote.textContent = `Host "${parsed.host}", event "${parsed.eventId}".`;
    liveURLNote.hidden = false;
    refreshSRTPlaceholder();
  }

  // 'input' rather than 'change': a paste should fill the fields the moment it
  // lands, not when focus leaves.
  liveURLInput.addEventListener('input', applyLiveURL);
  liveURLInput.addEventListener('change', applyLiveURL);

  /**
   * refreshSRTPlaceholder shows the host an empty srtHost will actually dial —
   * the same string internal/config.EffectiveSRTHost will resolve. "Optional"
   * without saying what the default is would just move the guesswork.
   */
  function refreshSRTPlaceholder() {
    const derived = bareHost(fields.m2lxHost.input.value);
    fields.srtHost.input.placeholder = derived
      ? `Same as M2L-X: ${derived}`
      : 'Same as the M2L-X host';
  }

  // --- populate / collect --------------------------------------------

  function populate(config) {
    // The diff baseline for Apply, and the values the machine-fields note
    // shows. Recorded BEFORE the form gets a chance to be edited: the confirm
    // dialog compares a preset against what is SAVED, not against keystrokes
    // that were never saved.
    lastLoadedConfig = config;
    renderPresetNote(config);
    fields.m2lxHost.input.value = config.m2lxHost || '';
    fields.alias.input.value = config.alias || '';
    fields.eventId.input.value = config.eventId || '';
    fields.srtHost.input.value = config.srtHost || '';
    fields.srtPort.input.value = String(config.srtPort ?? 0);
    fields.srtLatencyMs.input.value = String(config.srtLatencyMs ?? 120);
    fields.pbkeylen.input.value = String(config.pbkeylen ?? 0);
    fields.statusKey.input.value = config.statusKey || '';
    fields.audioDeviceId.input.value = config.audioDeviceId || '';
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
    liveURLInput.value = formatLiveOperationURL(config.m2lxHost, config.eventId);
    showDerived();
    hideLiveURLMessages();
    refreshSRTPlaceholder();
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
      srtHost: fields.srtHost.input.value.trim(),
      srtPort: Number(fields.srtPort.input.value),
      srtLatencyMs: Number(fields.srtLatencyMs.input.value),
      pbkeylen: Number(fields.pbkeylen.input.value),
      statusKey: fields.statusKey.input.value.trim(),
      audioDeviceId: fields.audioDeviceId.input.value.trim(),
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
    const config = collectConfig();
    const errors = validateConfig(config);
    if (Object.keys(errors).length > 0) {
      displayErrors(errors);
      setSaveMessage('Fix the highlighted fields before saving.', true);
      return;
    }
    clearAllErrors();
    saveBtn.disabled = true;
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
      setSaveMessage('Settings saved.', false);
      handlers.onSaved(config);
    } catch (err) {
      setSaveMessage(`Could not save settings: ${err.message}`, true);
    } finally {
      saveBtn.disabled = false;
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

    // Separately from the config load, and after it, so a failure to reach one
    // does not blank the other.
    try {
      renderSuggestions(await backend.getStatusKeyCandidates());
    } catch (err) {
      console.error('wslcomms: could not fetch statusKey suggestions', err);
    }

    // And the instance picker, separately again and last: a preset listing
    // failure must not stop the operator editing the fields it would have
    // filled in.
    await refreshPresets();
  }

  return { el, open, setSending };
}
