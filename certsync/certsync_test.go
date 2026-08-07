package certsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is an in-memory S3API. Keys map to object bodies.
type fakeS3 struct {
	objects map[string][]byte

	listErr error
	getErr  error
	putErr  error

	// Per-key failures let tests exercise partial success: only the listed
	// keys error, every other key behaves normally.
	getErrKeys map[string]error
	putErrKeys map[string]error
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	prefix := aws.ToString(in.Prefix)
	var contents []s3types.Object
	for k := range f.objects {
		if prefix == "" || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			key := k
			contents = append(contents, s3types.Object{Key: aws.String(key)})
		}
	}
	sort.Slice(contents, func(i, j int) bool {
		return aws.ToString(contents[i].Key) < aws.ToString(contents[j].Key)
	})
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(false)}, nil
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if err := f.getErrKeys[aws.ToString(in.Key)]; err != nil {
		return nil, err
	}
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	if err := f.putErrKeys[aws.ToString(in.Key)]; err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(in.Key)] = data
	return &s3.PutObjectOutput{}, nil
}

func newSyncer(t *testing.T, fake S3API, prefix string) (*Syncer, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, Bucket: "b", Prefix: prefix}, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

func TestHydrateRestoresCertFilesOnly(t *testing.T) {
	fake := newFakeS3()
	fake.objects["certs/web.example.ts.net.crt"] = []byte("CRT")
	fake.objects["certs/web.example.ts.net.key"] = []byte("KEY")
	fake.objects["certs/acme-account.key.pem"] = []byte("ACME")
	fake.objects["certs/README.txt"] = []byte("nope")           // wrong suffix
	fake.objects["certs/nested/deep.crt"] = []byte("nope")      // nested
	fake.objects["other/web.other.ts.net.crt"] = []byte("nope") // outside prefix

	s, dir := newSyncer(t, fake, "certs/")
	if err := s.Hydrate(context.Background()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	want := map[string]string{
		"web.example.ts.net.crt": "CRT",
		"web.example.ts.net.key": "KEY",
		"acme-account.key.pem":   "ACME",
	}
	got, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		names := make([]string, len(got))
		for i, e := range got {
			names[i] = e.Name()
		}
		t.Fatalf("restored %v, want keys %v", names, want)
	}
	for name, body := range want {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(b) != body {
			t.Errorf("%s = %q, want %q", name, b, body)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s perm = %o, want 0600", name, perm)
		}
	}
}

func TestHydrateEmptyBucketIsNoError(t *testing.T) {
	s, dir := newSyncer(t, newFakeS3(), "certs/")
	if err := s.Hydrate(context.Background()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected empty dir, got %d entries", len(entries))
	}
}

func TestSyncAllUploadsCertFilesOnly(t *testing.T) {
	fake := newFakeS3()
	s, dir := newSyncer(t, fake, "certs/")

	writeFile(t, dir, "web.ts.net.crt", "CRT")
	writeFile(t, dir, "web.ts.net.key", "KEY")
	writeFile(t, dir, "acme-account.key.pem", "ACME")
	writeFile(t, dir, "web.ts.net.crt.tmp", "TEMP") // atomicfile leftover
	writeFile(t, dir, "notes.txt", "nope")

	if err := s.syncAll(context.Background()); err != nil {
		t.Fatalf("syncAll: %v", err)
	}

	want := map[string]string{
		"certs/web.ts.net.crt":       "CRT",
		"certs/web.ts.net.key":       "KEY",
		"certs/acme-account.key.pem": "ACME",
	}
	if len(fake.objects) != len(want) {
		keys := make([]string, 0, len(fake.objects))
		for k := range fake.objects {
			keys = append(keys, k)
		}
		t.Fatalf("uploaded %v, want %v", keys, want)
	}
	for k, body := range want {
		if string(fake.objects[k]) != body {
			t.Errorf("%s = %q, want %q", k, fake.objects[k], body)
		}
	}
}

func TestEmptyPrefixMapsToBareFilenames(t *testing.T) {
	fake := newFakeS3()
	s, dir := newSyncer(t, fake, "")
	writeFile(t, dir, "web.ts.net.crt", "CRT")

	if err := s.syncAll(context.Background()); err != nil {
		t.Fatalf("syncAll: %v", err)
	}
	if _, ok := fake.objects["web.ts.net.crt"]; !ok {
		t.Fatalf("expected key %q, got %v", "web.ts.net.crt", fake.objects)
	}
}

func TestRunFinalSweepOnCancel(t *testing.T) {
	fake := newFakeS3()
	s, dir := newSyncer(t, fake, "certs/")
	writeFile(t, dir, "web.ts.net.crt", "CRT")
	writeFile(t, dir, "web.ts.net.key", "KEY")

	if err := s.Watch(); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Cancel immediately; Run must flush the current dir contents to S3 before
	// returning nil.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	for _, k := range []string{"certs/web.ts.net.crt", "certs/web.ts.net.key"} {
		if _, ok := fake.objects[k]; !ok {
			t.Errorf("expected %q uploaded on final sweep, got %v", k, fake.objects)
		}
	}
}

func TestHydratePartialFailureRestoresRest(t *testing.T) {
	fake := newFakeS3()
	fake.objects["certs/web.example.ts.net.crt"] = []byte("CRT")
	fake.objects["certs/web.example.ts.net.key"] = []byte("KEY")
	fake.objects["certs/acme-account.key.pem"] = []byte("ACME")
	// One object fails to download; the others must still be restored and the
	// failure must surface as a (non-fatal) returned error.
	boom := errors.New("boom")
	fake.getErrKeys = map[string]error{"certs/web.example.ts.net.key": boom}

	s, dir := newSyncer(t, fake, "certs/")
	err := s.Hydrate(context.Background())
	if err == nil {
		t.Fatal("Hydrate: expected error from failed object, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Hydrate error = %v, want it to wrap boom", err)
	}
	for name, body := range map[string]string{
		"web.example.ts.net.crt": "CRT",
		"acme-account.key.pem":   "ACME",
	} {
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Errorf("read %s: %v", name, rerr)
			continue
		}
		if string(b) != body {
			t.Errorf("%s = %q, want %q", name, b, body)
		}
	}
	// The failed object was not written.
	if _, rerr := os.Stat(filepath.Join(dir, "web.example.ts.net.key")); !os.IsNotExist(rerr) {
		t.Errorf("expected failed object absent, stat err = %v", rerr)
	}
}

func TestSyncAllPartialFailureUploadsRest(t *testing.T) {
	fake := newFakeS3()
	s, dir := newSyncer(t, fake, "certs/")
	writeFile(t, dir, "web.ts.net.crt", "CRT")
	writeFile(t, dir, "web.ts.net.key", "KEY")
	writeFile(t, dir, "acme-account.key.pem", "ACME")

	// One upload fails; the others must still be uploaded and the failure must
	// surface so the caller can log it.
	boom := errors.New("boom")
	fake.putErrKeys = map[string]error{"certs/web.ts.net.key": boom}

	err := s.syncAll(context.Background())
	if err == nil {
		t.Fatal("syncAll: expected error from failed upload, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("syncAll error = %v, want it to wrap boom", err)
	}
	for _, k := range []string{"certs/web.ts.net.crt", "certs/acme-account.key.pem"} {
		if _, ok := fake.objects[k]; !ok {
			t.Errorf("expected %q uploaded despite the failure, got %v", k, fake.objects)
		}
	}
	if _, ok := fake.objects["certs/web.ts.net.key"]; ok {
		t.Error("failed upload should be absent from the bucket")
	}
}

func TestRunBeforeWatchErrors(t *testing.T) {
	s, _ := newSyncer(t, newFakeS3(), "certs/")
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run before Watch: expected error, got nil")
	}
}

func TestWatchTwiceErrors(t *testing.T) {
	s, _ := newSyncer(t, newFakeS3(), "certs/")
	if err := s.Watch(); err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	defer s.watcher.Close()
	if err := s.Watch(); err == nil {
		t.Error("second Watch: expected error, got nil")
	}
}

func TestNewRequiresDirAndBucket(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(Config{Bucket: "b"}, newFakeS3(), log); err == nil {
		t.Error("expected error for missing Dir")
	}
	if _, err := New(Config{Dir: t.TempDir()}, newFakeS3(), log); err == nil {
		t.Error("expected error for missing Bucket")
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
