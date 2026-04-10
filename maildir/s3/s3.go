package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/minio/minio-go/v7"

	"github.com/foxcpp/go-imap-maildir/maildir"
	"github.com/foxcpp/go-imap-maildir/maildir/metadata"
)

const (
	hierarchySep   = "."
	inboxName      = "INBOX"
	maxNesting     = 100
	registryName   = "mailboxes"
	mboxPrefixName = "mbox"
	blobPrefixName = "blobs"
	markerName     = "mailbox"

	defaultCacheBytes    int64 = 64 << 20
	defaultCacheItemSize int64 = 4 << 20
)

type Storage struct {
	client     *minio.Client
	bucket     string
	rootPrefix string
	basePath   string

	cache            *ristretto.Cache[string, []byte]
	maxCacheItemSize int64
}

func (s *Storage) Dir(name string) (maildir.Dir, error) {
	return &Dir{
		storage: s,
		name:    name,
	}, nil
}

func (s *Storage) ListDirs() ([]string, error) {
	registryPrefix := s.registryPrefix()
	keys, err := s.listKeys(registryPrefix)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for _, key := range keys {
		rel := strings.TrimPrefix(key, registryPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		seen[rel] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		if strings.EqualFold(name, inboxName) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Storage) listRegistryNames() ([]string, error) {
	registryPrefix := s.registryPrefix()
	keys, err := s.listKeys(registryPrefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		rel := strings.TrimPrefix(key, registryPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		names = append(names, rel)
	}
	return names, nil
}

func (s *Storage) Attributes() (maildir.AttributesStore, error) {
	return &attributesStore{storage: s}, nil
}

func (s *Storage) registryPrefix() string {
	return joinKey(s.rootPrefix, s.basePath, registryName)
}

func (s *Storage) basePrefix() string {
	return joinKey(s.rootPrefix, s.basePath)
}

func (s *Storage) mboxRootPrefix() string {
	return joinKey(s.rootPrefix, s.basePath, mboxPrefixName)
}

func (s *Storage) blobRootPrefix() string {
	return joinKey(s.rootPrefix, s.basePath, blobPrefixName)
}

func (s *Storage) registryKey(name string) string {
	return joinKey(s.registryPrefix(), normalizeMailboxName(name))
}

func (s *Storage) readRegistry(name string) (string, error) {
	key := s.registryKey(name)
	obj, err := s.client.GetObject(context.Background(), s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			s.deleteCachedObject(key)
			return "", maildir.ErrNotExist
		}
		return "", err
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		if isNotFound(err) {
			s.deleteCachedObject(key)
			return "", maildir.ErrNotExist
		}
		return "", err
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			s.deleteCachedObject(key)
			return "", maildir.ErrNotExist
		}
		return "", err
	}
	s.setCachedObject(key, data)
	guid := strings.TrimSpace(string(data))
	if guid == "" {
		return "", errors.New("maildir: empty mailbox registry")
	}
	return guid, nil
}

func (s *Storage) writeRegistry(name, guid string) error {
	key := s.registryKey(name)
	buf := bytes.NewBufferString(guid)
	_, err := s.client.PutObject(context.Background(), s.bucket, key, buf, int64(buf.Len()), minio.PutObjectOptions{})
	if err == nil {
		s.setCachedObject(key, []byte(guid))
		s.invalidateList(s.registryPrefix())
	}
	return err
}

func (s *Storage) deleteRegistry(name string) error {
	key := s.registryKey(name)
	err := s.client.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{})
	if err == nil {
		s.deleteCachedObject(key)
		s.invalidateList(s.registryPrefix())
	}
	return err
}

func (s *Storage) listKeys(prefix string) ([]string, error) {
	if keys, ok := s.getCachedList(prefix); ok {
		return keys, nil
	}
	var keys []string
	for obj := range s.client.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	s.setCachedList(prefix, keys)
	return keys, nil
}

func (s *Storage) removeKeys(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for _, key := range keys {
			if key == "" {
				continue
			}
			objectsCh <- minio.ObjectInfo{Key: key}
		}
	}()
	var firstErr error
	for err := range s.client.RemoveObjects(context.Background(), s.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if err.Err == nil {
			continue
		}
		if firstErr == nil {
			firstErr = err.Err
		}
	}
	return firstErr
}

func (s *Storage) objectExists(key string) (bool, error) {
	_, err := s.client.StatObject(context.Background(), s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Storage) getCachedObject(key string) ([]byte, bool) {
	return s.getCachedBytes("obj:" + key)
}

func (s *Storage) setCachedObject(key string, data []byte) {
	s.setCachedBytes("obj:"+key, data)
}

func (s *Storage) deleteCachedObject(key string) {
	s.deleteCachedKey("obj:" + key)
}

func (s *Storage) getCachedList(prefix string) ([]string, bool) {
	data, ok := s.getCachedBytes("list:" + prefix)
	if !ok {
		return nil, false
	}
	return decodeListCache(data), true
}

func (s *Storage) setCachedList(prefix string, keys []string) {
	s.setCachedBytes("list:"+prefix, encodeListCache(keys))
}

func (s *Storage) invalidateList(prefix string) {
	s.deleteCachedKey("list:" + prefix)
}

func (s *Storage) invalidateListForKey(key string) {
	if key == "" {
		return
	}
	if idx := strings.Index(key, "/cur/"); idx > 0 {
		base := key[:idx]
		s.invalidateList(base)
		s.invalidateList(base + "/cur")
		return
	}
	if idx := strings.Index(key, "/new/"); idx > 0 {
		base := key[:idx]
		s.invalidateList(base)
		s.invalidateList(base + "/new")
		return
	}
}

func (s *Storage) getCachedBytes(key string) ([]byte, bool) {
	if s.cache == nil {
		return nil, false
	}
	data, ok := s.cache.Get(key)
	if !ok {
		return nil, false
	}
	return bytes.Clone(data), true
}

func (s *Storage) setCachedBytes(key string, data []byte) {
	if s.cache == nil || data == nil {
		return
	}
	copyData := bytes.Clone(data)
	_ = s.cache.Set(key, copyData, int64(len(copyData)))
}

func (s *Storage) deleteCachedKey(key string) {
	if s.cache == nil {
		return
	}
	s.cache.Del(key)
}

func encodeListCache(keys []string) []byte {
	if len(keys) == 0 {
		return nil
	}
	return []byte(strings.Join(keys, "\x1f"))
}

func decodeListCache(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(string(data), "\x1f")
}

func newCache(size int64) *ristretto.Cache[string, []byte] {
	if size <= 0 {
		return nil
	}
	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: size / 64,
		MaxCost:     size,
		BufferItems: 64,
	})
	if err != nil {
		return nil
	}
	return cache
}

func (s *Storage) prefixExists(prefix string) (bool, error) {
	for obj := range s.client.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return false, obj.Err
		}
		return true, nil
	}
	return false, nil
}

type attributesStore struct {
	storage *Storage
}

func (a *attributesStore) Read() (map[string]string, error) {
	key := joinKey(a.storage.basePrefix(), metadata.AttributesFileName)
	if cached, ok := a.storage.getCachedObject(key); ok {
		return metadata.ReadAttributes(bytes.NewReader(cached))
	}
	obj, err := a.storage.client.GetObject(context.Background(), a.storage.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			a.storage.deleteCachedObject(key)
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		if isNotFound(err) {
			a.storage.deleteCachedObject(key)
			return map[string]string{}, nil
		}
		return nil, err
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			a.storage.deleteCachedObject(key)
			return map[string]string{}, nil
		}
		return nil, err
	}
	a.storage.setCachedObject(key, data)
	return metadata.ReadAttributes(bytes.NewReader(data))
}

func (a *attributesStore) Write(attrs map[string]string) error {
	key := joinKey(a.storage.basePrefix(), metadata.AttributesFileName)
	buf := &bytes.Buffer{}
	if err := metadata.WriteAttributes(buf, attrs); err != nil {
		return err
	}
	_, err := a.storage.client.PutObject(context.Background(), a.storage.bucket, key, buf, int64(buf.Len()), minio.PutObjectOptions{})
	if err == nil {
		a.storage.setCachedObject(key, buf.Bytes())
	}
	return err
}

type Dir struct {
	storage *Storage
	name    string
	guid    string
	prefix  string
}

func (d *Dir) Name() string {
	return d.name
}

func (d *Dir) ensurePrefix(create bool) error {
	if d.prefix != "" {
		return nil
	}
	guid, err := d.storage.readRegistry(d.name)
	if err != nil {
		if maildir.IsNotExist(err) && create {
			guid = newGUID()
			if guid == "" {
				return errors.New("maildir: failed to generate mailbox guid")
			}
			if err := d.storage.writeRegistry(d.name, guid); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	d.guid = guid
	d.prefix = joinKey(d.storage.mboxRootPrefix(), guid)
	return nil
}

func (d *Dir) Init() error {
	if err := d.ensurePrefix(true); err != nil {
		return err
	}
	key := joinKey(d.prefix, markerName)
	_, err := d.storage.client.PutObject(context.Background(), d.storage.bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if err == nil {
		d.storage.invalidateList(d.prefix)
	}
	return err
}

func (d *Dir) Exists() (bool, error) {
	_, err := d.storage.readRegistry(d.name)
	if err != nil {
		if maildir.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (d *Dir) Children() ([]maildir.Dir, error) {
	names, err := d.storage.ListDirs()
	if err != nil {
		return nil, err
	}
	var children []maildir.Dir
	baseName := normalizeMailboxName(d.name)
	prefix := baseName + hierarchySep
	if strings.EqualFold(d.name, inboxName) {
		prefix = ""
	}
	for _, name := range names {
		if strings.EqualFold(name, baseName) {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			rel := strings.TrimPrefix(name, prefix)
			if strings.Contains(rel, hierarchySep) {
				continue
			}
		} else if strings.Contains(name, hierarchySep) {
			continue
		}
		childDir, err := d.storage.Dir(name)
		if err != nil {
			continue
		}
		children = append(children, childDir)
	}
	return children, nil
}

func (d *Dir) Remove(childExists bool) error {
	if err := d.ensurePrefix(false); err != nil {
		return err
	}
	if childExists {
		for _, folder := range []string{"cur", "new"} {
			prefix := joinKey(d.prefix, folder)
			if err := d.deletePointers(prefix); err != nil {
				return err
			}
		}
		return nil
	}
	if err := d.deletePointers(joinKey(d.prefix, "cur")); err != nil {
		return err
	}
	if err := d.deletePointers(joinKey(d.prefix, "new")); err != nil {
		return err
	}
	if err := d.removePrefix(d.prefix); err != nil {
		return err
	}
	return d.storage.deleteRegistry(d.name)
}

func (d *Dir) removePrefix(prefix string) error {
	keys, err := d.storage.listKeys(prefix)
	if err != nil {
		if isNotFound(err) {
			return maildir.ErrNotExist
		}
		return err
	}
	err = d.storage.removeKeys(keys)
	if err == nil {
		d.storage.invalidateList(prefix)
	}
	return err
}

func (d *Dir) deletePointers(prefix string) error {
	keys, err := d.storage.listKeys(prefix)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	deleteKeys := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		guid, _ := parsePointerKey(key)
		if guid == "" {
			continue
		}
		deleteKeys = append(deleteKeys, key)
		deleteKeys = append(deleteKeys, joinKey(d.storage.blobRootPrefix(), guid))
	}
	err = d.storage.removeKeys(deleteKeys)
	if err == nil {
		d.storage.invalidateList(prefix)
	}
	return err
}

func (d *Dir) Rename(name string) error {
	fullName := d.renameTarget(name)
	if fullName == "" {
		return errors.New("illegal mailbox name")
	}
	baseName := normalizeMailboxName(d.name)
	guid, err := d.storage.readRegistry(d.name)
	if err != nil {
		return err
	}
	if _, err := d.storage.readRegistry(fullName); err == nil {
		return errors.New("maildir: mailbox already exists")
	} else if !maildir.IsNotExist(err) {
		return err
	}
	names, err := d.storage.listRegistryNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, baseName+hierarchySep) {
			continue
		}
		suffix := strings.TrimPrefix(name, baseName+hierarchySep)
		if suffix == "" {
			continue
		}
		childGUID, err := d.storage.readRegistry(name)
		if err != nil {
			return err
		}
		newChildName := fullName + hierarchySep + suffix
		if err := d.storage.writeRegistry(newChildName, childGUID); err != nil {
			return err
		}
		if err := d.storage.deleteRegistry(name); err != nil {
			return err
		}
	}
	if err := d.storage.writeRegistry(fullName, guid); err != nil {
		return err
	}
	if err := d.storage.deleteRegistry(d.name); err != nil {
		return err
	}
	d.name = fullName
	d.guid = guid
	d.prefix = joinKey(d.storage.mboxRootPrefix(), guid)
	return nil
}

func (d *Dir) LoadMetadata(state *maildir.Metadata) error {
	if err := d.ensurePrefix(false); err != nil {
		return err
	}
	store := newMetadataStore(d.storage, d.prefix)
	return store.Load(state)
}

func (d *Dir) WriteMetadata(state *maildir.Metadata) error {
	if err := d.ensurePrefix(false); err != nil {
		return err
	}
	store := newMetadataStore(d.storage, d.prefix)
	return store.Write(state)
}

func (d *Dir) Unseen() ([]maildir.Message, error) {
	if err := d.ensurePrefix(false); err != nil {
		return nil, err
	}
	keys, err := d.storage.listKeys(joinKey(d.prefix, "new"))
	if err != nil {
		if isNotFound(err) {
			return nil, maildir.ErrNotExist
		}
		return nil, err
	}
	msgs := make([]maildir.Message, 0, len(keys))
	for _, key := range keys {
		guid, flags := parsePointerKey(key)
		msgs = append(msgs, &message{
			storage:    d.storage,
			bucket:     d.storage.bucket,
			pointerKey: key,
			guid:       guid,
			flags:      flags,
			isNew:      true,
		})
	}
	return msgs, nil
}

func (d *Dir) NewMessageKeys() ([]string, error) {
	if err := d.ensurePrefix(false); err != nil {
		return nil, err
	}
	keys, err := d.storage.listKeys(joinKey(d.prefix, "new"))
	if err != nil {
		if isNotFound(err) {
			return nil, maildir.ErrNotExist
		}
		return nil, err
	}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		guid, _ := parsePointerKey(key)
		if guid == "" {
			continue
		}
		result = append(result, guid)
	}
	return result, nil
}

func (d *Dir) Messages() ([]maildir.Message, error) {
	if err := d.ensurePrefix(false); err != nil {
		return nil, err
	}
	keys, err := d.storage.listKeys(d.prefix)
	if err != nil {
		if isNotFound(err) {
			return nil, maildir.ErrNotExist
		}
		return nil, err
	}
	var msgs []maildir.Message
	for _, key := range keys {
		if !strings.Contains(key, "/cur/") && !strings.Contains(key, "/new/") {
			continue
		}
		guid, flags := parsePointerKey(key)
		msgs = append(msgs, &message{
			storage:    d.storage,
			bucket:     d.storage.bucket,
			pointerKey: key,
			guid:       guid,
			flags:      flags,
			isNew:      strings.Contains(key, "/new/"),
		})
	}
	return msgs, nil
}

func (d *Dir) Create(flags []maildir.Flag) (maildir.Message, io.WriteCloser, error) {
	if err := d.ensurePrefix(false); err != nil {
		return nil, nil, err
	}
	guid := newGUID()
	file, err := os.CreateTemp("", "maildir-s3-blob-*")
	if err != nil {
		return nil, nil, err
	}

	msg := &message{
		storage:    d.storage,
		bucket:     d.storage.bucket,
		pointerKey: d.pointerKey(guid, flags, len(flags) == 0),
		guid:       guid,
		flags:      flags,
		isNew:      len(flags) == 0,
	}
	return msg, &blobWriter{file: file, msg: msg}, nil
}

func (d *Dir) NewDelivery() (maildir.Delivery, error) {
	if err := d.ensurePrefix(false); err != nil {
		return nil, err
	}
	guid := newGUID()
	file, err := os.CreateTemp("", "maildir-s3-delivery-*")
	if err != nil {
		return nil, err
	}
	return &delivery{dir: d, file: file, guid: guid}, nil
}

func (d *Dir) pointerKey(guid string, flags []maildir.Flag, isNew bool) string {
	name := pointerName(guid, flags, isNew)
	folder := "cur"
	if isNew && len(flags) == 0 {
		folder = "new"
	}
	return joinKey(d.prefix, folder, name)
}

func (d *Dir) renameTarget(name string) string {
	if strings.EqualFold(d.name, inboxName) {
		return name
	}
	if d.name == "" {
		return name
	}
	parts := strings.Split(d.name, hierarchySep)
	if len(parts) <= 1 {
		return name
	}
	parent := strings.Join(parts[:len(parts)-1], hierarchySep)
	if parent == "" {
		return name
	}
	return parent + hierarchySep + name
}

type blobWriter struct {
	file *os.File
	msg  *message
}

func (w *blobWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}

func (w *blobWriter) Close() error {
	info, err := w.file.Stat()
	if err != nil {
		_ = w.file.Close()
		_ = os.Remove(w.file.Name())
		return err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		_ = w.file.Close()
		_ = os.Remove(w.file.Name())
		return err
	}
	blobKey := joinKey(w.msg.storage.blobRootPrefix(), w.msg.guid)
	_, err = w.msg.storage.client.PutObject(context.Background(), w.msg.bucket, blobKey, w.file, info.Size(), minio.PutObjectOptions{})
	if err != nil {
		_ = w.file.Close()
		_ = os.Remove(w.file.Name())
		return err
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(w.file.Name())
		return err
	}
	if err := os.Remove(w.file.Name()); err != nil {
		return err
	}
	return w.msg.createPointer()
}

type delivery struct {
	dir  *Dir
	file *os.File
	guid string
}

func (d *delivery) Write(p []byte) (int, error) {
	return d.file.Write(p)
}

func (d *delivery) Close() error {
	info, err := d.file.Stat()
	if err != nil {
		_ = d.file.Close()
		_ = os.Remove(d.file.Name())
		return err
	}
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		_ = d.file.Close()
		_ = os.Remove(d.file.Name())
		return err
	}
	blobKey := joinKey(d.dir.storage.blobRootPrefix(), d.guid)
	_, err = d.dir.storage.client.PutObject(context.Background(), d.dir.storage.bucket, blobKey, d.file, info.Size(), minio.PutObjectOptions{})
	if err != nil {
		_ = d.file.Close()
		_ = os.Remove(d.file.Name())
		return err
	}
	if err := d.file.Close(); err != nil {
		_ = os.Remove(d.file.Name())
		return err
	}
	if err := os.Remove(d.file.Name()); err != nil {
		return err
	}
	key := d.dir.pointerKey(d.guid, nil, true)
	_, err = d.dir.storage.client.PutObject(context.Background(), d.dir.storage.bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if err == nil {
		d.dir.storage.invalidateListForKey(key)
	}
	return err
}

func (d *delivery) Abort() error {
	name := d.file.Name()
	if err := d.file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

type message struct {
	storage    *Storage
	bucket     string
	pointerKey string
	guid       string
	flags      []maildir.Flag
	isNew      bool
}

func (m *message) Key() string {
	return m.guid
}

func (m *message) Name() string {
	return path.Base(m.pointerKey)
}

func (m *message) Flags() []maildir.Flag {
	return append([]maildir.Flag{}, m.flags...)
}

func (m *message) SetFlags(flags []maildir.Flag) error {
	newKey := m.pointerKeyForFlags(flags)
	if newKey == m.pointerKey {
		m.flags = append([]maildir.Flag{}, flags...)
		if m.isNew {
			m.isNew = false
		}
		return nil
	}
	if err := m.createPointerWithKey(newKey); err != nil {
		return err
	}
	if err := m.storage.client.RemoveObject(context.Background(), m.bucket, m.pointerKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	m.storage.invalidateListForKey(m.pointerKey)
	m.pointerKey = newKey
	m.flags = append([]maildir.Flag{}, flags...)
	m.isNew = false
	return nil
}

func (m *message) Open() (io.ReadCloser, error) {
	blobKey := joinKey(m.storage.blobRootPrefix(), m.guid)
	if m.storage.cache != nil {
		if data, ok := m.storage.cache.Get(blobKey); ok {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	}
	obj, err := m.storage.client.GetObject(context.Background(), m.bucket, blobKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if m.storage.cache == nil || m.storage.maxCacheItemSize <= 0 {
		return obj, nil
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, maildir.ErrNotExist
		}
		return nil, err
	}
	if info.Size <= 0 || info.Size > m.storage.maxCacheItemSize {
		return obj, nil
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		_ = obj.Close()
		return nil, err
	}
	_ = obj.Close()
	m.storage.cache.Set(blobKey, data, int64(len(data)))
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *message) Stat() (maildir.Info, error) {
	blobKey := joinKey(m.storage.blobRootPrefix(), m.guid)
	stat, err := m.storage.client.StatObject(context.Background(), m.bucket, blobKey, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, maildir.ErrNotExist
		}
		return nil, err
	}
	return &info{size: stat.Size, modTime: stat.LastModified}, nil
}

func (m *message) Chtimes(atime, mtime time.Time) error {
	return nil
}

func (m *message) Remove() error {
	if err := m.storage.client.RemoveObject(context.Background(), m.bucket, m.pointerKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	m.storage.invalidateListForKey(m.pointerKey)
	blobKey := joinKey(m.storage.blobRootPrefix(), m.guid)
	if err := m.storage.client.RemoveObject(context.Background(), m.bucket, blobKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	return nil
}

func (m *message) MoveTo(target maildir.Dir) error {
	other, ok := target.(*Dir)
	if !ok {
		return errors.New("maildir: unsupported target dir implementation")
	}
	newKey := other.pointerKey(m.guid, m.flags, m.isNew)
	if err := m.createPointerWithKey(newKey); err != nil {
		return err
	}
	if err := m.storage.client.RemoveObject(context.Background(), m.bucket, m.pointerKey, minio.RemoveObjectOptions{}); err != nil {
		return err
	}
	m.storage.invalidateListForKey(m.pointerKey)
	m.pointerKey = newKey
	return nil
}

func (m *message) CopyTo(target maildir.Dir) (maildir.Message, error) {
	other, ok := target.(*Dir)
	if !ok {
		return nil, errors.New("maildir: unsupported target dir implementation")
	}
	newGUID := newGUID()
	src := minio.CopySrcOptions{Bucket: m.bucket, Object: joinKey(m.storage.blobRootPrefix(), m.guid)}
	dst := minio.CopyDestOptions{Bucket: m.bucket, Object: joinKey(m.storage.blobRootPrefix(), newGUID)}
	if _, err := m.storage.client.CopyObject(context.Background(), dst, src); err != nil {
		return nil, err
	}
	newMsg := &message{
		storage:    m.storage,
		bucket:     m.bucket,
		guid:       newGUID,
		flags:      append([]maildir.Flag{}, m.flags...),
		isNew:      m.isNew,
		pointerKey: other.pointerKey(newGUID, m.flags, m.isNew),
	}
	if err := newMsg.createPointer(); err != nil {
		return nil, err
	}
	return newMsg, nil
}

func (m *message) pointerKeyForFlags(flags []maildir.Flag) string {
	parts := strings.Split(m.pointerKey, "/")
	if len(parts) < 2 {
		return m.pointerKey
	}
	base := pointerName(m.guid, flags, false)
	parts[len(parts)-1] = base
	if m.isNew && len(parts) >= 2 {
		parts[len(parts)-2] = "cur"
	}
	return strings.Join(parts, "/")
}

func (m *message) createPointer() error {
	return m.createPointerWithKey(m.pointerKey)
}

func (m *message) createPointerWithKey(key string) error {
	_, err := m.storage.client.PutObject(context.Background(), m.bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if err == nil {
		m.storage.invalidateListForKey(key)
	}
	return err
}

type info struct {
	size    int64
	modTime time.Time
}

func (i *info) Size() int64 {
	return i.size
}

func (i *info) ModTime() time.Time {
	return i.modTime
}

func parsePointerKey(key string) (string, []maildir.Flag) {
	name := path.Base(key)
	parts := strings.SplitN(name, ":2,", 2)
	guid := parts[0]
	if len(parts) < 2 {
		return guid, nil
	}
	var flags []maildir.Flag
	for _, r := range parts[1] {
		flags = append(flags, maildir.Flag(r))
	}
	return guid, flags
}

func pointerName(guid string, flags []maildir.Flag, isNew bool) string {
	if isNew && len(flags) == 0 {
		return guid
	}
	flagRunes := make([]rune, 0, len(flags))
	for _, flag := range flags {
		flagRunes = append(flagRunes, rune(flag))
	}
	slices.Sort(flagRunes)
	return guid + ":2," + string(flagRunes)
}

func joinKey(parts ...string) string {
	var clean []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		clean = append(clean, strings.Trim(part, "/"))
	}
	if len(clean) == 0 {
		return ""
	}
	return path.Join(clean...)
}

func normalizeMailboxName(name string) string {
	if name == "" {
		return ""
	}
	parts := strings.Split(name, hierarchySep)
	normalized := make([]string, 0, len(parts))
	for i, part := range parts {
		if i == 0 && strings.EqualFold(part, inboxName) {
			continue
		}
		if part == "" {
			continue
		}
		normalized = append(normalized, part)
	}
	if len(normalized) == 0 {
		return inboxName
	}
	return strings.Join(normalized, hierarchySep)
}

func newGUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return strings.ToLower(hexString(buf))
}

func hexString(buf []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(buf)*2)
	for i, b := range buf {
		out[i*2] = hextable[b>>4]
		out[i*2+1] = hextable[b&0x0f]
	}
	return string(out)
}

func validMboxPart(name string) bool {
	if strings.ContainsAny(name, ":*?\"<>|") {
		return false
	}
	for _, ch := range name {
		if ch < ' ' {
			return false
		}
	}
	return !strings.Contains(name, "..")
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.Code == "NotFound" {
		return true
	}
	return resp.StatusCode == 404
}
