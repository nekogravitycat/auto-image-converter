// Package fsutil holds small filesystem helpers shared by the watcher and the
// startup batch, chiefly the recursion-depth and output-exclusion rules that
// decide which directories to traverse.
package fsutil

import (
	"path/filepath"
	"strings"
)

// AbsClean returns a cleaned absolute form of path, falling back to Clean when
// the absolute path cannot be determined.
func AbsClean(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// Depth returns how many directory levels path sits below root. The root itself
// is depth 0, a direct child directory is depth 1, and so on.
func Depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == "" {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// Within reports whether child is parent itself or lies somewhere beneath it.
// Both paths should already be absolute and cleaned.
func Within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TraversalRules captures the configuration that governs directory traversal
// for both the watcher and the batch scan.
type TraversalRules struct {
	Root       string
	Recursive  bool
	MaxDepth   int    // 0 = unlimited (when Recursive is true)
	IgnoredDir string // absolute path to exclude, or "" for none
	HasIgnored bool
}

// PruneDir reports whether the directory at path should be excluded from
// traversal (i.e. neither scanned nor watched, and not descended into).
//
// A directory is pruned when it is the ignored output directory, when recursion
// is disabled and it is not the root, or when its depth exceeds MaxDepth.
func (r TraversalRules) PruneDir(path string) bool {
	ap := AbsClean(path)
	if r.HasIgnored && Within(r.IgnoredDir, ap) {
		return true
	}
	depth := Depth(r.Root, ap)
	if depth == 0 {
		return false // always traverse the root
	}
	if !r.Recursive {
		return true
	}
	if r.MaxDepth > 0 && depth > r.MaxDepth {
		return true
	}
	return false
}
