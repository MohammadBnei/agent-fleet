// Package filestore backs the fleet's shared file space (docs/adr/0030):
// one flat Garage S3 bucket, core is the sole credential holder, and the
// only thing core itself ever does with a file is mint a short-lived
// presigned PUT/GET URL — actual bytes move directly between the caller
// (agent curl, dashboard browser fetch) and Garage, never through core.
package filestore

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// presignExpiry matches the fleet's other short-lived-token convention
// (lease IDs, AskUserQuestion's long-poll timeout) — long enough for a
// human or an agent's curl call to actually start the transfer, short
// enough that a leaked URL isn't a standing credential.
const presignExpiry = 15 * time.Minute

type FileMetadata struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ContentType  string
}

// Store is the interface coreserver/dashboard depend on — a fake
// implementing this (no real Garage call) is enough to unit-test their
// RPC handlers, same pattern transcript.Store already established.
type Store interface {
	List(ctx context.Context) ([]FileMetadata, error)
	PresignUpload(ctx context.Context, filename, contentType string) (url, key string, expiresAt time.Time, err error)
	PresignDownload(ctx context.Context, key string) (url string, expiresAt time.Time, err error)
	Delete(ctx context.Context, key string) error
}

type Config struct {
	Endpoint  string // e.g. https://s3.bnei.dev — must be externally reachable, see package doc
	Bucket    string
	AccessKey string
	SecretKey string
}

type S3Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

var _ Store = (*S3Store)(nil)

func New(ctx context.Context, cfg Config) (*S3Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("garage"), // Garage's fixed region name, see garage-configure.yml
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("filestore: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true // Garage requires path-style addressing
	})
	return &S3Store{client: client, presign: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

func (s *S3Store) List(ctx context.Context) ([]FileMetadata, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket)})
	if err != nil {
		return nil, fmt.Errorf("filestore: list: %w", err)
	}
	files := make([]FileMetadata, 0, len(out.Contents))
	for _, obj := range out.Contents {
		files = append(files, FileMetadata{
			Key:          aws.ToString(obj.Key),
			SizeBytes:    aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
		})
	}
	return files, nil
}

func (s *S3Store) PresignUpload(ctx context.Context, filename, contentType string) (string, string, time.Time, error) {
	if filename == "" {
		return "", "", time.Time{}, fmt.Errorf("filestore: filename is required")
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(filename),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(presignExpiry))
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("filestore: presign upload: %w", err)
	}
	return req.URL, filename, time.Now().Add(presignExpiry), nil
}

func (s *S3Store) PresignDownload(ctx context.Context, key string) (string, time.Time, error) {
	if key == "" {
		return "", time.Time{}, fmt.Errorf("filestore: key is required")
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(presignExpiry))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("filestore: presign download: %w", err)
	}
	return req.URL, time.Now().Add(presignExpiry), nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("filestore: key is required")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("filestore: delete: %w", err)
	}
	return nil
}
