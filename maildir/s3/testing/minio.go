package s3testing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func StartMinio(t *testing.T) (*minio.Client, string, string, func()) {
	t.Helper()
	bin, binCleanup := ensureMinioBinary(t)
	accessKey := "minio"
	secretKey := "minio123"
	dataDir := t.TempDir()
	addr := freeLocalAddr(t)
	consoleAddr := freeLocalAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "server", dataDir, "--address", addr, "--console-address", consoleAddr)
	cmd.Env = append(os.Environ(),
		"MINIO_ROOT_USER="+accessKey,
		"MINIO_ROOT_PASSWORD="+secretKey,
	)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		binCleanup()
		t.Fatalf("start minio: %v", err)
	}

	if err := waitForMinioReady(addr, 30*time.Second); err != nil {
		cancel()
		_ = cmd.Wait()
		binCleanup()
		t.Fatalf("minio not ready: %v", err)
	}

	client, err := minio.New(addr, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		cancel()
		_ = cmd.Wait()
		binCleanup()
		t.Fatalf("minio client: %v", err)
	}

	bucket := "test-bucket"
	if err := client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		exists, errExists := client.BucketExists(context.Background(), bucket)
		if errExists != nil || !exists {
			cancel()
			_ = cmd.Wait()
			binCleanup()
			t.Fatalf("make bucket: %v", err)
		}
	}

	rootPrefix := "root"
	cleanup := func() {
		RemoveAllObjects(t, client, bucket, rootPrefix)
		cancel()
		_ = cmd.Wait()
		binCleanup()
	}
	return client, bucket, rootPrefix, cleanup
}

var sharedMinio struct {
	mu         sync.Mutex
	started    bool
	refcount   int
	client     *minio.Client
	bucket     string
	dataDir    string
	cancel     context.CancelFunc
	cmd        *exec.Cmd
	binCleanup func()
}

var sharedMinioCounter uint64

func StartMinioShared(t *testing.T) (*minio.Client, string, string, func()) {
	t.Helper()
	sharedMinio.mu.Lock()
	if !sharedMinio.started {
		bin, binCleanup := ensureMinioBinary(t)
		accessKey := "minio"
		secretKey := "minio123"
		dataDir, err := os.MkdirTemp("", "go-imap-maildir-minio-")
		if err != nil {
			sharedMinio.mu.Unlock()
			t.Fatalf("create minio data dir: %v", err)
		}
		addr := freeLocalAddr(t)
		consoleAddr := freeLocalAddr(t)

		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, bin, "server", dataDir, "--address", addr, "--console-address", consoleAddr)
		cmd.Env = append(os.Environ(),
			"MINIO_ROOT_USER="+accessKey,
			"MINIO_ROOT_PASSWORD="+secretKey,
		)
		cmd.Stdout = &bytes.Buffer{}
		cmd.Stderr = &bytes.Buffer{}
		if err := cmd.Start(); err != nil {
			sharedMinio.mu.Unlock()
			binCleanup()
			_ = os.RemoveAll(dataDir)
			t.Fatalf("start minio: %v", err)
		}

		if err := waitForMinioReady(addr, 30*time.Second); err != nil {
			cancel()
			_ = cmd.Wait()
			binCleanup()
			_ = os.RemoveAll(dataDir)
			sharedMinio.mu.Unlock()
			t.Fatalf("minio not ready: %v", err)
		}

		client, err := minio.New(addr, &minio.Options{
			Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
			Secure: false,
		})
		if err != nil {
			cancel()
			_ = cmd.Wait()
			binCleanup()
			_ = os.RemoveAll(dataDir)
			sharedMinio.mu.Unlock()
			t.Fatalf("minio client: %v", err)
		}

		bucket := "test-bucket"
		if err := client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			exists, errExists := client.BucketExists(context.Background(), bucket)
			if errExists != nil || !exists {
				cancel()
				_ = cmd.Wait()
				binCleanup()
				_ = os.RemoveAll(dataDir)
				sharedMinio.mu.Unlock()
				t.Fatalf("make bucket: %v", err)
			}
		}

		sharedMinio.started = true
		sharedMinio.client = client
		sharedMinio.bucket = bucket
		sharedMinio.dataDir = dataDir
		sharedMinio.cancel = cancel
		sharedMinio.cmd = cmd
		sharedMinio.binCleanup = binCleanup
	}
	sharedMinio.refcount++
	client := sharedMinio.client
	bucket := sharedMinio.bucket
	sharedMinio.mu.Unlock()

	prefixID := atomic.AddUint64(&sharedMinioCounter, 1)
	rootPrefix := "root-" + strconv.FormatUint(prefixID, 10) + "-" + time.Now().UTC().Format("20060102150405.000000000")
	cleanup := func() {
		RemoveAllObjects(t, client, bucket, rootPrefix)
		sharedMinio.mu.Lock()
		sharedMinio.refcount--
		if sharedMinio.refcount == 0 && sharedMinio.started {
			sharedMinio.cancel()
			_ = sharedMinio.cmd.Wait()
			sharedMinio.binCleanup()
			_ = os.RemoveAll(sharedMinio.dataDir)
			sharedMinio.started = false
			sharedMinio.client = nil
			sharedMinio.bucket = ""
			sharedMinio.dataDir = ""
			sharedMinio.cancel = nil
			sharedMinio.cmd = nil
			sharedMinio.binCleanup = nil
		}
		sharedMinio.mu.Unlock()
	}
	return client, bucket, rootPrefix, cleanup
}

func RemoveAllObjects(t *testing.T, client *minio.Client, bucket, prefix string) {
	t.Helper()
	if prefix == "" {
		return
	}
	for obj := range client.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("list objects: %v", obj.Err)
		}
		if err := client.RemoveObject(context.Background(), bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			t.Fatalf("remove object: %v", err)
		}
	}
}

func ensureMinioBinary(t *testing.T) (string, func()) {
	if bin := os.Getenv("MINIO_BIN"); bin != "" {
		return bin, func() {}
	}
	url, ok := minioBinaryURL()
	if !ok {
		t.Fatalf("unsupported platform for minio binary")
	}
	cacheDir := filepath.Join(os.TempDir(), "go-imap-maildir-minio")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("create cache dir: %v", err)
	}
	binPath := filepath.Join(cacheDir, "minio")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, func() {}
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("download minio: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download minio: %s", resp.Status)
	}
	file, err := os.OpenFile(binPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatalf("write minio: %v", err)
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(binPath)
		t.Fatalf("write minio: %v", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(binPath)
		t.Fatalf("close minio: %v", err)
	}
	return binPath, func() {}
}

func minioBinaryURL() (string, bool) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	switch osName {
	case "darwin", "linux":
		// ok
	default:
		return "", false
	}
	switch arch {
	case "amd64", "arm64":
		// ok
	default:
		return "", false
	}
	return "https://dl.min.io/server/minio/release/" + osName + "-" + arch + "/minio", true
}

func freeLocalAddr(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func waitForMinioReady(addr string, timeout time.Duration) error {
	url := "http://" + addr + "/minio/health/ready"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timeout")
}
