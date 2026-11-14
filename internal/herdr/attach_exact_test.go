package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachExactRequiresSavedGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	family := Prefix + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32)
	for _, ref := range []string{"", family, family + "@0", family + "@01", family + "@-1", family + "@18446744073709551616", "herdr:../../fixture"} {
		if action, err := PrepareAttachExact(ref, "/fixture", 123); action != nil || err == nil {
			t.Fatalf("unsafe reference accepted %q", ref)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "herdr")); !os.IsNotExist(err) {
		t.Fatal("invalid exact reference initialized ownership state")
	}
}
