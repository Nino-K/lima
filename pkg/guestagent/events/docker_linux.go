// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/v2/pkg/guestagent/api"
)

type DockerEventMonitor struct {
	dockerSocketPaths      []string
	dockerClients          []*client.Client
	runningContainersMutex sync.Mutex
	// dockerClients holds the list of Docker clients connected to the specified sockets.
	// We maintain a record of all active containers because neither the stop nor
	// die events provide the port mapping. As an API consumer, it's our responsibility
	// to track this information. The map uses the container ID as the key and
	// stores all published ports associated with that container as the value.
	runningContainers map[string][]*api.IPPort
}

func NewDockerEventMonitor(dockerSocketPaths []string) *DockerEventMonitor {
	return &DockerEventMonitor{
		dockerSocketPaths: dockerSocketPaths,
		runningContainers: make(map[string][]*api.IPPort),
	}
}

// MonitorPorts starts monitoring Docker ports on the specified sockets.
// It connects to the Docker daemon, listens for container events, and sends
// port mapping events to the provided channel.
// It returns an error if it fails to connect to any of the Docker sockets or if
// it encounters an error while monitoring the ports.
func (d *DockerEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) error {
	if err := d.tryConnectClient(ctx); err != nil {
		return err
	}

	errCh := make(chan error, len(d.dockerClients))
	var wg sync.WaitGroup
	for _, cli := range d.dockerClients {
		wg.Add(1)
		go func(c *client.Client) {
			defer wg.Done()
			if err := d.monitorClient(ctx, c, ch); err != nil {
				errCh <- fmt.Errorf("monitoring ports failed: %w", err)
			}
		}(cli)
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

// Close closes all Docker clients and clears the running containers map.
func (d *DockerEventMonitor) Close() {
	for _, cli := range d.dockerClients {
		if err := cli.Close(); err != nil {
			logrus.Errorf("failed to close Docker client: %v", err)
		}
	}
	d.dockerClients = nil
	d.runningContainers = nil
}

func (d *DockerEventMonitor) tryConnectClient(ctx context.Context) error {
	const (
		maxRetries = 20
		retryDelay = 3 * time.Second
	)

	for _, socket := range d.dockerSocketPaths {
		logrus.Debugf("attempting to read Docker socket %s", socket)

		var (
			cli       *client.Client
			lastErr   error
			socketURL string
		)

		for attempt := 1; attempt <= maxRetries; attempt++ {
			info, statErr := os.Stat(socket)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					logrus.Warnf("attempt %d/%d: Docker socket %s does not exist: %s", attempt, maxRetries, socket, statErr)
					lastErr = statErr
				}
			} else if info.IsDir() {
				logrus.Warnf("docker socket %s is a directory, skipping", socket)
				// No point retrying if it's a directory, break early for this socket
				lastErr = fmt.Errorf("docker socket %s is a directory", socket)
				break
			} else {
				if !strings.HasPrefix(socket, "unix://") {
					if strings.HasPrefix(socket, "/") {
						socketURL = "unix://" + strings.Trim(socket, "/")
					} else {
						socketURL = "unix://" + socket
					}
				}
				cli, lastErr = client.NewClientWithOpts(client.WithHost(socketURL), client.WithAPIVersionNegotiation())
				if lastErr == nil {
					_, lastErr = cli.Ping(ctx)
					if lastErr != nil {
						logrus.Warnf("attempt %d/%d: failed to ping Docker at %s: %v", attempt, maxRetries, socketURL, lastErr)
					}
				}

				if lastErr == nil {
					logrus.Infof("successfully connected to Docker daemon at %s", socketURL)
					d.dockerClients = append(d.dockerClients, cli)
					break
				}
			}

			logrus.Warnf("attempt %d/%d: failed to connect to Docker at %s: %v", attempt, maxRetries, socket, lastErr)

			select {
			case <-ctx.Done():
				logrus.Warn("retry canceled, context done")
				return ctx.Err()
			case <-time.After(retryDelay):
				continue
			}
		}

		if cli == nil {
			logrus.Errorf("failed to connect to Docker at %s after %d attempts: %v", socketURL, maxRetries, lastErr)
		}
	}

	if len(d.dockerClients) == 0 {
		return &NoClientError{}
	}
	return nil
}

func (d *DockerEventMonitor) monitorClient(ctx context.Context, cli *client.Client, ch chan *api.Event) error {
	socket := cli.DaemonHost()
	backoff := time.Second * 2
	maxBackoff := time.Minute

	for {
		err := d.runMonitorClient(ctx, cli, ch)
		if err != nil {
			logrus.Errorf("monitoring failed for Docker socket %s: %v", socket, err)
		}

		select {
		case <-ctx.Done():
			logrus.Infof("context cancelled for monitorClient (socket: %s)", socket)
			return ctx.Err()
		default:
			logrus.Infof("retrying connection to Docker socket %s in %s", socket, backoff)

			wait := time.After(backoff)
			<-wait
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			newCli, err := client.NewClientWithOpts(
				client.WithHost(socket),
				client.WithAPIVersionNegotiation(),
			)
			if err != nil {
				logrus.Errorf("failed to reconnect Docker client at %s: %v", socket, err)
				continue
			}
			// close the old client
			cli.Close()
			cli = newCli
		}
	}
}

func (d *DockerEventMonitor) runMonitorClient(ctx context.Context, cli *client.Client, ch chan *api.Event) error {
	defer cli.Close()

	if err := d.initializeRunningContainers(ctx, cli, ch); err != nil {
		logrus.Errorf("failed to initialize existing docker container published ports: %s", err)
	}

	msgCh, errCh := cli.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", string(types.ContainerObject)),
			filters.Arg("event", string(events.ActionStart)),
			filters.Arg("event", string(events.ActionStop)),
			filters.Arg("event", string(events.ActionDie))),
	})

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancellation: %w", ctx.Err())

		case event := <-msgCh:
			container, err := cli.ContainerInspect(ctx, event.Actor.ID)
			if err != nil {
				logrus.Errorf("inspecting container [%v] failed: %v", event.Actor.ID, err)
				continue
			}
			portMap := container.NetworkSettings.NetworkSettingsBase.Ports
			logrus.Debugf("received an event: {Status: %+v ContainerID: %+v Ports: %+v}",
				event.Action,
				event.Actor.ID,
				portMap)

			switch event.Action {
			case events.ActionStart:
				if len(portMap) != 0 {
					validatePortMapping(portMap)
					ipPorts, err := convertToIPPort(portMap)
					if err != nil {
						logrus.Errorf("converting docker's portMapping: %+v to api.IPPort: %v failed: %s", portMap, ipPorts, err)
						continue
					}
					logrus.Infof("successfully converted PortMapping:%+v to IPPorts: %+v", portMap, ipPorts)
					d.runningContainersMutex.Lock()
					d.runningContainers[event.Actor.ID] = ipPorts
					d.runningContainersMutex.Unlock()
					sendHostAgentEvent(false, ipPorts, ch)
				}
			case events.ActionStop, events.ActionDie:
				d.runningContainersMutex.Lock()
				ipPorts, ok := d.runningContainers[event.Actor.ID]
				if ok {
					delete(d.runningContainers, event.Actor.ID)
				}
				d.runningContainersMutex.Unlock()
				if ok {
					sendHostAgentEvent(true, ipPorts, ch)
				}
			}
		case err := <-errCh:
			return fmt.Errorf("receiving container event failed: %w", err)
		}
	}
}

func (d *DockerEventMonitor) initializeRunningContainers(ctx context.Context, cli *client.Client, ch chan *api.Event) error {
	containers, err := cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("status", "running")),
	})
	if err != nil {
		return err
	}

	for _, container := range containers {
		if len(container.Ports) == 0 {
			continue
		}
		var ipPorts []*api.IPPort
		for _, port := range container.Ports {
			if port.IP == "" || port.PublicPort == 0 {
				continue
			}

			ipPorts = append(ipPorts, &api.IPPort{
				Protocol: strings.ToLower(port.Type),
				Ip:       port.IP,
				Port:     int32(port.PublicPort),
			})
		}
		sendHostAgentEvent(false, ipPorts, ch)
		d.runningContainersMutex.Lock()
		d.runningContainers[container.ID] = ipPorts
		d.runningContainersMutex.Unlock()
	}
	return nil
}

func convertToIPPort(portMap nat.PortMap) ([]*api.IPPort, error) {
	var ipPorts []*api.IPPort
	for key, portBindings := range portMap {
		for _, portBinding := range portBindings {
			hostPort, err := strconv.ParseInt(portBinding.HostPort, 10, 32)
			if err != nil {
				return ipPorts, err
			}
			if portBinding.HostIP == "" || hostPort == 0 {
				continue
			}

			logrus.Debugf("converted the following PortMapping to IPPort, containerPort:%v HostPort:%v IP:%v Protocol:%v",
				key.Port(), portBinding.HostPort, portBinding.HostIP, key.Proto())

			ipPorts = append(ipPorts, &api.IPPort{
				Protocol: strings.ToLower(key.Proto()),
				Ip:       portBinding.HostIP,
				Port:     int32(hostPort),
			})
		}
	}
	return ipPorts, nil
}

// Removes entries in port mapping that do not hold any values
// for IP and Port e.g 9000/tcp:[].
func validatePortMapping(portMap nat.PortMap) {
	for k, v := range portMap {
		if len(v) == 0 {
			logrus.Debugf("removing entry: %v from the portmappings: %v", k, portMap)
			delete(portMap, k)
		}
	}
}
