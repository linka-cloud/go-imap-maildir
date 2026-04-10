package metadata

import (
	"bytes"
	"strings"
	"testing"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

func TestWriteUIDListStripsAbsolutePaths(t *testing.T) {
	state := &maildir.Metadata{}
	state.Ensure()
	state.FilenameByUID[1] = "/tmp/maildir/cur/123:2,S"

	buf := &bytes.Buffer{}
	if err := WriteUIDList(buf, state); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, "/tmp/maildir/") {
		t.Fatalf("uidlist contains absolute path: %q", out)
	}
	if !strings.Contains(out, "\n1 :123:2,S\n") {
		t.Fatalf("uidlist does not contain stripped filename: %q", out)
	}
}
