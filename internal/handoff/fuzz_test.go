package handoff

import (
	"strings"
	"testing"
)

// A transfer reference is pasted by a human from whatever reached them, so it
// is fully untrusted. Parse is documented to validate it completely before any
// network request, and to use expectedOrigin so "a deep link/token cannot
// redirect credentials to a different service".
//
// These targets assert that property rather than only checking for panics: an
// accepted intent carries a master key and a claim capability, so an origin
// that slipped through would send those secrets to whoever chose the link.
func FuzzParseOriginIsEnforced(f *testing.F) {
	const expected = "https://aispace.sh"
	f.Add("")
	f.Add("https://aispace.sh/t/abc#v1.AAAA.BBBB")
	f.Add("https://evil.test/t/abc#v1.AAAA.BBBB")
	f.Add("aispace://transfer?origin=https://evil.test")
	f.Add("HTTPS://AISPACE.SH/t/abc#v1.AAAA.BBBB")
	f.Add("https://aispace.sh:443/t/abc#v1.AAAA.BBBB")
	f.Add("https://aispace.sh@evil.test/t/abc#v1.AAAA.BBBB")
	f.Add("https://aispace.sh./t/abc#v1.AAAA.BBBB")
	f.Add(strings.Repeat("/", 100))

	f.Fuzz(func(t *testing.T, value string) {
		intent, err := Parse(value, expected)
		if err != nil {
			return
		}
		if intent.ServiceOrigin != expected {
			t.Fatalf("Parse(%q, %q) accepted an intent for %q; secrets would go to the wrong service",
				value, expected, intent.ServiceOrigin)
		}
	})
}

// With no expected origin the embedded one is accepted, but it still has to be
// an origin the secrets can safely travel to: a cleartext scheme would put a
// master key on the wire in the clear.
func FuzzParseAcceptsOnlySafeOrigins(f *testing.F) {
	f.Add("http://aispace.sh/t/abc#v1.AAAA.BBBB")
	f.Add("https://aispace.sh/t/abc#v1.AAAA.BBBB")
	f.Add("aispace://transfer")
	f.Add("ftp://aispace.sh/t/abc")
	f.Add("//aispace.sh/t/abc")
	f.Add("https:///t/abc")

	f.Fuzz(func(t *testing.T, value string) {
		intent, err := Parse(value, "")
		if err != nil {
			return
		}
		switch {
		case intent.ServiceOrigin == "":
			t.Fatalf("Parse(%q, \"\") accepted an intent with no origin", value)
		case strings.HasPrefix(intent.ServiceOrigin, "https://"):
			// The only scheme a master key may travel over.
		case strings.HasPrefix(intent.ServiceOrigin, "http://"):
			// Loopback is the documented development exception.
			host := strings.TrimPrefix(intent.ServiceOrigin, "http://")
			if !isLoopbackHost(host) {
				t.Fatalf("Parse(%q, \"\") accepted cleartext origin %q", value, intent.ServiceOrigin)
			}
		default:
			t.Fatalf("Parse(%q, \"\") accepted origin %q with an unexpected scheme", value, intent.ServiceOrigin)
		}
	})
}

func isLoopbackHost(host string) bool {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}
