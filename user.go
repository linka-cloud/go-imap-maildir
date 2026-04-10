package imapmaildir

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

const (
	InboxName      = "INBOX"
	HierarchySep   = "."
	MaxMboxNesting = 100
)

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

func (u *User) validateMboxName(mbox string) error {
	if strings.EqualFold(mbox, InboxName) {
		return nil
	}
	parts := strings.Split(mbox, HierarchySep)
	if len(parts) > MaxMboxNesting {
		return errors.New("mailbox nesting limit exceeded")
	}
	for i, part := range parts {
		if i == 0 && strings.EqualFold(part, InboxName) {
			continue
		}
		if part == "" {
			if i != len(parts)-1 {
				return errors.New("illegal mailbox name")
			}
			continue
		}
		if !validMboxPart(part) {
			u.b.Log.Printf("illegal mailbox name requested by %s: %v", u.name, mbox)
			return errors.New("illegal mailbox name")
		}
	}
	return nil
}

func mailboxAncestors(mbox string) []string {
	if strings.EqualFold(mbox, InboxName) {
		return []string{InboxName}
	}
	parts := strings.Split(mbox, HierarchySep)
	ancestors := make([]string, 0, len(parts))
	current := ""
	for i, part := range parts {
		if i == 0 && strings.EqualFold(part, InboxName) {
			continue
		}
		if part == "" {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = current + HierarchySep + part
		}
		ancestors = append(ancestors, current)
	}
	return ancestors
}

func relativeMailboxName(existingName, newName string) (string, error) {
	if strings.EqualFold(existingName, InboxName) {
		return newName, nil
	}
	parts := strings.Split(existingName, HierarchySep)
	if len(parts) <= 1 {
		return newName, nil
	}
	parent := strings.Join(parts[:len(parts)-1], HierarchySep)
	if parent == "" {
		return newName, nil
	}
	prefix := parent + HierarchySep
	if !strings.HasPrefix(newName, prefix) {
		return "", errors.New("illegal mailbox name")
	}
	rel := strings.TrimPrefix(newName, prefix)
	if rel == "" {
		return "", errors.New("illegal mailbox name")
	}
	return rel, nil
}

type User struct {
	b *Backend

	name    string
	storage maildir.Storage

	mailboxesLock sync.Mutex
	mailboxes     map[string]*Mailbox

	limitLock   sync.Mutex
	appendLimit *uint32

	mboxLimitLock sync.Mutex
	mboxLimits    map[string]*uint32

	logoutOnce sync.Once
}

func (u *User) Username() string {
	return u.name
}

func (u *User) getMailbox(mbox string) (*Mailbox, bool) {
	u.mailboxesLock.Lock()
	defer u.mailboxesLock.Unlock()

	mb, ok := u.mailboxes[mbox]
	return mb, ok
}

type DefaultMailboxSpec struct {
	Name       string
	SpecialUse string
}

var defaultMailboxSpecs = []DefaultMailboxSpec{
	{Name: "Trash", SpecialUse: imap.TrashAttr},
	{Name: "Junk", SpecialUse: imap.JunkAttr},
	{Name: "Sent", SpecialUse: imap.SentAttr},
	{Name: "Archive", SpecialUse: imap.ArchiveAttr},
	{Name: "Draft", SpecialUse: imap.DraftsAttr},
}

func (u *User) ensureDefaultMailboxes() {
	specs := u.b.DefaultMailboxes
	if len(specs) == 0 {
		specs = defaultMailboxSpecs
	}
	for _, spec := range specs {
		name := spec.Name
		mboxDir, err := u.storage.Dir(name)
		if err != nil {
			continue
		}
		exists, err := mboxDir.Exists()
		if err != nil {
			continue
		}
		if !exists {
			_ = u.CreateMailbox(name)
		}
		u.mailboxesLock.Lock()
		mbox := u.ensureMailbox(name)
		u.mailboxesLock.Unlock()
		if spec.SpecialUse != "" {
			_ = u.setMailboxSpecialUse(mbox, spec.SpecialUse)
		}
	}
}

func defaultMailboxSpecialUse(name string) (string, bool) {
	for _, spec := range defaultMailboxSpecs {
		if strings.EqualFold(spec.Name, name) {
			return spec.SpecialUse, true
		}
	}
	return "", false
}

func (u *User) setMailboxSpecialUse(mbox *Mailbox, value string) error {
	if mbox == nil {
		return nil
	}
	if err := mbox.loadMetadataState(); err != nil {
		return err
	}
	mbox.state.meta.EnsureGUID()
	if err := mbox.writeMetadataState(mbox.state.meta); err != nil {
		return err
	}
	key := "priv/" + mbox.state.meta.GUID + "/specialuse"
	attrsStore, err := u.storage.Attributes()
	if err != nil {
		return err
	}
	attrs, err := attrsStore.Read()
	if err != nil {
		return err
	}
	if value == "" {
		delete(attrs, key)
	} else {
		attrs[key] = value
	}
	return attrsStore.Write(attrs)
}

func (u *User) mailboxSpecialUse(mbox *Mailbox) ([]string, error) {
	if mbox == nil {
		return nil, nil
	}
	if err := mbox.loadMetadataState(); err != nil {
		return nil, err
	}
	guid := mbox.state.meta.GUID
	if guid == "" {
		return nil, nil
	}
	attrsStore, err := u.storage.Attributes()
	if err != nil {
		return nil, err
	}
	attrs, err := attrsStore.Read()
	if err != nil {
		return nil, err
	}
	value := attrs["priv/"+guid+"/specialuse"]
	if value == "" {
		return nil, nil
	}
	return strings.Fields(value), nil
}

func (u *User) ensureMailbox(mbox string) *Mailbox {
	if m, ok := u.mailboxes[mbox]; ok {
		return m
	}

	mboxDir, err := u.storage.Dir(mbox)
	if err != nil {
		return nil
	}

	mboxObj := &Mailbox{
		b:          u.b,
		user:       u,
		name:       mbox,
		dir:        mboxDir,
		state:      u.b.getMailboxState(u.name, mbox),
		subscribed: true,
	}
	if u.mailboxes == nil {
		u.mailboxes = map[string]*Mailbox{}
	}
	u.mailboxes[mbox] = mboxObj
	return mboxObj
}

func (u *User) ListMailboxes(subscribed bool) ([]imap.MailboxInfo, error) {
	u.mailboxesLock.Lock()
	defer u.mailboxesLock.Unlock()

	if u.mailboxes == nil {
		u.mailboxes = map[string]*Mailbox{}
	}

	var mboxes []imap.MailboxInfo

	inbox := u.ensureMailbox(InboxName)
	if !subscribed || inbox.subscribed {
		info, err := inbox.Info()
		if err == nil {
			mboxes = append(mboxes, *info)
		}
	}

	names, err := u.storage.ListDirs()
	if err != nil {
		u.b.Log.Printf("failed to list mailboxes: %v", err)
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	for _, name := range names {
		mbox := u.ensureMailbox(name)
		if mbox == nil {
			continue
		}
		if subscribed && !mbox.subscribed {
			continue
		}
		mboxInfo, err := mbox.Info()
		if err != nil {
			continue
		}
		mboxes = append(mboxes, *mboxInfo)
	}

	return mboxes, nil
}

func (u *User) GetMailbox(name string, readOnly bool, conn backend.Conn) (*imap.MailboxStatus, backend.Mailbox, error) {
	if err := u.validateMboxName(name); err != nil {
		return nil, nil, err
	}
	mboxDir, err := u.storage.Dir(name)
	if err != nil {
		return nil, nil, err
	}
	exists, err := mboxDir.Exists()
	if err != nil {
		return nil, nil, fmt.Errorf("I/O error: %w", err)
	}
	if !exists {
		return nil, nil, backend.ErrNoSuchMailbox
	}

	mbox := u.ensureMailbox(name)
	status, err := u.Status(name, []imap.StatusItem{
		imap.StatusMessages, imap.StatusRecent, imap.StatusUnseen,
		imap.StatusUidNext, imap.StatusUidValidity,
	})
	if err != nil {
		return nil, nil, err
	}

	entries, err := mbox.listEntries()
	if err != nil {
		return nil, nil, fmt.Errorf("I/O error: %w", err)
	}

	var uids []uint32
	var recent imap.SeqSet
	for _, entry := range entries {
		uids = append(uids, entry.uid)
		if entry.meta.recent {
			recent.AddNum(entry.uid)
			entry.meta.recent = false
		}
	}

	selected := &SelectedMailbox{
		Mailbox:  mbox,
		conn:     conn,
		readOnly: readOnly,
	}

	handle, err := u.b.Manager.Mailbox(mbox.mailboxKey(), selected, uids, &recent)
	if err != nil {
		return nil, nil, err
	}
	selected.handle = handle

	return status, selected, nil
}

func (u *User) Status(mbox string, items []imap.StatusItem) (*imap.MailboxStatus, error) {
	mboxObj, ok := u.getMailbox(mbox)
	if !ok {
		if err := u.validateMboxName(mbox); err != nil {
			return nil, err
		}
		mboxDir, err := u.storage.Dir(mbox)
		if err != nil {
			return nil, err
		}
		exists, err := mboxDir.Exists()
		if err != nil {
			return nil, fmt.Errorf("I/O error: %w", err)
		}
		if !exists {
			return nil, backend.ErrNoSuchMailbox
		}
		u.mailboxesLock.Lock()
		mboxObj = u.ensureMailbox(mbox)
		u.mailboxesLock.Unlock()
	}

	entries, err := mboxObj.listEntries()
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	if err := mboxObj.loadMetadataState(); err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}

	status := imap.NewMailboxStatus(mboxObj.name, items)
	baseFlags := []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	status.Flags = append([]string{}, baseFlags...)
	status.PermanentFlags = append([]string{}, baseFlags...)
	status.PermanentFlags = append(status.PermanentFlags, "\\*")
	status.UnseenSeqNum = 0

	flagsMap := map[string]struct{}{}
	for _, entry := range entries {
		for _, flag := range entry.meta.flags {
			flagsMap[flag] = struct{}{}
		}
	}
	flagSeen := map[string]struct{}{}
	for _, flag := range status.Flags {
		flagSeen[flag] = struct{}{}
	}
	permSeen := map[string]struct{}{}
	for _, flag := range status.PermanentFlags {
		permSeen[flag] = struct{}{}
	}
	for flag := range flagsMap {
		if _, ok := flagSeen[flag]; !ok {
			status.Flags = append(status.Flags, flag)
			flagSeen[flag] = struct{}{}
		}
		if _, ok := permSeen[flag]; !ok {
			status.PermanentFlags = append(status.PermanentFlags, flag)
			permSeen[flag] = struct{}{}
		}
	}

	for i, entry := range entries {
		seqNum := uint32(i + 1)
		if !hasFlag(entry.meta.flags, imap.SeenFlag) && status.UnseenSeqNum == 0 {
			status.UnseenSeqNum = seqNum
		}
	}

	for _, item := range items {
		switch item {
		case imap.StatusMessages:
			status.Messages = uint32(len(entries))
		case imap.StatusUidNext:
			u.b.statesLock.Lock()
			if mboxObj.state.meta != nil {
				status.UidNext = mboxObj.state.meta.UIDNext
			} else {
				status.UidNext = mboxObj.state.uidNext
			}
			u.b.statesLock.Unlock()
		case imap.StatusUidValidity:
			status.UidValidity = mboxObj.uidValidity()
		case imap.StatusRecent:
			for _, entry := range entries {
				if entry.meta.recent {
					status.Recent++
				}
			}
		case imap.StatusUnseen:
			for _, entry := range entries {
				if !hasFlag(entry.meta.flags, imap.SeenFlag) {
					status.Unseen++
				}
			}
		case imap.StatusAppendLimit:
			limit := u.mailboxLimit(mboxObj)
			if limit == nil {
				limit = mboxObj.CreateMessageLimit()
			}
			if limit != nil {
				status.AppendLimit = *limit
			} else {
				status.AppendLimit = 0
			}
		}
	}

	return status, nil
}

func (u *User) SetSubscribed(mbox string, subscribed bool) error {
	mboxObj, ok := u.getMailbox(mbox)
	if !ok {
		return backend.ErrNoSuchMailbox
	}
	mboxObj.subscribed = subscribed
	return nil
}

func (u *User) CreateMessageLimit() *uint32 {
	u.limitLock.Lock()
	defer u.limitLock.Unlock()

	if u.appendLimit == nil {
		return nil
	}
	val := *u.appendLimit
	return &val
}

func (u *User) SetMessageLimit(val *uint32) error {
	u.limitLock.Lock()
	defer u.limitLock.Unlock()

	if val == nil {
		u.appendLimit = nil
		return nil
	}
	copyVal := *val
	u.appendLimit = &copyVal
	return nil
}

func (u *User) effectiveLimit(selected backend.Mailbox, mbox *Mailbox) *uint32 {
	if limit := u.mailboxLimit(mbox); limit != nil {
		return limit
	}
	if selected != nil {
		if sel, ok := selected.(*SelectedMailbox); ok {
			if limit := sel.CreateMessageLimit(); limit != nil {
				return limit
			}
		}
	}
	if mbox != nil {
		if limit := mbox.CreateMessageLimit(); limit != nil {
			return limit
		}
	}
	if limit := u.CreateMessageLimit(); limit != nil {
		return limit
	}
	return u.b.CreateMessageLimit()
}

func (u *User) setMailboxLimit(name string, val *uint32) {
	u.mboxLimitLock.Lock()
	defer u.mboxLimitLock.Unlock()

	if u.mboxLimits == nil {
		u.mboxLimits = map[string]*uint32{}
	}
	if val == nil {
		delete(u.mboxLimits, name)
		return
	}
	copyVal := *val
	u.mboxLimits[name] = &copyVal
}

func (u *User) mailboxLimit(mbox *Mailbox) *uint32 {
	if mbox == nil {
		return nil
	}
	u.mboxLimitLock.Lock()
	defer u.mboxLimitLock.Unlock()

	if u.mboxLimits == nil {
		return nil
	}
	val, ok := u.mboxLimits[mbox.name]
	if !ok || val == nil {
		return nil
	}
	copyVal := *val
	return &copyVal
}

func (u *User) CreateMessage(mboxName string, flags []string, date time.Time, body imap.Literal, selected backend.Mailbox) error {
	mbox, ok := u.getMailbox(mboxName)
	if !ok {
		if err := u.validateMboxName(mboxName); err != nil {
			return err
		}

		mboxDir, err := u.storage.Dir(mboxName)
		if err != nil {
			return err
		}
		exists, err := mboxDir.Exists()
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		if !exists {
			return backend.ErrNoSuchMailbox
		}

		u.mailboxesLock.Lock()
		mbox = u.ensureMailbox(mboxName)
		u.mailboxesLock.Unlock()
		if mbox == nil {
			return errors.New("I/O error")
		}
	}

	newFlags := flags[:0]
	for _, flag := range flags {
		if flag == imap.RecentFlag {
			continue
		}
		newFlags = append(newFlags, flag)
	}
	flags = uniqueFlags(newFlags)

	if date.IsZero() {
		date = time.Now()
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return errors.New("I/O error, try again later")
	}

	if limit := u.effectiveLimit(selected, mbox); limit != nil {
		if uint32(len(data)) > *limit {
			return backend.ErrTooBig
		}
	}
	if err := mbox.loadMetadataState(); err != nil {
		return errors.New("I/O error, try again later")
	}

	msg, writer, err := mbox.dir.Create(mbox.maildirFlagsFromImap(flags))
	if err != nil {
		return errors.New("I/O error, try again later")
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return errors.New("I/O error, try again later")
	}
	if err := writer.Close(); err != nil {
		return errors.New("I/O error, try again later")
	}
	if err := msg.Chtimes(date, date); err != nil {
		u.b.Log.Printf("CreateMessage: chtimes: %v", err)
	}

	u.b.statesLock.Lock()
	uid := mbox.state.meta.UIDNext
	mbox.state.meta.UIDNext++
	mbox.state.meta.UIDByKey[msg.Key()] = uid
	mbox.state.meta.FilenameByUID[uid] = msg.Name()
	mbox.state.meta.DirtyUIDList = true
	meta := &messageMeta{
		uid:          uid,
		flags:        flags,
		internalDate: date,
	}
	mbox.state.uidNext = mbox.state.meta.UIDNext
	mbox.state.messages[msg.Key()] = meta
	storeRecent := u.b.Manager.NewMessage(mbox.mailboxKey(), meta.uid)
	meta.recent = storeRecent
	u.b.statesLock.Unlock()

	if err := mbox.writeMetadataState(mbox.state.meta); err != nil {
		return errors.New("I/O error, try again later")
	}

	return nil
}

func (u *User) CreateMailbox(name string) error {
	if strings.EqualFold(name, InboxName) {
		u.mailboxesLock.Lock()
		mbox := u.ensureMailbox(InboxName)
		u.mailboxesLock.Unlock()
		if mbox == nil {
			return errors.New("I/O error")
		}
		if err := mbox.dir.Init(); err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		return nil
	}
	if err := u.validateMboxName(name); err != nil {
		return err
	}
	mboxDir, err := u.storage.Dir(name)
	if err != nil {
		return err
	}
	exists, err := mboxDir.Exists()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	if exists {
		return backend.ErrMailboxAlreadyExists
	}

	for _, ancestor := range mailboxAncestors(name) {
		dir, err := u.storage.Dir(ancestor)
		if err != nil {
			return err
		}
		if err := dir.Init(); err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		u.mailboxesLock.Lock()
		mbox := u.ensureMailbox(ancestor)
		u.mailboxesLock.Unlock()
		if mbox == nil {
			return errors.New("I/O error")
		}
	}

	if use, ok := defaultMailboxSpecialUse(name); ok {
		u.mailboxesLock.Lock()
		mbox := u.ensureMailbox(name)
		u.mailboxesLock.Unlock()
		if mbox == nil {
			return errors.New("I/O error")
		}
		if err := u.setMailboxSpecialUse(mbox, use); err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
	}

	return nil
}

func (u *User) DeleteMailbox(name string) error {
	if strings.EqualFold(name, InboxName) {
		return errors.New("cannot delete inbox")
	}
	if err := u.validateMboxName(name); err != nil {
		return err
	}
	mboxDir, err := u.storage.Dir(name)
	if err != nil {
		return err
	}
	exists, err := mboxDir.Exists()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	if !exists {
		return backend.ErrNoSuchMailbox
	}

	childExists := false
	children, err := mboxDir.Children()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	if len(children) > 0 {
		childExists = true
	}

	if err := mboxDir.Remove(childExists); err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}

	u.mailboxesLock.Lock()
	delete(u.mailboxes, name)
	u.mailboxesLock.Unlock()

	u.b.deleteMailboxState(u.name, name)
	u.b.Manager.MailboxDestroyed(u.name + "\x00" + name)
	return nil
}

func (u *User) RenameMailbox(existingName, newName string) error {
	if err := u.validateMboxName(existingName); err != nil {
		return err
	}
	if err := u.validateMboxName(newName); err != nil {
		return err
	}
	if strings.EqualFold(existingName, InboxName) {
		if _, ok := u.getMailbox(newName); ok {
			return backend.ErrMailboxAlreadyExists
		}
		destDir, err := u.storage.Dir(newName)
		if err != nil {
			return err
		}
		exists, err := destDir.Exists()
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		if exists {
			return backend.ErrMailboxAlreadyExists
		}
		if err := destDir.Init(); err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}

		inbox := u.ensureMailbox(existingName)
		dest := u.ensureMailbox(newName)
		if inbox == nil || dest == nil {
			return errors.New("I/O error")
		}

		entries, err := inbox.listEntries()
		if err != nil {
			return fmt.Errorf("I/O error: %w", err)
		}
		for _, entry := range entries {
			if err := entry.msg.MoveTo(dest.dir); err != nil {
				return fmt.Errorf("I/O error: %w", err)
			}
		}

		u.b.statesLock.Lock()
		for key, meta := range inbox.state.messages {
			dest.state.messages[key] = meta
		}
		if dest.state.uidNext < inbox.state.uidNext {
			dest.state.uidNext = inbox.state.uidNext
		}
		inbox.state.messages = map[string]*messageMeta{}
		u.b.statesLock.Unlock()

		return nil
	}
	srcDir, err := u.storage.Dir(existingName)
	if err != nil {
		return err
	}
	exists, err := srcDir.Exists()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	if !exists {
		return backend.ErrNoSuchMailbox
	}
	destDir, err := u.storage.Dir(newName)
	if err != nil {
		return err
	}
	exists, err = destDir.Exists()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	if exists {
		return backend.ErrMailboxAlreadyExists
	}
	relName, err := relativeMailboxName(existingName, newName)
	if err != nil {
		return err
	}
	if err := srcDir.Rename(relName); err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}

	u.mailboxesLock.Lock()
	for name, mbox := range u.mailboxes {
		if strings.HasPrefix(name, existingName) {
			newChild := strings.Replace(name, existingName, newName, 1)
			mbox.name = newChild
			newDir, err := u.storage.Dir(newChild)
			if err != nil {
				continue
			}
			mbox.dir = newDir
			u.mailboxes[newChild] = mbox
			delete(u.mailboxes, name)
			u.b.renameMailboxState(u.name, name, newChild)
			u.b.Manager.MailboxDestroyed(u.name + "\x00" + name)
			u.b.Manager.MailboxDestroyed(u.name + "\x00" + newChild)
		}
	}
	u.mailboxesLock.Unlock()

	return nil
}

func (u *User) Logout() error {
	u.logoutOnce.Do(func() {
		u.mailboxesLock.Lock()
		u.mailboxes = nil
		u.mailboxesLock.Unlock()

		u.mboxLimitLock.Lock()
		u.mboxLimits = nil
		u.mboxLimitLock.Unlock()

		u.b.userLogout(u.name)
	})

	return nil
}
