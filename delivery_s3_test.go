package imapmaildir

import (
	"testing"

	"github.com/foxcpp/go-imap-maildir/maildir/s3"
	"github.com/foxcpp/go-imap-maildir/maildir/s3/testing"
)

func TestBackendDeliveryS3(t *testing.T) {
	runDeliveryTests(t, newTestBackendS3)
}

func newTestBackendS3(t *testing.T) (*Backend, func()) {
	client, bucket, rootPrefix, cleanup := s3testing.StartMinio(t)
	provider := s3.NewProvider(client, bucket, rootPrefix)
	backend, err := New("users/{username}", provider, nil)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	return backend, cleanup
}
