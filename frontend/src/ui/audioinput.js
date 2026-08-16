/**
 * ONE COMMENTARY-INPUT PICKER, over two capture subsystems.
 *
 * Owner: WP-5b. Pure — no DOM, no browser API, no backend — so `node --test`
 * drives every rule below for real with nothing installed (audioinput.test.js).
 * settings.js is the wiring; this file is what it wires.
 *
 * ===================== THE FAILURE THIS REMOVES =============================
 *
 * The commentary input used to be TWO controls asking one question. A <select>
 * chose the SUBSYSTEM ("This computer's audio devices" / "A Blackmagic DeckLink
 * card"), and the device itself was chosen somewhere else — the main screen's
 * dropdown for the computer's endpoints, a free-text box for the card's
 * persistent-id. So an operator who wanted the microphone on the SDI input had
 * to know that picking a microphone implies a capture subsystem, and had to set
 * the two halves consistently by hand. Setting one and not the other is a
 * configuration that saves cleanly and captures from the wrong place.
 *
 * The owner's words: "On the mic input, we should have both the decklink found
 * audio sources and the normal ones in the same drop down, use optgroup to
 * seperate them. And then do the correct setup based on what is selected."
 *
 * So there is ONE list now, grouped, and SELECTING AN ENTRY IS THE WHOLE SETUP:
 * deriveAudioInputEffects returns all three config fields, with the one that no
 * longer applies cleared. An operator picks a microphone; the subsystem follows
 * from the microphone, which is the only order in which it was ever knowable.
 *
 * ===================== NOTHING HERE PARSES A DEVICE ID ======================
 *
 * The grouping reads device.kind and nothing else. internal/gst/gst.go
 * documents Device.ID as opaque and per-platform — a WASAPI GUID, a CoreAudio
 * unique-id, a DeckLink persistent-id — and Kind is a field precisely so that
 * nobody has to infer it from a string shape. Measured and relevant: the fitted
 * UltraStudio's Audio/Source and Video/Source entries BOTH publish 2747401380,
 * because a persistent-id names the CARD and not the stream, so an id says less
 * here than it looks like it does.
 *
 * The option VALUE is the one string this module does take apart, and that is a
 * different act: it is a string this module BUILT, out of a kind it was given
 * and an id it never inspected. See encodeAudioInput.
 */

// The kind vocabulary and the display order, from the one module that mirrors
// internal/gst's Device.Kind. Imported rather than restated: a second spelling
// of "decklink" would file the card's own microphone under the computer's audio
// devices and nothing would report an error.
import { DEVICE_KIND, sortDevices, describeDeviceSelection } from './devices.js';

/**
 * The two commentary capture kinds, spelled exactly as internal/config spells
 * them (config.AudioSourceNative / config.AudioSourceDeckLink).
 *
 * They are the values collectConfig sends, so a drift here is the silent kind:
 * Go's Validate refuses an unrecognised kind by name, which is at least loud,
 * but a kind that differs only in case would save cleanly and capture from the
 * wrong subsystem. audioinput.test.js pins both against config.go's own
 * constants.
 *
 * They live here rather than in settings.js — where they used to — because the
 * planning below is what decides which one a selection means, and a constant
 * owned by the screen that merely renders the answer is a constant two files
 * have to agree about.
 */
export const AUDIO_SOURCE_NATIVE = 'native';
export const AUDIO_SOURCE_DECKLINK = 'decklink';

/**
 * normaliseAudioSourceKind maps anything to one of the two kinds, defaulting to
 * native — which is what every machine did before the field existed, and what a
 * config.json written before it holds.
 *
 * It is applied on the way IN as well as on the way out, and the way in is the
 * one that would otherwise bite: assigning an unrecognised value to a <select>
 * leaves it showing '' — no option selected — and a save would then write that
 * empty kind over whatever the file actually held.
 *
 * @param {unknown} value
 * @returns {string}
 */
export function normaliseAudioSourceKind(value) {
  return value === AUDIO_SOURCE_DECKLINK ? AUDIO_SOURCE_DECKLINK : AUDIO_SOURCE_NATIVE;
}

/**
 * THE GROUP HEADINGS, in the operator's language rather than the driver's.
 *
 * "CoreAudio", "WASAPI" and "DeckLink" are the names of software nobody at a
 * commentary position chose or installed. SDI and HDMI are the cables in front
 * of them, and "this computer" is the box under the desk. The pair has to answer
 * "which one is my microphone" to somebody who knows only where they plugged it
 * in — which is the same argument devices.js's label suffixes are written
 * around, arriving at the same words.
 */
export const NATIVE_GROUP_LABEL = 'Audio inputs on this computer';
export const DECKLINK_GROUP_LABEL = 'Audio on the capture card (SDI/HDMI)';

/**
 * The label for a native selection of nothing — an empty audioDeviceId, which
 * is what blankConfig holds and what every seat starts on. It is a real setting
 * and not an absence: internal/gst opens the platform's default input for it.
 */
export const DEFAULT_INPUT_LABEL = 'Default input';

/**
 * The label for a DeckLink selection with no persistent-id, which means "the
 * card in this machine" and is the right answer on every seat with one card.
 *
 * It is offered ONLY when it is what is already saved — see planAudioInputs. On
 * a machine whose card enumerates, naming the card is unambiguous and offering
 * both would put two entries meaning the same thing on adjacent lines, which is
 * the choice devices.js's twin-labelling exists to stop having to make.
 */
export const ANY_CARD_LABEL = 'The card in this machine';

/**
 * THE LINE THAT USED TO LIVE HERE, and why its absence is the deliverable.
 *
 * DECKLINK_AUDIO_NOT_BUILT was one sentence on the picker row, shown whenever a
 * DeckLink entry was selected: "This version cannot capture audio from the card
 * yet — START will refuse it. The card's video still works." It existed because
 * app.go's preflightCapture REFUSED a DeckLink commentary seat outright, and it
 * was said here rather than by greying the group out so that the operator met
 * the refusal at the moment of choosing rather than at kick-off.
 *
 * THE LEG IS BUILT. gst.PipelineOpts.AudioCaptureID puts decklinkaudiosrc in the
 * pipeline with sixteen channels, a decklinkvideosrc clocks it — the card drives
 * audio capture off the video clock, so one is required even when the picture is
 * the slate — and the mix matrix this screen's routing grid writes is what turns
 * those sixteen unpositioned channels into the pair that goes to air. The gate
 * in preflightCapture is gone, its own comment having said exactly when to
 * delete it, and this constant went with it.
 *
 * SO THERE IS NOTHING TO SAY ON THE ROW ANY MORE, and saying nothing is correct
 * rather than merely tidy: a note under a control that is working is a note an
 * operator has to read and dismiss every time they open the screen. The one
 * thing that can still be wrong with a selection — the saved device is not
 * plugged in — is renderAudioInputNote's only remaining case.
 *
 * What DOES still refuse, and where: a DeckLink commentary seat on a machine
 * with no card, or with two and none named, is refused by App.preflightCapture,
 * naming audioSourceKind or decklinkPersistentId. Those are refusals about THIS
 * MACHINE RIGHT NOW rather than about the build, they cannot be known when the
 * screen is drawn, and a machine question belongs at START.
 */

/**
 * The separator between the kind and the id inside an <option> value.
 *
 * ===================== WHY THIS IS SAFE TO SPLIT ============================
 *
 * A device id may contain anything, colons included, and nothing above
 * internal/gst may assume otherwise. This survives that because the split is on
 * the FIRST separator only: the left half is one of two known words, neither of
 * which contains a colon, so everything after the first colon is the id
 * verbatim, however many more colons it has. The id is never examined, only
 * carried — which is the difference between this and parsing an id.
 */
const VALUE_SEPARATOR = ':';

/**
 * encodeAudioInput builds the <option> value for one (kind, id) pair.
 *
 * @param {string} kind
 * @param {string} id
 * @returns {string}
 */
export function encodeAudioInput(kind, id) {
  return `${normaliseAudioSourceKind(kind)}${VALUE_SEPARATOR}${typeof id === 'string' ? id : ''}`;
}

/**
 * decodeAudioInput takes an <option> value apart again. Anything unrecognised
 * reads as the native default input, for normaliseAudioSourceKind's reason: a
 * <select> that has lost its selection reports '', and the safe reading of ''
 * is what the machine did before any of this existed.
 *
 * @param {unknown} value
 * @returns {{kind: string, id: string}}
 */
export function decodeAudioInput(value) {
  const raw = typeof value === 'string' ? value : '';
  const at = raw.indexOf(VALUE_SEPARATOR);
  if (at < 0) return { kind: AUDIO_SOURCE_NATIVE, id: '' };
  return {
    kind: normaliseAudioSourceKind(raw.slice(0, at)),
    id: raw.slice(at + VALUE_SEPARATOR.length),
  };
}

/**
 * deriveAudioInputEffects is THE statement of what selecting an entry means: all
 * three config fields, with the one that no longer applies CLEARED.
 *
 * Clearing matters as much as setting. A seat that moved from the card to a USB
 * microphone, leaving decklinkPersistentId behind, carries a card id in its
 * configuration that nothing is using and that the next reader — a person or a
 * preset diff — has to work out is stale. Worse in the other direction: an
 * audioDeviceId left over from the computer's audio stack sitting beside
 * audioSourceKind "decklink" is two answers to one question, and which one wins
 * is a property of internal/gst rather than of anything on screen.
 *
 * @param {unknown} value an <option> value built by encodeAudioInput
 * @returns {{audioSourceKind: string, audioDeviceId: string, decklinkPersistentId: string}}
 */
export function deriveAudioInputEffects(value) {
  const { kind, id } = decodeAudioInput(value);
  const decklink = kind === AUDIO_SOURCE_DECKLINK;
  return {
    audioSourceKind: kind,
    audioDeviceId: decklink ? '' : id,
    decklinkPersistentId: decklink ? id : '',
  };
}

/**
 * planAudioInputs decides what the one picker holds and which entry is selected.
 *
 * `devices` is App.ListInputDevices' answer — [{id, name, kind}] — or null for
 * NOT ASKED / NOT ANSWERED. Null and [] are different claims and only the second
 * one says this machine has no capture devices; both leave the saved selection
 * on screen, which is the property that matters here.
 *
 * ===================== A SAVED SELECTION IS NEVER DROPPED ===================
 *
 * A configuration can name a device that is not plugged in today — a docked USB
 * interface, a card in a machine this config.json was copied from, a stopped
 * Dante Virtual Soundcard. It used to fall through the dropdown's fill silently,
 * leaving device #1 showing as selected: the operator reads a plausible device
 * on screen while Start refuses the id actually saved, and there is nothing on
 * the screen to reconcile the two with. So an absent selection is added to the
 * list, in its own group, MARKED — describeDeviceSelection words the marker, so
 * this screen and the main screen's dropdown say it the same way.
 *
 * @param {Array<{id?: string, name?: string, kind?: string}>|null|undefined} devices
 * @param {{audioSourceKind?: unknown, audioDeviceId?: unknown, decklinkPersistentId?: unknown}} config
 * @returns {{groups: Array<{label: string, options: Array<{value: string, label: string}>}>,
 *            value: string, absent: boolean}}
 */
export function planAudioInputs(devices, config) {
  const list = Array.isArray(devices) ? devices : [];
  const kind = normaliseAudioSourceKind(config && config.audioSourceKind);
  const savedId = str(
    kind === AUDIO_SOURCE_DECKLINK
      ? config && config.decklinkPersistentId
      : config && config.audioDeviceId,
  );

  // Display order is devices.js's — real microphones above the NDI/webcam
  // virtual sources, the Dante wall last, numbers compared as numbers. The
  // measured machine offers eight "DVS Receive N-M" pairs, and byte order files
  // them above the one Focusrite the commentator actually uses.
  const ordered = sortDevices(list);
  const native = ordered.filter((d) => d && d.kind !== DEVICE_KIND.DECKLINK);
  const decklink = ordered.filter((d) => d && d.kind === DEVICE_KIND.DECKLINK);

  // THE NAMES ARE BARE HERE, and that is not an oversight. devices.js's
  // labelDevices suffixes a DeckLink entry with "SDI/HDMI audio" and its native
  // twin with "computer sound input", because on the main screen the two land on
  // adjacent lines of one flat list under the same name. Here the OPTGROUP has
  // already said which is which, above them both, and repeating it on every line
  // is the noise that stops labels being read.
  const nativeOptions = native.map((d) => ({
    value: encodeAudioInput(AUDIO_SOURCE_NATIVE, str(d.id)),
    label: str(d.name),
  }));
  const decklinkOptions = decklink.map((d) => ({
    value: encodeAudioInput(AUDIO_SOURCE_DECKLINK, str(d.id)),
    label: str(d.name),
  }));

  // The platform's default input, which an empty audioDeviceId means and every
  // fresh configuration holds. First, because it is where a seat starts.
  nativeOptions.unshift({
    value: encodeAudioInput(AUDIO_SOURCE_NATIVE, ''),
    label: DEFAULT_INPUT_LABEL,
  });

  // "The card in this machine" — an empty decklinkPersistentId — only when it is
  // what is saved. See ANY_CARD_LABEL for why it is not offered otherwise.
  if (kind === AUDIO_SOURCE_DECKLINK && savedId === '') {
    decklinkOptions.unshift({
      value: encodeAudioInput(AUDIO_SOURCE_DECKLINK, ''),
      label: ANY_CARD_LABEL,
    });
  }

  const groups = [];
  if (nativeOptions.length > 0) groups.push({ label: NATIVE_GROUP_LABEL, options: nativeOptions });
  if (decklinkOptions.length > 0) {
    groups.push({ label: DECKLINK_GROUP_LABEL, options: decklinkOptions });
  }

  // Is the saved selection anywhere in what was just built? Asked of the group
  // it belongs to, not of the whole list, because the two id spaces are
  // different vocabularies and a coincidence across them would be the exact
  // wrong-subsystem match this module exists to prevent.
  const value = encodeAudioInput(kind, savedId);
  const inList = groups.some((g) => g.options.some((o) => o.value === value));
  if (inList) return { groups, value, absent: false };

  // Not present. describeDeviceSelection is asked for the WORDING only — it is
  // the one spelling of this marker in the application, and the main screen's
  // dropdown uses the same one.
  const marker = describeDeviceSelection([], savedId);
  groups.push({
    label: kind === AUDIO_SOURCE_DECKLINK ? DECKLINK_GROUP_LABEL : NATIVE_GROUP_LABEL,
    options: [{ value, label: marker.label }],
  });
  return { groups, value, absent: true };
}

function str(v) {
  return typeof v === 'string' ? v : '';
}
