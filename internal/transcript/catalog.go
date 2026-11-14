package transcript

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// AllDiscovery is an explicit cross-directory read. An empty directory never
// silently selects it through the ordinary Discovery API.
type AllDiscovery interface {
	Discovery
	AllCandidates(context.Context) ([]Candidate, error)
}

// AllCandidates enumerates configured provider roots only. It does not decode
// a Claude project folder back into a directory or treat a backend as a source.
func (x *Index) AllCandidates(ctx context.Context) ([]Candidate, error) {
	var out []Candidate
	seen := map[string]bool{}
	add := func(c Candidate) {
		key := c.Provider + "\x00" + c.Path
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	for _, root := range x.r.ClaudeProjects {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		projects, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, project := range projects {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !project.IsDir() {
				if project.Type()&os.ModeSymlink == 0 {
					continue
				}
				info, err := os.Stat(filepath.Join(root, project.Name()))
				if err != nil {
					return out, err
				}
				if !info.IsDir() {
					continue
				}
			}
			files, err := os.ReadDir(filepath.Join(root, project.Name()))
			if err != nil {
				return out, err
			}
			for _, file := range files {
				if err := ctx.Err(); err != nil {
					return out, err
				}
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
					continue
				}
				add(Candidate{Provider: "claude", Path: filepath.Join(root, project.Name(), file.Name()), Root: root, Account: x.r.Labels[root]})
			}
		}
	}
	cs, err := x.codexScan(ctx)
	if err != nil {
		return out, err
	}
	for _, c := range cs.all {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		add(c)
	}
	for _, root := range x.r.AgyAppData {
		associations := map[string]string{}
		for dir, ids := range agyAbsoluteWorkspaces(ctx, root) {
			for _, id := range ids {
				if previous, ok := associations[id]; ok && previous != dir {
					associations[id] = ""
				} else if !ok {
					associations[id] = dir
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		entries, err := os.ReadDir(filepath.Join(root, "brain"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !ValidID(entry.Name()) {
				continue
			}
			path := AgyTranscript(root, entry.Name())
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return out, err
			}
			if info.Mode().IsRegular() {
				add(Candidate{Provider: "agy", Path: path, Root: root, Account: x.r.Labels[root], Directory: associations[entry.Name()]})
			}
		}
	}
	return out, ctx.Err()
}

type allDiscovery struct{ AllDiscovery }

func (a allDiscovery) Candidates(ctx context.Context, _ string) ([]Candidate, error) {
	return a.AllCandidates(ctx)
}

// ListAll uses the ordinary collector, deduplication and eight-parser lifetime,
// without the single-directory filter. The caller supplies the same deadline
// and response admissions as an ordinary directory list.
func ListAll(ctx context.Context, src AllDiscovery, parse func(context.Context, string) (Summary, error)) (ListResult, error) {
	return listWith(ctx, allDiscovery{src}, "", parse, false)
}
