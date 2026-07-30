import * as backend from './backend.js';
import { validateConfig } from './validate.js';

// The Settings screen: every field of specification section 9, plus the two
// Credential Manager secrets. Same window, swapped view — there is no
// second window and no menu (spec section 10).
//
// Owner: WP-5b.
//
// audioDeviceId and headphoneDeviceId are included here even though they are
// normally written by the two dropdowns on the main screen, because section
// 9 lists them as part of the configuration and an engineer may need to
// paste a known endpoint GUID before the corresponding hardware is patched
// in. They are ordinary text fields, not re-implementations of the dropdowns.

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
    returnMid: 2,
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
  backBtn.addEventListener('click', () => handlers.onBack());
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
  function addField(key, labelText, input, hint) {
    const { wrap, errorEl } = row(labelText, input.id, input, hint);
    fields[key] = { input, errorEl };
    form.appendChild(wrap);
    return input;
  }

  // --- connection -------------------------------------------------------
  const connectionHeading = document.createElement('h2');
  connectionHeading.textContent = 'M2L-X connection';
  form.appendChild(connectionHeading);

  addField('m2lxHost', 'M2L-X host', textInput('f-m2lxHost'), 'Bare host, e.g. "m2lx.example.com" — no scheme.');
  addField('alias', 'Alias', textInput('f-alias'), 'The sign-in alias. Note: not "username".');

  const m2lxPasswordInput = textInput('f-m2lxPassword', 'password');
  m2lxPasswordInput.autocomplete = 'new-password';
  m2lxPasswordInput.placeholder = 'Leave blank to keep the stored password';
  const m2lxPasswordRow = row('M2L-X password', 'f-m2lxPassword', m2lxPasswordInput);
  const m2lxPasswordBadge = document.createElement('span');
  m2lxPasswordBadge.className = 'secret-badge';
  m2lxPasswordRow.wrap.insertBefore(m2lxPasswordBadge, m2lxPasswordRow.errorEl);
  fields.m2lxPassword = { input: m2lxPasswordInput, errorEl: m2lxPasswordRow.errorEl };
  form.appendChild(m2lxPasswordRow.wrap);

  addField('eventId', 'Event ID', textInput('f-eventId'));

  // --- SRT output ---------------------------------------------------------
  const srtHeading = document.createElement('h2');
  srtHeading.textContent = 'SRT output';
  form.appendChild(srtHeading);

  addField('srtHost', 'SRT host', textInput('f-srtHost'));
  addField('srtPort', 'SRT port', numberInput('f-srtPort'));
  addField('srtLatencyMs', 'SRT latency (ms)', numberInput('f-srtLatencyMs'), 'Default 120 — about 5x the measured median round-trip time.');
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
  form.appendChild(srtPassphraseRow.wrap);

  const secretsHint = document.createElement('p');
  secretsHint.className = 'field-hint secrets-hint';
  secretsHint.textContent =
    'Passwords are write-only: this app never reads them back. "set" only means this field was ' +
    'saved successfully during the current run of the app.';
  form.appendChild(secretsHint);

  // --- status ---------------------------------------------------------
  const statusHeading = document.createElement('h2');
  statusHeading.textContent = 'Status';
  form.appendChild(statusHeading);
  addField('statusKey', 'Status key', textInput('f-statusKey'), 'The switcher_status node for our router input, e.g. "cam7".');

  // --- devices ---------------------------------------------------------
  const devicesHeading = document.createElement('h2');
  devicesHeading.textContent = 'Devices';
  form.appendChild(devicesHeading);
  addField(
    'audioDeviceId',
    'Commentary input device ID',
    textInput('f-audioDeviceId'),
    'Normally set from the Commentary input dropdown on the main screen.',
  );
  addField(
    'headphoneDeviceId',
    'Headphone device ID',
    textInput('f-headphoneDeviceId'),
    'Normally set from the Headphones dropdown on the main screen.',
  );

  // --- monitor / return ---------------------------------------------------
  const monitorHeading = document.createElement('h2');
  monitorHeading.textContent = 'Monitor';
  form.appendChild(monitorHeading);
  addField(
    'returnMid',
    'Return',
    selectInput('f-returnMid', [
      { value: 2, label: 'CLN (effects, no commentary)' },
      { value: 1, label: 'PGM' },
    ]),
  );
  addField(
    'returnGainDb',
    'Return gain (dB)',
    numberInput('f-returnGainDb', 0.1),
    'Default 18 dB — the measured offset between the SRT-ingested level and the KVS monitor level.',
  );

  const tileHeading = document.createElement('h3');
  tileHeading.textContent = 'Monitor tile (position within the 2240x1440 mosaic)';
  form.appendChild(tileHeading);
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
  form.appendChild(tileGrid);

  // --- slate ---------------------------------------------------------
  const slateHeading = document.createElement('h2');
  slateHeading.textContent = 'Slate';
  form.appendChild(slateHeading);
  addField('slatePath', 'Slate image path', textInput('f-slatePath'), 'Defaults to the bundled slate.png.');

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
  cancelBtn.addEventListener('click', () => handlers.onBack());
  actions.append(saveBtn, cancelBtn);
  form.append(saveMessage, actions);

  el.append(header, form);

  // --- populate / collect --------------------------------------------

  function populate(config) {
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
    fields.returnMid.input.value = String(config.returnMid ?? 2);
    fields.returnGainDb.input.value = String(config.returnGainDb ?? 18);
    const tile = config.monitorTile || { x: 0, y: 360, w: 640, h: 360 };
    fields['monitorTile.x'].input.value = String(tile.x ?? 0);
    fields['monitorTile.y'].input.value = String(tile.y ?? 360);
    fields['monitorTile.w'].input.value = String(tile.w ?? 640);
    fields['monitorTile.h'].input.value = String(tile.h ?? 360);
    fields.slatePath.input.value = config.slatePath || 'slate.png';
    fields.m2lxPassword.input.value = '';
    fields.srtPassphrase.input.value = '';
    refreshSecretBadges();
    clearAllErrors();
    saveMessage.hidden = true;
  }

  function refreshSecretBadges() {
    const m2lxSet = backend.isSecretSetThisSession(backend.SECRET_KEY_M2LX);
    const srtSet = backend.isSecretSetThisSession(backend.SECRET_KEY_SRT);
    m2lxPasswordBadge.textContent = m2lxSet ? 'set' : 'not set';
    m2lxPasswordBadge.classList.toggle('secret-badge-set', m2lxSet);
    srtPassphraseBadge.textContent = srtSet ? 'set' : 'not set';
    srtPassphraseBadge.classList.toggle('secret-badge-set', srtSet);
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
      returnMid: Number(fields.returnMid.input.value),
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

  function displayErrors(errors) {
    clearAllErrors();
    let first = null;
    for (const [key, message] of Object.entries(errors)) {
      const field = fields[key];
      if (!field) continue;
      field.errorEl.textContent = message;
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
      if (m2lxPassword.length > 0) {
        await backend.setSecret(backend.SECRET_KEY_M2LX, m2lxPassword);
        fields.m2lxPassword.input.value = '';
      }
      if (srtPassphrase.length > 0) {
        await backend.setSecret(backend.SECRET_KEY_SRT, srtPassphrase);
        fields.srtPassphrase.input.value = '';
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
    try {
      const config = await backend.getConfig();
      populate(config);
    } catch (err) {
      populate(blankConfig());
      setSaveMessage(`Could not load the current configuration: ${err.message}`, true);
    } finally {
      saveBtn.disabled = false;
    }
  }

  return { el, open };
}
