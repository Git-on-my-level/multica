package githubpr

import "testing"

func TestParseCanonical(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://github.com/acme/widget/pull/42", true},
		{"https://github.com/acme/widget/pull/42/?diff=split#x", true},
		{"http://github.com/acme/widget/pull/42", false},
		{"https://github.com/acme/widget/pull/42/files", false},
		{"https://github.example/acme/widget/pull/42", false},
		{"https://github.com/acme//widget/pull/42", false},
		{"https://github.com/acme%2Fother/widget/pull/42", false},
		{"https://github.com/acme/widget.git/pull/42", false},
		{"https://user@github.com/acme/widget/pull/42", false},
	} {
		_, ok := ParseCanonical(tc.raw)
		if ok != tc.want {
			t.Errorf("ParseCanonical(%q) ok=%v, want %v", tc.raw, ok, tc.want)
		}
	}
}

func TestExtractCanonicalDeduplicatesAndRejectsSuffix(t *testing.T) {
	got := ExtractCanonical("PR https://github.com/Acme/Widget/pull/7, duplicate https://github.com/acme/widget/pull/7 and files https://github.com/acme/widget/pull/8/files")
	if len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("ExtractCanonical() = %#v, want one PR #7", got)
	}
}

func TestRepositoryFromRemote(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/Acme/Widget.git",
		"ssh://git@github.com/Acme/Widget.git",
		"git@github.com:Acme/Widget.git",
	} {
		got, ok := RepositoryFromRemote(raw)
		if !ok || got != "acme/widget" {
			t.Errorf("RepositoryFromRemote(%q) = %q, %v", raw, got, ok)
		}
	}
}
