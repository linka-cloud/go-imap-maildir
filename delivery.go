package imapmaildir

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-message/textproto"
)

type DeliveryTarget interface {
	NewDelivery() Delivery
}

type Delivery interface {
	AddRcpt(username string, userHeader textproto.Header) error
	Mailbox(name string) error
	SpecialMailbox(attribute, fallbackName string) error
	UserMailbox(username, mailbox string, flags []string)
	BodyRaw(message io.Reader) error
	BodyParsed(header textproto.Header, bodyLen int, body Buffer) error
	Abort() error
	Commit() error
}

type Buffer interface {
	Open() (io.ReadCloser, error)
}

type delivery struct {
	b *Backend

	recipients     map[string]*deliveryRecipient
	defaultMailbox string
	spoolPath      string
}

type deliveryRecipient struct {
	username string
	mailbox  string
	flags    []string
}

func (b *Backend) NewDelivery() Delivery {
	return &delivery{
		b:              b,
		recipients:     map[string]*deliveryRecipient{},
		defaultMailbox: InboxName,
	}
}

func (d *delivery) AddRcpt(username string, _ textproto.Header) error {
	if username == "" {
		return errors.New("maildir: empty recipient")
	}
	if _, ok := d.recipients[username]; !ok {
		d.recipients[username] = &deliveryRecipient{username: username}
	}
	return nil
}

func (d *delivery) Mailbox(name string) error {
	if name == "" {
		name = InboxName
	}
	d.defaultMailbox = name
	return nil
}

func (d *delivery) SpecialMailbox(attribute, fallbackName string) error {
	if attribute != "" {
		specs := d.b.DefaultMailboxes
		if len(specs) == 0 {
			specs = defaultMailboxSpecs
		}
		for _, spec := range specs {
			if strings.EqualFold(spec.SpecialUse, attribute) {
				return d.Mailbox(spec.Name)
			}
		}
	}
	return d.Mailbox(fallbackName)
}

func (d *delivery) UserMailbox(username, mailbox string, flags []string) {
	if username == "" {
		return
	}
	rcpt, ok := d.recipients[username]
	if !ok {
		rcpt = &deliveryRecipient{username: username}
		d.recipients[username] = rcpt
	}
	rcpt.mailbox = mailbox
	rcpt.flags = append([]string{}, flags...)
}

func (d *delivery) BodyRaw(message io.Reader) error {
	if message == nil {
		return errors.New("maildir: missing message body")
	}
	return d.writeSpool(func(w io.Writer) error {
		_, err := io.Copy(w, message)
		return err
	})
}

func (d *delivery) BodyParsed(header textproto.Header, _ int, body Buffer) error {
	reader, err := body.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	return d.writeSpool(func(w io.Writer) error {
		if err := textproto.WriteHeader(w, header); err != nil {
			return err
		}
		_, err := io.Copy(w, reader)
		return err
	})
}

func (d *delivery) Abort() error {
	d.recipients = nil
	d.cleanupSpool()
	return nil
}

func (d *delivery) Commit() error {
	if d.recipients == nil {
		return errors.New("maildir: delivery not initialized")
	}
	if len(d.recipients) == 0 {
		return errors.New("maildir: missing recipients")
	}
	if d.spoolPath == "" {
		return errors.New("maildir: missing message body")
	}
	defer d.cleanupSpool()

	defaultMailbox := d.defaultMailbox
	if defaultMailbox == "" {
		defaultMailbox = InboxName
	}

	recipients := make([]*deliveryRecipient, 0, len(d.recipients))
	for _, rcpt := range d.recipients {
		recipients = append(recipients, rcpt)
	}
	if len(recipients) == 0 {
		return nil
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(recipients) {
		workers = len(recipients)
	}

	workCh := make(chan *deliveryRecipient)
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
			case rcpt, ok := <-workCh:
				if !ok {
					return
				}
				mailbox := rcpt.mailbox
				if mailbox == "" {
					mailbox = defaultMailbox
				}
				reader, err := os.Open(d.spoolPath)
				if err != nil {
					setErr(err)
					continue
				}
				err = d.b.deliverToRecipient(rcpt.username, mailbox, rcpt.flags, reader)
				closeErr := reader.Close()
				if err == nil {
					err = closeErr
				}
				if err != nil {
					setErr(err)
				}
			}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
recipientLoop:
	for _, rcpt := range recipients {
		select {
		case <-done:
			break recipientLoop
		case workCh <- rcpt:
		}
	}
	close(workCh)
	wg.Wait()
	return firstErr
}

func (d *delivery) writeSpool(fn func(io.Writer) error) error {
	d.cleanupSpool()
	file, err := os.CreateTemp("", "maildir-delivery-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := fn(file); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	d.spoolPath = name
	return nil
}

func (d *delivery) cleanupSpool() {
	if d.spoolPath == "" {
		return
	}
	_ = os.Remove(d.spoolPath)
	d.spoolPath = ""
}

func (b *Backend) deliverToRecipient(username, mailbox string, flags []string, reader io.Reader) error {
	user, err := b.GetUser(username)
	if err != nil {
		return err
	}
	u, ok := user.(*User)
	if !ok {
		return errors.New("maildir: unsupported user implementation")
	}

	mboxDir, err := u.storage.Dir(mailbox)
	if err != nil {
		return err
	}
	exists, err := mboxDir.Exists()
	if err != nil {
		return err
	}
	if !exists {
		if strings.EqualFold(mailbox, InboxName) {
			if err := mboxDir.Init(); err != nil {
				return err
			}
		} else {
			return backend.ErrNoSuchMailbox
		}
	}

	delivery, err := mboxDir.NewDelivery()
	if err != nil {
		return err
	}
	if _, err := io.Copy(delivery, reader); err != nil {
		_ = delivery.Abort()
		return err
	}
	if err := delivery.Close(); err != nil {
		return err
	}

	u.mailboxesLock.Lock()
	mbox := u.ensureMailbox(mailbox)
	u.mailboxesLock.Unlock()
	if mbox == nil {
		return errors.New("maildir: failed to load mailbox state")
	}
	_, err = mbox.listEntries()
	return err
}
