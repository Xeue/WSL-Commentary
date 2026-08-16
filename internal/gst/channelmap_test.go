// Gate A tests for the routing model in channelmap.go.
//
// Owner: WP-3a. Untagged, like the file they test, so the arithmetic that
// decides which of a DeckLink card's sixteen channels reach the commentary feed
// is exercised on every build and under -race, rather than only on the one
// machine with a card in it.
//
// Every number asserted below was checked against the real hardware on
// 2026-08-16 — an UltraStudio 4K Mini with its own output looped back to its
// input, carrying a 1 kHz sine at -9 dBFS on channels 1 and 2 — and the
// measured readings are quoted beside the assertions they justify. What these
// tests cover is the MODEL: the shape of the matrix, the width rule, the clamp
// and the default. What only the card can prove — that a matrix of the wrong
// width stops the capture chain, that a rejected coefficient leaves the
// previous matrix in force — is recorded in the comments of the code they
// belong to, because no Gate A test can observe an element that is not there.

package gst

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestMixMatrixIsOutputRowsByInputColumns is the orientation guard, and it is
// the most important test in this file.
//
// MIX-MATRIX IS OUTPUT ROWS x INPUT COLUMNS: two rows of sixteen for the card's
// 16-in/2-out case. The reverse silently fails — verified both ways on real
// hardware — and "silently" is the word that matters: a transposed matrix is
// not malformed, it is a matrix of the wrong shape, and the wrong shape stops
// the capture chain with a flow error naming the capture source rather than the
// matrix.
//
// It asserts the shape AND an asymmetric map, because a 2-in/2-out matrix is
// square and a transpose is invisible in the dimensions alone. Both halves have
// to be here for this test to be able to fail for the reason it exists.
func TestMixMatrixIsOutputRowsByInputColumns(t *testing.T) {
	m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: 1}, {Output: OutputRight, Input: 1, Gain: 1}}

	matrix, err := m.MixMatrix(16)
	if err != nil {
		t.Fatalf("MixMatrix(16): %v", err)
	}
	if len(matrix) != ChannelMapOutputs {
		t.Fatalf("matrix has %d rows, want %d: rows are OUTPUTS, and a matrix with one row per "+
			"INPUT is the transposition that stops the capture chain", len(matrix), ChannelMapOutputs)
	}
	for out, row := range matrix {
		if len(row) != 16 {
			t.Fatalf("row %d has %d columns, want 16: columns are INPUTS", out, len(row))
		}
	}

	// The asymmetric half. Input 1 reaches output 0 and nothing else, so
	// matrix[0][1] is 1 and matrix[1][0] is 0. A transposed build has them the
	// other way round, and no dimension check anywhere could tell.
	asym := ChannelMap{{Output: OutputLeft, Input: 1, Gain: 1}}
	sq, err := asym.MixMatrix(ChannelMapOutputs)
	if err != nil {
		t.Fatalf("MixMatrix(2): %v", err)
	}
	if sq[OutputLeft][1] != 1 {
		t.Errorf("matrix[left][input 1] = %v, want 1: matrix[out][in] is the indexing order",
			sq[OutputLeft][1])
	}
	if sq[1][OutputLeft] != 0 {
		t.Errorf("matrix[1][0] = %v, want 0. The map routes input 1 to the LEFT output; a matrix "+
			"with it at [1][0] is transposed, which on a square matrix is invisible except here",
			sq[1][OutputLeft])
	}
}

// TestMixMatrixRefusesAChannelTheStreamDoesNotHave is the width rule.
//
// The count passed in must be what the pad NEGOTIATED, and the only defence
// against a map written for a wider device is that a map naming a channel
// outside the count is refused rather than silently truncated or padded.
//
// A wrong width is not a bad value to GStreamer: measured live on the card with
// a good 2x16 matrix in force, writing a 2x8 matrix was ACCEPTED by the
// property — every coefficient was inside the clamp — and the capture chain
// died before the next level message, "streaming stopped, reason error (-5)".
// Nothing downstream of this refusal will catch it.
func TestMixMatrixRefusesAChannelTheStreamDoesNotHave(t *testing.T) {
	// Channel 12 (index 11) is real on a 16-channel card and is not there on
	// the eight-channel stream this map is being sized against.
	m := ChannelMap{{Output: OutputLeft, Input: 11, Gain: 1}}

	if _, err := m.MixMatrix(16); err != nil {
		t.Fatalf("input 11 on a 16-channel stream should be fine: %v", err)
	}
	_, err := m.MixMatrix(8)
	if err == nil {
		t.Fatal("a map naming input 11 was accepted against an 8-channel stream; the matrix would " +
			"have been eight columns wide with the operator's channel silently absent")
	}
	if !errors.Is(err, ErrChannelMap) {
		t.Errorf("error does not wrap ErrChannelMap, so the App layer cannot tell it from a "+
			"capture fault: %v", err)
	}
	if !strings.Contains(err.Error(), "11") || !strings.Contains(err.Error(), "8") {
		t.Errorf("the refusal names neither the channel nor the width: %v", err)
	}
}

// TestMixMatrixRefusesAWidthThisApplicationCannotSize covers the other end of
// the width rule: a count that is not a channel count at all. A zero is what a
// pad that has not negotiated reports, and building a matrix against it would
// produce two empty rows — a valid-looking matrix that routes nothing.
func TestMixMatrixRefusesAWidthThisApplicationCannotSize(t *testing.T) {
	for _, width := range []int{-1, 0, MaxInputChannels + 1, 64} {
		if _, err := ChannelMap(nil).MixMatrix(width); err == nil {
			t.Errorf("MixMatrix(%d) was accepted; a matrix sized against a width nobody negotiated "+
				"is the failure mode this argument exists to prevent", width)
		} else if !errors.Is(err, ErrChannelMap) {
			t.Errorf("MixMatrix(%d) error does not wrap ErrChannelMap: %v", width, err)
		}
	}
	for _, width := range []int{1, 2, 8, MaxInputChannels} {
		if _, err := ChannelMap(nil).MixMatrix(width); err != nil {
			t.Errorf("MixMatrix(%d) was refused and is a width decklinkaudiosrc or a microphone "+
				"really produces: %v", width, err)
		}
	}
}

// TestMixMatrixClampsCoefficientsBeforeWriting is the clamp, and the boundary
// is the whole test: 1.0 is accepted and 1.0000001 is not.
//
// Those two numbers are GStreamer's, not ours. Measured on the card with a live
// pipeline: a matrix of 1.0000001 produced a GLib-GObject-CRITICAL on stderr —
// where no shipped build is looking — and LEFT THE PREVIOUS MATRIX IN FORCE.
// The meter did not move: -15.019498150506179 dBFS before the write and
// -15.019498150506179 after it. A rejected write and a successful one are
// indistinguishable at the property level, so every value has to be refused
// here, before anything is sent.
func TestMixMatrixClampsCoefficientsBeforeWriting(t *testing.T) {
	accepted := []float64{1.0, -1.0, 0, 0.5, -0.5, 0.999999}
	refused := []float64{
		1.0000001, -1.0000001, 2, -2,
		math.NaN(), math.Inf(1), math.Inf(-1),
	}

	for _, g := range accepted {
		m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: g}}
		matrix, err := m.MixMatrix(16)
		if err != nil {
			t.Errorf("gain %v was refused and audioconvert accepts it: %v", g, err)
			continue
		}
		if matrix[OutputLeft][0] != g {
			t.Errorf("gain %v reached the matrix as %v", g, matrix[OutputLeft][0])
		}
	}
	for _, g := range refused {
		m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: g}}
		if _, err := m.MixMatrix(16); err == nil {
			t.Errorf("gain %v was accepted. audioconvert refuses it SILENTLY and keeps running the "+
				"previous matrix, so nothing downstream of this check would ever notice", g)
		}
	}
}

// TestGainOfAHalfHalvesTheAmplitude pins the arithmetic the operator relies on:
// the coefficient is LINEAR amplitude, so 0.5 is half the amplitude and
// -6.02 dB, not half the power and not a percentage.
//
// PROVEN ON THE CARD on 2026-08-16 against a known test signal — the card's own
// output looped back to its input carrying a 1 kHz sine at -9 dBFS on channels
// 1 and 2, metered by the same level element the application uses, immediately
// downstream of the matrix:
//
//	matrix 1.0   peak -8.999645249881333 dBFS  (both channels)
//	matrix 0.5   peak -15.019498150506179 dBFS (both channels)
//	delta        -6.0199 dB, against the -6.0206 dB the arithmetic gives
//
// The 0.7 dB-thousandths of disagreement is 16-bit quantisation of the tone,
// not the matrix.
func TestGainOfAHalfHalvesTheAmplitude(t *testing.T) {
	const measuredUnityDB = -8.999645249881333
	const measuredHalfDB = -15.019498150506179

	m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: 0.5}, {Output: OutputRight, Input: 1, Gain: 0.5}}
	matrix, err := m.MixMatrix(16)
	if err != nil {
		t.Fatalf("MixMatrix: %v", err)
	}
	if matrix[OutputLeft][0] != 0.5 || matrix[OutputRight][1] != 0.5 {
		t.Fatalf("the coefficients did not reach the matrix: %v %v",
			matrix[OutputLeft][0], matrix[OutputRight][1])
	}

	// The model's coefficient, turned into decibels the way the meter reports
	// them, must land on what the card actually measured.
	predicted := measuredUnityDB + 20*math.Log10(matrix[OutputLeft][0])
	if math.Abs(predicted-measuredHalfDB) > 0.01 {
		t.Errorf("a coefficient of %v applied to the measured unity reading %v dBFS predicts "+
			"%v dBFS; the card measured %v dBFS. The coefficient is not linear amplitude any more",
			matrix[OutputLeft][0], measuredUnityDB, predicted, measuredHalfDB)
	}
}

// TestZeroChannelMapIsTheDefaultAndProducesAudio is requirement five: a card
// presenting sixteen unpositioned channels must give the operator audio without
// them opening any UI.
//
// The zero value therefore means the DEFAULT MAP and not silence. A map that is
// missing from config.json, or predates the feature, or belongs to an operator
// who has never opened the mapping panel, must go on air with something on it.
func TestZeroChannelMapIsTheDefaultAndProducesAudio(t *testing.T) {
	var unset ChannelMap
	if !unset.IsDefault() {
		t.Fatal("a nil ChannelMap does not report as the default")
	}
	if !(ChannelMap{}).IsDefault() {
		t.Fatal("an empty non-nil ChannelMap does not report as the default; a map decoded from " +
			"an empty JSON array must mean the same thing as one that was absent")
	}

	matrix, err := unset.MixMatrix(16)
	if err != nil {
		t.Fatalf("the zero map could not be built against sixteen channels: %v", err)
	}
	if matrix[OutputLeft][0] != 1 || matrix[OutputRight][1] != 1 {
		t.Fatalf("the default is not input 1 to the left and input 2 to the right at unity: %v",
			matrix)
	}
	for out, row := range matrix {
		for in, g := range row {
			if in < ChannelMapOutputs && in == out {
				continue
			}
			if g != 0 {
				t.Errorf("the default routes input %d to output %d at %v; it must be the identity "+
					"on the first pair and nothing else, which is bit-for-bit what "+
					"decklinkaudiosrc channels=2 already gave us", in, out, g)
			}
		}
	}
}

// TestDefaultChannelMapSendsAMonoInputToBothOutputs covers the one-channel
// case, which nothing in the DeckLink path can produce but which makes
// DefaultChannelMap total over every width a pad can negotiate. It is what
// audioconvert's own mono-to-stereo upmix does, so a mono microphone behaves as
// it always has.
func TestDefaultChannelMapSendsAMonoInputToBothOutputs(t *testing.T) {
	matrix, err := ChannelMap(nil).MixMatrix(1)
	if err != nil {
		t.Fatalf("MixMatrix(1): %v", err)
	}
	if matrix[OutputLeft][0] != 1 || matrix[OutputRight][0] != 1 {
		t.Errorf("a single input channel does not reach both outputs at unity: %v", matrix)
	}
}

// TestMixMatrixRefusesTwoContributionsToOneCell guards the hole that summing
// into a cell would open. One cell holds one coefficient; two contributions to
// the same cell would have to be added, and the sum can leave the [-1, 1] clamp
// without either contribution doing so — a matrix refused by the element,
// silently, leaving the previous one in force.
func TestMixMatrixRefusesTwoContributionsToOneCell(t *testing.T) {
	m := ChannelMap{
		{Output: OutputLeft, Input: 3, Gain: 0.6},
		{Output: OutputLeft, Input: 3, Gain: 0.6},
	}
	if _, err := m.MixMatrix(16); err == nil {
		t.Fatal("two contributions to one cell were accepted; summed they are 1.2, which the " +
			"element refuses silently")
	}
}

// TestMixMatrixRefusesAnOutputThisPipelineDoesNotHave is the other axis of the
// width rule. The AAC encoder is pinned to two channels, so a third output row
// is a matrix whose height does not match the negotiated output caps — the same
// instant death as a wrong input width, from the other side.
func TestMixMatrixRefusesAnOutputThisPipelineDoesNotHave(t *testing.T) {
	for _, out := range []int{-1, ChannelMapOutputs, 5} {
		m := ChannelMap{{Output: out, Input: 0, Gain: 1}}
		if _, err := m.MixMatrix(16); err == nil {
			t.Errorf("output %d was accepted and this pipeline has %d outputs", out, ChannelMapOutputs)
		}
	}
}

// TestMixMatrixArgIsTheSerialisationGStreamerParses pins the exact string
// handed to gst_util_set_object_arg.
//
// It is pinned by value because there is no way to read the property back —
// mix-matrix is a GST_TYPE_ARRAY, which neither binding marshals — so this
// string is the ONLY record of what was sent. The form below is what
// gst-launch takes and what the hardware runs was driven with.
func TestMixMatrixArgIsTheSerialisationGStreamerParses(t *testing.T) {
	m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: 1}, {Output: OutputRight, Input: 1, Gain: 0.5}}
	matrix, err := m.MixMatrix(3)
	if err != nil {
		t.Fatalf("MixMatrix: %v", err)
	}

	const want = "<<(float)1,(float)0,(float)0>,<(float)0,(float)0.5,(float)0>>"
	if got := mixMatrixArg(matrix); got != want {
		t.Errorf("mixMatrixArg =\n  %s\nwant\n  %s\nThe property is set from this string and "+
			"cannot be read back, so a rendering GStreamer will not parse is a matrix that never "+
			"arrives and never reports that it did not", got, want)
	}
}

// TestOutputGainStatesWhatTheMapCanReach covers the decision that summing is
// allowed rather than refused or normalised: the model does not stop an
// operator putting two channels at unity into one output, and it hands the UI
// the number so the operator can be told what it means.
//
// Linear rather than decibels on purpose: a muted output sums to 0, and 0 in
// decibels is negative infinity, which neither survives encoding/json nor draws
// on anything. levels.go documents the identical trap for the -inf a level
// element reports on silence.
func TestOutputGainStatesWhatTheMapCanReach(t *testing.T) {
	router := ChannelMap{{Output: OutputLeft, Input: 4, Gain: 1}}
	gains, err := router.OutputGain(16)
	if err != nil {
		t.Fatalf("OutputGain: %v", err)
	}
	if gains[OutputLeft] != 1 {
		t.Errorf("a pure router reports %v, want 1", gains[OutputLeft])
	}
	if gains[OutputRight] != 0 {
		t.Errorf("an output nothing reaches reports %v, want 0 — and 0 rather than -Inf dB is the "+
			"point of this being linear", gains[OutputRight])
	}

	summed := ChannelMap{
		{Output: OutputLeft, Input: 0, Gain: 1},
		{Output: OutputLeft, Input: 1, Gain: 1},
	}
	gains, err = summed.OutputGain(16)
	if err != nil {
		t.Fatalf("two channels at unity into one output were refused, and summing two commentary "+
			"microphones to a mono leg is the commonest map a commentary position has: %v", err)
	}
	if gains[OutputLeft] != 2 {
		t.Errorf("two unity contributions report %v, want 2 (+6.02 dB, which is what the UI has to "+
			"be able to say)", gains[OutputLeft])
	}

	// Polarity inversion counts towards the worst case: -1 and +1 summed cancel
	// on correlated material and add on uncorrelated, and the number the UI
	// shows has to be the one that can clip.
	inverted := ChannelMap{
		{Output: OutputRight, Input: 0, Gain: 1},
		{Output: OutputRight, Input: 1, Gain: -1},
	}
	gains, err = inverted.OutputGain(16)
	if err != nil {
		t.Fatalf("OutputGain: %v", err)
	}
	if gains[OutputRight] != 2 {
		t.Errorf("an inverted contribution reports %v, want 2: magnitude, not signed sum",
			gains[OutputRight])
	}
}

// TestChannelMapStringIsOperatorNumbering guards the one place this package
// counts from one. The log line and the UI label exist to be compared against
// what somebody reads off an embedder, where the first channel is channel 1.
func TestChannelMapStringIsOperatorNumbering(t *testing.T) {
	m := ChannelMap{{Output: OutputLeft, Input: 0, Gain: 1}, {Output: OutputRight, Input: 5, Gain: 0.25}}
	got := m.String()
	if !strings.Contains(got, "in 1 -> L") {
		t.Errorf("input index 0 is not rendered as channel 1: %q", got)
	}
	if !strings.Contains(got, "in 6 -> R at 0.25") {
		t.Errorf("input index 5 at 0.25 is not rendered as channel 6: %q", got)
	}
	if !strings.Contains(ChannelMap(nil).String(), "default") {
		t.Errorf("the zero map does not say it is the default: %q", ChannelMap(nil).String())
	}
}
