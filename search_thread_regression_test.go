package imapmaildir

import (
	"bytes"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	sortthread "github.com/emersion/go-imap-sortthread"
)

func TestSearchAndThreadWithUnindexedHeader(t *testing.T) {
	backend, cleanup := newTestBackend(t)
	defer cleanup()

	if err := backend.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	usr, err := backend.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u := usr.(*User)

	if _, _, err := u.GetMailbox(InboxName, false, nil); err != nil {
		t.Fatal(err)
	}

	raw := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: Topic\r\n" +
		"Reply-To: reply@example.com\r\n" +
		"\r\n" +
		"hello\r\n"
	if err := u.CreateMessage(InboxName, nil, time.Now(), bytes.NewReader([]byte(raw)), nil); err != nil {
		t.Fatal(err)
	}

	_, mbox, err := u.GetMailbox(InboxName, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := mbox.(*SelectedMailbox)

	crit := imap.NewSearchCriteria()
	crit.Header.Add("Reply-To", "reply@example.com")

	ids, err := sel.SearchMessages(true, crit)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected one search hit for unindexed header, got %d", len(ids))
	}

	threads, err := sel.Thread(true, sortthread.OrderedSubject, crit)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("expected one thread hit for unindexed header, got %d", len(threads))
	}
}

func TestThreadEmptyResultDoesNotError(t *testing.T) {
	backend, cleanup := newTestBackend(t)
	defer cleanup()

	if err := backend.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}

	usr, err := backend.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	u := usr.(*User)

	if _, _, err := u.GetMailbox(InboxName, false, nil); err != nil {
		t.Fatal(err)
	}

	raw := "Subject: Topic\r\n\r\nhello\r\n"
	if err := u.CreateMessage(InboxName, nil, time.Now(), bytes.NewReader([]byte(raw)), nil); err != nil {
		t.Fatal(err)
	}

	_, mbox, err := u.GetMailbox(InboxName, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := mbox.(*SelectedMailbox)

	crit := imap.NewSearchCriteria()
	crit.Header.Add("Subject", "missing")

	threads, err := sel.Thread(true, sortthread.OrderedSubject, crit)
	if err != nil {
		t.Fatalf("expected empty thread result, got error: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("expected zero threads for non-matching criteria, got %d", len(threads))
	}
}
