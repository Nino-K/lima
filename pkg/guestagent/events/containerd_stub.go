//go:build !linux
// +build !linux

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

type ContainerdEventMonitor struct{}

func NewContainerdEventMonitor(_ []string) (*ContainerdEventMonitor, error) {
	panic("Containerd event monitoring is not implemented on this platform")
}
