// channelmap.go is the ROUTING MODEL: which of a capture device's input
// channels reach the commentary feed's left and right, and at what gain.
//
// Owner: WP-3a, with the rest of this package.
//
// It carries NO BUILD TAG, deliberately and for the same reason levels.go
// carries none. Everything decidable without GStreamer is lifted into pure Go
// so that Gate A runs the real arithmetic rather than a paraphrase of it: the
// matrix this file builds is the matrix the cgo twin writes to the element and
// the matrix the stub twin validates, so the two builds cannot disagree about
// which maps are legal, what a rejected coefficient does, or which way round
// the matrix goes. The only cgo-side part is one g_object_set, in
// gst_cgo.go's SetChannelMap.
//
// # Why this file exists at all: sixteen unpositioned channels
//
// A Blackmagic UltraStudio 4K Mini presents its embedded SDI/HDMI audio as
// SIXTEEN channels with channel-mask=0x0 — UNPOSITIONED, meaning the stream
// says how many channels there are and refuses to say what any of them is.
// audioconvert can downmix a POSITIONED stream to stereo because it knows
// which channel is the centre and which is the left surround. Handed sixteen
// channels with no positions it has nothing to derive a downmix from, and the
// application's chain dies:
//
//	decklinkaudiosrc channels=16 ! audioconvert ! audioresample
//	  ! audio/x-raw,format=S16LE,rate=48000,channels=2,layout=interleaved
//	-> streaming stopped, reason not-negotiated (-4), 0.069 s after PLAYING
//
// Measured on the port machine (Darwin 25.3.0, GStreamer 1.26.10, real card,
// persistent-id 2747401380) on 2026-08-16. Written without audioresample the
// same graph does not even parse: "could not link decklinkaudiosrc0 to
// audioconvert0". The failure moves; it does not go away.
//
// An explicit audioconvert mix-matrix fixes it, because a matrix IS the
// missing statement of what each channel is for. That is the whole mechanism
// this file serves.
//
// # What is given up: this is a ROUTER WITH ATTENUATION, not a mixer
//
// audioconvert's mix-matrix coefficients are HARD-CLAMPED to [-1, 1] by
// GObject's own property validation. 1.0 is accepted; 1.0000001 is refused.
// There is no coefficient above unity, which means there is no make-up gain,
// and there is no output trim BELOW the matrix either — the matrix is the last
// gain stage before the level meter and the AAC encoder. So the only headroom
// this design has is the headroom the operator leaves in the map itself.
//
// The consequence is stated on ChannelMap.OutputGain and is deliberately
// ALLOWED rather than refused or normalised; the argument is there.
//
// # What is given up: two outputs, forever
//
// ChannelMapOutputs is 2 and there is no path to more. The contribution
// pipeline pins channels=2 into the AAC encoder because that is what M2L-X
// takes, so a third output row would be a matrix whose width did not match the
// negotiated output caps — the same instant death as a wrong INPUT width, from
// the other axis. If a facility ever needs more, the capsfilter and this
// constant move together or not at all.
package gst

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrChannelMap is wrapped by every refusal to turn a map into a matrix.
//
// It exists so the App layer can tell "the operator has asked for something
// this device cannot do" apart from "the pipeline is not running" and from a
// capture fault. Those three want three different things said to the operator,
// and only the first of them is the operator's to fix.
var ErrChannelMap = errors.New("gst: channel map refused")

// ChannelMapOutputs is the number of output channels the matrix produces: the
// stereo pair the AAC encoder is pinned to. See the file comment for why there
// is no path to a third.
const ChannelMapOutputs = 2

// The two output rows, by the names an operator uses. They are the ROW INDICES
// of the matrix — see ChannelMap.MixMatrix for why rows are outputs and columns
// are inputs, which is the one thing in this file that fails silently when it
// is wrong.
const (
	OutputLeft  = 0
	OutputRight = 1
)

// MaxInputChannels is the widest input this model will build a matrix for.
//
// It is a SANITY BOUND, not a hardware limit expressed twice. decklinkaudiosrc
// offers 2, 8 and 16 and nothing else, so 16 is the widest stream this
// application can meet today; the bound exists so that a channel count read
// back as a garbled number — a caps query against a pad that has renegotiated
// under us, a future element publishing something absurd — is refused here
// rather than turned into an allocation of that many columns and a matrix
// GStreamer will reject or, worse, accept.
//
// A device genuinely presenting more than sixteen channels is a change to what
// this application supports and should arrive as a change to this number with
// a measurement beside it, not as a matrix nobody sized.
const MaxInputChannels = 16

// ChannelGainLimit is the magnitude beyond which audioconvert refuses a
// coefficient, and it is GStreamer's number rather than a policy of ours.
//
// # The rejection is SILENT and this is the most important sentence in the file
//
// Measured on 2026-08-16 with the real card, a live pipeline and a known
// -9 dBFS 1 kHz tone on inputs 1 and 2: with a 0.5 matrix in place (meter at
// -15.019 dBFS), writing a matrix of 1.0000001 produced
//
//	GLib-GObject-CRITICAL: value "< < 1.000000, ... > >" of type
//	'GstValueArray' is invalid or out of range for property 'mix-matrix'
//
// on stderr, where no shipped build is looking, and the meter DID NOT MOVE:
// -15.019498150506179 dBFS before the write and -15.019498150506179 after it,
// a delta of exactly 0.0000 dB. The previous matrix stayed in place, the
// property setter returned normally, and nothing an application can observe
// distinguished the refused write from an accepted one.
//
// So THE APPLICATION MUST NEVER DEPEND ON GSTREAMER TO REJECT ANYTHING. Every
// coefficient is validated here, before the write, and a map that would be
// refused is refused by us with a message naming the channel and the value.
// Validation after the fact is not available at any price: there is nothing to
// read back that would tell us which matrix is in force.
const ChannelGainLimit = 1.0

// ChannelContribution is one cell of the routing matrix: one input channel
// reaching one output channel at one gain.
//
// A map is a LIST of these rather than a dense matrix because that is the shape
// the operator's intent has — "channel 5 is the commentator, put it on both
// sides" is two contributions — and because a list survives a change of input
// width without silently meaning something different. A dense 2x16 grid saved
// to config.json and reloaded against a device presenting 8 channels is a grid
// that has to be truncated by somebody; a list is refused by name.
//
// The json tags are the wire form for the mapping UI. The field names are the
// operator's vocabulary, not GStreamer's: audioconvert would call these a row
// and a column, and a row/column pair is precisely the thing that is impossible
// to get wrong in one direction and silently fatal in the other.
type ChannelContribution struct {
	// Output is the output channel this contribution feeds: OutputLeft or
	// OutputRight.
	Output int `json:"output"`

	// Input is the ZERO-BASED index of the input channel. Channel 1 on the
	// operator's SDI embedder is Input 0 here, and the UI is responsible for
	// that +1 — this package counts from zero because the matrix does, and a
	// model that counted from one would put the conversion somewhere it could
	// be done twice.
	Input int `json:"input"`

	// Gain is the linear coefficient, in [-ChannelGainLimit, ChannelGainLimit].
	// 1.0 is unity, 0.5 is -6.02 dB, 0 is a cell that contributes nothing, and
	// a negative value inverts the polarity — which is a real thing to want on
	// a desk that has sent a leg out of phase, and costs nothing to allow since
	// the element takes the whole range anyway.
	Gain float64 `json:"gain"`
}

// ChannelMap is the whole routing decision: every contribution, in no
// particular order.
//
// # THE ZERO VALUE IS THE DEFAULT MAP, AND THAT IS THE POINT
//
// A nil or empty ChannelMap does NOT mean silence. It means "nobody has chosen
// yet", and it resolves — see DefaultChannelMap — to input 1 on the left and
// input 2 on the right at unity.
//
// That reading is chosen deliberately over the tidier "empty means route
// nothing". A commentary position whose channel map is missing from
// config.json, or whose config.json predates this feature, or whose operator
// has never opened the mapping UI, must go on air with AUDIO on it. An empty
// map meaning silence is a feed that is perfectly healthy by every lamp in the
// application and carries nothing, discovered by the director. The cost of
// this choice is that a map cannot express "mute everything"; an operator who
// wants that has gains, and the application has a Stop button.
//
// The default itself is argued in DefaultChannelMap.
type ChannelMap []ChannelContribution

// DefaultChannelMap is what the zero value means, resolved against the channel
// count the input pad actually negotiated.
//
// # The choice: input 1 to the left, input 2 to the right, at unity
//
// Two reasons, and the second is the one that matters.
//
// Channels 1 and 2 are the programme pair. SDI embedded audio group 1 pair 1
// is where every commentary desk, every M2L-X output and every piece of
// facilities kit puts the main stereo mix; a card that has anything on it at
// all has something on channels 1 and 2.
//
// And it is BIT-FOR-BIT WHAT THE APPLICATION ALREADY DID. decklinkaudiosrc
// channels=2 negotiates a POSITIONED stereo pair (channel-mask 0x3) carrying
// the card's channels 1 and 2, which audioconvert passes through unchanged.
// This matrix — 1.0 from input 0 to output 0, 1.0 from input 1 to output 1, and
// zero everywhere else — is the identity on those two channels. So moving the
// steady state to sixteen channels, which is forced (the channels property is
// NOT live-settable, so reaching pairs 3/4 and up any other way costs a
// pipeline restart), changes NOTHING AUDIBLE for an operator who never opens
// the mapping UI. That is the strongest form the requirement can take: the
// default is not merely sensible, it is the previous behaviour.
//
// # The one-channel case
//
// A single-channel input goes to BOTH outputs at unity, which is what
// audioconvert's own mono-to-stereo upmix does and is what the operator of a
// mono microphone already gets today. It is here so that this function is total
// over every width a pad can negotiate; nothing in the DeckLink path can
// produce it.
//
// A width of zero or less returns nil, because there is no map to make. Callers
// get the refusal from MixMatrix, which names the width.
func DefaultChannelMap(inputChannels int) ChannelMap {
	switch {
	case inputChannels <= 0:
		return nil
	case inputChannels == 1:
		return ChannelMap{
			{Output: OutputLeft, Input: 0, Gain: 1},
			{Output: OutputRight, Input: 0, Gain: 1},
		}
	default:
		return ChannelMap{
			{Output: OutputLeft, Input: 0, Gain: 1},
			{Output: OutputRight, Input: 1, Gain: 1},
		}
	}
}

// IsDefault reports whether this map is the zero value — the state in which
// DefaultChannelMap decides what happens. It exists so the UI can say "this
// position has never been mapped" rather than drawing the default and implying
// somebody chose it.
func (m ChannelMap) IsDefault() bool { return len(m) == 0 }

// MixMatrix builds the audioconvert mix-matrix for this map against the channel
// count THE PAD ACTUALLY NEGOTIATED.
//
// # ORIENTATION: OUTPUT ROWS x INPUT COLUMNS
//
// The returned matrix has ChannelMapOutputs rows of inputChannels columns — two
// rows of sixteen for the card's 16-in/2-out case — and matrix[out][in] is the
// coefficient. THE REVERSE SILENTLY FAILS. It was verified both ways on the
// real card; a transposed matrix is not rejected as malformed, it is a matrix
// of the wrong shape, and a matrix of the wrong shape is the failure the width
// rule below is about. TestMixMatrixIsOutputRowsByInputColumns asserts both the
// shape and a deliberately asymmetric map, because a 2-in/2-out matrix is
// square and a transpose is invisible in the dimensions alone.
//
// # THE WIDTH RULE: inputChannels MUST BE WHAT THE PAD NEGOTIATED
//
// Not what the device advertised. The DeckLink device structure publishes
// max-channels=16 and it is not the same statement: it is what the card CAN
// do, and the element's channels property, the connection in use and the
// negotiation all sit between it and what arrives on the pad. This function
// exists in this shape — taking the count as an argument and refusing a map
// that names a channel outside it — because a comment saying "size it against
// the pad" is a comment somebody will read after the outage.
//
// A WRONG WIDTH KILLS THE PIPELINE INSTANTLY and is not reported as a bad
// value. Measured on the card on 2026-08-16, live and PLAYING with a good
// 2x16 matrix in force, writing a 2x8 matrix:
//
//	BUS ERROR: Internal data stream error.
//	  gst_base_src_loop (): /GstPipeline:pipeline0/GstDecklinkAudioSrc:...
//	  streaming stopped, reason error (-5)
//	pipeline state -> StateChangeFailure
//
// The property ACCEPTED the write — every coefficient was inside the clamp, so
// there was nothing for GObject to object to — and the capture chain was dead
// before the next level message. In a parse string the same mismatch fails
// earlier and just as hard: "could not link decklinkaudiosrc0 to audioconvert0".
// There is no width mistake that produces a degraded feed instead of no feed.
//
// # What is refused, and why every one of these is refused BEFORE the write
//
// The clamp is GStreamer's and is silent (see ChannelGainLimit), so validating
// afterwards is not possible: a refused write leaves the previous matrix in
// force and looks exactly like a successful one. Everything below is therefore
// checked here, and gst_cgo.go's SetChannelMap writes only what this function
// returned.
//
//   - a width outside [1, MaxInputChannels]
//   - an output that is not a row of the matrix
//   - an input outside the negotiated width
//   - a gain that is NaN, infinite, or outside [-1, 1] AS A float32, which is
//     the type the element's array actually holds; see gainOutOfRange
//   - two contributions naming the same cell, which is a UI bug rather than an
//     intention and would otherwise sum into a coefficient nobody typed
func (m ChannelMap) MixMatrix(inputChannels int) ([][]float64, error) {
	if inputChannels < 1 || inputChannels > MaxInputChannels {
		return nil, fmt.Errorf("%w: the input pad negotiated %d channels, which is not a channel "+
			"count this application can build a matrix for (1 to %d). The count must come from the "+
			"pad's own caps, never from the device's max-channels: a matrix of the wrong width does "+
			"not degrade the feed, it stops it",
			ErrChannelMap, inputChannels, MaxInputChannels)
	}

	resolved := m
	if resolved.IsDefault() {
		resolved = DefaultChannelMap(inputChannels)
	}

	matrix := make([][]float64, ChannelMapOutputs)
	for out := range matrix {
		matrix[out] = make([]float64, inputChannels)
	}

	// filled tracks cells rather than counting them, so the refusal below can
	// name the exact pair the operator sent twice.
	filled := make(map[[2]int]bool, len(resolved))

	for i, c := range resolved {
		if c.Output < 0 || c.Output >= ChannelMapOutputs {
			return nil, fmt.Errorf("%w: contribution %d routes to output %d, and this pipeline has "+
				"%d outputs (%d left, %d right)",
				ErrChannelMap, i, c.Output, ChannelMapOutputs, OutputLeft, OutputRight)
		}
		if c.Input < 0 || c.Input >= inputChannels {
			return nil, fmt.Errorf("%w: contribution %d takes input channel %d, and the pad "+
				"negotiated %d channels (0 to %d). A matrix naming a channel the stream does not "+
				"have is a matrix of the wrong width",
				ErrChannelMap, i, c.Input, inputChannels, inputChannels-1)
		}
		if why := gainOutOfRange(c.Gain); why != "" {
			return nil, fmt.Errorf("%w: contribution %d routes input %d to output %d at a gain of "+
				"%v, which is %s. audioconvert clamps coefficients to [%v, %v] and rejects the "+
				"whole matrix SILENTLY, leaving the previous one in force, so this is refused here "+
				"rather than written and hoped for",
				ErrChannelMap, i, c.Input, c.Output, c.Gain, why,
				-ChannelGainLimit, ChannelGainLimit)
		}
		cell := [2]int{c.Output, c.Input}
		if filled[cell] {
			return nil, fmt.Errorf("%w: contribution %d routes input %d to output %d, which is "+
				"already routed. One cell of the matrix holds one coefficient; two contributions "+
				"to the same cell would have to be summed into a gain nobody chose",
				ErrChannelMap, i, c.Input, c.Output)
		}
		filled[cell] = true
		matrix[c.Output][c.Input] = c.Gain
	}

	return matrix, nil
}

// gainOutOfRange returns the empty string for an acceptable coefficient, and
// otherwise the clause that says what is wrong with it.
//
// # It checks the float32, not the float64 it was given
//
// The element's mix-matrix is a GstValueArray of GstValueArrays of G_TYPE_FLOAT
// — a 32-bit float — and mixMatrixArg renders exactly that. So the value that
// meets GObject's range check is the ROUNDED one, and the rounded one is what
// is validated here. It costs one conversion and it means this function is
// checking the bytes that will actually be sent rather than the bytes that were
// handed to it.
//
// No float64 inside [-1, 1] rounds to a float32 outside it — the values just
// under 1 round to 1.0 exactly, never past it — so today this changes no
// answer. It is written this way so that it still cannot, if the rendering ever
// changes.
func gainOutOfRange(gain float64) string {
	if math.IsNaN(gain) {
		return "not a number"
	}
	if math.IsInf(gain, 0) {
		return "infinite"
	}
	if g := float64(float32(gain)); g > ChannelGainLimit || g < -ChannelGainLimit {
		return "outside the [-1, 1] range the element accepts"
	}
	return ""
}

// OutputGain is the worst-case LINEAR gain of each output row: the sum of the
// magnitudes of its coefficients.
//
// 1.0 is a pure router — one channel arriving at unity, out at unity. 0 is an
// output nothing reaches. 2.0 is two channels at unity summed into one output,
// which is the case this function exists for.
//
// # The decision this number represents: summing is ALLOWED and stated, not refused
//
// Two contributions at 1.0 into one output SUM, and with correlated material
// that is +6.02 dB. There is no headroom control anywhere in this design — the
// coefficients cannot exceed unity, and the matrix is the last gain stage
// before the meter and the encoder — so a map like that can clip, and nothing
// downstream can rescue it.
//
// The three available answers were refuse, normalise and allow:
//
//   - REFUSING any map whose row sums past 1.0 was tried on paper and is wrong.
//     Two commentators' microphones summed to a mono leg at unity is the single
//     most ordinary map a commentary position has, and at real programme levels
//     — peaks around -9 dBFS, as measured on the loopback tone — it does not
//     come close to clipping. A rule that refuses the commonest correct map to
//     prevent an uncommon incorrect one is a rule that gets worked around.
//   - NORMALISING silently disagrees with the numbers the operator typed. They
//     would set two gains to 1.0, read 1.0 back in the UI, and hear something
//     6 dB down from what the meter arithmetic says, with nothing anywhere
//     explaining it. This project does not have status displays that lie.
//   - ALLOWING it, and handing the UI the number, is what is done. The operator
//     is told what the map can reach; the meter beside it measures the real
//     signal at the real point (level sits immediately downstream of this
//     matrix, so what it shows is post-mix and post-gain — proven on the card:
//     the same tone read -8.9996 dBFS at unity and -15.0195 dBFS at 0.5); and
//     the decision about a stadium-effects bus stays with the person who can
//     hear it.
//
// It is LINEAR rather than decibels on purpose. A muted output is a sum of 0,
// and 0 in decibels is negative infinity, which neither survives encoding/json
// nor draws on anything — the identical trap levels.go documents at length for
// the -inf a level element reports on digital silence. The UI converts.
func (m ChannelMap) OutputGain(inputChannels int) ([ChannelMapOutputs]float64, error) {
	var gains [ChannelMapOutputs]float64
	matrix, err := m.MixMatrix(inputChannels)
	if err != nil {
		return gains, err
	}
	for out, row := range matrix {
		for _, g := range row {
			gains[out] += math.Abs(g)
		}
	}
	return gains, nil
}

// mixMatrixArg renders a matrix as the string gst_util_set_object_arg
// deserialises into the mix-matrix property.
//
// # Why a string and not a typed value
//
// mix-matrix is a GstValueArray of GstValueArrays of G_TYPE_FLOAT — GST_TYPE_ARRAY,
// which is a GStreamer fundamental type and not the deprecated GLib
// GValueArray. go-glib v0.0.2 binds the latter (gobject/valuearray.go, which is
// what the LEVEL messages come back as) and neither it nor go-gst v0.0.2 binds
// the former at all, so there is no typed setter to call. gst_util_set_object_arg
// runs the property's own gst_value_deserialize, which handles GST_TYPE_ARRAY,
// and it is already this package's idiom for a property whose GType we cannot
// name from Go — applyEncoderProperties sets every encoder setting the same way.
//
// The shape is GStreamer's own serialisation and is what gst-launch takes:
//
//	<<(float)1,(float)0>,<(float)0,(float)1>>
//
// Numbers are rendered with FormatFloat 'g' at bitSize 32, which is the
// shortest decimal that round-trips to the same float32 — the type the array
// actually holds. Rendering at 64 bits would write digits the element then
// rounds away, which is harmless but makes a log line disagree with the value
// in force.
//
// # THE TRAP NEXT DOOR: input-channels-reorder
//
// gst-inspect audioconvert lists input-channels-reorder immediately beside
// mix-matrix, its nicks read like exactly this problem's answer (gst, smpte,
// cine, ac3, aac, monogst), and it needs no matrix, no width and no arithmetic.
// WHOEVER READS THIS NEXT WILL FIND IT AND TRY IT.
//
// IT PRODUCES DIGITAL SILENCE. On a 2-channel unpositioned stream EVERY reorder
// nick took a -9 dBFS input to -96 dBFS — the noise floor, i.e. nothing —
// reproduced with two independently constructed sources. It is not a downmix
// and it is not a fallback for one; it renames positions on a stream that has
// positions, and a stream with channel-mask=0x0 has none to rename. There is no
// setting of it that helps here, and the way it fails is a feed that starts,
// locks, shows a healthy bitrate and is silent.
func mixMatrixArg(matrix [][]float64) string {
	var b strings.Builder
	b.WriteByte('<')
	for out, row := range matrix {
		if out > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('<')
		for in, g := range row {
			if in > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(float)")
			b.WriteString(strconv.FormatFloat(g, 'g', -1, 32))
		}
		b.WriteByte('>')
	}
	b.WriteByte('>')
	return b.String()
}

// String renders a map the way it goes in a log line: the contributions an
// operator would read out, left to right.
//
// It is the OPERATOR's numbering — channel 1 is the first channel — because
// this string's only job is to be compared against what somebody sees on an
// embedder or a desk. Nothing parses it, and nothing may: the wire form is the
// json above.
func (m ChannelMap) String() string {
	if m.IsDefault() {
		return "(default: input 1 to left, input 2 to right, unity)"
	}
	parts := make([]string, 0, len(m))
	for _, c := range m {
		side := "L"
		if c.Output == OutputRight {
			side = "R"
		} else if c.Output != OutputLeft {
			side = "output " + strconv.Itoa(c.Output)
		}
		parts = append(parts, fmt.Sprintf("in %d -> %s at %s",
			c.Input+1, side, strconv.FormatFloat(c.Gain, 'g', -1, 64)))
	}
	return strings.Join(parts, ", ")
}
