package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/foxcpp/go-imap-maildir/maildir"
	"github.com/foxcpp/go-imap-maildir/maildir/metadata"
)

type metadataStore struct {
	storage *Storage
	prefix  string
}

func newMetadataStore(storage *Storage, prefix string) *metadataStore {
	return &metadataStore{storage: storage, prefix: prefix}
}

func (s *metadataStore) controlKey(name string) string {
	return joinKey(s.prefix, name)
}

func (s *metadataStore) resolveControlNames(state *maildir.Metadata) {
	state.SetUIDListName(metadata.UIDListFileName)
	state.SetKeywordsName(metadata.KeywordsFileName)
}

func (s *metadataStore) objectExists(key string) (bool, error) {
	_, err := s.storage.client.StatObject(context.Background(), s.storage.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
	key := s.controlKey(state.UIDListName())
	if cached, ok := s.storage.getCachedObject(key); ok {
		return metadata.ParseUIDList(bytes.NewReader(cached), state)
	}
	obj, err := s.storage.client.GetObject(context.Background(), s.storage.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			metadata.InitUIDList(state)
			return nil
		}
		return err
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			metadata.InitUIDList(state)
			return nil
		}
		return err
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			metadata.InitUIDList(state)
			return nil
		}
		return err
	}
	s.storage.setCachedObject(key, data)
	if err := metadata.ParseUIDList(bytes.NewReader(data), state); err != nil {
		return err
	}
	return nil
}

func (s *metadataStore) readKeywords(state *maildir.Metadata) error {
	key := s.controlKey(state.KeywordsName())
	if cached, ok := s.storage.getCachedObject(key); ok {
		return metadata.ReadKeywords(bytes.NewReader(cached), state)
	}
	obj, err := s.storage.client.GetObject(context.Background(), s.storage.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			return nil
		}
		return err
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			return nil
		}
		return err
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			s.storage.deleteCachedObject(key)
			return nil
		}
		return err
	}
	s.storage.setCachedObject(key, data)
	if err := metadata.ReadKeywords(bytes.NewReader(data), state); err != nil {
		return err
	}
	return nil
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
	key := s.controlKey(state.UIDListName())
	buf := &bytes.Buffer{}
	if err := metadata.WriteUIDList(buf, state); err != nil {
		return err
	}
	_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, key, buf, int64(buf.Len()), minio.PutObjectOptions{})
	if err == nil {
		s.storage.setCachedObject(key, buf.Bytes())
	}
	return err
}

func (s *metadataStore) writeKeywords(state *maildir.Metadata) error {
	key := s.controlKey(state.KeywordsName())
	buf := &bytes.Buffer{}
	if err := metadata.WriteKeywords(buf, state); err != nil {
		return err
	}
	_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, key, buf, int64(buf.Len()), minio.PutObjectOptions{})
	if err == nil {
		s.storage.setCachedObject(key, buf.Bytes())
	}
	return err
}

func (s *metadataStore) withUidlistLock(state *maildir.Metadata, fn func() error) error {
	lockName := metadata.UIDListLockFileName
	lockKey := s.controlKey(lockName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		exists, err := s.objectExists(lockKey)
		if err != nil {
			return err
		}
		if !exists {
			_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, lockKey, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
			if err == nil {
				defer s.storage.client.RemoveObject(context.Background(), s.storage.bucket, lockKey, minio.RemoveObjectOptions{})
				return fn()
			}
		}
		if time.Now().After(deadline) {
			return errors.New("maildir: uidlist lock timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func pathBase(value string) string {
	return metadata.BaseName(value)
}
