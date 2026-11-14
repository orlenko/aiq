package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const resumeAccountFileLimit = 16 << 20

type resumeDirFacts struct {
	directory bool
	uid, gid  uint32
	mode      uint32
}
type resumeDirFD interface {
	facts() (resumeDirFacts, error)
	accessList(string) (bool, error)
	enter() error
	close() error
}
type resumeDirDeps struct {
	goos      string
	uid       uint32
	canonical func(string) (string, error)
	chdir     func(string) error
	getwd     func() (string, error)
	open      func(string) (resumeDirFD, error)
	stat      func(string) (resumeDirFacts, error)
	read      func(string) ([]byte, error)
	before    func() error
}

func defaultResumeDirDeps() resumeDirDeps {
	return resumeDirDeps{goos: runtime.GOOS, uid: uint32(os.Getuid()), canonical: filepath.EvalSymlinks,
		chdir: os.Chdir, getwd: os.Getwd, open: openResumeDir, stat: statResumeDir, read: readResumeAccountFile, before: checkResumePathEnvironment}
}

var resumeDirectoryRuntime = defaultResumeDirDeps

// Relative process configuration would refer to a different store or executable
// after chdir. Refuse before moving rather than retargeting a provider's home.
func checkResumePathEnvironment() error {
	for _, key := range []string{"AIQ_DATA_DIR", "AIQ_CONFIG", "HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "QUOTA_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "TMPDIR"} {
		if value := os.Getenv(key); value != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("%s is relative; set an absolute path, or cd to the session's directory and run the command again", key)
		}
	}
	if value, exists := os.LookupEnv("PATH"); exists {
		for _, entry := range filepath.SplitList(value) {
			if entry == "" || !filepath.IsAbs(entry) {
				return errors.New("PATH contains a relative directory; use absolute entries, or cd to the session's directory and run the command again")
			}
		}
	}
	return nil
}

func readResumeAccountFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, resumeAccountFileLimit+1))
	if err != nil || len(data) > resumeAccountFileLimit {
		return nil, errors.New("local account file unavailable")
	}
	return data, nil
}

func resumeNumericID(value string) (uint32, bool) {
	if value == "" {
		return 0, false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value, 10, 32)
	return uint32(n), err == nil && n < 1<<32-1
}

// Every noncomment record is parsed, including records unrelated to the caller.
// No name-service enumeration, subprocess or credential database is involved.
func resumePrivateGroup(passwd, groups []byte, uid, gid uint32) (bool, string) {
	if len(passwd) > resumeAccountFileLimit || len(groups) > resumeAccountFileLimit {
		return false, ""
	}
	type user struct {
		name     string
		uid, gid uint32
	}
	type group struct {
		name, members string
		gid           uint32
	}
	users := []user{}
	entries := []group{}
	parse := func(data []byte, fields int, visit func([]string) bool) bool {
		if !utf8.Valid(data) {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if line[0] == '+' || line[0] == '-' {
				return false
			}
			parts := strings.Split(line, ":")
			if len(parts) != fields || parts[0] == "" || !visit(parts) {
				return false
			}
		}
		return true
	}
	if !parse(passwd, 7, func(p []string) bool {
		u, ok := resumeNumericID(p[2])
		if !ok {
			return false
		}
		g, ok := resumeNumericID(p[3])
		if !ok {
			return false
		}
		users = append(users, user{p[0], u, g})
		return true
	}) || !parse(groups, 4, func(p []string) bool {
		g, ok := resumeNumericID(p[2])
		if !ok {
			return false
		}
		entries = append(entries, group{p[0], p[3], g})
		return true
	}) {
		return false, ""
	}
	var name string
	count := 0
	for _, u := range users {
		if u.uid == uid {
			count++
			name = u.name
			if u.gid != gid {
				return false, ""
			}
		} else if u.gid == gid {
			return false, ""
		}
	}
	if count != 1 {
		return false, ""
	}
	// A duplicate local username makes a membership token ambiguous.
	for _, u := range users {
		if u.name == name && u.uid != uid {
			return false, ""
		}
	}
	count = 0
	groupName := ""
	members := ""
	for _, g := range entries {
		if g.gid == gid {
			count++
			groupName = g.name
			members = g.members
		}
	}
	if count != 1 {
		return false, ""
	}
	if groupName != name {
		return false, groupName
	}
	if members != "" {
		for _, member := range strings.Split(members, ",") {
			if member != name {
				return false, groupName
			}
		}
	}
	return true, groupName
}

type resumeDirError struct {
	dir, sessionDir, id, reason, group string
	gid                                uint32
	cause                              error
	ownerKnown                         bool
}

func resumeErrno(err error) string {
	var path *os.PathError
	if errors.As(err, &path) {
		err = path.Err
	}
	if err == nil {
		return "unavailable"
	}
	return err.Error()
}
func (e *resumeDirError) Error() string {
	prefix := fmt.Sprintf("aiq: not resuming in %q: ", e.sessionDir)
	workspace := strings.HasPrefix(e.reason, "workspace_")
	if workspace {
		prefix += fmt.Sprintf("its workspace %q: ", e.dir)
	}
	reason := strings.TrimPrefix(e.reason, "workspace_")
	quoted := fmt.Sprintf("%q", e.dir)
	read := fmt.Sprintf("aiq convo %q", e.id)
	cd := "cd there"
	if workspace {
		cd = "cd to the session's directory (named at the start of this message)"
	}
	group := "group " + strconv.FormatUint(uint64(e.gid), 10)
	if e.group != "" {
		group = fmt.Sprintf("group %q", e.group)
	}
	var message, next string
	switch reason {
	case "missing":
		message = "the project moved or was deleted"
		next = "move the project back, or recreate " + quoted + ", then run the command again; to read the session, run " + read
	case "not_dir":
		message = "it is not a directory"
		next = "something else now has that path: move it aside and restore the project there, or read the session with " + read
	case "other_owner":
		message = "it belongs to another user, who can write it"
		next = "if it is yours, take ownership first; otherwise read it with " + read
	case "other_write":
		message = "another user can write it (world-writable)"
		next = "remove write for others (chmod o-w), then run the command again"
	case "not_linux":
		message = "it is writable by " + group + ", and aiq confirms private groups on Linux only"
		next = "remove group write, or " + cd + " and run the command again"
	case "group_unconfirmed":
		message = "it is writable by " + group + ", which aiq cannot confirm is yours alone (it checks the local account files)"
		next = "remove group write, or " + cd + " and run the command again"
	case "acl":
		next = "remove group write, or " + cd + " and run the command again"
		if e.cause != nil {
			message = "aiq could not read its access list (" + resumeErrno(e.cause) + ")"
		} else {
			message = "it has an access list that may let another user write"
			printable := utf8.ValidString(e.dir)
			for _, r := range e.dir {
				printable = printable && strconv.IsPrint(r)
			}
			if printable {
				next = "see who it lets write with getfacl -- " + shellQuote(e.dir) + "; " + next
			} else {
				next = "see who it lets write by running getfacl on that directory; " + next
			}
		}
	default:
		message = "aiq could not examine it (" + resumeErrno(e.cause) + ")"
		next = "check the permissions on " + quoted + " and its parents"
		if e.ownerKnown {
			next += ", or " + cd + " and run the command again"
		}
	}
	return prefix + message + "; " + next
}

func resumeDirRefusal(d resumeDirDeps, path, sessionDir, id string, workspace bool, reason string, cause error, facts *resumeDirFacts) *resumeDirError {
	if workspace {
		reason = "workspace_" + reason
	}
	e := &resumeDirError{dir: path, sessionDir: sessionDir, id: id, reason: reason, cause: cause}
	if facts != nil {
		e.gid = facts.gid
		e.ownerKnown = facts.uid == d.uid && facts.mode&0002 == 0
	}
	return e
}

// Open exactly one readable directory descriptor. The fallback stat can choose
// a refusal only; it can never approve a directory the descriptor could not open.
func checkResumeDirectory(d resumeDirDeps, path, sessionDir, id string, workspace, enter bool) error {
	fd, err := d.open(path)
	if err != nil {
		reason := "unexamined"
		if errors.Is(err, os.ErrNotExist) {
			reason = "missing"
		} else if errors.Is(err, syscall.ENOTDIR) {
			reason = "not_dir"
		}
		var facts *resumeDirFacts
		if f, e := d.stat(path); e == nil {
			facts = &f
			if !f.directory {
				reason = "not_dir"
			} else if f.uid != d.uid {
				reason = "other_owner"
			} else if f.mode&0002 != 0 {
				reason = "other_write"
			}
		}
		return resumeDirRefusal(d, path, sessionDir, id, workspace, reason, err, facts)
	}
	defer fd.close()
	facts, err := fd.facts()
	refuse := func(reason string, err error) error {
		return resumeDirRefusal(d, path, sessionDir, id, workspace, reason, err, &facts)
	}
	if err != nil {
		return resumeDirRefusal(d, path, sessionDir, id, workspace, "unexamined", err, nil)
	}
	if !facts.directory {
		return refuse("not_dir", nil)
	}
	if facts.uid != d.uid {
		return refuse("other_owner", nil)
	}
	if facts.mode&0002 != 0 {
		return refuse("other_write", nil)
	}
	if facts.mode&0020 != 0 {
		if d.goos != "linux" {
			return refuse("not_linux", nil)
		}
		for _, name := range []string{"system.posix_acl_access", "system.nfs4_acl"} {
			present, err := fd.accessList(name)
			if err != nil || present {
				return refuse("acl", err)
			}
		}
		passwd, e1 := d.read("/etc/passwd")
		groups, e2 := d.read("/etc/group")
		private, name := resumePrivateGroup(passwd, groups, d.uid, facts.gid)
		if e1 != nil || e2 != nil || !private {
			e := resumeDirRefusal(d, path, sessionDir, id, workspace, "group_unconfirmed", nil, &facts)
			e.group = name
			return e
		}
	}
	if enter {
		if err := fd.enter(); err != nil {
			return refuse("unexamined", err)
		}
	}
	return nil
}

// Enter the recorded directory before any workspace, live-session or launch
// decision. Exact canonical equality retains the existing in-directory behavior.
func enterResumeDirectory(d resumeDirDeps, recorded, id string) (dir string, changed bool, err error) {
	caller, err := d.getwd()
	if err != nil {
		return "", false, err
	}
	if !filepath.IsAbs(recorded) || strings.ContainsRune(recorded, 0) {
		return "", false, resumeDirRefusal(d, recorded, recorded, id, false, "unexamined", errors.New("recorded path is not an absolute directory"), nil)
	}
	canonical, canonicalErr := d.canonical(recorded)
	current, currentErr := d.canonical(caller)
	if canonicalErr == nil && currentErr == nil && canonical == current {
		return caller, false, nil
	}
	if d.before != nil {
		if err := d.before(); err != nil {
			return "", false, fmt.Errorf("aiq: not resuming in %q: %w", recorded, err)
		}
	}
	if err := d.chdir(recorded); err != nil {
		reason := "unexamined"
		if errors.Is(err, os.ErrNotExist) {
			reason = "missing"
		} else if errors.Is(err, syscall.ENOTDIR) {
			reason = "not_dir"
		}
		e := resumeDirRefusal(d, recorded, recorded, id, false, reason, err, nil)
		e.ownerKnown = reason == "unexamined"
		return "", false, e
	}
	if err := checkResumeDirectory(d, ".", recorded, id, false, true); err != nil {
		var refusal *resumeDirError
		if errors.As(err, &refusal) {
			refusal.dir = recorded
		}
		return "", false, err
	}
	dir, err = d.getwd()
	return dir, true, err
}

func checkResumeWorkspace(d resumeDirDeps, entered, workspace, id string, changed bool) error {
	if !changed || workspace == entered {
		return nil
	}
	a, e1 := d.canonical(entered)
	b, e2 := d.canonical(workspace)
	if e1 == nil && e2 == nil && a == b {
		return nil
	}
	return checkResumeDirectory(d, workspace, entered, id, true, false)
}
