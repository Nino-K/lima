// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	containerdNamespace "github.com/containerd/containerd/namespaces"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/lima-vm/lima/v2/pkg/guestagent/api"
)

const (
	stateKey             = "nerdctl/state-dir"
	portsKey             = "nerdctl/ports"
	namespaceKey         = "nerdctl/namespace"
	defaultSocketTimeout = 5 * time.Second
)

type ContainerdEventMonitor struct {
	socketPaths            []string
	runningContainersMutex sync.Mutex
	runningContainers      map[string][]*api.IPPort
}

func NewContainerdEventMonitor(socketPaths []string) *ContainerdEventMonitor {
	return &ContainerdEventMonitor{
		socketPaths:       socketPaths,
		runningContainers: make(map[string][]*api.IPPort),
	}
}

func (c *ContainerdEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event, errCh chan error) {
	var wg sync.WaitGroup
	for _, socket := range c.socketPaths {
		wg.Add(1)
		go func(socket string) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				default:
				}
				info, err := os.Stat(socket)
				if err != nil {
					if os.IsNotExist(err) {
						logrus.Warnf("containerd socket %s does not exist", socket)
						// Wait for 2s before retrying again
						time.Sleep(2 * time.Second)
					} else {
						logrus.Errorf("failed to stat containerd socket %s: %v", socket, err)
					}
					continue
				}
				if info.IsDir() {
					errCh <- fmt.Errorf("containerd socket path %s is a directory", socket)
					// this is unrecoverable
					return
				}
				cli, err := containerd.New(socket, containerd.WithDefaultNamespace(containerdNamespace.Default))
				if err != nil {
					logrus.Warnf("failed to create client for socket %s: %v", socket, err)
					continue
				}
				clientCtx, cancel := context.WithTimeout(ctx, defaultSocketTimeout)
				serving, serveErr := cli.IsServing(clientCtx)
				cancel()
				if serveErr != nil || !serving {
					logrus.Warnf("containerd daemon not serving on socket %s: %v. Retrying in 5s...", socket, serveErr)
					cli.Close()
					time.Sleep(5 * time.Second)
					continue
				}
				logrus.Infof("successfully connected to containerd on socket %s", socket)
				if err := c.runMonitorClient(ctx, cli, ch); err != nil {
					logrus.Errorf("containerd port monitoring for socket: %s failed: %s", socket, err)
					errCh <- err
				}
				cli.Close()
			}
		}(socket)
	}
	wg.Wait()
}

func (c *ContainerdEventMonitor) runMonitorClient(ctx context.Context, cli *containerd.Client, ch chan *api.Event) error {
	subscribeFilters := []string{
		`topic=="/tasks/start"`,
		`topic=="/containers/update"`,
		`topic=="/tasks/exit"`,
	}
	msgCh, errCh := cli.Subscribe(ctx, subscribeFilters...)

	if err := c.initializeRunningContainers(ctx, cli, ch); err != nil {
		logrus.Errorf("failed to initialize existing containers published ports: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancellation: %w", ctx.Err())

		case err := <-errCh:
			return fmt.Errorf("receiving container event failed: %w", err)

		case envelope := <-msgCh:
			logrus.Debugf("received an event: %+v", envelope.Topic)
			switch envelope.Topic {
			case "/tasks/start":
				startTask := &events.TaskStart{}
				err := proto.Unmarshal(envelope.Event.GetValue(), startTask)
				if err != nil {
					logrus.Errorf("failed to unmarshal TaskStart event: %v", err)
					continue
				}

				ipPorts, err := c.createIPPort(ctx, cli, envelope.Namespace, startTask.ContainerID)
				if err != nil {
					logrus.Errorf("creating IPPorts for start task ContainerID=%s failed: %s", startTask.ContainerID, err)
					continue
				}

				logrus.Debugf("received the following startTask: ContainerID=%s ipPorts=%+v", startTask.ContainerID, ipPorts)

				if len(ipPorts) != 0 {
					sendHostAgentEvent(false, ipPorts, ch)
					c.runningContainersMutex.Lock()
					c.runningContainers[startTask.ContainerID] = ipPorts
					c.runningContainersMutex.Unlock()
				}

			case "/containers/update":
				cuEvent := &events.ContainerUpdate{}
				err := proto.Unmarshal(envelope.Event.GetValue(), cuEvent)
				if err != nil {
					logrus.Errorf("failed to unmarshal container update event: %v", err)
					continue
				}

				ipPorts, err := c.createIPPort(ctx, cli, envelope.Namespace, cuEvent.ID)
				if err != nil {
					logrus.Errorf("creating IPPorts, for the following exit task: %v failed: %s", cuEvent, err)
					continue
				}

				logrus.Debugf("received the following updateTask: %v for: %v", cuEvent, ipPorts)

				c.runningContainersMutex.Lock()
				if existingipPorts, ok := c.runningContainers[cuEvent.ID]; ok {
					if !ipPortsEqual(ipPorts, existingipPorts) {
						// first remove the existing entry
						sendHostAgentEvent(true, existingipPorts, ch)
						// then update with the new entry
						sendHostAgentEvent(false, ipPorts, ch)
						c.runningContainers[cuEvent.ID] = ipPorts
					}
				}
				c.runningContainersMutex.Unlock()
			case "/tasks/exit":
				exitTask := &events.TaskExit{}
				err := proto.Unmarshal(envelope.Event.GetValue(), exitTask)
				if err != nil {
					logrus.Errorf("failed to unmarshal container's exit task: %v", err)
					continue
				}

				container, err := cli.LoadContainer(ctx, exitTask.ContainerID)
				if err != nil {
					if errdefs.IsNotFound(err) {
						logrus.Debugf("container: %s in namespace: %s not found, deleting port mapping", exitTask.ContainerID, envelope.Namespace)
						c.deleteRunningContainer(exitTask.ContainerID, ch)
						continue
					}
					logrus.Errorf("failed to get the container %s from namespace %s: %s", exitTask.ContainerID, envelope.Namespace, err)
					continue
				}

				tsk, err := container.Task(ctx, nil)
				if err != nil {
					if errdefs.IsNotFound(err) {
						logrus.Debugf("task for container %s in namespace %s not found, deleting port mapping", exitTask.ContainerID, envelope.Namespace)
						c.deleteRunningContainer(exitTask.ContainerID, ch)
						continue
					}
					logrus.Errorf("failed to get the task for container %s: %s", exitTask.ContainerID, err)
					continue
				}
				status, err := tsk.Status(ctx)
				if err != nil {
					logrus.Errorf("failed to get the task status for container %s: %s", exitTask.ContainerID, err)
					continue
				}

				if status.Status == containerd.Running {
					logrus.Debugf("container %s is still running, but received exit event with status %d", exitTask.ContainerID, exitTask.ExitStatus)
					continue
				}

				c.deleteRunningContainer(exitTask.ContainerID, ch)
			}
		}
	}
}

func (c *ContainerdEventMonitor) deleteRunningContainer(containerID string, ch chan *api.Event) {
	c.runningContainersMutex.Lock()
	defer c.runningContainersMutex.Unlock()
	if ipPorts, ok := c.runningContainers[containerID]; ok {
		delete(c.runningContainers, containerID)
		logrus.Debugf("deleted container %s from running containers", containerID)
		sendHostAgentEvent(true, ipPorts, ch)
	} else {
		logrus.Debugf("container %s not found in running containers", containerID)
	}
}

func (c *ContainerdEventMonitor) initializeRunningContainers(ctx context.Context, cli *containerd.Client, ch chan *api.Event) error {
	containers, err := cli.Containers(ctx)
	if err != nil {
		return err
	}

	for _, container := range containers {
		task, err := container.Task(ctx, nil)
		if err != nil || task == nil {
			logrus.Errorf("failed getting container %s task: %s", container.ID(), err)
			continue
		}

		status, err := task.Status(ctx)
		if err != nil || status.Status != containerd.Running {
			logrus.Errorf("failed getting container %s task status: %s", container.ID(), err)
			continue
		}

		labels, err := container.Labels(ctx)
		if err != nil {
			logrus.Errorf("failed getting container %s labels: %s", container.ID(), err)
			continue
		}

		namespace, ok := labels[namespaceKey]
		if !ok {
			logrus.Errorf("container %s does not have a namespace label", container.ID())
			continue
		}
		ipPorts, err := c.createIPPort(ctx, cli, namespace, container.ID())
		if err != nil {
			logrus.Errorf("creating IPPorts, while initializing containers the following: %v failed: %s", container.ID(), err)
		}

		sendHostAgentEvent(false, ipPorts, ch)
		c.runningContainersMutex.Lock()
		c.runningContainers[container.ID()] = ipPorts
		c.runningContainersMutex.Unlock()
	}

	return nil
}

func (c *ContainerdEventMonitor) createIPPort(ctx context.Context, cli *containerd.Client, namespace, containerID string) ([]*api.IPPort, error) {
	container, err := cli.ContainerService().Get(
		containerdNamespace.WithNamespace(ctx, namespace), containerID)
	if err != nil {
		return nil, err
	}

	var ipPorts []*api.IPPort

	// For backward compatibility, we first check if the container has the nerdctl/ports label.
	// If it does, we parse it and return the IPPorts.
	containerPorts, ok := container.Labels[portsKey]
	if ok {
		ipPorts, err = extractIPPortsFromLabel(containerPorts)
		if err != nil {
			return nil, fmt.Errorf("extracting IPPorts from container %s ports label failed: %w", containerID, err)
		}
		return ipPorts, nil
	}
	// If the label is not present, we check the network config in the following path:
	// <DATAROOT>/<ADDRHASH>/containers/<NAMESPACE>/<CID>/network-config.json
	stateDir, ok := container.Labels[stateKey]
	if !ok {
		return nil, fmt.Errorf("container %s does not have a state directory label", containerID)
	}
	content, err := os.ReadFile(fmt.Sprintf("%s/network-config.json", stateDir))
	if err != nil {
		return nil, fmt.Errorf("failed reading network-config.json in dir %s for container %s: %w", stateDir, containerID, err)
	}
	return extractIPPortsFromNetworkConfig(content)
}

func extractIPPortsFromLabel(jsonPorts string) ([]*api.IPPort, error) {
	var ports []Port
	err := json.Unmarshal([]byte(jsonPorts), &ports)
	if err != nil {
		return nil, err
	}

	var ipPorts []*api.IPPort
	for _, port := range ports {
		ipPorts = append(ipPorts, &api.IPPort{
			Protocol: strings.ToLower(port.Protocol),
			Ip:       port.HostIP,
			Port:     int32(port.HostPort),
		})
	}

	return ipPorts, nil
}

func extractIPPortsFromNetworkConfig(jsonStr []byte) ([]*api.IPPort, error) {
	var cfg NetworkConfig
	if err := json.Unmarshal(jsonStr, &cfg); err != nil {
		return nil, err
	}

	var ipPorts []*api.IPPort
	for _, port := range cfg.PortMappings {
		ipPorts = append(ipPorts, &api.IPPort{
			Protocol: strings.ToLower(port.Protocol),
			Ip:       port.HostIP,
			Port:     int32(port.HostPort),
		})
	}
	return ipPorts, nil
}

func ipPortsEqual(a, b []*api.IPPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Protocol != b[i].Protocol || a[i].Ip != b[i].Ip || a[i].Port != b[i].Port {
			return false
		}
	}
	return true
}

// Port is representing nerdctl/ports entry in the
// event envelope's labels.
type Port struct {
	HostPort      int    `json:"HostPort"`
	ContainerPort int    `json:"ContainerPort"`
	Protocol      string `json:"Protocol"`
	HostIP        string `json:"HostIP"`
}

// NetworkConfig is representing the network config
// of a container that is found in the following Path:
// <DATAROOT>/<ADDRHASH>/containers/<NAMESPACE>/<CID>/network-config.json.
type NetworkConfig struct {
	PortMappings []Port `json:"portMappings"`
}
