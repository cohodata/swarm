package cluster

import (
	"math"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types/network"
	"golang.org/x/net/context"
)

const (
	// Only make one attempt to reschedule containers by default
	DefaultRescheduleRetry = 1
)

type WatchdogOpts struct {
	RescheduleRetry            int
	RescheduleRetryInterval    time.Duration
	RescheduleRetryMaxInterval time.Duration
}

// Watchdog listens to cluster events and handles container rescheduling
type Watchdog struct {
	sync.Mutex
	cluster Cluster
	running bool
	opts    *WatchdogOpts
}

// Handle handles cluster callbacks
func (w *Watchdog) Handle(e *Event) error {
	// Skip non-swarm events.
	if e.From != "swarm" {
		return nil
	}

	switch e.Status {
	case "engine_connect", "engine_reconnect":
		go w.removeDuplicateContainers(e.Engine)
	case "engine_disconnect":
		go w.rescheduleContainers(e.Engine)
	}
	return nil
}

// removeDuplicateContainers removes duplicate containers when a node comes back
func (w *Watchdog) removeDuplicateContainers(e *Engine) {
	log.Debugf("removing duplicate containers from Node %s", e.ID)

	e.RefreshContainers(false)

	w.Lock()
	defer w.Unlock()

	for _, container := range e.Containers() {
		// skip non-swarm containers
		if container.Config.SwarmID() == "" {
			continue
		}

		for _, containerInCluster := range w.cluster.Containers() {
			if containerInCluster.Config.SwarmID() == container.Config.SwarmID() && containerInCluster.Engine.ID != container.Engine.ID {
				log.Debugf("container %s was rescheduled on node %s, removing it", container.ID, containerInCluster.Engine.Name)
				// container already exists in the cluster, destroy it
				if err := e.RemoveContainer(container, true, true); err != nil {
					log.Errorf("Failed to remove duplicate container %s on node %s: %v", container.ID, containerInCluster.Engine.Name, err)
				}
			}
		}
	}
}

// rescheduleContainers reschedules containers as soon as a node fails
func (w *Watchdog) rescheduleContainers(e *Engine) {
	limit := w.opts.RescheduleRetry
	retryInterval := w.opts.RescheduleRetryInterval
	retryMaxInterval := w.opts.RescheduleRetryMaxInterval

	log.Debugf("Node %s failed - attempting to reschedule containers interval=%s max_interval=%s limit=%d)", e.ID, retryInterval, retryMaxInterval, limit)

	attempt := 1
	for !w.rescheduleContainersHelper(e) {
		if limit == 0 || attempt < limit {
			sleep := time.Duration(math.Min(
				retryMaxInterval.Seconds(),
				retryInterval.Seconds()*math.Pow(1.5, float64(attempt-1)),
			)) * time.Second

			log.Debugf("Node %s - could not reschedule containers, attempt %d (limit %d), retrying in %s", e.ID, attempt, limit, sleep)
			attempt++
			time.Sleep(sleep)
		} else {
			log.Errorf("Node %s - could not reschedule containers after %d attempt(s)", e.ID, limit)
			break
		}
	}

	log.Debugf("Node %s - container rescheduling complete", e.ID)
}

func (w *Watchdog) rescheduleContainersHelper(e *Engine) bool {
	w.Lock()
	defer w.Unlock()

	if !w.running {
		log.Debugf("Watchdog is shutting down, abandon rescheduling")
		return true
	}

	done := true
	for _, c := range e.Containers() {

		// Skip containers which don't have an "on-node-failure" reschedule policy.
		if !c.Config.HasReschedulePolicy("on-node-failure") {
			log.Debugf("Skipping rescheduling of %s based on rescheduling policies", c.ID)
			continue
		}

		log.Debugf("Attempting to reschedule container %s (%s) %+v from %s", c.ID, c.Info.Name, c.Info.State, c.Engine.Name)

		// Remove the container from the dead engine. If we don't, then both
		// the old and new one will show up in docker ps.
		// We have to do this before calling `CreateContainer`, otherwise it
		// will abort because the name is already taken.
		c.Engine.removeContainer(c)

		// keep track of all global networks this container is connected to
		globalNetworks := make(map[string]*network.EndpointSettings)
		// if the existing container has global network endpoints,
		// they need to be removed with force option
		// "docker network disconnect -f network containername" only takes containername
		name := c.Info.Name
		if len(name) == 0 || len(name) == 1 && name[0] == '/' {
			log.Errorf("container %s has no name", c.ID)
			continue
		}
		// cut preceding '/'
		if name[0] == '/' {
			name = name[1:]
		}

		if c.Info.NetworkSettings != nil && len(c.Info.NetworkSettings.Networks) > 0 {
			// find an engine to do disconnect work
			randomEngine, err := w.cluster.RANDOMENGINE()
			if err != nil {
				log.Errorf("Failed to find an engine to do network cleanup for container %s: %v", c.ID, err)
				// add the container back, so we can retry later
				c.Engine.AddContainer(c)
				done = false
				continue
			}

			clusterNetworks := w.cluster.Networks().Uniq()
			for networkName, endpoint := range c.Info.NetworkSettings.Networks {
				net := clusterNetworks.Get(endpoint.NetworkID)
				if net != nil && (net.Scope == "global" || net.Scope == "swarm") {
					// record the network, they should be reconstructed on the new container
					globalNetworks[networkName] = endpoint
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					log.Debugf("Disconnecting container %s (%s) from network %s", c.ID, c.Info.Name, networkName)
					err = randomEngine.apiClient.NetworkDisconnect(ctx, networkName, name, true)
					if err != nil {
						// do not abort here as this endpoint might have been removed before
						log.Warnf("Failed to remove network endpoint from old container %s: %v", name, err)
					}
				}
			}
		}

		// Ensure that all global networks have been wiped from the container config.
		// This is necessary because NetworkDisconnect is allowed to fail in loop above.
		for networkName, _ := range globalNetworks {
			delete(c.Info.NetworkSettings.Networks, networkName)
		}

		// Clear out the network configs that we're going to reattach
		// later.
		endpointsConfig := map[string]*network.EndpointSettings{}
		for k, v := range c.Config.NetworkingConfig.EndpointsConfig {
			net := w.cluster.Networks().Uniq().Get(v.NetworkID)
			if net != nil && (net.Scope == "global" || net.Scope == "swarm") {
				// These networks are already in globalNetworks
				// and thus will be reattached later.
				continue
			}
			endpointsConfig[k] = v
		}
		c.Config.NetworkingConfig.EndpointsConfig = endpointsConfig

		newContainer, err := w.cluster.CreateContainer(c.Config, c.Info.Name, nil)
		if err != nil {
			log.Errorf("Failed to reschedule container %s: %v", c.ID, err)
			// resurrect removed global network endpoints before adding
			// the container back to the engine until the next retry.
			for networkName, endpoint := range globalNetworks {
				c.Info.NetworkSettings.Networks[networkName] = endpoint
				c.Config.NetworkingConfig.EndpointsConfig[networkName] = endpoint
			}

			// add the container back, so we can retry later
			c.Engine.AddContainer(c)
			done = false
			continue
		}

		// Docker create command cannot create a container with multiple networks
		// see https://github.com/docker/docker/issues/17750
		// Add the global networks one by one
		for networkName, endpoint := range globalNetworks {
			hasSubnet := false
			network := w.cluster.Networks().Uniq().Get(networkName)
			if network != nil {
				for _, config := range network.IPAM.Config {
					if config.Subnet != "" {
						hasSubnet = true
						break
					}
				}
			}
			// If this network did not have a defined subnet, we
			// cannot connect to it with an explicit IP address.
			if !hasSubnet && endpoint.IPAMConfig != nil {
				endpoint.IPAMConfig.IPv4Address = ""
				endpoint.IPAMConfig.IPv6Address = ""
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			log.Debugf("Connecting container %s (%s) to network %s", newContainer.ID, newContainer.Info.Name, networkName)
			err = newContainer.Engine.apiClient.NetworkConnect(ctx, networkName, name, endpoint)
			if err != nil {
				log.Warnf("Failed to connect network %s to container %s: %v", networkName, name, err)
			}
		}

		log.Infof("Rescheduled container %s (%s) from %s to %s as %s", c.ID, c.Info.Name, c.Engine.Name, newContainer.Engine.Name, newContainer.ID)
		if shouldRestart(c) {
			log.Infof("Scheduling start of container %s (%s)", newContainer.ID, newContainer.Info.Name)
			go w.restartContainer(newContainer)
		}
	}
	return done
}

// Attempt to restart a container.  If this watchdog has reschedule retry
// behaviour enabled, we attempt to start the container indefinately until
// this manager instance is no longer primary.  Otherwise, just attempt to start
// the container once.
func (w *Watchdog) restartContainer(c *Container) {
	w.Lock()
	defer w.Unlock()

	retryLimit := w.opts.RescheduleRetry
	retryInterval := w.opts.RescheduleRetryInterval

	done := false
	for !done && w.running {
		log.Infof("Attempting to start container %s (%s)", c.ID, c.Info.Name)
		if err := w.cluster.StartContainer(c, nil); err != nil {
			log.Errorf("Failed to start rescheduled container %s, retrying in %s: %v", c.ID, retryInterval, err)
			if retryLimit == DefaultRescheduleRetry {
				done = true
			} else {
				w.Unlock()
				time.Sleep(retryInterval)
				w.Lock()
			}
		} else {
			done = true
		}
	}
	if done {
		log.Debugf("Container %s (%s) started", c.ID, c.Info.Name)
	} else if !w.running {
		log.Debugf("Watchdog is shutting down, abandoning restart of container %s", c.ID)
	}
}

// Determines whether a container should be started after rescheduling by
// taking its state and restart policy into account.
func shouldRestart(c *Container) bool {
	if c.Info.State.Running {
		log.Debugf("Container %s (%s) was running and should be restarted", c.ID, c.Info.Name)
		return true
	}

	// Containers with a restart policy of 'unless-stopped' will not be
	// restarted because the swarm manager has no insight into whether the
	// container has been stopped manually by the user.
	var rp = c.Config.HostConfig.RestartPolicy
	if rp.IsAlways() {
		log.Debugf("Container %s (%s) has a restart policy of '%s' and should be restarted", c.ID, c.Info.Name, rp.Name)
		return true
	} else if rp.IsOnFailure() && c.Info.State.ExitCode != 0 && c.Info.RestartCount < rp.MaximumRetryCount {
		log.Debugf("Container %s (%s) has a restart policy of '%s' and should be restarted", c.ID, c.Info.Name, rp.Name)
		return true
	} else {
		log.Debugf("Container %s (%s) has a restart policy of '%s' and should not be restarted", c.ID, c.Info.Name, rp.Name)
		return false
	}
}

// NewWatchdog creates a new watchdog
func NewWatchdog(cluster Cluster, opts *WatchdogOpts) *Watchdog {
	log.Debugf("Watchdog enabled")
	w := &Watchdog{
		cluster: cluster,
		running: true,
		opts:    opts,
	}
	cluster.RegisterEventHandler(w)

	for _, e := range cluster.Engines() {
		if !e.IsHealthy() {
			go w.rescheduleContainers(e)
		}
	}

	return w
}

// Instruct the watchdog to stop
func (w *Watchdog) Stop() {
	w.Lock()
	defer w.Unlock()
	log.Debugf("Stopping Watchdog")
	w.running = false
}
