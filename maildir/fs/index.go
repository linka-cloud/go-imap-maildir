package fs

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"time"

	imapmaildir "github.com/foxcpp/go-imap-maildir/maildir"
	maildirindex "github.com/foxcpp/go-imap-maildir/maildir/index"
)

const (
	indexDirName      = "index"
	indexSnapshotName = "snapshot"
	indexLogName      = "log"
	indexLockName     = "lock"
)

type indexStore struct {
	basePath string
	guid     string

	appendCount int
}

func newIndexStore(basePath, guid string) *indexStore {
	return &indexStore{basePath: basePath, guid: guid}
}

func (s *indexStore) indexDir() string {
	return filepath.Join(s.basePath, indexDirName)
}

func (s *indexStore) snapshotPath() string {
	return filepath.Join(s.indexDir(), indexSnapshotName)
}

func (s *indexStore) logPath() string {
	return filepath.Join(s.indexDir(), indexLogName)
}

func (s *indexStore) lockPath() string {
	return filepath.Join(s.indexDir(), indexLockName)
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
	if err := os.MkdirAll(s.indexDir(), 0700); err != nil {
		return err
	}
	path := s.logPath()
	writeHeader := false
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		writeHeader = true
	} else if info.Size() == 0 {
		writeHeader = true
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if writeHeader {
		if err := maildirindex.WriteFileHeader(file, 2, s.guid); err != nil {
			return err
		}
	}
	for _, record := range records {
		if err := maildirindex.WriteRecord(file, record); err != nil {
			return err
		}
		s.appendCount++
	}
	return nil
}

func (s *indexStore) Snapshot(index *maildirindex.Index) error {
	if index == nil {
		return nil
	}
	if err := os.MkdirAll(s.indexDir(), 0700); err != nil {
		return err
	}
	return s.withLock(func() error {
		path := s.snapshotPath()
		tmp := path + ".tmp"
		buf := &bytes.Buffer{}
		if err := maildirindex.WriteFileHeader(buf, 1, s.guid); err != nil {
			return err
		}
		for _, entry := range index.Entries() {
			if err := maildirindex.WriteRecord(buf, maildirindex.UpsertRecord(entry)); err != nil {
				return err
			}
		}
		if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		_ = os.Remove(s.logPath())
		s.appendCount = 0
		return nil
	})
}

func (s *indexStore) loadSnapshot(idx *maildirindex.Index) error {
	path := s.snapshotPath()
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	header, err := maildirindex.ReadFileHeader(reader)
	if err != nil {
		return nil
	}
	if header.Version != 1 || header.FileType != 1 || (header.GUID != "" && header.GUID != s.guid) {
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

func (s *indexStore) loadLog(idx *maildirindex.Index) error {
	path := s.logPath()
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	header, err := maildirindex.ReadFileHeader(reader)
	if err != nil {
		return nil
	}
	if header.Version != 1 || header.FileType != 2 || (header.GUID != "" && header.GUID != s.guid) {
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

func (s *indexStore) withLock(fn func() error) error {
	lockPath := s.lockPath()
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = file.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *Dir) IndexStore(guid string) (imapmaildir.IndexStore, error) {
	if guid == "" {
		return nil, errors.New("maildir: missing mailbox GUID")
	}
	return newIndexStore(string(d.dir), guid), nil
}
