// Package server is the HTTP surface: the review page, the decision
// endpoints, the SSE stream and the upstream proxies.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	sandbox "github.com/santiagosayshey/sandbox"
	"github.com/santiagosayshey/sandbox/internal/decision"
	"github.com/santiagosayshey/sandbox/internal/proxy"
	"github.com/santiagosayshey/sandbox/internal/web"
)

// Config is everything the handler needs beyond the store.
type Config struct {
	MaxUpload int64
	PublicURL string
	Version   string
	Upstreams []proxy.Upstream
}

// reserved are the top-level route names an upstream may not shadow.
var reserved = map[string]bool{
	"decisions": true, "answers": true, "events": true, "reset": true,
	"llms.txt": true, "upstreams": true, "healthz": true, "version": true, "static": true,
}

const (
	maxTextPreview = 256 << 10
	maxWait        = 300 * time.Second
)

type server struct {
	st  *decision.Store
	cfg Config
	tpl *template.Template
}

// New builds the handler. It fails if an upstream name collides with a
// built-in route.
func New(st *decision.Store, cfg Config) (http.Handler, error) {
	for _, u := range cfg.Upstreams {
		if reserved[u.Name] {
			return nil, fmt.Errorf("upstream name %q collides with a built-in route", u.Name)
		}
	}
	s := &server{st: st, cfg: cfg}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"size":     humanSize,
		"filekind": fileKind,
		"aspect":   aspect,
		"text":     s.textOf,
		"verdict":  verdictLabel,
		"media":    mediaArgs,
		"btn":      btnArgs,
	}).ParseFS(web.Assets, "index.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /decisions", s.listDecisions)
	mux.HandleFunc("POST /decisions", s.createDecision)
	mux.HandleFunc("GET /decisions/{id}", s.getDecision)
	mux.HandleFunc("POST /decisions/{id}/answer", s.answer)
	mux.HandleFunc("GET /decisions/{id}/files/{name}", s.file)
	mux.HandleFunc("GET /answers", s.answers)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("POST /reset", s.reset)
	mux.HandleFunc("GET /llms.txt", s.llms)
	mux.HandleFunc("GET /upstreams", s.upstreams)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, cfg.Version+"\n") })
	static, _ := fs.Sub(web.Assets, ".")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	for _, u := range cfg.Upstreams {
		h := http.StripPrefix("/"+u.Name, u.Handler())
		mux.Handle("/"+u.Name+"/", h)
		mux.Handle("/"+u.Name, h)
	}
	return mux, nil
}

// pageData is what the page and the queue fragment render.
type pageData struct {
	Host      string
	Version   string
	Pending   []*decision.Decision
	Answered  []*decision.Decision
	Upstreams []proxy.Upstream
}

func (s *server) pageData(r *http.Request) pageData {
	host := ""
	if r != nil {
		host = r.Host
	}
	if u, err := url.Parse(s.publicURL()); err == nil && u.Host != "" {
		host = u.Host
	}
	pd := pageData{Host: host, Version: s.cfg.Version, Upstreams: s.cfg.Upstreams}
	for _, d := range s.st.List() {
		if d.Pending() {
			pd.Pending = append(pd.Pending, d)
		} else {
			pd.Answered = append(pd.Answered, d)
		}
	}
	return pd
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	s.render(w, "page", s.pageData(r))
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// textOf returns the beginning of a text attachment for inline display.
func (s *server) textOf(d *decision.Decision, file string) string {
	f, _, err := s.st.OpenFile(d.ID, file)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxTextPreview))
	return string(b)
}

func (s *server) listDecisions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.st.List())
}

func (s *server) getDecision(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Get(r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// createDecision accepts multipart/form-data with a "spec" field holding
// the JSON and one part per attached file, matched by filename, or a bare
// JSON body when no files are needed.
func (s *server) createDecision(w http.ResponseWriter, r *http.Request) {
	var spec decision.Spec
	parts := map[string]*multipart.FileHeader{}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "multipart/form-data":
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		raw := r.FormValue("spec")
		if raw == "" {
			http.Error(w, `multipart body needs a "spec" field holding the JSON`, http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			http.Error(w, "spec: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				parts[path.Base(fh.Filename)] = fh
			}
		}
	default:
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&spec); err != nil {
			http.Error(w, "spec: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	files := func(name string) (io.Reader, int64, error) {
		fh, ok := parts[name]
		if !ok {
			return nil, 0, errors.New("not in the request")
		}
		f, err := fh.Open()
		if err != nil {
			return nil, 0, err
		}
		return f, fh.Size, nil
	}
	d, err := s.st.Create(spec, files, s.cfg.MaxUpload)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// answer accepts the page's form (verdict, selected[], note) or JSON.
func (s *server) answer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var verdict, note string
	var selected []string
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		var body struct {
			Verdict  string   `json:"verdict"`
			Selected []string `json:"selected"`
			Note     string   `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		verdict, selected, note = body.Verdict, body.Selected, body.Note
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		verdict, note = r.Form.Get("verdict"), r.Form.Get("note")
		selected = r.Form["selected"]
	}
	a, err := s.st.Respond(id, verdict, selected, note)
	if err != nil {
		httpError(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		// The SSE stream re-renders the queue; nothing to swap here.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) file(w http.ResponseWriter, r *http.Request) {
	f, info, err := s.st.OpenFile(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// answers returns answers after ?since, long-polling for up to ?wait
// seconds when there are none yet.
func (s *server) answers(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait < 0 {
		wait = 0
	}
	if d := time.Duration(wait) * time.Second; d > maxWait {
		wait = int(maxWait / time.Second)
	}
	var got []decision.Answer
	if wait == 0 {
		got = s.st.Answers(since)
		if got == nil {
			got = []decision.Answer{}
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(wait)*time.Second)
		defer cancel()
		got = s.st.Wait(ctx, since)
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	send := func() bool {
		var buf bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&buf, "queue", s.pageData(r)); err != nil {
			log.Printf("render queue: %v", err)
			return true
		}
		if _, err := io.WriteString(w, "event: queue\n"+sseData(buf.String())+"\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for {
		// Take the change channel before rendering, so a mutation that
		// lands while rendering still wakes the next wait.
		ch := s.st.Changed()
		if !send() {
			return
		}
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				break wait
			case <-keepalive.C:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// sseData formats a multi-line payload as SSE data lines.
func sseData(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	if err := s.st.Reset(); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) llms(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := strings.ReplaceAll(sandbox.LLMs, "{{PUBLIC_URL}}", s.publicURL())
	var names []string
	for _, u := range s.cfg.Upstreams {
		names = append(names, u.Name)
	}
	if len(names) == 0 {
		names = []string{"none configured"}
	}
	body = strings.ReplaceAll(body, "{{UPSTREAMS}}", strings.Join(names, ", "))
	io.WriteString(w, body)
}

func (s *server) publicURL() string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return "http://localhost:8080"
}

// upstreams lists each upstream and the requests it allows.
func (s *server) upstreams(w http.ResponseWriter, _ *http.Request) {
	out := map[string][]string{}
	for _, u := range s.cfg.Upstreams {
		rules := []string{}
		for _, r := range u.Allow {
			rules = append(rules, r.String())
		}
		out[u.Name] = rules
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, decision.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, decision.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, decision.ErrAnswered):
		http.Error(w, "already answered", http.StatusConflict)
	default:
		log.Printf("error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// mediaView is what the "media" template renders.
type mediaView struct {
	D    *decision.Decision
	File string
	Name string
	Meta *decision.FileMeta
}

func mediaArgs(d *decision.Decision, file, name string, meta *decision.FileMeta) mediaView {
	if name == "" {
		name = file
	}
	return mediaView{D: d, File: file, Name: name, Meta: meta}
}

// btnView is what the "verdictbtn" template renders.
type btnView struct {
	D       *decision.Decision
	Verdict string
	Label   string
	Class   string
}

func btnArgs(d *decision.Decision, verdict, label, class string) btnView {
	return btnView{D: d, Verdict: verdict, Label: label, Class: class}
}

// fileKind maps a content type to the preview the page uses.
func fileKind(m *decision.FileMeta) string {
	if m == nil {
		return "none"
	}
	base, _, _ := mime.ParseMediaType(m.Type)
	switch {
	case strings.HasPrefix(base, "image/"):
		return "image"
	case strings.HasPrefix(base, "audio/"):
		return "audio"
	case strings.HasPrefix(base, "video/"):
		return "video"
	case strings.HasPrefix(base, "text/"), base == "application/json", base == "application/xml":
		return "text"
	}
	return "other"
}

// aspect names common poster and backdrop ratios, else the raw ratio.
func aspect(m *decision.FileMeta) string {
	if m == nil || m.Width == 0 || m.Height == 0 {
		return ""
	}
	r := float64(m.Width) / float64(m.Height)
	for _, c := range []struct {
		name string
		r    float64
	}{{"2:3", 2.0 / 3}, {"16:9", 16.0 / 9}, {"1:1", 1}, {"3:2", 1.5}, {"4:3", 4.0 / 3}, {"21:9", 21.0 / 9}} {
		if r > c.r*0.985 && r < c.r*1.015 {
			return c.name
		}
	}
	return fmt.Sprintf("%.2f:1", r)
}

var verdictLabels = map[string]string{
	"selected": "chose", "none": "none of these", "accept": "use it", "changes": "changes",
	"reject": "no", "new": "use new", "current": "keep current", "yes": "yes", "no": "no", "answered": "answered",
}

func verdictLabel(v string) string {
	if l, ok := verdictLabels[v]; ok {
		return l
	}
	return v
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
