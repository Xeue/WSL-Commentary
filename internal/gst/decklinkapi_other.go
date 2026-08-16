//go:build cgo && !gststub && !darwin

// decklinkapi_other.go is the non-macOS half of the "the decklink plugin is
// loaded and found nothing — why" question, and it deliberately does no
// measurement at all.
//
// Owner: WP-3a. Read decklinkapi_darwin.go for what is being answered; this file
// is the statement that the interesting answer has no Windows equivalent.
//
// # There is no library validation to fail here
//
// The macOS failure is specific and it is a code-signing one: libgstdecklink
// dlopens a framework that Blackmagic signed, into a process that we signed,
// under a hardened runtime whose library validation refuses a Mach-O from
// another team. Windows has no analogue. The decklink plugin there reaches the
// hardware through COM — CoCreateInstance on a class the Desktop Video installer
// registers — and COM activation is not gated on the signer of the calling
// process. A machine with the driver installed can activate it; a machine
// without it cannot, and the plugin's own "no hardware" path is then the correct
// and complete story.
//
// So this reports NOTHING WORTH SAYING, always, and ListInputDevices logs
// nothing on this platform as a result — which is precisely the behaviour
// Windows had before any of this existed. "No cards on a machine with no card
// in it" is not a diagnosis, and inventing a line to carry it would put a
// permanent invitation to go hunting into every Windows log this product
// produces.
//
// Probing the COM server to say more was considered and rejected: it would mean
// an activation this package has no other reason to perform, on a path that
// already knows there are no cards, buying a distinction that has never been
// observed to matter.
package gst

// deckLinkAPIDiagnosis reports whether "the plugin is loaded and found no cards"
// is worth a log line on this platform. It never is; see the file comment, and
// decklinkapi_darwin.go for the twin that measures.
func deckLinkAPIDiagnosis() (string, bool) { return "", false }
