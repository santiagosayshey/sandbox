// Package server is the HTTP surface: the live page, file endpoints, the
// SSE stream and the upstream proxies.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	sandbox "github.com/santiagosayshey/sandbox"
	"github.com/santiagosayshey/sandbox/internal/proxy"
	"github.com/santiagosayshey/sandbox/internal/store"
	"github.com/santiagosayshey/sandbox/internal/watch"
	"github.com/santiagosayshey/sandbox/internal/web"
)

// Config is everything the handler needs beyond the store and watcher.
type Config struct {
	MaxUpload int64
	PublicURL string
	Version   string
	Upstreams []proxy.Upstream
}

// reserved are the top-level route names an upstream may not shadow.
var reserved = map[string]bool{
	"files": true, "tree": true, "preview": true, "events": true, "reset": true,
	"llms.txt": true, "upstreams": true, "healthz": true, "version": true, "static": true,
}

const maxTextPreview = 256 << 10

type server struct {
	st  *store.Store
	w   *watch.Watcher
	cfg Config
	tpl *template.Template
}

// New builds the handler. It fails if an upstream name collides with a
// built-in route.
func New(st *store.Store, w *watch.Watcher, cfg Config) (http.Handler, error) {
	for _, u := range cfg.Upstreams {
		if reserved[u.Name] {
			return nil, fmt.Errorf("upstream name %q collides with a built-in route", u.Name)
		}
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"size":       humanSize,
		"pathEscape": escapePath,
	}).ParseFS(web.Assets, "index.html")
	if err != nil {
		return nil, err
	}
	s := &server{st: st, w: w, cfg: cfg, tpl: tpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /tree", s.tree)
	mux.HandleFunc("GET /preview/{path...}", s.preview)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /files/{path...}", s.getFile)
	mux.HandleFunc("PUT /files/{path...}", s.putFile)
	mux.HandleFunc("DELETE /files/{path...}", s.deleteFile)
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

// treeData is what the tree fragment renders.
type treeData struct {
	Node  store.Node
	Files int
	Bytes int64
}

func (s *server) treeData() (treeData, error) {
	n, err := s.st.Tree()
	if err != nil {
		return treeData{}, err
	}
	files, bytes := n.Stats()
	return treeData{Node: n, Files: files, Bytes: bytes}, nil
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	td, err := s.treeData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	host := r.Host
	if s.cfg.PublicURL != "" {
		if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Host != "" {
			host = u.Host
		}
	}
	s.render(w, "page", map[string]any{
		"Dir":     s.st.Dir(),
		"Host":    host,
		"Version": s.cfg.Version,
		"Tree":    td,
	})
}

func (s *server) tree(w http.ResponseWriter, r *http.Request) {
	td, err := s.treeData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The CLI asks for text; the page and SSE stream get the fragment.
	if strings.Contains(r.Header.Get("Accept"), "text/plain") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var b strings.Builder
		writePlain(&b, td.Node, "")
		fmt.Fprintf(&b, "%s, %s\n", plural(td.Files, "file"), humanSize(td.Bytes))
		io.WriteString(w, b.String())
		return
	}
	s.render(w, "tree", td)
}

// writePlain renders the tree as one line per entry, directories with a
// trailing slash, files with their size.
func writePlain(b *strings.Builder, n store.Node, indent string) {
	for _, c := range n.Children {
		if c.Dir {
			fmt.Fprintf(b, "%s%s/\n", indent, c.Name)
			writePlain(b, c, indent+"  ")
		} else {
			fmt.Fprintf(b, "%s%s  (%s)\n", indent, c.Name, humanSize(c.Size))
		}
	}
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// previewData is what the preview fragment renders.
type previewData struct {
	Path      string
	URL       string
	Type      string
	Kind      string // image, audio, video, text, other
	Size      int64
	ModTime   time.Time
	Text      string
	Truncated bool
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	p, err := store.Clean(r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, info, err := s.st.Open(p)
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()

	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	ctype := contentType(p, head)
	pd := previewData{
		Path:    p,
		URL:     "/files/" + escapePath(p),
		Type:    ctype,
		Kind:    kind(ctype),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if pd.Kind == "text" {
		rest, _ := io.ReadAll(io.LimitReader(f, maxTextPreview-int64(len(head))))
		pd.Text = string(append(head, rest...))
		pd.Truncated = info.Size() > maxTextPreview
	}
	s.render(w, "preview", pd)
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

	ch, unsub := s.w.Subscribe()
	defer unsub()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	send := func() bool {
		td, err := s.treeData()
		if err != nil {
			return true
		}
		var buf bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&buf, "tree", td); err != nil {
			return true
		}
		if _, err := io.WriteString(w, "event: tree\n"+sseData(buf.String())+"\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
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

func (s *server) getFile(w http.ResponseWriter, r *http.Request) {
	p, err := store.Clean(r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, info, err := s.st.Open(p)
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, path.Base(p), info.ModTime(), f)
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request) {
	p, err := store.Clean(r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength > s.cfg.MaxUpload {
		http.Error(w, "file exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	created, err := s.st.Put(p, r.Body, s.cfg.MaxUpload)
	if err != nil {
		httpError(w, err)
		return
	}
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	p, err := store.Clean(r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.st.Stat(p); err != nil {
		httpError(w, err)
		return
	}
	if err := s.st.Delete(p); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	body = strings.ReplaceAll(body, "{{UPSTREAMS}}", strings.Join(names, ", "))
	io.WriteString(w, body)
}

func (s *server) publicURL() string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return "http://sandbox.orion"
}

// upstreams lists each upstream and the requests it allows, e.g.
// {"tmdb":["GET /3/movie/*"],"plex":["GET /identity"]}.
func (s *server) upstreams(w http.ResponseWriter, _ *http.Request) {
	out := map[string][]string{}
	for _, u := range s.cfg.Upstreams {
		rules := []string{}
		for _, r := range u.Allow {
			rules = append(rules, r.String())
		}
		out[u.Name] = rules
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidPath):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrInvalid):
		http.Error(w, "is a directory", http.StatusBadRequest)
	case errors.Is(err, store.ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	default:
		log.Printf("error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func contentType(p string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

func kind(ctype string) string {
	base, _, _ := mime.ParseMediaType(ctype)
	switch {
	case strings.HasPrefix(base, "image/"):
		return "image"
	case strings.HasPrefix(base, "audio/"):
		return "audio"
	case strings.HasPrefix(base, "video/"):
		return "video"
	case strings.HasPrefix(base, "text/"), base == "application/json", base == "application/xml",
		base == "application/x-yaml", base == "application/yaml", base == "application/toml":
		return "text"
	}
	return "other"
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
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

func init() {
	for ext, t := range map[string]string{
		".yml": "text/yaml", ".yaml": "text/yaml", ".md": "text/markdown", ".toml": "text/plain",
		".ini": "text/plain", ".cfg": "text/plain", ".conf": "text/plain", ".log": "text/plain",
		".nfo": "text/plain", ".srt": "text/plain",
	} {
		if mime.TypeByExtension(ext) == "" {
			_ = mime.AddExtensionType(ext, t)
		}
	}
}
