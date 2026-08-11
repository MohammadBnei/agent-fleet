package coreserver

import (
	"context"
	"errors"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
)

// fakeFileStore is a plain in-memory filestore.Store — the RPC handlers'
// own job (arg passthrough, proto shape mapping) is what's under test
// here, not filestore.S3Store's real Garage calls (covered by that
// package's own guard-clause tests).
type fakeFileStore struct {
	files      []filestore.FileMetadata
	presignErr error
	deletedKey string
}

func (f *fakeFileStore) List(context.Context) ([]filestore.FileMetadata, error) {
	if f.presignErr != nil {
		return nil, f.presignErr
	}
	return f.files, nil
}

func (f *fakeFileStore) PresignUpload(_ context.Context, filename, _ string) (string, string, time.Time, error) {
	if f.presignErr != nil {
		return "", "", time.Time{}, f.presignErr
	}
	return "https://s3.bnei.dev/" + filename + "?sig=abc", filename, time.Unix(1000, 0), nil
}

func (f *fakeFileStore) PresignDownload(_ context.Context, key string) (string, time.Time, error) {
	if f.presignErr != nil {
		return "", time.Time{}, f.presignErr
	}
	return "https://s3.bnei.dev/" + key + "?sig=def", time.Unix(2000, 0), nil
}

func (f *fakeFileStore) Delete(_ context.Context, key string) error {
	if f.presignErr != nil {
		return f.presignErr
	}
	f.deletedKey = key
	return nil
}

func TestServer_ListFiles(t *testing.T) {
	fake := &fakeFileStore{files: []filestore.FileMetadata{
		{Key: "notes.txt", SizeBytes: 42, LastModified: time.Unix(1000, 0), ContentType: "text/plain"},
	}}
	s := New(nil, nil, nil, nil, nil, fake, nil)

	resp, err := s.ListFiles(context.Background(), &agentfleetv1.ListFilesRequest{})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(resp.GetFiles()) != 1 || resp.GetFiles()[0].GetKey() != "notes.txt" {
		t.Fatalf("unexpected files: %+v", resp.GetFiles())
	}
}

func TestServer_ListFiles_Error(t *testing.T) {
	fake := &fakeFileStore{presignErr: errors.New("garage unreachable")}
	s := New(nil, nil, nil, nil, nil, fake, nil)

	if _, err := s.ListFiles(context.Background(), &agentfleetv1.ListFilesRequest{}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestServer_GetFileUploadUrl(t *testing.T) {
	fake := &fakeFileStore{}
	s := New(nil, nil, nil, nil, nil, fake, nil)

	resp, err := s.GetFileUploadUrl(context.Background(), &agentfleetv1.GetFileUploadUrlRequest{Filename: "report.pdf", ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("GetFileUploadUrl: %v", err)
	}
	if resp.GetKey() != "report.pdf" || resp.GetUploadUrl() == "" || resp.GetExpiresAt() == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServer_GetFileDownloadUrl(t *testing.T) {
	fake := &fakeFileStore{}
	s := New(nil, nil, nil, nil, nil, fake, nil)

	resp, err := s.GetFileDownloadUrl(context.Background(), &agentfleetv1.GetFileDownloadUrlRequest{Key: "report.pdf"})
	if err != nil {
		t.Fatalf("GetFileDownloadUrl: %v", err)
	}
	if resp.GetDownloadUrl() == "" || resp.GetExpiresAt() == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServer_DeleteFile(t *testing.T) {
	fake := &fakeFileStore{}
	s := New(nil, nil, nil, nil, nil, fake, nil)

	resp, err := s.DeleteFile(context.Background(), &agentfleetv1.DeleteFileRequest{Key: "report.pdf"})
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if resp.GetStatus() != "deleted" || fake.deletedKey != "report.pdf" {
		t.Fatalf("unexpected response/state: resp=%+v deletedKey=%q", resp, fake.deletedKey)
	}
}
