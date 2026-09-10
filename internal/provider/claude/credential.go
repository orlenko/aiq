package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Claude Code keeps its macOS credential in the login keychain, under a
// service name derived from the config directory, and falls back to a
// .credentials.json inside that directory when the keychain answers
// nothing. A launcher that cuts the CLI off from the keychain therefore
// gets a login prompt unless the file is there, so `credential = "file"`
// writes it before the launch.
//
// On Linux the CLI already uses the file, and these are no-ops.

// credentialFile is the fallback the CLI reads when the keychain is silent.
func credentialFile(configDir string) string {
	return filepath.Join(configDir, ".credentials.json")
}

// exportMarker records what aiq last wrote, so a file the CLI has since
// refreshed can be told from one that is still aiq's copy.
func exportMarker(configDir string) string {
	return filepath.Join(configDir, ".credentials.aiq-export")
}

// KeychainService is the login-keychain service name Claude Code uses for a
// config directory. The default home keeps the bare name; any other
// directory is distinguished by a digest of its path, which is how one
// machine holds a credential per pooled account.
func KeychainService(configDir string) string {
	home, err := os.UserHomeDir()
	if err == nil {
		if abs, err := filepath.Abs(configDir); err == nil && abs == filepath.Join(home, ".claude") {
			return "Claude Code-credentials"
		}
	}
	sum := sha256.Sum256([]byte(configDir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// ExportCredential copies the account's keychain credential into the config
// directory so a CLI that cannot reach the keychain still authenticates. It
// reports whether a file was written. A missing keychain item is not an
// error: the CLI may already be using a file, or the account may need a
// login, and both say so far more clearly than aiq could here.
func ExportCredential(configDir string) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", KeychainService(configDir), "-w").Output()
	if err != nil {
		return false, nil
	}
	secret := strings.TrimRight(string(out), "\n")
	if secret == "" {
		return false, nil
	}
	path := credentialFile(configDir)
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(secret))
	os.WriteFile(exportMarker(configDir), []byte(hex.EncodeToString(sum[:])), 0o600)
	return true, nil
}

// CredentialDrifted reports whether the exported credential file has been
// rewritten since aiq wrote it, which happens when the CLI refreshes its
// token inside a launcher. The keychain copy is then the older of the two,
// and an unwrapped session on the same account keeps using it.
func CredentialDrifted(configDir string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	data, err := os.ReadFile(credentialFile(configDir))
	if err != nil {
		return false
	}
	want, err := os.ReadFile(exportMarker(configDir))
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) != strings.TrimSpace(string(want))
}
