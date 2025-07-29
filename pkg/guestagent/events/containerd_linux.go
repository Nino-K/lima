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
	socketPaths []string
	// clients holds the list of containerd clients connected to the specified sockets.
	clients                map[string]*containerd.Client
	runningContainersMutex sync.Mutex
	runningContainers      map[string][]*api.IPPort
}

func NewContainerdEventMonitor(socketPaths []string) *ContainerdEventMonitor {
	return &ContainerdEventMonitor{
		socketPaths:       socketPaths,
		clients:           make(map[string]*containerd.Client),
		runningContainers: make(map[string][]*api.IPPort),
	}
}

func (c *ContainerdEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) error {
	if err := c.tryConnectClient(ctx); err != nil {
		return err
	}

	errCh := make(chan error, len(c.clients))
	var wg sync.WaitGroup
	for socket, cli := range c.clients {
		wg.Add(1)
		go func(socket string, client *containerd.Client) {
			defer wg.Done()
			defer client.Close()
			if err := c.monitorClient(ctx, socket, client, ch); err != nil {
				errCh <- fmt.Errorf("monitoring ports failed: %w", err)
			}
		}(socket, cli)
	}
	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-allDone:
		close(errCh)
		var errs []error
		for err := range errCh {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return fmt.Errorf("errors occurred during monitoring: %v", errs)
		}
		return nil
	}
}

// Close closes all containerd clients and clears the running containers map.
func (c *ContainerdEventMonitor) Close() {
	for _, cli := range c.clients {
		if err := cli.Close(); err != nil {
			logrus.Errorf("failed to close containerd client: %v", err)
		}
	}
	c.clients = nil
	c.runningContainersMutex.Lock()
	defer c.runningContainersMutex.Unlock()
	c.runningContainers = nil
}

func (c *ContainerdEventMonitor) tryConnectClient(ctx context.Context) error {
	const (
		maxRetries = 10
		retryDelay = 3 * time.Second
	)

	for _, socket := range c.socketPaths {
		logrus.Debugf("reading containerd socket %s", socket)
		for attempt := 1; attempt <= maxRetries; attempt++ {
			info, statErr := os.Stat(socket)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					logrus.Warnf("attempt %d/%d: containerd socket %s does not exist", attempt, maxRetries, socket)
				}
			} else {
				if info.IsDir() {
					logrus.Errorf("socket %s is a directory", socket)
					// Skip retry if the socket is a directory
					break
				}
				cli, err := containerd.New(socket, containerd.WithDefaultNamespace(containerdNamespace.Default))
				if err != nil {
					logrus.Warnf("attempt %d/%d: failed to create client for socket %s: %v", attempt, maxRetries, socket, err)
				} else {
					clientCtx, cancel := context.WithTimeout(ctx, defaultSocketTimeout)
					serving, serveErr := cli.IsServing(clientCtx)
					cancel()

					if serveErr == nil && serving {
						logrus.Infof("successfully connected to containerd daemon at %s (attempt %d)", socket, attempt)
						c.clients[socket] = cli
						break
					}

					logrus.Warnf("attempt %d/%d: containerd client at %s not serving: %v", attempt, maxRetries, socket, serveErr)
					cli.Close()
				}
			}

			select {
			case <-time.After(retryDelay):
				continue
			case <-ctx.Done():
				logrus.Warn("retry canceled, context done")
				return ctx.Err()
			}
		}
	}

	if len(c.clients) == 0 {
		return &NoClientError{}
	}
	return nil
}

func (c *ContainerdEventMonitor) monitorClient(ctx context.Context, socket string, cli *containerd.Client, ch chan *api.Event) error {
	backoff := time.Second * 2
	maxBackoff := time.Minute
	for {
		if err := c.runMonitorClient(ctx, cli, ch); err != nil {
			logrus.Errorf("monitoring client failed: %v", err)
		}

		select {
		case <-ctx.Done():
			logrus.Info("context done, stopping monitoring")
			return ctx.Err()
		default:
			logrus.Infof("retrying connection to containerd socket %s in %s", socket, backoff)

			err := cli.Reconnect()
			if err == nil {
				logrus.Infof("reconnected to containerd socket %s successfully", socket)
				backoff = time.Second * 2 // reset backoff on successful reconnect
				continue
			}
			logrus.Warnf("failed to reconnect to containerd socket %s: %v", socket, err)

			wait := time.After(backoff)
			<-wait
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
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

				ipPorts, err := c.createIPPort(ctx, cli, envelope.Namespace, exitTask.ContainerID)
				if err != nil {
					logrus.Errorf("creating IPPorts, for the following exit task: %v failed: %s", exitTask, err)
					continue
				}

				logrus.Debugf("received the following exitTask: %v for: %v", exitTask, ipPorts)

				if len(ipPorts) != 0 {
					sendHostAgentEvent(true, ipPorts, ch)
					c.runningContainersMutex.Lock()
					delete(c.runningContainers, exitTask.ContainerID)
					c.runningContainersMutex.Unlock()
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
