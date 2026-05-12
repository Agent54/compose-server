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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containercmd "github.com/docker/cli/cli/command/container"
	dockerformatter "github.com/docker/cli/cli/command/formatter"
	"github.com/docker/docker/errdefs"
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

type systemInfoResponse struct {
	OK                bool    `json:"ok"`
	CPUCount          int     `json:"cpuCount"`
	NCPU              int     `json:"NCPU"`
	CPUPercent        float64 `json:"cpuPercent"`
	MemoryUsedBytes   float64 `json:"memoryUsedBytes"`
	MemoryTotalBytes  int64   `json:"memoryTotalBytes"`
	MemTotal          int64   `json:"MemTotal"`
	MemoryPercent     float64 `json:"memoryPercent"`
	UsageScope        string  `json:"usageScope"`
	OSType            string  `json:"osType,omitempty"`
	OperatingSystem   string  `json:"operatingSystem,omitempty"`
	Architecture      string  `json:"architecture,omitempty"`
	ServerVersion      string  `json:"serverVersion,omitempty"`
	DockerRootDir      string  `json:"dockerRootDir,omitempty"`
	StorageDriver     string  `json:"storageDriver,omitempty"`
	ContainersRunning int     `json:"containersRunning"`
	Raw               any     `json:"raw"`
}

type systemDiskUsageResponse struct {
	OK         bool  `json:"ok"`
	UsedBytes int64 `json:"usedBytes"`
	TotalBytes uint64 `json:"totalBytes,omitempty"`
	Images     diskUsageSummary `json:"images"`
	Containers diskUsageSummary `json:"containers"`
	Volumes    diskUsageSummary `json:"volumes"`
	BuildCache diskUsageSummary `json:"buildCache"`
	Raw        any              `json:"raw"`
}

type diskUsageSummary struct {
	ActiveCount int64 `json:"activeCount"`
	TotalCount  int64 `json:"totalCount"`
	UsedBytes   int64 `json:"usedBytes"`
	Reclaimable int64 `json:"reclaimable"`
}

type resourcesRequest struct {
	Path        string
	Services    []string
	All         bool
	Granularity string
}

type projectResourcesResponse struct {
	Project        string               `json:"project"`
	Granularity    string               `json:"granularity"`
	Containers     []containerResources `json:"containers,omitempty"`
	Services       []resourceAggregate  `json:"services,omitempty"`
	ProjectSummary *resourceAggregate   `json:"projectSummary,omitempty"`
}

type containerResources struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Project     string              `json:"project"`
	Service     string              `json:"service"`
	State       string              `json:"state,omitempty"`
	Status      string              `json:"status,omitempty"`
	Image       string              `json:"image,omitempty"`
	Usage       resourceUsage       `json:"usage"`
	Limits      resourceLimits      `json:"limits"`
	Utilization resourceUtilization `json:"utilization"`
	Error       string              `json:"error,omitempty"`
}

type resourceAggregate struct {
	Name        string              `json:"name"`
	Project     string              `json:"project"`
	Services    []string            `json:"services,omitempty"`
	Containers  int                 `json:"containers"`
	Usage       resourceUsage       `json:"usage"`
	Limits      resourceLimits      `json:"limits"`
	Utilization resourceUtilization `json:"utilization"`
}

type resourceUsage struct {
	CPUPercent       float64 `json:"cpuPercent"`
	MemoryBytes      float64 `json:"memoryBytes"`
	MemoryLimitBytes float64 `json:"memoryLimitBytes"`
	MemoryPercent    float64 `json:"memoryPercent"`
	NetworkRxBytes   float64 `json:"networkRxBytes"`
	NetworkTxBytes   float64 `json:"networkTxBytes"`
	BlockReadBytes    float64 `json:"blockReadBytes"`
	BlockWriteBytes   float64 `json:"blockWriteBytes"`
	PidsCurrent       uint64  `json:"pidsCurrent,omitempty"`
}

type resourceLimits struct {
	MemoryBytes            int64  `json:"memoryBytes,omitempty"`
	MemoryReservationBytes int64  `json:"memoryReservationBytes,omitempty"`
	MemorySwapBytes        int64  `json:"memorySwapBytes,omitempty"`
	NanoCPUs               int64  `json:"nanoCpus,omitempty"`
	CPUCores               float64 `json:"cpuCores,omitempty"`
	CPUPeriod              int64  `json:"cpuPeriod,omitempty"`
	CPUQuota               int64  `json:"cpuQuota,omitempty"`
	CPUShares              int64  `json:"cpuShares,omitempty"`
	CPUCount               int64  `json:"cpuCount,omitempty"`
	CpusetCpus             string `json:"cpusetCpus,omitempty"`
	PidsLimit              *int64 `json:"pidsLimit,omitempty"`
}

type resourceUtilization struct {
	MemoryLimitPercent float64 `json:"memoryLimitPercent,omitempty"`
	CPULimitPercent    float64 `json:"cpuLimitPercent,omitempty"`
}

type statsSnapshot struct {
	entry containercmd.StatsEntry
	err   error
}

func (a *serverApp) systemInfo(ctx context.Context) (systemInfoResponse, error) {
	if a.stats == nil {
		return systemInfoResponse{}, fmt.Errorf("stats runtime unavailable")
	}
	runtime, err := a.stats()
	if err != nil {
		return systemInfoResponse{}, err
	}
	if runtime.client == nil {
		return systemInfoResponse{}, fmt.Errorf("stats runtime unavailable")
	}
	info, err := runtime.client.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return systemInfoResponse{}, err
	}
	containers, err := runtime.client.ContainerList(ctx, mobyclient.ContainerListOptions{All: false})
	if err != nil {
		return systemInfoResponse{}, err
	}
	stats := collectStatsSnapshot(ctx, runtime.client, runtime.osType, containers.Items)
	usage := aggregateStatsUsage(stats)
	memoryPercent := 0.0
	if info.Info.MemTotal > 0 {
		memoryPercent = usage.MemoryBytes / float64(info.Info.MemTotal) * 100.0
	}
	return systemInfoResponse{
		OK:                true,
		CPUCount:          info.Info.NCPU,
		NCPU:              info.Info.NCPU,
		CPUPercent:        usage.CPUPercent,
		MemoryUsedBytes:   usage.MemoryBytes,
		MemoryTotalBytes:  info.Info.MemTotal,
		MemTotal:          info.Info.MemTotal,
		MemoryPercent:     memoryPercent,
		UsageScope:        "docker-containers",
		OSType:            info.Info.OSType,
		OperatingSystem:   info.Info.OperatingSystem,
		Architecture:      info.Info.Architecture,
		ServerVersion:      info.Info.ServerVersion,
		DockerRootDir:      info.Info.DockerRootDir,
		StorageDriver:     info.Info.Driver,
		ContainersRunning: len(containers.Items),
		Raw:               info,
	}, nil
}

func (a *serverApp) systemDiskUsage(ctx context.Context) (systemDiskUsageResponse, error) {
	if a.stats == nil {
		return systemDiskUsageResponse{}, fmt.Errorf("stats runtime unavailable")
	}
	runtime, err := a.stats()
	if err != nil {
		return systemDiskUsageResponse{}, err
	}
	if runtime.client == nil {
		return systemDiskUsageResponse{}, fmt.Errorf("stats runtime unavailable")
	}
	usage, err := runtime.client.DiskUsage(ctx, mobyclient.DiskUsageOptions{})
	if err != nil {
		return systemDiskUsageResponse{}, err
	}
	info, _ := runtime.client.Info(ctx, mobyclient.InfoOptions{})
	usedBytes := usage.Images.TotalSize + usage.Containers.TotalSize + usage.Volumes.TotalSize + usage.BuildCache.TotalSize
	return systemDiskUsageResponse{
		OK:         true,
		UsedBytes: usedBytes,
		TotalBytes: filesystemTotalBytes(info.Info.DockerRootDir),
		Images: diskUsageSummary{
			ActiveCount: usage.Images.ActiveCount,
			TotalCount:  usage.Images.TotalCount,
			UsedBytes:   usage.Images.TotalSize,
			Reclaimable: usage.Images.Reclaimable,
		},
		Containers: diskUsageSummary{
			ActiveCount: usage.Containers.ActiveCount,
			TotalCount:  usage.Containers.TotalCount,
			UsedBytes:   usage.Containers.TotalSize,
			Reclaimable: usage.Containers.Reclaimable,
		},
		Volumes: diskUsageSummary{
			ActiveCount: usage.Volumes.ActiveCount,
			TotalCount:  usage.Volumes.TotalCount,
			UsedBytes:   usage.Volumes.TotalSize,
			Reclaimable: usage.Volumes.Reclaimable,
		},
		BuildCache: diskUsageSummary{
			ActiveCount: usage.BuildCache.ActiveCount,
			TotalCount:  usage.BuildCache.TotalCount,
			UsedBytes:   usage.BuildCache.TotalSize,
			Reclaimable: usage.BuildCache.Reclaimable,
		},
		Raw: usage,
	}, nil
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

func (a *serverApp) projectResources(ctx context.Context, projectName string, req resourcesRequest) (projectResourcesResponse, error) {
	project, _, name, err := a.resolvePSProject(ctx, projectName, req.Path)
	if err != nil {
		return projectResourcesResponse{}, err
	}
	if a.stats == nil {
		return projectResourcesResponse{}, fmt.Errorf("stats runtime unavailable")
	}
	runtime, err := a.stats()
	if err != nil {
		return projectResourcesResponse{}, err
	}
	if runtime.client == nil {
		return projectResourcesResponse{}, fmt.Errorf("stats runtime unavailable")
	}

	granularity := req.Granularity
	if granularity == "" {
		granularity = "all"
	}
	if !slices.Contains([]string{"all", "container", "service", "project"}, granularity) {
		return projectResourcesResponse{}, errdefs.InvalidParameter(fmt.Errorf("unsupported granularity %q", granularity))
	}

	containers, err := listProjectContainers(ctx, runtime.client, name, req.All, req.Services)
	if err != nil {
		return projectResourcesResponse{}, err
	}
	if req.Path != "" && project != nil {
		containers = filterContainersByConfigFiles(containers, project.ComposeFiles)
	}

	stats := collectStatsSnapshot(ctx, runtime.client, runtime.osType, containers)
	out := projectResourcesResponse{
		Project:     name,
		Granularity: granularity,
	}

	containerRows := make([]containerResources, 0, len(containers))
	for _, ctr := range containers {
		row := containerResources{
			ID:      ctr.ID,
			Name:    ctr.ID,
			Project: name,
			Service: ctr.Labels[composeapi.ServiceLabel],
			State:   string(ctr.State),
			Status:  ctr.Status,
			Image:   ctr.Image,
		}
		if len(ctr.Names) > 0 {
			row.Name = strings.TrimPrefix(ctr.Names[0], "/")
		}
		if snapshot, ok := stats[ctr.ID]; ok {
			row.Usage = usageFromStats(snapshot.entry)
			if snapshot.entry.IsInvalid && snapshot.err != nil {
				row.Error = snapshot.err.Error()
			}
		}
		inspect, err := runtime.client.ContainerInspect(ctx, ctr.ID, mobyclient.ContainerInspectOptions{})
		if err != nil {
			row.Error = err.Error()
		} else if inspect.Container.HostConfig != nil {
			row.Limits = limitsFromResources(inspect.Container.HostConfig.Resources)
		}
		row.Utilization = utilizationFor(row.Usage, row.Limits)
		containerRows = append(containerRows, row)
	}
	slices.SortFunc(containerRows, func(a, b containerResources) int {
		if a.Service != b.Service {
			if a.Service < b.Service {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	if granularity == "all" || granularity == "container" {
		out.Containers = containerRows
	}
	if granularity == "all" || granularity == "service" {
		out.Services = aggregateResourcesByService(name, containerRows)
	}
	if granularity == "all" || granularity == "project" {
		summary := aggregateResourcesForProject(name, containerRows)
		out.ProjectSummary = &summary
	}
	return out, nil
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

func filterContainersByConfigFiles(containers []containertypes.Summary, configFiles []string) []containertypes.Summary {
	filtered := make([]containertypes.Summary, 0, len(containers))
	for _, ctr := range containers {
		if labelsContainAllConfigFiles(ctr.Labels[composeapi.ConfigFilesLabel], configFiles) {
			filtered = append(filtered, ctr)
		}
	}
	return filtered
}

func labelsContainAllConfigFiles(label string, configFiles []string) bool {
	if label == "" {
		return false
	}
	labelFiles := splitPathList(label)
	for _, file := range configFiles {
		if !slices.Contains(labelFiles, file) {
			return false
		}
	}
	return true
}

func collectStatsSnapshot(ctx context.Context, apiClient mobyclient.APIClient, osType string, containers []containertypes.Summary) map[string]statsSnapshot {
	stats := make([]*containercmd.Stats, 0, len(containers))
	waitFirst := &sync.WaitGroup{}
	for _, ctr := range containers {
		stat := containercmd.NewStats(ctr.ID)
		stats = append(stats, stat)
		waitFirst.Add(1)
		go collectStats(ctx, stat, apiClient, false, waitFirst, osType)
	}
	waitFirst.Wait()

	entries := make(map[string]statsSnapshot, len(stats))
	for _, stat := range stats {
		entry := stat.GetStatistics()
		entries[stat.Container] = statsSnapshot{
			entry: entry,
			err:   stat.GetError(),
		}
	}
	return entries
}

func aggregateStatsUsage(stats map[string]statsSnapshot) resourceUsage {
	usage := resourceUsage{}
	for _, snapshot := range stats {
		if snapshot.entry.IsInvalid {
			continue
		}
		item := usageFromStats(snapshot.entry)
		usage.CPUPercent += item.CPUPercent
		usage.MemoryBytes += item.MemoryBytes
		usage.MemoryLimitBytes += item.MemoryLimitBytes
		usage.NetworkRxBytes += item.NetworkRxBytes
		usage.NetworkTxBytes += item.NetworkTxBytes
		usage.BlockReadBytes += item.BlockReadBytes
		usage.BlockWriteBytes += item.BlockWriteBytes
		usage.PidsCurrent += item.PidsCurrent
	}
	if usage.MemoryLimitBytes > 0 {
		usage.MemoryPercent = usage.MemoryBytes / usage.MemoryLimitBytes * 100.0
	}
	return usage
}

func usageFromStats(entry containercmd.StatsEntry) resourceUsage {
	return resourceUsage{
		CPUPercent:       entry.CPUPercentage,
		MemoryBytes:      entry.Memory,
		MemoryLimitBytes: entry.MemoryLimit,
		MemoryPercent:    entry.MemoryPercentage,
		NetworkRxBytes:   entry.NetworkRx,
		NetworkTxBytes:   entry.NetworkTx,
		BlockReadBytes:    entry.BlockRead,
		BlockWriteBytes:   entry.BlockWrite,
		PidsCurrent:       entry.PidsCurrent,
	}
}

func limitsFromResources(resources containertypes.Resources) resourceLimits {
	limits := resourceLimits{
		MemoryBytes:            resources.Memory,
		MemoryReservationBytes: resources.MemoryReservation,
		MemorySwapBytes:        resources.MemorySwap,
		NanoCPUs:               resources.NanoCPUs,
		CPUCores:               cpuLimitCores(resources),
		CPUPeriod:              resources.CPUPeriod,
		CPUQuota:               resources.CPUQuota,
		CPUShares:              resources.CPUShares,
		CPUCount:               resources.CPUCount,
		CpusetCpus:             resources.CpusetCpus,
	}
	if resources.PidsLimit != nil {
		value := *resources.PidsLimit
		limits.PidsLimit = &value
	}
	return limits
}

func cpuLimitCores(resources containertypes.Resources) float64 {
	if resources.NanoCPUs > 0 {
		return float64(resources.NanoCPUs) / 1e9
	}
	if resources.CPUQuota > 0 && resources.CPUPeriod > 0 {
		return float64(resources.CPUQuota) / float64(resources.CPUPeriod)
	}
	return 0
}

func utilizationFor(usage resourceUsage, limits resourceLimits) resourceUtilization {
	utilization := resourceUtilization{}
	if limits.MemoryBytes > 0 {
		utilization.MemoryLimitPercent = usage.MemoryBytes / float64(limits.MemoryBytes) * 100.0
	} else if usage.MemoryLimitBytes > 0 {
		utilization.MemoryLimitPercent = usage.MemoryBytes / usage.MemoryLimitBytes * 100.0
	}
	if limits.CPUCores > 0 {
		utilization.CPULimitPercent = usage.CPUPercent / (limits.CPUCores * 100.0) * 100.0
	}
	return utilization
}

func aggregateResourcesByService(projectName string, containers []containerResources) []resourceAggregate {
	byService := map[string]*resourceAggregate{}
	for _, ctr := range containers {
		service := ctr.Service
		if service == "" {
			service = ctr.Name
		}
		aggregate := byService[service]
		if aggregate == nil {
			aggregate = &resourceAggregate{Name: service, Project: projectName}
			byService[service] = aggregate
		}
		addContainerToAggregate(aggregate, ctr)
	}

	names := make([]string, 0, len(byService))
	for name := range byService {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]resourceAggregate, 0, len(names))
	for _, name := range names {
		aggregate := *byService[name]
		aggregate.Utilization = utilizationFor(aggregate.Usage, aggregate.Limits)
		out = append(out, aggregate)
	}
	return out
}

func aggregateResourcesForProject(projectName string, containers []containerResources) resourceAggregate {
	aggregate := resourceAggregate{Name: projectName, Project: projectName}
	seenServices := map[string]struct{}{}
	for _, ctr := range containers {
		addContainerToAggregate(&aggregate, ctr)
		if ctr.Service != "" {
			seenServices[ctr.Service] = struct{}{}
		}
	}
	aggregate.Services = make([]string, 0, len(seenServices))
	for service := range seenServices {
		aggregate.Services = append(aggregate.Services, service)
	}
	sort.Strings(aggregate.Services)
	aggregate.Utilization = utilizationFor(aggregate.Usage, aggregate.Limits)
	return aggregate
}

func addContainerToAggregate(aggregate *resourceAggregate, ctr containerResources) {
	aggregate.Containers++
	aggregate.Usage.CPUPercent += ctr.Usage.CPUPercent
	aggregate.Usage.MemoryBytes += ctr.Usage.MemoryBytes
	aggregate.Usage.MemoryLimitBytes += ctr.Usage.MemoryLimitBytes
	aggregate.Usage.NetworkRxBytes += ctr.Usage.NetworkRxBytes
	aggregate.Usage.NetworkTxBytes += ctr.Usage.NetworkTxBytes
	aggregate.Usage.BlockReadBytes += ctr.Usage.BlockReadBytes
	aggregate.Usage.BlockWriteBytes += ctr.Usage.BlockWriteBytes
	aggregate.Usage.PidsCurrent += ctr.Usage.PidsCurrent
	if aggregate.Usage.MemoryLimitBytes > 0 {
		aggregate.Usage.MemoryPercent = aggregate.Usage.MemoryBytes / aggregate.Usage.MemoryLimitBytes * 100.0
	}
	aggregate.Limits.MemoryBytes += ctr.Limits.MemoryBytes
	aggregate.Limits.MemoryReservationBytes += ctr.Limits.MemoryReservationBytes
	aggregate.Limits.MemorySwapBytes += ctr.Limits.MemorySwapBytes
	aggregate.Limits.NanoCPUs += ctr.Limits.NanoCPUs
	aggregate.Limits.CPUCores += ctr.Limits.CPUCores
	aggregate.Limits.CPUShares += ctr.Limits.CPUShares
	aggregate.Limits.CPUCount += ctr.Limits.CPUCount
	if ctr.Limits.PidsLimit != nil {
		if aggregate.Limits.PidsLimit == nil {
			value := int64(0)
			aggregate.Limits.PidsLimit = &value
		}
		*aggregate.Limits.PidsLimit += *ctr.Limits.PidsLimit
	}
}

func filesystemTotalBytes(path string) uint64 {
	if path == "" {
		return 0
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return stat.Blocks * uint64(stat.Bsize)
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
