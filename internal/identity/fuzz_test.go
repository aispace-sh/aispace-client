package identity

import (
	"strings"
	"testing"
)

// An invitation, public key or fingerprint is supplied by whoever is on the
// other side of the exchange. A malformed one must be rejected, never panic.

func FuzzParseInvitation(f *testing.F) {
	f.Add("")
	f.Add("https://aispace.sh/i/abc")
	f.Add("aispace://invite")
	f.Add("https://aispace.sh/i/abc#fp")
	f.Add(strings.Repeat("/", 200))
	f.Add("https://" + strings.Repeat("a", 500))

	f.Fuzz(func(t *testing.T, value string) {
		server, id, fingerprint, err := ParseInvitation(value)
		if err != nil {
			return
		}
		// Anything accepted has to be complete: a half-parsed invitation would
		// pin trust to an empty identity.
		if server == "" || id == "" || fingerprint == "" {
			t.Fatalf("ParseInvitation(%q) accepted an incomplete result: server=%q id=%q fingerprint=%q",
				value, server, id, fingerprint)
		}
	})
}

func FuzzParseFingerprint(f *testing.F) {
	f.Add("")
	f.Add(strings.Repeat("a", 64))
	f.Add(strings.Repeat("A", 64))
	f.Add(strings.Repeat("a", 63))
	f.Add(strings.Repeat("z", 64))
	f.Add("aa:bb:cc")

	f.Fuzz(func(t *testing.T, value string) {
		got, err := ParseFingerprint(value)
		if err != nil {
			return
		}
		// A fingerprint is compared against a pinned one, so an accepted value
		// must be exactly 32 bytes of hex or the comparison is meaningless.
		if len(got) != 64 {
			t.Fatalf("ParseFingerprint(%q) returned %d characters, want 64", value, len(got))
		}
	})
}

func FuzzParsePublicKey(f *testing.F) {
	f.Add("", 32)
	f.Add(strings.Repeat("A", 43), 32)
	f.Add(strings.Repeat("A", 44), 32)
	f.Add("====", 32)
	f.Add(strings.Repeat("A", 43), 0)
	f.Add(strings.Repeat("A", 43), -1)

	f.Fuzz(func(t *testing.T, value string, size int) {
		// Sizes are fixed by the caller in real use; keep the fuzzer in range
		// rather than exploring allocation behaviour.
		if size < 0 || size > 1024 {
			return
		}
		got, err := ParsePublicKey(value, size)
		if err != nil {
			return
		}
		if len(got) != size {
			t.Fatalf("ParsePublicKey(%q, %d) returned %d bytes", value, size, len(got))
		}
	})
}
