// Level-meter arithmetic shared by both build twins.
//
// Owner: WP-3a, with the rest of this package.
//
// This file is deliberately UNTAGGED. The rule in this package is that
// everything decidable without GStreamer is lifted into pure Go, so that Gate A
// runs it under -race and Gate B links the identical code: the cgo twin's bus
// handler and the stub twin's synthetic ticker both produce gst.Levels through
// the functions below, which is what stops the two builds disagreeing about
// what "-inf" or a NaN turns into on a meter. The cgo-only part — reading a
// GValueArray out of a level element message — stays in gst_cgo.go, but it
// hands the raw []any straight to levelListToDB here.

package gst

import "math"

// levelSilenceDB is the clamped floor of every delivered level, in dBFS.
//
// The level element reports -inf for digital silence, and -inf is unusable
// downstream twice over: encoding/json refuses it (the "levels" event would
// silently stop arriving the moment the commentator went quiet, which reads as
// a frozen meter), and a meter has a floor anyway. -100 matches the mixer's own
// digital-silence reading (frontend/src/ui/mixer/model.js measured -100.0 on an
// idle strip), so both meters in this application say silence with the same
// number.
//
// The clamp does real work on macOS and not only in the -inf case. Measured on
// the port machine with the actual pipeline — osxaudiosrc ! audioconvert !
// audioresample ! S16LE/48k/2ch ! level — the built-in microphone is a MONO
// CoreAudio device, audioconvert widens it to the stereo the encoder is pinned
// to, and the level element then reports the second channel as
// -699.99999984363217 dBFS rather than -inf. That is not silence-as-infinity,
// it is a very large finite negative number, and it would have gone through
// encoding/json intact and rendered as a bar somewhere below the bottom of the
// scale. The "below the floor clamps up" branch is what makes it read as
// silence, which is what it is. A commentator on a mono microphone therefore
// sees one live bar and one at the floor, correctly.
const levelSilenceDB = -100

// levelStubChannels is how many channels the stub twin synthesises and how many
// the zero-frame emitted at session end carries: the specification's pipeline
// pins channels=2 upstream of the encoder, so a level report from the real
// build is stereo and the fakes must match it.
const levelStubChannels = 2

// clampLevelDB makes one dBFS reading deliverable: -inf, NaN and anything below
// the floor become levelSilenceDB, +inf becomes 0 (a meter's ceiling; the level
// element cannot legitimately report above digital full scale, so a +inf here
// is a bug upstream and full-scale red is the honest rendering of it).
func clampLevelDB(db float64) float64 {
	if math.IsNaN(db) {
		return levelSilenceDB
	}
	if db < levelSilenceDB {
		return levelSilenceDB
	}
	if math.IsInf(db, 1) {
		return 0
	}
	return db
}

// levelListToDB converts one field of a level element message — a GValueArray
// of G_TYPE_DOUBLE, one entry per channel, delivered by go-glib as []any — into
// a clamped []float64. Elements that are not doubles are reported as silence
// rather than skipped, so the channel count is preserved: a meter whose left
// bar silently became its right bar would be worse than one showing a silent
// channel.
//
// A nil or empty list returns nil, which the callers treat as "no usable
// reading in this message".
func levelListToDB(vals []any) []float64 {
	if len(vals) == 0 {
		return nil
	}
	out := make([]float64, len(vals))
	for i, v := range vals {
		switch n := v.(type) {
		case float64:
			out[i] = clampLevelDB(n)
		case float32:
			out[i] = clampLevelDB(float64(n))
		default:
			out[i] = levelSilenceDB
		}
	}
	return out
}

// silentLevels returns the all-silence frame: every channel at the floor.
//
// It is what the application emits once when a session ends, so the meter falls
// to nothing instead of freezing at the last level the pipeline happened to
// report — a frozen meter reads as a live one, which is the status-display
// direction this project never allows to be wrong.
func silentLevels() Levels {
	peak := make([]float64, levelStubChannels)
	rms := make([]float64, levelStubChannels)
	for i := range peak {
		peak[i] = levelSilenceDB
		rms[i] = levelSilenceDB
	}
	return Levels{PeakDB: peak, RMSDB: rms}
}

// Stub-twin synthetic waveform. These constants and stubLevelAt are used only
// by gst_stub.go's ticker, but they live here untagged so the shape of the
// fake signal is a testable pure function rather than arithmetic buried in a
// goroutine.
const (
	// stubLevelLowDB..stubLevelHighDB is the triangle wave's range: loud enough
	// to cross the mixer convention's -18 amber and -6-adjacent red boundaries
	// during development, quiet enough to spend most of its time green.
	stubLevelLowDB  = -40
	stubLevelHighDB = -6

	// stubLevelPeriod is the triangle's period in ticker steps. At the 50 ms
	// tick that is a six-second sweep up and down — slow enough to watch, fast
	// enough that a stuck meter is obvious within seconds.
	stubLevelPeriod = 120

	// stubLevelRightOffset staggers the right channel by a quarter period, so
	// the two bars visibly move independently and a UI that accidentally draws
	// channel 0 twice fails by eye at Gate A.
	stubLevelRightOffset = stubLevelPeriod / 4

	// stubLevelRMSBelowPeakDB is how far the synthetic RMS sits under the
	// synthetic peak. Real programme keeps RMS below peak; 8 dB is a plausible
	// crest factor for speech and keeps the two bars visually distinct.
	stubLevelRMSBelowPeakDB = 8
)

// stubLevelAt returns the synthetic peak level, in dBFS, for one channel at one
// ticker step: a triangle wave from stubLevelLowDB up to stubLevelHighDB and
// back, with the right channel (and any further channels) offset so they do not
// move in lockstep. Deterministic in step, so a test can assert exact values.
func stubLevelAt(step, channel int) float64 {
	if step < 0 {
		step = 0
	}
	if channel < 0 {
		channel = 0
	}
	phase := (step + channel*stubLevelRightOffset) % stubLevelPeriod
	half := stubLevelPeriod / 2
	span := float64(stubLevelHighDB - stubLevelLowDB)
	if phase < half {
		return stubLevelLowDB + span*float64(phase)/float64(half)
	}
	return stubLevelHighDB - span*float64(phase-half)/float64(half)
}

// stubLevelsAt builds the whole synthetic Levels frame for one ticker step:
// peaks from stubLevelAt, RMS a fixed distance below, everything clamped
// through the same clampLevelDB the real build uses.
func stubLevelsAt(step int) Levels {
	peak := make([]float64, levelStubChannels)
	rms := make([]float64, levelStubChannels)
	for ch := range peak {
		p := stubLevelAt(step, ch)
		peak[ch] = clampLevelDB(p)
		rms[ch] = clampLevelDB(p - stubLevelRMSBelowPeakDB)
	}
	return Levels{PeakDB: peak, RMSDB: rms}
}
