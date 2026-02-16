package imapmaildir

import (
	"github.com/foxcpp/go-imap-maildir/maildir"
)

func (m *Mailbox) loadMetadataState() error {
	if m.state.meta == nil {
		m.state.meta = &maildir.Metadata{}
	}
	return m.dir.LoadMetadata(m.state.meta)
}

func (m *Mailbox) writeMetadataState(state *maildir.Metadata) error {
	return m.dir.WriteMetadata(state)
}
