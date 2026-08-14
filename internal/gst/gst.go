// Package gst is the application's entire GStreamer surface, and the only
// package in the module that touches cgo.
//
// Owner: WP-3a. No other work package writes files in this directory.
//
// It is deliberately thin. It builds a pipeline, lists capture devices and swaps
// a sink. Every decision — when to reconnect, how long to wait, what the lamps
// say — lives in internal/sender, which is pure Go and never imports cgo.
//
// # Two implementations
//
// The interfaces and option types below carry no build tag. Two files implement
// them and are selected by whether cgo is enabled:
//
//   - gst_cgo.go  (//go:build cgo)  — the real go-gst pipeline. Requires MinGW
//     gcc and the GStreamer development installer (Gate B).
//   - gst_stub.go (//go:build !cgo) — a pure-Go fake that returns plausible
//     device names and a Pipeline whose transitions can be driven
//     programmatically. It requires no toolchain at all (Gate A) and is what
//     lets the rest of the application be built and demonstrated today.
//
// Each file provides Init, ListInputDevices and New. Everything above this seam
// is identical in both builds.
//
// # And, inside the real build, two platforms
//
// gst_cgo.go is itself platform-neutral. What differs between Windows and macOS
// is confined to two pairs of files, on this codebase's usual per-file
// build-tag idiom:
//
//   - elements_windows.go / elements_darwin.go — the ELEMENT contract: the
//     capture source and AAC encoder factory names, the platform half of the
//     bundle's required-element list, and the H.264 encoder preference and
//     settings.
//   - deviceprovider_windows.go / deviceprovider_darwin.go — the DEVICE seam:
//     which enumerated devices this build can offer, what string is persisted
//     for each, and how that string is turned back into whatever the capture
//     element will actually accept.
//
// There is deliberately no runtime.GOOS anywhere in this package.
//
// # The pipeline this package builds
//
// Specification section 5, as one gst_parse_launch string. EXACTLY TWO element
// names vary by platform; nothing else in the graph does.
//
//	mpegtsmux name=mux alignment=7 pcr-interval=3600
//	  ! queue name=srtq leaky=downstream max-size-buffers=4000
//	  ! srtsink name=srtout sync=false async=false auto-reconnect=false
//	filesrc location=<slate> ! pngdec ! imagefreeze is-live=true ! videoconvert
//	  ! video/x-raw,format=NV12,width=1920,height=1080,framerate=50/1,...
//	  ! mfh264enc name=venc bitrate=2000 rc-mode=cbr gop-size=100
//	    low-latency=true cabac=true
//	  ! h264parse config-interval=-1 ! queue ! mux.
//	wasapi2src name=asrc device=<endpoint id> low-latency=true
//	  ! audioconvert ! audioresample ! level name=alevel interval=50000000
//	  ! mfaacenc bitrate=128000
//	  ! aacparse ! queue ! mux.
//
// On macOS the same graph reads:
//
//	  ! vtenc_h264 name=venc bitrate=2000 rate-control=cbr
//	    max-keyframe-interval=100 realtime=true allow-frame-reordering=false
//	  ! h264parse config-interval=-1 ! queue ! mux.
//	osxaudiosrc name=asrc device=<AudioDeviceID resolved at Start>
//	  ! audioconvert ! audioresample ! level name=alevel interval=50000000
//	  ! atenc bitrate=128000
//	  ! aacparse ! queue ! mux.
//
// srtsink's own auto-reconnect is set to false and must stay false: on a write
// failure it reopens immediately with no backoff, retries once, and then errors
// the whole pipeline out — straight into M2L-X's roughly five second re-accept
// refusal window.
package gst

import "errors"

// ErrPipelineFatal is wrapped by every error reporting a failure that
// replacing the sink cannot repair — the capture or mux chain, not the SRT
// link. It exists so that internal/sender can stop treating such a failure as
// a connection problem: before this sentinel, a wasapi2 device that failed to
// open asynchronously (the operator selecting a playback endpoint as the
// commentary input) was retried as though the network were down, telling the
// operator "the feed is not connected and is retrying" forever about a fault
// no reconnect could ever fix.
//
// The condition LATCHES for the life of the pipeline: once one error wrapping
// this sentinel has been returned or delivered, every subsequent ReplaceSink
// on the same Pipeline returns it. Recovery is Stop, New, Start — nothing
// less rebuilds the failed chain.
//
// The message text deliberately keeps the "pipeline-fatal" substring that the
// pre-sentinel error carried, so anything still matching on the string —
// field-log greps, BUILD-NOTES.md instructions — keeps matching. New code
// must use errors.Is(err, ErrPipelineFatal) instead.
var ErrPipelineFatal = errors.New("gst: pipeline-fatal")

// Encoder settings fixed by specification section 5. The units differ between
// the two encoders and the names here say which is which.
const (
	// DefaultVideoBitrateKbps is mfh264enc's bitrate property, in KILOBITS per
	// second. Constant bitrate, not quality-targeted: a static slate under QVBR
	// collapses to 200-350 kbps, which is cheaper but bursty at every IDR and
	// makes "is it flowing" harder to observe.
	DefaultVideoBitrateKbps = 2000

	// DefaultAudioBitrateBps is mfaacenc's bitrate property, in BITS per second.
	DefaultAudioBitrateBps = 128000

	// DefaultSRTLatencyMs is srtsink's latency property, in MILLISECONDS. The
	// property is milliseconds despite what the original brief's example URI
	// suggested.
	DefaultSRTLatencyMs = 120
)

// Device is one audio capture endpoint offered in the commentary input
// dropdown.
type Device struct {
	// ID is the operating system's own STABLE identity for the endpoint. It is
	// what is persisted to config.json, and it survives a rename — which is why
	// it, rather than Name, is the thing stored. It sidesteps the double space
	// in "DVS Receive  1-2 (Dante Virtual Soundcard)" entirely.
	//
	// The shape and the meaning are per-platform, and the difference is bigger
	// than it looks:
	//
	//	Windows  a WASAPI IMMDevice endpoint ID GUID, e.g.
	//	         "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}".
	//	         It is passed to wasapi2src's device property UNCHANGED.
	//	macOS    a CoreAudio unique-id, e.g. "BuiltInMicrophoneDevice" or
	//	         "BF568F24-731B-41DB-932E-AC7E260BC71A". It is NOT what
	//	         osxaudiosrc accepts — that property is a gint AudioDeviceID,
	//	         a runtime handle that coreaudiod reassigns on every
	//	         enumeration — so it is resolved to the current integer at
	//	         pipeline-open time. See deviceprovider_darwin.go; storing the
	//	         integer instead is a silent wrong-device failure after the
	//	         operator's next reboot.
	//
	// What is true on both, and is the contract callers may rely on: this value
	// is stable across restarts, it is the only thing that should ever be
	// persisted, and it is meaningless to the browser (whose mediaDeviceId is a
	// per-origin salted hash and identifies nothing at the operating-system
	// level).
	ID string `json:"id"`

	// Name is the endpoint's display-name, shown in the dropdown and never
	// persisted or passed to GStreamer.
	Name string `json:"name"`
}

// PipelineOpts configures everything upstream of the sink: the slate branch, the
// capture branch, the encoders and the muxer. These cannot be changed without
// rebuilding the pipeline, which is why the SRT endpoint is not among them.
type PipelineOpts struct {
	// SlatePath is the PNG fed to filesrc ! pngdec ! imagefreeze. Required.
	SlatePath string

	// AudioDeviceID is the capture endpoint to open. Required; it is Device.ID,
	// never Device.Name, and the whole of what that means — including the fact
	// that macOS has to resolve it before the element will take it — is
	// documented on Device.ID.
	AudioDeviceID string

	// VideoBitrateKbps is the H.264 encoder's bitrate in kilobits per second.
	// Zero means DefaultVideoBitrateKbps. Kilobits is the unit on mfh264enc and
	// on vtenc_h264 alike.
	VideoBitrateKbps int

	// AudioBitrateBps is the AAC encoder's bitrate in bits per second. Zero
	// means DefaultAudioBitrateBps. Bits is the unit on mfaacenc and on atenc
	// alike.
	AudioBitrateBps int

	// OnLevels, if set, is called with the audio level of the commentary being
	// sent — peak and RMS per channel, in dBFS — roughly twenty times a second
	// (the level element's 50 ms interval). It is what feeds the input meters
	// beside the big picture, and it measures the signal at the one point that
	// answers the operator's actual question: after audioconvert and
	// audioresample, immediately upstream of the AAC encoder, so what the meter
	// shows is what is ACTUALLY being encoded and sent. A meter fed from the
	// browser's idea of the microphone would keep moving while the wrong device
	// was selected, which is a reassurance nobody should be given.
	//
	// It is called ON THE BUS/STREAMING GOROUTINE and MUST NOT BLOCK: in the
	// real build the caller is a GStreamer streaming thread inside
	// gst_element_post_message, and a callback that waits there stalls the
	// capture chain the way the file comment in gst_cgo.go forbids at length.
	// Hand the value to a queue or an atomic and return.
	//
	// Values are clamped to the -100 dBFS floor before delivery — the level
	// element reports -inf for digital silence, and -inf neither survives JSON
	// nor draws on a meter. Nil means no metering; nothing else changes.
	OnLevels func(Levels)
}

// Levels is one audio level report from the send pipeline: what the level
// element measured over its last 50 ms window, immediately upstream of the AAC
// encoder.
//
// PeakDB and RMSDB carry one entry per channel — [left, right] for the
// specification's stereo pipeline — in dBFS, 0 being digital full scale.
// Silence is -100, never -inf: the producer clamps (see clampLevelDB), because
// -inf does not survive JSON and a meter has a floor anyway.
type Levels struct {
	// PeakDB is the per-channel peak over the measurement window, dBFS.
	PeakDB []float64
	// RMSDB is the per-channel RMS over the same window, dBFS. Always at or
	// below the peak for any real signal.
	RMSDB []float64
}

// SinkOpts configures the srtsink alone. It is separate from PipelineOpts
// because reconnecting replaces only the sink: the capture, encode and mux chain
// never leaves PLAYING for the life of the process, so running time never moves
// backwards and the measured non-monotonic DTS jump cannot arise.
type SinkOpts struct {
	// Host is the M2L-X SRT listener's hostname or address.
	Host string

	// Port is that listener's port.
	Port int

	// LatencyMs is srtsink's latency property in MILLISECONDS. Zero means
	// DefaultSRTLatencyMs.
	LatencyMs int

	// Passphrase is the SRT passphrase from Credential Manager. It is set with
	// g_object_set, never placed in the URI, so it is neither percent-encoded
	// nor logged. Empty means an unencrypted session.
	Passphrase string

	// PBKeyLen is the SRT key length, 16 or 32. Ignored when Passphrase is
	// empty.
	PBKeyLen int
}

// Pipeline is one media pipeline: slate plus commentary audio, encoded, muxed to
// MPEG-TS and sent to an SRT listener as a caller.
//
// A Pipeline is single-use. After Stop it cannot be restarted; call New again.
// All methods are safe for concurrent use.
type Pipeline interface {
	// Start builds the pipeline and takes it to PLAYING, but installs NO SINK.
	// The pipeline runs with the src pad of the leaky srtq queue blocked, which
	// means capture and encoding are live and correctly paced before the first
	// connection attempt. The caller must follow Start with ReplaceSink to
	// actually connect.
	//
	// Start pins the system clock and reuses one process-lifetime base time, so
	// that a pipeline rebuilt after a device change does not restart PTS from
	// zero. It returns an error if called twice or after Stop.
	Start(opts PipelineOpts) error

	// ReplaceSink installs a fresh srtsink with the given properties, replacing
	// any sink already present: block the srtq src pad, unlink, set the old sink
	// to NULL, remove it, create and add the new one, link, SyncStateWithParent,
	// unblock.
	//
	// It returns synchronously. A nil error means the SRT caller handshake
	// succeeded and media is flowing; a non-nil error means it did not, and the
	// caller — internal/sender — is responsible for backing off and trying
	// again. Nothing upstream of srtq leaves PLAYING either way.
	//
	// One class of failure is not retryable, and ReplaceSink says so: an error
	// satisfying errors.Is(err, ErrPipelineFatal) means the capture or mux
	// chain itself has failed and no sink swap can carry media again. The
	// condition latches for the life of the pipeline — every subsequent
	// ReplaceSink returns it — and the only recovery is Stop, New, Start.
	// Backing off and retrying such an error is the measured misdiagnosis this
	// sentinel exists to end: a broken local device reported to the operator
	// as a network that keeps refusing to connect.
	ReplaceSink(opts SinkOpts) error

	// RemoveSink tears the current sink out without installing another: block the
	// srtq src pad, unlink, set srtsink to NULL, remove it. Everything upstream
	// stays in PLAYING. It is idempotent — removing when no sink is installed is
	// not an error.
	//
	// This exists because specification section 6.2 orders the reconnect
	// DRAINING (tear down) -> BACKOFF (wait) -> CONNECTING (install), and that
	// order is load-bearing rather than cosmetic. An M2L-X SRT listener accepts
	// exactly one peer, never displaces the incumbent, and refuses re-accept for
	// roughly five seconds; the >= 6 s first rung of sender.BackoffLadder is
	// sized to outlast that window. The wait only achieves anything if OUR socket
	// is already gone when it starts.
	//
	// Without this method the only teardown available is the one inside
	// ReplaceSink, which destroys the old sink microseconds before dialling the
	// new one — so the backoff elapses while we still hold the socket, and the
	// retry lands inside the refusal window it was supposed to clear. That costs
	// a wasted attempt, and roughly fourteen seconds off air instead of seven, on
	// every mid-match reconnect.
	//
	// Added after WP-0 by the coordinator, on the adversarial review of
	// internal/sender finding 1. It is the one change to this interface since it
	// was frozen.
	RemoveSink() error

	// ForceKeyUnit sends a GstForceKeyUnit event upstream so that the encoder
	// emits an IDR immediately. It is called after a successful ReplaceSink so
	// the picture recovers at once instead of waiting up to two seconds for the
	// next scheduled IDR.
	ForceKeyUnit() error

	// Errors returns the pipeline's asynchronous error channel, carrying
	// GST_ELEMENT_ERROR messages from the bus — in practice, srtout losing its
	// peer. Synchronous failures are returned by the method that caused them and
	// do not appear here.
	//
	// The channel is closed by Stop. Consumers must handle the closed case and
	// must drain it: implementations drop rather than block if it is full.
	Errors() <-chan error

	// Stop takes the pipeline to NULL, releases the capture device and closes
	// the channel returned by Errors. It is idempotent.
	Stop() error
}
