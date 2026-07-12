// Package humanize formats machine values for people. It exists so the manager
// (tray balloons) and the UI (the stats line) render the same number the same
// way, instead of each keeping its own copy of the formatter.
package humanize

import "fmt"

// Bytes formats a byte count as a short human-readable string, e.g. "1.5 MB".
// Counts below 1 KiB are shown exactly; larger ones use one decimal place and
// binary (1024-based) units.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
