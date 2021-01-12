/*
Copyright 2017 The Kubernetes Authors.

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

package server

import (
	v1 "github.com/containerd/cgroups/stats/v1"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/log"
	"github.com/containerd/cri/pkg/store"
	"github.com/containerd/cri/pkg/store/container"
	"github.com/containerd/cri/pkg/store/sandbox"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// ContainerStats returns stats of the container. If the container does not
// exist, the call returns an error.
func (c *criService) ContainerStats(ctx context.Context, in *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	var cntr container.Container
	var sndbx sandbox.Sandbox
	var id string
	cntr, err := c.containerStore.Get(in.GetContainerId())
	if err == nil {
		id = cntr.ID
	} else if err == store.ErrNotExist {
		sndbx, err = c.sandboxStore.Get(in.GetContainerId())
		if err != nil {
			return nil, errors.Wrap(err, "failed to find container or sandbox")
		}
		id = sndbx.ID
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to find container")
	}
	request := &tasks.MetricsRequest{Filters: []string{"id==" + id}}
	resp, err := c.client.TaskService().Metrics(ctx, request)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch metrics for task")
	}
	if len(resp.Metrics) == 0 {
		log.G(ctx).Warnf("found no metrics for %s", id)
		return &runtime.ContainerStatsResponse{Stats: nil}, nil
	}

	var cs *runtime.ContainerStats
	if cntr != (container.Container{}) {
		cs, err = c.getContainerMetrics(cntr.Metadata, resp.Metrics[0])
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode container metrics")
		}
	} else if sndbx != (sandbox.Sandbox{}) {
		cs, err = c.getSandboxMetrics(sndbx.Metadata, resp.Metrics[0])
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode sandbox metrics")
		}
	}

	return &runtime.ContainerStatsResponse{Stats: cs}, nil
}

// getWorkingSet calculates workingset memory from cgroup memory stats.
// The caller should make sure memory is not nil.
// workingset = usage - total_inactive_file
func getWorkingSet(memory *v1.MemoryStat) uint64 {
	if memory.Usage == nil {
		return 0
	}
	var workingSet uint64
	if memory.TotalInactiveFile < memory.Usage.Usage {
		workingSet = memory.Usage.Usage - memory.TotalInactiveFile
	}
	return workingSet
}
