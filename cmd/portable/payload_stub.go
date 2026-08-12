//go:build !portable

package main

// payload is empty in an ordinary build.
//
// The real payload is a ~20 MB zip produced by build\pack-portable.ps1 and
// embedded by payload_embed.go under the 'portable' build tag. Keeping it
// behind a tag is what lets `go build ./...`, `go vet ./...` and the test suite
// run against a clean checkout: without this stub every one of them would fail
// on a missing cmd\portable\payload.zip, and the blob is far too large to keep
// in git.
//
// A launcher built without the tag compiles and runs, and says exactly what is
// wrong instead of misbehaving - see run() in main_windows.go.
var payload []byte
