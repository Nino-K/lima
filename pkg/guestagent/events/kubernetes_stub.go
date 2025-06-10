//go:build !linux
// +build !linux

// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

type KubeServiceWatcher struct{}

func NewKubeServiceWatcher() *KubeServiceWatcher {
	panic("NewKubeServiceWatcher is not implemented on this platform")
}
