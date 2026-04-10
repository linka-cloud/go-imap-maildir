package maildir

import (
	"io"
	"time"

	maildirindex "github.com/foxcpp/go-imap-maildir/maildir/index"
)

// Flag is a message flag.
type Flag rune

const (
	FlagPassed  Flag = 'P'
	FlagReplied Flag = 'R'
	FlagSeen    Flag = 'S'
	FlagTrashed Flag = 'T'
	FlagDraft   Flag = 'D'
	FlagFlagged Flag = 'F'
)

// Message represents a message in a Maildir.
type Message interface {
	Key() string
	Name() string
	Flags() []Flag
	SetFlags(flags []Flag) error
	Open() (io.ReadCloser, error)
	Stat() (Info, error)
	Chtimes(atime, mtime time.Time) error
	Remove() error
	MoveTo(target Dir) error
	CopyTo(target Dir) (Message, error)
}

type Info interface {
	Size() int64
	ModTime() time.Time
}

// Dir represents a Maildir directory.
type Dir interface {
	Name() string
	Init() error
	Exists() (bool, error)
	Unseen() ([]Message, error)
	Messages() ([]Message, error)
	Create(flags []Flag) (Message, io.WriteCloser, error)
	NewDelivery() (Delivery, error)
	Children() ([]Dir, error)
	Remove(childExists bool) error
	Rename(name string) error
	LoadMetadata(state *Metadata) error
	WriteMetadata(state *Metadata) error
}

type Storage interface {
	Dir(name string) (Dir, error)
	ListDirs() ([]string, error)
	Attributes() (AttributesStore, error)
}

type Provider interface {
	Storage(basePath string) Storage
}

type AttributesStore interface {
	Read() (map[string]string, error)
	Write(attrs map[string]string) error
}

type IndexStore interface {
	Load() (*maildirindex.Index, error)
	Append(records ...maildirindex.Record) error
	Snapshot(index *maildirindex.Index) error
}

type IndexProvider interface {
	IndexStore(guid string) (IndexStore, error)
}

type NewMessageKeyLister interface {
	NewMessageKeys() ([]string, error)
}

type Delivery interface {
	io.WriteCloser
	Abort() error
}
