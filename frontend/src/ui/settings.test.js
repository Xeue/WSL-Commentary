/**
 * Tests for the SRT RETURN's encryption settings: the key-length control on the
 * Settings screen, the write-only passphrase beside it, and the four places the
 * two of them have to agree with Go.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * The SRT return did not work, and the cause was configuration rather than
 * code. M2L-X sets SRT encryption PER OUTPUT, measured on the live instance:
 *
 *     Output 1  src=pgm  port 40501  encrypted=false
 *     Output 2  src=pvw  port 40502  encrypted=true
 *     Output 3  src=cln  port 40503  encrypted=true
 *
 * The return dialled 40503 — encrypted — with no passphrase, because there was
 * nowhere for an operator to enter one and the return borrowed the SEND path's
 * credentials. Every handshake was refused, the reconnect ladder retried for
 * ever, and nothing said why. That cost an afternoon.
 *
 * So there are now two independent SRT credentials and two independent key
 * lengths, and the failures these tests exist to catch are the SILENT ones:
 *
 *   - a JSON key that the form spells one way and Go spells another, which does
 *     not error — Go simply never sees the value, keeps 0, and negotiates no
 *     encryption while the screen shows 32;
 *   - a Credential Manager key string that drifts, which writes a passphrase
 *     nothing ever reads while the badge says "set";
 *   - the return field wired to the SEND path's secret, which is the bug the
 *     whole change removes: fixing the monitor would break the feed.
 *
 * ======================= WHY SOME OF THIS READS SOURCE ======================
 *
 * settings.js builds its form against the real DOM. Driving it under
 * `node --test` would need a shim covering <form>, <select>, <input type=
 * password>, labels and submit events, and package.json is frozen so there is
 * no jsdom. A shim widened until a test passes stops being evidence. The
 * properties that matter here are structural — WHICH secret key the return
 * passphrase is written under, and that nothing anywhere reads a secret back —
 * so the source is what is asserted, exactly as mixerwiring.test.js and
 * returnsource.test.js already do. validateConfig is pure and is driven for
 * real.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import { validateConfig, isConfigValid } from './validate.js';
import { SECRET_KEY_M2LX, SECRET_KEY_SRT, SECRET_KEY_SRT_RETURN, setSecret, isSecretSetThisSession } from './backend.js';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..', '..');
const read = (...parts) => readFileSync(join(...parts), 'utf8');
const ui = (name) => read(here, name);

/** A configuration validateConfig accepts, so a case can change one field. */
function validForm() {
  return {
    m2lxHost: 'm2lx.example.com',
    alias: 'wsl-comms-ro',
    eventId: 'dl9-5p5ah0bd-empd',
    srtPort: 40001,
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
    srtReturnPBKeyLen: 0,
    pictureLatencyMs: 120,
    monitorTile: { x: 0, y: 360, w: 640, h: 360 },
    returnGainDb: 18,
    slatePath: 'slate.png',
  };
}

// ---------------------------------------------------------------------------
// validateConfig: the return key length
// ---------------------------------------------------------------------------

test('the baseline form is valid, so a failure below is about the field it changed', () => {
  assert.deepEqual(validateConfig(validForm()), {});
  assert.equal(isConfigValid(validForm()), true);
});

test('the return key length accepts exactly 0, 16 and 32', () => {
  for (const value of [0, 16, 32]) {
    const errors = validateConfig({ ...validForm(), srtReturnPBKeyLen: value });
    assert.equal(
      errors.srtReturnPBKeyLen,
      undefined,
      `srtReturnPBKeyLen ${value} is one of the three SRT AES-CTR key lengths`,
    );
  }
});

test('the return key length rejects everything else, including the plausible mistakes', () => {
  const cases = [
    24, // a plausible typo, and libsrt does not accept it
    128, // bits, not bytes: what the setting is called everywhere else
    256,
    -16,
    1.5,
    '16', // a <select> value that was never passed through Number()
    null,
    undefined,
    NaN,
  ];
  for (const value of cases) {
    const errors = validateConfig({ ...validForm(), srtReturnPBKeyLen: value });
    assert.ok(
      errors.srtReturnPBKeyLen,
      `srtReturnPBKeyLen ${String(value)} must be rejected`,
    );
  }
});

// ---------------------------------------------------------------------------
// validateConfig: the return PORT
// ---------------------------------------------------------------------------

test('the return port accepts the four measured M2L-X outputs', () => {
  // 40501 pgm, 40502 pvw, 40503 cln, 40504+ the byte-transparent relays.
  for (const value of [40501, 40502, 40503, 40504, 40507, 1, 65535]) {
    const errors = validateConfig({ ...validForm(), srtReturnPort: value });
    assert.equal(errors.srtReturnPort, undefined, `port ${value} is in range`);
  }
});

test('the return port is validated like a port, 1 to 65535', () => {
  for (const value of [0, -1, 65536, 1.5, '40501', null, undefined, NaN]) {
    const errors = validateConfig({ ...validForm(), srtReturnPort: value });
    assert.ok(errors.srtReturnPort, `srtReturnPort ${String(value)} must be rejected`);
  }
});

test('the two SRT ports are validated independently', () => {
  // srtPort is the M2L-X INPUT the feed is sent to; srtReturnPort is the
  // OUTPUT the monitor listens to. Blaming one for the other sends the
  // operator to the wrong control.
  const sendBad = validateConfig({ ...validForm(), srtPort: 0, srtReturnPort: 40501 });
  assert.ok(sendBad.srtPort, 'srtPort 0 must be reported');
  assert.equal(sendBad.srtReturnPort, undefined, 'the return port is fine and must not light up');

  const returnBad = validateConfig({ ...validForm(), srtPort: 40001, srtReturnPort: 0 });
  assert.ok(returnBad.srtReturnPort, 'srtReturnPort 0 must be reported');
  assert.equal(returnBad.srtPort, undefined, 'the send port is fine and must not light up');
});

test('the return port error names a port the operator can actually type', () => {
  const { srtReturnPort } = validateConfig({ ...validForm(), srtReturnPort: 0 });
  assert.match(srtReturnPort, /return/i);
  assert.match(srtReturnPort, /40501/);
});

test('the two key lengths are validated independently', () => {
  // The measured arrangement: an unencrypted commentary input and an encrypted
  // programme output. One shared control could not express it, and a validator
  // that copied one field's answer onto the other would reintroduce that.
  const errors = validateConfig({ ...validForm(), pbkeylen: 0, srtReturnPBKeyLen: 32 });
  assert.deepEqual(errors, {});

  // A bad send-path key length must not be blamed on the return field, or the
  // operator fixes the wrong control.
  const sendBad = validateConfig({ ...validForm(), pbkeylen: 24, srtReturnPBKeyLen: 32 });
  assert.ok(sendBad.pbkeylen, 'pbkeylen 24 must be reported');
  assert.equal(sendBad.srtReturnPBKeyLen, undefined, 'the return field is fine and must not light up');

  const returnBad = validateConfig({ ...validForm(), pbkeylen: 32, srtReturnPBKeyLen: 24 });
  assert.ok(returnBad.srtReturnPBKeyLen, 'srtReturnPBKeyLen 24 must be reported');
  assert.equal(returnBad.pbkeylen, undefined, 'the send field is fine and must not light up');
});

test('the return key length error names the control the operator is looking at', () => {
  const { srtReturnPBKeyLen } = validateConfig({ ...validForm(), srtReturnPBKeyLen: 24 });
  assert.match(srtReturnPBKeyLen, /return/i);
  assert.match(srtReturnPBKeyLen, /0|16|32/);
});

// ---------------------------------------------------------------------------
// validateConfig: the two pasted device ids
// ---------------------------------------------------------------------------
//
// THE FAILURE THESE PREVENT. wasapi2 republishes every Windows RENDER
// (playback) endpoint as a capture "loopback" device; an operator selected one
// as the commentary input, the pipeline prerolled, the device failed
// ASYNCHRONOUSLY, and the sender blamed the SRT network and retried forever.
// The dropdown filter fixes the selection route; these fields are the PASTE
// route — "an engineer may need to paste a known endpoint GUID before the
// corresponding hardware is patched in" — and a pasted GUID gives no other
// clue which of Windows' two namespaces it belongs to.

test('audioDeviceId refuses a Windows RENDER (playback) endpoint id', () => {
  // The operator's actual failing id, verbatim from wasapi2src's 1551 error.
  const measured = '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}';
  const errors = validateConfig({ ...validForm(), audioDeviceId: measured });
  assert.ok(errors.audioDeviceId, 'a playback endpoint cannot be a commentary input');
  assert.match(errors.audioDeviceId, /PLAYBACK/i, 'the message must say what the id IS');
  assert.ok(
    errors.audioDeviceId.includes('{0.0.1.00000000}.'),
    'and name the capture namespace, because the GUID itself gives the operator no clue',
  );

  // Case-insensitively: GUID casing is not stable across the APIs that print
  // these ids, and a case-sensitive refusal is a refusal that sometimes works.
  const upper = measured.toUpperCase();
  assert.ok(validateConfig({ ...validForm(), audioDeviceId: upper }).audioDeviceId);
});

test('audioDeviceId accepts capture ids, unknown shapes and empty', () => {
  // The asymmetric rule from internal/gst/device_id.go: refuse only a
  // POSITIVELY identified render id. Refusing unknown shapes would turn a
  // future Windows id-shape change into a Settings screen that cannot be
  // saved over a field the operator never touched.
  for (const value of [
    '', // optional, as ever
    '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}', // a real capture id
    'default', // an unknown shape
    '{0.0.2.00000000}.{11111111-2222-3333-4444-555555555555}', // a namespace that does not exist yet
  ]) {
    const errors = validateConfig({ ...validForm(), audioDeviceId: value });
    assert.equal(errors.audioDeviceId, undefined, `${JSON.stringify(value)} must be accepted`);
  }
});

test('headphoneEndpointId refuses a CAPTURE id — the same mistake, mirrored', () => {
  // wasapi2sink plays through a RENDER endpoint; a capture id pasted here is
  // the input list's id in the output field. Same silent symptom family, so
  // the same defence, pointed the other way.
  const capture = '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}';
  const errors = validateConfig({ ...validForm(), headphoneEndpointId: capture });
  assert.ok(errors.headphoneEndpointId, 'a recording endpoint cannot be the headphones');
  assert.match(errors.headphoneEndpointId, /CAPTURE|recording/i);
  assert.ok(errors.headphoneEndpointId.includes('{0.0.0.00000000}.'), 'and the render namespace is named');

  for (const value of ['', '{0.0.0.00000000}.{7a2c1f90-4b3e-4c1a-9d55-0d1b3f8e2a11}', 'default']) {
    const ok = validateConfig({ ...validForm(), headphoneEndpointId: value });
    assert.equal(ok.headphoneEndpointId, undefined, `${JSON.stringify(value)} must be accepted`);
  }
});

test('the two device-id checks light their own field, not each other', () => {
  // Blaming one field for the other sends the operator to the wrong control —
  // the same rule the two ports and the two key lengths already follow.
  const render = '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}';
  const capture = '{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}';

  const inputBad = validateConfig({ ...validForm(), audioDeviceId: render, headphoneEndpointId: render });
  assert.ok(inputBad.audioDeviceId, 'the render id in the input field must be reported');
  assert.equal(inputBad.headphoneEndpointId, undefined, 'a render id is CORRECT for headphones');

  const outputBad = validateConfig({ ...validForm(), audioDeviceId: capture, headphoneEndpointId: capture });
  assert.ok(outputBad.headphoneEndpointId, 'the capture id in the headphone field must be reported');
  assert.equal(outputBad.audioDeviceId, undefined, 'a capture id is CORRECT for the commentary input');
});

test('headphoneDeviceId — the browser mediaDeviceId — is deliberately not namespace-checked', () => {
  // It is a different identifier space with no namespace rule to test: a
  // browser mediaDeviceId is an opaque hash. A WASAPI-shaped string in it is
  // wrong, but there is no POSITIVE identification to refuse it on, and the
  // asymmetric rule refuses only what is positively identified.
  const render = '{0.0.0.00000000}.{8678ce58-90c0-4827-8ff7-c9edd8d074ed}';
  const errors = validateConfig({ ...validForm(), headphoneDeviceId: render });
  assert.equal(errors.headphoneDeviceId, undefined);
});

// ---------------------------------------------------------------------------
// The secret key, and the fact that it is a THIRD one
// ---------------------------------------------------------------------------

test('the three secret keys are distinct', () => {
  const keys = [SECRET_KEY_M2LX, SECRET_KEY_SRT, SECRET_KEY_SRT_RETURN];
  assert.equal(new Set(keys).size, 3, 'two secrets sharing a key overwrite each other');
});

test('the secret keys are spelled the way internal/secrets spells them', () => {
  // Wails passes this string straight to secrets.targetFor, which rejects
  // anything it does not know. A drift here is a Settings screen whose Save
  // fails on a key nobody typed, or — worse, if Go ever grew a fourth key — a
  // passphrase written where nothing reads it while the badge says "set".
  const go = read(repoRoot, 'internal', 'secrets', 'secrets.go');
  for (const [name, value] of [
    ['KeyM2LX', SECRET_KEY_M2LX],
    ['KeySRT', SECRET_KEY_SRT],
    ['KeySRTReturn', SECRET_KEY_SRT_RETURN],
  ]) {
    assert.ok(
      new RegExp(`${name}\\s*=\\s*"${value}"`).test(go),
      `internal/secrets no longer spells ${name} "${value}"`,
    );
  }
});

test('setSecret rejects a key that is not one of the three', async () => {
  await assert.rejects(() => setSecret('srt-return', 'x'), /unknown key/);
  await assert.rejects(() => setSecret('srtReturn', 'x'), /unknown key/);
  await assert.rejects(() => setSecret('', 'x'), /unknown key/);
  // Prototype keys are not secret keys. hasOwnProperty, not `in`.
  await assert.rejects(() => setSecret('toString', 'x'), /unknown key/);
});

test('writing the return passphrase does not mark the send passphrase as set', async () => {
  // The badges are the only "is it set" signal there is, and a badge that lit
  // for a credential nobody wrote is a lie about a passphrase. This also
  // catches the two keys being wired to one slot.
  assert.equal(isSecretSetThisSession(SECRET_KEY_SRT_RETURN), false);
  assert.equal(isSecretSetThisSession(SECRET_KEY_SRT), false);

  await setSecret(SECRET_KEY_SRT_RETURN, 'the-programme-output-key');

  assert.equal(isSecretSetThisSession(SECRET_KEY_SRT_RETURN), true);
  assert.equal(
    isSecretSetThisSession(SECRET_KEY_SRT),
    false,
    'writing the RETURN passphrase marked the SEND passphrase as set; they are one slot',
  );
  assert.equal(isSecretSetThisSession(SECRET_KEY_M2LX), false);
});

test('no secret is readable back through the backend module', () => {
  // internal/secrets.Store has no getter by design and App exposes none, so
  // there must be nothing on this side either. isSecretSetThisSession returns a
  // boolean and is the only trace a written secret leaves in this process.
  const js = ui('backend.js');
  assert.equal(/export\s+(async\s+)?function\s+getSecret\b/.test(js), false);
  assert.equal(/callGo\(\s*['"]GetSecret['"]/.test(js), false);
  assert.equal(typeof isSecretSetThisSession(SECRET_KEY_SRT_RETURN), 'boolean');

  // And Go must not have grown one, which is the half this side cannot see.
  const go = read(repoRoot, 'app.go');
  assert.equal(
    /func \(a \*App\) GetSecret\b/.test(go),
    false,
    'app.go has grown a secret getter; a secret must never cross the Wails boundary outbound',
  );
});

// ---------------------------------------------------------------------------
// The Settings form
// ---------------------------------------------------------------------------

test('Settings writes the return passphrase under the RETURN key', () => {
  const js = ui('settings.js');
  assert.match(
    js,
    /backend\.setSecret\(\s*backend\.SECRET_KEY_SRT_RETURN\s*,\s*srtReturnPassphrase\s*\)/,
    'the return passphrase field must be saved under SECRET_KEY_SRT_RETURN',
  );
  // And the send field must still go to its own key, unchanged. If the return
  // field were wired to SECRET_KEY_SRT the monitor would work and the feed
  // would stop, which is the failure the second credential exists to remove.
  assert.match(js, /backend\.setSecret\(\s*backend\.SECRET_KEY_SRT\s*,\s*srtPassphrase\s*\)/);
});

test('Settings never sends an empty passphrase, so Save does not delete a stored one', () => {
  // internal/secrets deletes the credential when Set is given "". A form that
  // sent the blank field on every Save would clear the passphrase every time
  // the operator changed the slate path.
  const js = ui('settings.js');
  const guard = /if \(srtReturnPassphrase\.length > 0\) \{\s*await backend\.setSecret\(/;
  assert.match(js, guard, 'the return passphrase must only be written when something was typed');
});

test('Settings collects and populates the return key length', () => {
  const js = ui('settings.js');
  assert.match(
    js,
    /srtReturnPBKeyLen: Number\(fields\.srtReturnPBKeyLen\.input\.value\)/,
    'collectConfig must send srtReturnPBKeyLen as a number, not a <select> string',
  );
  assert.match(
    js,
    /fields\.srtReturnPBKeyLen\.input\.value = String\(config\.srtReturnPBKeyLen \?\? 0\)/,
    'populate must show what is saved, or the form reports 0 for an encrypted return',
  );
  // The control offers exactly the three key lengths, in one place.
  assert.match(js, /selectInput\('f-srtReturnPBKeyLen',/);
});

test('the return port has a real control, populated and collected', () => {
  // THE DEFECT THIS PINS. srtReturnPort was carried through the form: read in
  // populate, written back in collectConfig, never rendered. An operator whose
  // config.json held 40503 — src=cln, measured encrypted=true — had no way to
  // correct it from the application, so their return could not connect and
  // nothing on screen would say why.
  const js = ui('settings.js');
  assert.match(js, /numberInput\('f-srtReturnPort'\)/, 'the return port must have a numeric input');
  assert.match(
    js,
    /addField\(\s*'srtReturnPort',/,
    'the return port must be a real field, not a carried value',
  );
  assert.match(
    js,
    /srtReturnPort: Number\(fields\.srtReturnPort\.input\.value\)/,
    'collectConfig must send the port as a number, not a string',
  );
  assert.match(
    js,
    /fields\.srtReturnPort\.input\.value = String\(config\.srtReturnPort/,
    'populate must show what is saved, or the operator corrects a value they cannot see',
  );
  assert.equal(
    /carriedSRTReturnPort/.test(js),
    false,
    'the return port is carried again; that is precisely how it became uneditable',
  );
});

test('the return port field names the outputs, because nothing else does', () => {
  // There is no endpoint that lists the M2L-X outputs. The port menu was
  // measured by dialling each one, and a bare "SRT return port" box is a
  // five-digit number with no way to find out what to type.
  const js = ui('settings.js');
  const field = js.slice(js.indexOf("addField(\n    'srtReturnPort',"), js.indexOf("SRT return encryption'"));
  assert.ok(field.length > 0, 'the return port field must be in the SRT return group');
  for (const port of ['40501', '40502', '40503', '40504']) {
    assert.ok(field.includes(port), `the hint must name port ${port}`);
  }
  assert.match(field, /pgm/, 'and say which source each output carries');
  assert.match(field, /cln/);
});

test('the return port sits with the return settings, above the encryption controls', () => {
  // Between the Monitor group and "SRT return encryption": the port chooses the
  // output, and M2L-X sets encryption PER OUTPUT, so changing one is a reason
  // to look at the other. Under "SRT output" it would read as the send port.
  const js = ui('settings.js');
  const monitor = js.indexOf("monitorHeading.textContent = 'Monitor'");
  const port = js.indexOf("addField(\n    'srtReturnPort',");
  const encryption = js.indexOf("returnEncryptionHeading.textContent = 'SRT return encryption'");
  assert.ok(monitor > 0 && port > 0 && encryption > 0);
  assert.ok(port > monitor, 'the return port belongs in the Monitor group, not the SRT output group');
  assert.ok(port < encryption, 'and above the encryption controls it decides the answer for');
});

test('srtReturnPort is spelled the same way in the form and in config.go', () => {
  // Same silent failure as srtReturnPBKeyLen: a key mismatch is not an error,
  // Go keeps 0, EffectiveSRTReturnPort substitutes 40501, and the screen shows
  // whatever the operator typed while the monitor dials something else.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(
    go,
    /SRTReturnPort int `json:"srtReturnPort"`/,
    'internal/config no longer tags the return port "srtReturnPort"',
  );
  for (const file of ['settings.js', 'validate.js', 'backend.js', 'returnpath.js']) {
    assert.ok(ui(file).includes('srtReturnPort'), `${file} must use the same key as config.go`);
  }
});

test('the return port range matches internal/config.ValidateReturn', () => {
  // Go refuses the same range. Two validators that disagree mean either a Save
  // the form accepts and StartReturn then refuses, or the reverse — and the
  // reverse is the one that reaches an operator as "it just does not connect".
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(
    go,
    /srtReturnPort must be between 1 and 65535/,
    'internal/config no longer bounds the return port at 1..65535',
  );
});

test('the return passphrase field is a password field and is cleared on open', () => {
  const js = ui('settings.js');
  assert.match(js, /textInput\('f-srtReturnPassphrase', 'password'\)/);
  assert.match(js, /srtReturnPassphraseInput\.autocomplete = 'new-password'/);
  assert.match(
    js,
    /fields\.srtReturnPassphrase\.input\.value = '';/,
    'populate must clear the field, so a re-opened Settings screen is not holding a passphrase',
  );
});

test('the return encryption controls are grouped with the return settings', () => {
  // "Passphrase key length" and "Return key length" are two different endpoints
  // and are one dropdown apart on screen. Putting the return pair under the SRT
  // OUTPUT heading would make the wrong one the obvious one to change.
  const js = ui('settings.js');
  const heading = js.indexOf("returnEncryptionHeading.textContent = 'SRT return encryption'");
  const monitor = js.indexOf("monitorHeading.textContent = 'Monitor'");
  const slate = js.indexOf("slateHeading.textContent = 'Slate'");
  assert.ok(heading > 0, 'the SRT return encryption group must exist');
  assert.ok(monitor > 0 && slate > 0);
  assert.ok(
    heading > monitor && heading < slate,
    'the return encryption controls belong in the Monitor group, not the SRT output group',
  );
});

test('the Settings form does not carry the old shared-credential wiring', () => {
  // The return used to read the send path's pbkeylen. Nothing on this screen
  // may write srtReturnPBKeyLen from the pbkeylen control or the reverse.
  const js = ui('settings.js');
  assert.equal(/srtReturnPBKeyLen: Number\(fields\.pbkeylen/.test(js), false);
  assert.equal(/pbkeylen: Number\(fields\.srtReturnPBKeyLen/.test(js), false);
});

// ---------------------------------------------------------------------------
// The cross-language contract
// ---------------------------------------------------------------------------

test('srtReturnPBKeyLen is spelled the same way in the form and in config.go', () => {
  // A JSON key mismatch does not fail: encoding/json ignores what it does not
  // recognise, Go keeps 0, and the return negotiates no encryption while the
  // Settings screen shows 32. Silent, and identical to the fault this whole
  // change exists to fix.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(
    go,
    /SRTReturnPBKeyLen int `json:"srtReturnPBKeyLen"`/,
    'internal/config no longer tags the return key length "srtReturnPBKeyLen"',
  );
  for (const file of ['settings.js', 'validate.js', 'backend.js', 'returnpath.js']) {
    assert.ok(
      ui(file).includes('srtReturnPBKeyLen'),
      `${file} must use the same key as config.go`,
    );
  }
});

test('the default return port on this side is 40501, the DIRTY programme output', () => {
  // The fallbacks the frontend uses when Go cannot be reached. 40503 is
  // src=cln and measured encrypted=true; defaulting to it is the configuration
  // that failed every handshake. The Go default is pinned in
  // internal/config/config_return_test.go; this is the half that lives here.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /DefaultSRTReturnPort = 40501/, 'the Go default moved off 40501');

  assert.match(ui('backend.js'), /srtReturnPort: 40501/);
  assert.match(ui('settings.js'), /srtReturnPort: 40501/);
  for (const file of ['backend.js', 'settings.js']) {
    assert.equal(
      /srtReturnPort: 40503/.test(ui(file)),
      false,
      `${file} still falls back to 40503, which is the encrypted CLN output`,
    );
  }
});

test('a change to the return key length rebuilds a running monitor', () => {
  // returnOptsFingerprint decides that, and returnpath.test.js asserts the list
  // matches app_return.go. This is the half that says WHY the field belongs in
  // it: switching the return between an encrypted and an unencrypted M2L-X
  // output while it is running must not leave a pipeline built for the old one.
  assert.match(ui('returnpath.js'), /RETURN_OPTS_CONFIG_KEYS = Object\.freeze\(\[[^\]]*'srtReturnPBKeyLen'/s);
});

test('the return passphrase never reaches config.json', () => {
  // It is a Credential Manager value. config.json is plain text in %APPDATA%,
  // hand-editable by design, and the first thing pasted into a support ticket.
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  assert.ok(collect.length > 0, 'settings.js must still have collectConfig');
  assert.equal(
    /srtReturnPassphrase/.test(collect),
    false,
    'collectConfig puts the return passphrase into the object saveConfig writes to config.json',
  );

  // And the receiving end has no field for one either. Matched on the JSON TAG
  // NAMES rather than on the struct's text, because the field comments discuss
  // passphrases at length and should go on doing so.
  const struct = read(repoRoot, 'internal', 'config', 'config.go').match(/type Config struct \{[\s\S]*?\n\}/);
  assert.ok(struct, 'internal/config must still declare type Config struct');
  const tags = [...struct[0].matchAll(/json:"([^"]+)"/g)].map((m) => m[1]);
  assert.ok(tags.includes('srtReturnPBKeyLen'), 'the key length is not a secret and belongs here');
  for (const tag of tags) {
    assert.equal(
      /pass(phrase|word)|secret/i.test(tag),
      false,
      `config.Config has grown a persisted secret: "${tag}". Secrets live in Credential ` +
        'Manager; config.json is plain text in %APPDATA%.',
    );
  }
});

// ---------------------------------------------------------------------------
// The picture monitor's SRT buffer
// ---------------------------------------------------------------------------
//
// THE FAILURE THESE PREVENT. The commentator's picture ran about a second
// behind the match. Two causes, both the same shape as the return-port defect
// above: a number with no control on any screen, and a number that meant
// something else.
//
//   - pictureLatencyMs did not exist. app_picture.go handed the monitor
//     cfg.srtLatencyMs — the CONTRIBUTION FEED's retransmission budget — so the
//     only way to make the picture quicker was to thin the protection on the
//     match going to air.
//   - and it had no field, so nobody could have done either.
//
// Measured on the live instance on 2026-08-07: 993.7 ms of the delay was the
// video sink synchronising to the pipeline clock, which is fixed in
// internal/gst. This field is the rest of the control, and its hint carries the
// one fact that makes it usable — that the far end sets a floor.

test('the picture latency is bounded, and 0 means the default', () => {
  for (const value of [0, 1, 40, 120, 300, 8000]) {
    const errors = validateConfig({ ...validForm(), pictureLatencyMs: value });
    assert.equal(errors.pictureLatencyMs, undefined, `picture latency ${value} is in range`);
  }

  // 0 is ACCEPTED, unlike srtReturnPort, and the difference has teeth. Go's
  // EffectivePictureLatencyMs substitutes the default for 0, and every
  // config.json written before this field existed holds 0 — so refusing it
  // would make Settings unsavable on the first launch after an upgrade, over a
  // field the operator never touched.
  assert.equal(validateConfig({ ...validForm(), pictureLatencyMs: 0 }).pictureLatencyMs, undefined);

  for (const value of [-1, -120, 8001, 1.5, '120', null, undefined, NaN]) {
    const errors = validateConfig({ ...validForm(), pictureLatencyMs: value });
    assert.ok(errors.pictureLatencyMs, `pictureLatencyMs ${String(value)} must be rejected`);
  }
});

test('the picture latency and the send latency are separate fields', () => {
  // The whole point of the change. A monitor set as quick as it goes must not
  // drag the contribution feed's protection down with it, and a heavily
  // protected feed must not hold the commentator's picture a second behind.
  const quickMonitor = validateConfig({
    ...validForm(),
    srtLatencyMs: 2000,
    pictureLatencyMs: 40,
  });
  assert.deepEqual(quickMonitor, {}, 'a protected feed with a quick monitor is the point');

  // And an error on one must not light the other's box.
  const sendBad = validateConfig({ ...validForm(), srtLatencyMs: 0, pictureLatencyMs: 120 });
  assert.ok(sendBad.srtLatencyMs, 'the send latency is out of range and must be reported');
  assert.equal(sendBad.pictureLatencyMs, undefined, 'the picture latency is fine');

  const pictureBad = validateConfig({ ...validForm(), srtLatencyMs: 120, pictureLatencyMs: -1 });
  assert.ok(pictureBad.pictureLatencyMs, 'the picture latency is out of range');
  assert.equal(pictureBad.srtLatencyMs, undefined, 'the send latency is fine');
});

test('the picture latency has a real control, populated and collected', () => {
  // Exactly the guard the return port carries, for exactly its defect: a field
  // read in populate and written back in collectConfig but never rendered is a
  // field nobody can fix. See the note at carriedReturnSource in settings.js.
  const js = ui('settings.js');
  assert.match(js, /numberInput\('f-pictureLatencyMs'\)/, 'the picture latency needs a numeric input');
  assert.match(
    js,
    /addField\(\s*'pictureLatencyMs',/,
    'the picture latency must be a real field, not a carried value',
  );
  assert.match(
    js,
    /pictureLatencyMs: Number\(fields\.pictureLatencyMs\.input\.value\)/,
    'collectConfig must send the latency as a number, not a string',
  );
  assert.match(
    js,
    /fields\.pictureLatencyMs\.input\.value = String\(\s*config\.pictureLatencyMs/,
    'populate must show what is saved, or the operator corrects a value they cannot see',
  );
  assert.equal(
    /carriedPictureLatency/.test(js),
    false,
    'the picture latency is carried; that is precisely how srtReturnPort became uneditable',
  );
});

test('the picture latency hint states the floor the far end sets', () => {
  // THE MOST IMPORTANT STRING ON THIS SCREEN. SRT negotiates the LARGER of the
  // two peers' latencies, and the operator's M2L-X Output 1 is set to
  // Buffer (msec) = 300. An operator who drops this to 40, sees nothing change
  // and is told nothing concludes the control is broken — when in fact it works
  // and the far end is overriding it. Without this sentence the field is worse
  // than no field.
  const js = ui('settings.js');
  const field = js.slice(
    js.indexOf("addField(\n    'pictureLatencyMs',"),
    js.indexOf('returnEncryptionHeading.textContent'),
  );
  assert.ok(field.length > 0, 'the picture latency field must sit above the encryption controls');
  assert.match(field, /300/, 'the hint must name the 300 ms floor M2L-X imposes');
  assert.match(field, /M2L-X/, 'and say where the floor comes from');
  assert.match(field, /larger/i, 'and why: SRT takes the larger of the two ends');
});

test('the picture latency sits beside the return port it shares a session with', () => {
  // The port picks which M2L-X output the picture comes from; this picks how
  // much delay it is buffered with. Read apart, the hint's talk of "this M2L-X
  // output" has no referent.
  const js = ui('settings.js');
  const port = js.indexOf("addField(\n    'srtReturnPort',");
  const latency = js.indexOf("addField(\n    'pictureLatencyMs',");
  const encryption = js.indexOf("returnEncryptionHeading.textContent = 'SRT return encryption'");
  assert.ok(port > 0 && latency > 0 && encryption > 0);
  assert.ok(latency > port, 'the picture latency belongs immediately below the return port');
  assert.ok(latency < encryption, 'and inside the return group, not after it');
});

test('pictureLatencyMs is spelled the same way in the form and in config.go', () => {
  // Same silent failure as srtReturnPBKeyLen and srtReturnPort: a key mismatch
  // is not an error, Go keeps 0, EffectivePictureLatencyMs substitutes 120, and
  // the screen shows whatever the operator typed while the monitor dials
  // something else. Their conclusion is that the control does nothing — the
  // very conclusion the M2L-X floor already risks, arriving for a second reason.
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(
    go,
    /PictureLatencyMs int `json:"pictureLatencyMs"`/,
    'internal/config no longer tags the picture latency "pictureLatencyMs"',
  );
  for (const file of ['settings.js', 'validate.js', 'backend.js']) {
    assert.ok(ui(file).includes('pictureLatencyMs'), `${file} must use the same key as config.go`);
  }
});

test('the picture latency range matches internal/config.ValidateReturn', () => {
  // Two validators that disagree mean either a Save the form accepts and
  // StartPicture then refuses, or the reverse — and the reverse reaches an
  // operator as "the picture just does not come up".
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(
    go,
    /pictureLatencyMs must be between 0 and 8000 milliseconds/,
    'internal/config no longer bounds the picture latency at 0..8000',
  );
  // And it is checked by ValidateReturn, not Validate: a monitor setting must
  // never be a reason the contribution feed does not go on air.
  const validateReturn = go.slice(go.indexOf('func (c *Config) ValidateReturn()'));
  assert.ok(
    validateReturn.indexOf('pictureLatencyMs') > -1,
    'the picture latency must be validated by ValidateReturn, not by the gate on Start',
  );
});

test('the default picture latency is 120 everywhere it is written down', () => {
  assert.match(ui('backend.js'), /pictureLatencyMs: 120/);
  assert.match(ui('settings.js'), /pictureLatencyMs: 120/);
  const go = read(repoRoot, 'internal', 'config', 'config.go');
  assert.match(go, /DefaultPictureLatencyMs = 120/);
});

// ---------------------------------------------------------------------------
// The M2L-X instance group (presets)
// ---------------------------------------------------------------------------
//
// The pure model has its own file (presets.test.js). What is asserted HERE is
// the Settings form's half of the contract: where the group sits, that the
// sending gate reaches its buttons, and — the one that guards the whole
// feature's data path — that
// collectConfig() did not grow a preset key, because saveConfig REPLACES the
// stored document and a key with no Go field is discarded silently
// (pictureSource is the standing example of that trap).

test('the instance group is the FIRST group, above M2L-X connection', () => {
  const js = ui('settings.js');
  const presets = js.indexOf("presetsHeading.textContent = 'M2L-X instance'");
  const connection = js.indexOf("connectionHeading.textContent = 'M2L-X connection'");
  assert.ok(presets > 0, 'the Settings form must have the M2L-X instance group');
  assert.ok(connection > 0);
  assert.ok(
    presets < connection,
    'the instance picker ACTS on the form — applying rewrites most fields below — so it comes first',
  );
  // And it opens through the same group machinery as every other heading,
  // with the card modifier main.css's presets contract hangs off.
  assert.match(js, /openGroup\(presetsHeading, 'settings-group--presets'\)/);
});

test('Apply and Delete are gated on the sending state, with the reason on the control', () => {
  // The gate itself is Go's — ApplyPreset refuses while a session runs. This
  // is the honest rendering of it: a button that would only ever fail must
  // say why it is disabled, not throw when pressed.
  const js = ui('settings.js');
  const setSending = js.slice(js.indexOf('function setSending(sending)'), js.indexOf('\n  }', js.indexOf('function setSending(sending)')));
  assert.ok(setSending.length > 0, 'settings.js must expose setSending');
  assert.match(setSending, /sendingNow = sending === true/);
  assert.match(setSending, /renderPresetButtons\(\)/);

  const buttons = js.slice(js.indexOf('function renderPresetButtons()'), js.indexOf('async function refreshPresets'));
  assert.match(buttons, /applyPresetBtn\.disabled = none \|\| sendingNow/);
  assert.match(buttons, /deletePresetBtn\.disabled = none \|\| sendingNow/);
  assert.match(buttons, /Disabled while SENDING/, 'the reason must be on the control, not in a log');

  // And the view object hands setSending out for app.js to drive from the
  // same place as the SENDING lamp.
  assert.match(js, /return \{ el, open, setSending \};/);
  assert.match(
    ui('app.js'),
    /settings\.setSending\(!!currentSenderState && currentSenderState !== backend\.SENDER_STATE\.STOPPED\)/,
    'app.js must drive the gate from renderSenderLamp, the same derivation the lamp uses',
  );
});

test('the preset card carries no scope readout or machine-fields note', () => {
  // Both were removed at the operator's request — the card is the picker and
  // its buttons only. The safety they narrated (a preset cannot carry a device
  // id) is still enforced by construction in presets.js and Go's whitelist, so
  // there is nothing to render. Guard the absence so the waffle cannot creep
  // back.
  const js = ui('settings.js');
  assert.equal(/Never part of a preset/.test(js), false, 'the machine-fields note must be gone');
  assert.equal(/Credentials scope/.test(js), false, 'the credential-scope readout must be gone');
  assert.equal(/renderPresetNote|renderPresetScopeLine|MACHINE_NOTE_TAGS/.test(js), false, 'their render code must be gone too');
});

test('collectConfig still restates every config.Config json tag and no preset key', () => {
  const js = ui('settings.js');
  const collect = js.slice(js.indexOf('function collectConfig()'), js.indexOf('function clearAllErrors()'));
  assert.ok(collect.length > 0);

  const struct = read(repoRoot, 'internal', 'config', 'config.go').match(/type Config struct \{[\s\S]*?\n\}/);
  assert.ok(struct);
  const tags = [...struct[0].matchAll(/json:"([^"]+)"/g)]
    .map((m) => m[1])
    // The Tile's nested x/y/w/h tags live under monitorTile and are restated
    // as monitorTile.{x,y,w,h}; the top-level key is what collectConfig owns.
    .filter((tag) => !['x', 'y', 'w', 'h'].includes(tag));
  for (const tag of tags) {
    // headphoneEndpointId is restated through the DEVICE_KEY_SRT constant —
    // the one shared spelling of the SRT headphone key (returnsource.js) —
    // so the literal tag does not appear in collectConfig's body.
    const restated =
      collect.includes(tag) || (tag === 'headphoneEndpointId' && collect.includes('DEVICE_KEY_SRT'));
    assert.ok(
      restated,
      `collectConfig no longer restates "${tag}": saveConfig REPLACES the whole document, so a ` +
        'field this form does not restate is a field this form DELETES',
    );
  }
  assert.ok(
    !/preset/i.test(collect),
    'collectConfig has grown a preset key; it has no Go field on Config and would be silently ' +
      'discarded on every save — the pictureSource trap again',
  );
});

// ---------------------------------------------------------------------------
// The preset PREVIEW
// ---------------------------------------------------------------------------
//
// Selecting a preset used to do nothing visible: the change was described only
// inside the confirm() dialog, read once, in a hurry, with the form it
// describes hidden behind the modal. It is drawn on the FORM now — the box
// holds the value the preset would put there, the row says what it replaces.
//
// The decisions are pure and are driven for real in presetpreview.test.js. What
// is asserted HERE is the form's half, which has no jsdom to drive it: that the
// preview hangs off the SELECT event, that it is built from the existing diff
// rather than a second comparison, that every route out of it puts the borrowed
// controls back — and, the one with teeth, that a preview can never be SAVED.

test('selecting a preset previews it — on the change event, not on Apply', () => {
  const js = ui('settings.js');
  assert.match(
    js,
    /presetSelect\.addEventListener\('change', \(\) => renderPresetPreview\(\)\)/,
    'the preview must hang off the picker\'s selection; on the Apply button it would be no preview at all',
  );

  const render = js.slice(js.indexOf('function renderPresetPreview()'), js.indexOf("presetSelect.addEventListener('change'"));
  assert.ok(render.length > 0, 'settings.js must define renderPresetPreview');
  // Built from the pure model, against the SAVED config — not a second
  // comparison written here, which could disagree with the one the apply makes.
  // This is now the ONLY rendering of the change there is: the confirm dialog
  // that used to carry its own copy has gone.
  assert.match(render, /diffPreset\(lastLoadedConfig, preset\.fields\)/);
  assert.match(render, /planPresetPreview\(diff, preset\.fields, previewControls\(diff\)\)/);
  // Redraw, never layer: switching the picker from one preset to another must
  // not leave the first preset's notes under the second's.
  assert.match(
    render,
    /function renderPresetPreview\(\) \{\s*clearPresetPreview\(\);/,
    'the redraw must clear FIRST, or two selections in a row stack their notes',
  );
  // And the quiet case: the active preset, or one that differs in nothing,
  // leaves the screen exactly as it was — UNLESS the file carries keys a preset
  // does not honour, which is a fact about the file rather than about the diff
  // and is worth stating even when nothing would change. That note used to live
  // in the confirm dialog, which fired on every apply including a no-change one;
  // widening the quiet case is how it keeps that reach without a modal.
  assert.match(
    render,
    /if \(rows\.length === 0 && ignoredNote === ''\) return;/,
    'a preset that changes nothing and carries nothing unhonoured must draw nothing — ' +
      'not an empty flourish',
  );
});

test('a preview is not an edit: it is withdrawn before anything reads the form', () => {
  // THE ONE WITH TEETH. The preview writes into the REAL controls — a ghost
  // drawn over a box is a second rendering that can disagree with the first,
  // and a <select> has nowhere to draw one — so collectConfig would happily
  // save the preset's values: a half-applied instance switch from a button that
  // says "Save settings", with no confirmation, no credential scope and no
  // monitor rebuild.
  const js = ui('settings.js');
  const save = js.slice(js.indexOf('async function handleSave()'), js.indexOf('const config = collectConfig()'));
  assert.match(
    save,
    /clearPresetPreview\(\);/,
    'handleSave must withdraw the preview BEFORE collectConfig reads the controls',
  );

  // populate() clears first as well, so a preview can never outlive the config
  // it was diffed against — the stale baseline would be silent.
  const populate = js.slice(js.indexOf('function populate(config)'), js.indexOf('lastLoadedConfig = config;'));
  assert.match(populate, /clearPresetPreview\(\);/, 'populate must clear the preview before it rewrites the form');

  // And typing into a previewed box hands that box back to the operator, on
  // both events — a <select> announces itself with 'change' and never 'input'.
  assert.match(js, /form\.addEventListener\('input', \(e\) => releasePreviewedControl\(e\.target\)\)/);
  assert.match(js, /form\.addEventListener\('change', \(e\) => releasePreviewedControl\(e\.target\)\)/);
});

test('applying clears the preview and leaves the operator where they are', () => {
  const js = ui('settings.js');
  const handler = js.slice(js.indexOf('async function handleApplyPreset()'), js.indexOf('async function handleSavePresetAs()'));

  // The values are the values now, so the green goes. Explicit rather than
  // relying on populate() below it: a handler that returns nothing must still
  // not leave green boxes claiming a change that has already happened.
  assert.match(
    handler,
    /const merged = await handlers\.onApplyPreset\(preset\.id\);\s*(\/\/[^\n]*\n\s*)*clearPresetPreview\(\);/,
    'the apply must clear the preview as soon as it has committed',
  );

  // AND NOTHING MAY TURN BACK BEFORE IT. The declined confirm used to be a
  // second way out of this function, and it sat above every line below — the
  // scroll read, the commit and the clear alike. With the dialog gone the only
  // early return is the "no preset selected" guard at the top; a new one added
  // underneath it would be a press of Apply that silently does nothing, on a
  // button whose whole job is now to apply.
  //
  // Comments stripped first, and that is not incidental: the prose explaining
  // why the dialog went talks about the SRT return and about a returned config,
  // and a guard that a word does not appear must never be satisfiable — or
  // breakable — by an explanation of itself.
  const guard = "typeof handlers.onApplyPreset !== 'function') return;";
  const afterGuard = handler.slice(handler.indexOf(guard) + guard.length);
  const beforeCommit = afterGuard
    .slice(0, afterGuard.indexOf('const scrollTop = form.scrollTop;'))
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1');
  assert.ok(
    !/\breturn\b/.test(beforeCommit),
    'Apply applies: nothing may return between the press and the commit',
  );

  // SAME PAGE, SAME SCROLL POSITION. Clearing the preview removes its notes,
  // the form gets shorter, and a scroll container whose content shrinks has its
  // scrollTop clamped by the browser — the page moving under someone who only
  // pressed a button. Disabling a focused button also blurs it.
  assert.match(handler, /const scrollTop = form\.scrollTop;/);
  assert.match(handler, /form\.scrollTop = scrollTop;/);
  assert.match(handler, /const hadFocus = document\.activeElement === applyPresetBtn;/);
  assert.match(handler, /if \(hadFocus && !applyPresetBtn\.disabled\) applyPresetBtn\.focus\(\);/);
  for (const gone of ['scrollIntoView', 'window.scrollTo', 'handlers.onBack()']) {
    assert.equal(
      handler.includes(gone),
      false,
      `applying a preset must not ${gone}: the operator stays on this screen, where they were`,
    );
  }

  // AND THE SCREEN THEY ARE LEFT ON HAS TO BE RIGHT. Staying is what makes this
  // necessary: open() is what re-lists the instance's events, open() runs on
  // entering Settings, and an apply that never leaves never triggers it — so the
  // event picker would keep offering the OLD instance's events beside a host box
  // naming the new one. A list an operator can choose the wrong thing from is
  // worse than no list.
  assert.match(
    handler,
    /void refreshEventsAfterSave\(true\)/,
    'an apply must re-list the events for the instance it just switched to',
  );
});

test('the confirm dialog is gone, and everything it carried has somewhere else to be', () => {
  // THE DIALOG LOST ITS ARGUMENT. It was the last chance to back out of an
  // action that restarts the KVS monitor, the SRT return and the picture — but
  // it was drawn OVER the form it described, so its thirteen-line list stood in
  // front of the same change now marked in green on the boxes it lands in, and
  // it is not the safety on this button in any case: Go's ApplyPreset refuses
  // while SENDING, and this screen disables Apply with that reason on it. The
  // owner, having used the build: "we don't need the confirm popup now we have
  // the green text."
  //
  // What this test guards is the OTHER half — that removing it lost nothing.
  // The dialog said three things and only one of them was the diff.
  const js = ui('settings.js');
  const handler = js.slice(js.indexOf('async function handleApplyPreset()'), js.indexOf('async function handleSavePresetAs()'));
  assert.ok(
    !handler.includes('window.confirm('),
    'Apply must apply: the modal both duplicated the preview and stood in front of it',
  );

  // 1. THE KEYS A FILE CARRIES THAT A PRESET DOES NOT HONOUR. A real fact about
  //    a hand-edited or foreign file, and one the diff cannot express — the
  //    whitelist means those keys never reach a row. It must still be said, and
  //    said BEFORE the operator commits, so it moved to the selection preview.
  const render = js.slice(js.indexOf('function renderPresetPreview()'), js.indexOf("presetSelect.addEventListener('change'"));
  assert.match(
    render,
    /describeIgnoredKeys\(filterPresetFields\(preset\.fields\)\.ignored\)/,
    'the ignored-key note must survive the dialog it used to live in',
  );
  assert.match(
    render,
    /presetPreviewLine\.textContent = \[describePresetPreview\(preset\.name, rows\), ignoredNote\]/,
    'and reach the operator on the summary line, which needs no dismissing',
  );

  // 2. WHAT APPLYING COSTS: the monitor and the picture reconnect, and the
  //    device fields deliberately do not move. Nothing on the form says either,
  //    so describePresetPreview's line says both — driven for real, on the
  //    rendered string, in presetpreview.test.js rather than grepped as prose
  //    out of a source file here.
  //
  //    And it is said a second time at the moment it starts, on this screen,
  //    because by then the preview line has gone with the green.
  assert.match(
    handler,
    /setSaveMessage\(`Applied "\$\{preset\.name\}"\. The monitor and the picture are reconnecting\.`/,
    'several seconds of black picture and silence, unexplained, reads as a fault',
  );

  // 3. And the diff itself, which was already on the form and is the reason the
  //    dialog could go at all.
  assert.match(render, /diffPreset\(lastLoadedConfig, preset\.fields\)/);
});

test('the preview is readable without colour, and an emptied box still reads as a change', () => {
  // Colour-blind operators, and a gallery projector that washes this screen
  // out. The green is the fastest signal, never the only one: the note carries
  // the from and the to in words, the row is marked with a class main.css draws
  // as a DASHED border (a shape, not a colour), and a field the preset CLEARS —
  // statusKey to blank takes the three lamps to NO STATUS — says so in the
  // placeholder, because an empty box is otherwise indistinguishable from one
  // the preset never mentioned.
  const js = ui('settings.js');
  assert.match(js, /const PREVIEW_CLEARED_PLACEHOLDER = 'cleared by this preset'/);
  assert.match(js, /if \(row\.cleared\) target\.input\.placeholder = PREVIEW_CLEARED_PLACEHOLDER;/);
  assert.match(js, /note\.textContent = row\.note;/, 'every changed row must carry its from/to in words');
  assert.match(js, /const PREVIEW_CLASS = 'field--preset-preview'/);
});

test('the preview restores exactly what it borrowed', () => {
  // It holds the operator's own value AND placeholder for every box it writes
  // into. Restoring the value but not the placeholder would leave "cleared by
  // this preset" under a field nobody is previewing.
  const js = ui('settings.js');
  const clear = js.slice(js.indexOf('function clearPresetPreview()'), js.indexOf('function releasePreviewedControl'));
  assert.ok(clear.length > 0, 'settings.js must define clearPresetPreview');
  assert.match(clear, /box\.input\.value = box\.value;/);
  assert.match(clear, /box\.input\.placeholder = box\.placeholder;/);
  assert.match(clear, /wrap\.classList\.remove\(PREVIEW_CLASS\)/);
  assert.match(clear, /note\.remove\(\)/);
  assert.match(clear, /presetPreview = null;/);

  // THE SUMMARY LINE GOES FIRST, ABOVE THE "nothing is previewed" GUARD.
  //
  // It can be on screen with no decoration behind it: a preset that changes no
  // field, but whose file carries keys a preset does not honour, is announced in
  // that line and nowhere else — that warning came off the confirm dialog and
  // this is where it landed. Clearing it below the guard would strand it on a
  // picker that has since moved, which is a caution about a preset the operator
  // can no longer see selected.
  assert.match(
    clear,
    /presetPreviewLine\.hidden = true;\s*presetPreviewLine\.textContent = '';\s*presetPreviewLineFor = '';\s*if \(presetPreview === null\) return;/,
    'the line must be cleared unconditionally, before the decorations are',
  );

  // And whatever asks "is anything on screen about a preset" must read the LINE,
  // for the same reason: it is the half that is always there when anything is.
  const refresh = js.slice(js.indexOf('async function refreshPresets()'), js.indexOf('async function handleApplyPreset()'));
  assert.match(
    refresh,
    /if \(presetPreviewLineFor !== '' && presetPreviewLineFor !== presetSelect\.value\)/,
    'a refresh that moves the selection must withdraw the line as well as the green',
  );
});

test('a field with no row of its own gets its preview where the operator can see it', () => {
  // The same rule ERROR_SURROGATES follows, for the same reason: a decoration
  // attached to a hidden input is a change the operator cannot see. m2lxHost's
  // row IS the address box — and what goes in it is the BASE address, never the
  // live-operation form the write path just stopped producing. monitorTile is
  // one value across four boxes, so it takes a note under the grid rather than
  // four numbers each claiming to be the change.
  const js = ui('settings.js');
  const surrogates = js.slice(js.indexOf('const PREVIEW_SURROGATES'), js.indexOf('function previewTargetFor'));
  assert.match(surrogates, /m2lxHost: \(\) => \(\{ wrap: liveURLRow\.wrap, input: liveURLInput, format: formatM2LXAddress \}\)/);
  assert.match(surrogates, /monitorTile: \(\) => \(\{ wrap: tileGrid, input: null \}\)/);
  // And anything else without a row falls through to "named in the summary
  // line, decorated nowhere" — which is why that line lists every changed field
  // instead of counting them.
  assert.match(
    js,
    /return field && field\.wrap \? \{ wrap: field\.wrap, input: field\.input \} : null;/,
    'a field with no wrap must resolve to no target rather than to a guess at one',
  );
});

test('what the operator is shown before an apply is built from the pure model', () => {
  // This used to be about the confirm dialog, which computed the diff and the
  // ignored keys itself, in the handler, "before anything moves". The dialog is
  // gone; the obligation is not, it has moved one step earlier — to SELECTION,
  // where both are drawn on the form and stated in the card, still from the pure
  // model and still against the SAVED config rather than against keystrokes.
  const js = ui('settings.js');
  const render = js.slice(js.indexOf('function renderPresetPreview()'), js.indexOf("presetSelect.addEventListener('change'"));
  assert.match(render, /diffPreset\(lastLoadedConfig, preset\.fields\)/, 'the preview must diff against the SAVED config');
  assert.match(render, /filterPresetFields\(preset\.fields\)/, 'and name the keys the apply will ignore');

  // The handler is then only the commit, and it still owns nothing it should
  // not: app.js runs the sequence because it can reach the monitor and the
  // picture, and the form redraws from the RETURNED config, which is the
  // authority — never from the preset fields this screen happens to hold.
  const handler = js.slice(js.indexOf('async function handleApplyPreset()'), js.indexOf('async function handleSavePresetAs()'));
  assert.match(
    handler,
    /await handlers\.onApplyPreset\(preset\.id\)/,
    'the sequence itself belongs to app.js, which can reach the monitor and the picture',
  );
  assert.match(handler, /populate\(merged\)/, 'the form must redraw from the RETURNED config, the authority');
});

// ---------------------------------------------------------------------------
// The Remote access group — status only
// ---------------------------------------------------------------------------
//
// The listener is unauthenticated by the owner's decision (docs/remote-access.md),
// so the Settings group manages nothing: no client accounts, no capability tiers,
// no enable/bind/port editing sprawl — just a read-only readout of whether the
// listener is on and which HTTP/HTTPS ports it is bound on. remotewiring.test.js
// carries the absence guards; this pins the group's shape and derivation.

test('the Remote access group renders the bound-port status and nothing to configure', () => {
  const js = ui('settings.js');
  // The group exists, hidden on a remote client, as its own settings-group.
  assert.match(js, /openGroup\(remoteHeading, 'settings-group--remote'\)/);
  assert.match(js, /remoteGroup\.hidden = isRemoteView/);
  // It reads GetRemoteState and derives "running" from the cert fingerprint —
  // there is no running field in the new RemoteState shape.
  assert.match(js, /await backend\.getRemoteState\(\)/);
  assert.match(
    js,
    /s\.certFingerprint === '' \? false : true|s\.certFingerprint !== ''|certFingerprint === ''/,
    'running must be derived from certFingerprint, not read from a running field',
  );
  // It shows the bound HTTP and HTTPS ports/URLs.
  assert.match(js, /s\.httpURL \|\| \(s\.httpPort/);
  assert.match(js, /s\.httpsURL \|\| \(s\.httpsPort/);
});

// ---------------------------------------------------------------------------
// The M2L-X address field
// ---------------------------------------------------------------------------
//
// THE DEFECT THESE PIN, found by running the macOS build. The address box ran
// its contents through parseLiveOperationURL, which REQUIRES a
// /live-operation/<event id> segment. Pasting the instance's own address —
//
//     https://m2lx-wslstudios-matcht.etapsiota.com
//
// — was therefore refused with an error and did not even fill the host in, so
// the operator was still made to go and find a full live-operation URL for an
// id the application can now ask for itself (GET /api/events/overview, via
// internal/m2lx/events.go and App.ListEvents).
//
// The parsing itself is pure and is driven for real in liveurl.test.js. What is
// asserted HERE is the FORM's half: that the field is wired to the parser that
// takes both forms, that a bare address does not throw away an event id that is
// already known, and that the event listing is actually asked for on that path
// — a picker that never populates would leave the operator exactly where the
// refused paste left them.

test('the address field is wired to the parser that takes BOTH forms', () => {
  const js = ui('settings.js');
  assert.match(js, /import \{ parseM2LXAddress, formatM2LXAddress \} from '\.\/liveurl\.js'/);
  assert.match(js, /const parsed = parseM2LXAddress\(raw\)/, 'the field must parse with parseM2LXAddress');
  assert.equal(
    /parseLiveOperationURL/.test(js),
    false,
    'the address field is back on the strict parser, which refuses the instance address outright',
  );
});

test('the address field no longer asks for the live-operation page', () => {
  // The label and the placeholder are the only instructions on this row. A
  // placeholder showing "/live-operation/…" tells the operator to go and find
  // one even though the box no longer needs it.
  const js = ui('settings.js');
  const field = js.slice(js.indexOf('const liveURLInput = textInput'), js.indexOf('function addHiddenField'));
  assert.ok(field.length > 0, 'settings.js must still build the address row');
  assert.equal(
    /placeholder = '[^']*live-operation/.test(field),
    false,
    'the placeholder still shows a live-operation URL as the shape to type',
  );
  assert.match(field, /instance address is enough/, 'the hint must say the instance address suffices');
  assert.match(
    field,
    /full live-operation URL is also accepted/,
    'and that the pasted URL still works — it is the only source of an id before sign-in',
  );
});

test('a bare address sets the host and does not clear a known event id', () => {
  // The stored id is the fallback for every case the instance cannot be
  // enumerated (not signed in, unreachable, an older build). Blanking it on a
  // keystroke would throw away the one value that works offline.
  const js = ui('settings.js');
  const apply = js.slice(js.indexOf('function applyLiveURL()'), js.indexOf('// \'input\' rather than \'change\''));
  assert.ok(apply.length > 0, 'settings.js must still have applyLiveURL');
  assert.match(apply, /fields\.m2lxHost\.input\.value = parsed\.host/);
  assert.match(
    apply,
    /if \(parsed\.eventId !== ''\) fields\.eventId\.input\.value = parsed\.eventId/,
    'the event id must only be written when the address actually carried one',
  );
});

test('populate puts the BASE address in the box, never a live-operation URL', () => {
  // THE DEFECT THIS PINS, in the operator's words: "it seems to do some stuff
  // where it is writing URLs with extra parts, not just pasting the whole URL
  // in". This line used to pass config.eventId as well, and formatM2LXAddress
  // then re-synthesised https://<host>/live-operation/<id> — so the box handed
  // back a longer string than the one that was pasted into it, naming a page on
  // the M2L-X GUI rather than the instance the field asks for.
  //
  // The event id is not lost by dropping it here: it lives in the hidden
  // eventId field, on the derived line under the address, and on the event
  // picker's own row. A config with a host and no event still shows the host,
  // which is the older defect (an empty box on a signed-in app) that this must
  // not reintroduce while fixing the newer one.
  const js = ui('settings.js');
  assert.match(js, /liveURLInput\.value = formatM2LXAddress\(config\.m2lxHost\)/);
  assert.equal(
    /formatM2LXAddress\(config\.m2lxHost, config\.eventId\)/.test(js),
    false,
    'the address box is synthesising a live-operation URL again',
  );
  // And nothing may BUILD one any more, whatever it is handed. The parser still
  // reads them — that half is load-bearing and is tested in liveurl.test.js.
  assert.equal(
    /function formatLiveOperationURL\b/.test(ui('liveurl.js')),
    false,
    'liveurl.js has a live-operation URL WRITER again; the display path is how the long form crept back last time',
  );
});

test('the event listing is asked for on the address path and after a save', () => {
  const js = ui('settings.js');
  const apply = js.slice(js.indexOf('function applyLiveURL()'), js.indexOf('// \'input\' rather than \'change\''));
  assert.match(
    apply,
    /scheduleEventRefresh\(parsed\.host\)/,
    'entering a bare address must lead to a listing, or the picker never populates',
  );
  // Debounced, because applyLiveURL runs on every keystroke.
  assert.match(js, /ADDRESS_SETTLE_MS/, 'the listing must wait for the address to settle');
  assert.match(js, /setTimeout\(\(\) => \{\s*addressSettleTimer = null;/);

  // And after a save, which for a NEW instance is the first moment a listing can
  // succeed at all: the client is rebuilt from the saved config and signs in.
  assert.match(js, /if \(saved\) void refreshEventsAfterSave\(hostChanged\)/);
  assert.match(js, /await refreshEvents\(\);/, 'open() must still list on entry');
});

test('events are only listed for the instance the app is actually signed in to', () => {
  // THE TRAP THIS CLOSES. backend.listEvents() takes no host — App.ListEvents
  // uses the client the control plane built from the SAVED configuration — so
  // listing while a different address is being typed would answer with the
  // PREVIOUS instance's events, and the auto-select rule would then write
  // another instance's event id into this form.
  const js = ui('settings.js');
  assert.match(
    js,
    /if \(host === '' \|\| host !== savedM2LXHost \|\| host === listedHost\) return;/,
    'the listing must be gated on the address matching the saved host',
  );
  assert.match(
    js,
    /Press Save settings to sign in to this instance/,
    'and when it does not match, the operator must be told what to do about it',
  );

  // The Go side of the same fact: a host-taking ListEvents would make the gate
  // unnecessary, and leaving the gate in place would then be a bug of its own.
  const go = read(repoRoot, 'app.go');
  assert.match(
    go,
    /func \(a \*App\) ListEvents\(\) \(\[\]m2lx\.Event, error\)/,
    'App.ListEvents has grown a parameter; the Settings screen gates on the saved host because it has none',
  );
  assert.match(ui('backend.js'), /export async function listEvents\(\) \{/);
});

test('liveurl.js no longer claims the events cannot be listed', () => {
  // The header used to state, at length, that no endpoint listed events and
  // that the pasted URL was therefore the ONLY source of an id. That is the
  // belief the defect was built on, internal/m2lx/events.go disproves it, and a
  // comment asserting it would send the next reader back round the same loop.
  const js = ui('liveurl.js');
  assert.equal(
    /there is no event-list endpoint|NO endpoint that lists it/.test(js),
    false,
    'liveurl.js still says no endpoint lists events; internal/m2lx/events.go calls one',
  );
  assert.match(js, /\/api\/events\/overview/, 'and it should name the endpoint that does');
  assert.match(
    read(repoRoot, 'internal', 'm2lx', 'events.go'),
    /eventsOverviewPath = "\/api\/events\/overview"/,
    'the endpoint liveurl.js names must be the one Go calls',
  );
  // The historical note stays: the paste path is still supported and still the
  // only source of an id before a successful sign-in.
  assert.match(js, /live-operation/, 'the strict form must still be documented');
});

test('the Remote access group has no listener-configuring controls', () => {
  // No toggle, no bind box, no port box, no Apply — the group is a readout. A
  // control here would be editable sprawl the owner asked to keep out.
  const js = ui('settings.js');
  for (const gone of ['f-remoteEnabled', 'f-remoteBind', 'f-remotePort', 'Apply listener settings']) {
    assert.equal(js.includes(gone), false, `settings.js still builds the "${gone}" control`);
  }
});
