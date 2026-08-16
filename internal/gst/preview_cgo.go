//go:build cgo && !gststub

// preview_cgo.go is the two things the confidence monitor cannot do in ordinary
// Go: ask the loaded GStreamer registry whether there is a video sink to draw
// in, and hand that sink the native surface to draw on.
//
// Read preview.go first. It carries the branch, the measurements, the naming
// rule the bus filter depends on and the argument for every one of them; this
// file is the seam and nothing else, exactly as picture_cgo.go is picture.go's.
//
// # It borrows the picture path's sink decision rather than making a second one
//
// pictureSinkCandidates in picture_cgo.go already answers "which element renders
// video into a native surface on this platform", with the proof behind it:
// d3d11videosink on Windows, taking the decoder's D3D11Memory without a copy;
// glimagesink on macOS, which is the one that was DRIVEN in a real Wails NSView
// — 400 buffers, repositioned mid-playback, a GstGLNSView appearing inside our
// view — with osxvideosink kept below it as a fallback that says so in the log.
//
// Asking that question twice would be two lists to keep in step, and the second
// one would be the one nobody measured. The preview's surface is the same kind
// of surface, over the same webview, on the same window; if the answer is ever
// different for the two of them, that is a finding that belongs in picture_cgo.go
// where the measurements are.
//
// What the preview deliberately does NOT share is the DECODER list, because it
// has no decoder: it is handed raw video off a tee that the pipeline has already
// conformed, which is most of why it costs single-figure percentages of a core.
//
// # The one call go-gst does not have, again
//
// gst_video_overlay_set_window_handle is not bound by go-gst v0.0.2 — girgen
// skips it, presumably over the guintptr argument — and there is no property
// equivalent on either sink. picture_cgo.go declares its own C wrapper for the
// same reason, and this file declares a SECOND one rather than calling that one,
// because a static function in one cgo file's preamble is not visible from
// another's. Two three-line wrappers is the cost of the file split; sharing would
// mean moving the declaration into a header or making one of the two paths
// depend on the other's internals, and neither is worth it for six lines of C.
//
// If it is not set, the sink creates its OWN TOP-LEVEL WINDOW: a second window
// the operator never asked for, outside the layout, with a title bar and a close
// button, on top of a commentary position during a match. That is why
// PreviewOpts.WindowHandle being zero means the branch is not built at all
// rather than built and left unattached, and why the failure to attach one is
// treated by Start as a reason to abort before anything reaches PLAYING.
package gst

/*
#cgo pkg-config: gstreamer-video-1.0

#include <gst/gst.h>
#include <gst/video/videooverlay.h>

// wslcomms_preview_set_window_handle is the GST_VIDEO_OVERLAY cast applied by
// the C compiler rather than open-coded in Go. The macro is a checked cast in a
// debug build of GLib and a plain one otherwise, so going through it is what
// makes a wrong element type a legible GLib critical instead of a jump through a
// garbage vtable.
static void wslcomms_preview_set_window_handle(gpointer sink, guintptr handle) {
    gst_video_overlay_set_window_handle(GST_VIDEO_OVERLAY(sink), handle);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// resolvePreviewSink returns the video sink factory the preview branch will be
// built with, or an error saying what the bundle is short of.
//
// The error is a SENTENCE and not a wrapped sentinel, because there is exactly
// one caller and what it does with it is print it. It reads as the tail of
// previewBranchFor's log line — "the confidence monitor is switched on and
// <this>" — so it is phrased as a clause rather than as a heading.
//
// A missing sink here is a bundling mistake and nothing else: every candidate
// ships with GStreamer and is present on any machine that has it installed. The
// message therefore names the bundler script, exactly as the picture path's
// does, because that is the file somebody has to edit.
func resolvePreviewSink() (string, error) {
	candidates := pictureSinkCandidates()
	if len(candidates) == 0 {
		return "", fmt.Errorf("this application has no video sink for %s at all, so there is "+
			"nowhere to render a preview", runtime.GOOS)
	}
	for i, c := range candidates {
		if gogst.ElementFactoryFind(c.factory) == nil {
			continue
		}
		if i > 0 {
			// The same fallback the picture path takes, logged for the same
			// reason: it is a working picture and a worse one, and this is the
			// only place it can ever be noticed.
			log.Printf("gst: preview: %s is not in this build's GStreamer; the confidence monitor "+
				"is using %s (plugin %s) instead, which is the fallback rather than the sink this "+
				"application was measured on", candidates[0].factory, c.factory, c.plugin)
		}
		return c.factory, nil
	}

	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, fmt.Sprintf("%s (plugin %s)", c.factory, c.plugin))
	}
	return "", fmt.Errorf("none of %s is in this build's GStreamer, so there is nothing to render "+
		"it with. That is a bundling mistake rather than a machine fault — every one of them ships "+
		"with GStreamer — and the plugin has to be added to %s",
		strings.Join(names, " or "), pictureBundler())
}

// attachPreview finishes the preview sink off: the properties that could not
// safely go in the parse string, and the native surface.
//
// IT MUST BE CALLED BEFORE THE PIPELINE LEAVES NULL. The window handle has to be
// set before the sink ever needs somewhere to draw, or it makes its own
// top-level window; see the file header. It is the same order picture_cgo.go's
// buildLocked uses, and it is the documented and well-travelled one.
//
// # Every error it returns means the graph is not what was just asked for
//
// The branch was rendered by this package, into a string this package built,
// parsed a few lines earlier in Start. So a missing element or a sink with no
// underlying GObject is not a condition to degrade around — it is a
// contradiction, and the honest answer is to abort the Start and let the caller
// rebuild WITHOUT the preview, which it does. Carrying on would leave an
// unattached GstVideoOverlay in a pipeline about to go PLAYING, which is the one
// outcome this path must never produce: a bare GStreamer window over the
// commentator's screen, mid-match, that nothing in this application knows how to
// move or close.
//
// It returns nil and does nothing at all when there is no preview branch, so the
// caller does not have to ask twice.
func attachPreview(pipeline gogst.Pipeline, opts PreviewOpts, branch string) error {
	if branch == "" {
		return nil
	}
	if pipeline == nil {
		return errors.New("gst: preview: there is no pipeline to attach the confidence monitor to")
	}

	sink := pipeline.GetByName(namePreviewSink)
	if sink == nil {
		return errors.New("gst: preview: the parsed pipeline has no element named " + namePreviewSink +
			", so the confidence monitor's sink cannot be given a surface and would open a window of " +
			"its own")
	}

	// force-aspect-ratio. The rectangle the page gives the overlay is whatever
	// the layout produced and the picture is 16:9; letterboxing inside the
	// rectangle is right and stretching is not. It matters MORE here than on the
	// picture path, not less: this monitor exists to be compared against what the
	// card is seeing, and a stretched comparison is a comparison of the wrong
	// thing.
	if hasProperty(sink, "force-aspect-ratio") {
		sink.SetObjectProperty("force-aspect-ratio", true)
	}

	// Navigation events off, in both of the two spellings the sinks use:
	// d3d11videosink calls it enable-navigation-events, and the GL and macOS
	// sinks inherit handle-events from GstVideoOverlay's helpers. Neither is
	// present on the other's element, which is what these guards are for.
	//
	// It is not cosmetic. The sink draws in a native surface sitting OVER the
	// page; turning mouse movement into upstream navigation events sends them to
	// a decklinkvideosrc that has nothing to do with them, and it is one more
	// reason for the sink to want the input focus it must never take from the
	// page.
	if hasProperty(sink, "enable-navigation-events") {
		sink.SetObjectProperty("enable-navigation-events", false)
	}
	if hasProperty(sink, "handle-events") {
		sink.SetObjectProperty("handle-events", false)
	}

	ptr := gogst.UnsafeElementToGlibNone(sink)
	if ptr == nil {
		return errors.New("gst: preview: " + namePreviewSink + " has no underlying GObject, so the " +
			"window handle cannot be set and the sink would open a window of its own")
	}
	C.wslcomms_preview_set_window_handle(C.gpointer(ptr), C.guintptr(opts.WindowHandle))

	// There is deliberately NO belt to this brace, unlike the picture path, which
	// also answers the prepare-window-handle element message on its bus. That
	// belt exists there because the picture pipeline is rebuilt on every
	// reconnect — dozens of times in a bad half — and the message is the only
	// signal that a rebuild raced the handle. This branch is built once, at
	// Start, before the pipeline has left NULL, and the contribution pipeline's
	// bus handler runs on streaming threads carrying level messages twenty times
	// a second. Adding a string comparison to that path for a message that
	// cannot arrive is a cost on the on-air path in exchange for nothing.
	log.Printf("gst: preview: %s is rendering into surface 0x%x", namePreviewSink, opts.WindowHandle)
	return nil
}
