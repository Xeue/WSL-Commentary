package remote

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Test 12: shim injection — the shim runs BEFORE the module bundle, and every
// other byte of the page is untouched
// ---------------------------------------------------------------------------

func TestInjectShim_InsertsBeforeModuleScript(t *testing.T) {
	out, err := injectShim([]byte(testIndexHTML))
	if err != nil {
		t.Fatalf("injectShim: %v", err)
	}
	s := string(out)

	shimAt := strings.Index(s, shimTag)
	modAt := strings.Index(s, moduleScriptMarker)
	if shimAt < 0 {
		t.Fatal("shim tag not present in injected index.html")
	}
	if modAt < 0 {
		t.Fatal("module script marker vanished from injected index.html")
	}
	if shimAt >= modAt {
		t.Fatalf("shim tag at %d is not before the module script at %d", shimAt, modAt)
	}
	// Exactly one injection.
	if strings.Count(s, shimTag) != 1 {
		t.Fatalf("shim tag appears %d times, want exactly 1", strings.Count(s, shimTag))
	}

	// Byte-identical apart from the insertion: removing the inserted run
	// (shimTag plus the readability whitespace) must yield the original.
	reconstructed := strings.Replace(s, shimTag+"\n    ", "", 1)
	if reconstructed != testIndexHTML {
		t.Fatalf("injection altered more than the insertion point.\n got: %q\nwant: %q", reconstructed, testIndexHTML)
	}
}

func TestInjectShim_NoMarkerReturnsOriginalAndError(t *testing.T) {
	// A page with no module script must be returned UNCHANGED with an error, so
	// the caller logs that the bridge will not load rather than shipping a
	// silently-broken page.
	orig := []byte("<html><head></head><body>no modules here</body></html>")
	out, err := injectShim(orig)
	if err == nil {
		t.Fatal("injectShim without a module script returned no error")
	}
	if !bytes.Equal(out, orig) {
		t.Fatal("injectShim without a marker altered the bytes")
	}
}

func TestAssetServer_GETSlashIsInjectedIndex(t *testing.T) {
	h := newHarness(t)
	resp, err := h.httpClient().Get("https://" + h.httpsAddr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	shimAt := strings.Index(s, shimTag)
	modAt := strings.Index(s, moduleScriptMarker)
	if shimAt < 0 || modAt < 0 || shimAt >= modAt {
		t.Fatalf("served index does not carry the shim before the module script (shim %d, module %d)", shimAt, modAt)
	}
}

func TestAssetServer_StaticPassthroughIsByteIdentical(t *testing.T) {
	h := newHarness(t)
	resp, err := h.httpClient().Get("https://" + h.httpsAddr + "/assets/index-B3qX8jaX.js")
	if err != nil {
		t.Fatalf("GET asset: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, testAssetBytes) {
		t.Fatalf("static asset was not served byte-identical.\n got: %q\nwant: %q", body, testAssetBytes)
	}
}

func TestAssetServer_ShimServedFromEmbedWithNoStore(t *testing.T) {
	h := newHarness(t)
	resp, err := h.httpClient().Get("https://" + h.httpsAddr + shimPath)
	if err != nil {
		t.Fatalf("GET shim: %v", err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("shim Cache-Control = %q, want no-store", cc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("shim Content-Type = %q, want a javascript type", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, shimSource) {
		t.Fatal("served shim.js differs from the embedded source")
	}
	// The shim must actually install what the frontend reaches for, or the whole
	// zero-frontend-change claim is hollow. A cheap source-text guard catches an
	// accidental rename of the contract points.
	for _, want := range []string{"window.go", "window.runtime", "EventsOn", "__wslremote/ws"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("shim.js does not mention %q", want)
		}
	}
}
