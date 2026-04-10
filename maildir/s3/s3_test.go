package s3

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foxcpp/go-imap-maildir/maildir"
	"github.com/foxcpp/go-imap-maildir/maildir/s3/testing"
)

func TestS3ListDirs(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	inbox, err := storage.Dir("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Init(); err != nil {
		t.Fatal(err)
	}

	projects, err := storage.Dir("Projects")
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.Init(); err != nil {
		t.Fatal(err)
	}

	list, err := storage.ListDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "Projects" {
		t.Fatalf("expected only Projects, got %v", list)
	}
}

func TestS3CopyMove(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	inbox := mustInitDir(t, storage, "INBOX")
	archive := mustInitDir(t, storage, "Archive")

	msg := createMessage(t, inbox, []byte("hello"), nil)

	copied, err := msg.CopyTo(archive)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Key() == msg.Key() {
		t.Fatalf("expected copy to generate new key")
	}
	if readMessage(t, copied) != "hello" {
		t.Fatalf("expected copied message content")
	}

	if err := msg.MoveTo(archive); err != nil {
		t.Fatal(err)
	}
	msgs, err := archive.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages after copy+move, got %d", len(msgs))
	}
}

func TestS3RenameNoCopy(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	oldDir := mustInitDir(t, storage, "Old")
	createMessage(t, oldDir, []byte("payload"), nil)

	if err := oldDir.Rename("New"); err != nil {
		t.Fatal(err)
	}

	oldDir = mustDir(t, storage, "Old")
	exists, err := oldDir.Exists()
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("expected Old to be invalid after rename")
	}

	newDir := mustDir(t, storage, "New")
	msgs, err := newDir.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in New, got %d", len(msgs))
	}
}

func TestS3Delivery(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	inbox := mustInitDir(t, storage, "INBOX")
	delivery, err := inbox.NewDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.Write([]byte("hello")); err != nil {
		_ = delivery.Abort()
		t.Fatal(err)
	}
	if err := delivery.Close(); err != nil {
		t.Fatal(err)
	}

	msgs, err := inbox.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if readMessage(t, msgs[0]) != "hello" {
		t.Fatalf("expected delivered message content")
	}
}

func TestS3DeliveryAbort(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	inbox := mustInitDir(t, storage, "INBOX")
	delivery, err := inbox.NewDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.Write([]byte("discard")); err != nil {
		_ = delivery.Abort()
		t.Fatal(err)
	}
	if err := delivery.Abort(); err != nil {
		t.Fatal(err)
	}

	msgs, err := inbox.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

func TestS3IndexLockMutualExclusion(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	inbox := mustInitDir(t, storage, "INBOX")
	dir := inbox.(*Dir)

	store, err := dir.IndexStore("guid-lock-test")
	if err != nil {
		t.Fatal(err)
	}
	idxStore := store.(*indexStore)

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	active := 0
	maxActive := 0
	var activeMu sync.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := idxStore.withLock(func() error {
				activeMu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				activeMu.Unlock()

				time.Sleep(20 * time.Millisecond)

				activeMu.Lock()
				active--
				activeMu.Unlock()
				return nil
			})
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if maxActive != 1 {
		t.Fatalf("expected lock to serialize access, max concurrent owners=%d", maxActive)
	}
}

func TestS3IndexLockAcrossStorageInstances(t *testing.T) {
	client, bucket, rootPrefix, cleanup := s3testing.StartMinioShared(t)
	defer cleanup()

	basePath := "lock-cross-" + time.Now().UTC().Format("20060102150405.000000000")
	storageA := NewProvider(client, bucket, rootPrefix).Storage(basePath).(*Storage)
	storageB := NewProvider(client, bucket, rootPrefix).Storage(basePath).(*Storage)

	dirA := mustInitDir(t, storageA, "INBOX")
	dirB := mustDir(t, storageB, "INBOX")

	storeA, err := dirA.(*Dir).IndexStore("guid-cross-lock")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := dirB.(*Dir).IndexStore("guid-cross-lock")
	if err != nil {
		t.Fatal(err)
	}

	idxA := storeA.(*indexStore)
	idxB := storeB.(*indexStore)

	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	errA := make(chan error, 1)
	go func() {
		errA <- idxA.withLock(func() error {
			close(enteredA)
			<-releaseA
			return nil
		})
	}()

	select {
	case <-enteredA:
	case <-time.After(2 * time.Second):
		t.Fatal("first storage instance did not acquire lock")
	}

	_, _ = storageB.listKeys(idxB.lockKey())

	errB := make(chan error, 1)
	go func() {
		errB <- idxB.withLock(func() error {
			return nil
		})
	}()

	time.Sleep(100 * time.Millisecond)
	close(releaseA)

	select {
	case err := <-errA:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first storage instance did not release lock")
	}

	select {
	case err := <-errB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second storage instance failed to acquire lock after release")
	}
}

func newTestStorage(t *testing.T) (*Storage, func()) {
	client, bucket, rootPrefix, cleanup := s3testing.StartMinioShared(t)
	basePath := "test-" + time.Now().UTC().Format("20060102150405.000000000")
	storage := NewProvider(client, bucket, rootPrefix).Storage(basePath).(*Storage)
	return storage, cleanup
}

func mustInitDir(t *testing.T, storage *Storage, name string) maildir.Dir {
	dir := mustDir(t, storage, name)
	if err := dir.Init(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustDir(t *testing.T, storage *Storage, name string) maildir.Dir {
	dir, err := storage.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func createMessage(t *testing.T, dir maildir.Dir, body []byte, flags []maildir.Flag) maildir.Message {
	msg, writer, err := dir.Create(flags)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return msg
}

func readMessage(t *testing.T, msg maildir.Message) string {
	r, err := msg.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(buf.String())
}
