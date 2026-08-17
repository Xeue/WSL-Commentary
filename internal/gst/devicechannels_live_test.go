//go:build live && cgo && !gststub

// devicechannels_live_test.go is step 3 of the always-live capture plan: the
// proof that Device.Channels is what the enumeration actually advertises.
//
// # Why the field exists, and what fails without it
//
// Device.Channels is what fills CaptureOpts.DeviceChannels, which is what SIZES
// THE MIX-MATRIX while the commentary capture is still in NULL. A matrix is a
// negotiation constraint and not a gain: no CoreAudio source emits a positioned
// channel-mask above two channels — gstosxcoreaudio.c:886-889 sets
// `layout = NULL; /* no supported for sources */` unconditionally for every
// source — so audioconvert cannot map a 16-in device to stereo without one and
// the leg dies about 0.07 s after PLAYING with not-negotiated (-4). With this
// field at 0 the only remaining source of a width is the pad, and osxaudiosrc's
// src template is `channels: [1, 2147483647]`, which fixes nothing.
//
// So a multichannel commentary seat cannot start at all until something fills
// this in. That is what this file checks is now true.
//
// # NOTHING IS OPENED HERE
//
// This is a query over the devices the provider has already enumerated, and it is
// the same walk ListInputDevices does for the dropdown at launch. No capture
// element is created, no device is opened, and the card is not touched: its
// decklink entry is READ and deliberately left shut.
//
// channelwidth_live_test.go (step 0c) is what established that the advertised
// count is the count that negotiates, by opening devices and comparing. This file
// does not repeat that; it checks that the shipped accessor reports the same
// number that walk reads, so the two cannot drift.
package gst

import (
	"testing"
)

// TestLiveDeviceChannelsIsWhatTheProviderAdvertises cross-checks the shipped
// ListInputDevices against an independent walk of the same population.
//
// The independent walk is channelwidth_live_test.go's enumerateAdvertisedWidths,
// which reads structure 0 of each enumerated GstDevice's caps and records the
// field exactly as GStreamer serialised it — including the shapes no typed getter
// can return, `{ 2, 8, 16 }` and `[ 1, 32 ]`, which are the shapes that must read
// as "advertised nothing fixed" rather than as a width.
func TestLiveDeviceChannelsIsWhatTheProviderAdvertises(t *testing.T) {
	liveInitDarwin(t)

	advertised := enumerateAdvertisedWidths(t)
	if len(advertised) == 0 {
		t.Fatal("no Audio/Source device was offered at all, so there is nothing to compare")
	}

	devices, err := ListInputDevices()
	if err != nil {
		t.Fatalf("ListInputDevices failed: %v", err)
	}

	byKey := make(map[string]Device, len(devices))
	for _, d := range devices {
		byKey[string(d.Kind)+":"+d.ID] = d
	}

	t.Log("---- Device.Channels against the caps it is read from, nothing opened ----")
	wide := 0
	for _, a := range advertised {
		key := string(NormaliseDeviceKind(a.kind)) + ":" + a.id
		d, ok := byKey[key]
		if !ok {
			// enumerateAdvertisedWidths applies the same offer test, so a device
			// present there and absent here is a real divergence and not a filter.
			t.Errorf("%q (%s) was offered by the enumeration walk and is missing from "+
				"ListInputDevices", a.name, key)
			continue
		}

		want := 0
		if a.fixed {
			want = a.channels
		}
		if d.Channels != want {
			t.Errorf("%q (%s): Device.Channels = %d, want %d. The caps say channels=%s (mask %s), "+
				"and structure 0 is the only one that negotiates",
				a.name, key, d.Channels, want, a.channelsAs, a.mask)
		}
		if d.Channels > ChannelMapOutputs {
			wide++
		}
		t.Logf("  %-34q kind=%-8s Device.Channels=%-3d caps channels=%-16s mask=%s",
			a.name, d.Kind, d.Channels, a.channelsAs, a.mask)
	}

	// The DeckLink entry must NOT advertise a width. Its provider offers
	// `{ 2, 8, 16 }`, which fixes nothing; a card commentary's width is the
	// constant 16 by construction, stated on the element by the description, and a
	// number arriving from the enumeration here would be a second account of the
	// same fact that could disagree with it.
	for _, d := range devices {
		if d.Kind == KindDeckLink && d.Channels != 0 {
			t.Errorf("the DeckLink entry %q reports Device.Channels = %d. The card's width is "+
				"stated on decklinkaudiosrc by the description (channels=%d) and must not also "+
				"arrive from the provider", d.Name, d.Channels, deckLinkAudioChannels)
		}
	}

	if wide == 0 {
		t.Logf("NOTE: no device on this machine advertises more than %d channels, so this run "+
			"cannot show the multichannel case. It is not a failure — it is a fact about what is "+
			"plugged in — but R2's acceptance test needs a wide device present", ChannelMapOutputs)
	}
}
