package domain

import "testing"

func TestValidNamespaceSlug(t *testing.T) {
	for slug, want := range map[string]bool{
		"default": true, "acme": true, "nim-chat": true, "a1": true,
		"": false, "-x": false, "Acme": false, "a_b": false, "a b": false,
		"x123456789012345678901234567890123456789012345678901234567890123": false,
	} {
		if got := ValidNamespaceSlug(slug); got != want {
			t.Errorf("ValidNamespaceSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}
