package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
)

// fakeFileStore is a plain in-memory filestore.Store — same rationale as
// coreserver's own fakeFileStore: these RPC handlers are thin proto-shape
// passthroughs, not where filestore.S3Store's real Garage calls need
// re-testing.
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
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, fake, nil, 5, nil, nil)

	resp, err := s.ListFiles(context.Background(), connect.NewRequest(&agentfleetv1.ListFilesRequest{}))
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(resp.Msg.GetFiles()) != 1 || resp.Msg.GetFiles()[0].GetKey() != "notes.txt" {
		t.Fatalf("unexpected files: %+v", resp.Msg.GetFiles())
	}
}

func TestServer_ListFiles_Error(t *testing.T) {
	fake := &fakeFileStore{presignErr: errors.New("garage unreachable")}
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, fake, nil, 5, nil, nil)

	if _, err := s.ListFiles(context.Background(), connect.NewRequest(&agentfleetv1.ListFilesRequest{})); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestServer_GetFileUploadUrl(t *testing.T) {
	fake := &fakeFileStore{}
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, fake, nil, 5, nil, nil)

	resp, err := s.GetFileUploadUrl(context.Background(), connect.NewRequest(&agentfleetv1.GetFileUploadUrlRequest{Filename: "report.pdf"}))
	if err != nil {
		t.Fatalf("GetFileUploadUrl: %v", err)
	}
	if resp.Msg.GetKey() != "report.pdf" || resp.Msg.GetUploadUrl() == "" {
		t.Fatalf("unexpected response: %+v", resp.Msg)
	}
}

func TestServer_GetFileDownloadUrl(t *testing.T) {
	fake := &fakeFileStore{}
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, fake, nil, 5, nil, nil)

	resp, err := s.GetFileDownloadUrl(context.Background(), connect.NewRequest(&agentfleetv1.GetFileDownloadUrlRequest{Key: "report.pdf"}))
	if err != nil {
		t.Fatalf("GetFileDownloadUrl: %v", err)
	}
	if resp.Msg.GetDownloadUrl() == "" {
		t.Fatalf("unexpected response: %+v", resp.Msg)
	}
}

func TestServer_DeleteFile(t *testing.T) {
	fake := &fakeFileStore{}
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, fake, nil, 5, nil, nil)

	resp, err := s.DeleteFile(context.Background(), connect.NewRequest(&agentfleetv1.DeleteFileRequest{Key: "report.pdf"}))
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if resp.Msg.GetStatus() != "deleted" || fake.deletedKey != "report.pdf" {
		t.Fatalf("unexpected response/state: resp=%+v deletedKey=%q", resp.Msg, fake.deletedKey)
	}
}
