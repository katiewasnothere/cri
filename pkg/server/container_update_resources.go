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
	gocontext "context"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/typeurl"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/api/criextension"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	"github.com/containerd/cri/pkg/store"
	containerstore "github.com/containerd/cri/pkg/store/container"
	"github.com/containerd/cri/pkg/store/sandbox"
	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
	"github.com/containerd/cri/pkg/util"
)

// UpdateContainerResources updates ContainerConfig of the container.
func (c *criService) UpdateContainerResources(ctx context.Context, r *runtime.UpdateContainerResourcesRequest) (retRes *runtime.UpdateContainerResourcesResponse, retErr error) {
	if err := c.genericUpdateContainerResources(ctx, r.GetContainerId(), r.GetLinux()); err != nil {
		return nil, err
	}
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

// UpdateContainerResourcesV2 updates ContainerConfig of the container. This call supports both windows and linux.
//
// This is only needed since the CRI spec does not support windows resources updates.
func (c *criService) UpdateContainerResourcesV2(ctx context.Context, r *criextension.UpdateContainerResourcesV2Request) (retRes *criextension.UpdateContainerResourcesV2Response, retErr error) {
	var genericResources interface{} = r.GetResources().GetStdLinuxResources()
	if genericResources.(*runtime.LinuxContainerResources) == nil {
		genericResources = r.GetResources().GetStdWindowsResources()
	}
	if err := c.genericUpdateContainerResources(ctx, r.GetContainerId(), genericResources); err != nil {
		return nil, err
	}
	return &criextension.UpdateContainerResourcesV2Response{}, nil
}

func (c *criService) genericUpdateContainerResources(ctx context.Context, id string, resources interface{}) (retErr error) {
	// Update resources in status update transaction, so that:
	// 1) There won't be race condition with container start.
	// 2) There won't be concurrent resource update to the same container.
	cntr, err := c.containerStore.Get(id)
	if err == nil {
		if err := cntr.Status.Update(func(status containerstore.Status) (containerstore.Status, error) {
			return status, c.updateContainerResources(ctx, cntr.Container, resources, status)
		}); err != nil {
			return errors.Wrap(err, "failed to update resources")
		}
	} else if err == store.ErrNotExist {
		sndbx, err := c.sandboxStore.Get(id)
		if err != nil {
			return errors.Wrap(err, "failed to find container or sandbox")
		}
		if err := sndbx.Status.Update(func(status sandboxstore.Status) (sandboxstore.Status, error) {
			return status, c.updatePodContainerResources(ctx, sndbx.Container, resources, status)
		}); err != nil {
			return errors.Wrap(err, "failed to update resources")
		}

	} else if err != nil {
		return errors.Wrap(err, "failed to find container")
	}
	return nil
}

func (c *criService) updatePodContainerResources(ctx context.Context,
	cntr containerd.Container,
	genericResources interface{},
	status sandboxstore.Status) (retErr error) {

	id := cntr.ID
	if status.State == sandbox.StateUnknown {
		return errors.Errorf("sandbox %q is in unknown state", id)
	}

	oldSpec, err := cntr.Spec(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get container spec")
	}

	newSpec, newResources, err := createUpdatedSpec(cntr, oldSpec, genericResources)
	if err != nil {
		return err
	}

	if err := updateContainerSpec(ctx, cntr, newSpec); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			// Reset spec on error.
			if err := updateContainerSpec(deferCtx, cntr, oldSpec); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to update spec %+v for container %q", oldSpec, id)
			}
		}
	}()

	// If sandbox is not ready, only update spec is enough, new resource
	// limit will be applied on pod start.
	if status.State != sandbox.StateNotReady {
		return nil
	}
	return requestTaskUpdate(ctx, cntr, newResources)
}

func (c *criService) updateContainerResources(ctx context.Context,
	cntr containerd.Container,
	genericResources interface{},
	status containerstore.Status) (retErr error) {
	id := cntr.ID
	// Do not update the container when there is a removal in progress.
	if status.Removing {
		return errors.Errorf("container %q is in removing state", id)
	}

	// Update container spec. If the container is not started yet, updating
	// spec makes sure that the resource limits are correct when start;
	// if the container is already started, updating spec is still required,
	// the spec will become our source of truth for resource limits.
	oldSpec, err := cntr.Spec(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get container spec")
	}
	newSpec, newResources, err := createUpdatedSpec(cntr, oldSpec, genericResources)
	if err != nil {
		return err
	}

	if err := updateContainerSpec(ctx, cntr, newSpec); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			// Reset spec on error.
			if err := updateContainerSpec(deferCtx, cntr, oldSpec); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to update spec %+v for container %q", oldSpec, id)
			}
		}
	}()

	// If container is not running, only update spec is enough, new resource
	// limit will be applied on container start.
	if status.State() != runtime.ContainerState_CONTAINER_RUNNING {
		return nil
	}
	return requestTaskUpdate(ctx, cntr, newResources)
}

func createUpdatedSpec(cntr containerd.Container, oldSpec *runtimespec.Spec, genericResources interface{}) (*runtimespec.Spec, interface{}, error) {
	var err error
	newSpec := (*runtimespec.Spec)(nil)
	newResources := (interface{})(nil)

	if resources, ok := (genericResources).(*runtime.LinuxContainerResources); ok {
		log.G(context.Background()).WithField("Updating as a linux resource", resources).Info("createUpdatedSpec")
		newSpec, err = updateOCILinuxResource(oldSpec, resources)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to update resource in spec")
		}
		newResources = newSpec.Linux.Resources
	} else if resources, ok := (genericResources).(*runtime.WindowsContainerResources); ok {
		log.G(context.Background()).WithField("Updating as a windows resource", resources).Info("createUpdatedSpec")
		newSpec, err = updateOCIWindowsResource(oldSpec, resources)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to update resource in spec")
		}
		newResources = newSpec.Windows.Resources
	}

	return newSpec, newResources, nil
}

// updateContainerSpec updates container spec on disk.
func updateContainerSpec(ctx context.Context, cntr containerd.Container, spec *runtimespec.Spec) error {
	any, err := typeurl.MarshalAny(spec)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal spec %+v", spec)
	}
	if err := cntr.Update(ctx, func(ctx gocontext.Context, client *containerd.Client, c *containers.Container) error {
		c.Spec = any
		return nil
	}); err != nil {
		return errors.Wrap(err, "failed to update container spec")
	}
	return nil
}

// updateOCILinuxResource updates container resource limit.
func updateOCILinuxResource(spec *runtimespec.Spec, new *runtime.LinuxContainerResources) (*runtimespec.Spec, error) {
	// Copy to make sure old spec is not changed.
	var cloned runtimespec.Spec
	if err := util.DeepCopy(&cloned, spec); err != nil {
		return nil, errors.Wrap(err, "failed to deep copy")
	}
	g := newSpecGenerator(&cloned)

	if new.GetCpuPeriod() != 0 {
		g.SetLinuxResourcesCPUPeriod(uint64(new.GetCpuPeriod()))
	}
	if new.GetCpuQuota() != 0 {
		g.SetLinuxResourcesCPUQuota(new.GetCpuQuota())
	}
	if new.GetCpuShares() != 0 {
		g.SetLinuxResourcesCPUShares(uint64(new.GetCpuShares()))
	}
	if new.GetMemoryLimitInBytes() != 0 {
		g.SetLinuxResourcesMemoryLimit(new.GetMemoryLimitInBytes())
	}
	// OOMScore is not updatable.
	if new.GetCpusetCpus() != "" {
		g.SetLinuxResourcesCPUCpus(new.GetCpusetCpus())
	}
	if new.GetCpusetMems() != "" {
		g.SetLinuxResourcesCPUMems(new.GetCpusetMems())
	}

	return g.Config, nil
}

// updateOCIWindowsResource updates container resource limits for windows containers.
func updateOCIWindowsResource(spec *runtimespec.Spec, new *runtime.WindowsContainerResources) (*runtimespec.Spec, error) {
	// Copy to make sure old spec is not changed.
	var cloned runtimespec.Spec
	if err := util.DeepCopy(&cloned, spec); err != nil {
		return nil, errors.Wrap(err, "failed to deep copy")
	}
	updateCPUResource := false
	g := newSpecGenerator(&cloned)
	cpuResources := runtimespec.WindowsCPUResources{}

	if new.GetCpuShares() != 0 {
		val := uint16(new.GetCpuShares())
		cpuResources.Shares = &val
		updateCPUResource = true
	}
	if new.GetCpuCount() != 0 {
		val := uint64(new.GetCpuCount())
		cpuResources.Count = &val
		updateCPUResource = true
	}
	if new.GetCpuMaximum() != 0 {
		val := uint16(new.GetCpuMaximum())
		cpuResources.Maximum = &val
		updateCPUResource = true
	}
	if updateCPUResource {
		g.SetWindowsResourcesCPU(cpuResources)
	}
	if new.GetMemoryLimitInBytes() != 0 {
		g.SetWindowsResourcesMemoryLimit(uint64(new.GetMemoryLimitInBytes()))
	}

	return g.Config, nil
}

func requestTaskUpdate(ctx context.Context, cntr containerd.Container, resources interface{}) error {
	task, err := cntr.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			// Task exited already.
			return nil
		}
		return errors.Wrap(err, "failed to get task")
	}
	if err := task.Update(ctx, containerd.WithResources(resources)); err != nil {
		if errdefs.IsNotFound(err) {
			// Task exited already.
			return nil
		}
		return errors.Wrap(err, "failed to update resources")
	}
	return nil
}
