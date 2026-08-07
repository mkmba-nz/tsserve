// Package certsync mirrors tsnet's on-disk TLS certificate cache to an S3
// bucket. tsnet stores the certs it provisions from Let's Encrypt (via
// Tailscale) as files under <state_dir>/certs — one <domain>.crt and
// <domain>.key per served FQDN, plus a shared acme-account.key.pem. On an
// ephemeral filesystem (ECS/Fargate) that directory is lost on every restart,
// so each fresh task re-runs the ACME flow and risks Let's Encrypt rate limits.
//
// certsync treats the local directory as a cache: Hydrate downloads the bucket
// contents into it before tsnet starts, and Run watches the directory and
// uploads any cert tsnet writes (issuance or renewal), plus a final sweep on
// shutdown. Only the three known cert filename shapes are mirrored; the sync is
// overwrite-only, so a renewal reuses the same object key and nothing is ever
// deleted from the bucket.
package certsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fsnotify/fsnotify"
)

// debounce is how long Run waits after the last filesystem event before
// uploading. atomicfile writes a temp file then renames it into place, so a
// single cert write produces a small burst of events; debouncing coalesces
// them into one sweep.
const debounce = 2 * time.Second

// S3API is the subset of the AWS S3 client certsync uses. It lets tests supply
// an in-memory fake.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Config configures a Syncer. Dir and Bucket are required.
type Config struct {
	// Dir is the local cert directory to mirror, i.e. <state_dir>/certs.
	Dir string
	// Bucket is the S3 bucket that backs the cache.
	Bucket string
	// Prefix is an optional key prefix within the bucket. A trailing slash is
	// added if missing; a leading slash is trimmed.
	Prefix string
}

// Syncer mirrors a local cert directory to and from an S3 bucket.
type Syncer struct {
	dir     string
	bucket  string
	prefix  string
	client  S3API
	logger  *slog.Logger
	watcher *fsnotify.Watcher
}

// New returns a Syncer. It creates Dir (0700) if it does not exist so the
// directory can be watched immediately, before tsnet writes its first cert.
func New(cfg Config, client S3API, logger *slog.Logger) (*Syncer, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("certsync: Dir is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("certsync: Bucket is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, fmt.Errorf("certsync: create dir: %w", err)
	}
	prefix := strings.TrimPrefix(cfg.Prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Syncer{
		dir:    cfg.Dir,
		bucket: cfg.Bucket,
		prefix: prefix,
		client: client,
		logger: logger,
	}, nil
}

// isCertFile reports whether name (a bare filename) is one certsync mirrors.
// Restricting to these shapes keeps atomicfile temp files out of the bucket.
func isCertFile(name string) bool {
	return name == "acme-account.key.pem" ||
		strings.HasSuffix(name, ".crt") ||
		strings.HasSuffix(name, ".key")
}

// key maps a bare filename to its S3 object key.
func (s *Syncer) key(name string) string { return s.prefix + name }

// Hydrate downloads every mirrored cert object from the bucket into Dir. It is
// meant to run once, before tsnet starts, so a fresh task reuses the existing
// ACME account key and certificates instead of re-provisioning them. A missing
// or empty bucket is not an error: there is simply nothing to restore.
func (s *Syncer) Hydrate(ctx context.Context) error {
	var token *string
	restored := 0
	var errs []error
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("certsync: list %s/%s: %w", s.bucket, s.prefix, err)
		}
		for _, obj := range out.Contents {
			k := aws.ToString(obj.Key)
			name := strings.TrimPrefix(k, s.prefix)
			// Flat directory only: skip nested keys and non-cert files.
			if name == "" || strings.Contains(name, "/") || !isCertFile(name) {
				continue
			}
			// Best effort: one bad object must not block restoring the rest.
			// The caller downgrades a returned error to a warning and keeps
			// serving, so tsnet simply re-provisions whatever failed to restore.
			if err := s.download(ctx, k, name); err != nil {
				errs = append(errs, err)
				continue
			}
			restored++
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	s.logger.Info("hydrated cert cache from s3", "bucket", s.bucket, "prefix", s.prefix, "files", restored)
	return errors.Join(errs...)
}

// download fetches one object and writes it atomically into Dir.
func (s *Syncer) download(ctx context.Context, key, name string) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("certsync: get %s: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return fmt.Errorf("certsync: read %s: %w", key, err)
	}
	dst := filepath.Join(s.dir, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("certsync: write %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("certsync: rename %s: %w", dst, err)
	}
	return nil
}

// Watch registers the filesystem watch on Dir. It must be called (successfully)
// before Run, and before tsnet starts writing certs, so no early cert write is
// missed. Keeping it out of Run lets a watcher-setup failure surface to the
// caller at startup instead of dying silently in a background goroutine.
func (s *Syncer) Watch() error {
	if s.watcher != nil {
		return fmt.Errorf("certsync: Watch already called")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("certsync: watcher: %w", err)
	}
	if err := w.Add(s.dir); err != nil {
		w.Close()
		return fmt.Errorf("certsync: watch %s: %w", s.dir, err)
	}
	s.watcher = w
	return nil
}

// Run watches Dir and uploads mirrored certs as tsnet writes them, coalescing
// bursts with a short debounce. It performs a final sweep and returns nil when
// ctx is cancelled, so callers can wait on it during shutdown to ensure a cert
// issued just before exit reaches the bucket. Watch must have been called first.
func (s *Syncer) Run(ctx context.Context) error {
	if s.watcher == nil {
		return fmt.Errorf("certsync: Run called before Watch")
	}
	w := s.watcher
	defer w.Close()

	// Stopped timer, started/reset on each event.
	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	pending := false

	for {
		select {
		case <-ctx.Done():
			// Final sweep uses a fresh context: ctx is already cancelled.
			sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.syncAll(sweepCtx); err != nil {
				s.logger.Warn("final cert sync failed", "error", err)
			}
			return nil

		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			if !isCertFile(filepath.Base(event.Name)) {
				continue
			}
			timer.Reset(debounce)
			pending = true

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			s.logger.Warn("cert watcher error", "error", err)

		case <-timer.C:
			// If ctx is already cancelled, let the ctx.Done case own the sweep
			// (with a fresh context) rather than firing a doomed one here.
			if !pending || ctx.Err() != nil {
				continue
			}
			pending = false
			if err := s.syncAll(ctx); err != nil {
				s.logger.Warn("cert sync failed", "error", err)
			}
		}
	}
}

// syncAll uploads every mirrored cert file currently in Dir.
func (s *Syncer) syncAll(ctx context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("certsync: read dir: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !isCertFile(e.Name()) {
			continue
		}
		// Best effort: one failed upload must not strand the other certs in
		// this sweep (notably the final shutdown sweep).
		if err := s.upload(ctx, e.Name()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// upload writes one local cert file to the bucket, overwriting any prior object.
func (s *Syncer) upload(ctx context.Context, name string) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // raced with a rename; a later event will catch it
		}
		return fmt.Errorf("certsync: read %s: %w", name, err)
	}
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(name)),
		Body:   bytes.NewReader(data),
	}); err != nil {
		return fmt.Errorf("certsync: put %s: %w", s.key(name), err)
	}
	s.logger.Debug("uploaded cert to s3", "key", s.key(name))
	return nil
}
