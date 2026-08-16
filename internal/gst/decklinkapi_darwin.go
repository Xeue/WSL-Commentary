//go:build cgo && !gststub && darwin

// decklinkapi_darwin.go answers ONE question, on a path that has already
// established something is wrong: the decklink plugin is loaded and enumerated
// no cards — is that because there is no card, or because this process was not
// allowed to load Blackmagic's API at all?
//
// Owner: WP-3a, with build/darwin/wslcomms.entitlements, which is where the fix
// lives and which must be read beside this file. There is no Windows twin doing
// anything: see deviceprovider_windows.go's stub for why the failure mode does
// not exist there.
//
// # The measurement this file exists to make legible
//
// A fitted, working UltraStudio 4K Mini was absent from the commentary input
// list of the signed, notarised bundle and present in every unsigned build and
// in gst-device-monitor-1.0 on the same machine. GStreamer said nothing at any
// debug level, because from its point of view nothing failed: the plugin
// registered, the device provider registered, the provider probed, and the probe
// found zero cards. The whole diagnosis was in the kernel log, which no operator
// and no support engineer is ever going to look at:
//
//	AppleMobileFileIntegrity: Library Validation failed: Rejecting
//	'/Library/Frameworks/DeckLinkAPI.framework/Versions/A/DeckLinkAPI'
//	(Team ID: 9ZGFBWLSYP, platform: no) for process 'wslcomms' (Team ID:
//	5P76UVY5WF, platform: no), reason: mapping process and mapped file
//	(non-platform) have different Team IDs
//
// libgstdecklink.dylib does not link DeckLinkAPI — its only reference to it is
// the string "/Library/Frameworks/DeckLinkAPI.framework", loaded through
// CFBundle at first use — so the hardened runtime's library validation refuses
// it silently at the first Create*Instance call, and the plugin's own
// "no hardware" path takes over. A Blackmagic-signed framework is a DIFFERENT
// TEAM from ours by construction and always will be; the only waiver Apple
// offers is com.apple.security.cs.disable-library-validation, which the
// entitlements file now takes and explains.
//
// # Why the probe repeats a dlopen the plugin has already done
//
// Because the plugin's answer is a device count and this one is a REASON. The
// two failures that produce an identical empty list — a driver installed with no
// card in the slot, and a card in the slot that the process may not reach — want
// completely different things from the person reading the log, and nothing else
// in the process can tell them apart.
//
// It costs one dlopen, on a path that has already found no cards, at most once
// per enumeration. If the framework IS loadable, dlopen finds it already
// resident (the plugin loaded it moments ago), returns the same handle and does
// no work. The handle is deliberately NOT closed: unloading a framework the
// decklink plugin is holding open is a way to turn a diagnostic into a crash,
// and dlclose on an already-resident library is refcounted anyway.
package gst

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"
)

// deckLinkAPIPath is the Mach-O inside Blackmagic's Desktop Video framework, by
// the same absolute path libgstdecklink.dylib has compiled into it. Verified
// with `strings` on the bundled plugin: the framework directory is the only
// DeckLink path it carries.
//
// It is written out in full rather than assembled from parts because it is a
// string to be GREPPED — for in this source, in a log line, and in the kernel's
// AMFI message, which all have to be recognisably about the same file.
const deckLinkAPIPath = "/Library/Frameworks/DeckLinkAPI.framework/Versions/A/DeckLinkAPI"

// deckLinkAPIDiagnosis reports whether the enumeration finding no cards is worth
// a log line, and if so what that line should say.
//
// Three outcomes, and only ONE of them is worth telling anybody about. The
// silence in the other two is the point: ListInputDevices calls this on every
// enumeration on every Mac, and a line that appears on machines where nothing is
// wrong is a line that sends somebody looking for hardware that was never
// fitted.
//
//	the framework is not on disk   SILENT. Desktop Video is not installed, so
//	                               there is no card and never was. The plugin
//	                               registering anyway is a macOS quirk (it has no
//	                               link-time dependency on the API), not a fault.
//
//	dlopen failed                  LOUD, and this is the whole reason the file
//	                               exists. dlerror is quoted verbatim: on the
//	                               library-validation refusal it contains the
//	                               words "different Team IDs", which is the
//	                               entire diagnosis and appears nowhere else the
//	                               application can see.
//
//	dlopen succeeded               SILENT. The process CAN reach the API, so an
//	                               empty list means exactly what it says — no
//	                               card is plugged in — and that is not news.
func deckLinkAPIDiagnosis() (string, bool) {
	if _, err := os.Stat(deckLinkAPIPath); err != nil {
		return "", false
	}

	cpath := C.CString(deckLinkAPIPath)
	defer C.free(unsafe.Pointer(cpath))

	// RTLD_LAZY|RTLD_LOCAL: the lightest load there is, and LOCAL so nothing
	// this probe does can change symbol resolution for the plugin that owns the
	// real handle. The handle is deliberately not closed — see the file comment.
	if h := C.dlopen(cpath, C.RTLD_LAZY|C.RTLD_LOCAL); h == nil {
		return fmt.Sprintf("%s IS installed but this process cannot load it: %s. If that mentions "+
			"Team IDs, it is the hardened runtime's LIBRARY VALIDATION refusing a framework signed "+
			"by Blackmagic into a process signed by us — the card is fine, and the fix is the "+
			"com.apple.security.cs.disable-library-validation entitlement in "+
			"build/darwin/wslcomms.entitlements, not the hardware",
			deckLinkAPIPath, C.GoString(C.dlerror())), true
	}

	return "", false
}
