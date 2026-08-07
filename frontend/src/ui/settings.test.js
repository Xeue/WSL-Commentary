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
    srtHost: '',
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
