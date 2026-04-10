package imapmaildir

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foxcpp/go-imap-maildir/maildir/fs"
)

func newBackendForLifecycleTest(t *testing.T) *Backend {
	t.Helper()

	b, err := New(filepath.Join(t.TempDir(), "{username}"), fs.Provider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackendDropsUserStateOnLastLogout(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	if err := b.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	u1, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}

	b.getMailboxState("alice", InboxName)

	if err := u1.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.states[b.mailboxKey("alice", InboxName)]; !ok {
		t.Fatal("mailbox state was removed while one session was still active")
	}

	if err := u2.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.states[b.mailboxKey("alice", InboxName)]; ok {
		t.Fatal("mailbox state was not removed after last logout")
	}
}

func TestUserLogoutIsIdempotent(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	if err := b.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	u, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := u.Logout(); err != nil {
		t.Fatal(err)
	}

	if cnt := b.users["alice"]; cnt != 0 {
		t.Fatalf("unexpected user refcount after double logout: %d", cnt)
	}
}

func TestBackendCloseClearsCachedState(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	b.getMailboxState("alice", InboxName)
	b.getMailboxState("bob", InboxName)
	b.users["alice"] = 1
	b.users["bob"] = 2

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if len(b.states) != 0 {
		t.Fatalf("backend states not cleared, got %d entries", len(b.states))
	}
	if len(b.users) != 0 {
		t.Fatalf("backend user refs not cleared, got %d entries", len(b.users))
	}
}

func TestDeliverToRecipientDoesNotLeakUserRefs(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	if err := b.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	msg := strings.NewReader("Subject: test\r\n\r\nbody")
	if err := b.deliverToRecipient("alice", InboxName, nil, msg); err != nil {
		t.Fatal(err)
	}

	if cnt := b.users["alice"]; cnt != 0 {
		t.Fatalf("unexpected user refcount after delivery: %d", cnt)
	}
}

func TestCreateMessageLoadsExistingMailboxInNewSession(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	if err := b.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	u1raw, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u1 := u1raw.(*User)
	const box = "Sent"
	if err := u1.Logout(); err != nil {
		t.Fatal(err)
	}

	u2raw, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u2 := u2raw.(*User)
	defer u2.Logout()

	raw := []byte("Subject: test\r\n\r\nbody")
	if err := u2.CreateMessage(box, nil, time.Now(), bytes.NewReader(raw), nil); err != nil {
		t.Fatalf("append to existing mailbox failed: %v", err)
	}
}
