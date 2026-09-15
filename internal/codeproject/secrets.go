package codeproject

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Access tokens live in their own file, never in projects.json.
//
// projects.json is the file an operator reads, diffs, copies to another host
// and includes in a backup — it is handled like configuration because that is
// what it is. A credential in it would ride along on every one of those. Here
// the two have different lifetimes and different handling, and the code that
// serves the project list to the console cannot reach the token even by
// accident, because it is not in the struct it serializes.
//
// The protection at rest is file permissions: 0600 inside a 0700 directory,
// same as the config file that already holds provider API keys. Encrypting it
// with a key stored beside it would look stronger and defend against nothing.
const secretsFile = "secrets.json"

func (s *Store) secretsPath() string { return filepath.Join(s.dir, secretsFile) }

func (s *Store) readSecrets() (map[string]string, error) {
	b, err := os.ReadFile(s.secretsPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) writeSecrets(m map[string]string) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	if len(m) == 0 {
		err := os.Remove(s.secretsPath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// CreateTemp makes the file 0600, and the rename carries that mode across,
	// so the token is never briefly world-readable.
	f, err := os.CreateTemp(s.dir, ".secrets-")
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
	return os.Rename(f.Name(), s.secretsPath())
}

// SetToken stores (or, given an empty token, clears) one project's credential.
func (s *Store) SetToken(id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setTokenLocked(id, token)
}

func (s *Store) setTokenLocked(id, token string) error {
	m, err := s.readSecrets()
	if err != nil {
		return err
	}
	if token == "" {
		if _, ok := m[id]; !ok {
			return nil
		}
		delete(m, id)
	} else {
		m[id] = token
	}
	return s.writeSecrets(m)
}

// HasToken reports whether a credential is stored, which is the only thing
// about it any API ever returns.
func (s *Store) HasToken(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.readSecrets()
	return err == nil && m[id] != ""
}

// tokenFor is the single reader of the secret, used by clone. Unexported: no
// caller outside this package can obtain the value.
func (s *Store) tokenFor(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.readSecrets()
	if err != nil {
		return ""
	}
	return m[id]
}
