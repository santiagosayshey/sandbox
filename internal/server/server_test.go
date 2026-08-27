package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santiagosayshey/sandbox/internal/proxy"
	"github.com/santiagosayshey/sandbox/internal/store"
	"github.com/santiagosayshey/sandbox/internal/watch"
)

func newServer(t *testing.T, upstreams ...proxy.Upstream) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w, err := watch.New(dir, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	h, err := New(st, w, Config{MaxUpload: 1024, Version: "test", Upstreams: upstreams})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, dir
}

func do(t *testing.T, method, u string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// A stray client credential must never reach an upstream.
	req.Header.Set("Authorization", "Bearer stray")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPutCreatesParentsAndReplaces(t *testing.T) {
	srv, dir := newServer(t)
	u := srv.URL + "/files/assets/A%20Film%20(1966)/poster.jpg"
	if resp := do(t, http.MethodPut, u, "one"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first put: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodPut, u, "two"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second put: %d", resp.StatusCode)
	}
	b, err := os.ReadFile(filepath.Join(dir, "assets", "A Film (1966)", "poster.jpg"))
	if err != nil || string(b) != "two" {
		t.Fatalf("on disk: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "assets", "A Film (1966)")); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestPutRejectsTraversalAndRoot(t *testing.T) {
	srv, _ := newServer(t)
	for _, p := range []string{"/files/..%2Fescape", "/files/a/..%2F..%2Fescape", "/files/."} {
		resp := do(t, http.MethodPut, srv.URL+p, "x")
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestPutTooLarge(t *testing.T) {
	srv, dir := newServer(t)
	resp := do(t, http.MethodPut, srv.URL+"/files/big.bin", strings.Repeat("x", 2048))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestGetDeleteAndTree(t *testing.T) {
	srv, _ := newServer(t)
	do(t, http.MethodPut, srv.URL+"/files/dir/a.txt", "hello")
	do(t, http.MethodPut, srv.URL+"/files/dir/sub/b.yml", "k: v")

	resp := do(t, http.MethodGet, srv.URL+"/files/dir/a.txt", "")
	if resp.StatusCode != http.StatusOK || readAll(t, resp.Body) != "hello" {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/dir", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("get dir: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/missing", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tree", nil)
	req.Header.Set("Accept", "text/plain")
	resp, _ = http.DefaultClient.Do(req)
	plain := readAll(t, resp.Body)
	if !strings.Contains(plain, "dir/\n  sub/\n    b.yml") || !strings.Contains(plain, "2 files") {
		t.Errorf("plain tree:\n%s", plain)
	}
	resp = do(t, http.MethodGet, srv.URL+"/tree", "")
	html := readAll(t, resp.Body)
	if !strings.Contains(html, `data-path="dir/sub/b.yml"`) || !strings.Contains(html, "2 files") {
		t.Errorf("html tree:\n%s", html)
	}

	if resp := do(t, http.MethodDelete, srv.URL+"/files/dir/sub", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete dir: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodDelete, srv.URL+"/files/dir/sub", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/dir/sub/b.yml", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: %d", resp.StatusCode)
	}
}

func TestPreviewKinds(t *testing.T) {
	srv, _ := newServer(t)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)
	do(t, http.MethodPut, srv.URL+"/files/p.png", png)
	do(t, http.MethodPut, srv.URL+"/files/t.mp3", "ID3"+strings.Repeat("\x00", 16))
	do(t, http.MethodPut, srv.URL+"/files/m.yml", "metadata:\n  x: <y>\n")
	do(t, http.MethodPut, srv.URL+"/files/o.bin", "\x00\x01\x02\x03")

	cases := map[string]string{
		"p.png": `<img src="/files/p.png"`,
		"t.mp3": `<audio controls src="/files/t.mp3"`,
		"m.yml": `<pre>metadata:
  x: &lt;y&gt;`,
		"o.bin": "No preview",
	}
	for name, want := range cases {
		resp := do(t, http.MethodGet, srv.URL+"/preview/"+name, "")
		body := readAll(t, resp.Body)
		if !strings.Contains(body, want) {
			t.Errorf("%s: want %q in\n%s", name, want, body)
		}
	}
}

func TestReset(t *testing.T) {
	srv, dir := newServer(t)
	do(t, http.MethodPut, srv.URL+"/files/a/b/c.txt", "x")
	do(t, http.MethodPut, srv.URL+"/files/d.txt", "x")
	if resp := do(t, http.MethodPost, srv.URL+"/reset", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestEvents(t *testing.T) {
	srv, _ := newServer(t)
	resp := do(t, http.MethodGet, srv.URL+"/events", "")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	do(t, http.MethodPut, srv.URL+"/files/new.txt", "x")
	buf := make([]byte, 8192)
	var got string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(got, "new.txt") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "event: tree\n") || !strings.Contains(got, "new.txt") {
		t.Fatalf("stream:\n%s", got)
	}
}

func TestProxyInjectsCredentialAndStripsPrefix(t *testing.T) {
	var seen *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	u := proxy.Upstream{
		Name:   "tmdb",
		URL:    target,
		Header: http.Header{"X-Api-Key": {"secret"}},
		Query:  url.Values{"api_key": {"qsecret"}},
		Allow:  []proxy.Rule{{Method: "GET", Path: "/3/movie/*"}},
	}
	srv, _ := newServer(t, u)

	resp := do(t, http.MethodGet, srv.URL+"/tmdb/3/movie/429?language=en", "")
	if resp.StatusCode != http.StatusOK || readAll(t, resp.Body) != `{"ok":true}` {
		t.Fatalf("proxied: %d", resp.StatusCode)
	}
	if seen.URL.Path != "/3/movie/429" {
		t.Errorf("path %q", seen.URL.Path)
	}
	if q := seen.URL.Query(); q.Get("api_key") != "qsecret" || q.Get("language") != "en" {
		t.Errorf("query %q", seen.URL.RawQuery)
	}
	if seen.Header.Get("X-Api-Key") != "secret" {
		t.Errorf("header not injected: %v", seen.Header)
	}
	if seen.Header.Get("Authorization") != "" {
		t.Errorf("client Authorization header leaked upstream")
	}

	resp = do(t, http.MethodGet, srv.URL+"/upstreams", "")
	if body := readAll(t, resp.Body); !strings.Contains(body, `"tmdb": [
    "GET /3/movie/*"
  ]`) {
		t.Errorf("upstreams: %q", body)
	}

	seen = nil
	resp = do(t, http.MethodDelete, srv.URL+"/tmdb/3/movie/429", "")
	if body := readAll(t, resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "GET /3/movie/*") {
		t.Errorf("denied method: %d %q", resp.StatusCode, body)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/tmdb/3/account", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied path: %d", resp.StatusCode)
	}
	if seen != nil {
		t.Errorf("denied request reached the upstream")
	}
}

func TestUpstreamCannotShadowRoute(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	target, _ := url.Parse("http://example.com")
	_, err := New(st, nil, Config{Upstreams: []proxy.Upstream{{Name: "files", URL: target}}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPlainRoutes(t *testing.T) {
	srv, _ := newServer(t)
	for p, want := range map[string]string{
		"/healthz":  "ok\n",
		"/version":  "test\n",
		"/llms.txt": "# sandbox",
		"/":         "<title>sandbox</title>",
	} {
		resp := do(t, http.MethodGet, srv.URL+p, "")
		if body := readAll(t, resp.Body); resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
	}
	resp := do(t, http.MethodGet, srv.URL+"/llms.txt", "")
	if body := readAll(t, resp.Body); strings.Contains(body, "{{") {
		t.Errorf("unexpanded placeholder in llms.txt")
	}
}

func TestDeadUpstreamExplains(t *testing.T) {
	target, _ := url.Parse("https://does-not-exist.invalid")
	srv, _ := newServer(t, proxy.Upstream{Name: "dead", URL: target, Allow: []proxy.Rule{{Method: "*", Path: "/*"}}})
	resp := do(t, http.MethodGet, srv.URL+"/dead/x", "")
	body := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "upstream dead:") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}
