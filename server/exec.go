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
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/pkg/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/server/errdefs"
)

type containerExecRequest struct {
	Path            string   `json:"path,omitempty"`
	Container       string   `json:"container,omitempty"`
	Service         string   `json:"service,omitempty"`
	Index           int      `json:"index,omitempty"`
	Command         []string `json:"command,omitempty"`
	Shell           string   `json:"shell,omitempty"`
	ShellExecutable string   `json:"shellExecutable,omitempty"`
	WorkingDir      string   `json:"workingDir,omitempty"`
	User            string   `json:"user,omitempty"`
	Env             []string `json:"env,omitempty"`
	Privileged      bool     `json:"privileged,omitempty"`
	TTY             bool     `json:"tty,omitempty"`
	StartStopped    *bool    `json:"startStopped,omitempty"`
}

type containerExecResponse struct {
	OK            bool      `json:"ok"`
	Project       string    `json:"project"`
	Service       string    `json:"service,omitempty"`
	ContainerID   string    `json:"containerId"`
	ContainerName string    `json:"containerName"`
	ExecID        string    `json:"execId"`
	Command       []string  `json:"command"`
	StartedAt     time.Time `json:"startedAt"`
	Message       string    `json:"message,omitempty"`
}

type processKillRequest struct {
	Path      string `json:"path,omitempty"`
	Container string `json:"container,omitempty"`
	Service   string `json:"service,omitempty"`
	Index     int    `json:"index,omitempty"`
	PID       int    `json:"pid"`
	Signal    string `json:"signal,omitempty"`
	Hard      bool   `json:"hard,omitempty"`
}

type processKillResponse struct {
	OK            bool   `json:"ok"`
	Project       string `json:"project"`
	Service       string `json:"service,omitempty"`
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	PID           int    `json:"pid"`
	Signal        string `json:"signal"`
}

type execProcessState struct {
	Project       string     `json:"project"`
	Service       string     `json:"service,omitempty"`
	ContainerID   string     `json:"containerId"`
	ContainerName string     `json:"containerName"`
	ExecID        string     `json:"execId"`
	Command       []string   `json:"command"`
	StartedAt     time.Time  `json:"startedAt"`
	EndedAt       *time.Time `json:"endedAt,omitempty"`
	Running       bool       `json:"running"`
	ExitCode      *int       `json:"exitCode,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type execRegistry struct {
	mu        sync.Mutex
	processes map[string]execProcessState
}

func newExecRegistry() *execRegistry {
	return &execRegistry{processes: map[string]execProcessState{}}
}

func (r *execRegistry) start(state execProcessState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processes[state.ExecID] = state
}

func (r *execRegistry) finish(execID string, exitCode *int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.processes[execID]
	now := time.Now().UTC()
	state.EndedAt = &now
	state.Running = false
	state.ExitCode = exitCode
	if err != nil {
		state.Error = err.Error()
	}
	r.processes[execID] = state
}

func (r *execRegistry) list(project string) []execProcessState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]execProcessState, 0)
	for _, state := range r.processes {
		if project == "" || state.Project == project {
			out = append(out, state)
		}
	}
	return out
}

func (a *serverApp) execInContainer(ctx context.Context, projectName string, req containerExecRequest) (containerExecResponse, error) {
	cmd, err := execCommand(req)
	if err != nil {
		return containerExecResponse{}, err
	}
	runtime, err := a.dockerRuntime()
	if err != nil {
		return containerExecResponse{}, err
	}
	project, _, name, err := a.resolvePSProject(ctx, projectName, req.Path)
	if err != nil {
		return containerExecResponse{}, err
	}
	ctr, err := a.resolveTargetContainer(ctx, runtime.client, name, project, req.Path, req.Container, req.Service, req.Index)
	if err != nil {
		return containerExecResponse{}, err
	}
	inspect, err := runtime.client.ContainerInspect(ctx, ctr.ID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return containerExecResponse{}, err
	}
	startStopped := req.StartStopped == nil || *req.StartStopped
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		if !startStopped {
			return containerExecResponse{}, errdefs.Conflict(fmt.Errorf("container %s is stopped", containerDisplayName(ctr)))
		}
		if _, err := runtime.client.ContainerStart(ctx, ctr.ID, mobyclient.ContainerStartOptions{}); err != nil {
			return containerExecResponse{}, err
		}
		a.broadcastExecLog(name, "status", containerDisplayName(ctr), fmt.Sprintf("container started for exec: %s", strings.Join(cmd, " ")))
	}

	create, err := runtime.client.ExecCreate(ctx, ctr.ID, mobyclient.ExecCreateOptions{
		User:         req.User,
		Privileged:   req.Privileged,
		TTY:          req.TTY,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   req.WorkingDir,
		Env:          req.Env,
		Cmd:          cmd,
	})
	if err != nil {
		return containerExecResponse{}, err
	}

	state := execProcessState{
		Project:       name,
		Service:       ctr.Labels[composeapi.ServiceLabel],
		ContainerID:   ctr.ID,
		ContainerName: containerDisplayName(ctr),
		ExecID:        create.ID,
		Command:       cmd,
		StartedAt:     time.Now().UTC(),
		Running:       true,
	}
	a.execs.start(state)
	go a.runContainerExec(context.Background(), runtime.client, state, req.TTY)

	return containerExecResponse{
		OK:            true,
		Project:       name,
		Service:       state.Service,
		ContainerID:   ctr.ID,
		ContainerName: state.ContainerName,
		ExecID:        create.ID,
		Command:       cmd,
		StartedAt:     state.StartedAt,
		Message:       "exec started; stdout/stderr is mixed into the project logs stream",
	}, nil
}

func (a *serverApp) listExecs(projectName string) []execProcessState {
	return a.execs.list(projectName)
}

func (a *serverApp) killContainerProcess(ctx context.Context, projectName string, req processKillRequest) (processKillResponse, error) {
	if req.PID <= 0 {
		return processKillResponse{}, errdefs.InvalidParameter(fmt.Errorf("pid must be > 0"))
	}
	runtime, err := a.dockerRuntime()
	if err != nil {
		return processKillResponse{}, err
	}
	project, _, name, err := a.resolvePSProject(ctx, projectName, req.Path)
	if err != nil {
		return processKillResponse{}, err
	}
	ctr, err := a.resolveTargetContainer(ctx, runtime.client, name, project, req.Path, req.Container, req.Service, req.Index)
	if err != nil {
		return processKillResponse{}, err
	}
	signal := strings.TrimSpace(req.Signal)
	if signal == "" {
		signal = "SIGTERM"
	}
	if req.Hard {
		signal = "SIGKILL"
	}
	if _, err := runExecSync(ctx, runtime.client, ctr.ID, []string{"kill", killSignalArg(signal), strconv.Itoa(req.PID)}); err != nil {
		return processKillResponse{}, err
	}
	a.broadcastExecLog(name, "status", containerDisplayName(ctr), fmt.Sprintf("sent %s to pid %d", signal, req.PID))
	return processKillResponse{
		OK:            true,
		Project:       name,
		Service:       ctr.Labels[composeapi.ServiceLabel],
		ContainerID:   ctr.ID,
		ContainerName: containerDisplayName(ctr),
		PID:           req.PID,
		Signal:        signal,
	}, nil
}

func (a *serverApp) dockerRuntime() (statsRuntime, error) {
	if a.stats == nil {
		return statsRuntime{}, fmt.Errorf("docker runtime unavailable")
	}
	runtime, err := a.stats()
	if err != nil {
		return statsRuntime{}, err
	}
	if runtime.client == nil {
		return statsRuntime{}, fmt.Errorf("docker runtime unavailable")
	}
	return runtime, nil
}

func (a *serverApp) resolveTargetContainer(
	ctx context.Context,
	apiClient mobyclient.APIClient,
	projectName string,
	project *types.Project,
	requestPath, containerRef, service string,
	index int,
) (containertypes.Summary, error) {
	containers, err := listProjectContainers(ctx, apiClient, projectName, true, nil)
	if err != nil {
		return containertypes.Summary{}, err
	}
	if requestPath != "" {
		if project != nil {
			containers = filterContainersByConfigFiles(containers, project.ComposeFiles)
		}
	}
	if containerRef != "" {
		for _, ctr := range containers {
			if containerMatches(ctr, containerRef) {
				return ctr, nil
			}
		}
		return containertypes.Summary{}, errdefs.NotFound(fmt.Errorf("container %q not found", containerRef))
	}
	if service == "" {
		return containertypes.Summary{}, errdefs.InvalidParameter(fmt.Errorf("service or container is required"))
	}
	if index <= 0 {
		index = 1
	}
	for _, ctr := range containers {
		if ctr.Labels[composeapi.ServiceLabel] != service {
			continue
		}
		if ctr.Labels[composeapi.ContainerNumberLabel] == strconv.Itoa(index) {
			return ctr, nil
		}
	}
	return containertypes.Summary{}, errdefs.NotFound(fmt.Errorf("container for service %q index %d not found", service, index))
}

func execCommand(req containerExecRequest) ([]string, error) {
	if len(req.Command) > 0 {
		return req.Command, nil
	}
	if req.Shell == "" {
		return nil, errdefs.InvalidParameter(fmt.Errorf("command or shell is required"))
	}
	shell := req.ShellExecutable
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell, "-lc", req.Shell}, nil
}

func killSignalArg(signal string) string {
	signal = strings.TrimSpace(strings.ToUpper(signal))
	signal = strings.TrimPrefix(signal, "SIG")
	if signal == "" {
		signal = "TERM"
	}
	return "-" + signal
}

func (a *serverApp) runContainerExec(ctx context.Context, apiClient mobyclient.APIClient, state execProcessState, tty bool) {
	a.broadcastExecLog(state.Project, "status", state.ContainerName, fmt.Sprintf("exec %s started: %s", state.ExecID, strings.Join(state.Command, " ")))
	conn, err := apiClient.ExecAttach(ctx, state.ExecID, mobyclient.ExecAttachOptions{TTY: tty})
	if err != nil {
		a.execs.finish(state.ExecID, nil, err)
		a.broadcastExecLog(state.Project, "stderr", state.ContainerName, err.Error())
		return
	}
	defer conn.Close()

	stdout := newExecLogWriter(func(line string) {
		a.broadcastExecLog(state.Project, "stdout", state.ContainerName, line)
	})
	stderr := newExecLogWriter(func(line string) {
		a.broadcastExecLog(state.Project, "stderr", state.ContainerName, line)
	})
	if tty {
		_, err = io.Copy(stdout, conn.Reader)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, conn.Reader)
	}
	stdout.Flush()
	stderr.Flush()
	if err != nil {
		a.broadcastExecLog(state.Project, "stderr", state.ContainerName, err.Error())
	}
	inspect, inspectErr := apiClient.ExecInspect(context.Background(), state.ExecID, mobyclient.ExecInspectOptions{})
	if inspectErr != nil {
		a.execs.finish(state.ExecID, nil, inspectErr)
		a.broadcastExecLog(state.Project, "stderr", state.ContainerName, inspectErr.Error())
		return
	}
	exitCode := inspect.ExitCode
	a.execs.finish(state.ExecID, &exitCode, err)
	a.broadcastExecLog(state.Project, "status", state.ContainerName, fmt.Sprintf("exec %s exited with code %d", state.ExecID, exitCode))
}

func runExecSync(ctx context.Context, apiClient mobyclient.APIClient, containerID string, cmd []string) (int, error) {
	create, err := apiClient.ExecCreate(ctx, containerID, mobyclient.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return 0, err
	}
	conn, err := apiClient.ExecAttach(ctx, create.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_, copyErr := io.Copy(io.Discard, conn.Reader)
	inspect, err := apiClient.ExecInspect(ctx, create.ID, mobyclient.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	if inspect.ExitCode != 0 {
		return inspect.ExitCode, errdefs.InvalidParameter(fmt.Errorf("exec exited with code %d", inspect.ExitCode))
	}
	return inspect.ExitCode, copyErr
}

func (a *serverApp) broadcastExecLog(project, stream, source, message string) {
	a.logs.broadcast(project, sseMessage{
		Project: project,
		Stream:  stream,
		Source:  source,
		Message: message,
		Time:    time.Now().UTC(),
	})
}

func containerDisplayName(ctr containertypes.Summary) string {
	if len(ctr.Names) > 0 {
		return strings.TrimPrefix(ctr.Names[0], "/")
	}
	return ctr.ID
}

func containerMatches(ctr containertypes.Summary, ref string) bool {
	if ctr.ID == ref || strings.HasPrefix(ctr.ID, ref) {
		return true
	}
	for _, name := range ctr.Names {
		trimmed := strings.TrimPrefix(name, "/")
		if name == ref || trimmed == ref {
			return true
		}
	}
	return false
}

type execLogWriter struct {
	mu      sync.Mutex
	pending string
	send    func(string)
}

func newExecLogWriter(send func(string)) *execLogWriter {
	return &execLogWriter{send: send}
}

func (w *execLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\r", "\n")
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.TrimSpace(line) != "" {
			w.send(line)
		}
	}
	return len(p), nil
}

func (w *execLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := strings.TrimSpace(w.pending)
	w.pending = ""
	if line != "" {
		w.send(line)
	}
}
