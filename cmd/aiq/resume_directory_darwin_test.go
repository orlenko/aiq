package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResumeDirectoryNativeDarwinACLsOnSameDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name, acl string
		write     bool
	}{{"none", "", false}, {"deny delete", "everyone deny delete", false}, {"read grant", "everyone allow read,readattr,readextattr,readsecurity", false}, {"write grant", "everyone allow write,append,delete_child,writeattr,writeextattr,writesecurity,chown", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invented-directory")
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			clear := func() {
				if out, err := exec.Command("/bin/chmod", "-N", path).CombinedOutput(); err != nil {
					t.Fatalf("could not clear owned fixture ACL: %v %q", err, out)
				}
			}
			clear()
			t.Cleanup(clear)
			fd, err := openResumeDir(path)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.close()
			if tc.acl != "" {
				if out, err := exec.Command("/bin/chmod", "+a", tc.acl, path).CombinedOutput(); err != nil {
					t.Fatalf("could not set invented ACL: %v %q", err, out)
				}
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatal("ACL fixture changed BSD mode", err)
			}
			write, err := fd.accessList("darwin.acl")
			if err != nil || write != tc.write {
				t.Fatal("native descriptor ACL mismatch", write, err)
			}
			err = checkResumeDirectory(defaultResumeDirDeps(), path, path, fullIDFixtureID, false, false)
			if tc.write {
				resumeReason(t, err, "mac_acl")
			} else if err != nil {
				t.Fatal("safe native ACL refused", err)
			}
		})
	}
	if _, err := (&unixResumeDir{fd: -1}).accessList("darwin.acl"); err == nil {
		t.Fatal("invalid descriptor approved")
	}
}
