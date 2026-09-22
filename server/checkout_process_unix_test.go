//go:build !windows

package serve

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func TestCheckoutCancellationStopsGitHelpers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{}, 1)
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & printf ready; wait")
	cmd.Stdout = &checkoutReadyWriter{ready: ready}
	cmd.WaitDelay = 3 * time.Second
	finished := make(chan error, 1)
	go func() { finished <- runCheckoutCommand(cmd) }()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("checkout helper did not start")
	}
	cancel()
	select {
	case err := <-finished:
		assert.Assert(t, err != nil)
	case <-time.After(2 * time.Second):
		t.Fatal("Git helper process kept the checkout command alive after cancellation")
	}
}

type checkoutReadyWriter struct {
	ready chan<- struct{}
}

func (w *checkoutReadyWriter) Write(data []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(data), nil
}
