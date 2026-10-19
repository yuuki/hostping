//go:build !unix

package agent

import "time"

// processGone cannot check without signals; gate scripts are skipped here.
func processGone(int, time.Duration) bool { return true }
