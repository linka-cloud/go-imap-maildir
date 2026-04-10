package imapmaildir

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	mess "github.com/foxcpp/go-imap-mess"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

type Backend struct {
	Log   *log.Logger
	Debug *log.Logger

	PathTemplate     string
	Authenticator    func(*imap.ConnInfo, string, string) (bool, error)
	DefaultMailboxes []DefaultMailboxSpec
	StorageProvider  maildir.Provider

	Manager *mess.Manager

	limitLock   sync.Mutex
	appendLimit *uint32

	statesLock sync.Mutex
	states     map[string]*mailboxState
	users      map[string]int
}

type mailboxState struct {
	uidValidity uint32
	uidNext     uint32
	messages    map[string]*messageMeta
	meta        *maildir.Metadata
}

type messageMeta struct {
	uid          uint32
	flags        []string
	internalDate time.Time
	recent       bool
}

func (b *Backend) Login(connInfo *imap.ConnInfo, username, password string) (backend.User, error) {
	if b.Authenticator != nil {
		ok, err := b.Authenticator(connInfo, username, password)
		if err != nil || !ok {
			if err != nil {
				b.Log.Printf("authentication error: %v", err)
			}
			return nil, backend.ErrInvalidCredentials
		}
	}

	return b.GetUser(username)
}

func (b *Backend) GetUser(username string) (backend.User, error) {
	basePath := strings.ReplaceAll(b.PathTemplate, "{username}", username)
	storage := b.StorageProvider.Storage(basePath)
	mailbox, err := storage.Dir(InboxName)
	if err != nil {
		b.Log.Printf("%v", err)
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	exists, err := mailbox.Exists()
	if err != nil {
		b.Log.Printf("%v", err)
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	if !exists {
		return nil, backend.ErrInvalidCredentials
	}

	b.Debug.Printf("user logged in (%v, %v)", username, basePath)

	user := &User{
		b:         b,
		name:      username,
		storage:   storage,
		mailboxes: map[string]*Mailbox{},
	}
	if err := mailbox.Init(); err != nil {
		b.Log.Printf("failed to init inbox: %v", err)
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	b.userLogin(username)

	return user, nil
}

func (b *Backend) CreateUser(username string) error {
	basePath := strings.ReplaceAll(b.PathTemplate, "{username}", username)
	storage := b.StorageProvider.Storage(basePath)
	mailbox, err := storage.Dir(InboxName)
	if err != nil {
		return err
	}
	if err := mailbox.Init(); err != nil {
		return err
	}

	user := &User{
		b:         b,
		name:      username,
		storage:   storage,
		mailboxes: map[string]*Mailbox{},
	}
	user.ensureMailbox(InboxName)
	user.ensureDefaultMailboxes()
	return nil
}

func (b *Backend) DeleteUser(username string) error {
	basePath := strings.ReplaceAll(b.PathTemplate, "{username}", username)
	storage := b.StorageProvider.Storage(basePath)

	mailboxes := []string{}
	seen := map[string]struct{}{}
	addMailbox := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		mailboxes = append(mailboxes, name)
	}

	if inbox, err := storage.Dir(InboxName); err == nil {
		exists, err := inbox.Exists()
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		if exists {
			addMailbox(InboxName)
		}
	}

	names, err := storage.ListDirs()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	for _, name := range names {
		addMailbox(name)
	}

	sort.Slice(mailboxes, func(i, j int) bool {
		depthI := strings.Count(mailboxes[i], HierarchySep)
		depthJ := strings.Count(mailboxes[j], HierarchySep)
		if depthI == depthJ {
			return mailboxes[i] > mailboxes[j]
		}
		return depthI > depthJ
	})

	for _, name := range mailboxes {
		dir, err := storage.Dir(name)
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		exists, err := dir.Exists()
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		if !exists {
			continue
		}
		if err := dir.Remove(false); err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		b.deleteMailboxState(username, name)
	}

	return nil
}

func (b *Backend) CreateMessageLimit() *uint32 {
	b.limitLock.Lock()
	defer b.limitLock.Unlock()

	if b.appendLimit == nil {
		return nil
	}
	val := *b.appendLimit
	return &val
}

func (b *Backend) SetMessageLimit(val *uint32) error {
	b.limitLock.Lock()
	defer b.limitLock.Unlock()

	if val == nil {
		b.appendLimit = nil
		return nil
	}

	copyVal := *val
	b.appendLimit = &copyVal
	return nil
}

func (b *Backend) Close() error {
	b.statesLock.Lock()
	defer b.statesLock.Unlock()

	b.states = map[string]*mailboxState{}
	b.users = map[string]int{}
	return nil
}

func New(pathTemplate string, provider maildir.Provider, defaultMailboxes []DefaultMailboxSpec) (*Backend, error) {
	if len(defaultMailboxes) == 0 {
		defaultMailboxes = defaultMailboxSpecs
	}
	return &Backend{
		Log:              log.New(os.Stderr, "imapmaildir: ", 0),
		Debug:            log.New(os.Stderr, "imapmaildir[debug]: ", 0),
		PathTemplate:     pathTemplate,
		DefaultMailboxes: defaultMailboxes,
		StorageProvider:  provider,
		Manager:          mess.NewManager(),
		states:           map[string]*mailboxState{},
		users:            map[string]int{},
	}, nil
}

func (b *Backend) userLogin(username string) {
	b.statesLock.Lock()
	defer b.statesLock.Unlock()

	b.users[username]++
}

func (b *Backend) userLogout(username string) {
	b.statesLock.Lock()
	defer b.statesLock.Unlock()

	active := b.users[username]
	if active <= 1 {
		delete(b.users, username)
		for key := range b.states {
			if strings.HasPrefix(key, username+"\x00") {
				delete(b.states, key)
			}
		}
		return
	}

	b.users[username] = active - 1
}

func (b *Backend) mailboxKey(username, mailbox string) string {
	return username + "\x00" + mailbox
}

func (b *Backend) getMailboxState(username, mailbox string) *mailboxState {
	key := b.mailboxKey(username, mailbox)

	b.statesLock.Lock()
	defer b.statesLock.Unlock()

	state, ok := b.states[key]
	if ok {
		return state
	}

	uidValidity := uint32(time.Now().UnixNano())
	if uidValidity == 0 {
		uidValidity = 1
	}

	state = &mailboxState{
		uidValidity: uidValidity,
		uidNext:     1,
		messages:    map[string]*messageMeta{},
	}
	b.states[key] = state
	return state
}

func (b *Backend) deleteMailboxState(username, mailbox string) {
	key := b.mailboxKey(username, mailbox)
	b.statesLock.Lock()
	defer b.statesLock.Unlock()
	delete(b.states, key)
}

func (b *Backend) renameMailboxState(username, oldName, newName string) {
	oldKey := b.mailboxKey(username, oldName)
	newKey := b.mailboxKey(username, newName)

	b.statesLock.Lock()
	defer b.statesLock.Unlock()

	if state, ok := b.states[oldKey]; ok {
		delete(b.states, oldKey)
		b.states[newKey] = state
	}
}
