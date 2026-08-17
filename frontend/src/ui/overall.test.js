/**
 * Tests for the ONE overall indicator's reduction rule.
 *
 * Run with:  node --test "src/ui/*.test.js"
 *
 * Owner: WP-5b.
 *
 * ======================= THE FAILURE THESE PREVENT ==========================
 *
 * The operator asked for "an overall status, single green/red indicator". A
 * summary of six lamps is trivial to write and easy to write WRONG, and the two
 * ways of being wrong are opposite and both fatal to the control:
 *
 *  1. It reads GOOD over something that is not known. Spec section 8's rule is
 *     "never show stale green": a status feed this application cannot see is not
 *     a healthy one, and a green light over it is the single most dangerous
 *     false statement this screen can make.
 *
 *  2. It NEVER reads GOOD, because a plain worst-of over four levels treats grey
 *     as bad — and CAMERA reads grey SLATE on every position shipping today, on
 *     purpose, correctly. An indicator that is amber on a perfectly healthy seat
 *     for ever is an indicator nobody looks at, which costs the same as not
 *     having one.
 *
 * Both are here, and so is the rule that separates them: a grey with a SETTLED
 * answer does not stop GOOD; a grey that means "not known" does.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  deriveOverallStatus,
  describeOverall,
  OVERALL,
  OVERALL_WORDS,
  OVERALL_WORDS_BEFORE_START,
  SETTLED_GREY_TEXTS,
  rankOf,
} from './overall.js';
import { LEVEL } from './lamps.js';

const lamp = (level, text) => ({ level, text });

// The lamp NAMES are home.js's, in row order. The last two say SWITCHER because
// they are the switcher's report of what it is receiving and this desk now
// measures its own audio and video before START — see home.js's LAMP_NAMES.
// They are fixture data here (this reduction is a pure function of whatever it
// is handed), but a fixture that does not match the row is a fixture whose
// `detail` assertions prove nothing about what an operator would read.

/** A healthy running seat on a SLATE position — every seat shipping today. */
function healthySlateSeat() {
  return [
    { name: 'SENDING', lamp: lamp(LEVEL.GREEN, 'SENDING') },
    { name: 'CAMERA', lamp: lamp(LEVEL.GREY, 'SLATE') },
    { name: 'SWITCHER SEES FEED', lamp: lamp(LEVEL.GREEN, 'STREAMING') },
    { name: 'SWITCHER VIDEO', lamp: lamp(LEVEL.GREEN, '1080P50') },
    { name: 'SWITCHER AUDIO', lamp: lamp(LEVEL.GREEN, 'AAC 48K STEREO') },
    { name: 'MONITOR', lamp: lamp(LEVEL.GREEN, 'CONNECTED') },
  ];
}

/** A seat that has not been started: every lamp grey. */
function standbySeat() {
  return [
    { name: 'SENDING', lamp: lamp(LEVEL.GREY, 'NOT STARTED') },
    { name: 'CAMERA', lamp: lamp(LEVEL.GREY, 'SLATE') },
    { name: 'SWITCHER SEES FEED', lamp: lamp(LEVEL.GREY, 'NO STATUS') },
    { name: 'SWITCHER VIDEO', lamp: lamp(LEVEL.GREY, 'NO STATUS') },
    { name: 'SWITCHER AUDIO', lamp: lamp(LEVEL.GREY, 'NO STATUS') },
    { name: 'MONITOR', lamp: lamp(LEVEL.GREY, 'NOT STARTED') },
  ];
}

/**
 * A seat before START with capture already live and a fault in it: the card is
 * selected and its input is unplugged.
 *
 * THIS SEAT DID NOT EXIST BEFORE. Nothing was captured, measured or watched
 * until START, so the pre-air row was all grey and there was no red to word.
 * The card and the microphone are open from launch now, so this is the state the
 * pre-air vocabulary is for — and it is the one that must read as an
 * instruction, because it is the last moment at which anything can be fixed
 * cheaply.
 */
function preAirCardFaultSeat() {
  const seat = standbySeat();
  seat.find((e) => e.name === 'CAMERA').lamp = lamp(LEVEL.RED, 'NO SIGNAL');
  return seat;
}

// ---------------------------------------------------------------------------
// The two failures the rule exists for
// ---------------------------------------------------------------------------

test('a healthy SLATE seat reads GOOD — grey CAMERA does not spoil it', () => {
  // THE SECOND FAILURE, and the reason "worst of them" is not the whole answer.
  // Every position shipping today sends a slate; CAMERA is grey and that is the
  // configured, correct, healthy state of the machine.
  const o = deriveOverallStatus(healthySlateSeat(), { running: true });
  assert.equal(o.state, OVERALL.GOOD);
  assert.equal(o.level, LEVEL.GREEN);
  assert.equal(o.text, 'GOOD');
  assert.equal(o.detail, '', 'nothing to name when everything is green');
});

test('a stale status feed reads WORKING, never GOOD', () => {
  // THE FIRST FAILURE. deriveStatusLamps greys all three WebSocket lamps with
  // "STATUS UNAVAILABLE" when the telemetry socket has been quiet; those greys
  // are NOT settled answers, they are the application unable to see.
  const seat = healthySlateSeat();
  for (const name of ['SWITCHER SEES FEED', 'SWITCHER VIDEO', 'SWITCHER AUDIO']) {
    seat.find((e) => e.name === name).lamp = lamp(LEVEL.GREY, 'STATUS UNAVAILABLE');
  }
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.WORKING, 'spec section 8: never show stale green');
  assert.equal(o.detail, 'SWITCHER SEES FEED: STATUS UNAVAILABLE', 'and it names what it cannot see');
});

test('SLATE is the only settled grey, and a CAMERA that is merely unmeasured is not', () => {
  assert.deepEqual([...SETTLED_GREY_TEXTS], ['SLATE'], 'adding to this list adds a state that reads GOOD');

  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'CAMERA').lamp = lamp(LEVEL.GREY, 'NOT MEASURED');
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(
    o.state,
    OVERALL.WORKING,
    'a DeckLink seat whose signal event has not arrived is genuinely not known — the whitelist is ' +
      'of TEXTS, not of lamp names, precisely so this case is not waved through by a rule written ' +
      'about its neighbour',
  );
  assert.equal(o.detail, 'CAMERA: NOT MEASURED');
});

// ---------------------------------------------------------------------------
// The ordinary reductions
// ---------------------------------------------------------------------------

test('one red lamp makes it a FAULT, whatever else is green', () => {
  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'SWITCHER AUDIO').lamp = lamp(LEVEL.RED, 'NO AUDIO (DROPPED?)');
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.FAULT);
  assert.equal(o.level, LEVEL.RED);
  assert.equal(o.detail, 'SWITCHER AUDIO: NO AUDIO (DROPPED?)', 'a summary that cannot say which lamp is a verdict');
});

test('red beats amber, and the row order decides which of two equals is named', () => {
  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'SENDING').lamp = lamp(LEVEL.AMBER, 'RETRYING');
  seat.find((e) => e.name === 'SWITCHER VIDEO').lamp = lamp(LEVEL.RED, 'WRONG FORMAT');
  seat.find((e) => e.name === 'SWITCHER AUDIO').lamp = lamp(LEVEL.RED, 'NO AUDIO (DROPPED?)');

  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.FAULT, 'red outranks amber');
  assert.equal(
    o.detail,
    'SWITCHER VIDEO: WRONG FORMAT',
    'the LEFTMOST of two equally bad lamps, which is the one the eye reaches first on the row ' +
      'this summarises',
  );
});

test('an amber lamp with nothing red reads WORKING', () => {
  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'SENDING').lamp = lamp(LEVEL.AMBER, 'CONNECTING');
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.WORKING);
  assert.equal(o.detail, 'SENDING: CONNECTING');
});

test('a seat that has not started reads STANDBY, not FAULT and not GOOD', () => {
  const o = deriveOverallStatus(standbySeat(), { running: false });
  assert.equal(o.state, OVERALL.STANDBY);
  assert.equal(o.level, LEVEL.GREY);
  assert.equal(
    o.detail,
    '',
    'nothing is wrong and nothing is on air; naming a lamp would invent a complaint',
  );
});

test('STANDBY is not reached by a running session, however many greys it has', () => {
  // The running flag is the difference between "not started" and "not known",
  // and it comes from the same state that flips the START/STOP button so the two
  // cannot drift apart.
  const seat = standbySeat();
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.WORKING, 'a session that IS up with unknown lamps is not standby');
});

test('an omitted session argument is read as not running', () => {
  assert.equal(deriveOverallStatus(standbySeat()).state, OVERALL.STANDBY);
  assert.equal(deriveOverallStatus(standbySeat(), null).state, OVERALL.STANDBY);
});

// ---------------------------------------------------------------------------
// Robustness: this runs on every lamp update, ~20 a second in the worst case
// ---------------------------------------------------------------------------

test('no lamps at all is STANDBY, not GOOD', () => {
  // The earliest moment of startup, before anything has been derived. An empty
  // list vacuously satisfies "every lamp is green", which is exactly the kind of
  // true-by-emptiness that must not paint a green light.
  assert.equal(deriveOverallStatus([]).state, OVERALL.STANDBY);
  assert.equal(deriveOverallStatus(null).state, OVERALL.STANDBY);
  assert.equal(deriveOverallStatus(undefined).state, OVERALL.STANDBY);
});

test('malformed entries are skipped rather than thrown on', () => {
  const seat = [...healthySlateSeat(), null, {}, { name: 'X' }];
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.GOOD, 'an entry with no lamp is not a lamp');
});

test('a lamp with no level is treated as grey, not as green', () => {
  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'MONITOR').lamp = { text: 'SOMETHING' };
  const o = deriveOverallStatus(seat, { running: true });
  assert.equal(o.state, OVERALL.WORKING, 'the safe direction for a value nobody set');
});

// ---------------------------------------------------------------------------
// What it says
// ---------------------------------------------------------------------------

test('the four words differ in length and share no prefix — in BOTH vocabularies', () => {
  // At the distance this is read from, word SHAPE arrives before the letters do.
  // There are two sets of four now, one for a seat that is on air and one for a
  // seat that is not, and the property has to hold inside each of them: an
  // operator reads one set at a time and never a mixture.
  for (const [which, table] of [
    ['on air', OVERALL_WORDS],
    ['before START', OVERALL_WORDS_BEFORE_START],
  ]) {
    const words = Object.values(table);
    assert.equal(new Set(words).size, words.length, `${which}: no two states may say the same word`);
    assert.equal(
      new Set(words.map((w) => w.length)).size,
      words.length,
      `${which}: and no two may be the same length, so the shape alone separates them`,
    );
    for (const a of words) {
      for (const b of words) {
        if (a === b) continue;
        assert.equal(
          a.startsWith(b),
          false,
          `${which}: "${a}" starts with "${b}": the first glance is ambiguous`,
        );
      }
    }
  }

  // AND ACROSS THE TWO, no word may mean two different things. A word that said
  // one state on air and another off it would be the worst of both: the operator
  // learns it once and it is wrong half the time.
  const meanings = new Map();
  for (const table of [OVERALL_WORDS, OVERALL_WORDS_BEFORE_START]) {
    for (const [state, word] of Object.entries(table)) {
      const seen = meanings.get(word);
      assert.ok(
        seen === undefined || seen === state,
        `"${word}" means ${seen} on one side of START and ${state} on the other`,
      );
      meanings.set(word, state);
    }
  }
});

// ---------------------------------------------------------------------------
// The pre-air vocabulary
// ---------------------------------------------------------------------------

test('before START, red says NOT READY and amber says CHECK', () => {
  // ================== WHY TWO WORDS CHANGE AND NOTHING ELSE =================
  //
  // Same lamps, same reduction, same colours and the same glyph — only the word
  // differs, and only in the two states whose ON-AIR word misreads off air:
  //
  //   FAULT reads as "something has broken", and an operator who has started
  //   nothing reads that as broken in the APPLICATION. NOT READY is the same red
  //   saying the thing they can act on.
  //
  //   WORKING is the dangerous one: it means "this application cannot presently
  //   see all of it", and beside a START button it is read as "it is working".
  //   CHECK cannot be read as reassurance.
  const red = deriveOverallStatus(preAirCardFaultSeat(), { running: false });
  assert.equal(red.state, OVERALL.FAULT, 'the STATE is unchanged — this is wording, not logic');
  assert.equal(red.level, LEVEL.RED, 'and so is the colour, and so is the glyph that goes with it');
  assert.equal(red.text, 'NOT READY');
  assert.equal(red.detail, 'CAMERA: NO SIGNAL', 'and it still names the lamp it came from');

  const amberSeat = standbySeat();
  amberSeat.find((e) => e.name === 'MONITOR').lamp = lamp(LEVEL.AMBER, 'RECONNECTING');
  const amber = deriveOverallStatus(amberSeat, { running: false });
  assert.equal(amber.state, OVERALL.WORKING);
  assert.equal(amber.level, LEVEL.AMBER);
  assert.equal(amber.text, 'CHECK');

  // ON AIR, THE SAME TWO SEATS SAY WHAT THEY ALWAYS SAID. This is the half that
  // proves the change is a vocabulary and not a new rule.
  assert.equal(deriveOverallStatus(preAirCardFaultSeat(), { running: true }).text, 'FAULT');
  assert.equal(deriveOverallStatus(amberSeat, { running: true }).text, 'WORKING');
});

test('a seat that is merely idle is STANDING BY, and never GOOD, in either vocabulary', () => {
  // The pre-air words must not reach a seat that has nothing wrong with it. The
  // idle seat's greys take case 1 before any wording is chosen, and GOOD is
  // unreachable off air because SENDING is grey NOT STARTED and NOT STARTED is
  // not on the settled list. That is what stops an indicator that has learnt to
  // speak about pre-air faults from also learning to bless an idle seat.
  const idle = deriveOverallStatus(standbySeat(), { running: false });
  assert.equal(idle.state, OVERALL.STANDBY);
  assert.equal(idle.text, 'STANDING BY', 'the one word that is the same on both sides of START');

  // Even with every OTHER lamp green: SENDING grey off air is the whole guard.
  const nearly = healthySlateSeat();
  nearly.find((e) => e.name === 'SENDING').lamp = lamp(LEVEL.GREY, 'NOT STARTED');
  const o = deriveOverallStatus(nearly, { running: false });
  assert.equal(o.state, OVERALL.STANDBY, 'a healthy seat that is not sending is not GOOD');
  assert.notEqual(o.state, OVERALL.GOOD);
});

test('describeOverall always says where the detail lives', () => {
  const good = describeOverall(deriveOverallStatus(healthySlateSeat(), { running: true }));
  assert.match(good, /^GOOD\./);
  assert.match(good, /column beside the picture/, 'the six lamps must not look deleted');

  const seat = healthySlateSeat();
  seat.find((e) => e.name === 'SWITCHER AUDIO').lamp = lamp(LEVEL.RED, 'NO AUDIO (DROPPED?)');
  const bad = describeOverall(deriveOverallStatus(seat, { running: true }));
  assert.match(bad, /^FAULT — SWITCHER AUDIO: NO AUDIO \(DROPPED\?\)\./);

  // And off air the same seat opens with the pre-air word, because describeOverall
  // reads the text the reduction already chose rather than choosing one of its
  // own. Two places that picked a word would be two places that can disagree
  // about what the indicator says, on the control and in its own tooltip.
  const preAir = describeOverall(deriveOverallStatus(seat, { running: false }));
  assert.match(preAir, /^NOT READY — SWITCHER AUDIO: NO AUDIO \(DROPPED\?\)\./);
});

test('describeOverall survives being handed nothing', () => {
  assert.match(describeOverall(null), /^STANDING BY\./);
  assert.match(describeOverall(undefined), /^STANDING BY\./);
});

test('rankOf orders red above amber above green, with grey outside the order', () => {
  assert.ok(rankOf(LEVEL.RED) > rankOf(LEVEL.AMBER));
  assert.ok(rankOf(LEVEL.AMBER) > rankOf(LEVEL.GREEN));
  assert.equal(
    rankOf(LEVEL.GREY),
    0,
    'grey ranks BELOW green, which is why the reduction cannot be a plain maximum: taking one ' +
      'would make a grey lamp the best news on the row',
  );
  assert.equal(rankOf('nonsense'), 0);
});

test('an unrecognised lamp level is not a good one', () => {
  // THE FAILURE: the level was taken as `e.lamp.level || LEVEL.GREY`, which
  // catches empty and undefined and lets everything else through verbatim. A
  // lamp carrying 'RED' in the wrong case, or a level from a newer backend,
  // matched none of the red/amber/grey filters, fell out of the greys count and
  // reached `greys.length === 0 -> GOOD`. One lamp screaming, summary green.
  //
  // This indicator is the single control a commentator glances at, so its one
  // unbreakable property is that it fails towards concern, never away from it.
  for (const bogus of ['RED', 'Red', 'critical', 'orange', 'ok', 0, 1, {}, []]) {
    const out = deriveOverallStatus(
      [{ name: 'SWITCHER AUDIO', lamp: { level: bogus, text: 'WHO KNOWS' } }],
      { running: true },
    );
    assert.notEqual(
      out.state,
      OVERALL.GOOD,
      `level ${JSON.stringify(bogus)} is not understood, so it may not read GOOD`,
    );
    assert.equal(out.state, OVERALL.WORKING, 'not understood, mid-session, is exactly WORKING');
  }

  // Not running, and the same unknown level: STANDBY, for the same reason every
  // other grey gives STANDBY before START. Still not GOOD.
  const idle = deriveOverallStatus([{ name: 'SWITCHER AUDIO', lamp: { level: 'RED', text: 'X' } }], null);
  assert.equal(idle.state, OVERALL.STANDBY);

  // One bogus level does not drag down a real red beside it: the red still wins,
  // and still names itself.
  const mixed = deriveOverallStatus(
    [
      { name: 'SWITCHER AUDIO', lamp: { level: 'nonsense', text: 'X' } },
      { name: 'SWITCHER VIDEO', lamp: { level: LEVEL.RED, text: 'NO SIGNAL' } },
    ],
    { running: true },
  );
  assert.equal(mixed.state, OVERALL.FAULT);
  assert.equal(mixed.detail, 'SWITCHER VIDEO: NO SIGNAL');
});
