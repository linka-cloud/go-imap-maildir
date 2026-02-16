package maildir

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"
)

func MigrateStorage(src, dst Storage) error {
	if src == nil || dst == nil {
		return errors.New("maildir: missing storage")
	}

	if err := migrateAttributes(src, dst); err != nil {
		return err
	}

	mailboxes, err := listMailboxes(src)
	if err != nil {
		return err
	}
	if err := migrateMailboxes(src, dst, mailboxes); err != nil {
		return err
	}

	return nil
}

func migrateMailboxes(src, dst Storage, mailboxes []string) error {
	if len(mailboxes) == 0 {
		return nil
	}
	workers := min(max(runtime.GOMAXPROCS(0), 1), len(mailboxes))

	workCh := make(chan string)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	done := make(chan struct{})
	setErr := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			close(done)
		})
	}

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case name, ok := <-workCh:
				if !ok {
					return
				}
				if err := migrateMailbox(src, dst, name); err != nil {
					setErr(err)
				}
			}
		}
	}

	for range workers {
		wg.Add(1)
		go worker()
	}
mailboxLoop:
	for _, name := range mailboxes {
		select {
		case <-done:
			break mailboxLoop
		case workCh <- name:
		}
	}
	close(workCh)
	wg.Wait()
	return firstErr
}

func listMailboxes(src Storage) ([]string, error) {
	names, err := src.ListDirs()
	if err != nil {
		return nil, fmt.Errorf("maildir: list mailboxes: %w", err)
	}

	var result []string
	seen := map[string]struct{}{}
	addName := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}

	if inbox, err := src.Dir("INBOX"); err == nil {
		exists, err := inbox.Exists()
		if err != nil {
			return nil, fmt.Errorf("maildir: check inbox: %w", err)
		}
		if exists {
			addName("INBOX")
		}
	}
	for _, name := range names {
		addName(name)
	}

	queue := append([]string{}, result...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		dir, err := src.Dir(name)
		if err != nil {
			return nil, fmt.Errorf("maildir: open mailbox %s: %w", name, err)
		}
		children, err := dir.Children()
		if err != nil {
			if IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("maildir: list child mailboxes %s: %w", name, err)
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			childName := child.Name()
			if _, ok := seen[childName]; ok {
				continue
			}
			addName(childName)
			queue = append(queue, childName)
		}
	}

	return result, nil
}

func migrateAttributes(src, dst Storage) error {
	srcStore, err := src.Attributes()
	if err != nil {
		return fmt.Errorf("maildir: read attributes: %w", err)
	}
	attrs, err := srcStore.Read()
	if err != nil {
		return fmt.Errorf("maildir: read attributes: %w", err)
	}
	if len(attrs) == 0 {
		return nil
	}
	dstStore, err := dst.Attributes()
	if err != nil {
		return fmt.Errorf("maildir: write attributes: %w", err)
	}
	if err := dstStore.Write(attrs); err != nil {
		return fmt.Errorf("maildir: write attributes: %w", err)
	}
	return nil
}

func migrateMailbox(src, dst Storage, name string) error {
	srcDir, err := src.Dir(name)
	if err != nil {
		return fmt.Errorf("maildir: open source mailbox %s: %w", name, err)
	}
	exists, err := srcDir.Exists()
	if err != nil {
		return fmt.Errorf("maildir: check source mailbox %s: %w", name, err)
	}
	if !exists {
		return nil
	}

	dstDir, err := dst.Dir(name)
	if err != nil {
		return fmt.Errorf("maildir: open destination mailbox %s: %w", name, err)
	}
	if err := dstDir.Init(); err != nil {
		return fmt.Errorf("maildir: init destination mailbox %s: %w", name, err)
	}
	if err := ensureEmptyMailbox(dstDir, name); err != nil {
		return err
	}

	srcMeta := &Metadata{}
	if err := srcDir.LoadMetadata(srcMeta); err != nil {
		return fmt.Errorf("maildir: read metadata %s: %w", name, err)
	}

	dstMeta := &Metadata{}
	dstMeta.Ensure()
	copyKeywords(dstMeta, srcMeta)
	dstMeta.UIDValidity = srcMeta.UIDValidity
	dstMeta.UIDNext = srcMeta.UIDNext
	dstMeta.GUID = srcMeta.GUID
	dstMeta.SetUIDListName(srcMeta.UIDListName())
	dstMeta.SetKeywordsName(srcMeta.KeywordsName())

	maxUID := uint32(0)
	for _, uid := range srcMeta.UIDByKey {
		if uid > maxUID {
			maxUID = uid
		}
	}
	if dstMeta.UIDNext == 0 {
		dstMeta.UIDNext = maxUID + 1
		if dstMeta.UIDNext == 0 {
			dstMeta.UIDNext = 1
		}
	}

	msgs, err := srcDir.Messages()
	if err != nil {
		if !IsNotExist(err) {
			return fmt.Errorf("maildir: list messages %s: %w", name, err)
		}
		msgs = nil
	}
	unseen, err := srcDir.Unseen()
	if err != nil {
		if !IsNotExist(err) {
			return fmt.Errorf("maildir: list unseen %s: %w", name, err)
		}
		unseen = nil
	}

	allMessages := make([]Message, 0, len(msgs)+len(unseen))
	seen := map[string]struct{}{}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		key := msg.Key()
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		allMessages = append(allMessages, msg)
	}
	for _, msg := range unseen {
		if msg == nil {
			continue
		}
		key := msg.Key()
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		allMessages = append(allMessages, msg)
	}

	if err := migrateMessages(dstDir, allMessages, srcMeta, dstMeta, &maxUID); err != nil {
		return fmt.Errorf("maildir: migrate message %s: %w", name, err)
	}

	if dstMeta.UIDNext <= maxUID {
		dstMeta.UIDNext = maxUID + 1
	}
	if dstMeta.UIDValidity == 0 {
		dstMeta.UIDValidity = uint32(time.Now().UnixNano())
		if dstMeta.UIDValidity == 0 {
			dstMeta.UIDValidity = 1
		}
	}
	if dstMeta.GUID == "" {
		dstMeta.EnsureGUID()
	}
	if len(dstMeta.KeywordByName) > 0 {
		dstMeta.DirtyKeywords = true
	}
	dstMeta.DirtyUIDList = true
	dstMeta.Loaded = true

	if err := dstDir.WriteMetadata(dstMeta); err != nil {
		return fmt.Errorf("maildir: write metadata %s: %w", name, err)
	}

	return nil
}

func ensureEmptyMailbox(dir Dir, name string) error {
	msgs, err := dir.Messages()
	if err != nil && !IsNotExist(err) {
		return fmt.Errorf("maildir: check destination mailbox %s: %w", name, err)
	}
	if len(msgs) > 0 {
		return fmt.Errorf("maildir: destination mailbox %s is not empty", name)
	}
	return nil
}

func migrateMessages(dst Dir, messages []Message, srcMeta, dstMeta *Metadata, maxUID *uint32) error {
	if len(messages) == 0 {
		return nil
	}
	workers := min(max(runtime.GOMAXPROCS(0), 1), len(messages))

	workCh := make(chan Message)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	done := make(chan struct{})
	setErr := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			close(done)
		})
	}
	var metaLock sync.Mutex

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case msg, ok := <-workCh:
				if !ok {
					return
				}
				dstMsg, err := copyMessage(dst, msg)
				if err != nil {
					setErr(err)
					continue
				}
				metaLock.Lock()
				updateMessageMetadata(dstMsg, msg, srcMeta, dstMeta, maxUID)
				metaLock.Unlock()
			}
		}
	}

	for range workers {
		wg.Add(1)
		go worker()
	}
messageLoop:
	for _, msg := range messages {
		select {
		case <-done:
			break messageLoop
		case workCh <- msg:
		}
	}
	close(workCh)
	wg.Wait()
	return firstErr
}

func copyMessage(dst Dir, srcMsg Message) (Message, error) {
	flags := srcMsg.Flags()
	dstMsg, writer, err := dst.Create(flags)
	if err != nil {
		return nil, err
	}
	srcReader, err := srcMsg.Open()
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	_, copyErr := io.Copy(writer, srcReader)
	closeErr := srcReader.Close()
	writeErr := writer.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if writeErr != nil {
		return nil, writeErr
	}
	if info, err := srcMsg.Stat(); err == nil {
		_ = dstMsg.Chtimes(info.ModTime(), info.ModTime())
	}
	return dstMsg, nil
}

func updateMessageMetadata(dstMsg, srcMsg Message, srcMeta, dstMeta *Metadata, maxUID *uint32) {
	srcUID := dstMeta.UIDNext
	if uid, ok := srcMeta.UIDByKey[srcMsg.Key()]; ok && uid != 0 {
		srcUID = uid
	} else if uid, ok := srcMeta.UIDByKey[srcMsg.Name()]; ok && uid != 0 {
		srcUID = uid
	}
	if uid := srcUID; uid == dstMeta.UIDNext {
		if uid <= *maxUID {
			uid = *maxUID + 1
		}
		*maxUID = uid
		srcUID = uid
	}
	if srcUID > *maxUID {
		*maxUID = srcUID
	}
	dstMeta.UIDByKey[dstMsg.Key()] = srcUID
	dstMeta.FilenameByUID[srcUID] = dstMsg.Name()
}

func copyKeywords(dst, src *Metadata) {
	if src == nil || dst == nil {
		return
	}
	for key, value := range src.KeywordByName {
		dst.KeywordByName[key] = value
	}
	for key, value := range src.NameByKeyword {
		dst.NameByKeyword[key] = value
	}
}
