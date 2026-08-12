package remote

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"
)

// This file serves the remote page from the SAME embed.FS the Wails asset
// server serves the local window from, so the remote UI is byte-identical to the
// local one and there is no second frontend to keep in step. The only change is
// a single classic <script> injected immediately before the deferred module
// bundle in index.html. Because module scripts are deferred, that classic script
// is GUARANTEED to run first — which is the whole reason
// `export const usingFakeBackend = !hasWails()`, evaluated at backend.js module
// scope, can be made false by the time it runs without touching backend.js.

// shimSource is the transport shim, embedded from shim.js in this package (NOT
// from the frontend tree). It is the entire frontend side of the bridge and is
// served verbatim at shimPath.
//
//go:embed shim.js
var shimSource []byte

// moduleScriptMarker is the substring the shim tag is inserted BEFORE. The
// built index.html loads the bundle as `<script type="module" crossorigin
// src="/assets/index-*.js">`; matching on the type attribute rather than the
// hashed src means a new build (which changes the hash) does not break the
// injection. If this marker is ever not found the page is served unmodified and
// a line is logged, because serving the local page without the shim is a
// visible, debuggable failure, whereas refusing to serve it at all is not.
const moduleScriptMarker = `<script type="module"`

// shimTag is the classic script inserted ahead of the module bundle. It is a
// non-module script on purpose: a classic script with no defer/async runs
// synchronously at the point it appears in the document, before any deferred
// module, so window.go and window.runtime exist before backend.js is evaluated.
const shimTag = `<script src="` + shimPath + `"></script>`

// assetServer serves the injected index and the untouched remainder of the
// embedded tree. It is App-agnostic: it is handed an fs.FS whose root contains
// index.html (the caller does the fs.Sub to strip the embed's frontend/dist
// prefix), and it neither knows nor cares what application produced it.
type assetServer struct {
	fsys      fs.FS
	fileSrv   http.Handler
	logf      func(string, ...any)
	indexOnce []byte // cached injected index.html; computed lazily, then reused
}

// newAssetServer builds the handler over dist. It does not read index.html up
// front; the first GET / computes the injected page and caches it, so a broken
// or missing index surfaces as a request-time 500 rather than a construction
// error that would take the whole listener down.
func newAssetServer(dist fs.FS, logf func(string, ...any)) *assetServer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &assetServer{
		fsys:    dist,
		fileSrv: http.FileServer(http.FS(dist)),
		logf:    logf,
	}
}

// ServeHTTP routes the shim, the index (with injection) and everything else
// (byte-for-byte from the embedded tree).
func (s *assetServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The shim is served from THIS package's embed, not the frontend tree, with
	// no-store caching: it changes with the Go binary, not with the frontend
	// build, and must never be served stale from a browser cache after an
	// upgrade that changed the protocol.
	if r.URL.Path == shimPath {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(shimSource)
		return
	}

	// GET / and /index.html get the injected page. Every other path — the
	// hashed JS/CSS bundles, fonts, images — passes straight through the file
	// server unchanged, so only the one HTML document is ever rewritten.
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		s.serveIndex(w, r)
		return
	}

	s.fileSrv.ServeHTTP(w, r)
}

// serveIndex writes the shim-injected index.html, caching the injected bytes on
// first use.
func (s *assetServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	if s.indexOnce == nil {
		injected, err := s.buildIndex()
		if err != nil {
			s.logf("remote: serving index.html: %v", err)
			http.Error(w, "index unavailable", http.StatusInternalServerError)
			return
		}
		s.indexOnce = injected
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page itself is not cached: it is tiny, and caching it would let a
	// browser keep a pre-upgrade page (with a stale bundle hash) after the app
	// is updated.
	w.Header().Set("Cache-Control", "no-store")
	// A zero modtime tells ServeContent to emit no Last-Modified header and skip
	// conditional-request handling, which is exactly right for a no-store page.
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexOnce))
}

// buildIndex reads index.html from the embedded tree and inserts the shim tag
// immediately before the module bundle.
func (s *assetServer) buildIndex() ([]byte, error) {
	f, err := s.fsys.Open("index.html")
	if err != nil {
		return nil, fmt.Errorf("opening index.html: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading index.html: %w", err)
	}
	return injectShim(raw)
}

// injectShim inserts shimTag immediately before the first module-script tag.
//
// It is a pure function of the input bytes so it can be unit-tested without a
// server, and it changes nothing but the one insertion: the returned bytes are
// the original with exactly shimTag (plus a newline for readability) spliced in
// ahead of the module script. If the marker is absent the original is returned
// unchanged together with an error, so the caller can log that the page will
// load WITHOUT the bridge rather than silently shipping a dead page.
func injectShim(indexHTML []byte) ([]byte, error) {
	idx := bytes.Index(indexHTML, []byte(moduleScriptMarker))
	if idx < 0 {
		return indexHTML, fmt.Errorf("index.html has no %q; shim not injected", moduleScriptMarker)
	}
	var b bytes.Buffer
	b.Grow(len(indexHTML) + len(shimTag) + 8)
	b.Write(indexHTML[:idx])
	b.WriteString(shimTag)
	b.WriteString("\n    ")
	b.Write(indexHTML[idx:])
	return b.Bytes(), nil
}
