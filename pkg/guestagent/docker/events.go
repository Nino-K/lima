package docker

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/lima-vm/lima/pkg/guestagent/api"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sirupsen/logrus"
)

const (
	startEvent = "start"
	stopEvent  = "stop"
	// die event is a confirmation of kill event.
	dieEvent = "die"
)

type EventMonitor struct {
	dockerClient *client.Client
	// We maintain a record of all active containers because neither the stop nor
	// die events provide the port mapping. As API consumers, it's our responsibility
	// to track this information. The map uses the container ID as the key and
	// stores all published ports associated with that container as the value.
	runningContainers    map[string][]*api.IPPort
	runningContainersMux sync.Mutex
}

func NewEventMonitor() *EventMonitor {
	return &EventMonitor{
		runningContainers: make(map[string][]*api.IPPort),
	}
}

func (e *EventMonitor) MonitorPorts(ctx context.Context, ch chan *api.Event) {
	e.tryGetClient(ctx)
	defer e.dockerClient.Close()

	if err := e.initializeRunningContainers(ctx, ch); err != nil {
		logrus.Errorf("failed to initialize existing docker container published ports: %v", err)
	}

	msgCh, errCh := e.dockerClient.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("event", startEvent),
			filters.Arg("event", stopEvent),
			filters.Arg("event", dieEvent)),
	})

	for {
		select {
		case <-ctx.Done():
			logrus.Errorf("context cancellation: %v", ctx.Err())

			return
		case event := <-msgCh:
			container, err := e.dockerClient.ContainerInspect(ctx, event.ID)
			if err != nil {
				logrus.Errorf("inspecting container [%v] failed: %v", event.ID, err)

				continue
			}

			portMap := container.NetworkSettings.NetworkSettingsBase.Ports
			logrus.Infof("received an event: {Status: %+v ContainerID: %+v Ports: %+v}",
				event.Action,
				event.ID,
				portMap)

			switch event.Action {
			case startEvent:

				if len(portMap) != 0 {
					validatePortMapping(portMap)
					ipPorts, err := convertToIPPort(portMap)
					if err != nil {
						logrus.Errorf("converting docker's portMapping: %+v to api.IPPort: %v failed: %s", portMap, ipPorts, err)

					}

					logrus.Infof("successfully converted PortMapping:%+v to IPPorts: %+v", portMap, ipPorts)

					e.runningContainersMux.Lock()
					e.runningContainers[event.ID] = ipPorts
					e.runningContainersMux.Unlock()

					sendEvent(false, ipPorts, ch)
				}
			case stopEvent, dieEvent:
				e.runningContainersMux.Lock()
				ipPorts, ok := e.runningContainers[event.ID]
				if !ok {
					e.runningContainersMux.Unlock()
					continue
				}
				sendEvent(true, ipPorts, ch)
			}
		case err := <-errCh:
			logrus.Errorf("receiving container event failed: %v", err)

			return
		}
	}
}

func sendEvent(remove bool, ipPorts []*api.IPPort, ch chan *api.Event) {
	var ev *api.Event
	if remove {
		ev = &api.Event{
			LocalPortsRemoved: ipPorts,
			Time:              timestamppb.Now(),
		}
	} else {
		ev = &api.Event{
			LocalPortsAdded: ipPorts,
			Time:            timestamppb.Now(),
		}
	}
	ch <- ev
	logrus.Infof("sent the following event to hostAgent: %+v", ev)
}

func (e *EventMonitor) initializeRunningContainers(ctx context.Context, ch chan *api.Event) error {
	containers, err := e.dockerClient.ContainerList(ctx, container.ListOptions{
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
					Protocol: port.Type,
					Ip:       port.IP,
					Port:     int32(port.PublicPort),
				})
			}
			sendEvent(false, ipPorts, ch)
			e.runningContainersMux.Lock()
			e.runningContainers[container.ID] = ipPorts
			e.runningContainersMux.Unlock()
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

			logrus.Infof("converted the following PortMapping to IPPort, containerPort:%v HostPort:%v IP:%v Protocol:%v",
				key.Port(), portBinding.HostPort, portBinding.HostIP, key.Proto())

			ipPorts = append(ipPorts, &api.IPPort{
				Protocol: key.Proto(),
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

func (e *EventMonitor) tryGetClient(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)

	for {
		select {
		case <-ctx.Done():
			logrus.Warn("context cancelled, stopping attempts to connect to Docker daemon")
			return
		case <-ticker.C:
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				logrus.Errorf("error creating Docker client: %v", err)
			} else {
				info, err := cli.Info(ctx)
				if err != nil {
					logrus.Errorf("error getting Docker info: %v", err)
				} else {
					logrus.Infof("successfully connected to Docker daemon: %s", info.ID)
					e.dockerClient = cli
					return
				}
			}

			logrus.Error("retrying to connect to Docker daemon in 2 seconds...")
		}
	}
}
