/*
   Copyright 2026 Docker Compose CLI authors

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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
	"github.com/docker/compose/v5/server/errdefs"
)

func TestProjectMutationLocksAllowIndependentProjectsAndCancellation(t *testing.T) {
	var locks projectMutationLocks
	unlock, err := locks.lock(t.Context(), "demo")
	assert.NilError(t, err)
	otherUnlock, err := locks.lock(t.Context(), "other")
	assert.NilError(t, err)
	otherUnlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = locks.lock(ctx, "DEMO")
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded))
	unlock()
	assert.Equal(t, len(locks.entries), 0)
	unlock, err = locks.lock(t.Context(), "demo")
	assert.NilError(t, err)
	unlock()
}

func TestUpCoordinatesWithSingleContainerStart(t *testing.T) {
	ctrl := gomock.NewController(t)
	backend := mocks.NewMockCompose(ctrl)
	app := newServerApp(serveConfig{rootDir: t.TempDir()}, func(...composepkg.Option) (composeapi.Compose, error) { return backend, nil }, nil)
	project := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web", Image: "web"}}}
	backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, *types.Project, composeapi.UpOptions) error {
		close(entered)
		<-release
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := app.upProject(t.Context(), upRequest{Project: "demo", Path: app.config.rootDir})
		done <- err
	}()
	awaitMutationSignal(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := app.startContainer(ctx, "demo", containerActionRequest{Path: app.config.rootDir, Service: "web"})
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded))
	// An unrelated project's HTTP mutation can acquire its token during /up.
	unlock, err := app.lockProjectMutation(t.Context(), "other")
	assert.NilError(t, err)
	unlock()
	finish()
	assert.NilError(t, <-done)
}

func TestWatchCoordinatesUntilCanceledMutationFinishes(t *testing.T) {
	ctrl := gomock.NewController(t)
	backend := mocks.NewMockCompose(ctrl)
	app := newServerApp(serveConfig{}, func(...composepkg.Option) (composeapi.Compose, error) { return backend, nil }, nil)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { app.stopWatch("demo"); finish() })
	backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ *types.Project, _ composeapi.UpOptions) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	})
	project := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web"}}}
	assert.NilError(t, app.startWatch("demo", []*types.Project{project}, nil, false, false))
	awaitMutationSignal(t, entered)
	_, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: "/demo", Service: "web"})
	assert.Assert(t, errdefs.IsConflict(err))
	unlock, err := app.lockProjectMutation(t.Context(), "other")
	assert.NilError(t, err)
	unlock()
	assert.Equal(t, app.stopWatch("demo"), true)
	awaitMutationSignal(t, canceled)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = app.lockProjectMutation(ctx, "demo")
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded))
	finish()
	unlock, err = app.lockProjectMutation(t.Context(), "demo")
	assert.NilError(t, err)
	unlock()
}

// awaitMutationSignal bounds synchronization failures without timing-based sleeps.
func awaitMutationSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("project mutation did not reach the synchronization point")
	}
}
