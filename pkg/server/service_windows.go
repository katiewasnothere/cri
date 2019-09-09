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
	"strings"

	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

type Isolation string

const (
	IsolationUnknown Isolation = ""
	IsolationProcess           = "process"
	IsolationHyperV            = "hyperv"
)

// isApparmorEnabled is not supported on Windows.
func isApparmorEnabled() bool {
	return false
}

// isSeccompEnabled is not supported on Windows.
func isSeccompEnabled() bool {
	return false
}

// doRunningInUserNSCheck is not supported on Windows.
func doRunningInUserNSCheck(disableCGroup, apparmorEnabled, restrictOOMScoreAdj bool) {
}

// doSelinux is not supported on Windows.
func doSelinux(enable bool) {
}

func (c *criService) getDefaultSnapshotterForSandbox(cfg *runtime.PodSandboxConfig) string {
	platform := c.getDefaultSnapshotterPlatform(cfg)
	return c.getDefaultSnapshotterForPlatform(platform)
}

func (c *criService) getDefaultSnapshotterPlatform(cfg *runtime.PodSandboxConfig) string {
	var (
		platform string
	)
	if cfg != nil {
		platform = strings.Replace(cfg.Labels["sandbox-platform"], "-", "/", -1)
	}
	return platform
}

func (c *criService) getDefaultSnapshotterForPlatform(platform string) string {
	if platform == "linux/amd64" {
		return "windows-lcow"
	} else if platform == "windows/amd64" {
		return "windows"
	}
	return c.config.ContainerdConfig.Snapshotter
}
