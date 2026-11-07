package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine is still running once
// they have finished.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
