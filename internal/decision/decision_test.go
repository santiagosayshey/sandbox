package decision

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"
)

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

func files(m map[string][]byte) FileSource {
	return func(name string) (io.Reader, int64, error) {
		b, ok := m[name]
		if !ok {
			return nil, 0, errors.New("missing")
		}
		return bytes.NewReader(b), int64(len(b)), nil
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestChooseLifecycle(t *testing.T) {
	s := open(t)
	d, err := s.Create(Spec{
		Title: "Poster", Type: Choose,
		Options:     []Option{{ID: "a", File: "a.png"}, {ID: "b", File: "b.png"}},
		Attachments: []Attachment{{File: "cur.png"}},
	}, files(map[string][]byte{"a.png": pngBytes(20, 30), "b.png": pngBytes(40, 60), "cur.png": pngBytes(2, 3)}), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "d1" || d.Min != 1 || d.Max != 1 || !d.None || !d.Pending() {
		t.Fatalf("defaults: %+v", d)
	}
	if m := d.Options[1].Meta; m == nil || m.Width != 40 || m.Height != 60 || m.Type != "image/png" {
		t.Fatalf("meta: %+v", d.Options[1].Meta)
	}
	if d.Attachments[0].Name != "cur.png" {
		t.Fatalf("attachment name defaulted to %q", d.Attachments[0].Name)
	}
	f, info, err := s.OpenFile("d1", "b.png")
	if err != nil || info.Size() != int64(len(pngBytes(40, 60))) {
		t.Fatalf("open file: %v", err)
	}
	f.Close()

	if _, err := s.Respond("d1", "selected", []string{"zzz"}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown option: %v", err)
	}
	if _, err := s.Respond("d1", "accept", nil, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong verdict: %v", err)
	}
	a, err := s.Respond("d1", "selected", []string{"b"}, "the blue one")
	if err != nil || a.Selected[0] != "b" || a.Note != "the blue one" || a.Seq != 2 {
		t.Fatalf("answer: %+v %v", a, err)
	}
	if _, err := s.Respond("d1", "none", nil, ""); !errors.Is(err, ErrAnswered) {
		t.Errorf("second answer: %v", err)
	}
	got, _ := s.Get("d1")
	if got.Pending() || got.Answer.Verdict != "selected" {
		t.Fatalf("stored answer: %+v", got.Answer)
	}
	if p, a := s.Counts(); p != 0 || a != 1 {
		t.Fatalf("counts %d %d", p, a)
	}
}

func TestSpecValidation(t *testing.T) {
	s := open(t)
	one := files(map[string][]byte{"x.png": pngBytes(1, 1), "y.png": pngBytes(1, 1)})
	bad := []Spec{
		{Type: Choose, Options: []Option{{ID: "a", File: "x.png"}, {ID: "b", File: "y.png"}}}, // no title
		{Title: "t", Type: "vote"}, // unknown kind
		{Title: "t", Type: Choose, Options: []Option{{ID: "a", File: "x.png"}}},                                   // one option
		{Title: "t", Type: Choose, Options: []Option{{ID: "a", File: "x.png"}, {ID: "a", File: "y.png"}}},         // dup id
		{Title: "t", Type: Choose, Options: []Option{{ID: "a", File: "x.png"}, {ID: "b", File: "x.png"}}},         // dup file
		{Title: "t", Type: Choose, Options: []Option{{ID: "a", File: "x.png"}, {ID: "b", File: "y.png"}}, Min: 3}, // min > n
		{Title: "t", Type: Approve, Options: []Option{{ID: "a", File: "x.png"}, {ID: "b", File: "y.png"}}},        // two
		{Title: "t", Type: Compare, Options: []Option{{ID: "a", File: "x.png"}, {ID: "b", File: "y.png"}}},        // wrong ids
		{Title: "t", Type: Confirm, Options: []Option{{ID: "a"}}},                                                 // options
		{Title: "t", Type: Approve, Options: []Option{{ID: "a", File: "../x.png"}}},                               // path
		{Title: "t", Type: Approve, Options: []Option{{ID: "a", File: "missing.png"}}},                            // no file
		{Title: "t", Type: Confirm, Supersedes: "d99"},                                                            // unknown
	}
	for i, spec := range bad {
		if _, err := s.Create(spec, one, 1<<20); !errors.Is(err, ErrInvalid) {
			t.Errorf("spec %d: want ErrInvalid, got %v", i, err)
		}
	}
	if p, _ := s.Counts(); p != 0 {
		t.Fatalf("rejected specs were stored")
	}
	if d, err := s.Create(Spec{Title: "ok?", Type: Confirm, Min: 5, Max: 9}, one, 1<<20); err != nil || d.Min != 0 || d.None {
		t.Fatalf("confirm: %+v %v", d, err)
	}
}

func TestOtherKinds(t *testing.T) {
	s := open(t)
	src := files(map[string][]byte{"x.png": pngBytes(1, 1), "y.png": pngBytes(1, 1)})
	ap, _ := s.Create(Spec{Title: "a", Type: Approve, Options: []Option{{ID: "theme", File: "x.png"}}}, src, 1<<20)
	cp, _ := s.Create(Spec{Title: "c", Type: Compare, Options: []Option{{ID: "current", File: "x.png"}, {ID: "new", File: "y.png"}}}, src, 1<<20)
	cf, _ := s.Create(Spec{Title: "y/n", Type: Confirm}, src, 1<<20)
	ask, _ := s.Create(Spec{Title: "which", Type: Ask, Supersedes: cf.ID}, src, 1<<20)

	if a, err := s.Respond(ap.ID, "accept", nil, ""); err != nil || a.Selected[0] != "theme" {
		t.Errorf("approve: %+v %v", a, err)
	}
	if a, err := s.Respond(cp.ID, "new", nil, ""); err != nil || a.Selected[0] != "new" {
		t.Errorf("compare: %+v %v", a, err)
	}
	if _, err := s.Respond(cf.ID, "maybe", nil, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("confirm maybe: %v", err)
	}
	if _, err := s.Respond(cf.ID, "no", nil, ""); err != nil {
		t.Errorf("confirm: %v", err)
	}
	if _, err := s.Respond(ask.ID, "answered", nil, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("ask without text: %v", err)
	}
	if _, err := s.Respond(ask.ID, "answered", nil, "281485"); err != nil {
		t.Errorf("ask: %v", err)
	}
	if !Revise("changes") || !Revise("none") || Revise("reject") || Revise("accept") {
		t.Error("Revise")
	}
}

func TestWaitAndReset(t *testing.T) {
	s := open(t)
	src := files(nil)
	d, _ := s.Create(Spec{Title: "y/n", Type: Confirm}, src, 1<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := s.Wait(ctx, 0); got == nil || len(got) != 0 {
		t.Fatalf("timeout should give empty non-nil slice, got %v", got)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = s.Respond(d.ID, "yes", nil, "")
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	got := s.Wait(ctx2, 0)
	if len(got) != 1 || got[0].Verdict != "yes" || got[0].Decision != d.ID {
		t.Fatalf("wait: %+v", got)
	}
	if again := s.Answers(got[0].Seq); len(again) != 0 {
		t.Fatalf("since should exclude delivered: %+v", again)
	}

	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if p, a := s.Counts(); p != 0 || a != 0 || len(s.List()) != 0 {
		t.Fatal("reset left decisions")
	}
	if _, _, err := s.OpenFile(d.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("reset left files reachable")
	}
	d2, _ := s.Create(Spec{Title: "again", Type: Confirm}, src, 1<<20)
	if d2.ID == d.ID || !strings.HasPrefix(d2.ID, "d") {
		t.Fatalf("ids must not repeat after reset: %s", d2.ID)
	}
}
