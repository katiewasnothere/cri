// +build !windows

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
	runcapparmor "github.com/opencontainers/runc/libcontainer/apparmor"
	runcseccomp "github.com/opencontainers/runc/libcontainer/seccomp"
	runcsystem "github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/sirupsen/logrus"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

// isApparmorEnabled is not supported on Windows.
func isApparmorEnabled() bool {
	return runcapparmor.IsEnabled()
}

// isSeccompEnabled is not supported on Windows.
func isSeccompEnabled() bool {
	return runcseccomp.IsEnabled()
}

func doRunningInUserNSCheck(disableCGroup, apparmorEnabled, restrictOOMScoreAdj bool) {
	if runcsystem.RunningInUserNS() {
		if !(disableCGroup && !apparmorEnabled && restrictOOMScoreAdj) {
			logrus.Warn("Running containerd in a user namespace typically requires disable_cgroup, disable_apparmor, restrict_oom_score_adj set to be true")
		}
	}
}

func doSelinux(enable bool) {
	if enable {
		if !selinux.GetEnabled() {
			logrus.Warn("Selinux is not supported")
		}
	} else {
		selinux.SetDisabled()
	}
}

func (c *criService) getDefaultSnapshotterForSandbox(_ *runtime.PodSandboxConfig) string {
	return c.config.ContainerdConfig.Snapshotter
}

func (c *criService) getDefaultSnapshotterPlatform(cfg *runtime.PodSandboxConfig) string {
	return platforms.DefaultString()
}
