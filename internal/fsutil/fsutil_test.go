package fsutil

import (
	"path/filepath"
	"testing"
)

func TestAbsCleanLeavesEmptyEmpty(t *testing.T) {
	// "" means "not set" (an ad-hoc conversion has no watch root). Resolving it
	// to the working directory would make unrelated files look as if they lived
	// inside it.
	if got := AbsClean(""); got != "" {
		t.Errorf("AbsClean(%q) = %q, want %q", "", got, "")
	}
}

func TestDepth(t *testing.T) {
	root := filepath.Clean("/watch")
	cases := []struct {
		path string
		want int
	}{
		{filepath.Clean("/watch"), 0},
		{filepath.Clean("/watch/a"), 1},
		{filepath.Clean("/watch/a/b"), 2},
		{filepath.Clean("/watch/a/b/c"), 3},
	}
	for _, c := range cases {
		if got := Depth(root, c.path); got != c.want {
			t.Errorf("Depth(%q, %q) = %d, want %d", root, c.path, got, c.want)
		}
	}
}

func TestWithin(t *testing.T) {
	parent := filepath.Clean("/watch/out")
	cases := []struct {
		child string
		want  bool
	}{
		{filepath.Clean("/watch/out"), true},  // the directory itself
		{filepath.Clean("/watch/out/a"), true},
		{filepath.Clean("/watch"), false},
		{filepath.Clean("/watch/other"), false},
		{filepath.Clean("/watch/outside"), false}, // a prefix match is not containment
	}
	for _, c := range cases {
		if got := Within(parent, c.child); got != c.want {
			t.Errorf("Within(%q, %q) = %v, want %v", parent, c.child, got, c.want)
		}
	}
}

func TestPruneDir(t *testing.T) {
	root := filepath.Clean("/watch")
	p := func(elem ...string) string { return filepath.Join(append([]string{root}, elem...)...) }

	cases := []struct {
		name  string
		rules TraversalRules
		path  string
		want  bool
	}{
		{
			name:  "root is always traversed",
			rules: TraversalRules{Root: root, Recursive: true},
			path:  root,
			want:  false,
		},
		{
			name:  "non-recursive prunes subdirectories",
			rules: TraversalRules{Root: root, Recursive: false},
			path:  p("a"),
			want:  true,
		},
		{
			name:  "non-recursive still traverses the root",
			rules: TraversalRules{Root: root, Recursive: false},
			path:  root,
			want:  false,
		},
		{
			name:  "within max depth",
			rules: TraversalRules{Root: root, Recursive: true, MaxDepth: 2},
			path:  p("a", "b"),
			want:  false,
		},
		{
			name:  "beyond max depth",
			rules: TraversalRules{Root: root, Recursive: true, MaxDepth: 2},
			path:  p("a", "b", "c"),
			want:  true,
		},
		{
			name:  "zero max depth is unlimited",
			rules: TraversalRules{Root: root, Recursive: true, MaxDepth: 0},
			path:  p("a", "b", "c", "d"),
			want:  false,
		},
		{
			name:  "the ignored output directory is pruned",
			rules: TraversalRules{Root: root, Recursive: true, IgnoredDir: p("out"), HasIgnored: true},
			path:  p("out"),
			want:  true,
		},
		{
			name:  "a subdirectory of the ignored output directory is pruned",
			rules: TraversalRules{Root: root, Recursive: true, IgnoredDir: p("out"), HasIgnored: true},
			path:  p("out", "nested"),
			want:  true,
		},
		{
			name:  "a sibling of the ignored output directory is kept",
			rules: TraversalRules{Root: root, Recursive: true, IgnoredDir: p("out"), HasIgnored: true},
			path:  p("other"),
			want:  false,
		},
		{
			// Regression: an ignored directory that *is* the root must not prune the
			// root, or the job would silently scan and watch nothing at all.
			name:  "an ignored directory equal to the root does not prune the root",
			rules: TraversalRules{Root: root, Recursive: true, IgnoredDir: root, HasIgnored: true},
			path:  root,
			want:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rules.PruneDir(c.path); got != c.want {
				t.Errorf("PruneDir(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}
