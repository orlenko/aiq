package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const resumeFixturePasswd = "invented:x:1001:2001::/invented:/bin/sh\nroot:x:0:0::/:/bin/sh\n"
const resumeFixtureGroup = "invented:x:2001:invented\nroot:x:0:\n"

func TestResumePrivateGroupWholeFileClass(t *testing.T) {
	for _, tc := range []struct {
		name, passwd, group string
		want                bool
	}{
		{"private", resumeFixturePasswd, resumeFixtureGroup, true},
		{"empty member list", resumeFixturePasswd, "invented:x:2001:\n", true},
		{"CRLF comments blank final newline", " \r\n # invented comment\r\n" + strings.ReplaceAll(resumeFixturePasswd, "\n", "\r\n"), "\t# invented\r\n" + strings.ReplaceAll(resumeFixtureGroup, "\n", "\r\n"), true},
		{"no final newline", strings.TrimSuffix(resumeFixturePasswd, "\n"), strings.TrimSuffix(resumeFixtureGroup, "\n"), true},
		{"missing caller", "other:x:1002:3001::/:/bin/sh\n", resumeFixtureGroup, false},
		{"different primary gid", strings.Replace(resumeFixturePasswd, "1001:2001", "1001:3001", 1), resumeFixtureGroup, false},
		{"other primary member", resumeFixturePasswd + "other:x:1002:2001::/:/bin/sh\n", resumeFixtureGroup, false},
		{"supplementary member", resumeFixturePasswd, "invented:x:2001:invented,other\n", false},
		{"other group name", resumeFixturePasswd, "staff:x:2001:invented\n", false},
		{"duplicate uid", resumeFixturePasswd + "alias:x:1001:2001::/:/bin/sh\n", resumeFixtureGroup, false},
		{"duplicate username", resumeFixturePasswd + "invented:x:1002:3001::/:/bin/sh\n", resumeFixtureGroup, false},
		{"duplicate gid", resumeFixturePasswd, resumeFixtureGroup + "alias:x:2001:\n", false},
		{"passwd compat", resumeFixturePasswd + "+::::::\n", resumeFixtureGroup, false},
		{"group compat", resumeFixturePasswd, resumeFixtureGroup + "-::: \n", false},
		{"unrelated malformed passwd", resumeFixturePasswd + "malformed\n", resumeFixtureGroup, false},
		{"unrelated malformed group", resumeFixturePasswd, resumeFixtureGroup + "malformed\n", false},
		{"signed uid", strings.Replace(resumeFixturePasswd, "1001:2001", "+1001:2001", 1), resumeFixtureGroup, false},
		{"reserved uid", strings.Replace(resumeFixturePasswd, "1001:2001", "4294967295:2001", 1), resumeFixtureGroup, false},
		{"overflow gid", resumeFixturePasswd, strings.Replace(resumeFixtureGroup, "2001", "4294967296", 1), false},
		{"bad numeric unrelated", resumeFixturePasswd, resumeFixtureGroup + "other:x:not-numeric:\n", false},
		{"oversize passwd", strings.Repeat("#", resumeAccountFileLimit+1), resumeFixtureGroup, false},
		{"oversize group", resumeFixturePasswd, strings.Repeat("#", resumeAccountFileLimit+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := resumePrivateGroup([]byte(tc.passwd), []byte(tc.group), 1001, 2001)
			if got != tc.want {
				t.Fatalf("private=%v want %v", got, tc.want)
			}
		})
	}
}

type fixtureResumeDir struct {
	value                     resumeDirFacts
	statErr, aclErr, enterErr error
	acl                       bool
	names                     []string
	entered, closed           int
}

func (f *fixtureResumeDir) facts() (resumeDirFacts, error) { return f.value, f.statErr }
func (f *fixtureResumeDir) accessList(name string) (bool, error) {
	f.names = append(f.names, name)
	return f.acl, f.aclErr
}
func (f *fixtureResumeDir) enter() error { f.entered++; return f.enterErr }
func (f *fixtureResumeDir) close() error { f.closed++; return nil }
func resumeDirFixture(fd *fixtureResumeDir) resumeDirDeps {
	return resumeDirDeps{goos: "linux", uid: 1001, canonical: func(s string) (string, error) { return s, nil }, chdir: func(string) error { return nil }, getwd: func() (string, error) { return "/invented/caller", nil },
		open: func(string) (resumeDirFD, error) { return fd, nil }, stat: func(string) (resumeDirFacts, error) { return fd.value, nil }, read: func(path string) ([]byte, error) {
			if path == "/etc/passwd" {
				return []byte(resumeFixturePasswd), nil
			}
			if path == "/etc/group" {
				return []byte(resumeFixtureGroup), nil
			}
			panic("unexpected account file")
		}}
}
func resumeReason(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *resumeDirError
	if !errors.As(err, &refusal) || refusal.reason != want {
		t.Fatalf("reason: got %v want %s", err, want)
	}
}

func TestResumeDirectoryDescriptorDecisionClass(t *testing.T) {
	for _, tc := range []string{"safe", "private group", "other owner", "other write", "sticky other write", "shared group", "ACL present", "ACL unavailable", "macOS group", "macOS write ACL with 0755", "macOS unavailable ACL with 0755", "fstat unavailable", "not directory", "fchdir unavailable", "unreadable accounts"} {
		t.Run(tc, func(t *testing.T) {
			fd := &fixtureResumeDir{value: resumeDirFacts{true, 1001, 2001, 0755}}
			d := resumeDirFixture(fd)
			want := ""
			switch tc {
			case "private group":
				fd.value.mode = 0775
			case "other owner":
				fd.value.uid = 1002
				want = "other_owner"
			case "other write":
				fd.value.mode = 0777
				want = "other_write"
			case "sticky other write":
				fd.value.mode = 01777
				want = "other_write"
			case "shared group":
				fd.value.mode = 0775
				d.read = func(string) ([]byte, error) { return []byte(""), nil }
				want = "group_unconfirmed"
			case "ACL present":
				fd.value.mode = 0775
				fd.acl = true
				want = "acl"
			case "ACL unavailable":
				fd.value.mode = 0775
				fd.aclErr = syscall.EACCES
				want = "acl"
			case "macOS group":
				fd.value.mode = 0775
				d.goos = "darwin"
				want = "not_linux"
			case "macOS write ACL with 0755":
				d.goos = "darwin"
				fd.acl = true
				want = "mac_acl"
			case "macOS unavailable ACL with 0755":
				d.goos = "darwin"
				fd.aclErr = syscall.EACCES
				want = "mac_acl"
			case "fstat unavailable":
				fd.statErr = syscall.EBADF
				want = "unexamined"
			case "not directory":
				fd.value.directory = false
				want = "not_dir"
			case "fchdir unavailable":
				fd.enterErr = syscall.EACCES
				want = "unexamined"
			case "unreadable accounts":
				fd.value.mode = 0775
				d.read = func(string) ([]byte, error) { return nil, syscall.EACCES }
				want = "group_unconfirmed"
			}
			err := checkResumeDirectory(d, ".", "/invented/target", fullIDFixtureID, false, true)
			if want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if fd.entered != 1 {
					t.Fatal("not bound to examined descriptor")
				}
			} else {
				resumeReason(t, err, want)
				if want != "unexamined" && fd.entered != 0 {
					t.Fatal("entered refused descriptor")
				}
			}
			if fd.closed != 1 {
				t.Fatal("descriptor not closed exactly once")
			}
			if tc == "private group" && strings.Join(fd.names, ",") != "system.posix_acl_access,system.nfs4_acl" {
				t.Fatal("both access lists not examined", fd.names)
			}
		})
	}
}

func TestResumeDirectoryFailedOpenClassifiesButCannotPass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		facts   resumeDirFacts
		statErr error
		reason  string
		cd      bool
	}{
		{"own unreadable", resumeDirFacts{true, 1001, 2001, 0711}, nil, "unexamined", true},
		{"foreign searchable", resumeDirFacts{true, 1002, 2001, 0711}, nil, "other_owner", false},
		{"other writable", resumeDirFacts{true, 1001, 2001, 0733}, nil, "other_write", false},
		{"unknown", resumeDirFacts{}, syscall.EACCES, "unexamined", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := &fixtureResumeDir{value: tc.facts}
			d := resumeDirFixture(fd)
			d.open = func(string) (resumeDirFD, error) { return nil, syscall.EACCES }
			d.stat = func(string) (resumeDirFacts, error) { return tc.facts, tc.statErr }
			err := checkResumeDirectory(d, ".", "/invented/target", fullIDFixtureID, false, true)
			resumeReason(t, err, tc.reason)
			if strings.Contains(err.Error(), "or cd there") != tc.cd {
				t.Fatal("unsafe or missing recovery advice", err)
			}
		})
	}
}

func TestResumeDirectoryEnteredFirstAndWorkspaceNeverEntered(t *testing.T) {
	fd := &fixtureResumeDir{value: resumeDirFacts{true, 1001, 2001, 0755}}
	d := resumeDirFixture(fd)
	cwd := "/invented/caller"
	events := []string{}
	d.getwd = func() (string, error) { return cwd, nil }
	d.chdir = func(path string) error { events = append(events, "chdir "+path); cwd = path; return nil }
	d.open = func(path string) (resumeDirFD, error) {
		events = append(events, "open "+path)
		if path == "." && cwd != "/invented/target" {
			t.Fatal("examined caller before entering target")
		}
		return fd, nil
	}
	dir, changed, err := enterResumeDirectory(d, "/invented/target", fullIDFixtureID)
	if err != nil || !changed || dir != "/invented/target" || strings.Join(events, ",") != "chdir /invented/target,open ." || fd.entered != 1 {
		t.Fatal("wrong entry order", events, dir, changed, err)
	}
	fd.value.mode = 0777
	err = checkResumeWorkspace(d, dir, "/invented/workspace", fullIDFixtureID, changed)
	resumeReason(t, err, "workspace_other_write")
	if fd.entered != 1 || cwd != dir {
		t.Fatal("workspace was entered")
	}
	// Canonical equality, including symlink aliases, keeps existing behavior.
	d.canonical = func(string) (string, error) { return "/same", nil }
	d.open = func(string) (resumeDirFD, error) { t.Fatal("already-there check was not bypassed"); return nil, nil }
	if _, changed, err := enterResumeDirectory(d, "/invented/alias", fullIDFixtureID); err != nil || changed {
		t.Fatal("canonical equality not honored", changed, err)
	}
	if err := checkResumeWorkspace(d, dir, "/invented/shared", fullIDFixtureID, false); err != nil {
		t.Fatal("typed in-directory command changed behavior", err)
	}
}

func TestResumeDirectoryRefusalWordingAndQuoting(t *testing.T) {
	d := resumeDirFixture(&fixtureResumeDir{})
	for _, tc := range []struct{ reason, phrase string }{
		{"missing", "the project moved or was deleted"}, {"not_dir", "something else now has that path"}, {"other_owner", "if it is yours, take ownership first"}, {"other_write", "remove write for others (chmod o-w)"}, {"group_unconfirmed", "which aiq cannot confirm is yours alone (it checks the local account files)"}, {"not_linux", "aiq confirms private groups on Linux only"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			e := resumeDirRefusal(d, "/invented/target", "/invented/target", fullIDFixtureID, false, tc.reason, nil, nil)
			if !strings.HasPrefix(e.Error(), "aiq: not resuming in \"/invented/target\": ") || !strings.Contains(e.Error(), tc.phrase) {
				t.Fatal("refusal wording", e)
			}
		})
	}
	for _, path := range []string{"/invented/$(example) 'quoted'", "/invented/escape\x1b", "/invented/bidi\u202e"} {
		e := resumeDirRefusal(d, path, "/invented/session", fullIDFixtureID, true, "acl", nil, nil)
		text := e.Error()
		if strings.ContainsAny(text, "\x1b\u202e") {
			t.Fatal("unescaped terminal control")
		}
		if !strings.Contains(text, "cd to the session's directory (named at the start of this message)") {
			t.Fatal("workspace recovery points to wrong directory", text)
		}
		if strings.ContainsAny(path, "\x1b\u202e") {
			if strings.Contains(text, "getfacl -- ") {
				t.Fatal("printed unprintable shell command")
			}
		} else if !strings.Contains(text, "getfacl -- "+shellQuote(path)) {
			t.Fatal("getfacl was not POSIX quoted", text)
		}
	}
}

func TestResumeAccountFileBoundedOwnedFixtures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invented-accounts")
	if err := os.WriteFile(path, []byte(resumeFixturePasswd), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readResumeAccountFile(path); err != nil || string(data) != resumeFixturePasswd {
		t.Fatal("fixture read", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", resumeAccountFileLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readResumeAccountFile(path); err == nil {
		t.Fatal("oversize local account file admitted")
	}
}

func TestResumeDirectoryExactRefusalTable(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		known    bool
		cause    error
		expected string
	}{
		{"missing", false, nil, "the project moved or was deleted; move the project back, or recreate \"/invented/target\", then run the command again; to read the session, run aiq convo \"a1000000-1111-4222-8333-000000000001\""},
		{"not_dir", false, nil, "it is not a directory; something else now has that path: move it aside and restore the project there, or read the session with aiq convo \"a1000000-1111-4222-8333-000000000001\""},
		{"other_owner", false, nil, "it belongs to another user, who can write it; if it is yours, take ownership first; otherwise read it with aiq convo \"a1000000-1111-4222-8333-000000000001\""},
		{"other_write", false, nil, "another user can write it (world-writable); remove write for others (chmod o-w), then run the command again"},
		{"unexamined", true, syscall.EACCES, "aiq could not examine it (permission denied); check the permissions on \"/invented/target\" and its parents, or cd there and run the command again"},
		{"unexamined", false, syscall.EACCES, "aiq could not examine it (permission denied); check the permissions on \"/invented/target\" and its parents"},
		{"group_unconfirmed", false, nil, "it is writable by group 2001, which aiq cannot confirm is yours alone (it checks the local account files); remove group write, or cd there and run the command again"},
		{"not_linux", false, nil, "it is writable by group 2001, and aiq confirms private groups on Linux only; remove group write, or cd there and run the command again"},
		{"acl", false, nil, "it has an access list that may let another user write; see who it lets write with getfacl -- /invented/target; remove group write, or cd there and run the command again"},
		{"acl", false, syscall.EACCES, "aiq could not read its access list (permission denied); remove group write, or cd there and run the command again"},
		{"workspace_group_unconfirmed", false, nil, "its workspace \"/invented/target\": it is writable by group 2001, which aiq cannot confirm is yours alone (it checks the local account files); remove group write, or cd to the session's directory (named at the start of this message) and run the command again"},
		{"workspace_other_owner", false, nil, "its workspace \"/invented/target\": it belongs to another user, who can write it; if it is yours, take ownership first; otherwise read it with aiq convo \"a1000000-1111-4222-8333-000000000001\""},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			e := &resumeDirError{dir: "/invented/target", sessionDir: "/invented/session", id: fullIDFixtureID, reason: tc.reason, gid: 2001, cause: tc.cause, ownerKnown: tc.known}
			if got, want := e.Error(), "aiq: not resuming in \"/invented/session\": "+tc.expected; got != want {
				t.Fatalf("refusal differs:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

func TestResumeDirectoryRelativeEnvironmentNeverChangesStores(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"AIQ_DATA_DIR", "relative-data"}, {"AIQ_CONFIG", "relative-config"}, {"CLAUDE_CONFIG_DIR", "relative-claude"}, {"CODEX_HOME", "relative-codex"}, {"HOME", "relative-home"}, {"PATH", "/invented/bin:relative-bin"}, {"PATH", "/invented/bin:"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			d := resumeDirFixture(&fixtureResumeDir{})
			d.before = checkResumePathEnvironment
			d.chdir = func(string) error { t.Fatal("relative environment changed cwd"); return nil }
			if _, _, err := enterResumeDirectory(d, "/invented/target", fullIDFixtureID); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatal("relative path was not refused", err)
			}
			d.canonical = func(string) (string, error) { return "/same", nil }
			if _, changed, err := enterResumeDirectory(d, "/invented/target", fullIDFixtureID); err != nil || changed {
				t.Fatal("already-there escape changed behavior", changed, err)
			}
		})
	}
}
