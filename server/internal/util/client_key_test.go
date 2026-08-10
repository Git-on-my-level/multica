package util

import "testing"

func TestParseSHA256ClientKey(t *testing.T) {
	valid := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	digest, err := ParseSHA256ClientKey(valid)
	if err != nil {
		t.Fatalf("ParseSHA256ClientKey(valid): %v", err)
	}
	if got, want := digest, valid[len("sha256:"):]; got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}

	for _, key := range []string{
		"",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"sha256:short",
		"sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg",
	} {
		if _, err := ParseSHA256ClientKey(key); err == nil {
			t.Errorf("ParseSHA256ClientKey(%q) unexpectedly succeeded", key)
		}
	}
}
