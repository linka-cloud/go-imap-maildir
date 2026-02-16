package imapmaildir

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message/textproto"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

func (m *SelectedMailbox) fetch(seqNum uint32, entry msgEntry, items []imap.FetchItem, recent bool) (*imap.Message, error) {
	result := imap.NewMessage(seqNum, items)

	var (
		bodyItems  []imap.FetchItem
		header     textproto.Header
		infoLoaded bool
		info       maildir.Info
	)

	for _, item := range items {
		switch item {
		case imap.FetchUid:
			result.Uid = entry.uid
		case imap.FetchFlags:
			result.Flags = m.entryFlags(entry, recent)
		case imap.FetchInternalDate:
			if !entry.meta.internalDate.IsZero() {
				result.InternalDate = entry.meta.internalDate
				continue
			}
			if !infoLoaded {
				var err error
				info, err = entry.msg.Stat()
				if err != nil {
					return nil, fmt.Errorf("fetch: stat: %w", err)
				}
				infoLoaded = true
			}
			result.InternalDate = info.ModTime()
		case imap.FetchRFC822Size:
			if !infoLoaded {
				var err error
				info, err = entry.msg.Stat()
				if err != nil {
					return nil, fmt.Errorf("fetch: stat: %w", err)
				}
				infoLoaded = true
			}
			result.Size = uint32(info.Size())
		case imap.FetchEnvelope, imap.FetchBodyStructure, imap.FetchBody:
			bodyItems = append(bodyItems, item)
		default:
			bodyItems = append(bodyItems, item)
		}
	}

	for _, item := range bodyItems {
		err := m.fetchBodyItem(result, &header, entry.msg, item)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (m *SelectedMailbox) fetchBodyItem(result *imap.Message, header *textproto.Header, msg maildir.Message, item imap.FetchItem) error {
	openBody := func() (*bufio.Reader, io.Closer, error) {
		f, err := msg.Open()
		if err != nil {
			return nil, nil, err
		}
		return bufio.NewReader(f), f, nil
	}
	ensureHeader := func(bufR *bufio.Reader) error {
		if header.Len() != 0 {
			return skipHeader(bufR)
		}
		hdr, err := textproto.ReadHeader(bufR)
		if err != nil {
			return err
		}
		*header = hdr
		return nil
	}

	switch item {
	case imap.FetchEnvelope:
		bufR, cl, err := openBody()
		if err != nil {
			return err
		}
		defer cl.Close()
		if err := ensureHeader(bufR); err != nil {
			return err
		}

		env, err := backendutil.FetchEnvelope(*header)
		if err != nil {
			return err
		}
		result.Envelope = env
	case imap.FetchBodyStructure, imap.FetchBody:
		bufR, cl, err := openBody()
		if err != nil {
			return err
		}
		defer cl.Close()
		if err := ensureHeader(bufR); err != nil {
			return err
		}

		bs, err := backendutil.FetchBodyStructure(*header, bufR, item == imap.FetchBodyStructure)
		if err != nil {
			return err
		}
		result.BodyStructure = bs
	default:
		sectName, err := imap.ParseBodySectionName(item)
		if err != nil {
			return err
		}

		bufR, cl, err := openBody()
		if err != nil {
			return err
		}
		defer cl.Close()
		if err := ensureHeader(bufR); err != nil {
			return err
		}

		literal, err := backendutil.FetchBodySection(*header, bufR, sectName)
		if err != nil {
			literal = bytes.NewReader(nil)
		}
		result.Body[sectName] = literal
	}

	return nil
}

func skipHeader(bufR *bufio.Reader) error {
	for {
		line, err := bufR.ReadSlice('\n')
		if err != nil {
			return err
		}
		if len(line) == 0 || (len(line) == 1 || line[0] == '\r') {
			break
		}
	}
	return nil
}
