// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/pkg/guestagent/api"
)

type DockerEventMonitor struct {
	dockerClients []*client.Client
	// We maintain a record of all active containers because neither the stop nor
	// die events provide the port mapping. As API consumers, it's our responsibility
	// to track this information. The map uses the container ID as the key and
	// stores all published ports associated with that container as the value.
	runningContainers map[string][]*api.IPPort
}

func NewDockerEventMonitor(dockerSocketPaths []string) (*DockerEventMonitor, error) {
	const (
		maxRetries = 5
		retryDelay = 2 * time.Second
	)

	var dockerClients []*client.Client

	for _, socket := range dockerSocketPaths {
		logrus.Debugf("attempting to read Docker socket %s", socket)

		info, statErr := os.Stat(socket)
		if os.IsNotExist(statErr) {
			logrus.Debugf("Docker socket %s does not exist", socket)
			continue
		} else if statErr != nil {
			return nil, fmt.Errorf("error checking Docker socket %s: %w", socket, statErr)
		} else if info.IsDir() {
			logrus.Debugf("Docker socket %s is a directory, skipping", socket)
			continue
		}

		var cli *client.Client
		var err error

		for attempt := 1; attempt <= maxRetries; attempt++ {
			cli, err = client.NewClientWithOpts(client.WithHost(socket), client.WithAPIVersionNegotiation())
			if err == nil {
				if _, err = cli.Ping(context.Background()); err == nil {
					logrus.Infof("successfully connected to Docker daemon at %s", socket)
					dockerClients = append(dockerClients, cli)
					break
				}
			}

			logrus.Warnf("attempt %d/%d: failed to connect to Docker at %s: %s", attempt, maxRetries, socket, err)

			if attempt < maxRetries {
				select {
				case <-time.After(retryDelay):
				case <-context.Background().Done():
					logrus.Warn("retry canceled, context done")
					return nil, context.Canceled
				}
			} else {
				logrus.Errorf("failed to connect to Docker at %s after %d attempts: %v", socket, maxRetries, err)
			}
		}
	}

	if len(dockerClients) == 0 {
		logrus.Warn("no valid Docker clients created from provided sockets, please check the socket paths")
		return nil, nil
	}

	return &DockerEventMonitor{
		dockerClients:     dockerClients,
		runningContainers: make(map[string][]*api.IPPort),
	}, nil
}

func (d *DockerEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) error {
	errCh := make(chan error, len(d.dockerClients))
	for _, cli := range d.dockerClients {
		go func(c *client.Client) {
			if err := d.monitorClient(ctx, c, ch); err != nil {
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

func (d *DockerEventMonitor) monitorClient(ctx context.Context, cli *client.Client, ch chan *api.Event) error {
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
			container, err := cli.ContainerInspect(ctx, event.ID)
			if err != nil {
				logrus.Errorf("inspecting container [%v] failed: %v", event.ID, err)
				continue
			}
			portMap := container.NetworkSettings.NetworkSettingsBase.Ports
			logrus.Debugf("received an event: {Status: %+v ContainerID: %+v Ports: %+v}",
				event.Action,
				event.ID,
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
					d.runningContainers[event.ID] = ipPorts
					sendHostAgentEvent(false, ipPorts, ch)
				}
			case events.ActionStop, events.ActionDie:
				ipPorts, ok := d.runningContainers[event.ID]
				if !ok {
					continue
				}
				delete(d.runningContainers, event.ID)
				sendHostAgentEvent(true, ipPorts, ch)
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
		if len(container.Ports) != 0 {
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
			d.runningContainers[container.ID] = ipPorts
		}
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
