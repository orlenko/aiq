package quota

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"accounts":[{"id":"codex1","provider":"codex","label":"L","credentials":"/old"}],"extra":true}`), 0o600)
	if err := SetCredentials(path, "codex1", "/new/auth.json"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].Credentials != "/new/auth.json" || c.Accounts[0].Label != "L" {
		t.Fatalf("%+v", c.Accounts)
	}
	data, _ := os.ReadFile(path)
	if !contains(string(data), `"extra": true`) {
		t.Fatalf("unknown keys must survive: %s", data)
	}
	if err := SetCredentials(path, "nope", "/x"); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
