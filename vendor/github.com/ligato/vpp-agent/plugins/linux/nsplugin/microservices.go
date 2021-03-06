// Copyright (c) 2018 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nsplugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/ligato/cn-infra/logging"
	"github.com/ligato/cn-infra/servicelabel"
)

var microserviceContainerCreated = make(map[string]time.Time)

// how often in seconds to refresh the microservice label -> docker container PID map
const (
	dockerRefreshPeriod = 3 * time.Second
	dockerRetryPeriod   = 5 * time.Second
)

// Microservice event types
const (
	// NewMicroservice event type
	NewMicroservice = "new-ms"
	// TerminatedMicroservice event type
	TerminatedMicroservice = "term-ms"
)

// unavailableMicroserviceErr is error implementation used when a given microservice is not deployed.
type unavailableMicroserviceErr struct {
	label string
}

func (e *unavailableMicroserviceErr) Error() string {
	return fmt.Sprintf("Microservice '%s' is not available", e.label)
}

// Microservice is used to store PID and ID of the container running a given microservice.
type Microservice struct {
	Label string
	Pid   int
	Id    string
}

// MicroserviceEvent contains microservice object and event type
type MicroserviceEvent struct {
	*Microservice
	EventType string
}

// MicroserviceCtx contains all data required to handle microservice changes
type MicroserviceCtx struct {
	nsMgmtCtx     *NamespaceMgmtCtx
	created       []string
	since         string
	lastInspected int64
}

// HandleMicroservices handles microservice changes
func (plugin *NsHandler) HandleMicroservices(ctx *MicroserviceCtx) {
	var err error
	var newest int64
	var containers []docker.APIContainers
	var nextCreated []string

	// First check if any microservice has terminated.
	plugin.cfgLock.Lock()
	for container := range plugin.microServiceByID {
		details, err := plugin.dockerClient.InspectContainer(container)
		if err != nil || !details.State.Running {
			plugin.processTerminatedMicroservice(ctx.nsMgmtCtx, container)
		}
	}
	plugin.cfgLock.Unlock()

	// Now check if previously created containers have transitioned to the state "running".
	for _, container := range ctx.created {
		details, err := plugin.dockerClient.InspectContainer(container)
		if err == nil {
			if details.State.Running {
				plugin.detectMicroservice(ctx.nsMgmtCtx, details)
			} else if details.State.Status == "created" {
				nextCreated = append(nextCreated, container)
			}
		} else {
			plugin.log.Debugf("Inspect container ID %v failed: %v", container, err)
		}
	}
	ctx.created = nextCreated

	// Inspect newly created containers
	listOpts := docker.ListContainersOptions{
		All:     true,
		Filters: map[string][]string{},
	}
	// List containers and filter all older than 'since' ID
	if ctx.since != "" {
		listOpts.Filters["since"] = []string{ctx.since}
	}
	containers, err = plugin.dockerClient.ListContainers(listOpts)
	if err != nil {
		// If 'since' container was not found, list all containers (404 is required to support older docker version)
		if dockerErr, ok := err.(*docker.Error); ok && (dockerErr.Status == 500 || dockerErr.Status == 404) {
			// Reset filter and list containers again
			plugin.log.Debug("clearing 'since' %s", ctx.since)
			ctx.since = ""
			delete(listOpts.Filters, "since")
			containers, err = plugin.dockerClient.ListContainers(listOpts)
		}
		if err != nil {
			// If there is other error, return it
			plugin.log.Errorf("Error listing docker containers: %v", err)
			return
		}
	}

	for _, container := range containers {
		plugin.log.Debugf("processing new container %v with state %v", container.ID, container.State)
		if container.State == "running" && container.Created > ctx.lastInspected {
			// Inspect the container to get the list of defined environment variables.
			details, err := plugin.dockerClient.InspectContainer(container.ID)
			if err != nil {
				plugin.log.Debugf("Inspect container %v failed: %v", container.ID, err)
				continue
			}
			plugin.detectMicroservice(ctx.nsMgmtCtx, details)
		}
		if container.State == "created" {
			ctx.created = append(ctx.created, container.ID)
		}
		if container.Created > newest {
			newest = container.Created
			ctx.since = container.ID
		}
	}

	if newest > ctx.lastInspected {
		ctx.lastInspected = newest
	}
}

// detectMicroservice inspects container to see if it is a microservice.
// If microservice is detected, processNewMicroservice() is called to process it.
func (plugin *NsHandler) detectMicroservice(nsMgmtCtx *NamespaceMgmtCtx, container *docker.Container) {
	// Search for the microservice label.
	var label string
	for _, env := range container.Config.Env {
		if strings.HasPrefix(env, servicelabel.MicroserviceLabelEnvVar+"=") {
			label = env[len(servicelabel.MicroserviceLabelEnvVar)+1:]
			if label != "" {
				plugin.log.Debugf("detected container as microservice: Name=%v ID=%v Created=%v State.StartedAt=%v", container.Name, container.ID, container.Created, container.State.StartedAt)
				last := microserviceContainerCreated[label]
				if last.After(container.Created) {
					plugin.log.Debugf("ignoring older container created at %v as microservice: %+v", last, container)
					continue
				}
				microserviceContainerCreated[label] = container.Created
				plugin.processNewMicroservice(nsMgmtCtx, label, container.ID, container.State.Pid)
			}
		}
	}
}

// processNewMicroservice is triggered every time a new microservice gets freshly started. All pending interfaces are moved
// to its namespace.
func (plugin *NsHandler) processNewMicroservice(nsMgmtCtx *NamespaceMgmtCtx, microserviceLabel string, id string, pid int) {
	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	microservice, restarted := plugin.microServiceByLabel[microserviceLabel]
	if restarted {
		plugin.processTerminatedMicroservice(nsMgmtCtx, microservice.Id)
		plugin.log.WithFields(logging.Fields{"label": microserviceLabel, "new-pid": pid, "new-id": id}).
			Warn("Microservice has been restarted")
	} else {
		plugin.log.WithFields(logging.Fields{"label": microserviceLabel, "pid": pid, "id": id}).
			Debug("Discovered new microservice")
	}

	microservice = &Microservice{Label: microserviceLabel, Pid: pid, Id: id}
	plugin.microServiceByLabel[microserviceLabel] = microservice
	plugin.microServiceByID[id] = microservice

	// Send notification to interface configurator
	plugin.ifMicroserviceNotif <- &MicroserviceEvent{
		Microservice: microservice,
		EventType:    NewMicroservice,
	}
}

// processTerminatedMicroservice is triggered every time a known microservice has terminated. All associated interfaces
// become obsolete and are thus removed.
func (plugin *NsHandler) processTerminatedMicroservice(nsMgmtCtx *NamespaceMgmtCtx, id string) {
	microservice, exists := plugin.microServiceByID[id]
	if !exists {
		plugin.log.WithFields(logging.Fields{"id": id}).
			Warn("Detected removal of an unknown microservice")
		return
	}
	plugin.log.WithFields(logging.Fields{"label": microservice.Label, "pid": microservice.Pid, "id": microservice.Id}).
		Debug("Microservice has terminated")

	delete(plugin.microServiceByLabel, microservice.Label)
	delete(plugin.microServiceByID, microservice.Id)

	// Send notification to interface configurator
	plugin.ifMicroserviceNotif <- &MicroserviceEvent{
		Microservice: microservice,
		EventType:    TerminatedMicroservice,
	}
}

// trackMicroservices is running in the background and maintains a map of microservice labels to container info.
func (plugin *NsHandler) trackMicroservices(ctx context.Context) {
	plugin.wg.Add(1)
	defer func() {
		plugin.wg.Done()
		plugin.log.Debugf("Microservice tracking ended")
	}()

	msCtx := &MicroserviceCtx{
		nsMgmtCtx: NewNamespaceMgmtCtx(),
	}

	var clientOk bool

	timer := time.NewTimer(0)
	for {
		select {
		case <-timer.C:
			if err := plugin.dockerClient.Ping(); err != nil {
				if clientOk {
					plugin.log.Errorf("Docker ping check failed: %v", err)
				}
				clientOk = false

				// Sleep before another retry.
				timer.Reset(dockerRetryPeriod)
				continue
			}

			if !clientOk {
				plugin.log.Infof("Docker ping check OK")
				/*if info, err := plugin.dockerClient.Info(); err != nil {
					plugin.Log.Errorf("Retrieving docker info failed: %v", err)
					timer.Reset(dockerRetryPeriod)
					continue
				} else {
					plugin.Log.Infof("Docker connection established: server version: %v (%v %v %v)",
						info.ServerVersion, info.OperatingSystem, info.Architecture, info.KernelVersion)
				}*/
			}
			clientOk = true

			select {
			case plugin.microserviceChan <- msCtx:
			case <-plugin.ctx.Done():
				return
			}

			// Sleep before another refresh.
			timer.Reset(dockerRefreshPeriod)
		case <-plugin.ctx.Done():
			return
		}
	}
}
