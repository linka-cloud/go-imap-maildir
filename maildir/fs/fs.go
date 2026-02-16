package fs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-maildir"

	imapmaildir "github.com/foxcpp/go-imap-maildir/maildir"
)

const (
	hierarchySep = "."
	inboxName    = "INBOX"
	maxNesting   = 100
)

type Storage struct {
	basePath string
}

func (s *Storage) Dir(name string) (imapmaildir.Dir, error) {
	path, err := s.pathForMailbox(name)
	if err != nil {
		return nil, err
	}
	return &Dir{
		dir:      maildir.Dir(path),
		basePath: s.basePath,
		name:     name,
	}, nil
}

func (s *Storage) ListDirs() ([]string, error) {
	var paths []string

	err := filepath.WalkDir(s.basePath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if path == s.basePath {
			return nil
		}
		if !strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	mboxes := make([]string, 0, len(paths))
	for _, path := range paths {
		name, err := s.mboxName(path)
		if err != nil {
			continue
		}
		if strings.EqualFold(name, inboxName) {
			continue
		}
		mboxes = append(mboxes, name)
	}

	return mboxes, nil
}

func (s *Storage) Attributes() (imapmaildir.AttributesStore, error) {
	return &attributesStore{basePath: s.basePath}, nil
}

func (s *Storage) pathForMailbox(name string) (string, error) {
	if strings.EqualFold(name, inboxName) {
		return s.basePath, nil
	}
	parts := strings.Split(name, hierarchySep)
	if len(parts) > maxNesting {
		return "", errors.New("mailbox nesting limit exceeded")
	}
	currentPath := s.basePath
	for i, part := range parts {
		if i == 0 && strings.EqualFold(part, inboxName) {
			continue
		}
		if part == "" {
			if i != len(parts)-1 {
				return "", errors.New("illegal mailbox name")
			}
			continue
		}
		if !validMboxPart(part) {
			return "", errors.New("illegal mailbox name")
		}
		currentPath = filepath.Join(currentPath, "."+part)
	}
	return currentPath, nil
}

func (s *Storage) mboxName(fsPath string) (string, error) {
	fsPath = strings.TrimPrefix(fsPath, s.basePath+string(filepath.Separator))
	if fsPath == "" {
		return inboxName, nil
	}
	parts := strings.Split(fsPath, string(filepath.Separator))
	if len(parts) > maxNesting {
		return "", errors.New("mailbox nesting limit exceeded")
	}
	mboxParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if !strings.HasPrefix(part, ".") {
			return "", errors.New("not a maildir++ path")
		}
		mboxParts = append(mboxParts, part[1:])
	}
	return strings.Join(mboxParts, hierarchySep), nil
}

type attributesStore struct {
	basePath string
}

func (s *attributesStore) Read() (map[string]string, error) {
	return readAttributes(s.basePath)
}

func (s *attributesStore) Write(attrs map[string]string) error {
	return writeAttributes(s.basePath, attrs)
}

type Dir struct {
	dir      maildir.Dir
	basePath string
	name     string
}

func (d *Dir) Name() string {
	return d.name
}

func (d *Dir) Path() string {
	return string(d.dir)
}

func (d *Dir) NewDelivery() (imapmaildir.Delivery, error) {
	wrapped, err := maildir.NewDelivery(string(d.dir))
	if err != nil {
		return nil, err
	}
	return &delivery{wrapped: wrapped}, nil
}

func (d *Dir) Init() error {
	if err := os.MkdirAll(string(d.dir), 0700); err != nil {
		return err
	}
	return d.dir.Init()
}

func (d *Dir) Exists() (bool, error) {
	_, err := os.Stat(string(d.dir))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (d *Dir) Unseen() ([]imapmaildir.Message, error) {
	msgs, err := d.dir.Unseen()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, imapmaildir.ErrNotExist
		}
		return nil, err
	}
	return wrapMessages(msgs), nil
}

func (d *Dir) Messages() ([]imapmaildir.Message, error) {
	msgs, err := d.dir.Messages()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, imapmaildir.ErrNotExist
		}
		return nil, err
	}
	return wrapMessages(msgs), nil
}

func (d *Dir) Create(flags []imapmaildir.Flag) (imapmaildir.Message, io.WriteCloser, error) {
	emFlags := make([]maildir.Flag, len(flags))
	for i, flag := range flags {
		emFlags[i] = maildir.Flag(flag)
	}
	msg, writer, err := d.dir.Create(emFlags)
	if err != nil {
		return nil, nil, err
	}
	return &message{msg: msg}, writer, nil
}

type delivery struct {
	wrapped *maildir.Delivery
}

func (d *delivery) Write(p []byte) (int, error) {
	return d.wrapped.Write(p)
}

func (d *delivery) Close() error {
	return d.wrapped.Close()
}

func (d *delivery) Abort() error {
	return d.wrapped.Abort()
}

func (d *Dir) Children() ([]imapmaildir.Dir, error) {
	entries, err := os.ReadDir(string(d.dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, imapmaildir.ErrNotExist
		}
		return nil, err
	}

	children := make([]imapmaildir.Dir, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "cur" || name == "new" || name == "tmp" {
			continue
		}
		if !strings.HasPrefix(name, ".") {
			continue
		}
		childPath := filepath.Join(string(d.dir), name)
		childName := d.childName(name[1:])
		children = append(children, &Dir{dir: maildir.Dir(childPath), basePath: d.basePath, name: childName})
	}

	return children, nil
}

func (d *Dir) Remove(childExists bool) error {
	if childExists {
		for _, name := range []string{"cur", "new", "tmp"} {
			if err := os.RemoveAll(filepath.Join(string(d.dir), name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	return os.RemoveAll(string(d.dir))
}

func (d *Dir) Rename(name string) error {
	fullName := d.renameTarget(name)
	if fullName == "" {
		return errors.New("illegal mailbox name")
	}
	path, err := (&Storage{basePath: d.basePath}).pathForMailbox(fullName)
	if err != nil {
		return err
	}
	return os.Rename(string(d.dir), path)
}

func (d *Dir) LoadMetadata(state *imapmaildir.Metadata) error {
	return newMetadataStore(string(d.dir)).Load(state)
}

func (d *Dir) WriteMetadata(state *imapmaildir.Metadata) error {
	return newMetadataStore(string(d.dir)).Write(state)
}

func (d *Dir) childName(child string) string {
	if strings.EqualFold(d.name, inboxName) {
		return child
	}
	if d.name == "" {
		return child
	}
	return d.name + hierarchySep + child
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

type message struct {
	msg *maildir.Message
}

func (m *message) Key() string {
	return m.msg.Key()
}

func (m *message) Name() string {
	return m.msg.Filename()
}

func (m *message) Flags() []imapmaildir.Flag {
	flags := m.msg.Flags()
	result := make([]imapmaildir.Flag, len(flags))
	for i, flag := range flags {
		result[i] = imapmaildir.Flag(flag)
	}
	return result
}

func (m *message) SetFlags(flags []imapmaildir.Flag) error {
	emFlags := make([]maildir.Flag, len(flags))
	for i, flag := range flags {
		emFlags[i] = maildir.Flag(flag)
	}
	return m.msg.SetFlags(emFlags)
}

func (m *message) Open() (io.ReadCloser, error) {
	return m.msg.Open()
}

func (m *message) Stat() (imapmaildir.Info, error) {
	info, err := os.Stat(m.msg.Filename())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, imapmaildir.ErrNotExist
		}
		return nil, err
	}
	return info, nil
}

func (m *message) Chtimes(atime, mtime time.Time) error {
	return os.Chtimes(m.msg.Filename(), atime, mtime)
}

func (m *message) Remove() error {
	return m.msg.Remove()
}

func (m *message) MoveTo(target imapmaildir.Dir) error {
	emDir, ok := target.(*Dir)
	if !ok {
		return errors.New("maildir: unsupported target dir implementation")
	}
	return m.msg.MoveTo(emDir.dir)
}

func (m *message) CopyTo(target imapmaildir.Dir) (imapmaildir.Message, error) {
	emDir, ok := target.(*Dir)
	if !ok {
		return nil, errors.New("maildir: unsupported target dir implementation")
	}
	newMsg, err := m.msg.CopyTo(emDir.dir)
	if err != nil {
		return nil, err
	}
	return &message{msg: newMsg}, nil
}

func wrapMessages(msgs []*maildir.Message) []imapmaildir.Message {
	result := make([]imapmaildir.Message, len(msgs))
	for i, msg := range msgs {
		result[i] = &message{msg: msg}
	}
	return result
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
