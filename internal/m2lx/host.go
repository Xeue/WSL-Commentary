package m2lx

import (
	"log"
	"strings"
)

// resolvedHost is a configured M2L-X host after its scheme has been
// decided: the bare host[:port] to dial, and whether the insecure http/ws
// pair applies instead of the production https/wss pair.
//
// RESTScheme and WSScheme are always the matching pair — https comes with
// wss, http comes with ws, never mixed — because both are derived from
// the single insecure flag here rather than decided separately at the two
// call sites (doJSON and statusURL) that need one each. The zero value
// (insecure: false) is the secure pair, so a resolvedHost nobody has
// bothered to set up deliberately can never come out insecure by
// accident.
type resolvedHost struct {
	hostPort string
	insecure bool
}

// restScheme is "https", unless this host was explicitly resolved as
// insecure, in which case it is "http".
func (r resolvedHost) restScheme() string {
	if r.insecure {
		return "http"
	}
	return "https"
}

// wsScheme is restScheme's WebSocket counterpart: "wss" unless insecure,
// in which case "ws". It is never decided independently of restScheme —
// see the resolvedHost doc comment.
func (r resolvedHost) wsScheme() string {
	if r.insecure {
		return "ws"
	}
	return "wss"
}

// resolveHost decides, once, whether host talks the production
// https/wss pair or the insecure http/ws pair, and strips any scheme
// prefix and trailing slash from the value used as url.URL.Host.
//
// NewClient and NewWatcher both document host as "a bare hostname or
// host:port with no scheme", and a bare host — no literal "://" anywhere
// in it — ALWAYS resolves to https/wss. That is not just the production
// path; it is the only choice that cannot put a commentator's password or
// bearer token on the wire in clear, so it is what every input defaults
// to when there is any doubt at all, including a host string this
// function does not otherwise recognise (see the default case below).
//
// An explicit "http://" prefix opts into plain HTTP/WS. Its one
// legitimate use is cmd/mockm2lx (WP-7), which serves plain HTTP at Gate
// A with no TLS option: this machine has no MinGW/OpenSSL toolchain to
// give it a certificate, and a self-signed one would not have been
// sufficient on its own even if it had one, because this package's
// http.Client uses the default transport, which validates against the
// Windows root store and would reject a self-signed leaf regardless.
// Because opting into http is a deliberate downgrade from the secure
// default, it is logged once, loudly, here — so an operator who
// fat-fingers "http://" into the production Settings field sees it,
// rather than discovering it from a packet capture.
//
// An explicit "https://" prefix is accepted and resolves exactly like a
// bare host.
//
// Anything else — a scheme this package does not recognise (a typo, or
// something like "ftp://") — gets the SAME treatment as a bare host,
// https/wss, never the insecure pair: guessing http for an unrecognised
// scheme risks a plaintext password over one bad guess, while defaulting
// to https merely risks a REST call that fails loudly against a host that
// was not actually reachable over https either. A one-line warning is
// logged so the mismatch is visible instead of silently "working" via a
// safety net nobody knew was there.
//
// purpose is folded into the log line ("REST" from NewClient, "status
// socket" from NewWatcher) purely so a warning names which of the two
// consumers logged it; it does not otherwise affect the result.
func resolveHost(host, purpose string) resolvedHost {
	scheme, rest, hasScheme := splitScheme(host)
	rest = strings.TrimSuffix(rest, "/")

	if !hasScheme {
		return resolvedHost{hostPort: rest, insecure: false}
	}

	switch strings.ToLower(scheme) {
	case "https":
		return resolvedHost{hostPort: rest, insecure: false}
	case "http":
		log.Printf("m2lx: %s host %q selected via an explicit \"http://\" scheme — "+
			"REST and the status socket will use http/ws, NOT https/wss. Credentials "+
			"and the bearer token will travel in clear. This must never point at a "+
			"production M2L-X instance; it exists only to reach cmd/mockm2lx.", purpose, host)
		return resolvedHost{hostPort: rest, insecure: true}
	default:
		log.Printf("m2lx: %s host %q has scheme %q, which this package does not "+
			"recognise; treating it as https/wss (the safe default) rather than "+
			"guessing at what was meant", purpose, host, scheme)
		return resolvedHost{hostPort: rest, insecure: false}
	}
}

// splitScheme splits host on a literal "scheme://" prefix. hasScheme is
// false for anything without that exact separator — including a bare
// "host:port", where the colon is a port separator, not a scheme
// separator. This is deliberately NOT net/url.Parse: net/url treats a
// bare "host:port" string as ambiguous (a leading component that looks
// like a scheme is parsed as one even with no "//" following), which is
// exactly the wrong behaviour here — "127.0.0.1:18081" must resolve as a
// bare host, not as a host named "127.0.0.1" with scheme "18081"-shaped
// nonsense. Requiring the literal "://" sidesteps that ambiguity entirely.
func splitScheme(host string) (scheme, rest string, hasScheme bool) {
	if i := strings.Index(host, "://"); i >= 0 {
		return host[:i], host[i+3:], true
	}
	return "", host, false
}
