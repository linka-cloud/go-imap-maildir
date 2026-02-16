package imapmaildir

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-message/textproto"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

type backendFactory func(t *testing.T) (*Backend, func())

func runDeliveryTests(t *testing.T, factory backendFactory) {
	t.Run("commit", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.BodyRaw(bytes.NewReader([]byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err != nil {
			t.Fatal(err)
		}

		msgs := mustUnseen(t, backend, "alice", InboxName)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message in new, got %d", len(msgs))
		}
		if readMessage(t, msgs[0]) != "hello" {
			t.Fatalf("expected delivered message content")
		}
	})

	t.Run("abort", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.BodyRaw(bytes.NewReader([]byte("discard"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Abort(); err != nil {
			t.Fatal(err)
		}

		msgs := mustUnseen(t, backend, "alice", InboxName)
		if len(msgs) != 0 {
			t.Fatalf("expected 0 messages in new, got %d", len(msgs))
		}
	})

	t.Run("missing recipients", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		delivery := backend.NewDelivery()
		if err := delivery.BodyRaw(bytes.NewReader([]byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err == nil {
			t.Fatalf("expected error for missing recipients")
		}
	})

	t.Run("missing body", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err == nil {
			t.Fatalf("expected error for missing body")
		}
	})

	t.Run("multiple recipients", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		for _, name := range []string{"alice", "bob"} {
			if err := backend.CreateUser(name); err != nil {
				t.Fatal(err)
			}
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.AddRcpt("bob", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.BodyRaw(bytes.NewReader([]byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err != nil {
			t.Fatal(err)
		}

		if len(mustUnseen(t, backend, "alice", InboxName)) != 1 {
			t.Fatalf("expected alice delivery")
		}
		if len(mustUnseen(t, backend, "bob", InboxName)) != 1 {
			t.Fatalf("expected bob delivery")
		}
	})

	t.Run("mailbox override", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Mailbox("Archive"); err != nil {
			t.Fatal(err)
		}
		if err := delivery.BodyRaw(bytes.NewReader([]byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err != nil {
			t.Fatal(err)
		}

		if len(mustUnseen(t, backend, "alice", InboxName)) != 0 {
			t.Fatalf("expected inbox empty")
		}
		if len(mustUnseen(t, backend, "alice", "Archive")) != 1 {
			t.Fatalf("expected archive delivery")
		}
	})

	t.Run("special mailbox", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		if err := delivery.SpecialMailbox(imap.TrashAttr, "Trash"); err != nil {
			t.Fatal(err)
		}
		if err := delivery.BodyRaw(bytes.NewReader([]byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err != nil {
			t.Fatal(err)
		}

		if len(mustUnseen(t, backend, "alice", InboxName)) != 0 {
			t.Fatalf("expected inbox empty")
		}
		if len(mustUnseen(t, backend, "alice", "Trash")) != 1 {
			t.Fatalf("expected trash delivery")
		}
	})

	t.Run("body parsed", func(t *testing.T) {
		backend, cleanup := factory(t)
		defer cleanup()

		if err := backend.CreateUser("alice"); err != nil {
			t.Fatal(err)
		}

		delivery := backend.NewDelivery()
		if err := delivery.AddRcpt("alice", textproto.Header{}); err != nil {
			t.Fatal(err)
		}
		header := textproto.Header{}
		header.Set("Subject", "Test")
		body := testBuffer{r: bytes.NewReader([]byte("payload"))}
		if err := delivery.BodyParsed(header, 7, body); err != nil {
			t.Fatal(err)
		}
		if err := delivery.Commit(); err != nil {
			t.Fatal(err)
		}

		msgs := mustUnseen(t, backend, "alice", InboxName)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message in new, got %d", len(msgs))
		}
		content := readMessage(t, msgs[0])
		if !strings.Contains(content, "Subject: Test") || !strings.Contains(content, "payload") {
			t.Fatalf("expected header and body in delivered message")
		}
	})
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

func mustUnseen(t *testing.T, backend *Backend, username, mailbox string) []maildir.Message {
	user, err := backend.GetUser(username)
	if err != nil {
		t.Fatal(err)
	}
	u := user.(*User)

	mbox, err := u.storage.Dir(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mbox.Unseen()
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

type testBuffer struct {
	r *bytes.Reader
}

func (t testBuffer) Open() (io.ReadCloser, error) {
	return io.NopCloser(t.r), nil
}
