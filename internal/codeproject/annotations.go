package codeproject

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Annotations are stored per REPOSITORY, not per project.
//
// "services/order is the order service" is a fact about the repository. Storing
// it on the project made it a fact about the project, and a monorepo normally
// carries several: a coupon project and a payment project over the same repo
// each had to be labelled separately, the generated labels disagreed about the
// same directory, and editing one never reached the other. Keying by repository
// means you label once and every project over that repository benefits.
//
// What stays per project is what an agent may SEE: hydrate filters the
// catalogue down to each project's analysis scope, so a project scoped to the
// coupon service still learns nothing about the payment service's directories.
const annotationsFile = "annotations.json"

type repoAnnotations struct {
	// Annotations and Drafts are repoKey -> repo-relative path -> labels.
	Annotations map[string]map[string]DirectoryInfo `json:"annotations,omitempty"`
	Drafts      map[string]map[string]DirectoryInfo `json:"drafts,omitempty"`
}

// RepoKey normalizes a clone URL so the same repository spelled two ways shares
// one catalogue. Case in the host and a trailing ".git" are not different
// repositories; a different path is.
func RepoKey(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

func (s *Store) annotationsPath() string { return filepath.Join(s.dir, annotationsFile) }

func (s *Store) readAnnotations() (repoAnnotations, error) {
	var a repoAnnotations
	b, err := os.ReadFile(s.annotationsPath())
	if errors.Is(err, os.ErrNotExist) {
		return repoAnnotations{}, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return repoAnnotations{}, err
	}
	return a, nil
}

func (s *Store) writeAnnotations(a repoAnnotations) error {
	for key, m := range a.Annotations {
		if len(m) == 0 {
			delete(a.Annotations, key)
		}
	}
	for key, m := range a.Drafts {
		if len(m) == 0 {
			delete(a.Drafts, key)
		}
	}
	if len(a.Annotations) == 0 && len(a.Drafts) == 0 {
		err := os.Remove(s.annotationsPath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".annotations-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.annotationsPath())
}

// scoped narrows a repository's catalogue to what one project may see. A
// project scoped to services/discount_coupon must not learn that
// services/silkworm_pay exists, which is the same rule list_project_dir follows
// when it refuses to list siblings.
func scoped(catalogue map[string]DirectoryInfo, scope []string) map[string]DirectoryInfo {
	if len(catalogue) == 0 {
		return nil
	}
	out := map[string]DirectoryInfo{}
	for path, info := range catalogue {
		if withinScope(path, scope) {
			out[path] = info
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hydrate fills in each project's view of the repository catalogue. Callers hold
// at least a read lock.
func (s *Store) hydrate(ps []Project) []Project {
	a, err := s.readAnnotations()
	if err != nil {
		return ps
	}
	for i := range ps {
		key := RepoKey(ps[i].URL)
		scope := ps[i].Scope()
		ps[i].DirectoryMeta = scoped(a.Annotations[key], scope)
		ps[i].DirectoryDrafts = scoped(a.Drafts[key], scope)
	}
	return ps
}

// editAnnotations applies fn to one repository's catalogue, under the write
// lock, with the edit validated against the project that requested it.
func (s *Store) editAnnotations(projectID string, fn func(p Project, ann, drafts map[string]DirectoryInfo) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	var project *Project
	for i := range ps {
		if ps[i].ID == projectID {
			project = &ps[i]
		}
	}
	if project == nil {
		return ErrNotFound
	}
	a, err := s.readAnnotations()
	if err != nil {
		return err
	}
	key := RepoKey(project.URL)
	if a.Annotations == nil {
		a.Annotations = map[string]map[string]DirectoryInfo{}
	}
	if a.Drafts == nil {
		a.Drafts = map[string]map[string]DirectoryInfo{}
	}
	if a.Annotations[key] == nil {
		a.Annotations[key] = map[string]DirectoryInfo{}
	}
	if a.Drafts[key] == nil {
		a.Drafts[key] = map[string]DirectoryInfo{}
	}
	if err := fn(*project, a.Annotations[key], a.Drafts[key]); err != nil {
		return err
	}
	scope := project.Scope()
	if err := validateAnnotations(a.Annotations[key], scope, "目录说明"); err != nil {
		return err
	}
	if err := validateAnnotations(a.Drafts[key], scope, "目录说明草稿"); err != nil {
		return err
	}
	return s.writeAnnotations(a)
}

// migrateAnnotations moves catalogues that were stored on projects into the
// repository store. Runs once per process, beside the interrupted-sync sweep.
func (s *Store) migrateAnnotations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return
	}
	a, err := s.readAnnotations()
	if err != nil {
		return
	}
	changed := false
	for i := range ps {
		if len(ps[i].DirectoryMeta) == 0 && len(ps[i].DirectoryDrafts) == 0 {
			continue
		}
		key := RepoKey(ps[i].URL)
		if a.Annotations == nil {
			a.Annotations = map[string]map[string]DirectoryInfo{}
		}
		if a.Drafts == nil {
			a.Drafts = map[string]map[string]DirectoryInfo{}
		}
		if a.Annotations[key] == nil {
			a.Annotations[key] = map[string]DirectoryInfo{}
		}
		if a.Drafts[key] == nil {
			a.Drafts[key] = map[string]DirectoryInfo{}
		}
		// An existing repository entry wins: two projects over one repository
		// may both carry a label for the same path, and the first one migrated
		// is as good a tie-break as any — the alternative is losing one.
		for path, info := range ps[i].DirectoryMeta {
			if _, exists := a.Annotations[key][path]; !exists {
				a.Annotations[key][path] = info
			}
		}
		for path, info := range ps[i].DirectoryDrafts {
			if _, exists := a.Drafts[key][path]; !exists {
				a.Drafts[key][path] = info
			}
		}
		changed = true
	}
	if !changed {
		return
	}
	if err := s.writeAnnotations(a); err != nil {
		return
	}
	// write() strips the per-project copies, so this persists the move.
	_ = s.write(ps)
}

// dropRepoAnnotations removes a repository's catalogue once no project refers
// to it any more. While another project still does, the labels stay: they
// describe the repository, not the project being deleted.
func (s *Store) dropRepoAnnotations(remaining []Project, url string) {
	key := RepoKey(url)
	for _, p := range remaining {
		if RepoKey(p.URL) == key {
			return
		}
	}
	a, err := s.readAnnotations()
	if err != nil {
		return
	}
	delete(a.Annotations, key)
	delete(a.Drafts, key)
	_ = s.writeAnnotations(a)
}
