// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lima-vm/lima/pkg/guestagent/api"
)

func sendHostAgentEvent(remove bool, ipPorts []*api.IPPort, ch chan *api.Event) {
	ev := &api.Event{
		Time: timestamppb.Now(),
	}
	if remove {
		ev.LocalPortsRemoved = ipPorts
	} else {
		ev.LocalPortsAdded = ipPorts
	}
	ch <- ev
	logrus.Infof("sent the following event to hostAgent: %+v", ev)
}
