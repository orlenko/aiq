package navtoken

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsPublicReaderIsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invented-valid-state")
	if err := os.WriteFile(path, []byte(literalOn), 0600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, filepath.Join(t.TempDir(), "missing")} {
		record, err := Read(candidate)
		if err != ErrUnavailable || record.Status() != Invalid {
			t.Error("Windows production reader is enabled")
		}
		if record.PID() != 0 || record.StartedUnix() != 0 || record.port != 0 || record.key != nil {
			t.Error("disabled reader retained metadata or key closure")
		}
		if port, present := record.Port(); port != 0 || present {
			t.Error("disabled reader published a port")
		}
		if _, present := record.Key(); present {
			t.Error("disabled reader published a key")
		}
	}
}
