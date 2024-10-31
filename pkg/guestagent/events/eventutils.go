package events

import (
	"context"
	"time"

	"github.com/lima-vm/lima/v2/pkg/guestagent/api"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

func sendHostAgentEvent(remove bool, ipPorts []*api.IPPort, ch chan *api.Event) {
	ev := &api.Event{
		Time: timestamppb.Now(),
	}
	if remove {
		ev.RemovedLocalPorts = ipPorts
	} else {
		ev.AddedLocalPorts = ipPorts
	}
	ch <- ev
	logrus.Infof("sent the following event to hostAgent: %+v", ev)
}

func tryGetClient(ctx context.Context, tryConnect func(context.Context) (bool, error)) error {
	const retryInterval = 10 * time.Second
	const pollImmediately = true
	return wait.PollUntilContextCancel(ctx, retryInterval, pollImmediately, tryConnect)
}
