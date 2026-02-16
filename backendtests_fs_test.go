package imapmaildir

import (
	"log"
	"os"
	"strings"
	"testing"

	backendtests "github.com/foxcpp/go-imap-backend-tests"

	"github.com/foxcpp/go-imap-maildir/maildir/fs"
)

func initTestBackend() backendtests.Backend {
	tempDir, err := os.MkdirTemp("", "go-imap-maildir-")
	if err != nil {
		panic(err)
	}

	be, err := New(tempDir+"/{username}", fs.Provider{}, nil)
	if err != nil {
		panic(err)
	}

	if testing.Verbose() {
		be.Log = log.New(os.Stderr, "imapmaildir: ", log.LstdFlags)
		be.Debug = log.New(os.Stderr, "imapmaildir[debug]: ", log.LstdFlags)
	}

	return be
}

func cleanBackend(b backendtests.Backend) {
	be := b.(*Backend)

	if os.Getenv("KEEP_IMAPMAILDIR") == "1" {
		log.Println("Maildirs in", be.PathTemplate, "are not deleted")
		return
	}
	if err := os.RemoveAll(strings.TrimSuffix(be.PathTemplate, "/{username}")); err != nil {
		panic(err)
	}
}

func TestBackend(t *testing.T) {
	backendtests.RunTests(t, initTestBackend, cleanBackend)
}
