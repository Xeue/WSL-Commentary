package tsprobe

import "time"

// Verdict is the probe's one-word reading of the video PID, in the order the
// text verdict tests them: the first condition that holds is the verdict.
type Verdict string

const (
	// VerdictNoVideo: the PMT never named an HEVC/H.264 PID, or nothing arrived.
	VerdictNoVideo Verdict = "no-video"

	// VerdictDTSBackwards: the video PID's decode timestamp went backwards —
	// the sender-side pipeline-restart failure, not a decode fault.
	VerdictDTSBackwards Verdict = "dts-backwards"

	// VerdictRealHoles: unflagged continuity errors on the video PID — genuine
	// gaps in the elementary stream as received. A tear source, upstream of
	// the decoder.
	VerdictRealHoles Verdict = "real-holes"

	// VerdictFlaggedOnly: continuity jumps, every one carrying a
	// discontinuity_indicator. Legal, intentional, and the source of tsdemux's
	// "CONTINUITY: Mismatch" noise. The bytes are intact.
	VerdictFlaggedOnly Verdict = "flagged-only"

	// VerdictClean: no continuity errors of any kind on the video PID.
	VerdictClean Verdict = "clean"
)

// Summary is the probe's report as numbers, for a caller that has to reason
// about it rather than read it — the field rig. Every number here is also in
// the text of Report; this is the same reading, typed.
type Summary struct {
	Bytes uint64
	Span  time.Duration // first byte to last byte
	Mbps  float64

	VideoPID   int    // -1 when the PMT never named one
	VideoCodec string // "hevc", "h264" or ""

	// Continuity on the video PID. See the file header of analyze.go for what
	// REAL, FLAGGED and duplicate mean and why the split is the measurement.
	RealCC    uint64
	FlaggedCC uint64
	DupCC     uint64
	TEI       uint64

	// DTSBackwards is how many times the video PID's decode timestamp stepped
	// backwards.
	DTSBackwards uint64

	NALUnits  uint64
	IDRFrames uint64

	Verdict Verdict
}

// Summary reads the analyzer's state as a Summary. Safe to call at any time,
// including from a goroutine other than the one writing.
func (a *Analyzer) Summary() Summary {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := Summary{
		Bytes:     a.bytesTotal,
		Span:      a.lastAt - a.firstAt,
		VideoPID:  a.videoPID,
		NALUnits:  a.video.nalTotal,
		IDRFrames: a.video.idrCount,
	}
	if s.Span > 0 {
		s.Mbps = float64(a.bytesTotal) * 8 / s.Span.Seconds() / 1e6
	}
	vps := a.pids[a.videoPID]
	if a.videoPID < 0 || vps == nil {
		s.Verdict = VerdictNoVideo
		return s
	}
	switch vps.streamType {
	case streamTypeHEVC:
		s.VideoCodec = "hevc"
	case streamTypeH264:
		s.VideoCodec = "h264"
	}
	s.RealCC, s.FlaggedCC, s.DupCC, s.TEI = vps.ccReal, vps.ccFlagged, vps.ccDup, vps.tei
	s.DTSBackwards = vps.dtsBack
	switch {
	case vps.dtsBack > 0:
		s.Verdict = VerdictDTSBackwards
	case vps.ccReal > 0:
		s.Verdict = VerdictRealHoles
	case vps.ccFlagged > 0:
		s.Verdict = VerdictFlaggedOnly
	default:
		s.Verdict = VerdictClean
	}
	return s
}
