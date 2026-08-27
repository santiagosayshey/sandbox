// Package watch watches a directory tree and tells subscribers when
// anything under it changes. Bursts of events are collapsed into one
// notification.
package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches one directory tree.
type Watcher struct {
	fw       *fsnotify.Watcher
	debounce time.Duration
	mu       sync.Mutex
	subs     map[chan struct{}]struct{}
	done     chan struct{}
}

// New starts watching dir and every directory below it.
func New(dir string, debounce time.Duration) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fw: fw, debounce: debounce, subs: map[chan struct{}]struct{}{}, done: make(chan struct{})}
	if err := w.addTree(dir); err != nil {
		fw.Close()
		return nil, err
	}
	go w.run()
	return w, nil
}

func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a directory removed mid-walk is not an error
		}
		if d.IsDir() {
			return w.fw.Add(p)
		}
		return nil
	})
}

func (w *Watcher) run() {
	var timer *time.Timer
	var fire <-chan time.Time
	for {
		select {
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.addTree(ev.Name)
				}
			}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
			} else {
				timer.Reset(w.debounce)
			}
			fire = timer.C
		case <-fire:
			fire = nil
			w.broadcast()
		case _, ok := <-w.fw.Errors:
			if !ok {
				return
			}
		case <-w.done:
			return
		}
	}
}

func (w *Watcher) broadcast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber has a pending notification already
		}
	}
}

// Subscribe returns a channel that receives a value after each burst of
// changes, and a function that unsubscribes.
func (w *Watcher) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subs, ch)
		w.mu.Unlock()
	}
}

// Close stops the watcher.
func (w *Watcher) Close() error {
	close(w.done)
	return w.fw.Close()
}
