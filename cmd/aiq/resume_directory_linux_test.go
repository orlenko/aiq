package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResumeDirectoryRealReadableDescriptorAndFgetxattr(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "invented-target")
	if err := os.Mkdir(target, 0775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0775); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	fd, err := openResumeDir(target)
	if err != nil {
		t.Fatal(err)
	}
	owned := fd.(*unixResumeDir)
	flags, err := unix.FcntlInt(uintptr(owned.fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_PATH != 0 || flags&unix.O_DIRECTORY == 0 {
		fd.close()
		t.Fatal("descriptor is not an ordinary readable directory", err)
	}
	fdflags, err := unix.FcntlInt(uintptr(owned.fd), unix.F_GETFD, 0)
	if err != nil || fdflags&unix.FD_CLOEXEC == 0 {
		fd.close()
		t.Fatal("directory descriptor may survive exec", err)
	}
	facts, err := fd.facts()
	if err != nil {
		fd.close()
		t.Fatal(err)
	}
	for _, name := range []string{"system.posix_acl_access", "system.nfs4_acl"} {
		present, err := fd.accessList(name)
		if err != nil || present {
			fd.close()
			t.Fatalf("real descriptor could not prove absent access list: %s %v", name, err)
		}
	}
	if err := fd.close(); err != nil {
		t.Fatal(err)
	}
	d := defaultResumeDirDeps()
	d.read = func(path string) ([]byte, error) {
		if path == "/etc/passwd" {
			return []byte(fmt.Sprintf("invented:x:%d:%d::/invented:/bin/sh\n", facts.uid, facts.gid)), nil
		}
		if path == "/etc/group" {
			return []byte(fmt.Sprintf("invented:x:%d:invented\n", facts.gid)), nil
		}
		t.Fatal("unexpected account source")
		return nil, nil
	}
	entered, changed, err := enterResumeDirectory(d, target, fullIDFixtureID)
	if err != nil || !changed || entered != target {
		t.Fatal("real private-group directory refused", changed, err)
	}
	// Make the fd path unreadable to the caller; stat may explain a refusal,
	// but must never bypass the failed open. Root cannot exercise EACCES.
	if os.Geteuid() == 0 {
		t.Log("unreadable directory case NOT RUN: root bypasses mode read denial")
		return
	}
	if err := os.Chmod(target, 0111); err != nil {
		t.Fatal(err)
	}
	err = checkResumeDirectory(d, target, target, fullIDFixtureID, false, false)
	resumeReason(t, err, "unexamined")
}

func TestResumeDirectoryRealAccessListPresence(t *testing.T) {
	setfacl, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("real ACL presence NOT RUN: setfacl is not installed")
	}
	path := t.TempDir()
	if output, err := exec.Command(setfacl, "-m", "u:424242:rwx", path).CombinedOutput(); err != nil {
		t.Skipf("real ACL presence NOT RUN: owned fixture filesystem rejected setfacl (%v; %q)", err, output)
	}
	fd, err := openResumeDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.close()
	present, err := fd.accessList("system.posix_acl_access")
	if err != nil || !present {
		t.Fatal("real Fgetxattr did not detect installed ACL", err)
	}
	d := defaultResumeDirDeps()
	d.read = func(string) ([]byte, error) {
		t.Fatal("ACL presence reached group-file exception")
		return nil, errors.New("not allowed")
	}
	err = checkResumeDirectory(d, path, path, fullIDFixtureID, false, false)
	resumeReason(t, err, "acl")
}
