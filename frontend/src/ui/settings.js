import * as backend from './backend.js';
import { validateConfig } from './validate.js';
import { parseLiveOperationURL, formatLiveOperationURL, bareHost } from './liveurl.js';
import { RETURN_BUSES, DEFAULT_RETURN_MID, isValidReturnMid } from './returns.js';
import { normaliseReturnSource, DEVICE_KEY_SRT } from './returnsource.js';
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
    headphoneEndpointId: '',
    returnMid: 2,
    returnChannel: 'stereo',
    returnSource: 'webrtc',
    srtReturnPort: 40503,
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
  // The port of the M2L-X output the SRT return dials. 40503 is Output 3,
  // src=cln — the only output that carries the CLN bus. Carried rather than
  // exposed: it is measured, it is the same on every instance seen so far, and
  // a wrong value here is a monitor that never connects with nothing to say why.
  let carriedSRTReturnPort = 0;
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

  // The primary input: the address bar of the M2L-X GUI the operator is
  // already looking at. Both the host and the event ID are in it, and the
  // event ID is the one field nothing in the API can supply — there is no
  // event-list endpoint (/api/event/list, /api/events, /api/live_operation*
  // and /api/user/me were all checked) and it is an opaque string nobody
  // remembers. One paste fills both fields, which stay visible and editable
  // below so nothing here becomes un-fixable.
  const liveURLInput = textInput('f-liveUrl');
  liveURLInput.placeholder = 'https://m2lx-…/live-operation/…';
  liveURLInput.autocomplete = 'off';
  liveURLInput.spellcheck = false;
  const liveURLRow = row(
    'M2L-X address (paste from the browser)',
    'f-liveUrl',
    liveURLInput,
    'The page you use to operate the event. The host and the event ID are read out of it into the two fields below.',
  );
  const liveURLNote = document.createElement('p');
  liveURLNote.className = 'field-hint field-note';
  liveURLNote.hidden = true;
  liveURLRow.wrap.insertBefore(liveURLNote, liveURLRow.errorEl);
  form.appendChild(liveURLRow.wrap);

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

  addField(
    'eventId',
    'Event ID',
    textInput('f-eventId'),
    'The last part of the live-operation address, e.g. "dl9-5p5ah0bd-empd". Filled in by the paste above.',
  );

  // Typing in either field keeps the pasted address honest rather than leaving
  // a stale URL sitting above two edited fields.
  const syncLiveURL = () => {
    liveURLInput.value = formatLiveOperationURL(fields.m2lxHost.input.value, fields.eventId.input.value);
    hideLiveURLMessages();
  };
  fields.m2lxHost.input.addEventListener('input', () => {
    syncLiveURL();
    refreshSRTPlaceholder();
  });
  fields.eventId.input.addEventListener('input', syncLiveURL);

  // --- SRT output ---------------------------------------------------------
  const srtHeading = document.createElement('h2');
  srtHeading.textContent = 'SRT output';
  form.appendChild(srtHeading);

  // Optional and clearly secondary: on every instance seen so far the SRT
  // listener answers on the same name as the REST API, and the operator should
  // not have to type it twice. internal/config.EffectiveSRTHost owns the
  // fallback; this field is the override for an ingest published elsewhere.
  addField(
    'srtHost',
    'SRT host — optional',
    textInput('f-srtHost'),
    'Leave blank to use the M2L-X host. Only fill this in if SRT ingest is on a different name.',
  );
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
  addField(
    'statusKey',
    'Status key — optional',
    textInput('f-statusKey'),
    'The switcher_status node for our router input, e.g. "cam7". Blank is allowed: the three ' +
    'WebSocket lamps then read NO STATUS and everything else works normally.',
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
  form.appendChild(suggestions);

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
    'Headphone device ID — WebRTC return',
    textInput('f-headphoneDeviceId'),
    'A browser mediaDeviceId, used when the return source is WebRTC. Normally set from the ' +
      'Headphones dropdown on the main screen.',
  );
  // A SECOND device field, not a duplicate. The two return paths address the
  // same headphones through different identifier spaces — a browser
  // mediaDeviceId and a WASAPI endpoint id — and one put in the other's field
  // does not fail: it plays on the default device. Two labelled fields is the
  // cheapest way to make that visible to whoever is pasting a GUID.
  addField(
    DEVICE_KEY_SRT,
    'Headphone device ID — SRT return',
    textInput('f-headphoneEndpointId'),
    'A Windows WASAPI endpoint id, used when the return source is SRT. Not interchangeable with ' +
      'the field above even though both name the same headphones.',
  );

  // --- monitor / return ---------------------------------------------------
  const monitorHeading = document.createElement('h2');
  monitorHeading.textContent = 'Monitor';
  form.appendChild(monitorHeading);
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
    CHANNEL_MODES.map((m) => `${m.label}: ${m.hint}`).join(' '),
  );
  // The return SOURCE — WebRTC or the native SRT path — has NO control here on
  // purpose. It decides what the commentator can hear right now, and a
  // configuration screen that changes it as a side effect of pressing Save is a
  // way to silence somebody mid-match from a screen they are not looking at. It
  // lives on the main screen only; this form carries the saved value through
  // untouched — see collectConfig.

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
  cancelBtn.addEventListener('click', () => leaveSettings());
  actions.append(saveBtn, cancelBtn);
  form.append(saveMessage, actions);

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
    // Held, not shown. See the note beside the returnChannel field.
    carriedReturnSource = normaliseReturnSource(config.returnSource);
    carriedSRTReturnPort = Number(config.srtReturnPort) || 0;
    fields.returnGainDb.input.value = String(config.returnGainDb ?? 18);
    const tile = config.monitorTile || { x: 0, y: 360, w: 640, h: 360 };
    fields['monitorTile.x'].input.value = String(tile.x ?? 0);
    fields['monitorTile.y'].input.value = String(tile.y ?? 360);
    fields['monitorTile.w'].input.value = String(tile.w ?? 640);
    fields['monitorTile.h'].input.value = String(tile.h ?? 360);
    fields.slatePath.input.value = config.slatePath || 'slate.png';
    fields.m2lxPassword.input.value = '';
    fields.srtPassphrase.input.value = '';
    liveURLInput.value = formatLiveOperationURL(config.m2lxHost, config.eventId);
    hideLiveURLMessages();
    refreshSRTPlaceholder();
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
      [DEVICE_KEY_SRT]: fields[DEVICE_KEY_SRT].input.value.trim(),
      returnMid: Number(fields.returnMid.input.value),
      returnChannel: normaliseChannelMode(fields.returnChannel.input.value),
      // Carried through from the loaded config, not collected from a control.
      // See the declarations of these two for why.
      returnSource: carriedReturnSource,
      srtReturnPort: carriedSRTReturnPort,
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
  }

  return { el, open };
}
