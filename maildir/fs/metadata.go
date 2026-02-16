package fs

import (
	"bytes"
	"os"
	"path/filepath"
	"time"

	"github.com/foxcpp/go-imap-maildir/maildir"
	"github.com/foxcpp/go-imap-maildir/maildir/metadata"
)

const (
	legacyUIDListFileName  = "dovecot-uidlist"
	legacyUIDListLockName  = "dovecot-uidlist.lock"
	legacyKeywordsFileName = "dovecot-keywords"
	legacyAttributesName   = "dovecot-attributes"
)

type metadataStore struct {
	basePath string
}

func newMetadataStore(basePath string) *metadataStore {
	return &metadataStore{basePath: basePath}
}

func (s *metadataStore) controlPath(name string) string {
	return filepath.Join(s.basePath, name)
}

func (s *metadataStore) resolveControlNames(state *maildir.Metadata) {
	state.SetUIDListName(metadata.UIDListFileName)
	state.SetKeywordsName(metadata.KeywordsFileName)
}

func (s *metadataStore) Load(state *maildir.Metadata) error {
	if state.Loaded {
		return nil
	}
	state.Ensure()
	s.resolveControlNames(state)

	if err := s.readUidlist(state); err != nil {
		return err
	}
	if err := s.readKeywords(state); err != nil {
		return err
	}

	state.Loaded = true
	return nil
}

func (s *metadataStore) readUidlist(state *maildir.Metadata) error {
	path := s.controlPath(metadata.UIDListFileName)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			legacyPath := s.controlPath(legacyUIDListFileName)
			legacyFile, legacyErr := os.Open(legacyPath)
			if legacyErr != nil {
				if os.IsNotExist(legacyErr) {
					metadata.InitUIDList(state)
					return nil
				}
				return legacyErr
			}
			defer legacyFile.Close()
			return metadata.ParseUIDList(legacyFile, state)
		}
		return err
	}
	defer file.Close()
	return metadata.ParseUIDList(file, state)
}

func (s *metadataStore) readKeywords(state *maildir.Metadata) error {
	path := s.controlPath(metadata.KeywordsFileName)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			legacyPath := s.controlPath(legacyKeywordsFileName)
			legacyFile, legacyErr := os.Open(legacyPath)
			if legacyErr != nil {
				if os.IsNotExist(legacyErr) {
					return nil
				}
				return legacyErr
			}
			defer legacyFile.Close()
			return metadata.ReadKeywords(legacyFile, state)
		}
		return err
	}
	defer file.Close()
	return metadata.ReadKeywords(file, state)
}

func (s *metadataStore) Write(state *maildir.Metadata) error {
	if !state.DirtyUIDList && !state.DirtyKeywords {
		return nil
	}
	return s.withUidlistLock(state, func() error {
		if state.DirtyKeywords {
			if err := s.writeKeywords(state); err != nil {
				return err
			}
			state.DirtyKeywords = false
		}
		if state.DirtyUIDList {
			if err := s.writeUidlist(state); err != nil {
				return err
			}
			state.DirtyUIDList = false
		}
		return nil
	})
}

func (s *metadataStore) writeUidlist(state *maildir.Metadata) error {
	path := s.controlPath(state.UIDListName())
	tmp := path + ".tmp"

	buf := &bytes.Buffer{}
	if err := metadata.WriteUIDList(buf, state); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *metadataStore) writeKeywords(state *maildir.Metadata) error {
	path := s.controlPath(state.KeywordsName())
	tmp := path + ".tmp"

	buf := &bytes.Buffer{}
	if err := metadata.WriteKeywords(buf, state); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *metadataStore) withUidlistLock(state *maildir.Metadata, fn func() error) error {
	lockName := metadata.UIDListLockFileName
	lockPath := s.controlPath(lockName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = file.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !os.IsExist(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readAttributes(basePath string) (map[string]string, error) {
	path := attributesPath(basePath)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer file.Close()
	return metadata.ReadAttributes(file)
}

func writeAttributes(basePath string, attrs map[string]string) error {
	path := attributesPath(basePath)
	return withAttributesLock(basePath, func() error {
		buf := &bytes.Buffer{}
		if err := metadata.WriteAttributes(buf, attrs); err != nil {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	})
}

func attributesPath(basePath string) string {
	path := filepath.Join(basePath, metadata.AttributesFileName)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	legacyPath := filepath.Join(basePath, legacyAttributesName)
	if _, err := os.Stat(legacyPath); err == nil {
		return legacyPath
	}
	return path
}

func withAttributesLock(basePath string, fn func() error) error {
	lockPath := attributesPath(basePath) + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = file.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !os.IsExist(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *metadataStore) controlExists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.basePath, name))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
