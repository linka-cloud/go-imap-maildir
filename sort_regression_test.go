package imapmaildir

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	sortthread "github.com/emersion/go-imap-sortthread"
)

func TestSortWithUnindexedHeaderCriteria(t *testing.T) {
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

	baseDate := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	rawA := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: Zebra\r\n" +
		"Reply-To: reply@example.com\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:05 +0000\r\n" +
		"\r\n" +
		"body\r\n"
	rawB := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: Alpha\r\n" +
		"Reply-To: reply@example.com\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:06 +0000\r\n" +
		"\r\n" +
		"body\r\n"
	rawC := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: Ignored\r\n" +
		"Reply-To: other@example.com\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:07 +0000\r\n" +
		"\r\n" +
		"body\r\n"

	if err := u.CreateMessage(InboxName, nil, baseDate, bytes.NewReader([]byte(rawA)), nil); err != nil {
		t.Fatal(err)
	}
	if err := u.CreateMessage(InboxName, nil, baseDate.Add(time.Second), bytes.NewReader([]byte(rawB)), nil); err != nil {
		t.Fatal(err)
	}
	if err := u.CreateMessage(InboxName, nil, baseDate.Add(2*time.Second), bytes.NewReader([]byte(rawC)), nil); err != nil {
		t.Fatal(err)
	}

	_, mbox, err := u.GetMailbox(InboxName, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := mbox.(*SelectedMailbox)

	searchCrit := imap.NewSearchCriteria()
	searchCrit.Header.Add("Reply-To", "reply@example.com")

	ids, err := sel.Sort(true, []sortthread.SortCriterion{{Field: sortthread.SortSubject}}, searchCrit)
	if err != nil {
		t.Fatal(err)
	}

	expected := []uint32{2, 1}
	if !reflect.DeepEqual(ids, expected) {
		t.Fatalf("unexpected sorted ids: got %v want %v", ids, expected)
	}
}

func TestSortSubjectReverse(t *testing.T) {
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

	baseDate := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	rawA := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: bravo\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:05 +0000\r\n" +
		"\r\n" +
		"body\r\n"
	rawB := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: alpha\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:06 +0000\r\n" +
		"\r\n" +
		"body\r\n"
	rawC := "From: sender@example.com\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: charlie\r\n" +
		"Date: Fri, 02 Jan 2026 03:04:07 +0000\r\n" +
		"\r\n" +
		"body\r\n"

	if err := u.CreateMessage(InboxName, nil, baseDate, bytes.NewReader([]byte(rawA)), nil); err != nil {
		t.Fatal(err)
	}
	if err := u.CreateMessage(InboxName, nil, baseDate.Add(time.Second), bytes.NewReader([]byte(rawB)), nil); err != nil {
		t.Fatal(err)
	}
	if err := u.CreateMessage(InboxName, nil, baseDate.Add(2*time.Second), bytes.NewReader([]byte(rawC)), nil); err != nil {
		t.Fatal(err)
	}

	_, mbox, err := u.GetMailbox(InboxName, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := mbox.(*SelectedMailbox)

	searchCrit := imap.NewSearchCriteria()

	asc, err := sel.Sort(true, []sortthread.SortCriterion{{Field: sortthread.SortSubject}}, searchCrit)
	if err != nil {
		t.Fatal(err)
	}

	ascExpected := []uint32{2, 1, 3}
	if !reflect.DeepEqual(asc, ascExpected) {
		t.Fatalf("unexpected ascending sort ids: got %v want %v", asc, ascExpected)
	}

	desc, err := sel.Sort(true, []sortthread.SortCriterion{{Field: sortthread.SortSubject, Reverse: true}}, searchCrit)
	if err != nil {
		t.Fatal(err)
	}

	descExpected := []uint32{3, 1, 2}
	if !reflect.DeepEqual(desc, descExpected) {
		t.Fatalf("unexpected descending sort ids: got %v want %v", desc, descExpected)
	}
}
