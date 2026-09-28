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
	"fmt"
	"strings"
	"sync"

	"github.com/docker/compose/v5/server/errdefs"
)

// projectMutationLocks serializes mutations within each project. Waiters can
// cancel independently, and idle entries are removed once their users finish.
type projectMutationLocks struct {
	mu      sync.Mutex
	entries map[string]*projectMutationLock
}

// projectMutationLock holds one project's token and counts owners and waiters.
type projectMutationLock struct {
	token chan struct{}
	users int
}

// lock acquires a project token, returning a release function on success.
func (l *projectMutationLocks) lock(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name = strings.ToLower(name)
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[string]*projectMutationLock{}
	}
	entry := l.entries[name]
	if entry == nil {
		entry = &projectMutationLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		l.entries[name] = entry
	}
	entry.users++
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		l.release(name, entry)
		return nil, ctx.Err()
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			l.release(name, entry)
			return nil, err
		}
		return func() {
			entry.token <- struct{}{}
			l.release(name, entry)
		}, nil
	}
}

// release drops a reference after either unlocking or abandoning a wait.
func (l *projectMutationLocks) release(name string, entry *projectMutationLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.users--
	if entry.users == 0 {
		delete(l.entries, name)
	}
}

// lockProjectMutation coordinates HTTP mutations with watch's project token.
// An active watch must be stopped before a competing HTTP mutation can proceed.
func (a *serverApp) lockProjectMutation(ctx context.Context, name string) (func(), error) {
	name = strings.ToLower(name)
	if _, watching := a.watches.snapshot()[name]; watching {
		return nil, errdefs.Conflict(fmt.Errorf("project %q is being watched; stop watch before modifying it", name))
	}
	unlock, err := a.mutations.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	if _, watching := a.watches.snapshot()[name]; watching {
		unlock()
		return nil, errdefs.Conflict(fmt.Errorf("project %q is being watched; stop watch before modifying it", name))
	}
	return unlock, nil
}
