package events

import (
	"context"
	"fmt"
	"time"

	"github.com/lima-vm/lima/pkg/guestagent/api"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

func tryGetClient(ctx context.Context, serving func(context.Context) (bool, error)) error {
	initialInterval := 2 * time.Second
	finalInterval := 10 * time.Second
	maxAttempt := 5

	ticker := time.NewTicker(initialInterval)
	defer ticker.Stop()
	attempts := 0

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled, stopping attempts to connect to deamon")
		case <-ticker.C:
			isServing, err := serving(ctx)
			if !isServing {
				attempts++
				if attempts >= maxAttempt {
					ticker.Stop()
					ticker = time.NewTicker(finalInterval)
				}
				logrus.Debugf("trying to connect to the deamon...")
				continue
			}
			if err != nil {
				logrus.Errorf("error getting container engine's server info: %v", err)
				continue
			}

			logrus.Info("successfully connected to daemon")
			return nil
		}
	}
}
