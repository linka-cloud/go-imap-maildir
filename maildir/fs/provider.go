package fs

import "github.com/foxcpp/go-imap-maildir/maildir"

type Provider struct {
	RootDir string
}

func (c Provider) Storage(basePath string) maildir.Storage {
	if c.RootDir == "" {
		return &Storage{basePath: basePath}
	}
	return &Storage{basePath: c.RootDir + "/" + basePath}
}
