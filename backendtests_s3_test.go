package imapmaildir

import (
	"strings"
	"testing"

	backendtests "github.com/foxcpp/go-imap-backend-tests"

	"github.com/foxcpp/go-imap-maildir/maildir/s3"
	"github.com/foxcpp/go-imap-maildir/maildir/s3/testing"
)

func TestBackendS3(t *testing.T) {
	client, bucket, rootPrefix, cleanup := s3testing.StartMinio(t)
	defer cleanup()

	provider := s3.NewProvider(client, bucket, rootPrefix)

	init := func() backendtests.Backend {
		be, err := New("users/{username}", provider, defaultMailboxSpecs)
		if err != nil {
			panic(err)
		}
		return be
	}

	clean := func(b backendtests.Backend) {
		be := b.(*Backend)
		prefix := ""
		if be.PathTemplate != "" {
			prefix = joinKey(rootPrefix, "users")
		}
		s3testing.RemoveAllObjects(t, client, bucket, prefix)
	}

	backendtests.RunTests(t, init, clean)
}

func joinKey(parts ...string) string {
	var clean []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		clean = append(clean, strings.Trim(part, "/"))
	}
	if len(clean) == 0 {
		return ""
	}
	return strings.Join(clean, "/")
}
