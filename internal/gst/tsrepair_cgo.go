//go:build cgo && !gststub

package gst

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>

// wslcomms_tsrepair_map makes the buffer a BUFFER pad probe carries writable
// — in place when the pad's reference is the only one, which an srtsrc or
// filesrc buffer's is, and by a metadata-preserving copy otherwise — puts it
// back into the probe, and maps it for writing. Returns the buffer, or NULL
// when there is none or it would not map; the caller unmaps it with
// wslcomms_tsrepair_unmap. This is the documented way to modify a buffer from
// a probe: gst_buffer_make_writable takes over info->data's reference and
// GST_PAD_PROBE_INFO_DATA is pointed at what it returns.
static GstBuffer *wslcomms_tsrepair_map(gpointer info_ptr, GstMapInfo *map) {
    GstPadProbeInfo *info = (GstPadProbeInfo *)info_ptr;
    GstBuffer *b = GST_PAD_PROBE_INFO_BUFFER(info);
    if (!b) return NULL;
    b = gst_buffer_make_writable(b);
    GST_PAD_PROBE_INFO_DATA(info) = b;
    if (!gst_buffer_map(b, map, GST_MAP_WRITE)) return NULL;
    return b;
}

static void wslcomms_tsrepair_unmap(GstBuffer *b, GstMapInfo *map) {
    gst_buffer_unmap(b, map);
}
*/
import "C"

import (
	"sync/atomic"
	"unsafe"

	gogst "github.com/go-gst/go-gst/pkg/gst"
)

// tsRepairProbe returns a BUFFER probe that runs RepairZeroPayloadPackets over
// every buffer crossing the pad, in place, adding what it repaired to repaired.
// It belongs on the pad that carries the raw transport stream into tsdemux:
// srtsrc's src pad in the picture path, the source's in the dissector.
func tsRepairProbe(repaired *atomic.Uint64) func(gogst.Pad, *gogst.PadProbeInfo) gogst.PadProbeReturn {
	return func(_ gogst.Pad, info *gogst.PadProbeInfo) gogst.PadProbeReturn {
		var m C.GstMapInfo
		b := C.wslcomms_tsrepair_map(C.gpointer(gogst.UnsafePadProbeInfoToGlibNone(info)), &m)
		if b == nil {
			return gogst.PadProbeOK
		}
		if m.size > 0 {
			data := unsafe.Slice((*byte)(unsafe.Pointer(m.data)), int(m.size))
			if n := RepairZeroPayloadPackets(data); n > 0 {
				repaired.Add(uint64(n))
			}
		}
		C.wslcomms_tsrepair_unmap(b, &m)
		return gogst.PadProbeOK
	}
}
