package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// objectInfo is one object under a prefix.
type objectInfo struct {
	Key  string
	Size int64
}

// objectStore exposes the two backend operations the storage abstraction does
// not have: listing a prefix, and deleting exactly one key.
//
// Exact-key deletion matters: prefix deletion is not a substitute, because the
// plain payload path is a prefix of its own header ("{build}/memfile" matches
// "{build}/memfile.header") and of its compressed variants
// ("{build}/memfile.zstd"), so deleting a superseded payload by prefix would
// destroy the artifact the migration just wrote.
//
// Only the backends a migration can be run against are implemented; anything
// else fails loudly instead of silently skipping deletions.
type objectStore interface {
	List(ctx context.Context, prefix string) ([]objectInfo, error)
	Delete(ctx context.Context, key string) error
}

func storeFor(ctx context.Context, spec storage.Spec) (objectStore, error) {
	switch spec.Provider {
	case storage.LocalStorageProvider:
		return &fsStore{root: spec.BasePath}, nil
	case storage.AWSStorageProvider:
		region := spec.Region
		if region == "" {
			region = "us-east-1"
		}

		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}

		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if spec.Endpoint != "" {
				o.BaseEndpoint = aws.String(spec.Endpoint)
			}

			o.UsePathStyle = spec.UsePathStyle
		})

		return &s3Store{client: client, bucket: spec.Bucket}, nil
	default:
		return nil, fmt.Errorf("provider %s: listing and exact-key deletion are not implemented here yet; run without -delete-superseded/-scan-prefix", spec.Provider)
	}
}

// fsStore is the filesystem backend (file:// storage URLs).
type fsStore struct {
	root string
}

func (s *fsStore) resolve(key string) (string, error) {
	full := filepath.Join(s.root, filepath.FromSlash(key))

	rel, err := filepath.Rel(s.root, full)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", key, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("key %q escapes the storage root", key)
	}

	return full, nil
}

func (s *fsStore) List(_ context.Context, prefix string) ([]objectInfo, error) {
	var out []objectInfo

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		key, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}

		key = filepath.ToSlash(key)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		out = append(out, objectInfo{Key: key, Size: info.Size()})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", s.root, err)
	}

	return out, nil
}

func (s *fsStore) Delete(_ context.Context, key string) error {
	for _, candidate := range []string{key, storage.SizeSidecar(key)} {
		full, err := s.resolve(candidate)
		if err != nil {
			return err
		}

		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", candidate, err)
		}
	}

	return nil
}

// s3Store is the S3-compatible backend (s3:// storage URLs, Silo included).
type s3Store struct {
	client *s3.Client
	bucket string
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	var out []objectInfo

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}

		for _, object := range page.Contents {
			out = append(out, objectInfo{Key: aws.ToString(object.Key), Size: aws.ToInt64(object.Size)})
		}
	}

	return out, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	for _, candidate := range []string{key, storage.SizeSidecar(key)} {
		// Deleting a missing key is not an error in S3, which is what makes this
		// safe to re-run.
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(candidate),
		}); err != nil {
			return fmt.Errorf("delete %s: %w", candidate, err)
		}
	}

	return nil
}
