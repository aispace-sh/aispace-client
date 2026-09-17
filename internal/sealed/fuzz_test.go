package sealed

import (
	"strings"
	"testing"
)

// The values these parse arrive from outside: a fragment or reference is
// whatever a human pasted, and a manifest is whatever the far side produced.
// A malformed one must be rejected, never panic the process.

func FuzzParseFragment(f *testing.F) {
	f.Add("")
	f.Add("v1.")
	f.Add("v1.AAAA.BBBB")
	f.Add("v1." + strings.Repeat("A", 43) + "." + strings.Repeat("B", 43))
	f.Add("v1..")
	f.Add("v1.-.-")
	f.Add(strings.Repeat("v1.", 100))

	f.Fuzz(func(t *testing.T, value string) {
		secrets, err := ParseFragment(value)
		if err != nil {
			return
		}
		// A value that parsed has to round-trip, or the encoder and parser
		// disagree about what the format is.
		if got := secrets.Fragment(); got != value {
			t.Fatalf("ParseFragment(%q) round-tripped to %q", value, got)
		}
	})
}

func FuzzParseReference(f *testing.F) {
	f.Add("")
	f.Add("t/abc#v1.AAAA.BBBB")
	f.Add("https://aispace.sh/t/abc#v1.AAAA.BBBB")
	f.Add("#")
	f.Add("##")
	f.Add(strings.Repeat("#", 50))

	f.Fuzz(func(t *testing.T, value string) {
		id, _, err := ParseReference(value)
		if err != nil {
			return
		}
		if id == "" {
			t.Fatalf("ParseReference(%q) accepted an empty transfer ID", value)
		}
	})
}

func FuzzDecodeManifest(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"protocol":"x"}`))
	f.Add([]byte(`{"files":[]}`))
	f.Add([]byte(`{"files":[{"size_bytes":-1}]}`))
	f.Add([]byte("[" + strings.Repeat("[", 200)))

	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = DecodeManifest(b)
	})
}
