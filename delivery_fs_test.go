package imapmaildir

import (
	"os"
	"testing"

	"github.com/foxcpp/go-imap-maildir/maildir/fs"
)

func TestBackendDeliveryFS(t *testing.T) {
	runDeliveryTests(t, newTestBackend)
}

func newTestBackend(t *testing.T) (*Backend, func()) {
	root, err := os.MkdirTemp("", "go-imap-maildir-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New(root+"/{username}", fs.Provider{}, nil)
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	cleanup := func() {
		_ = os.RemoveAll(root)
	}
	return backend, cleanup
}
