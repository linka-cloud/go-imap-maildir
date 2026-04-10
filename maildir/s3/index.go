package s3

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	imapmaildir "github.com/foxcpp/go-imap-maildir/maildir"
	maildirindex "github.com/foxcpp/go-imap-maildir/maildir/index"
)

const (
	indexPrefixName   = "index"
	indexSnapshotName = "snapshot"
	indexLogPrefix    = "log"
	indexLockName     = "lock"
)

type indexStore struct {
	storage *Storage
	prefix  string
	guid    string

	appendCount int
}

func newIndexStore(storage *Storage, prefix, guid string) *indexStore {
	return &indexStore{storage: storage, prefix: prefix, guid: guid}
}

func (s *indexStore) indexPrefix() string {
	return joinKey(s.prefix, indexPrefixName)
}

func (s *indexStore) snapshotKey() string {
	return joinKey(s.indexPrefix(), indexSnapshotName)
}

func (s *indexStore) logPrefix() string {
	return joinKey(s.indexPrefix(), indexLogPrefix)
}

func (s *indexStore) lockKey() string {
	return joinKey(s.indexPrefix(), indexLockName)
}

func (s *indexStore) Load() (*maildirindex.Index, error) {
	idx := maildirindex.New(s.guid)
	if err := s.loadSnapshot(idx); err != nil {
		return nil, err
	}
	if err := s.loadLog(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func (s *indexStore) Append(records ...maildirindex.Record) error {
	if len(records) == 0 {
		return nil
	}
	segmentKey := joinKey(s.logPrefix(), s.segmentName())
	buf := &bytes.Buffer{}
	if err := maildirindex.WriteFileHeader(buf, 2, s.guid); err != nil {
		return err
	}
	for _, record := range records {
		if err := maildirindex.WriteRecord(buf, record); err != nil {
			return err
		}
		s.appendCount++
	}
	_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, segmentKey, buf, int64(buf.Len()), minio.PutObjectOptions{})
	if err == nil {
		s.storage.invalidateList(s.logPrefix())
	}
	return err
}

func (s *indexStore) Snapshot(index *maildirindex.Index) error {
	if index == nil {
		return nil
	}
	return s.withLock(func() error {
		buf := &bytes.Buffer{}
		if err := maildirindex.WriteFileHeader(buf, 1, s.guid); err != nil {
			return err
		}
		for _, entry := range index.Entries() {
			if err := maildirindex.WriteRecord(buf, maildirindex.UpsertRecord(entry)); err != nil {
				return err
			}
		}
		key := s.snapshotKey()
		_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, key, buf, int64(buf.Len()), minio.PutObjectOptions{})
		if err != nil {
			return err
		}
		s.storage.setCachedObject(key, buf.Bytes())
		if err := s.deleteLogSegments(); err != nil {
			return err
		}
		s.appendCount = 0
		return nil
	})
}

func (s *indexStore) loadSnapshot(idx *maildirindex.Index) error {
	key := s.snapshotKey()
	if cached, ok := s.storage.getCachedObject(key); ok {
		return s.readIndexFrom(bytes.NewReader(cached), idx, 1)
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
		return err
	}
	s.storage.setCachedObject(key, data)
	return s.readIndexFrom(bytes.NewReader(data), idx, 1)
}

func (s *indexStore) loadLog(idx *maildirindex.Index) error {
	prefix := s.logPrefix()
	keys, err := s.storage.listKeys(prefix)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		obj, err := s.storage.client.GetObject(context.Background(), s.storage.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		if _, err := obj.Stat(); err != nil {
			_ = obj.Close()
			if isNotFound(err) {
				continue
			}
			return err
		}
		if err := s.readIndexFrom(obj, idx, 2); err != nil {
			_ = obj.Close()
			return err
		}
		_ = obj.Close()
	}
	return nil
}

func (s *indexStore) readIndexFrom(r io.Reader, idx *maildirindex.Index, expectedType uint8) error {
	reader := bufio.NewReader(r)
	header, err := maildirindex.ReadFileHeader(reader)
	if err != nil {
		return nil
	}
	if header.Version != 1 || header.FileType != expectedType || (header.GUID != "" && header.GUID != s.guid) {
		return nil
	}
	return maildirindex.ReadRecords(reader, func(record maildirindex.Record) error {
		if record.Type == maildirindex.RecordUpsert && record.Entry != nil {
			idx.Upsert(record.Entry)
		} else if record.Type == maildirindex.RecordDelete {
			idx.Delete(record.Key)
		}
		return nil
	})
}

func (s *indexStore) deleteLogSegments() error {
	prefix := s.logPrefix()
	keys, err := s.storage.listKeys(prefix)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if err := s.storage.removeKeys(keys); err != nil {
		return err
	}
	s.storage.invalidateList(prefix)
	return nil
}

func (s *indexStore) withLock(fn func() error) error {
	lockKey := s.lockKey()
	owner := newGUID()
	data := []byte(owner)
	deadline := time.Now().Add(5 * time.Second)
	for {
		opts := minio.PutObjectOptions{}
		opts.SetMatchETagExcept("*")
		_, err := s.storage.client.PutObject(context.Background(), s.storage.bucket, lockKey, bytes.NewReader(data), int64(len(data)), opts)
		if err == nil {
			defer func() {
				_ = s.storage.client.RemoveObject(context.Background(), s.storage.bucket, lockKey, minio.RemoveObjectOptions{})
			}()
			return fn()
		}
		if !isLockBusy(err) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("maildir: index lock timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isLockBusy(err error) bool {
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusPreconditionFailed {
		return true
	}
	return resp.Code == "PreconditionFailed"
}

func (s *indexStore) segmentName() string {
	return fmt.Sprintf("%020d-%s", time.Now().UnixNano(), newGUID())
}

func (d *Dir) IndexStore(guid string) (imapmaildir.IndexStore, error) {
	if guid == "" {
		return nil, errors.New("maildir: missing mailbox GUID")
	}
	if err := d.ensurePrefix(false); err != nil {
		return nil, err
	}
	return newIndexStore(d.storage, d.prefix, guid), nil
}
