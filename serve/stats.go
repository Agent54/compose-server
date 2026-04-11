package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	containercmd "github.com/docker/cli/cli/command/container"
	dockerformatter "github.com/docker/cli/cli/command/formatter"
	"github.com/docker/go-units"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

type statsRequest struct {
	Path     string
	Services []string
	All      bool
	NoStream bool
	NoTrunc  bool
	Format   string
}

func (a *serverApp) streamStats(ctx context.Context, projectName string, req statsRequest, w http.ResponseWriter) error {
	_, _, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return err
	}
	if a.stats == nil {
		return fmt.Errorf("stats runtime unavailable")
	}
	runtime, err := a.stats()
	if err != nil {
		return err
	}
	if runtime.client == nil {
		return fmt.Errorf("stats runtime unavailable")
	}

	containers, err := listProjectContainers(ctx, runtime.client, name, req.All, req.Services)
	if err != nil {
		return err
	}

	stream, err := newTextSSEStream(w)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return nil
	}

	stats := make([]*containercmd.Stats, 0, len(containers))
	waitFirst := &sync.WaitGroup{}
	for _, ctr := range containers {
		stat := containercmd.NewStats(ctr.ID)
		stats = append(stats, stat)
		waitFirst.Add(1)
		go collectStats(ctx, stat, runtime.client, !req.NoStream, waitFirst, runtime.osType)
	}
	waitFirst.Wait()

	render := func() error {
		entries := make([]containercmd.StatsEntry, 0, len(stats))
		for _, stat := range stats {
			entries = append(entries, stat.GetStatistics())
		}
		slices.SortFunc(entries, func(a, b containercmd.StatsEntry) int {
			left := statsEntryName(a)
			right := statsEntryName(b)
			if left < right {
				return -1
			}
			if left > right {
				return 1
			}
			return 0
		})
		frame, err := renderStatsFrame(entries, runtime.osType, req.Format, !req.NoTrunc)
		if err != nil {
			return err
		}
		if frame == "" {
			return nil
		}
		return stream.Send("frame", frame)
	}

	if req.NoStream {
		return render()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := render(); err != nil {
				return nil
			}
		}
	}
}

func listProjectContainers(ctx context.Context, apiClient mobyclient.APIClient, projectName string, all bool, services []string) ([]containertypes.Summary, error) {
	filters := mobyclient.Filters{}
	filters.Add("label", fmt.Sprintf("%s=%s", composeapi.ProjectLabel, strings.ToLower(projectName)))
	containers, err := apiClient.ContainerList(ctx, mobyclient.ContainerListOptions{
		All:     all,
		Filters: filters,
	})
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return containers.Items, nil
	}
	filtered := make([]containertypes.Summary, 0, len(containers.Items))
	for _, ctr := range containers.Items {
		if slices.Contains(services, ctr.Labels[composeapi.ServiceLabel]) {
			filtered = append(filtered, ctr)
		}
	}
	return filtered, nil
}

func statsEntryName(entry containercmd.StatsEntry) string {
	if name := strings.TrimPrefix(entry.Name, "/"); name != "" {
		return name
	}
	if entry.Container != "" {
		return entry.Container
	}
	return entry.ID
}

func collectStats(ctx context.Context, stat *containercmd.Stats, apiClient mobyclient.APIClient, streamStats bool, waitFirst *sync.WaitGroup, daemonOSType string) { //nolint:gocyclo
	var gotFirst bool
	defer func() {
		if !gotFirst {
			waitFirst.Done()
		}
	}()

	response, err := apiClient.ContainerStats(ctx, stat.Container, mobyclient.ContainerStatsOptions{
		Stream:                streamStats,
		IncludePreviousSample: !streamStats,
	})
	if err != nil {
		stat.SetError(err)
		return
	}

	results := make(chan error, 1)
	go func() {
		defer response.Body.Close()
		decoder := json.NewDecoder(response.Body)
		for {
			if ctx.Err() != nil {
				return
			}
			var payload containertypes.StatsResponse
			if err := decoder.Decode(&payload); err != nil {
				decoder = json.NewDecoder(io.MultiReader(decoder.Buffered(), response.Body))
				results <- err
				if err == io.EOF {
					break
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if payload.OSType == "" {
				payload.OSType = daemonOSType
			}
			if daemonOSType == "windows" {
				netRx, netTx := calculateNetwork(payload.Networks)
				stat.SetStatistics(containercmd.StatsEntry{
					Name:          payload.Name,
					ID:            payload.ID,
					CPUPercentage: calculateCPUPercentWindows(&payload),
					Memory:        float64(payload.MemoryStats.PrivateWorkingSet),
					NetworkRx:     netRx,
					NetworkTx:     netTx,
					BlockRead:     float64(payload.StorageStats.ReadSizeBytes),
					BlockWrite:    float64(payload.StorageStats.WriteSizeBytes),
				})
			} else {
				memUsage := calculateMemUsageUnixNoCache(payload.MemoryStats)
				netRx, netTx := calculateNetwork(payload.Networks)
				blockRead, blockWrite := calculateBlockIO(payload.BlkioStats)
				stat.SetStatistics(containercmd.StatsEntry{
					Name:             payload.Name,
					ID:               payload.ID,
					CPUPercentage:    calculateCPUPercentUnix(payload.PreCPUStats, payload.CPUStats),
					Memory:           memUsage,
					MemoryPercentage: calculateMemPercentUnixNoCache(float64(payload.MemoryStats.Limit), memUsage),
					MemoryLimit:      float64(payload.MemoryStats.Limit),
					NetworkRx:        netRx,
					NetworkTx:        netTx,
					BlockRead:        float64(blockRead),
					BlockWrite:       float64(blockWrite),
					PidsCurrent:      payload.PidsStats.Current,
				})
			}
			results <- nil
			if !streamStats {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			stat.SetError(ctx.Err())
			return
		case <-time.After(2 * time.Second):
			stat.SetErrorAndReset(errors.New("timeout waiting for stats"))
			if !gotFirst {
				gotFirst = true
				waitFirst.Done()
			}
		case err := <-results:
			stat.SetError(err)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				continue
			}
			if !gotFirst {
				gotFirst = true
				waitFirst.Done()
			}
		}
		if !streamStats {
			return
		}
	}
}

func calculateCPUPercentUnix(previousCPU containertypes.CPUStats, curCPUStats containertypes.CPUStats) float64 {
	cpuDelta := float64(curCPUStats.CPUUsage.TotalUsage) - float64(previousCPU.CPUUsage.TotalUsage)
	systemDelta := float64(curCPUStats.SystemUsage) - float64(previousCPU.SystemUsage)
	onlineCPUs := float64(curCPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(curCPUStats.CPUUsage.PercpuUsage))
	}
	if systemDelta > 0 && cpuDelta > 0 {
		return (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}
	return 0
}

func calculateCPUPercentWindows(payload *containertypes.StatsResponse) float64 {
	possibleIntervals := uint64(payload.Read.Sub(payload.PreRead).Nanoseconds())
	possibleIntervals /= 100
	possibleIntervals *= uint64(payload.NumProcs)
	if possibleIntervals > 0 {
		used := payload.CPUStats.CPUUsage.TotalUsage - payload.PreCPUStats.CPUUsage.TotalUsage
		return float64(used) / float64(possibleIntervals) * 100.0
	}
	return 0
}

func calculateBlockIO(stats containertypes.BlkioStats) (uint64, uint64) {
	var read, write uint64
	for _, entry := range stats.IoServiceBytesRecursive {
		if entry.Op == "" {
			continue
		}
		switch entry.Op[0] {
		case 'r', 'R':
			read += entry.Value
		case 'w', 'W':
			write += entry.Value
		}
	}
	return read, write
}

func calculateNetwork(networks map[string]containertypes.NetworkStats) (float64, float64) {
	var rx, tx float64
	for _, network := range networks {
		rx += float64(network.RxBytes)
		tx += float64(network.TxBytes)
	}
	return rx, tx
}

func calculateMemUsageUnixNoCache(memory containertypes.MemoryStats) float64 {
	if value, ok := memory.Stats["total_inactive_file"]; ok && value < memory.Usage {
		return float64(memory.Usage - value)
	}
	if value := memory.Stats["inactive_file"]; value < memory.Usage {
		return float64(memory.Usage - value)
	}
	return float64(memory.Usage)
}

func calculateMemPercentUnixNoCache(limit, used float64) float64 {
	if limit != 0 {
		return used / limit * 100.0
	}
	return 0
}

func renderStatsFrame(entries []containercmd.StatsEntry, osType, format string, trunc bool) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	if format == "" {
		format = dockerformatter.TableFormatKey
	}
	ctx := dockerformatter.Context{
		Output: &bytes.Buffer{},
		Format: containercmd.NewStatsFormat(format, osType),
	}
	buffer := ctx.Output.(*bytes.Buffer)
	if err := writeStatsFormat(ctx, entries, osType, trunc); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

const (
	statsContainerHeader = "CONTAINER"
	statsCPUPercHeader   = "CPU %"
	statsNetIOHeader     = "NET I/O"
	statsBlockIOHeader   = "BLOCK I/O"
	statsMemPercHeader   = "MEM %"
	statsWinMemUseHeader = "PRIV WORKING SET"
	statsMemUseHeader    = "MEM USAGE / LIMIT"
	statsPidsHeader      = "PIDS"
	statsNoValue         = "--"
)

func writeStatsFormat(ctx dockerformatter.Context, stats []containercmd.StatsEntry, osType string, trunc bool) error {
	memUsage := statsMemUseHeader
	if osType == "windows" {
		memUsage = statsWinMemUseHeader
	}
	statsCtx := serveStatsContext{os: osType}
	statsCtx.Header = dockerformatter.SubHeaderContext{
		"Container": statsContainerHeader,
		"Name":      dockerformatter.NameHeader,
		"ID":        dockerformatter.ContainerIDHeader,
		"CPUPerc":   statsCPUPercHeader,
		"MemUsage":  memUsage,
		"MemPerc":   statsMemPercHeader,
		"NetIO":     statsNetIOHeader,
		"BlockIO":   statsBlockIOHeader,
		"PIDs":      statsPidsHeader,
	}
	return ctx.Write(&statsCtx, func(format func(subContext dockerformatter.SubContext) error) error {
		for _, stat := range stats {
			if err := format(&serveStatsContext{
				entry: stat,
				os:    osType,
				trunc: trunc,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

type serveStatsContext struct {
	dockerformatter.HeaderContext
	entry containercmd.StatsEntry
	os    string
	trunc bool
}

func (c *serveStatsContext) MarshalJSON() ([]byte, error) {
	return dockerformatter.MarshalJSON(c)
}

func (c *serveStatsContext) Container() string {
	return c.entry.Container
}

func (c *serveStatsContext) Name() string {
	if name := strings.TrimPrefix(c.entry.Name, "/"); name != "" {
		return name
	}
	return statsNoValue
}

func (c *serveStatsContext) ID() string {
	if c.trunc {
		return dockerformatter.TruncateID(c.entry.ID)
	}
	return c.entry.ID
}

func (c *serveStatsContext) CPUPerc() string {
	if c.entry.IsInvalid {
		return statsNoValue
	}
	return formatPercentage(c.entry.CPUPercentage)
}

func (c *serveStatsContext) MemUsage() string {
	if c.entry.IsInvalid {
		return "-- / --"
	}
	if c.os == "windows" {
		return units.BytesSize(c.entry.Memory)
	}
	return units.BytesSize(c.entry.Memory) + " / " + units.BytesSize(c.entry.MemoryLimit)
}

func (c *serveStatsContext) MemPerc() string {
	if c.entry.IsInvalid || c.os == "windows" {
		return statsNoValue
	}
	return formatPercentage(c.entry.MemoryPercentage)
}

func (c *serveStatsContext) NetIO() string {
	if c.entry.IsInvalid {
		return statsNoValue
	}
	return units.HumanSizeWithPrecision(c.entry.NetworkRx, 3) + " / " + units.HumanSizeWithPrecision(c.entry.NetworkTx, 3)
}

func (c *serveStatsContext) BlockIO() string {
	if c.entry.IsInvalid {
		return statsNoValue
	}
	return units.HumanSizeWithPrecision(c.entry.BlockRead, 3) + " / " + units.HumanSizeWithPrecision(c.entry.BlockWrite, 3)
}

func (c *serveStatsContext) PIDs() string {
	if c.entry.IsInvalid || c.os == "windows" {
		return statsNoValue
	}
	return strconv.FormatUint(c.entry.PidsCurrent, 10)
}

func formatPercentage(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64) + "%"
}
