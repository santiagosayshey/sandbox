package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santiagosayshey/sandbox/internal/decision"
	"github.com/santiagosayshey/sandbox/internal/proxy"
)

func newServer(t *testing.T, upstreams ...proxy.Upstream) *httptest.Server {
	t.Helper()
	st, err := decision.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, err := New(st, Config{MaxUpload: 1 << 20, Version: "test", Upstreams: upstreams})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// post sends a multipart decision: spec JSON plus named files.
func post(t *testing.T, srv *httptest.Server, spec string, files map[string][]byte) (*http.Response, decision.Decision) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("spec", spec)
	for name, b := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(b)
	}
	mw.Close()
	resp, err := http.Post(srv.URL+"/decisions", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var d decision.Decision
	if resp.StatusCode == http.StatusCreated {
		_ = json.NewDecoder(resp.Body).Decode(&d)
	}
	return resp, d
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCreateAndRenderChoose(t *testing.T) {
	srv := newServer(t)
	resp, d := post(t, srv, `{"title":"Poster","prompt":"pick","type":"choose",
		"options":[{"id":"a","label":"A","file":"a.png"},{"id":"b","file":"b.png"}],
		"attachments":[{"name":"current","file":"cur.png"}]}`,
		map[string][]byte{"a.png": pngBytes(20, 30), "b.png": pngBytes(16, 9), "cur.png": pngBytes(2, 3)})
	if resp.StatusCode != http.StatusCreated || d.ID != "d1" {
		t.Fatalf("create: %d %+v", resp.StatusCode, d)
	}

	page := readAll(t, get(t, srv.URL+"/").Body)
	for _, want := range []string{`data-id="d1"`, `data-option="a"`, `data-option="b"`, "/decisions/d1/files/a.png", "20×30 · 2:3", "16×9 · 16:9", "Reference", "None of these", "Use selected"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	f := get(t, srv.URL+"/decisions/d1/files/b.png")
	if f.StatusCode != http.StatusOK || f.Header.Get("Content-Type") != "image/png" {
		t.Errorf("file: %d %s", f.StatusCode, f.Header.Get("Content-Type"))
	}
	if get(t, srv.URL+"/decisions/d1/files/nope.png").StatusCode != http.StatusNotFound {
		t.Error("missing file should 404")
	}
	if get(t, srv.URL+"/decisions/d9").StatusCode != http.StatusNotFound {
		t.Error("missing decision should 404")
	}
}

func get(t *testing.T, u string) *http.Response {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestCreateErrors(t *testing.T) {
	srv := newServer(t)
	if resp, _ := post(t, srv, `{"title":"x","type":"choose","options":[{"id":"a","file":"a.png"},{"id":"b","file":"b.png"}]}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing files: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv, `{"title":"x","type":"vote"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad type: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv, `not json`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: %d", resp.StatusCode)
	}
	resp, err := http.Post(srv.URL+"/decisions", "application/json", strings.NewReader(`{"title":"ok?","type":"confirm"}`))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Errorf("bare json confirm: %v %d", err, resp.StatusCode)
	}
}

func TestAnswerFormAndJSON(t *testing.T) {
	srv := newServer(t)
	post(t, srv, `{"title":"Poster","type":"choose","options":[{"id":"a","file":"a.png"},{"id":"b","file":"b.png"}]}`,
		map[string][]byte{"a.png": pngBytes(2, 3), "b.png": pngBytes(2, 3)})
	post(t, srv, `{"title":"ok?","type":"confirm"}`, nil)

	// The page posts a form; selected comes as repeated fields.
	resp, err := http.PostForm(srv.URL+"/decisions/d1/answer", url.Values{"verdict": {"selected"}, "selected": {"b"}, "note": {"blue"}})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("form answer: %v %d", err, resp.StatusCode)
	}
	var a decision.Answer
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if a.Verdict != "selected" || a.Selected[0] != "b" || a.Note != "blue" {
		t.Fatalf("answer: %+v", a)
	}
	// Second answer conflicts.
	resp, _ = http.PostForm(srv.URL+"/decisions/d1/answer", url.Values{"verdict": {"none"}})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second answer: %d", resp.StatusCode)
	}
	// Agents may answer with JSON too (handy for tests and scripts).
	resp, _ = http.Post(srv.URL+"/decisions/d2/answer", "application/json", strings.NewReader(`{"verdict":"maybe"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad verdict: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/decisions/d2/answer", "application/json", strings.NewReader(`{"verdict":"yes"}`))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("json answer: %d", resp.StatusCode)
	}

	page := readAll(t, get(t, srv.URL+"/").Body)
	if !strings.Contains(page, "Nothing to decide") || !strings.Contains(page, "<b>chose</b> · b · “blue”") || !strings.Contains(page, "<b>yes</b>") {
		t.Errorf("answered page:\n%s", page)
	}
	list := readAll(t, get(t, srv.URL+"/decisions").Body)
	if !strings.Contains(list, `"verdict": "yes"`) {
		t.Errorf("list: %s", list)
	}
}

func TestAnswersLongPoll(t *testing.T) {
	srv := newServer(t)
	post(t, srv, `{"title":"ok?","type":"confirm"}`, nil)

	// No wait: immediate empty list, not null.
	if body := readAll(t, get(t, srv.URL+"/answers?since=0").Body); strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty answers: %q", body)
	}

	start := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		http.Post(srv.URL+"/decisions/d1/answer", "application/json", strings.NewReader(`{"verdict":"no","note":"later"}`))
	}()
	resp := get(t, srv.URL+"/answers?since=0&wait=5")
	var got []decision.Answer
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 1 || got[0].Verdict != "no" || got[0].Decision != "d1" {
		t.Fatalf("long poll: %+v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("long poll did not return promptly")
	}
	if body := readAll(t, get(t, srv.URL+"/answers?since="+itoa(got[0].Seq)+"&wait=1").Body); strings.TrimSpace(body) != "[]" {
		t.Errorf("since should exclude delivered: %q", body)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestEventsAndReset(t *testing.T) {
	srv := newServer(t)
	resp := get(t, srv.URL+"/events")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	post(t, srv, `{"title":"Fresh question","type":"confirm"}`, nil)
	buf := make([]byte, 16384)
	var got string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(got, "Fresh question") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "event: queue\n") || !strings.Contains(got, "Fresh question") {
		t.Fatalf("stream:\n%s", got)
	}
	r, _ := http.Post(srv.URL+"/reset", "", nil)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: %d", r.StatusCode)
	}
	if get(t, srv.URL+"/decisions/d1").StatusCode != http.StatusNotFound {
		t.Error("reset should forget decisions")
	}
}

func TestProxyInjectsCredentialAndEnforcesAllow(t *testing.T) {
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
	srv := newServer(t, u)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tmdb/3/movie/429?language=en", nil)
	req.Header.Set("Authorization", "Bearer stray")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK || readAll(t, resp.Body) != `{"ok":true}` {
		t.Fatalf("proxied: %d", resp.StatusCode)
	}
	if seen.URL.Path != "/3/movie/429" {
		t.Errorf("path %q", seen.URL.Path)
	}
	if q := seen.URL.Query(); q.Get("api_key") != "qsecret" || q.Get("language") != "en" {
		t.Errorf("query %q", seen.URL.RawQuery)
	}
	if seen.Header.Get("X-Api-Key") != "secret" || seen.Header.Get("Authorization") != "" {
		t.Errorf("headers: %v", seen.Header)
	}

	seen = nil
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/tmdb/3/movie/429", nil)
	resp, _ = http.DefaultClient.Do(req)
	if body := readAll(t, resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "GET /3/movie/*") {
		t.Errorf("denied: %d %q", resp.StatusCode, body)
	}
	if seen != nil {
		t.Error("denied request reached the upstream")
	}
	if body := readAll(t, get(t, srv.URL+"/upstreams").Body); !strings.Contains(body, `"GET /3/movie/*"`) {
		t.Errorf("upstreams: %q", body)
	}
	if get(t, srv.URL+"/nope/x").StatusCode != http.StatusNotFound {
		t.Error("unknown upstream should 404")
	}
}

func TestUpstreamCannotShadowRoute(t *testing.T) {
	st, _ := decision.Open(t.TempDir())
	defer st.Close()
	target, _ := url.Parse("http://example.com")
	if _, err := New(st, Config{Upstreams: []proxy.Upstream{{Name: "decisions", URL: target}}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPlainRoutes(t *testing.T) {
	srv := newServer(t)
	for p, want := range map[string]string{
		"/healthz":  "ok\n",
		"/version":  "test\n",
		"/llms.txt": "# sandbox",
		"/":         "<title>sandbox</title>",
	} {
		resp := get(t, srv.URL+p)
		if body := readAll(t, resp.Body); resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
	}
	if body := readAll(t, get(t, srv.URL+"/llms.txt").Body); strings.Contains(body, "{{") {
		t.Error("unexpanded placeholder in llms.txt")
	}
}
