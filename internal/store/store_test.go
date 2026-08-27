package store

import "testing"

func TestClean(t *testing.T) {
	ok := map[string]string{
		"a/b.txt":      "a/b.txt",
		"/a/b.txt":     "a/b.txt",
		"a//b/./c.txt": "a/b/c.txt",
		" x.txt ":      "x.txt",
	}
	for in, want := range ok {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "/", "..", "../x", "a/../b.txt", "a/../../x", "a\x00b"} {
		if got, err := Clean(in); err == nil {
			t.Errorf("Clean(%q) = %q; want error", in, got)
		}
	}
}
