// Package store is a filesystem rooted at one directory. Every operation
// goes through os.Root, so a path can never escape the directory no matter
// what the caller sends.
package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrInvalidPath is returned for empty paths or paths that try to escape.
var ErrInvalidPath = errors.New("invalid path")

// ErrTooLarge is returned when a Put exceeds the size limit.
var ErrTooLarge = errors.New("file exceeds size limit")

// Store is a directory and the rooted handle used to reach into it.
type Store struct {
	dir  string
	root *os.Root
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
	return &Store{dir: dir, root: root}, nil
}

// Close releases the root handle.
func (s *Store) Close() error { return s.root.Close() }

// Dir is the directory the store is rooted at.
func (s *Store) Dir() string { return s.dir }

// Clean normalises a client-supplied path to a relative, slash-separated
// path with no empty, dot or dot-dot segments. It rejects the root itself.
func Clean(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "\x00") {
		return "", ErrInvalidPath
	}
	// A ".." anywhere is a client bug, not something to collapse quietly.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", ErrInvalidPath
		}
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" || c == "." {
		return "", ErrInvalidPath
	}
	return c, nil
}

// Put writes r to p, creating parent directories. It returns true if the
// file did not exist before. Writes larger than max bytes fail with
// ErrTooLarge and leave nothing behind.
func (s *Store) Put(p string, r io.Reader, max int64) (created bool, err error) {
	p, err = Clean(p)
	if err != nil {
		return false, err
	}
	if dir := path.Dir(p); dir != "." {
		if err := s.root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return false, err
		}
	}
	_, statErr := s.root.Stat(filepath.FromSlash(p))
	created = errors.Is(statErr, fs.ErrNotExist)

	tmp := p + ".sandbox-upload"
	f, err := s.root.OpenFile(filepath.FromSlash(tmp), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if err != nil {
		_ = s.root.Remove(filepath.FromSlash(tmp))
		return false, err
	}
	if err := s.root.Rename(filepath.FromSlash(tmp), filepath.FromSlash(p)); err != nil {
		_ = s.root.Remove(filepath.FromSlash(tmp))
		return false, err
	}
	return created, nil
}

// Open returns the file at p for reading, with its info. Directories are
// rejected with fs.ErrInvalid.
func (s *Store) Open(p string) (*os.File, fs.FileInfo, error) {
	p, err := Clean(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := s.root.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", p, fs.ErrInvalid)
	}
	return f, info, nil
}

// Stat returns info for p.
func (s *Store) Stat(p string) (fs.FileInfo, error) {
	p, err := Clean(p)
	if err != nil {
		return nil, err
	}
	return s.root.Stat(filepath.FromSlash(p))
}

// Delete removes p; directories are removed recursively.
func (s *Store) Delete(p string) error {
	p, err := Clean(p)
	if err != nil {
		return err
	}
	return s.root.RemoveAll(filepath.FromSlash(p))
}

// Reset removes every entry under the root.
func (s *Store) Reset() error {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := s.root.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// Node is one entry in the tree.
type Node struct {
	Name     string
	Path     string // slash-separated, relative to the root; "" for the root
	Dir      bool
	Size     int64
	ModTime  time.Time
	Children []Node
}

// Tree returns the whole directory tree, directories first, sorted by name.
func (s *Store) Tree() (Node, error) {
	return s.node(".", "")
}

func (s *Store) node(fsPath, rel string) (Node, error) {
	info, err := fs.Stat(s.root.FS(), fsPath)
	if err != nil {
		return Node{}, err
	}
	n := Node{Name: path.Base(rel), Path: rel, Dir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime()}
	if rel == "" {
		n.Name = ""
	}
	if !n.Dir {
		return n, nil
	}
	entries, err := fs.ReadDir(s.root.FS(), fsPath)
	if err != nil {
		return n, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		childRel := e.Name()
		if rel != "" {
			childRel = rel + "/" + e.Name()
		}
		child, err := s.node(path.Join(fsPath, e.Name()), childRel)
		if err != nil {
			return n, err
		}
		n.Children = append(n.Children, child)
	}
	return n, nil
}

// Stats counts files and sums their sizes across the tree.
func (n Node) Stats() (files int, bytes int64) {
	if !n.Dir {
		return 1, n.Size
	}
	for _, c := range n.Children {
		f, b := c.Stats()
		files += f
		bytes += b
	}
	return files, bytes
}
