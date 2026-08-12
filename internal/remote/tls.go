package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TLS is load-bearing here, not decorative. Over plain HTTP a LAN page is not a
// secure context, so navigator.mediaDevices is undefined (the headphone picker
// renders empty) and setSinkId is missing (return audio plays on the wrong
// device); and GetKVSCredentials returns live AWS session credentials that must
// not cross a facility LAN in clear. So the listener is HTTPS-only, and because
// a venue rarely has a CA that will issue for a machine's LAN IP, this package
// mints and persists its own self-signed certificate, whose SHA-256 fingerprint
// Settings prints so an operator can verify the box they reached is the box they
// meant to reach.

const (
	// certFileName / keyFileName are the PEM files kept beside remote.json. The
	// key is written 0600; both are regenerated when the bind IP changes and the
	// stored cert no longer covers it.
	certFileName = "cert.pem"
	keyFileName  = "key.pem"

	// certValidity is the certificate's lifetime. 825 days is the longest a
	// publicly-trusted cert may live under the CA/Browser Forum baseline; there
	// is no such rule for a self-signed LAN cert, but staying within it keeps the
	// habit and means the cert expires on a sane schedule rather than never.
	certValidity = 825 * 24 * time.Hour
)

// certPaths returns the cert and key file paths inside dir.
func certPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, certFileName), filepath.Join(dir, keyFileName)
}

// EnsureCertificate loads the persisted self-signed certificate from dir, or
// generates and persists a fresh one, returning the certificate ready for a
// tls.Config together with its SHA-256 fingerprint for display.
//
// Because the listener binds 0.0.0.0 and remote browsers reach it by whatever
// LAN IP the commentary PC answers on, the cert must name EVERY such address or
// the browser adds a name-mismatch error on top of the unavoidable self-signed
// warning — a second, more alarming click-through. So the SANs are the union of
// every non-loopback interface IP (net.InterfaceAddrs), loopback (v4 and v6),
// "localhost", the hostname, and — when a specific non-wildcard bind is
// configured — that bind IP too. bindIP may be "0.0.0.0" (the default), in which
// case it contributes nothing and the interface IPs carry the cert.
//
// It regenerates when there is no stored cert, when it cannot be parsed, when it
// has expired, or — the case that matters operationally — when the machine's set
// of interface IPs has GROWN and the stored cert no longer covers an address a
// client could now dial. It does NOT regenerate merely because an interface
// disappeared: a cert that names an address nothing answers on is harmless.
func EnsureCertificate(dir, bindIP string) (tls.Certificate, string, error) {
	want := certIPs(bindIP)
	certPath, keyPath := certPaths(dir)
	if cert, fp, ok := loadIfUsable(certPath, keyPath, want); ok {
		return cert, fp, nil
	}
	return generateAndPersist(dir, want)
}

// certIPs is the set of IP addresses the served certificate must name: every
// non-loopback interface IP, both loopbacks, and the configured bind when it is
// a specific (non-wildcard) address. Duplicates are removed; a machine's own IPs
// naturally overlap loopback only if it is oddly configured, and dedupeIPs
// tolerates it.
func certIPs(bindIP string) []net.IP {
	ips := interfaceIPs()
	ips = append(ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	if ip := net.ParseIP(strings.TrimSpace(bindIP)); ip != nil && !ip.IsUnspecified() {
		ips = append(ips, ip)
	}
	return dedupeIPs(ips)
}

// interfaceIPs returns every non-loopback, non-wildcard IP currently assigned to
// a network interface — the LAN addresses a remote browser might dial. A failure
// to enumerate is not fatal: the caller still has the loopbacks, and a cert that
// covers fewer addresses only costs an extra browser warning, never correctness.
func interfaceIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// loadIfUsable tries to load an existing cert/key pair and reports whether it is
// usable right now: parseable, unexpired, and covering every IP in want. Any
// reason it is not returns ok=false so the caller regenerates, rather than an
// error — a stale cert is a normal condition to recover from, not a failure to
// report.
func loadIfUsable(certPath, keyPath string, want []net.IP) (tls.Certificate, string, bool) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return tls.Certificate{}, "", false
	}
	for _, ip := range want {
		if !certCoversIP(leaf, ip.String()) {
			return tls.Certificate{}, "", false
		}
	}
	cert.Leaf = leaf
	return cert, fingerprintDER(cert.Certificate[0]), true
}

// certCoversIP reports whether leaf lists ip among its IP SANs. Loopback is
// always acceptable regardless of the configured bind (the local operator and
// the developer both reach the listener as 127.0.0.1), which is why a loopback
// bind never forces a regeneration.
func certCoversIP(leaf *x509.Certificate, ip string) bool {
	want := net.ParseIP(ip)
	if want == nil {
		return false
	}
	for _, got := range leaf.IPAddresses {
		if got.Equal(want) {
			return true
		}
	}
	return false
}

// generateAndPersist mints a fresh ECDSA P-256 self-signed certificate covering
// the given IP set, "localhost" and the machine's hostname, writes it atomically
// alongside remote.json, and returns it with its fingerprint.
//
// P-256 rather than RSA is chosen because the keygen is near-instant — this runs
// on first enable, off the Wails main thread, and must not be a perceptible
// stall — and because a modern browser accepts it without complaint once the
// issuer is trusted.
func generateAndPersist(dir string, ips []net.IP) (tls.Certificate, string, error) {
	if len(ips) == 0 {
		// Should not happen — certIPs always adds the loopbacks — but a cert with
		// no IP SANs would be useless, so refuse rather than mint one.
		return tls.Certificate{}, "", fmt.Errorf("remote: no IP addresses to put in the certificate")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: generating serial: %w", err)
	}

	// SANs. The caller has already assembled the IP set — every LAN interface IP
	// a remote browser might dial, plus both loopbacks and any specific bind;
	// "localhost" and the hostname are the names either the operator or a
	// developer might type instead of an address.
	dns := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "wslcomms remote"},
		NotBefore:    now.Add(-1 * time.Hour), // tolerate mild clock skew on the client
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// A leaf, not a CA: BasicConstraintsValid with IsCA false states plainly
		// that this certificate signs nothing but itself.
		BasicConstraintsValid: true,
		IsCA:                  false,
		IPAddresses:           ips,
		DNSNames:              dns,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: marshalling key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: creating %s: %w", dir, err)
	}
	certPath, keyPath := certPaths(dir)
	// The private key is written 0600. The cert is public and could be 0644, but
	// it is written 0600 too for uniformity — nothing reads it but this process.
	if err := writeFileAtomic(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: reloading generated cert: %w", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	cert.Leaf = leaf
	return cert, fingerprintDER(der), nil
}

// dedupeIPs removes duplicate addresses while preserving order, so a loopback
// bind does not list 127.0.0.1 twice in the SANs.
func dedupeIPs(in []net.IP) []net.IP {
	out := in[:0:0]
	for _, ip := range in {
		dup := false
		for _, seen := range out {
			if seen.Equal(ip) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, ip)
		}
	}
	return out
}

// fingerprintDER returns the colon-separated uppercase-hex SHA-256 of a DER
// certificate — the exact string a browser's certificate viewer shows, so an
// operator can compare the two character for character.
func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	const hex = "0123456789ABCDEF"
	for i, by := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteByte(hex[by>>4])
		b.WriteByte(hex[by&0x0f])
	}
	return b.String()
}

// CertificateFingerprint returns the SHA-256 fingerprint of a loaded
// certificate's leaf, for the Settings display. It reads Certificate[0], the
// leaf DER, which is exactly what is served on the wire.
func CertificateFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	return fingerprintDER(cert.Certificate[0])
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, the same durability discipline used for remote.json:
// a reader never sees a half-written key or cert.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("remote: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("remote: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remote: closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("remote: renaming %s to %s: %w", tmpPath, path, err)
	}
	renamed = true
	return nil
}
