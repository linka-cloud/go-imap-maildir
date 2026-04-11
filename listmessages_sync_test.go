package imapmaildir

import (
	"bytes"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
)

type waitConn struct {
	closed <-chan struct{}
}

func (c waitConn) SendUpdate(backend.Update) error {
	<-c.closed
	return nil
}

func TestListMessagesClosesChannelBeforeSync(t *testing.T) {
	b := newBackendForLifecycleTest(t)

	if err := b.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	uraw, err := b.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u := uraw.(*User)
	defer u.Logout()

	if err := u.CreateMessage(InboxName, nil, time.Now(), bytes.NewReader([]byte("Subject: test\r\n\r\nbody")), nil); err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	_, mraw, err := u.GetMailbox(InboxName, false, waitConn{closed: closed})
	if err != nil {
		t.Fatal(err)
	}
	m := mraw.(*SelectedMailbox)
	defer m.Close()

	ch := make(chan *imap.Message)
	go func() {
		for range ch {
		}
		close(closed)
	}()

	set := new(imap.SeqSet)
	set.AddNum(1)
	done := make(chan error, 1)
	go func() {
		done <- m.ListMessages(true, set, []imap.FetchItem{imap.FetchItem("BODY[]")}, ch)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListMessages deadlocked waiting for FETCH sync")
	}
}
