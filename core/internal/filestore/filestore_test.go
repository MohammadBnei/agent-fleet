package filestore

import (
	"context"
	"testing"
)

// These exercise the guard clauses only — the zero-value *S3Store never
// touches its nil client/presign fields for an empty filename/key, so no
// real Garage endpoint is needed. The actual presign/list/delete calls are
// thin passthroughs to aws-sdk-go-v2, exercised end-to-end in the manual
// verification steps (docs/adr/0030), not re-tested here.
func TestS3Store_GuardClauses(t *testing.T) {
	s := &S3Store{}
	ctx := context.Background()

	t.Run("PresignUpload rejects empty filename", func(t *testing.T) {
		if _, _, _, err := s.PresignUpload(ctx, "", "text/plain"); err == nil {
			t.Fatal("expected error for empty filename, got nil")
		}
	})

	t.Run("PresignDownload rejects empty key", func(t *testing.T) {
		if _, _, err := s.PresignDownload(ctx, ""); err == nil {
			t.Fatal("expected error for empty key, got nil")
		}
	})

	t.Run("Delete rejects empty key", func(t *testing.T) {
		if err := s.Delete(ctx, ""); err == nil {
			t.Fatal("expected error for empty key, got nil")
		}
	})
}
