//go:build integration

package defmodels

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))
	if o, err := s.Get(ctx, "a"); err != nil || o != nil {
		t.Fatalf("Get on empty = %v, %v", o, err)
	}
	if err := s.Set(ctx, "a", "claude-sonnet-5", "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "a", "z-ai-glm-5-3", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "b", "claude-haiku-4-5", "root"); err != nil {
		t.Fatal(err)
	}
	o, err := s.Get(ctx, "a")
	if err != nil || o == nil || o.Model != "z-ai-glm-5-3" || o.SetBy != "bob" || o.SetAt.IsZero() {
		t.Fatalf("Get = %+v, %v", o, err)
	}
	if l, err := s.List(ctx); err != nil || len(l) != 2 || l[0].Definition != "a" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if err := s.Clear(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx, "a"); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
	if o, _ := s.Get(ctx, "a"); o != nil {
		t.Fatalf("after Clear: %+v", o)
	}
	if err := s.Set(ctx, "c", "", "x"); err == nil {
		t.Fatal("empty model accepted")
	}
}
