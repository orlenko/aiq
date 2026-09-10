package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeychainService(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	// The default config directory keeps the undecorated service name.
	if got := KeychainService(filepath.Join(home, ".claude")); got != "Claude Code-credentials" {
		t.Errorf("default home: got %q", got)
	}
	// Any other directory is distinguished by a digest of its path, which
	// is what gives one machine a credential per pooled account.
	a := KeychainService("/a/overlay/one")
	b := KeychainService("/a/overlay/two")
	if a == b {
		t.Errorf("two overlays must not share a service name: %q", a)
	}
	if len(a) != len("Claude Code-credentials-")+8 {
		t.Errorf("unexpected shape: %q", a)
	}
	if a != KeychainService("/a/overlay/one") {
		t.Error("service name must be stable")
	}
}

func TestCredentialDrift(t *testing.T) {
	dir := t.TempDir()
	// Nothing exported yet: nothing to report.
	if CredentialDrifted(dir) {
		t.Error("empty dir must not report drift")
	}
	os.WriteFile(credentialFile(dir), []byte(`{"token":"one"}`), 0o600)
	if CredentialDrifted(dir) {
		t.Error("a file with no marker is not aiq's export")
	}
}
