package remote

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test 11: the generated cert covers the bind IP, localhost and the hostname,
// and the reported fingerprint is the fingerprint of the leaf actually served
// ---------------------------------------------------------------------------

func TestEnsureCertificate_CoversBindLocalhostAndHostname(t *testing.T) {
	dir := t.TempDir()
	const bind = "192.0.2.5"
	cert, fp, err := EnsureCertificate(dir, bind)
	if err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if !certCoversIP(leaf, bind) {
		t.Errorf("cert does not cover the bind IP %s (SANs %v)", bind, leaf.IPAddresses)
	}
	if !certCoversIP(leaf, "127.0.0.1") {
		t.Errorf("cert does not cover loopback (SANs %v)", leaf.IPAddresses)
	}
	if !hasDNS(leaf, "localhost") {
		t.Errorf("cert does not name localhost (DNS %v)", leaf.DNSNames)
	}
	if host, _ := os.Hostname(); host != "" && !hasDNS(leaf, host) {
		t.Errorf("cert does not name the hostname %q (DNS %v)", host, leaf.DNSNames)
	}

	// The returned fingerprint is the fingerprint of the served leaf.
	if got := CertificateFingerprint(cert); got != fp {
		t.Errorf("CertificateFingerprint = %q, returned fp = %q", got, fp)
	}
	if want := fingerprintDER(cert.Certificate[0]); fp != want {
		t.Errorf("returned fp %q does not match the leaf DER fingerprint %q", fp, want)
	}
}

func TestEnsureCertificate_PersistsAndIsReused(t *testing.T) {
	dir := t.TempDir()
	const bind = "127.0.0.1"
	_, fp1, err := EnsureCertificate(dir, bind)
	if err != nil {
		t.Fatalf("first EnsureCertificate: %v", err)
	}
	// The files exist on disk.
	certPath, keyPath := certPaths(dir)
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert not persisted: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key not persisted: %v", err)
	}
	// A second call with the same bind reuses the same cert, not a fresh one.
	_, fp2, err := EnsureCertificate(dir, bind)
	if err != nil {
		t.Fatalf("second EnsureCertificate: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("cert regenerated on reuse: fp1 %q, fp2 %q", fp1, fp2)
	}
}

func TestEnsureCertificate_RegeneratesWhenBindNoLongerCovered(t *testing.T) {
	dir := t.TempDir()
	_, fpA, err := EnsureCertificate(dir, "192.0.2.5")
	if err != nil {
		t.Fatalf("EnsureCertificate A: %v", err)
	}
	// A different, uncovered bind forces a regeneration whose cert covers it.
	certB, fpB, err := EnsureCertificate(dir, "198.51.100.9")
	if err != nil {
		t.Fatalf("EnsureCertificate B: %v", err)
	}
	if fpA == fpB {
		t.Error("cert was not regenerated for an uncovered bind")
	}
	leafB, _ := x509.ParseCertificate(certB.Certificate[0])
	if !certCoversIP(leafB, "198.51.100.9") {
		t.Errorf("regenerated cert does not cover the new bind (SANs %v)", leafB.IPAddresses)
	}
}

func TestEnsureCertificate_ValidityIsBounded(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := EnsureCertificate(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	// Roughly 825 days plus the one-hour skew allowance; assert it is in the
	// right ballpark and not, say, 100 years.
	if life < 800*24*time.Hour || life > 830*24*time.Hour {
		t.Errorf("certificate lifetime = %v, want ~825 days", life)
	}
}

// TestFingerprint_MatchesServedLeaf ties the number Settings would print to the
// leaf a client actually receives on the wire, over the running server.
func TestFingerprint_MatchesServedLeaf(t *testing.T) {
	h := newHarness(t)
	conn, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()
	served := conn.ConnectionState().PeerCertificates
	if len(served) == 0 {
		t.Fatal("server presented no certificate")
	}
	got := fingerprintDER(served[0].Raw)
	if got != h.srv.Fingerprint() {
		t.Fatalf("served leaf fingerprint %q != reported %q", got, h.srv.Fingerprint())
	}
}

func hasDNS(leaf *x509.Certificate, name string) bool {
	for _, d := range leaf.DNSNames {
		if d == name {
			return true
		}
	}
	return false
}
