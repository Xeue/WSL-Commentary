package m2lx

import (
	"encoding/json"
	"sort"
	"strconv"
)

// Reading the conform format off the switcher
// -------------------------------------------
//
// M2L-X can be configured into ANY video format, and EVERY source feeding it
// must be produced in that format. Two consequences follow, and the second is
// the one this file exists for:
//
//   - a contribution pipeline that hard-codes 1920x1080p50 is a live defect on
//     any instance not configured for it. gst_cgo.go's slate capsfilter and
//     frontend/src/ui/lamps.js's GOOD_VIDEO both do exactly that today.
//   - the instance never states its configured format anywhere this
//     application can ask for it. There is no REST endpoint for it, and
//     switcher_status carries no instance-level format field either. What it
//     DOES carry is streams.video.format for every node — the DETECTED format
//     of whatever that node is actually receiving.
//
// So the format is derived rather than read: because every source must match,
// ANY NODE THAT IS RUNNING IS REPORTING THE TARGET. One streaming camera is
// enough. That is a weaker claim than "the switcher told us", and it is stated
// that way on purpose — see the disagreement handling below, which is what
// keeps it honest rather than merely convenient.
//
// TWO PROPERTIES OF THE WIRE ARE LOAD-BEARING HERE, both established in
// format.go and neither of them re-derived:
//
//   - frame_rate is a STRING ("50") while width and height beside it are
//     NUMBERS. parseVideoFormat is the only thing in this package that knows
//     that, and it is what is called below. Nothing here reads the raw fields.
//   - format is JSON null on every node that is not streaming — all 22 stopped
//     nodes in the 2026-07-31 capture. That is the NORMAL case, not an error
//     and not a fault to report: a stopped node has no detected format because
//     nothing is arriving at it. Such a node is stepped over in silence.
//
// WHOLE NODES ONLY. This reads a frame the way wire.go's lookupNode does: an
// entry at path "/" carries a node's whole state and nothing else does. The
// frame this is meant for is the opening snapshot from Watcher.RawSnapshot,
// every entry of which is at "/", so the restriction costs nothing there; and
// it means a "/statistics" delta — the one that already fooled an earlier
// parser into condemning the only working input on the switcher — can never be
// mistaken for a node with no format.

// ConformFormat is the video format every source feeding one M2L-X instance
// must be produced in, as read off the nodes that are actually running.
//
// The embedded VideoFormat is the format itself, with Raw carrying the
// rendering of everything the node reported — including the fields this
// application does not model, which is what makes a surprise visible instead
// of silently zero.
type ConformFormat struct {
	VideoFormat

	// Interlaced is true when the node reported scan_type "I" rather than the
	// measured "P".
	//
	// It is read here rather than added to VideoFormat because VideoFormat's
	// typed fields are what the three status lamps render, and a scan the lamps
	// do not use would be a JSON key on the "status" event that nothing reads.
	// The conform target IS a place it matters — internal/gst carries an
	// Interlaced flag whose stated job is to put a mismatch in the log, since
	// the video leg begins at pngdec and can only produce progressive — so it
	// is parsed at the one point that has a use for it.
	//
	// Whether an interlaced instance reports the FIELD rate or the FRAME rate
	// in frame_rate is UNMEASURED, and nothing here guesses: FrameRate is
	// whatever arrived.
	Interlaced bool

	// Node is the switcher_status node the format was read from: the first by
	// name of those that agree, so repeated reads of an unchanged switcher
	// name the same node and a log line does not appear to move about.
	Node string

	// Agreeing is how many running nodes reported this same raster and rate.
	// One is enough to act on — the premise is that every source matches — but
	// it is carried so a caller can say how much evidence it had.
	Agreeing int

	// Disagreeing names every OTHER running node whose raster or rate differs
	// from the one chosen, sorted, and is empty in the healthy case.
	//
	// It exists because the premise of this whole file — every source matches
	// the switcher — is an assumption about how the instance is configured,
	// not something the wire confirms. If two running nodes disagree then one
	// of them is not conforming, and the caller is entitled to know that before
	// it builds an encoder to the majority's answer. Swallowing it would turn
	// "the switcher is misconfigured" into "your feed looks wrong", which is
	// the wrong person's problem.
	Disagreeing []string
}

// String renders the format the way an operator reads a raster: 1920x1080p50.
//
// The rate is rendered with the smallest number of decimals that is exact, so
// 50 is "50" and 29.97 is "29.97" — a fixed %g would print 29.969999999999999
// for a rate that arrived as the string "29.97" and round-tripped through
// float64, and a fixed %.0f would print both 50 and 59.94 as whole numbers.
//
// Scan type is deliberately absent. It is in Raw, but it is not part of what
// this application conforms TO: the contribution pipeline deinterlaces and
// produces progressive whatever the switcher's own inputs are doing, so
// printing a "p" that came from somewhere else would be a claim about the
// switcher this package cannot make.
func (c ConformFormat) String() string {
	return strconv.Itoa(c.Width) + "x" + strconv.Itoa(c.Height) +
		"@" + strconv.FormatFloat(c.FrameRate, 'f', -1, 64)
}

// ConformFormatFrom reads the conform format out of one switcher_status frame.
//
// ok is false when nothing in the frame reports a usable format, which is the
// ordinary state of an instance with no source running and is NOT an error:
// every node is stopped, every format is null, and the caller falls back to
// its configured default. It is also false for a payload that is not a
// switcher_status frame at all — a distinction the caller does not need,
// because the answer is the same in both cases and there is nothing it could
// usefully do differently.
//
// A node counts only when it is STREAMING. StreamStateStarting is excluded
// deliberately: a node that is negotiating has been observed reporting a
// format, and a half-negotiated raster is exactly the kind of value that would
// make the encoder conform to a transient.
//
// A format counts only when width, height AND frame rate all parsed. A partial
// format cannot be conformed to — 1920x0 at 50 is not a raster — and leaving
// the missing field at a Go zero is how format.go warns that its typed fields
// must never be read as "good" without Raw beside them.
//
// When running nodes disagree the LARGEST agreeing group wins, ties broken by
// node name so the result is stable between calls. That is a deliberate choice
// of the least-bad answer rather than a refusal: refusing would fall back to
// the compiled-in 1080p50, which on a 720p50 instance is the very defect this
// function exists to remove, and which would be chosen on the evidence of ONE
// misconfigured node outvoting five correct ones. The disagreement is reported
// in Disagreeing rather than swallowed.
func ConformFormatFrom(payload []byte) (ConformFormat, bool) {
	entries, err := parseFrame(payload)
	if err != nil {
		return ConformFormat{}, false
	}

	groups := make(map[rasterKey]*ConformFormat, 4)
	total := 0

	for _, raw := range entries {
		name, node, ok := decodeStreamEntry(raw)
		if !ok || node.StreamState != StreamStateStreaming {
			continue
		}
		f := parseVideoFormat(node.Streams.Video.Format)
		if f.Width <= 0 || f.Height <= 0 || f.FrameRate <= 0 {
			// Either a stopped-shaped null that arrived on a node calling
			// itself streaming, or a format whose fields did not parse. Both
			// are stepped over; Raw already carries whatever it was, and this
			// function has no lamp to turn red.
			continue
		}
		total++

		k := rasterKey{f.Width, f.Height, f.FrameRate}
		g, seen := groups[k]
		if !seen {
			groups[k] = &ConformFormat{
				VideoFormat: f,
				Interlaced:  isInterlacedScan(node.Streams.Video.Format),
				Node:        name,
				Agreeing:    1,
			}
			continue
		}
		g.Agreeing++
		if name < g.Node {
			// Keep the first node BY NAME rather than the first seen: the
			// "status" array's order is the device's and has never been
			// promised to be stable, and a conform target that names a
			// different node on every read reads as instability that is not
			// there.
			g.Node = name
			g.VideoFormat = f
			g.Interlaced = isInterlacedScan(node.Streams.Video.Format)
		}
	}

	if len(groups) == 0 {
		return ConformFormat{}, false
	}

	best := pickConformGroup(groups)
	if best.Agreeing < total {
		for _, g := range groups {
			if g == best {
				continue
			}
			best.Disagreeing = append(best.Disagreeing, g.Node)
		}
		sort.Strings(best.Disagreeing)
	}
	return *best, true
}

// isInterlacedScan reads scan_type out of a format object.
//
// MEASURED as the single letter "P" on every healthy node in the 2026-07-31
// capture. No interlaced sample exists, so the test is deliberately narrow and
// fails PROGRESSIVE: only a value that begins with "i" or "I" is read as
// interlaced, and everything else — "P", a shape nobody has seen, an absent
// field — is not. That asymmetry is the safe one. The video leg can only
// produce progressive (see internal/gst's VideoFormat.Interlaced), so a false
// negative changes nothing at all, where a false positive would put a
// permanent "your switcher is interlaced and we cannot match it" line in the
// log of a facility whose switcher is nothing of the kind.
func isInterlacedScan(raw json.RawMessage) bool {
	fields, ok := formatFields(raw)
	if !ok {
		return false
	}
	s, ok := jsonString(fields["scan_type"])
	if !ok || s == "" {
		return false
	}
	return s[0] == 'i' || s[0] == 'I'
}

// rasterKey is one distinct raster-and-rate: the whole of what the contribution
// pipeline conforms to, and therefore the whole of what two running nodes have
// to match on to be counted as agreeing.
//
// Codec is deliberately not part of it. The contribution feed is H.264 whatever
// the switcher's other sources are carrying, so a node arriving as HEVC at the
// same raster is agreeing about the thing this derivation is for. FrameRate is
// a float64 map key, which is safe here only because parseFrameRate has already
// refused NaN — see format.go.
type rasterKey struct {
	width, height int
	frameRate     float64
}

// pickConformGroup returns the group with the most nodes in it, ties broken by
// the node name already chosen for each group — so the winner is a function of
// the frame's CONTENT and not of Go's map iteration order, which is randomised
// per run and would otherwise make this return a different raster on identical
// input.
func pickConformGroup(groups map[rasterKey]*ConformFormat) *ConformFormat {
	var best *ConformFormat
	for _, g := range groups {
		switch {
		case best == nil:
		case g.Agreeing > best.Agreeing:
		case g.Agreeing == best.Agreeing && g.Node < best.Node:
		default:
			continue
		}
		best = g
	}
	return best
}
