package appservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "as.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// cfg renders app_services entries from {id, namespace, as_token} triples.
func cfg(entries ...[3]string) string {
	var b strings.Builder
	b.WriteString("app_services:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  - id: %s\n    namespace: %s\n    endpoint: http://bot\n    hs_token: hs-%s\n    as_token: %s\n    sender_username: bot\n",
			e[0], e[1], e[0], e[2])
	}
	return b.String()
}

func TestLoadFromFile_Namespace(t *testing.T) {
	reg, err := LoadFromFile(writeConfig(t, cfg(
		[3]string{"a", "acme", "tok-a"},
		[3]string{"b", `""`, "tok-b"},
	)))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := reg.GetByID("a")
	b, _ := reg.GetByID("b")
	if a.NamespaceSlug != "acme" || b.NamespaceSlug != domain.DefaultNamespaceSlug {
		t.Fatalf("namespaces = %q, %q", a.NamespaceSlug, b.NamespaceSlug)
	}
}

func TestLoadFromFile_RejectsAmbiguousEntries(t *testing.T) {
	for name, yaml := range map[string]string{
		"duplicate id":    cfg([3]string{"a", "x", "tok-1"}, [3]string{"a", "y", "tok-2"}),
		"shared as_token": cfg([3]string{"a", "x", "tok"}, [3]string{"b", "y", "tok"}),
		"bad namespace":   cfg([3]string{"a", "Not_A_Slug", "tok"}),
	} {
		if _, err := LoadFromFile(writeConfig(t, yaml)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
