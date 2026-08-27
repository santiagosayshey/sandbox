// Package decision holds the questions an agent asks and the answers a
// person gives. Everything lives in memory plus a directory for attached
// files; a reset or restart empties both.
package decision

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "image/gif"  // dimensions for attached images
	_ "image/jpeg" // dimensions for attached images
	_ "image/png"  // dimensions for attached images
)

// Type is the shape of a question, which fixes which verdicts answer it.
type Type string

// The types. Each maps to one card layout on the page.
const (
	Choose  Type = "choose"  // pick one (or min..max) of several options, or none
	Approve Type = "approve" // one option: accept, changes or reject
	Compare Type = "compare" // options "current" and "new": new, current or changes
	Confirm Type = "confirm" // no options: yes or no
	Ask     Type = "ask"     // no options: free text
)

// Verdicts is the closed list of answers each type accepts.
var Verdicts = map[Type][]string{
	Choose:  {"selected", "none"},
	Approve: {"accept", "changes", "reject"},
	Compare: {"new", "current", "changes"},
	Confirm: {"yes", "no"},
	Ask:     {"answered"},
}

// Revise reports whether a verdict asks the agent for another attempt.
func Revise(v string) bool { return v == "changes" || v == "none" }

// FileMeta describes an attached file.
type FileMeta struct {
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Option is one thing the person can pick.
type Option struct {
	ID    string    `json:"id"`
	Label string    `json:"label,omitempty"`
	File  string    `json:"file,omitempty"`
	Meta  *FileMeta `json:"meta,omitempty"`
}

// Attachment is context shown with the question but not pickable.
type Attachment struct {
	Name string    `json:"name"`
	File string    `json:"file"`
	Meta *FileMeta `json:"meta,omitempty"`
}

// Spec is what the agent posts.
type Spec struct {
	Title       string       `json:"title"`
	Prompt      string       `json:"prompt,omitempty"`
	Type        Type         `json:"type"`
	Options     []Option     `json:"options,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Min         int          `json:"min,omitempty"`
	Max         int          `json:"max,omitempty"`
	None        *bool        `json:"none,omitempty"`
	Supersedes  string       `json:"supersedes,omitempty"`
}

// Answer is what the person gave.
type Answer struct {
	Decision string    `json:"decision"`
	Seq      int       `json:"seq"`
	Verdict  string    `json:"verdict"`
	Selected []string  `json:"selected,omitempty"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
}

// Decision is a posted question, answered or not.
type Decision struct {
	ID          string       `json:"id"`
	Seq         int          `json:"seq"`
	Title       string       `json:"title"`
	Prompt      string       `json:"prompt,omitempty"`
	Type        Type         `json:"type"`
	Options     []Option     `json:"options,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Min         int          `json:"min"`
	Max         int          `json:"max"`
	None        bool         `json:"none"`
	Supersedes  string       `json:"supersedes,omitempty"`
	Created     time.Time    `json:"created"`
	Answer      *Answer      `json:"answer,omitempty"`
}

// Pending reports whether the decision still needs an answer.
func (d *Decision) Pending() bool { return d.Answer == nil }

// Verdicts lists the answers this decision accepts.
func (d *Decision) Verdicts() []string { return Verdicts[d.Type] }

// ErrInvalid marks a spec or answer the store refuses.
var ErrInvalid = errors.New("invalid")

// ErrNotFound marks an unknown decision or file.
var ErrNotFound = errors.New("not found")

// ErrAnswered marks a second answer to the same decision.
var ErrAnswered = errors.New("already answered")

// Store keeps decisions in memory and their files under a directory.
type Store struct {
	root *os.Root
	dir  string

	mu        sync.Mutex
	decisions []*Decision
	answers   []Answer
	nextID    int
	seq       int
	changed   chan struct{} // closed and replaced on every mutation
}

// Open opens dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, dir: dir, changed: make(chan struct{})}, nil
}

// Close releases the directory handle.
func (s *Store) Close() error { return s.root.Close() }

// Dir is the directory files are kept in.
func (s *Store) Dir() string { return s.dir }

func (s *Store) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Changed returns a channel that closes on the next mutation.
func (s *Store) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// FileSource provides an attached file by the name the spec refers to.
type FileSource func(name string) (io.Reader, int64, error)

// Create validates the spec, stores its files, and adds the decision.
// files is consulted for every option.file and attachment.file.
func (s *Store) Create(spec Spec, files FileSource, maxFile int64) (*Decision, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if spec.Supersedes != "" && s.find(spec.Supersedes) == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: supersedes unknown decision %q", ErrInvalid, spec.Supersedes)
	}
	s.nextID++
	id := fmt.Sprintf("d%d", s.nextID)
	s.mu.Unlock()

	d := &Decision{
		ID: id, Title: spec.Title, Prompt: spec.Prompt, Type: spec.Type,
		Options: spec.Options, Attachments: spec.Attachments,
		Min: spec.Min, Max: spec.Max, None: *spec.None, Supersedes: spec.Supersedes,
		Created: time.Now(),
	}
	if err := s.root.Mkdir(id, 0o755); err != nil {
		return nil, err
	}
	store := func(name string) (*FileMeta, error) {
		r, size, err := files(name)
		if err != nil {
			return nil, fmt.Errorf("%w: file %q: %v", ErrInvalid, name, err)
		}
		if size > maxFile {
			return nil, fmt.Errorf("%w: file %q exceeds %d bytes", ErrInvalid, name, maxFile)
		}
		return s.saveFile(id, name, r, maxFile)
	}
	seen := map[string]bool{}
	for i := range d.Options {
		if d.Options[i].File == "" {
			continue
		}
		if seen[d.Options[i].File] {
			return nil, fmt.Errorf("%w: file %q used twice", ErrInvalid, d.Options[i].File)
		}
		seen[d.Options[i].File] = true
		meta, err := store(d.Options[i].File)
		if err != nil {
			_ = s.root.RemoveAll(id)
			return nil, err
		}
		d.Options[i].Meta = meta
	}
	for i := range d.Attachments {
		if seen[d.Attachments[i].File] {
			return nil, fmt.Errorf("%w: file %q used twice", ErrInvalid, d.Attachments[i].File)
		}
		seen[d.Attachments[i].File] = true
		meta, err := store(d.Attachments[i].File)
		if err != nil {
			_ = s.root.RemoveAll(id)
			return nil, err
		}
		d.Attachments[i].Meta = meta
	}

	s.mu.Lock()
	s.seq++
	d.Seq = s.seq
	s.decisions = append(s.decisions, d)
	s.notify()
	s.mu.Unlock()
	return d, nil
}

func validateSpec(spec *Spec) error {
	spec.Title = strings.TrimSpace(spec.Title)
	if spec.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if _, ok := Verdicts[spec.Type]; !ok {
		return fmt.Errorf("%w: type must be one of choose, approve, compare, confirm, ask", ErrInvalid)
	}
	ids := map[string]bool{}
	for i := range spec.Options {
		o := &spec.Options[i]
		o.ID = strings.TrimSpace(o.ID)
		if o.ID == "" {
			return fmt.Errorf("%w: option %d has no id", ErrInvalid, i)
		}
		if ids[o.ID] {
			return fmt.Errorf("%w: option id %q repeated", ErrInvalid, o.ID)
		}
		ids[o.ID] = true
		if err := checkFileName(o.File); err != nil {
			return err
		}
	}
	for i := range spec.Attachments {
		a := &spec.Attachments[i]
		if a.File == "" {
			return fmt.Errorf("%w: attachment %d has no file", ErrInvalid, i)
		}
		if err := checkFileName(a.File); err != nil {
			return err
		}
		if a.Name == "" {
			a.Name = a.File
		}
	}
	n := len(spec.Options)
	switch spec.Type {
	case Choose:
		if n < 2 {
			return fmt.Errorf("%w: choose needs at least two options", ErrInvalid)
		}
		if spec.Min == 0 {
			spec.Min = 1
		}
		if spec.Max == 0 {
			spec.Max = spec.Min
		}
		if spec.Min < 1 || spec.Max < spec.Min || spec.Max > n {
			return fmt.Errorf("%w: min/max must satisfy 1 <= min <= max <= %d", ErrInvalid, n)
		}
		if spec.None == nil {
			t := true
			spec.None = &t
		}
	case Approve:
		if n != 1 {
			return fmt.Errorf("%w: approve needs exactly one option", ErrInvalid)
		}
	case Compare:
		if n != 2 || !ids["current"] || !ids["new"] {
			return fmt.Errorf("%w: compare needs exactly the options \"current\" and \"new\"", ErrInvalid)
		}
		for _, o := range spec.Options {
			if o.File == "" {
				return fmt.Errorf("%w: compare option %q needs a file", ErrInvalid, o.ID)
			}
		}
	case Confirm, Ask:
		if n != 0 {
			return fmt.Errorf("%w: %s takes no options", ErrInvalid, spec.Type)
		}
	}
	if spec.Type != Choose {
		spec.Min, spec.Max = 0, 0
		f := false
		spec.None = &f
	}
	return nil
}

func checkFileName(name string) error {
	if name == "" {
		return nil
	}
	if name != path.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("%w: file name %q must be a plain name", ErrInvalid, name)
	}
	return nil
}

func (s *Store) saveFile(id, name string, r io.Reader, max int64) (*FileMeta, error) {
	p := filepath.Join(id, name)
	f, err := s.root.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("%w: file %q exceeds %d bytes", ErrInvalid, name, max)
	}
	return s.meta(p, n)
}

func (s *Store) meta(p string, size int64) (*FileMeta, error) {
	f, err := s.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	m := &FileMeta{Size: size, Type: contentType(p, head[:n])}
	if strings.HasPrefix(m.Type, "image/") {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if cfg, _, err := image.DecodeConfig(f); err == nil {
				m.Width, m.Height = cfg.Width, cfg.Height
			}
		}
	}
	return m, nil
}

func contentType(p string, head []byte) string {
	if t := typeByExt(path.Ext(p)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

func typeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".yml", ".yaml":
		return "text/yaml; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json"
	case ".txt", ".log", ".cfg", ".ini", ".conf", ".toml", ".nfo", ".srt", ".csv":
		return "text/plain; charset=utf-8"
	}
	return ""
}

func (s *Store) find(id string) *Decision {
	for _, d := range s.decisions {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// Get returns a copy of one decision.
func (s *Store) Get(id string) (*Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return nil, ErrNotFound
	}
	c := *d
	return &c, nil
}

// List returns copies of every decision, newest first.
func (s *Store) List() []*Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Decision, 0, len(s.decisions))
	for i := len(s.decisions) - 1; i >= 0; i-- {
		c := *s.decisions[i]
		out = append(out, &c)
	}
	return out
}

// Counts returns how many decisions are pending and answered.
func (s *Store) Counts() (pending, answered int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.decisions {
		if d.Pending() {
			pending++
		} else {
			answered++
		}
	}
	return
}

// OpenFile returns an attached file and its info.
func (s *Store) OpenFile(id, name string) (*os.File, fs.FileInfo, error) {
	if err := checkFileName(name); err != nil || name == "" {
		return nil, nil, ErrNotFound
	}
	s.mu.Lock()
	d := s.find(id)
	s.mu.Unlock()
	if d == nil {
		return nil, nil, ErrNotFound
	}
	f, err := s.root.Open(filepath.Join(id, name))
	if err != nil {
		return nil, nil, ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}

// Respond records the person's answer.
func (s *Store) Respond(id, verdict string, selected []string, note string) (*Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return nil, ErrNotFound
	}
	if d.Answer != nil {
		return nil, ErrAnswered
	}
	verdict = strings.TrimSpace(verdict)
	note = strings.TrimSpace(note)
	ok := false
	for _, v := range Verdicts[d.Type] {
		if v == verdict {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: verdict must be one of %s", ErrInvalid, strings.Join(Verdicts[d.Type], ", "))
	}
	var sel []string
	switch {
	case d.Type == Choose && verdict == "selected":
		seen := map[string]bool{}
		for _, id := range selected {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			found := false
			for _, o := range d.Options {
				if o.ID == id {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: unknown option %q", ErrInvalid, id)
			}
			seen[id] = true
			sel = append(sel, id)
		}
		if len(sel) < d.Min || len(sel) > d.Max {
			return nil, fmt.Errorf("%w: select between %d and %d options", ErrInvalid, d.Min, d.Max)
		}
	case d.Type == Choose && verdict == "none":
		if !d.None {
			return nil, fmt.Errorf("%w: this decision does not allow \"none\"", ErrInvalid)
		}
	case d.Type == Ask:
		if note == "" {
			return nil, fmt.Errorf("%w: an answer is required", ErrInvalid)
		}
	case d.Type == Approve && verdict == "accept":
		sel = []string{d.Options[0].ID}
	case d.Type == Compare && verdict == "new", d.Type == Compare && verdict == "current":
		sel = []string{verdict}
	}
	s.seq++
	a := &Answer{Decision: id, Seq: s.seq, Verdict: verdict, Selected: sel, Note: note, At: time.Now()}
	d.Answer = a
	s.answers = append(s.answers, *a)
	s.notify()
	return a, nil
}

// Answers returns answers with a sequence after since, oldest first.
func (s *Store) Answers(since int) []Answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Answer
	for _, a := range s.answers {
		if a.Seq > since {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Wait returns answers after since, blocking until there is at least one
// or ctx ends. On timeout it returns an empty, non-nil slice.
func (s *Store) Wait(ctx context.Context, since int) []Answer {
	for {
		s.mu.Lock()
		var out []Answer
		for _, a := range s.answers {
			if a.Seq > since {
				out = append(out, a)
			}
		}
		ch := s.changed
		s.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return []Answer{}
		}
	}
}

// Reset forgets every decision and removes every file.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := s.root.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	s.decisions = nil
	s.answers = nil
	s.notify()
	return nil
}
