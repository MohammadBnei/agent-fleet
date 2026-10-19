package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func fake(t *testing.T, failPage2 bool) (*Client, *[]string) {
	t.Helper()
	var patched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/repos":
			if r.URL.Query().Get("affiliation") != "owner,collaborator" {
				t.Errorf("affiliation = %q", r.URL.Query().Get("affiliation"))
			}
			var page []Repo
			switch r.URL.Query().Get("page") {
			case "1":
				for i := range perPage {
					page = append(page, Repo{ID: int64(i + 1), Name: fmt.Sprintf("r%d", i), Archived: i == 0, Fork: i == 1})
				}
			case "2":
				if failPage2 {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				page = []Repo{{ID: 999, Name: "last"}}
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodGet && r.URL.Path == "/user/repository_invitations":
			_, _ = w.Write([]byte(`[{"id":1,"inviter":{"login":"MohammadBnei"},"repository":{"full_name":"MohammadBnei/a"}},
				{"id":2,"inviter":{"login":"stranger"},"repository":{"full_name":"stranger/b"}}]`))
		case r.Method == http.MethodPatch:
			patched = append(patched, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok", []string{"MohammadBnei"})
	c.API = srv.URL
	return c, &patched
}

func TestListReposAllPagesSkipsArchivedAndForks(t *testing.T) {
	c, _ := fake(t, false)
	got, err := c.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 100 on page 1 minus one archived and one fork, plus page 2's one.
	if len(got) != perPage-1 {
		t.Fatalf("len = %d, want %d", len(got), perPage-1)
	}
	if got[len(got)-1].ID != 999 {
		t.Errorf("page 2 missing")
	}
	for _, r := range got {
		if r.Archived || r.Fork {
			t.Errorf("kept %+v", r)
		}
	}
}

// A failed later page must fail the whole listing: a partial list reads as
// removals in core's sync.
func TestListReposPartialIsError(t *testing.T) {
	c, _ := fake(t, true)
	if got, err := c.ListRepos(context.Background()); err == nil {
		t.Fatalf("got %d repos and no error", len(got))
	}
}

func TestAcceptInvitesOnlyAllowlisted(t *testing.T) {
	c, patched := fake(t, false)
	got, err := c.AcceptInvites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"MohammadBnei/a"}) {
		t.Errorf("accepted = %v", got)
	}
	if !slices.Equal(*patched, []string{"/user/repository_invitations/1"}) {
		t.Errorf("patched = %v", *patched)
	}
}

func TestNoTokenFailsClosed(t *testing.T) {
	c := New("", nil)
	if _, err := c.ListRepos(context.Background()); err == nil {
		t.Fatal("no error without a token")
	}
}
