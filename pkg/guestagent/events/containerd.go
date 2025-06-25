// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	containerdNamespace "github.com/containerd/containerd/namespaces"
	"github.com/gogo/protobuf/proto"
	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/pkg/guestagent/api"
)

const (
	portsKey             = "nerdctl/ports"
	namespaceKey         = "nerdctl/namespace"
	defaultSocketTimeout = 5 * time.Second
)

type ContainerdEventMonitor struct {
	clients           []*containerd.Client
	runningContainers map[string][]*api.IPPort
}

func NewContainerdEventMonitor(socketPaths []string) (*ContainerdEventMonitor, error) {
	var clients []*containerd.Client
	for _, socket := range socketPaths {
		logrus.Debugf("reading containerd socket %s", socket)
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			logrus.Debugf("containerd socket %s does not exist", socket)
			continue
		} else if err != nil {
			return nil, fmt.Errorf("error checking containerd socket %s: %w", socket, err)
		}
		cli, err := containerd.New(socket, containerd.WithDefaultNamespace(containerdNamespace.Default))
		if err != nil {
			logrus.Errorf("failed to create Containerd client for socket %s: %s", socket, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultSocketTimeout)
		serving, err := cli.IsServing(ctx)
		cancel()
		if err == nil && serving {
			logrus.Infof("successfully connected to containerd daemon at %s", socket)
			clients = append(clients, cli)
		} else {
			logrus.Errorf("error checking if containerd client for socket %s is serving: %s", socket, err)
			cli.Close()
		}
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("no valid Containerd clients created from provided sockets: %v", socketPaths)
	}
	return &ContainerdEventMonitor{
		clients:           clients,
		runningContainers: make(map[string][]*api.IPPort),
	}, nil
}

func (c *ContainerdEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) error {
	errCh := make(chan error, len(c.clients))

	for _, cli := range c.clients {
		go func(client *containerd.Client) {
			defer client.Close()
			if err := c.monitorClient(ctx, client, ch); err != nil {
				errCh <- fmt.Errorf("monitoring ports failed: %w", err)
			}
		}(cli)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (c *ContainerdEventMonitor) monitorClient(ctx context.Context, cli *containerd.Client, ch chan *api.Event) error {
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
					logrus.Errorf("failed to unmarshal container's start task: %v", err)
					continue
				}

				ipPorts, err := c.createIPPort(ctx, cli, envelope.Namespace, startTask.ContainerID)
				if err != nil {
					logrus.Errorf("creating IPPorts, for the following start task: %v failed: %s", startTask, err)
					continue
				}

				logrus.Debugf("received the following startTask: %v for: %v", startTask, ipPorts)

				if len(ipPorts) != 0 {
					sendHostAgentEvent(false, ipPorts, ch)
					c.runningContainers[startTask.ContainerID] = ipPorts
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

				if exsitingipPorts, ok := c.runningContainers[cuEvent.ID]; ok {
					if !ipPortsEqual(ipPorts, exsitingipPorts) {
						// first remove the existing entry
						sendHostAgentEvent(true, exsitingipPorts, ch)
						// then update with the new entry
						sendHostAgentEvent(false, ipPorts, ch)
						c.runningContainers[cuEvent.ID] = ipPorts
					}
				}
			case "/tasks/exit":
				exitTask := &events.TaskExit{}
				err := proto.Unmarshal(envelope.Event.GetValue(), exitTask)
				if err != nil {
					logrus.Errorf("failed to unmarshal container's exit task: %v", err)
					continue
				}

				ipPorts, err := c.createIPPort(ctx, cli, envelope.Namespace, exitTask.ContainerID)
				if err != nil {
					logrus.Errorf("creating IPPorts, for the following exit task: %v failed: %s", exitTask, err)
					continue
				}

				logrus.Debugf("received the following exitTask: %v for: %v", exitTask, ipPorts)

				if len(ipPorts) != 0 {
					sendHostAgentEvent(true, ipPorts, ch)
					delete(c.runningContainers, exitTask.ContainerID)
				}
			}
		}
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

		ipPorts, err := c.createIPPort(ctx, cli, labels[namespaceKey], container.ID())
		if err != nil {
			logrus.Errorf("creating IPPorts, while initializing containers the following: %v failed: %s", container.ID(), err)
		}

		sendHostAgentEvent(false, ipPorts, ch)
		c.runningContainers[container.ID()] = ipPorts
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

	containerPorts := container.Labels[portsKey]
	if containerPorts == "" {
		return ipPorts, nil
	}

	var ports []Port
	err = json.Unmarshal([]byte(containerPorts), &ports)
	if err != nil {
		return nil, err
	}

	for _, port := range ports {
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
	HostPort      int
	ContainerPort int
	Protocol      string
	HostIP        string
}
