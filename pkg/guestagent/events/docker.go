package events

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/lima-vm/lima/pkg/guestagent/api"
	"github.com/sirupsen/logrus"
)

type DockerEventMonitor struct {
	dockerClient *client.Client
	// We maintain a record of all active containers because neither the stop nor
	// die events provide the port mapping. As API consumers, it's our responsibility
	// to track this information. The map uses the container ID as the key and
	// stores all published ports associated with that container as the value.
	runningContainers map[string][]*api.IPPort
}

func NewDockerEventMonitor() *DockerEventMonitor {
	return &DockerEventMonitor{
		runningContainers: make(map[string][]*api.IPPort),
	}
}

func (d *DockerEventMonitor) createAndVerifyClient(ctx context.Context) (bool, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logrus.Tracef("error creating Docker client: %s", err)
		return false, nil
	}

	_, err = cli.Info(ctx)
	if err != nil {
		logrus.Tracef("error getting Docker info: %s", err)
		return false, nil
	}

	d.dockerClient = cli
	logrus.Info("successfully connected to docker daemon")
	return true, nil
}

func (d *DockerEventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) error {
	if err := tryGetClient(ctx, d.createAndVerifyClient); err != nil {
		return fmt.Errorf("failed getting docker client: %w", err)
	}
	defer d.dockerClient.Close()

	if err := d.initializeRunningContainers(ctx, ch); err != nil {
		logrus.Errorf("failed to initialize existing docker container published ports: %s", err)
	}

	msgCh, errCh := d.dockerClient.Events(ctx, events.ListOptions{
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
			container, err := d.dockerClient.ContainerInspect(ctx, event.ID)
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
					sendEvent(false, ipPorts, ch)
				}
			case events.ActionStop, events.ActionDie:
				ipPorts, ok := d.runningContainers[event.ID]
				if !ok {
					continue
				}
				delete(d.runningContainers, event.ID)
				sendEvent(true, ipPorts, ch)
			}
		case err := <-errCh:
			return fmt.Errorf("receiving container event failed: %w", err)
		}
	}
}

func (d *DockerEventMonitor) initializeRunningContainers(ctx context.Context, ch chan *api.Event) error {
	containers, err := d.dockerClient.ContainerList(ctx, container.ListOptions{
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
			sendEvent(false, ipPorts, ch)
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
