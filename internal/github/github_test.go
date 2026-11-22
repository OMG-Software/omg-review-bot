package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The installation token goes only to the configured API host: not to
// another host named in a pagination link, and not through a redirect.
func TestTokenStaysOnAPIHost(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			elsewhere.Add(1)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer other.Close()

	var contents atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/web/contents") {
			contents.Add(1)
		}
		switch r.URL.Path {
		case "/repos/acme/web/pulls/1/comments":
			w.Header().Set("Link", `<`+other.URL+`/steal?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/web/pulls/1":
			http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		case "/repos/acme/web/pulls/2":
			http.Redirect(w, r, "/repos/acme/web/pulls/3", http.StatusMovedPermanently) // same host: followed
		case "/repos/acme/web/pulls/3":
			_, _ = w.Write([]byte(`{"number":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	a := &App{base: api.URL, hc: apiHTTPClient(api.URL, 0), tokens: map[int64]instToken{1: {token: "secret", exp: time.Now().Add(time.Hour)}}}
	c := a.Installation(1)
	ctx := context.Background()

	if _, err := c.ReviewComments(ctx, "acme/web", 1); err == nil || !strings.Contains(err.Error(), "not the GitHub API host") {
		t.Errorf("foreign pagination link: err = %v", err)
	}
	if _, err := c.Pull(ctx, "acme/web", 1); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("foreign redirect: err = %v", err)
	}
	if pr, err := c.Pull(ctx, "acme/web", 2); err != nil || pr.Number != 3 {
		t.Errorf("same-host redirect: %+v, %v", pr, err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("token sent to another host %d time(s)", n)
	}
	for _, p := range []string{"../../orgs/x", "a/../b", "a//b", "./a"} {
		if _, err := c.FileContent(ctx, "acme/web", p, "main"); err == nil || !strings.Contains(err.Error(), "refusing file path") {
			t.Errorf("FileContent(%q): err = %v, want the path refused", p, err)
		}
	}
	if n := contents.Load(); n != 0 {
		t.Errorf("refused paths still made %d contents request(s)", n)
	}
}
