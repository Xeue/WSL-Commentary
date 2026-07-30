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
// # The pipeline this package builds
//
// Specification section 5, as one gst_parse_launch string:
//
//	mpegtsmux name=mux alignment=7 pcr-interval=3600
//	  ! queue name=srtq leaky=downstream max-size-buffers=4000
//	  ! srtsink name=srtout sync=false async=false auto-reconnect=false
//	filesrc location=<slate> ! pngdec ! imagefreeze is-live=true ! videoconvert
//	  ! video/x-raw,format=NV12,width=1920,height=1080,framerate=50/1,...
//	  ! mfh264enc name=venc bitrate=2000 rc-mode=cbr gop-size=100 bframes=0
//	    low-latency=true cabac=true
//	  ! h264parse config-interval=-1 ! queue ! mux.
//	wasapi2src name=asrc device=<endpoint id> low-latency=true
//	  ! audioconvert ! audioresample ! mfaacenc bitrate=128000
//	  ! aacparse ! queue ! mux.
//
// srtsink's own auto-reconnect is set to false and must stay false: on a write
// failure it reopens immediately with no backoff, retries once, and then errors
// the whole pipeline out — straight into M2L-X's roughly five second re-accept
// refusal window.
package gst

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
	// ID is the WASAPI IMMDevice endpoint ID GUID, e.g.
	// "{0.0.1.00000000}.{b3f8fa53-0004-438e-9003-51a46e139bfc}". This is what is
	// persisted to config.json and passed to wasapi2src's device property. It
	// survives a rename and sidesteps the double space in
	// "DVS Receive  1-2 (Dante Virtual Soundcard)" entirely.
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

	// AudioDeviceID is the IMMDevice endpoint ID GUID passed to wasapi2src's
	// device property. Required; it is Device.ID, never Device.Name.
	AudioDeviceID string

	// VideoBitrateKbps is mfh264enc's bitrate in kilobits per second. Zero means
	// DefaultVideoBitrateKbps.
	VideoBitrateKbps int

	// AudioBitrateBps is mfaacenc's bitrate in bits per second. Zero means
	// DefaultAudioBitrateBps.
	AudioBitrateBps int
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
	ReplaceSink(opts SinkOpts) error

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
