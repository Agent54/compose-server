//go:build !windows

/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

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
