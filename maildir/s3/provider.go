package s3

import (
	"github.com/dgraph-io/ristretto/v2"
	"github.com/minio/minio-go/v7"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

func NewProvider(client *minio.Client, bucket, rootPrefix string) maildir.Provider {
	cache := newCache(defaultCacheBytes)
	return &Provider{
		Client:      client,
		Bucket:      bucket,
		RootPrefix:  rootPrefix,
		Cache:       cache,
		MaxItemSize: defaultCacheItemSize,
	}
}

func NewProviderWithCache(client *minio.Client, bucket, prefix string, cache *ristretto.Cache[string, []byte], maxItemSize int64) maildir.Provider {
	if maxItemSize < 0 {
		maxItemSize = 0
	}
	return &Provider{
		Client:      client,
		Bucket:      bucket,
		RootPrefix:  prefix,
		Cache:       cache,
		MaxItemSize: maxItemSize,
	}
}

type Provider struct {
	Client     *minio.Client
	Bucket     string
	RootPrefix string

	Cache       *ristretto.Cache[string, []byte]
	MaxItemSize int64
}

func (c *Provider) Storage(basePath string) maildir.Storage {
	return &Storage{
		basePath:         basePath,
		client:           c.Client,
		bucket:           c.Bucket,
		rootPrefix:       c.RootPrefix,
		cache:            c.Cache,
		maxCacheItemSize: c.MaxItemSize,
	}
}
