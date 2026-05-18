package github_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chain-registry-sentinel/internal/github"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *github.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return github.NewClient("test-token").WithBaseURL(srv.URL)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDefaultBranch_ok(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"default_branch": "main"})
	})
	branch, err := client.DefaultBranch(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "main" {
		t.Errorf("want main, got %q", branch)
	}
}

func TestNextBranchN_noExisting(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	n, err := client.NextBranchN(context.Background(), "owner", "repo", "cosmoshub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1, got %d", n)
	}
}

func TestNextBranchN_existing(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]string{
			{"ref": "refs/heads/sentinel/cosmoshub-1"},
			{"ref": "refs/heads/sentinel/cosmoshub-3"},
			{"ref": "refs/heads/sentinel/cosmoshub-2"},
		})
	})
	n, err := client.NextBranchN(context.Background(), "owner", "repo", "cosmoshub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 {
		t.Errorf("want 4, got %d", n)
	}
}

func TestHasOpenPR_true(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"total_count": 1})
	})
	got, err := client.HasOpenPR(context.Background(), "owner", "repo", "cosmoshub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("want true for total_count > 0")
	}
}

func TestHasOpenPR_false(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"total_count": 0})
	})
	got, err := client.HasOpenPR(context.Background(), "owner", "repo", "cosmoshub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("want false for total_count == 0")
	}
}

func TestEnsureLabel_creates(t *testing.T) {
	postCalled := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			postCalled = true
			w.WriteHeader(http.StatusCreated)
		}
	})
	if err := client.EnsureLabel(context.Background(), "owner", "repo", "sentinel", "0052cc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !postCalled {
		t.Error("want POST to create label")
	}
}

func TestEnsureLabel_alreadyExists(t *testing.T) {
	postCalled := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
		case http.MethodPost:
			postCalled = true
			w.WriteHeader(http.StatusCreated)
		}
	})
	if err := client.EnsureLabel(context.Background(), "owner", "repo", "sentinel", "0052cc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if postCalled {
		t.Error("want no POST when label already exists")
	}
}

func TestCreateBranch_ok(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	if err := client.CreateBranch(context.Background(), "owner", "repo", "sentinel/foo-1", "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetFileSHA_ok(t *testing.T) {
	raw := []byte(`{"chain_name":"osmosis"}`)
	encoded := base64.StdEncoding.EncodeToString(raw)
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"content": encoded,
			"sha":     "blobsha123",
		})
	})
	content, sha, err := client.GetFileSHA(context.Background(), "owner", "repo", "osmosis/chain.json", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(content, raw) {
		t.Errorf("content: got %q want %q", content, raw)
	}
	if sha != "blobsha123" {
		t.Errorf("sha: got %q want blobsha123", sha)
	}
}

func TestCommitFile_ok(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := client.CommitFile(context.Background(), "owner", "repo", "chain.json", "sentinel/foo-1", "msg", "sha", []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePR_ok(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"number":   42,
			"html_url": "https://github.com/owner/repo/pull/42",
		})
	})
	num, prURL, err := client.CreatePR(context.Background(), "owner", "repo", "title", "body", "head", "base")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if num != 42 {
		t.Errorf("want number 42, got %d", num)
	}
	if prURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("unexpected url: %s", prURL)
	}
}
