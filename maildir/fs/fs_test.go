package fs

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

func TestFSDelivery(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	dir := mustInitDir(t, storage, "INBOX")
	delivery, err := dir.NewDelivery()
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

	msgs, err := dir.Unseen()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in new, got %d", len(msgs))
	}
	if readMessage(t, msgs[0]) != "hello" {
		t.Fatalf("expected delivered message content")
	}
}

func TestFSDeliveryAbort(t *testing.T) {
	storage, cleanup := newTestStorage(t)
	defer cleanup()

	dir := mustInitDir(t, storage, "INBOX")
	delivery, err := dir.NewDelivery()
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

	msgs, err := dir.Unseen()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages in new, got %d", len(msgs))
	}
}

func newTestStorage(t *testing.T) (maildir.Storage, func()) {
	root, err := os.MkdirTemp("", "go-imap-maildir-fs-")
	if err != nil {
		t.Fatal(err)
	}
	storage := Provider{}.Storage(root)
	cleanup := func() {
		_ = os.RemoveAll(root)
	}
	return storage, cleanup
}

func mustInitDir(t *testing.T, storage maildir.Storage, name string) maildir.Dir {
	dir := mustDir(t, storage, name)
	if err := dir.Init(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustDir(t *testing.T, storage maildir.Storage, name string) maildir.Dir {
	dir, err := storage.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
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
