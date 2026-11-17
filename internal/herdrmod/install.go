// Package herdrmod distributes aiq's native Herdr app client. Installation is
// confined to its own personal mod; layouts, keys and shipped mods are untouched.
package herdrmod

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed mod/main.js mod/protocol.js mod/state.js mod/ui.js mod/markdown.js mod/mod.json mod/package.json
var assets embed.FS

const manifestName = ".aiq-managed.json"

// Herdr's checker/language tooling creates these beside the mod. Preserve
// their bytes on update, including personal edits; they are not aiq assets.
var generatedFiles = []string{"gpui-kit.d.ts", "herdr.d.ts", "jsconfig.json"}

type manifest struct {
	Owner string            `json:"owner"`
	Files map[string]string `json:"files"`
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// Install refuses to replace an unrelated mod or one the person has edited.
// Staging and rename keep hot reload from observing a half-written module set.
func Install(appHome string) error {
	if !filepath.IsAbs(appHome) {
		return errors.New("Herdr app home must be absolute")
	}
	mods := filepath.Join(appHome, "mods")
	if err := os.MkdirAll(mods, 0700); err != nil {
		return errors.New("cannot create the Herdr mods directory")
	}
	lock := filepath.Join(mods, ".aiq-install-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return errors.New("another aiq mod install is in progress; check the install lock")
	}
	defer os.Remove(lock)
	target := filepath.Join(mods, "aiq")
	exists := false
	if info, err := os.Lstat(target); err == nil {
		exists = true
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !managed(target) {
			return errors.New("aiq mod has local changes or belongs to another installation; preserve it before installing")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errors.New("cannot inspect the existing aiq mod")
	}
	stage, err := os.MkdirTemp(mods, ".aiq-stage-")
	if err != nil {
		return errors.New("cannot stage the aiq mod")
	}
	defer os.RemoveAll(stage)
	m := manifest{Owner: "aiq-herdr-mod-v1", Files: map[string]string{}}
	entries, _ := fs.ReadDir(assets, "mod")
	for _, entry := range entries {
		b, err := assets.ReadFile("mod/" + entry.Name())
		if err != nil {
			return errors.New("cannot read bundled aiq mod")
		}
		if err := os.WriteFile(filepath.Join(stage, entry.Name()), b, 0600); err != nil {
			return errors.New("cannot write the staged aiq mod")
		}
		m.Files[entry.Name()] = digest(b)
	}
	if exists {
		for _, name := range generatedFiles {
			path := filepath.Join(target, name)
			info, err := os.Lstat(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
				return errors.New("cannot preserve Herdr generated files")
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return errors.New("cannot preserve Herdr generated files")
			}
			if err := os.WriteFile(filepath.Join(stage, name), b, 0600); err != nil {
				return errors.New("cannot preserve Herdr generated files")
			}
		}
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(stage, manifestName), b, 0600); err != nil {
		return errors.New("cannot record the staged aiq mod")
	}
	backup := ""
	if exists {
		backup, err = os.MkdirTemp(mods, ".aiq-previous-")
		if err != nil {
			return errors.New("cannot prepare aiq mod replacement")
		}
		if err := os.Remove(backup); err != nil {
			return errors.New("cannot prepare aiq mod replacement")
		}
		if err := os.Rename(target, backup); err != nil {
			return errors.New("cannot preserve the previous aiq mod")
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if backup != "" {
			if restore := os.Rename(backup, target); restore != nil {
				return errors.New("aiq mod install and rollback failed; previous files remain in the mods directory")
			}
		}
		return errors.New("cannot install the aiq mod")
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return errors.New("aiq mod installed; previous managed files could not be removed")
		}
	}
	return nil
}

func managed(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, manifestName))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return false
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || m.Owner != "aiq-herdr-mod-v1" || len(m.Files) == 0 || len(m.Files) > 32 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == manifestName {
			continue
		}
		if _, ok := m.Files[name]; ok {
			continue
		}
		generated := false
		for _, allowed := range generatedFiles {
			generated = generated || name == allowed
		}
		if !generated {
			return false
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return false
		}
	}
	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if filepath.Base(name) != name || name == "." || name == manifestName {
			return false
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return false
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || digest(b) != m.Files[name] {
			return false
		}
	}
	return true
}
