package imapmaildir

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	sortthread "github.com/emersion/go-imap-sortthread"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	mess "github.com/foxcpp/go-imap-mess"

	"github.com/foxcpp/go-imap-maildir/maildir"
	maildirindex "github.com/foxcpp/go-imap-maildir/maildir/index"
)

var (
	_ sortthread.ThreadMailbox = (*SelectedMailbox)(nil)
	_ sortthread.SortMailbox   = (*SelectedMailbox)(nil)
)

type Mailbox struct {
	b *Backend

	user *User

	dir maildir.Dir

	name string

	state      *mailboxState
	subscribed bool

	limitLock   sync.Mutex
	appendLimit *uint32

	indexLock        sync.Mutex
	indexLoaded      bool
	indexStore       maildir.IndexStore
	index            *maildirindex.Index
	indexAppendCount int
}

type SelectedMailbox struct {
	*Mailbox
	conn     backend.Conn
	readOnly bool
	handle   *mess.MailboxHandle
}

type msgEntry struct {
	msg    maildir.Message
	meta   *messageMeta
	uid    uint32
	seqNum uint32
}

type sortMsg struct {
	uid      uint32
	seq      uint32
	arrival  time.Time
	date     time.Time
	size     uint32
	subject  string
	fromAddr string
	toAddr   string
	ccAddr   string
}

func (m *Mailbox) Name() string {
	return m.name
}

func (m *Mailbox) Info() (*imap.MailboxInfo, error) {
	info := &imap.MailboxInfo{
		Delimiter: HierarchySep,
		Name:      m.name,
	}

	if strings.Count(m.name, HierarchySep) == MaxMboxNesting {
		info.Attributes = append(info.Attributes, imap.NoInferiorsAttr)
	}

	hasChildren := false
	childDir := m.dir
	if strings.EqualFold(m.name, InboxName) {
		inboxDir, err := m.user.storage.Dir(InboxName)
		if err != nil {
			return nil, fmt.Errorf("I/O error: %w", err)
		}
		childDir = inboxDir
	}
	children, err := childDir.Children()
	if err != nil {
		if maildir.IsNotExist(err) {
			info.Attributes = append(info.Attributes, imap.NoSelectAttr)
		} else {
			return nil, fmt.Errorf("I/O error: %w", err)
		}
	} else if len(children) > 0 {
		hasChildren = true
	}
	if hasChildren {
		info.Attributes = append(info.Attributes, imap.HasChildrenAttr)
	} else {
		info.Attributes = append(info.Attributes, imap.HasNoChildrenAttr)
	}

	if specialUses, err := m.user.mailboxSpecialUse(m); err == nil {
		for _, use := range specialUses {
			info.Attributes = append(info.Attributes, use)
		}
	}

	return info, nil
}

func (m *SelectedMailbox) Conn() backend.Conn {
	return m.conn
}

func (m *SelectedMailbox) Poll(expunge bool) error {
	m.handle.Sync(expunge)
	return nil
}

func (m *SelectedMailbox) Check() error {
	return nil
}

func (m *SelectedMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	return m.user.Status(m.name, items)
}

func (m *SelectedMailbox) SetSubscribed(subscribed bool) error {
	return m.user.SetSubscribed(m.name, subscribed)
}

func (m *SelectedMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	return m.user.CreateMessage(m.name, flags, date, body, nil)
}

func (m *SelectedMailbox) Idle(done <-chan struct{}) {
	m.handle.Idle(done)
}

func (m *SelectedMailbox) Close() error {
	return m.handle.Close()
}

func (m *Mailbox) CreateMessageLimit() *uint32 {
	m.limitLock.Lock()
	defer m.limitLock.Unlock()

	if m.appendLimit == nil {
		return nil
	}
	val := *m.appendLimit
	return &val
}

func (m *Mailbox) updateUidlistFilename(uid uint32, msg maildir.Message) {
	if m.state.meta == nil {
		return
	}
	filename := msg.Name()
	if m.state.meta.FilenameByUID[uid] != filename {
		m.state.meta.FilenameByUID[uid] = filename
		m.state.meta.DirtyUIDList = true
	}
}

func (m *Mailbox) SetMessageLimit(val *uint32) error {
	m.limitLock.Lock()
	defer m.limitLock.Unlock()

	if val == nil {
		m.appendLimit = nil
		return nil
	}
	copyVal := *val
	m.appendLimit = &copyVal
	return nil
}

func (m *SelectedMailbox) CreateMessageLimit() *uint32 {
	return m.Mailbox.CreateMessageLimit()
}

func (m *SelectedMailbox) SetMessageLimit(val *uint32) error {
	if err := m.Mailbox.SetMessageLimit(val); err != nil {
		return err
	}
	m.user.setMailboxLimit(m.name, val)
	return nil
}

func (m *Mailbox) mailboxKey() string {
	return m.user.name + "\x00" + m.name
}

func (m *Mailbox) uidValidity() uint32 {
	_ = m.loadMetadataState()
	m.b.statesLock.Lock()
	defer m.b.statesLock.Unlock()

	if m.state.meta != nil && m.state.meta.UIDValidity != 0 {
		return m.state.meta.UIDValidity
	}
	if m.state.uidValidity == 0 {
		m.state.uidValidity = 1
	}
	return m.state.uidValidity
}

func (m *Mailbox) listEntries() ([]msgEntry, error) {
	recentMsgs, err := m.dir.Unseen()
	if err != nil {
		if !maildir.IsNotExist(err) {
			return nil, err
		}
		recentMsgs = nil
	}
	recentKeys := map[string]struct{}{}
	for _, msg := range recentMsgs {
		recentKeys[msg.Key()] = struct{}{}
	}

	if err := m.loadMetadataState(); err != nil {
		return nil, err
	}

	msgs, err := m.dir.Messages()
	if err != nil {
		return nil, err
	}

	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].Key() < msgs[j].Key()
	})

	present := make(map[string]bool, len(msgs))
	entries := make([]msgEntry, 0, len(msgs))

	m.b.statesLock.Lock()
	defer m.b.statesLock.Unlock()

	state := m.state.meta
	for _, msg := range msgs {
		key := msg.Key()
		present[key] = true

		meta, ok := m.state.messages[key]
		if !ok {
			meta = &messageMeta{}
		}

		internalDate := meta.internalDate
		if internalDate.IsZero() {
			if info, err := msg.Stat(); err == nil {
				internalDate = info.ModTime()
			}
		}

		uid := state.UIDByKey[key]
		if uid == 0 {
			uid = state.UIDNext
			state.UIDNext++
			state.UIDByKey[key] = uid
			state.DirtyUIDList = true
			storeRecent := m.b.Manager.NewMessage(m.mailboxKey(), uid)
			meta.recent = storeRecent
		}

		meta.uid = uid
		meta.flags = m.imapFlagsFromMaildir(msg.Flags())
		meta.internalDate = internalDate
		if _, ok := recentKeys[key]; ok {
			meta.recent = true
		}
		m.state.messages[key] = meta

		if _, ok := recentKeys[key]; ok {
			meta.recent = true
		}

		filename := msg.Name()
		if state.FilenameByUID[meta.uid] != filename {
			state.FilenameByUID[meta.uid] = filename
			state.DirtyUIDList = true
		}

		entries = append(entries, msgEntry{msg: msg, meta: meta, uid: meta.uid})
	}

	for key := range m.state.messages {
		if !present[key] {
			delete(m.state.messages, key)
		}
	}

	m.state.uidNext = state.UIDNext
	m.state.uidValidity = state.UIDValidity

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].uid < entries[j].uid
	})
	for i := range entries {
		entries[i].seqNum = uint32(i + 1)
	}

	if err := m.writeMetadataState(state); err != nil {
		return nil, err
	}

	return entries, nil
}

func (m *Mailbox) imapFlagsFromMaildir(flags []maildir.Flag) []string {
	_ = m.loadMetadataState()
	state := m.state.meta
	if state == nil {
		state = &maildir.Metadata{}
		state.Ensure()
	}

	imapFlags := make([]string, 0, len(flags))
	for _, flag := range flags {
		switch flag {
		case maildir.FlagSeen:
			imapFlags = append(imapFlags, imap.SeenFlag)
		case maildir.FlagReplied:
			imapFlags = append(imapFlags, imap.AnsweredFlag)
		case maildir.FlagFlagged:
			imapFlags = append(imapFlags, imap.FlaggedFlag)
		case maildir.FlagTrashed:
			imapFlags = append(imapFlags, imap.DeletedFlag)
		case maildir.FlagDraft:
			imapFlags = append(imapFlags, imap.DraftFlag)
		default:
			if name, ok := state.NameByKeyword[rune(flag)]; ok {
				imapFlags = append(imapFlags, name)
			}
		}
	}
	return imapFlags
}

func (m *Mailbox) maildirFlagsFromImap(flags []string) []maildir.Flag {
	_ = m.loadMetadataState()
	state := m.state.meta
	if state == nil {
		state = &maildir.Metadata{}
		state.Ensure()
	}

	seen := false
	replied := false
	flagged := false
	trashed := false
	draft := false
	keywords := make([]rune, 0, len(flags))

	for _, flag := range flags {
		switch flag {
		case imap.SeenFlag:
			seen = true
		case imap.AnsweredFlag:
			replied = true
		case imap.FlaggedFlag:
			flagged = true
		case imap.DeletedFlag:
			trashed = true
		case imap.DraftFlag:
			draft = true
		case imap.RecentFlag:
			continue
		default:
			if letter := state.EnsureKeyword(flag); letter != 0 {
				keywords = append(keywords, letter)
			}
		}
	}

	result := make([]maildir.Flag, 0, 5)
	if seen {
		result = append(result, maildir.FlagSeen)
	}
	if replied {
		result = append(result, maildir.FlagReplied)
	}
	if flagged {
		result = append(result, maildir.FlagFlagged)
	}
	if trashed {
		result = append(result, maildir.FlagTrashed)
	}
	if draft {
		result = append(result, maildir.FlagDraft)
	}
	for _, letter := range keywords {
		result = append(result, maildir.Flag(letter))
	}
	return result
}

func hasFlag(flags []string, flag string) bool {
	return slices.Contains(flags, flag)
}

func uniqueFlags(flags []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if _, ok := seen[flag]; ok {
			continue
		}
		seen[flag] = struct{}{}
		result = append(result, flag)
	}
	return result
}

func (m *Mailbox) entryFlags(entry msgEntry, recent bool) []string {
	flags := entry.meta.flags
	if recent {
		flags = append(flags, imap.RecentFlag)
	}
	return uniqueFlags(flags)
}

func (m *SelectedMailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer m.handle.Sync(false)
	defer close(ch)

	entries, err := m.listEntries()
	if err != nil {
		return errors.New("I/O error, try again later")
	}

	shouldSetSeen := false
	for _, item := range items {
		sect, err := imap.ParseBodySectionName(item)
		if err != nil {
			continue
		}
		if !sect.Peek {
			shouldSetSeen = true
		}
	}

	itemsToFetch := items
	if shouldSetSeen && !containsFetchItem(items, imap.FetchFlags) {
		itemsToFetch = append(append([]imap.FetchItem{}, items...), imap.FetchFlags)
	}

	seqset, err = m.handle.ResolveSeq(uid, seqset)
	if err != nil {
		if uid {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !seqset.Contains(entry.uid) {
			continue
		}

		seq, ok := m.handle.UidAsSeq(entry.uid)
		if !ok {
			continue
		}

		if shouldSetSeen && !hasFlag(entry.meta.flags, imap.SeenFlag) {
			m.b.statesLock.Lock()
			entry.meta.flags = uniqueFlags(append(entry.meta.flags, imap.SeenFlag))
			m.b.statesLock.Unlock()
			if err := entry.msg.SetFlags(m.maildirFlagsFromImap(entry.meta.flags)); err == nil {
				m.handle.FlagsChanged(entry.uid, entry.meta.flags, false)
			}
		}

		msg, err := m.fetch(seq, entry, itemsToFetch, m.handle.IsRecent(entry.uid))
		if err != nil {
			continue
		}

		ch <- msg
	}

	return nil
}

func (m *SelectedMailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	entries, err := m.listEntries()
	if err != nil {
		return nil, errors.New("I/O error, try again later")
	}

	m.handle.ResolveCriteria(criteria)
	defer m.handle.Sync(uid)

	needsBody := searchNeedsBody(criteria)
	needsFullHeaders := searchNeedsUnindexedHeaders(criteria)
	idx, idxStore, idxErr := m.loadIndex()
	if idxErr != nil {
		idx = nil
		idxStore = nil
	}

	var ids []uint32
	for _, entry := range entries {
		seq, ok := m.handle.UidAsSeq(entry.uid)
		if !ok {
			continue
		}

		var entity *message.Entity
		if needsBody {
			entity, err = m.messageEntity(entry.msg)
			if entity == nil {
				if err != nil {
					continue
				}
				continue
			}
		} else {
			var header textproto.Header
			if needsFullHeaders {
				header, err = m.messageHeader(entry.msg)
			} else {
				header, _, err = m.headerForEntry(entry, idx, idxStore)
			}
			if err != nil {
				continue
			}
			entity, err = message.New(message.Header{Header: header}, bytes.NewReader(nil))
			if err != nil {
				continue
			}
		}

		flags := m.entryFlags(entry, m.handle.IsRecent(entry.uid))
		ok, err = backendutil.Match(entity, seq, entry.uid, entry.meta.internalDate, flags, criteria)
		if err != nil || !ok {
			continue
		}

		if uid {
			ids = append(ids, entry.uid)
		} else {
			ids = append(ids, seq)
		}
	}

	return ids, nil
}

func (m *SelectedMailbox) Sort(uid bool, sortCrit []sortthread.SortCriterion, searchCrit *imap.SearchCriteria) ([]uint32, error) {
	entries, err := m.listEntries()
	if err != nil {
		return nil, errors.New("I/O error, try again later")
	}

	m.handle.ResolveCriteria(searchCrit)
	defer m.handle.Sync(uid)

	if len(sortCrit) == 0 {
		sortCrit = []sortthread.SortCriterion{{Field: sortthread.SortArrival}}
	}

	needsBody := searchNeedsBody(searchCrit)
	needsFullHeaders := searchNeedsUnindexedHeaders(searchCrit)
	idx, idxStore, idxErr := m.loadIndex()
	if idxErr != nil {
		idx = nil
		idxStore = nil
	}

	matched := make([]sortMsg, 0, len(entries))
	for _, entry := range entries {
		seq, ok := m.handle.UidAsSeq(entry.uid)
		if !ok {
			continue
		}

		var header textproto.Header
		var entity *message.Entity
		if needsBody {
			entity, err = m.messageEntity(entry.msg)
			if entity == nil {
				if err != nil {
					continue
				}
				continue
			}
			header = entity.Header.Header
		} else {
			if needsFullHeaders {
				header, err = m.messageHeader(entry.msg)
			} else {
				header, _, err = m.headerForEntry(entry, idx, idxStore)
			}
			if err != nil {
				continue
			}
			entity, err = message.New(message.Header{Header: header}, bytes.NewReader(nil))
			if err != nil {
				continue
			}
		}

		flags := m.entryFlags(entry, m.handle.IsRecent(entry.uid))
		ok, err = backendutil.Match(entity, seq, entry.uid, entry.meta.internalDate, flags, searchCrit)
		if err != nil || !ok {
			continue
		}

		mh := mail.Header{Header: message.Header{Header: header}}
		subject, err := mh.Subject()
		if err != nil {
			subject = header.Get("Subject")
		}

		date, err := mh.Date()
		if err != nil {
			date = entry.meta.internalDate
			if date.IsZero() {
				if info, statErr := entry.msg.Stat(); statErr == nil {
					date = info.ModTime()
				}
			}
		} else {
			date = date.UTC()
		}

		matched = append(matched, sortMsg{
			uid:      entry.uid,
			seq:      seq,
			arrival:  entry.meta.internalDate,
			date:     date,
			size:     m.messageSize(entry.msg),
			subject:  strings.ToLower(subject),
			fromAddr: firstAddressSortKey(&mh, "From", header.Get("From")),
			toAddr:   firstAddressSortKey(&mh, "To", header.Get("To")),
			ccAddr:   firstAddressSortKey(&mh, "Cc", header.Get("Cc")),
		})
	}

	sort.Slice(matched, func(i, j int) bool {
		for _, crit := range sortCrit {
			cmp := compareSortField(matched[i], matched[j], crit.Field)
			if cmp == 0 {
				continue
			}
			if crit.Reverse {
				return cmp > 0
			}
			return cmp < 0
		}
		return matched[i].seq < matched[j].seq
	})

	ids := make([]uint32, 0, len(matched))
	for _, entry := range matched {
		if uid {
			ids = append(ids, entry.uid)
			continue
		}
		ids = append(ids, entry.seq)
	}

	return ids, nil
}

func (m *SelectedMailbox) Thread(uid bool, threading sortthread.ThreadAlgorithm, searchCrit *imap.SearchCriteria) ([]*sortthread.Thread, error) {
	if threading != sortthread.OrderedSubject {
		return nil, errors.New("Unsupported threading algorithm")
	}

	entries, err := m.listEntries()
	if err != nil {
		return nil, errors.New("I/O error, try again later")
	}

	m.handle.ResolveCriteria(searchCrit)
	defer m.handle.Sync(uid)

	needsBody := searchNeedsBody(searchCrit)
	needsFullHeaders := searchNeedsUnindexedHeaders(searchCrit)
	idx, idxStore, idxErr := m.loadIndex()
	if idxErr != nil {
		idx = nil
		idxStore = nil
	}

	type threadMsg struct {
		id       uint32
		sentDate time.Time
	}

	threads := make(map[string][]threadMsg)
	matchedCount := 0

	for _, entry := range entries {
		seq, ok := m.handle.UidAsSeq(entry.uid)
		if !ok {
			continue
		}

		var header textproto.Header
		var entity *message.Entity
		if needsBody {
			entity, err = m.messageEntity(entry.msg)
			if entity == nil {
				if err != nil {
					continue
				}
				continue
			}
			header = entity.Header.Header
		} else {
			if needsFullHeaders {
				header, err = m.messageHeader(entry.msg)
			} else {
				header, _, err = m.headerForEntry(entry, idx, idxStore)
			}
			if err != nil {
				continue
			}
			entity, err = message.New(message.Header{Header: header}, bytes.NewReader(nil))
			if err != nil {
				continue
			}
		}

		flags := m.entryFlags(entry, m.handle.IsRecent(entry.uid))
		ok, err = backendutil.Match(entity, seq, entry.uid, entry.meta.internalDate, flags, searchCrit)
		if err != nil || !ok {
			continue
		}

		id := entry.uid
		if !uid {
			id = seq
		}

		mailHeader := mail.Header{Header: message.Header{Header: header}}
		subject, err := mailHeader.Subject()
		if err != nil {
			subject = header.Get("Subject")
		}
		baseSubject, _ := sortthread.GetBaseSubject(subject)

		sentDate, err := mailHeader.Date()
		if err != nil {
			sentDate = entry.meta.internalDate
			if sentDate.IsZero() {
				if info, statErr := entry.msg.Stat(); statErr == nil {
					sentDate = info.ModTime()
				}
			}
		} else {
			sentDate = sentDate.UTC()
		}

		threads[baseSubject] = append(threads[baseSubject], threadMsg{
			id:       id,
			sentDate: sentDate,
		})
		matchedCount++
	}

	if matchedCount == 0 {
		return []*sortthread.Thread{}, nil
	}

	sortedThreads := make([][]threadMsg, 0, len(threads))
	for _, thread := range threads {
		sort.Slice(thread, func(i, j int) bool {
			if thread[i].sentDate.Equal(thread[j].sentDate) {
				return thread[i].id < thread[j].id
			}
			return thread[i].sentDate.Before(thread[j].sentDate)
		})
		sortedThreads = append(sortedThreads, thread)
	}

	sort.Slice(sortedThreads, func(i, j int) bool {
		if sortedThreads[i][0].sentDate.Equal(sortedThreads[j][0].sentDate) {
			return sortedThreads[i][0].id < sortedThreads[j][0].id
		}
		return sortedThreads[i][0].sentDate.Before(sortedThreads[j][0].sentDate)
	})

	threadsTree := make([]sortthread.Thread, matchedCount)
	nodeOffset := 0
	result := make([]*sortthread.Thread, 0, len(sortedThreads))

	for _, thread := range sortedThreads {
		if len(thread) == 0 {
			continue
		}
		current := &threadsTree[nodeOffset]
		nodeOffset++
		result = append(result, current)
		current.Id = thread[0].id
		for _, msg := range thread[1:] {
			next := &threadsTree[nodeOffset]
			nodeOffset++
			next.Id = msg.id
			current.Children = []*sortthread.Thread{next}
			current = next
		}
	}

	return result, nil
}

func firstAddressSortKey(header *mail.Header, field, fallback string) string {
	addrs, err := header.AddressList(field)
	if err == nil && len(addrs) > 0 {
		if addr := strings.TrimSpace(addrs[0].Address); addr != "" {
			return strings.ToLower(addr)
		}
		if name := strings.TrimSpace(addrs[0].Name); name != "" {
			return strings.ToLower(name)
		}
	}
	return strings.ToLower(strings.TrimSpace(fallback))
}

func compareSortField(a, b sortMsg, field sortthread.SortField) int {
	switch field {
	case sortthread.SortArrival:
		return compareTime(a.arrival, b.arrival)
	case sortthread.SortCc:
		return strings.Compare(a.ccAddr, b.ccAddr)
	case sortthread.SortDate:
		return compareTime(a.date, b.date)
	case sortthread.SortFrom:
		return strings.Compare(a.fromAddr, b.fromAddr)
	case sortthread.SortSize:
		if a.size < b.size {
			return -1
		}
		if a.size > b.size {
			return 1
		}
		return 0
	case sortthread.SortSubject:
		return strings.Compare(a.subject, b.subject)
	case sortthread.SortTo:
		return strings.Compare(a.toAddr, b.toAddr)
	default:
		return 0
	}
}

func compareTime(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.Before(b) {
		return -1
	}
	return 1
}

func searchNeedsBody(criteria *imap.SearchCriteria) bool {
	if criteria == nil {
		return false
	}
	if len(criteria.Body) > 0 || len(criteria.Text) > 0 || criteria.Larger > 0 || criteria.Smaller > 0 {
		return true
	}
	for _, not := range criteria.Not {
		if searchNeedsBody(not) {
			return true
		}
	}
	for _, or := range criteria.Or {
		if searchNeedsBody(or[0]) || searchNeedsBody(or[1]) {
			return true
		}
	}
	return false
}

func searchNeedsUnindexedHeaders(criteria *imap.SearchCriteria) bool {
	if criteria == nil {
		return false
	}
	for name := range criteria.Header {
		if _, ok := maildirindex.FieldIDFromName(name); !ok {
			return true
		}
	}
	for _, not := range criteria.Not {
		if searchNeedsUnindexedHeaders(not) {
			return true
		}
	}
	for _, or := range criteria.Or {
		if searchNeedsUnindexedHeaders(or[0]) || searchNeedsUnindexedHeaders(or[1]) {
			return true
		}
	}
	return false
}

func (m *Mailbox) loadIndex() (*maildirindex.Index, maildir.IndexStore, error) {
	m.indexLock.Lock()
	defer m.indexLock.Unlock()

	if m.indexLoaded {
		return m.index, m.indexStore, nil
	}
	if err := m.loadMetadataState(); err != nil {
		return nil, nil, err
	}
	provider, ok := m.dir.(maildir.IndexProvider)
	if !ok {
		m.indexLoaded = true
		return nil, nil, nil
	}
	store, err := provider.IndexStore(m.state.meta.GUID)
	if err != nil {
		return nil, nil, err
	}
	idx, err := store.Load()
	if err != nil {
		return nil, nil, err
	}
	m.indexStore = store
	m.index = idx
	m.indexLoaded = true
	return idx, store, nil
}

func (m *Mailbox) headerForEntry(entry msgEntry, idx *maildirindex.Index, store maildir.IndexStore) (textproto.Header, *maildirindex.Entry, error) {
	if idx != nil {
		m.indexLock.Lock()
		cached, ok := idx.Get(entry.msg.Key())
		m.indexLock.Unlock()
		if ok {
			return m.headerFromIndexEntry(cached), cached, nil
		}
	}
	header, err := m.messageHeader(entry.msg)
	if err != nil {
		return textproto.Header{}, nil, err
	}
	size := m.messageSize(entry.msg)
	newEntry := m.buildIndexEntry(entry, header, size)
	if idx != nil && store != nil && newEntry != nil {
		m.upsertIndex(idx, store, newEntry)
	}
	return header, newEntry, nil
}

func (m *Mailbox) entryHeaderFromIndex(entry msgEntry, idx *maildirindex.Index) (textproto.Header, bool) {
	if idx == nil {
		return textproto.Header{}, false
	}
	m.indexLock.Lock()
	cached, ok := idx.Get(entry.msg.Key())
	m.indexLock.Unlock()
	if !ok {
		return textproto.Header{}, false
	}
	return m.headerFromIndexEntry(cached), true
}

func (m *Mailbox) headerFromIndexEntry(entry *maildirindex.Entry) textproto.Header {
	var header textproto.Header
	if entry == nil || entry.Headers == nil {
		return header
	}
	ids := make([]int, 0, len(entry.Headers))
	for id := range entry.Headers {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, idVal := range ids {
		id := maildirindex.FieldID(idVal)
		name := maildirindex.FieldName(id)
		if name == "" {
			continue
		}
		for _, value := range entry.Headers[id] {
			header.Add(name, value)
		}
	}
	return header
}

func (m *Mailbox) buildIndexEntry(entry msgEntry, header textproto.Header, size uint32) *maildirindex.Entry {
	if entry.msg == nil {
		return nil
	}
	values := map[maildirindex.FieldID][]string{}
	fields := []maildirindex.FieldID{
		maildirindex.FieldSubject,
		maildirindex.FieldFrom,
		maildirindex.FieldTo,
		maildirindex.FieldCc,
		maildirindex.FieldDate,
		maildirindex.FieldMsgID,
		maildirindex.FieldInReply,
		maildirindex.FieldRefs,
	}
	for _, id := range fields {
		name := maildirindex.FieldName(id)
		if name == "" {
			continue
		}
		vals := headerValues(header, name)
		if len(vals) > 0 {
			values[id] = vals
		}
	}
	internalDate := int64(0)
	if !entry.meta.internalDate.IsZero() {
		internalDate = entry.meta.internalDate.Unix()
	}
	return &maildirindex.Entry{
		Key:          entry.msg.Key(),
		UID:          entry.uid,
		InternalDate: internalDate,
		Size:         size,
		Headers:      values,
	}
}

func headerValues(header textproto.Header, key string) []string {
	msgHeader := message.Header{Header: header}
	fields := msgHeader.FieldsByKey(key)
	var values []string
	for fields.Next() {
		value, err := fields.Text()
		if err != nil {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func (m *Mailbox) upsertIndex(idx *maildirindex.Index, store maildir.IndexStore, entry *maildirindex.Entry) {
	if idx == nil || store == nil || entry == nil {
		return
	}
	m.indexLock.Lock()
	defer m.indexLock.Unlock()
	idx.Upsert(entry)
	_ = store.Append(maildirindex.UpsertRecord(entry))
	m.indexAppendCount++
	if m.indexAppendCount >= 128 {
		_ = store.Snapshot(idx)
		m.indexAppendCount = 0
	}
}

func (m *Mailbox) deleteIndex(key string) {
	idx, store, err := m.loadIndex()
	if err != nil || idx == nil || store == nil {
		return
	}
	m.indexLock.Lock()
	defer m.indexLock.Unlock()
	idx.Delete(key)
	_ = store.Append(maildirindex.DeleteRecord(key))
	m.indexAppendCount++
	if m.indexAppendCount >= 128 {
		_ = store.Snapshot(idx)
		m.indexAppendCount = 0
	}
}

func (m *Mailbox) messageHeader(msg maildir.Message) (textproto.Header, error) {
	file, err := msg.Open()
	if err != nil {
		return textproto.Header{}, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	return textproto.ReadHeader(reader)
}

func (m *Mailbox) messageSize(msg maildir.Message) uint32 {
	info, err := msg.Stat()
	if err != nil {
		return 0
	}
	size := info.Size()
	if size <= 0 {
		return 0
	}
	if size > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(size)
}

func (m *Mailbox) registerNewKeys(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := m.loadMetadataState(); err != nil {
		return err
	}

	m.b.statesLock.Lock()
	defer m.b.statesLock.Unlock()

	state := m.state.meta
	for _, key := range keys {
		if key == "" {
			continue
		}
		if state.UIDByKey[key] != 0 {
			continue
		}
		uid := state.UIDNext
		state.UIDNext++
		state.UIDByKey[key] = uid
		state.FilenameByUID[uid] = key
		state.DirtyUIDList = true
		meta := &messageMeta{uid: uid}
		storeRecent := m.b.Manager.NewMessage(m.mailboxKey(), uid)
		meta.recent = storeRecent
		m.state.messages[key] = meta
	}

	m.state.uidNext = state.UIDNext
	m.state.uidValidity = state.UIDValidity

	if err := m.writeMetadataState(state); err != nil {
		return err
	}
	return nil
}

func (m *SelectedMailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, operation imap.FlagsOp, silent bool, flags []string) error {
	if err := m.loadMetadataState(); err != nil {
		return errors.New("I/O error, try again later")
	}
	state := m.state.meta

	newFlags := flags[:0]
	for _, flag := range flags {
		if flag == imap.RecentFlag {
			continue
		}
		newFlags = append(newFlags, flag)
	}
	flags = newFlags

	entries, err := m.listEntries()
	if err != nil {
		return errors.New("I/O error, try again later")
	}

	defer m.handle.Sync(uid)

	seqset, err = m.handle.ResolveSeq(uid, seqset)
	if err != nil {
		if uid {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !seqset.Contains(entry.uid) {
			continue
		}

		m.b.statesLock.Lock()
		entry.meta.flags = uniqueFlags(backendutil.UpdateFlags(entry.meta.flags, operation, flags))
		m.b.statesLock.Unlock()

		if err := entry.msg.SetFlags(m.maildirFlagsFromImap(entry.meta.flags)); err != nil {
			continue
		}
		m.updateUidlistFilename(entry.uid, entry.msg)
		m.handle.FlagsChanged(entry.uid, entry.meta.flags, silent)
	}

	if err := m.writeMetadataState(state); err != nil {
		return errors.New("I/O error, try again later")
	}

	return nil
}

func (m *SelectedMailbox) CopyMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	destMbox, ok := m.user.getMailbox(dest)
	if !ok {
		if err := m.user.validateMboxName(dest); err != nil {
			return err
		}
		mboxDir, err := m.user.storage.Dir(dest)
		if err != nil {
			return err
		}
		exists, err := mboxDir.Exists()
		if err != nil {
			return errors.New("I/O error, try again later")
		}
		if !exists {
			return backend.ErrNoSuchMailbox
		}
		m.user.mailboxesLock.Lock()
		destMbox = m.user.ensureMailbox(dest)
		m.user.mailboxesLock.Unlock()
	}
	if err := destMbox.loadMetadataState(); err != nil {
		return errors.New("I/O error, try again later")
	}

	entries, err := m.listEntries()
	if err != nil {
		return errors.New("I/O error, try again later")
	}

	srcIdx, _, _ := m.loadIndex()
	destIdx, destStore, _ := destMbox.loadIndex()

	defer m.handle.Sync(true)

	seqset, err = m.handle.ResolveSeq(uid, seqset)
	if err != nil {
		if uid {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !seqset.Contains(entry.uid) {
			continue
		}

		header, headerOk := m.entryHeaderFromIndex(entry, srcIdx)
		if !headerOk {
			if hdr, err := m.messageHeader(entry.msg); err == nil {
				header = hdr
				headerOk = true
			}
		}

		src, err := entry.msg.Open()
		if err != nil {
			continue
		}
		copied, writer, err := destMbox.dir.Create(destMbox.maildirFlagsFromImap(entry.meta.flags))
		if err != nil {
			src.Close()
			return errors.New("I/O error, try again later")
		}
		if _, err := io.Copy(writer, src); err != nil {
			_ = writer.Close()
			src.Close()
			return errors.New("I/O error, try again later")
		}
		if err := writer.Close(); err != nil {
			src.Close()
			return errors.New("I/O error, try again later")
		}
		_ = src.Close()

		if !entry.meta.internalDate.IsZero() {
			if err := copied.Chtimes(entry.meta.internalDate, entry.meta.internalDate); err != nil {
				m.b.Log.Printf("CopyMessages: chtimes: %v", err)
			}
		}

		m.b.statesLock.Lock()
		uid := destMbox.state.meta.UIDNext
		destMbox.state.meta.UIDNext++
		destMbox.state.meta.UIDByKey[copied.Key()] = uid
		destMbox.state.meta.FilenameByUID[uid] = copied.Name()
		destMbox.state.meta.DirtyUIDList = true
		destMbox.state.meta.DirtyUIDList = true
		meta := &messageMeta{
			uid:          uid,
			flags:        append([]string{}, entry.meta.flags...),
			internalDate: entry.meta.internalDate,
		}
		destMbox.state.uidNext = destMbox.state.meta.UIDNext
		destMbox.state.messages[copied.Key()] = meta
		storeRecent := m.b.Manager.NewMessage(destMbox.mailboxKey(), meta.uid)
		meta.recent = storeRecent
		m.b.statesLock.Unlock()

		if headerOk && destIdx != nil && destStore != nil {
			size := destMbox.messageSize(copied)
			indexEntry := destMbox.buildIndexEntry(msgEntry{msg: copied, meta: meta, uid: uid}, header, size)
			destMbox.upsertIndex(destIdx, destStore, indexEntry)
		}
	}

	if err := destMbox.writeMetadataState(destMbox.state.meta); err != nil {
		return errors.New("I/O error, try again later")
	}

	return nil
}

func (m *SelectedMailbox) MoveMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	destMbox, ok := m.user.getMailbox(dest)
	if !ok {
		if err := m.user.validateMboxName(dest); err != nil {
			return err
		}
		mboxDir, err := m.user.storage.Dir(dest)
		if err != nil {
			return err
		}
		exists, err := mboxDir.Exists()
		if err != nil {
			return errors.New("I/O error, try again later")
		}
		if !exists {
			return backend.ErrNoSuchMailbox
		}
		m.user.mailboxesLock.Lock()
		destMbox = m.user.ensureMailbox(dest)
		m.user.mailboxesLock.Unlock()
	}
	if err := destMbox.loadMetadataState(); err != nil {
		return errors.New("I/O error, try again later")
	}

	entries, err := m.listEntries()
	if err != nil {
		return errors.New("I/O error, try again later")
	}

	srcIdx, _, _ := m.loadIndex()
	destIdx, destStore, _ := destMbox.loadIndex()

	defer m.handle.Sync(true)

	seqset, err = m.handle.ResolveSeq(uid, seqset)
	if err != nil {
		if uid {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !seqset.Contains(entry.uid) {
			continue
		}

		header, headerOk := m.entryHeaderFromIndex(entry, srcIdx)
		if !headerOk {
			if hdr, err := m.messageHeader(entry.msg); err == nil {
				header = hdr
				headerOk = true
			}
		}

		src, err := entry.msg.Open()
		if err != nil {
			continue
		}
		copied, writer, err := destMbox.dir.Create(destMbox.maildirFlagsFromImap(entry.meta.flags))
		if err != nil {
			src.Close()
			return errors.New("I/O error, try again later")
		}
		if _, err := io.Copy(writer, src); err != nil {
			_ = writer.Close()
			src.Close()
			return errors.New("I/O error, try again later")
		}
		if err := writer.Close(); err != nil {
			src.Close()
			return errors.New("I/O error, try again later")
		}
		_ = src.Close()

		if !entry.meta.internalDate.IsZero() {
			if err := copied.Chtimes(entry.meta.internalDate, entry.meta.internalDate); err != nil {
				m.b.Log.Printf("MoveMessages: chtimes: %v", err)
			}
		}

		m.b.statesLock.Lock()
		uid := destMbox.state.meta.UIDNext
		destMbox.state.meta.UIDNext++
		destMbox.state.meta.UIDByKey[copied.Key()] = uid
		destMbox.state.meta.FilenameByUID[uid] = copied.Name()
		destMbox.state.meta.DirtyUIDList = true
		destMbox.state.meta.DirtyUIDList = true
		meta := &messageMeta{
			uid:          uid,
			flags:        append([]string{}, entry.meta.flags...),
			internalDate: entry.meta.internalDate,
		}
		destMbox.state.uidNext = destMbox.state.meta.UIDNext
		destMbox.state.messages[copied.Key()] = meta
		storeRecent := m.b.Manager.NewMessage(destMbox.mailboxKey(), meta.uid)
		meta.recent = storeRecent
		delete(m.state.messages, entry.msg.Key())
		m.b.statesLock.Unlock()

		if headerOk && destIdx != nil && destStore != nil {
			size := destMbox.messageSize(copied)
			indexEntry := destMbox.buildIndexEntry(msgEntry{msg: copied, meta: meta, uid: uid}, header, size)
			destMbox.upsertIndex(destIdx, destStore, indexEntry)
		}

		if err := entry.msg.Remove(); err != nil {
			m.b.Log.Printf("MoveMessages: remove: %v", err)
		}
		m.deleteIndex(entry.msg.Key())

		m.handle.Removed(entry.uid)
	}

	if err := destMbox.writeMetadataState(destMbox.state.meta); err != nil {
		return errors.New("I/O error, try again later")
	}

	return nil
}

func (m *SelectedMailbox) Expunge() error {
	entries, err := m.listEntries()
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}

	for _, entry := range entries {
		if !hasFlag(entry.meta.flags, imap.DeletedFlag) {
			continue
		}
		if err := entry.msg.Remove(); err != nil {
			m.b.Log.Printf("Expunge: %v", err)
			continue
		}

		m.b.statesLock.Lock()
		delete(m.state.messages, entry.msg.Key())
		m.b.statesLock.Unlock()
		m.deleteIndex(entry.msg.Key())

		m.handle.Removed(entry.uid)
	}

	m.handle.Sync(true)
	return nil
}

func (m *Mailbox) messageEntity(msg maildir.Message) (*message.Entity, error) {
	file, err := msg.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	return message.Read(bytes.NewReader(data))
}

func containsFetchItem(items []imap.FetchItem, item imap.FetchItem) bool {
	return slices.Contains(items, item)
}
