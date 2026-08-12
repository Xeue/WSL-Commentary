//go:build portable

package main

import _ "embed"

// payload is the whole application - wslcomms.exe, the 15 GStreamer plugins,
// the 35 runtime DLLs, slate.png and the licence texts - as a zip.
//
// build\pack-portable.ps1 writes cmd\portable\payload.zip from a verified
// build\bin and then builds this package with -tags portable. The file is
// generated, is around 20 MB, and is git-ignored; see payload_stub.go for why
// the tag exists.
//
//go:embed payload.zip
var payload []byte
