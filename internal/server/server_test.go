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

const token = "test-token"

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
	h, err := New(st, w, Config{Token: token, MaxUpload: 1024, Version: "test", Upstreams: upstreams})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, dir
}

func do(t *testing.T, method, u string, body string, auth bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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
	if resp := do(t, http.MethodPut, u, "one", true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first put: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodPut, u, "two", true); resp.StatusCode != http.StatusNoContent {
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
		resp := do(t, http.MethodPut, srv.URL+p, "x", true)
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestPutTooLarge(t *testing.T) {
	srv, dir := newServer(t)
	resp := do(t, http.MethodPut, srv.URL+"/files/big.bin", strings.Repeat("x", 2048), true)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestAuth(t *testing.T) {
	srv, _ := newServer(t)
	if resp := do(t, http.MethodPut, srv.URL+"/files/x", "x", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/files/x", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong bearer: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodDelete, srv.URL+"/files/x", "", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("delete without bearer: %d", resp.StatusCode)
	}
}

func TestGetDeleteAndTree(t *testing.T) {
	srv, _ := newServer(t)
	do(t, http.MethodPut, srv.URL+"/files/dir/a.txt", "hello", true)
	do(t, http.MethodPut, srv.URL+"/files/dir/sub/b.yml", "k: v", true)

	resp := do(t, http.MethodGet, srv.URL+"/files/dir/a.txt", "", false)
	if resp.StatusCode != http.StatusOK || readAll(t, resp.Body) != "hello" {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/dir", "", false); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("get dir: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/missing", "", false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tree", nil)
	req.Header.Set("Accept", "text/plain")
	resp, _ = http.DefaultClient.Do(req)
	plain := readAll(t, resp.Body)
	if !strings.Contains(plain, "dir/\n  sub/\n    b.yml") || !strings.Contains(plain, "2 files") {
		t.Errorf("plain tree:\n%s", plain)
	}
	resp = do(t, http.MethodGet, srv.URL+"/tree", "", false)
	html := readAll(t, resp.Body)
	if !strings.Contains(html, `data-path="dir/sub/b.yml"`) || !strings.Contains(html, "2 files") {
		t.Errorf("html tree:\n%s", html)
	}

	if resp := do(t, http.MethodDelete, srv.URL+"/files/dir/sub", "", true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete dir: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodDelete, srv.URL+"/files/dir/sub", "", true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/files/dir/sub/b.yml", "", false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: %d", resp.StatusCode)
	}
}

func TestPreviewKinds(t *testing.T) {
	srv, _ := newServer(t)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)
	do(t, http.MethodPut, srv.URL+"/files/p.png", png, true)
	do(t, http.MethodPut, srv.URL+"/files/t.mp3", "ID3"+strings.Repeat("\x00", 16), true)
	do(t, http.MethodPut, srv.URL+"/files/m.yml", "metadata:\n  x: <y>\n", true)
	do(t, http.MethodPut, srv.URL+"/files/o.bin", "\x00\x01\x02\x03", true)

	cases := map[string]string{
		"p.png": `<img src="/files/p.png"`,
		"t.mp3": `<audio controls src="/files/t.mp3"`,
		"m.yml": `<pre>metadata:
  x: &lt;y&gt;`,
		"o.bin": "No preview",
	}
	for name, want := range cases {
		resp := do(t, http.MethodGet, srv.URL+"/preview/"+name, "", false)
		body := readAll(t, resp.Body)
		if !strings.Contains(body, want) {
			t.Errorf("%s: want %q in\n%s", name, want, body)
		}
	}
}

func TestReset(t *testing.T) {
	srv, dir := newServer(t)
	do(t, http.MethodPut, srv.URL+"/files/a/b/c.txt", "x", true)
	do(t, http.MethodPut, srv.URL+"/files/d.txt", "x", true)
	if resp := do(t, http.MethodPost, srv.URL+"/reset", "", false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestEvents(t *testing.T) {
	srv, _ := newServer(t)
	resp := do(t, http.MethodGet, srv.URL+"/events", "", false)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	do(t, http.MethodPut, srv.URL+"/files/new.txt", "x", true)
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

	if resp := do(t, http.MethodGet, srv.URL+"/tmdb/3/movie/429?language=en", "", false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", resp.StatusCode)
	}
	resp := do(t, http.MethodGet, srv.URL+"/tmdb/3/movie/429?language=en", "", true)
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
		t.Errorf("sandbox bearer leaked upstream")
	}

	resp = do(t, http.MethodGet, srv.URL+"/upstreams", "", false)
	if body := readAll(t, resp.Body); !strings.Contains(body, `"tmdb": [
    "GET /3/movie/*"
  ]`) {
		t.Errorf("upstreams: %q", body)
	}

	seen = nil
	resp = do(t, http.MethodDelete, srv.URL+"/tmdb/3/movie/429", "", true)
	if body := readAll(t, resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "GET /3/movie/*") {
		t.Errorf("denied method: %d %q", resp.StatusCode, body)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/tmdb/3/account", "", true); resp.StatusCode != http.StatusForbidden {
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
	_, err := New(st, nil, Config{Token: "x", Upstreams: []proxy.Upstream{{Name: "files", URL: target}}})
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
		resp := do(t, http.MethodGet, srv.URL+p, "", false)
		if body := readAll(t, resp.Body); resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
	}
	resp := do(t, http.MethodGet, srv.URL+"/llms.txt", "", false)
	if body := readAll(t, resp.Body); strings.Contains(body, "{{") {
		t.Errorf("unexpanded placeholder in llms.txt")
	}
}

func TestDeadUpstreamExplains(t *testing.T) {
	target, _ := url.Parse("https://does-not-exist.invalid")
	srv, _ := newServer(t, proxy.Upstream{Name: "dead", URL: target, Allow: []proxy.Rule{{Method: "*", Path: "/*"}}})
	resp := do(t, http.MethodGet, srv.URL+"/dead/x", "", true)
	body := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "upstream dead:") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}
