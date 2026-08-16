//go:build dev || production || bindings

package main

import (
	"strings"
	"testing"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
)

// app_capturepair_test.go pins what ONE COMMENTARY-INPUT SELECTION may write,
// and what happens to every combination of the fields it writes.
//
// Owner: WP-3a, with the Settings screen.
//
// # Why this file exists
//
// The commentary input became ONE dropdown over BOTH kinds of device — the
// platform's audio endpoints and a DeckLink card's embedded audio, separated by
// optgroup — and picking from it sets a PAIR: audioSourceKind, and the id field
// belonging to that kind. Two fields written from one gesture is exactly the
// shape that produces an incoherent document, and the failure it produces if
// nothing checks is the one this whole area exists to prevent: an empty device
// on osxaudiosrc or wasapi2src is not an error, it is THE SYSTEM DEFAULT INPUT,
// so the match goes out from the laptop's built-in microphone with every lamp
// green.
//
// The rules were already enforced, in two places and for two different reasons —
// config.Validate for "is this document well formed", App.preflightCapture for
// "does THIS MACHINE have what it describes". What did not exist was a single
// statement of the MATRIX, which is what a frontend author needs and what stops
// the two halves drifting into disagreeing about a combination neither one owns
// alone.
//
// # THE TWO RESULTS THAT ARE NOT WHAT A READER EXPECTS
//
// Both are non-refusals, and both would be actively harmful to "tidy up".
//
//  1. AN EMPTY decklinkPersistentId ON A DECKLINK SEAT IS CORRECT, and means
//     "the card in this machine". A commentary position has one card; making the
//     operator find and type a number to name it would be a worse screen and a
//     new way to be wrong. It is resolved against the enumeration at Start, and
//     refused only when the machine has no card, or more than one and none
//     named.
//
//  2. A decklinkPersistentId ON A NATIVE SEAT IS CORRECT AND MUST SURVIVE. The
//     two settings are INDEPENDENT: audioSourceKind says where the COMMENTARY
//     comes from and videoSource says where the PICTURE comes from, and "the
//     operator's microphone on a USB interface, the camera down the SDI" is an
//     ordinary rig. The id field is shared between them because one persistent-id
//     names the CARD and serves its audio and video entries alike. So a picker
//     that CLEARS decklinkPersistentId when a native microphone is chosen would
//     silently change which card the VIDEO leg opens on a two-card machine —
//     a wrong-source failure with every lamp green, reached from a control the
//     operator believes is about audio.
func TestCommentaryInputPairIsCoherent(t *testing.T) {
	const (
		cardID  = "2747401380"
		otherID = "1234567890"
	)

	// The machine under test: one native input carrying validConfig's saved id,
	// and one card. That is a commentary position.
	devices := []gst.Device{
		nativeStubDevice(),
		deckLinkStubDevice(cardID, "UltraStudio 4K Mini (Audio Capture)"),
	}

	for _, tc := range []struct {
		name string
		edit func(*config.Config)

		// wantValidateNames is the substring config.Validate's refusal must
		// contain, or "" when the document is well formed. It is the FIELD NAME
		// on purpose: a refusal an operator cannot act on is a refusal that
		// sends them to the log instead of to the box.
		wantValidateNames string

		// wantPreflightNames is the same for App.preflightCapture, which is
		// asked only when Validate passed.
		wantPreflightNames string
	}{
		{
			name: "a native seat with a device chosen is the ordinary case",
			edit: func(c *config.Config) {},
		},
		{
			name: "a native seat with NO device is refused, naming the field",
			edit: func(c *config.Config) { c.AudioDeviceID = "" },
			// The one field Start genuinely cannot proceed without. Empty on
			// the element means the system default input, silently.
			wantValidateNames: "audioDeviceId",
		},
		{
			name: "a native seat that also names a card KEEPS the card and is accepted",
			edit: func(c *config.Config) { c.DeckLinkPersistentID = cardID },
			// Result 2 above. Nothing refuses this and nothing may start to.
		},
		{
			name: "a native microphone with a DeckLink camera is an ordinary rig",
			edit: func(c *config.Config) {
				c.VideoSource = config.VideoSourceDeckLink
				c.DeckLinkPersistentID = cardID
			},
		},
		{
			name: "a native microphone with a DeckLink camera and no card named picks the one card",
			edit: func(c *config.Config) { c.VideoSource = config.VideoSourceDeckLink },
		},
		{
			name: "a DeckLink commentary seat is refused by name while its capture leg is unbuilt",
			edit: func(c *config.Config) {
				c.AudioSourceKind = config.AudioSourceDeckLink
				c.AudioDeviceID = ""
			},
			// NOT "audioDeviceId is required": Validate stops requiring it for
			// this kind, correctly, because no CoreAudio or WASAPI endpoint is
			// ever opened. The refusal comes from the pre-flight and names the
			// field that actually decides it.
			wantPreflightNames: "audioSourceKind",
		},
		{
			name: "a DeckLink commentary seat naming its card is refused the same way",
			edit: func(c *config.Config) {
				c.AudioSourceKind = config.AudioSourceDeckLink
				c.AudioDeviceID = ""
				c.DeckLinkPersistentID = cardID
			},
			wantPreflightNames: "audioSourceKind",
		},
		{
			name: "a kind this build cannot build is refused at the document, naming the field",
			edit: func(c *config.Config) { c.AudioSourceKind = "ndi" },
			// There is no pipeline behind an unrecognised kind, so guessing
			// would build SOMETHING and it would be the wrong thing.
			wantValidateNames: "audioSourceKind",
		},
		{
			name: "a camera whose card is not in this machine is refused, naming the field",
			edit: func(c *config.Config) {
				c.VideoSource = config.VideoSourceDeckLink
				c.DeckLinkPersistentID = otherID
			},
			wantPreflightNames: "decklinkPersistentId",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApp(t)
			withStubDevices(t, devices)

			cfg := validConfig()
			tc.edit(cfg)

			err := cfg.Validate()
			if tc.wantValidateNames == "" {
				if err != nil {
					t.Fatalf("Validate() refused a well-formed document: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() accepted a document it must refuse; the failure would be "+
						"%s missing at the element, which is not an error there", tc.wantValidateNames)
				}
				if !strings.Contains(err.Error(), tc.wantValidateNames) {
					t.Fatalf("the refusal does not name %s, so the operator does not know which box "+
						"to fix: %v", tc.wantValidateNames, err)
				}
				// Validate having refused, the pre-flight is never reached.
				return
			}

			_, err = a.preflightCapture(cfg)
			if tc.wantPreflightNames == "" {
				if err != nil {
					t.Fatalf("preflightCapture() refused a startable seat: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflightCapture() accepted a seat it must refuse; the pipeline would " +
					"fail with not-negotiated (-4) naming nothing, twenty seconds after START")
			}
			if !strings.Contains(err.Error(), tc.wantPreflightNames) {
				t.Fatalf("the refusal does not name %s: %v", tc.wantPreflightNames, err)
			}
		})
	}
}

// TestDeckLinkPersistentIDSurvivesANativeSelection is result 2 above stated as
// its own test rather than as one row, because it is the one an editor is most
// likely to "fix" and the damage is invisible.
//
// A single dropdown that writes audioSourceKind AND an id has to decide what to
// do with the OTHER kind's id. The answer is: nothing. Clearing
// decklinkPersistentId when a native microphone is picked would, on a machine
// with two cards, move the VIDEO leg from the named card to whichever one
// enumerates first — from a control the operator believes is about their
// microphone, with no message anywhere.
func TestDeckLinkPersistentIDSurvivesANativeSelection(t *testing.T) {
	a, _ := newTestApp(t)
	silencePump(a)

	const cardID = "2747401380"

	// A seat set up with a native microphone and a DeckLink camera.
	cfg := validConfig()
	cfg.VideoSource = config.VideoSourceDeckLink
	cfg.DeckLinkPersistentID = cardID
	if err := a.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	// The operator now picks a different NATIVE microphone from the one
	// dropdown. The frontend restates the whole document, as it always does.
	next := a.snapshotConfig()
	next.AudioSourceKind = config.AudioSourceNative
	next.AudioDeviceID = "{0.0.1.00000000}.{c41a9d7e-0004-438e-9003-51a46e13a0c1}"
	if err := a.SaveConfig(next); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	got := a.snapshotConfig()
	if got.DeckLinkPersistentID != cardID {
		t.Errorf("decklinkPersistentId = %q after a native microphone was chosen, want %q; the "+
			"card id belongs to the VIDEO leg as well and clearing it moves the camera",
			got.DeckLinkPersistentID, cardID)
	}
	if !got.UsesDeckLinkVideo() {
		t.Errorf("videoSource = %q after a native microphone was chosen; the two settings are "+
			"independent", got.VideoSource)
	}
}
