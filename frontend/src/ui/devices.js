/**
 * The device dropdowns' pure logic: what order the list is shown in, what a
 * saved-but-missing selection looks like, and which Windows endpoint-id
 * namespace a pasted GUID belongs to.
 *
 * Owner: WP-5b. No DOM, no browser API — it runs under `node --test` with
 * nothing installed, and nothing here should ever need anything installed.
 * home.js's fillDeviceSelect and validate.js are the callers; devices.test.js
 * drives everything here for real.
 *
 * ======================= THE FAILURE THIS SERVES ============================
 *
 * GStreamer's wasapi2 provider republishes every Windows RENDER (playback)
 * endpoint as an Audio/Source "loopback" device, so the commentary-input list
 * offered playback endpoints — measured: 11 of 25 entries on the dev machine.
 * An operator on another machine selected one ("we had an output selected as
 * an input" — their words), the pipeline prerolled, wasapi2's rbuf failed
 * ASYNCHRONOUSLY, and the sender blamed the SRT network and retried forever.
 * The Go side now refuses those devices (internal/gst/device_id.go); this
 * module is the frontend's share of the same defence:
 *
 *   - the namespace helpers let the Settings form refuse a pasted playback
 *     GUID at the field, before it is saved and Start fails on it;
 *   - describeDeviceSelection makes a saved id that is NOT in today's list
 *     visible as such, instead of the dropdown silently showing device #1
 *     selected while Start refuses the id actually saved — two truths the
 *     operator cannot reconcile from the screen;
 *   - sortDevices puts the devices an operator actually wants — real
 *     microphones — above the walls of virtual endpoints (eight DVS pairs on
 *     the measured machine) that pushed them off screen.
 *   - labelDevices makes the TWO ENTRIES FOR ONE BOX tellable apart; see its
 *     own section below, which is a different failure with the same shape.
 */

/**
 * The two WASAPI IMMDevice endpoint-id namespaces, verbatim from
 * internal/gst/device_id.go (CaptureEndpointPrefix / RenderEndpointPrefix).
 * The middle token is Windows' data-flow discriminator: 0.0.1 is capture,
 * 0.0.0 is render. devices.test.js pins these against the operator's measured
 * failing id so the two sides cannot drift.
 */
export const CAPTURE_ENDPOINT_PREFIX = '{0.0.1.00000000}.';
export const RENDER_ENDPOINT_PREFIX = '{0.0.0.00000000}.';

/**
 * The DEVINTERFACE class GUIDs Windows uses to name "the default capture /
 * render device" as pseudo entries — mirrored from device_id.go's
 * DefaultCaptureDeviceID / DefaultRenderDeviceID. They appear EMBEDDED in an
 * id (the pseudo entries and the \\?\SWD#MMDEVAPI# path form both carry them
 * mid-string), which is why the helpers below test containment, not prefix.
 */
export const DEFAULT_CAPTURE_DEVICE_GUID = '{2EEF81BE-33FA-4800-9670-1CD474972C3F}';
export const DEFAULT_RENDER_DEVICE_GUID = '{E6327CAD-DCEC-4949-AE8A-991E976A79D2}';

/**
 * isCaptureEndpointId reports whether id is POSITIVELY identified as a Windows
 * CAPTURE endpoint. Case-insensitive, because GUID casing is not stable across
 * the APIs that print these ids.
 *
 * The rule is deliberately ASYMMETRIC, exactly as device_id.go documents it:
 * an id of unrecognised shape fails this AND isRenderEndpointId, and such an
 * id must be ACCEPTED, never refused. Refusing unknown shapes would turn a
 * future Windows or GStreamer id-shape change into an unstartable commentary
 * position twenty minutes before kick-off; a refusal may key on
 * isRenderEndpointId alone.
 *
 * @param {unknown} id
 * @returns {boolean}
 */
export function isCaptureEndpointId(id) {
  const l = lower(id);
  return (
    l.startsWith(CAPTURE_ENDPOINT_PREFIX.toLowerCase()) ||
    l.includes(DEFAULT_CAPTURE_DEVICE_GUID.toLowerCase())
  );
}

/**
 * isRenderEndpointId reports whether id is POSITIVELY identified as a Windows
 * RENDER (playback) endpoint — the namespace of the operator's measured
 * failure, "{0.0.0.00000000}.{8678ce58-...}". This is the only test a refusal
 * may key on; see isCaptureEndpointId for why unknown shapes are neither.
 *
 * @param {unknown} id
 * @returns {boolean}
 */
export function isRenderEndpointId(id) {
  const l = lower(id);
  return (
    l.startsWith(RENDER_ENDPOINT_PREFIX.toLowerCase()) ||
    l.includes(DEFAULT_RENDER_DEVICE_GUID.toLowerCase())
  );
}

/**
 * describeDeviceSelection says what a dropdown should do about a saved device
 * id: nothing (present, or nothing saved), or show it as missing.
 *
 * THE DEFECT THIS REMOVES: a saved id absent from today's list used to fall
 * through fillDeviceSelect's `if` silently, leaving device #1 showing as
 * selected. The operator then reads a plausible device on screen while Start
 * refuses the id actually in config.json — two statements about the same
 * setting that cannot both be true, with no way to reconcile them from the
 * GUI. The label below is what the dropdown shows instead: the truth, with
 * the id in it, because the id is the only handle the operator has on what
 * was saved (a docked USB interface, a stopped Dante Virtual Soundcard).
 *
 * A blank savedId reports present:true — there is no selection to be absent,
 * and the dropdown's existing "show the first device" behaviour stands.
 *
 * @param {Array<{id: string}>|null|undefined} devices today's device list
 * @param {unknown} savedId the id out of config.json, if any
 * @returns {{present: boolean, savedId: string, label: string}} label is the
 *   marker-option text, '' when no marker is needed
 */
export function describeDeviceSelection(devices, savedId) {
  const id = typeof savedId === 'string' ? savedId : '';
  const list = Array.isArray(devices) ? devices : [];
  const present = id === '' || list.some((d) => d && d.id === id);
  return {
    present,
    savedId: id,
    label: present ? '' : `NOT PRESENT — ${id}`,
  };
}

/**
 * DEVICE_RANKS orders the FAMILIES of a device list: real hardware first,
 * virtual endpoints after it. Lower sorts first; the first matching pattern
 * wins; anything unmatched is rank 0 — an unrecognised device is presumed to
 * be the operator's real microphone, not filed under the virtual walls.
 *
 * WHY RANKS AT ALL: on the measured dev machine the list carries eight
 * "DVS Receive N-M" pairs and an NDI webcam feed alongside the one Focusrite
 * the commentator actually plugs a microphone into. Alphabetical order files
 * "DVS Receive 1-2" above "Microphone (Focusrite...)" every time, so the
 * device that matters starts below the fold of every dropdown.
 *
 *   rank 0  real hardware (anything not matching the patterns below)
 *   rank 1  NDI / Webcam virtual audio — software sources somebody might
 *           occasionally mean, above the Dante wall
 *   rank 2  Dante Virtual Soundcard — sixteen entries of patch fabric,
 *           legitimate but never what a hurried operator is scanning for
 */
const DEVICE_RANKS = Object.freeze([
  { rank: 2, test: /dante virtual soundcard|\bDVS\b/i },
  { rank: 1, test: /\bNDI\b|webcam/i },
]);

/** deviceRank returns a device name's family rank; 0 for real hardware. */
function deviceRank(name) {
  for (const r of DEVICE_RANKS) {
    if (r.test.test(name)) return r.rank;
  }
  return 0;
}

/**
 * compareDevicesForDisplay orders two devices the way an operator reads a
 * list: family rank first, then the names with embedded numbers compared AS
 * NUMBERS — so "DVS Receive 2-3" precedes "DVS Receive 10-11" instead of the
 * classic 1, 10, 11 … 19, 2 byte order the operator has already complained
 * about once (the mixer's strip list had the same fault).
 *
 * The digit/text chunk walk is the SAME IDIOM as mixer/model.js's
 * compareStripsForDisplay — mirrored rather than imported, because that
 * comparator bakes in the strip families (cam/replay/vtr/MIC) and their ranks,
 * which are wrong for device names. The natural-sort walk itself is identical.
 *
 * @param {{id?: string, name?: string}} a
 * @param {{id?: string, name?: string}} b
 * @returns {number}
 */
export function compareDevicesForDisplay(a, b) {
  // Whitespace runs are collapsed BEFORE chunking, because Dante pads its
  // numbers for column alignment: single digits get a double space
  // ("DVS Receive  1-2") and double digits a single one ("DVS Receive 11-12").
  // Chunked as-is, the TEXT chunks differ — "DVS Receive  " vs "DVS Receive "
  // — and the comparison is decided there, before the numbers are ever looked
  // at: the shorter text chunk wins and 11-12 lands ABOVE 1-2. The operator
  // saw exactly that. Collapsing makes the text chunks equal, so the walk
  // reaches the digits and compares 1 against 11 as numbers. Display names
  // keep their real spacing; only the comparison key is normalised.
  const nameA = str(a && a.name).replace(/\s+/g, ' ').trim();
  const nameB = str(b && b.name).replace(/\s+/g, ' ').trim();
  const ra = deviceRank(nameA);
  const rb = deviceRank(nameB);
  if (ra !== rb) return ra - rb;

  // Split each name into alternating text and number runs and walk them in
  // step: text compared case-insensitively, digits compared numerically.
  const chunk = (s) => String(s).match(/\d+|\D+/g) || [];
  const ca = chunk(nameA);
  const cb = chunk(nameB);

  for (let i = 0; i < Math.min(ca.length, cb.length); i += 1) {
    const x = ca[i];
    const y = cb[i];
    const bothNumeric = /^\d/.test(x) && /^\d/.test(y);
    if (bothNumeric) {
      const nx = Number(x);
      const ny = Number(y);
      if (nx !== ny) return nx - ny;
      continue;
    }
    const lx = x.toLowerCase();
    const ly = y.toLowerCase();
    if (lx !== ly) return lx < ly ? -1 : 1;
  }

  if (ca.length !== cb.length) return ca.length - cb.length;
  // Identical names under the rules above: fall back to the raw name, then the
  // KIND, then the id, so the order is total and a refreshed list cannot
  // reshuffle equals.
  if (nameA !== nameB) return nameA < nameB ? -1 : 1;
  // The one place kind decides anything about ORDER, and it decides it only
  // between two entries the operating system has given the SAME name — which is
  // exactly the UltraStudio's pair of entries. The DeckLink one goes first
  // because it is the one carrying the microphone: measured, the CoreAudio twin
  // reports -96 dBFS on all sixteen channels with the mic live. Putting the
  // silent entry above the working one in a dropdown two identical lines long
  // is an invitation to pick the wrong one.
  const ka = kindOrder(a && a.kind);
  const kb = kindOrder(b && b.kind);
  if (ka !== kb) return ka - kb;
  const idA = str(a && a.id);
  const idB = str(b && b.id);
  return idA < idB ? -1 : idA > idB ? 1 : 0;
}

/**
 * sortDevices returns a NEW array of devices in display order — real
 * microphones first, then NDI/webcam virtual audio, then the Dante wall,
 * natural-sorted by name within each family. The input is not mutated:
 * app.js keeps currentInputDevices, and a sort that reordered it in place
 * would be a side effect on somebody else's state.
 *
 * @param {Array<{id?: string, name?: string}>|null|undefined} devices
 * @returns {Array<{id?: string, name?: string}>}
 */
export function sortDevices(devices) {
  if (!Array.isArray(devices)) return [];
  return devices.slice().sort(compareDevicesForDisplay);
}

// ===================== TWO ENTRIES, ONE PHYSICAL BOX =========================
//
// A Blackmagic UltraStudio enumerates TWICE, measured: once through CoreAudio,
// which the operating system names after the box, and once through the DeckLink
// driver, which names the same box. One of those two entries delivers the
// commentator's microphone. The other delivers silence — the CoreAudio twin
// reads -96 dBFS on all sixteen channels with the mic live — and there is
// nothing in the name, the order or the id to say which is which.
//
// That is the owner's original bug seen from the other end. It used to be
// invisible because captureDeviceID (deviceprovider_darwin.go) skipped any
// device publishing no unique-id, which HID the DeckLink entry entirely and
// offered only the silent one. With both offered, the operator has to be able
// to choose, at speed, under pressure, from a dropdown of two identical lines.
//
// So the kind is rendered into the label. Two rules, and the asymmetry is
// deliberate:
//
//   - a DeckLink entry is ALWAYS labelled. It is the unfamiliar one, and its
//     audio arrives from somewhere the device name does not mention: the video
//     signal on the card's input, not the computer's sound system.
//   - a native entry is labelled only when a DeckLink entry shares its name.
//     That is the collision above and the only case where the name alone is
//     ambiguous; suffixing every built-in microphone on every other machine
//     would be noise, and noise is what stops labels being read.
//
// NOTHING HERE PARSES AN ID. internal/gst/gst.go documents Device.ID as opaque
// and per-platform — a WASAPI GUID, a CoreAudio unique-id, whatever a future
// provider publishes — and the kind is a field precisely so that nobody has to
// infer it from a string shape. Measured and relevant: DeckLink audio and video
// publish the SAME persistent-id, because it names the CARD and not the stream,
// so an id says less here than it looks like it does.

/**
 * DEVICE_KIND mirrors internal/gst's Device.Kind values EXACTLY. These strings
 * are the contract; devices.test.js pins them against gst.go so the two sides
 * cannot drift into a state where every device silently falls through to the
 * unlabelled default.
 */
export const DEVICE_KIND = Object.freeze({
  /** The platform's own audio system: WASAPI on Windows, CoreAudio on macOS. */
  NATIVE: 'native',
  /** A Blackmagic DeckLink / UltraStudio capture card's embedded audio. */
  DECKLINK: 'decklink',
});

/**
 * The label suffixes, in the operator's vocabulary rather than the driver's.
 *
 * "DeckLink" and "CoreAudio" are the names of software nobody at a commentary
 * position chose or installed; SDI and HDMI are the cables in front of them.
 * The pair reads, on the measured machine:
 *
 *   Blackmagic UltraStudio 4K Mini — SDI/HDMI audio
 *   Blackmagic UltraStudio 4K Mini — computer sound input
 *
 * which answers "which one is my microphone" without knowing anything about
 * either driver, provided the microphone is patched into the card. Both lines
 * are needed: one labelled entry beside one bare entry reads as though the bare
 * one were the normal choice, which is precisely the wrong conclusion here.
 */
const DECKLINK_SUFFIX = 'SDI/HDMI audio';
const NATIVE_TWIN_SUFFIX = 'computer sound input';

/** The separator between an OS device name and the kind suffix. */
const LABEL_SEPARATOR = ' — ';

/**
 * labelDevices returns a NEW array of NEW objects, each the input device plus a
 * `label`: what the dropdown should show for it. The input is not mutated and
 * neither are its entries — app.js keeps currentInputDevices, and a device that
 * grew a label under it would be a side effect on somebody else's state.
 *
 * Order is preserved; this does not sort. Compose it with sortDevices at the
 * call site (home.js: `labelDevices(sortDevices(devices))`) so that each
 * function stays one idea.
 *
 * A device with no kind — every output device, every Windows entry today, an
 * older Go build — is labelled with its bare name. Falling through to the name
 * is the right default in all three cases: nothing is claimed that is not known.
 *
 * @param {Array<{id?: string, name?: string, kind?: string}>|null|undefined} devices
 * @returns {Array<{id?: string, name?: string, kind?: string, label: string}>}
 */
export function labelDevices(devices) {
  if (!Array.isArray(devices)) return [];

  // The names a DeckLink entry has claimed. Matching is on the NAME, normalised
  // the same way the comparator normalises it (case and whitespace runs), and
  // not on the id — see the section header: the id is opaque, and the DeckLink
  // card's own audio and video entries share theirs anyway.
  const decklinkNames = new Set();
  for (const d of devices) {
    if (d && d.kind === DEVICE_KIND.DECKLINK) decklinkNames.add(labelKey(d.name));
  }

  return devices.map((d) => {
    const device = d || {};
    const name = str(device.name);
    if (device.kind === DEVICE_KIND.DECKLINK) {
      return { ...device, label: name + LABEL_SEPARATOR + DECKLINK_SUFFIX };
    }
    if (device.kind === DEVICE_KIND.NATIVE && decklinkNames.has(labelKey(name))) {
      return { ...device, label: name + LABEL_SEPARATOR + NATIVE_TWIN_SUFFIX };
    }
    return { ...device, label: name };
  });
}

/** labelKey normalises a device name for the twin test: case and padding. */
function labelKey(name) {
  return str(name).replace(/\s+/g, ' ').trim().toLowerCase();
}

/**
 * kindOrder ranks the kinds for the comparator's final tie-break: DeckLink
 * first, native second, an unknown or absent kind last. Unknown sorting last
 * rather than first is the opposite of DEVICE_RANKS' "unrecognised is presumed
 * real hardware", and deliberately so — this tie-break only ever runs between
 * devices whose NAMES are already identical, where an entry nothing knows the
 * kind of is the one least worth putting at the top.
 */
function kindOrder(kind) {
  if (kind === DEVICE_KIND.DECKLINK) return 0;
  if (kind === DEVICE_KIND.NATIVE) return 1;
  return 2;
}

function str(v) {
  return typeof v === 'string' ? v : '';
}

function lower(v) {
  return typeof v === 'string' ? v.toLowerCase() : '';
}
