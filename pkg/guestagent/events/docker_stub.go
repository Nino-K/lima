//go:build !linux
// +build !linux

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

type DockerEventMonitor struct{}

func NewDockerEventMonitor(_ []string) (*DockerEventMonitor, error) {
	panic("Dockert event monitoring is not implemented on this platform")
}
