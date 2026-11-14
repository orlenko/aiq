package longrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retirementFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	path := HandoffPath("/invented/work")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invented next step: "+fakeInvite), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1234567890, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path, stamp
}

func TestNoteRetirementRedactsBeforePublishingAndDoesNotClobberArchive(t *testing.T) {
	path, stamp := retirementFixture(t)
	now := time.Unix(1500000000, 0)
	old := strings.TrimSuffix(path, ".md") + "." + now.Format("20060102-150405") + ".md"
	if err := os.WriteFile(old, []byte("invented earlier archive"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareNoteRetirement("/invented/work")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), fakeInvite) {
		t.Fatal("preparation changed current note")
	}
	scratch, err := os.ReadFile(plan.temp)
	if err != nil || strings.Contains(string(scratch), fakeInvite) || !strings.Contains(string(scratch), "[redacted orchestra invite]") {
		t.Fatal("scratch contains unredacted material", err)
	}
	archive, err := plan.Retire(now)
	if err != nil || archive == old || archive == "" {
		t.Fatal("exclusive archive failed", archive, err)
	}
	data, err := os.ReadFile(archive)
	if err != nil || string(data) != string(scratch) {
		t.Fatal("archive differed from sanitized preparation", err)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0600 || !info.ModTime().Equal(stamp) {
		t.Fatal("archive lost privacy/time", err)
	}
	prior, _ := os.ReadFile(old)
	if string(prior) != "invented earlier archive" {
		t.Fatal("existing archive overwritten")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("current note not retired", err)
	}
	plan.Close()
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".redact-*")); len(files) != 0 {
		t.Fatal("scratch leaked")
	}
}

func TestNoteRetirementVersionChangesLeaveCurrentAndArchivesAlone(t *testing.T) {
	for _, change := range []string{"replace", "write", "symlink", "remove", "appeared"} {
		t.Run(change, func(t *testing.T) {
			path, _ := retirementFixture(t)
			if change == "appeared" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := PrepareNoteRetirement("/invented/work")
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			switch change {
			case "replace":
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, []byte("new note"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "write", "appeared":
				if err := os.WriteFile(path, []byte("changed note"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("invented-missing", path); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := plan.Retire(time.Unix(1500000000, 0)); err == nil {
				t.Fatal("changed note was retired")
			}
			if archives, _ := filepath.Glob(strings.TrimSuffix(path, ".md") + ".*.md"); len(archives) != 0 {
				t.Fatal("changed note produced archive")
			}
			if change == "write" || change == "appeared" {
				if b, _ := os.ReadFile(path); string(b) != "changed note" {
					t.Fatal("changed current note overwritten")
				}
			}
			plan.Close()
			if files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".redact-*")); len(files) != 0 {
				t.Fatal("scratch leaked")
			}
		})
	}
}

func TestNoteRetirementMissingAndUnusableSources(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	plan, err := PrepareNoteRetirement("/invented/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if dst, err := plan.Retire(time.Unix(1500000000, 0)); err != nil || dst != "" {
		t.Fatal("absent note rejected", dst, err)
	}
	if _, err := os.Stat(HandoffDir()); !os.IsNotExist(err) {
		t.Fatal("absent note created directory")
	}
	path := HandoffPath("/invented/directory")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if plan, err := PrepareNoteRetirement("/invented/directory"); err == nil || plan != nil {
		t.Fatal("nonregular note accepted")
	}
}

func TestNoteRetirementCancellationAndArchiveCapacityPreserveNotes(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "busy archive names"}[full], func(t *testing.T) {
			path, _ := retirementFixture(t)
			plan, err := PrepareNoteRetirement("/invented/work")
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			if full {
				now := time.Unix(1500000000, 0)
				for i := 0; i < 60; i++ {
					dst := strings.TrimSuffix(path, ".md") + "." + now.Add(time.Duration(i)*time.Second).Format("20060102-150405") + ".md"
					if err := os.WriteFile(dst, []byte("earlier archive"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if archive, err := plan.Retire(now); err == nil || archive != "" {
					t.Fatal("full archive namespace overwrote a note", archive, err)
				}
				text, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(text), fakeInvite) || !strings.Contains(string(text), "[redacted orchestra invite]") {
					t.Fatal("failed publication lost redacted current note", err)
				}
			} else {
				plan.Close()
				text, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(text), fakeInvite) {
					t.Fatal("cancel changed original note", err)
				}
			}
			if files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".redact-*")); len(files) != 0 {
				t.Fatal("scratch leaked")
			}
		})
	}
}
