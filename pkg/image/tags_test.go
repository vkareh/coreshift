package image

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

// newTagRegistry serves a minimal v2 registry that pages through tags
// pageSize at a time using Link headers, like real registries do. It
// returns a repository reference pointing at the fake registry.
func newTagRegistry(t *testing.T, tags []string, pageSize int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/repo/tags/list", func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if last := r.URL.Query().Get("last"); last != "" {
			start = slices.Index(tags, last) + 1
		}
		end := min(start+pageSize, len(tags))
		if end < len(tags) {
			w.Header().Set("Link", fmt.Sprintf("</v2/repo/tags/list?last=%s>; rel=\"next\"", tags[end-1]))
		}
		w.Header().Set("Content-Type", "application/json")
		page, err := json.Marshal(map[string][]string{"tags": tags[start:end]})
		if err != nil {
			t.Errorf("marshal tag page: %v", err)
			return
		}
		_, _ = w.Write(page)
	})
	// Registry ping endpoint.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1 would be reached over https; localhost keeps it http.
	return "localhost:" + parsed.Port() + "/repo"
}

func TestListTagsFollowsLinkPagination(t *testing.T) {
	// "duplicate" appears on two different pages.
	ref := newTagRegistry(t, []string{"one", "two", "duplicate", "three", "duplicate", "four"}, 2)

	tags, err := (&Client{}).ListTags(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "duplicate", "three", "four"}
	if !slices.Equal(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}

func TestListTagsSinglePage(t *testing.T) {
	// The whole tag list fits one page, so the registry sends no Link header.
	ref := newTagRegistry(t, []string{"one", "two"}, 100)

	tags, err := (&Client{}).ListTags(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two"}
	if !slices.Equal(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}
