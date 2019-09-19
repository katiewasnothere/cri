// +build windows

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
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/snapshots"
	"github.com/davecgh/go-spew/spew"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	criconfig "github.com/containerd/cri/pkg/config"
	customopts "github.com/containerd/cri/pkg/containerd/opts"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	cio "github.com/containerd/cri/pkg/server/io"
	containerstore "github.com/containerd/cri/pkg/store/container"
	"github.com/containerd/cri/pkg/util"
)

// CreateContainer creates a new container in the given PodSandbox.
func (c *criService) CreateContainer(ctx context.Context, r *runtime.CreateContainerRequest) (_ *runtime.CreateContainerResponse, retErr error) {
	config := r.GetConfig()
	log.G(ctx).Debugf("Container config %+v", config)
	sandboxConfig := r.GetSandboxConfig()
	sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find sandbox id %q", r.GetPodSandboxId())
	}
	sandboxID := sandbox.ID
	s, err := sandbox.Container.Task(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox container task")
	}
	sandboxPid := s.Pid()

	// Generate unique id and name for the container and reserve the name.
	// Reserve the container name to avoid concurrent `CreateContainer` request creating
	// the same container.
	id := util.GenerateID()
	metadata := config.GetMetadata()
	if metadata == nil {
		return nil, errors.New("container config must include metadata")
	}
	name := makeContainerName(metadata, sandboxConfig.GetMetadata())
	log.G(ctx).Debugf("Generated id %q for container %q", id, name)
	if err = c.containerNameIndex.Reserve(name, id); err != nil {
		return nil, errors.Wrapf(err, "failed to reserve container name %q", name)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.containerNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal container metadata.
	meta := containerstore.Metadata{
		ID:        id,
		Name:      name,
		SandboxID: sandboxID,
		Config:    config,
	}

	// Prepare container image snapshot. For container, the image should have
	// been pulled before creating the container, so do not ensure the image.
	image, err := c.localResolve(config.GetImage().GetImage())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve image %q", config.GetImage().GetImage())
	}

	// Run container using the same runtime with sandbox.
	sandboxInfo, err := sandbox.Container.Info(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get sandbox %q info", sandboxID)
	}

	// Create container root directory.
	containerRootDir := c.getContainerRootDir(id)
	if err = c.os.MkdirAll(containerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create container root directory %q",
			containerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the container root directory.
			if err = c.os.RemoveAll(containerRootDir); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to remove container root directory %q",
					containerRootDir)
			}
		}
	}()
	volatileContainerRootDir := c.getVolatileContainerRootDir(id)
	if err = c.os.MkdirAll(volatileContainerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create volatile container root directory %q",
			volatileContainerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the volatile container root directory.
			if err = c.os.RemoveAll(volatileContainerRootDir); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to remove volatile container root directory %q",
					volatileContainerRootDir)
			}
		}
	}()

	var sandboxPlatform string

	// Get the RuntimeHandler config overrides
	var ociRuntime criconfig.Runtime
	if sandbox.RuntimeHandler != "" {
		ociRuntime = c.config.Runtimes[sandbox.RuntimeHandler]
	} else {
		ociRuntime = c.config.DefaultRuntime
	}
	runtimeOpts, err := generateRuntimeOptions(ociRuntime, c.config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime options")
	}
	rhcso := runtimeOpts.(*runhcsoptions.Options)
	sandboxPlatform = rhcso.SandboxPlatform
	if sandboxPlatform == "" {
		sandboxPlatform = "windows/amd64"
	}

	spec, err := c.generateContainerSpec(id, sandboxID, sandboxPid, sandbox.NetNSPath, config, sandboxConfig, sandboxPlatform, &image.ImageSpec.Config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate container %q spec", id)
	}

	log.G(ctx).Debugf("Container %q spec: %#+v", id, spew.NewFormatter(spec))

	snapshotterOpt := snapshots.WithLabels(config.Annotations)

	// Set snapshotter before any other options.
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(c.getDefaultSnapshotterForPlatform(sandboxPlatform)),
		customopts.WithNewSnapshot(id, image.Image, snapshotterOpt),
	}

	meta.ImageRef = image.ID
	meta.StopSignal = image.ImageSpec.Config.StopSignal

	// Get container log path.
	if config.GetLogPath() != "" {
		meta.LogPath = filepath.Join(sandbox.Config.GetLogDirectory(), config.GetLogPath())
	}

	containerIO, err := cio.NewContainerIO(id,
		cio.WithNewFIFOs(volatileContainerRootDir, config.GetTty(), config.GetStdin()))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container io")
	}
	defer func() {
		if retErr != nil {
			if err := containerIO.Close(); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to close container io %q", id)
			}
		}
	}()

	containerLabels := buildLabels(config.Labels, containerKindContainer)
	runtimeOptions, err := getRuntimeOptions(sandboxInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get runtime options")
	}
	opts = append(opts,
		containerd.WithSpec(spec),
		containerd.WithContainerLabels(containerLabels),
		containerd.WithContainerExtension(containerMetadataExtension, &meta),
		containerd.WithRuntime(sandboxInfo.Runtime.Name, runtimeOptions))

	var cntr containerd.Container
	if cntr, err = c.client.NewContainer(ctx, id, opts...); err != nil {
		return nil, errors.Wrap(err, "failed to create containerd container")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			if err := cntr.Delete(deferCtx, containerd.WithSnapshotCleanup); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to delete containerd container %q", id)
			}
		}
	}()

	status := containerstore.Status{CreatedAt: time.Now().UnixNano()}
	container, err := containerstore.NewContainer(meta,
		containerstore.WithStatus(status, containerRootDir),
		containerstore.WithContainer(cntr),
		containerstore.WithContainerIO(containerIO),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create internal container object for %q", id)
	}
	defer func() {
		if retErr != nil {
			// Cleanup container checkpoint on error.
			if err := container.Delete(); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to cleanup container checkpoint for %q", id)
			}
		}
	}()

	// Add container into container store.
	if err := c.containerStore.Add(container); err != nil {
		return nil, errors.Wrapf(err, "failed to add container %q into store", id)
	}

	return &runtime.CreateContainerResponse{ContainerId: id}, nil
}

func (c *criService) generateContainerSpec(id string, sandboxID string, sandboxPid uint32, netnsPath string, config *runtime.ContainerConfig,
	sandboxConfig *runtime.PodSandboxConfig, sandboxPlatform string, imageConfig *imagespec.ImageConfig) (*runtimespec.Spec, error) {
	// Creates a spec Generator with the default spec.
	ctx := ctrdutil.NamespacedContext()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, sandboxPlatform, &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}
	g := newSpecGenerator(spec)

	if err := setOCIProcessArgs(&g, config, imageConfig); err != nil {
		return nil, err
	}

	if config.GetWorkingDir() != "" {
		g.SetProcessCwd(config.GetWorkingDir())
	} else if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	g.SetProcessTerminal(config.GetTty())
	if sandboxPlatform == "linux/amd64" {
		g.AddProcessEnv("TERM", "xterm")
		if sandboxConfig.GetHostname() != "" {
			g.AddProcessEnv(hostnameEnv, sandboxConfig.GetHostname())
		}
	}

	// Apply envs from image config first, so that envs from container config
	// can override them.
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}
	for _, e := range config.GetEnvs() {
		g.AddProcessEnv(e.GetKey(), e.GetValue())
	}

	// Clear the root location since runhcs sets it on the mount path in the
	// guest.
	g.Config.Root = nil

	// Set the Network Namespace
	g.SetWindowsNetworkNamespace(netnsPath)

	// Forward any annotations from the orchestrator
	for k, v := range config.Annotations {
		g.AddAnnotation(k, v)
	}

	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeContainer)
	g.AddAnnotation(annotations.SandboxID, sandboxID)

	// Add OCI Mounts
	automanageVhdIndex := 0
	for _, m := range config.GetMounts() {
		var formattedDestination string
		if sandboxPlatform == "linux/amd64" {
			formattedDestination = strings.Replace(m.ContainerPath, "\\", "/", -1)
			//kubelet will prepend c: if it's running on Windows and there's no drive letter, so we need to strip it out
			if match, _ := regexp.MatchString("^[A-Za-z]:", formattedDestination); match {
				formattedDestination = formattedDestination[2:]
			}
		} else {
			formattedDestination = strings.Replace(m.ContainerPath, "/", "\\", -1)
		}

		mo := runtimespec.Mount{
			Source:      m.HostPath,
			Destination: formattedDestination,
			Options:     []string{"ro"},
		}
		if !m.Readonly {
			mo.Options[0] = "rw"
		}
		if strings.HasPrefix(mo.Source, `\\.\PHYSICALDRIVE`) {
			mo.Type = "physical-disk"
			if sandboxPlatform == "linux/amd64" {
				mo.Options = append(mo.Options, "bind")
			}
		} else if strings.HasPrefix(mo.Source, `\\.\pipe`) {
			// mo.Type == "" for pipe but we don't want to Stat the path.
			if sandboxPlatform == "linux/amd64" {
				return nil, errors.Errorf(`pipe mount.HostPath '%s' not supported for LCOW`, mo.Source)
			}
		} else if strings.HasPrefix(mo.Source, "sandbox://") {
			// mount source prefix sandbox:// is only supported with lcow
			if sandboxPlatform != "linux/amd64" {
				return nil, errors.Errorf(`sandbox://' mounts are only supported for LCOW`, mo.Source)
			}

			mo.Options = append(mo.Options, "bind")
			mo.Type = "bind"
		} else if strings.HasPrefix(mo.Source, "automanage-vhd://") {
			formattedSource, err := filepath.EvalSymlinks(strings.TrimPrefix(mo.Source, "automanage-vhd://"))
			if err != nil {
				return nil, errors.Wrapf(err, "failed to EvalSymlinks automanage-vhd:// mount.HostPath %q", mo.Source)
			}
			s, err := c.os.Stat(formattedSource)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to Stat automanage-vhd:// mount.HostPath %q", formattedSource)
			}
			if s.IsDir() {
				// TODO: JTERRY75 - This is a hack and needs to be removed. The
				// orchestrator should be controlling the entry host path. For
				// now if its a directory copy the template vhd and update the
				// source.
				if c.config.PluginConfig.AutoManageVHDTemplatePath == "" {
					return nil, errors.New("automange-vhd:// prefix is not supported with no 'AutoManageVHDTemplatePath' in config")
				}
				formattedSource = filepath.Join(formattedSource, fmt.Sprintf("%s-%s.%d.vhdx", sandboxID, id, automanageVhdIndex))
				if err := c.os.CopyFile(c.config.PluginConfig.AutoManageVHDTemplatePath, formattedSource, 0); err != nil {
					return nil, errors.Wrapf(err, "failed to copy automanage-vhd:// from %q to %q", c.config.PluginConfig.AutoManageVHDTemplatePath, formattedSource)
				}
				// increment our automanage index so source generated paths
				// don't collide.
				automanageVhdIndex++
			} else {
				ext := strings.ToLower(filepath.Ext(formattedSource))
				if ext != ".vhd" && ext != ".vhdx" {
					return nil, errors.Errorf("automanage-vhd:// prefix MUST have .vhd or .vhdx extension found: %q", ext)
				}
			}
			mo.Source = formattedSource
			mo.Type = "automanage-virtual-disk"
			if sandboxPlatform == "linux/amd64" {
				mo.Options = append(mo.Options, "bind")
			}
		} else if strings.HasPrefix(mo.Source, "vhd://") {
			formattedSource, err := filepath.EvalSymlinks(strings.TrimPrefix(mo.Source, "vhd://"))
			if err != nil {
				return nil, errors.Wrapf(err, "failed to EvalSymlinks vhd:// mount.HostPath %q", mo.Source)
			}
			s, err := c.os.Stat(formattedSource)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to Stat vhd:// mount.HostPath %q", formattedSource)
			}
			if s.IsDir() {
				return nil, errors.New("vhd:// prefix is only supported on file paths")
			}
			ext := strings.ToLower(filepath.Ext(formattedSource))
			if ext != ".vhd" && ext != ".vhdx" {
				return nil, errors.New("vhd:// prefix is only supported on file paths ending in .vhd or .vhdx")
			}
			mo.Source = formattedSource
			mo.Type = "virtual-disk"
			if sandboxPlatform == "linux/amd64" {
				mo.Options = append(mo.Options, "bind")
			}
		} else {
			//normalize the format of the host path
			formattedSource, err := filepath.EvalSymlinks(strings.Replace(m.HostPath, "/", "\\", -1))
			if err != nil {
				return nil, err
			}
			_, err = c.os.Stat(formattedSource)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to Stat mount.HostPath '%s'", formattedSource)
			}
			mo.Source = formattedSource
			if sandboxPlatform == "linux/amd64" {
				// Linux requires a folder/file to be bind mount.
				mo.Type = "bind"
				mo.Options = append(mo.Options, "rbind")
			}
		}

		g.AddMount(mo)
	}

	if sandboxPlatform == "linux/amd64" {
		securityContext := config.GetLinux().GetSecurityContext()
		if securityContext.GetPrivileged() {
			if !sandboxConfig.GetLinux().GetSecurityContext().GetPrivileged() {
				return nil, errors.New("no privileged container allowed in sandbox")
			}
			if err := setOCIPrivileged(&g, config); err != nil {
				return nil, err
			}
		}
		userstr, err := generateUserString(
			securityContext.GetRunAsUsername(),
			securityContext.GetRunAsUser(),
			securityContext.GetRunAsGroup())
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate user string")
		}
		if userstr == "" {
			// Lastly, since no user override was passed via CRI try to set via
			// OCI Image
			userstr = imageConfig.User
		}
		if userstr != "" {
			g.AddAnnotation("io.microsoft.lcow.userstr", userstr)
		}
		for _, group := range securityContext.GetSupplementalGroups() {
			g.AddProcessAdditionalGid(uint32(group))
		}
		setOCINamespaces(&g, securityContext.GetNamespaceOptions(), sandboxPid)
		g.SetProcessNoNewPrivileges(securityContext.GetNoNewPrivs())
	}

	return g.Config, nil
}

// setOCIDevicesPrivileged set device mapping with privilege.
func setOCIDevicesPrivileged(g *generator) error {
	// For Windows LCOW we cannot calculate the proper host device list. We
	// forward a custom annotation to the guest to signal it to calculate the
	// list itself.
	g.AddAnnotation("io.microsoft.virtualmachine.lcow.privileged", "true")
	return nil
}
