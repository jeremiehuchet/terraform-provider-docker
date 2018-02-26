package docker

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"time"

	"os"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	dc "github.com/fsouza/go-dockerclient"
	"github.com/hashicorp/terraform/helper/schema"
)

var (
	numberedStates = map[swarm.TaskState]int64{
		swarm.TaskStateNew:       1,
		swarm.TaskStateAllocated: 2,
		swarm.TaskStatePending:   3,
		swarm.TaskStateAssigned:  4,
		swarm.TaskStateAccepted:  5,
		swarm.TaskStatePreparing: 6,
		swarm.TaskStateReady:     7,
		swarm.TaskStateStarting:  8,
		swarm.TaskStateRunning:   9,

		// The following states are not actually shown in progress
		// output, but are used internally for ordering.
		swarm.TaskStateComplete: 10,
		swarm.TaskStateShutdown: 11,
		swarm.TaskStateFailed:   12,
		swarm.TaskStateRejected: 13,
	}

	longestState int
)

func resourceDockerServiceExists(d *schema.ResourceData, meta interface{}) (bool, error) {
	client := meta.(*ProviderConfig).DockerClient
	if client == nil {
		return false, nil
	}

	apiService, err := fetchDockerService(d.Id(), d.Get("name").(string), client)
	if err != nil {
		return false, err
	}
	if apiService == nil {
		return false, nil
	}

	return true, nil
}

type convergeConfig struct {
	interval time.Duration
	monitor  time.Duration
}

func resourceDockerServiceCreate(d *schema.ResourceData, meta interface{}) error {
	var err error
	client := meta.(*ProviderConfig).DockerClient

	serviceSpec, err := createServiceSpec(d)
	if err != nil {
		return err
	}

	createOpts := dc.CreateServiceOptions{
		ServiceSpec: serviceSpec,
	}

	if v, ok := d.GetOk("auth"); ok {
		createOpts.Auth = authToServiceAuth(v.(map[string]interface{}))
	} else {
		createOpts.Auth = fromRegistryAuth(d.Get("image").(string), meta.(*ProviderConfig).AuthConfigs.Configs)
	}

	if v, ok := d.GetOk("update_config"); ok {
		createOpts.UpdateConfig, _ = createUpdateOrRollbackConfig(v.([]interface{}))
	}

	if v, ok := d.GetOk("rollback_config"); ok {
		createOpts.RollbackConfig, _ = createUpdateOrRollbackConfig(v.([]interface{}))
	}

	service, err := client.CreateService(createOpts)
	if err != nil {
		return err
	}

	if v, ok := d.GetOk("converge_config"); ok {
		plainConvergeConfig := &convergeConfig{}
		if len(v.([]interface{})) > 0 {
			for _, rawConvergeConfig := range v.([]interface{}) {
				rawConvergeConfig := rawConvergeConfig.(map[string]interface{})
				if interval, ok := rawConvergeConfig["interval"]; ok {
					plainConvergeConfig.interval, _ = time.ParseDuration(interval.(string))
				}
				if monitor, ok := rawConvergeConfig["monitor"]; ok {
					plainConvergeConfig.monitor, _ = time.ParseDuration(monitor.(string))
				}
			}
		}
		if err := waitOnService(context.Background(), client, plainConvergeConfig, service.ID); err != nil {
			return err
		}
	}

	d.SetId(service.ID)
	return resourceDockerServiceRead(d, meta)
}

func resourceDockerServiceRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient

	apiService, err := fetchDockerService(d.Id(), d.Get("name").(string), client)
	if err != nil {
		return err
	}
	if apiService == nil {
		d.SetId("")
		return nil
	}

	var service *swarm.Service
	service, err = client.InspectService(apiService.ID)
	if err != nil {
		return fmt.Errorf("Error inspecting service %s: %s", apiService.ID, err)
	}

	d.Set("version", service.Version.Index)
	d.Set("name", service.Spec.Name)

	d.SetId(service.ID)

	return nil
}

func resourceDockerServiceUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient

	service, err := client.InspectService(d.Id())
	if err != nil {
		return err
	}

	serviceSpec, err := createServiceSpec(d)
	if err != nil {
		return err
	}

	updateOpts := dc.UpdateServiceOptions{
		ServiceSpec: serviceSpec,
		Version:     service.Version.Index,
	}

	if v, ok := d.GetOk("auth"); ok {
		updateOpts.Auth = authToServiceAuth(v.(map[string]interface{}))
	} else {
		updateOpts.Auth = fromRegistryAuth(d.Get("image").(string), meta.(*ProviderConfig).AuthConfigs.Configs)
	}

	if v, ok := d.GetOk("update_config"); ok {
		updateOpts.UpdateConfig, _ = createUpdateOrRollbackConfig(v.([]interface{}))
	}

	if v, ok := d.GetOk("rollback_config"); ok {
		updateOpts.RollbackConfig, _ = createUpdateOrRollbackConfig(v.([]interface{}))
	}

	if err = client.UpdateService(d.Id(), updateOpts); err != nil {
		return err
	}

	if v, ok := d.GetOk("converge_config"); ok {
		plainConvergeConfig := &convergeConfig{}
		if len(v.([]interface{})) > 0 {
			for _, rawConvergeConfig := range v.([]interface{}) {
				rawConvergeConfig := rawConvergeConfig.(map[string]interface{})
				if interval, ok := rawConvergeConfig["interval"]; ok {
					plainConvergeConfig.interval, _ = time.ParseDuration(interval.(string))
				}
				if monitor, ok := rawConvergeConfig["monitor"]; ok {
					plainConvergeConfig.monitor, _ = time.ParseDuration(monitor.(string))
				}
			}
		}
		if err := waitOnService(context.Background(), client, plainConvergeConfig, service.ID); err != nil {
			log.Printf("[INFO] error on update --> %v", err)
			return err
		}
	}

	return resourceDockerServiceRead(d, meta)
}

func resourceDockerServiceDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient

	// == 1: get containerIDs of the running service
	serviceContainerIds := make([]string, 0)
	if _, ok := d.GetOk("stop_grace_period"); ok {
		filter := make(map[string][]string)
		filter["service"] = []string{d.Get("name").(string)}
		tasks, err := client.ListTasks(dc.ListTasksOptions{
			Filters: filter,
		})
		if err != nil {
			return err
		}
		for i := 0; i < len(tasks); i++ {
			task, _ := client.InspectTask(tasks[i].ID)
			log.Printf("[INFO] Inspecting container '%s' and state '%s' for shutdown", task.Status.ContainerStatus.ContainerID, task.Status.State)
			if strings.TrimSpace(task.Status.ContainerStatus.ContainerID) != "" && task.Status.State != swarm.TaskStateShutdown {
				serviceContainerIds = append(serviceContainerIds, task.Status.ContainerStatus.ContainerID)
			}
		}
	}

	// == 2: delete the service
	if err := deleteService(d.Id(), client); err != nil {
		return err
	}

	// == 3: wait until all containers of the service are down to be able to unmount the associated volumes
	if v, ok := d.GetOk("stop_grace_period"); ok {
		stopGracePeriod, _ := time.ParseDuration(v.(string))
		for _, containerID := range serviceContainerIds {
			log.Printf("[INFO] Waiting for container: '%s' to exit: max %v", containerID, stopGracePeriod)
			ctx, cancel := context.WithTimeout(context.Background(), stopGracePeriod)
			defer cancel()
			if exitCode, errOnExit := client.WaitContainerWithContext(containerID, ctx); errOnExit == nil {
				log.Printf("[INFO] Container: '%s' exited with code '%d'", containerID, exitCode)
				select {
				case <-ctx.Done():
					log.Printf("[INFO] Container: '%s' shutdown was canceled. Killing it now", containerID)
					killOps := dc.KillContainerOptions{
						Signal:  dc.SIGKILL,
						Context: ctx,
					}
					if errOnKill := client.KillContainer(killOps); errOnKill != nil {
						log.Printf("[INFO] Container: '%s' killing errord with '%s'", containerID, errOnKill)
					}
				default:
					// ctx is not canceled, continue with removal TODO does it work properly?
				}
				// Remove the container if it did not exit properly to be able to unmount the volumes
				log.Printf("[INFO] Removing container: '%s'", containerID)
				removeOps := dc.RemoveContainerOptions{
					ID:      containerID,
					Force:   true,
					Context: ctx,
				}
				if errOnRemove := client.RemoveContainer(removeOps); errOnRemove != nil {
					// if the removal is already in progress, this error can be ignored
					log.Printf("[INFO] Error '%s' on removal of Container: '%s'", errOnRemove, containerID)
				}
			}
		}
	}

	d.SetId("")
	return nil
}

////////////
// Helpers
////////////
func waitOnService(ctx context.Context, client *dc.Client, plainConvergeConfig *convergeConfig, serviceID string) error {
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer signal.Stop(sigint)

	filter := make(map[string][]string)
	filter["service"] = []string{serviceID}
	filter["desired-state"] = []string{"running"}

	getUpToDateTasks := func() ([]swarm.Task, error) {
		return client.ListTasks(dc.ListTasksOptions{
			Filters: filter,
			Context: ctx,
		})
	}

	var (
		updater     progressUpdater
		converged   bool
		convergedAt time.Time
		monitor     = plainConvergeConfig.monitor
		rollback    bool
	)

	for {
		service, err := client.InspectService(serviceID)
		if err != nil {
			return err
		}

		if service.Spec.UpdateConfig != nil && service.Spec.UpdateConfig.Monitor != 0 {
			monitor = service.Spec.UpdateConfig.Monitor
		}

		if updater == nil {
			updater = &replicatedProgressUpdater{}
		}

		if service.UpdateStatus != nil {
			switch service.UpdateStatus.State {
			case swarm.UpdateStateUpdating:
				rollback = false
			case swarm.UpdateStateCompleted:
				if !converged {
					return nil
				}
			case swarm.UpdateStatePaused:
				return fmt.Errorf("service update paused: %s", service.UpdateStatus.Message)
			case swarm.UpdateStateRollbackStarted:
				rollback = true
			case swarm.UpdateStateRollbackPaused:
				return fmt.Errorf("service rollback paused: %s", service.UpdateStatus.Message)
			case swarm.UpdateStateRollbackCompleted:
				if !converged {
					return fmt.Errorf("service rolled back: %s", service.UpdateStatus.Message)
				}
			}
		}

		if converged && time.Since(convergedAt) >= monitor {
			if service.UpdateStatus != nil {
				if service.UpdateStatus.State == swarm.UpdateStateRollbackCompleted {
					return fmt.Errorf("service rollback completed at %v", convergedAt)
				}
				log.Printf("[INFO] return after update with status: %v", service.UpdateStatus.State)
			}
			log.Printf("[INFO] return after converged")
			return nil
		}

		tasks, err := getUpToDateTasks()
		if err != nil {
			return err
		}

		activeNodes, err := getActiveNodes(ctx, client)
		if err != nil {
			return err
		}

		converged, err = updater.update(service, tasks, activeNodes, rollback)
		if err != nil {
			return err
		}

		if converged {
			if convergedAt.IsZero() {
				convergedAt = time.Now()
				log.Printf("[INFO] converged at %v", convergedAt)
			}
			// wait := monitor - time.Since(convergedAt)
			// if wait >= 0 {
			// }
		} else {
			// if !convergedAt.IsZero() {
			// }
			convergedAt = time.Time{}
		}

		select {
		case <-time.After(plainConvergeConfig.interval):
		case <-sigint:
			if !converged {
				log.Printf("[INFO] Operation continuing in background.")
			}
			return nil
		}
	}
}

func getActiveNodes(ctx context.Context, client *dc.Client) (map[string]struct{}, error) {
	// nodes, err := client.NodeList(ctx, types.NodeListOptions{})
	nodes, err := client.ListNodes(dc.ListNodesOptions{Context: ctx})
	if err != nil {
		return nil, err
	}

	activeNodes := make(map[string]struct{})
	for _, n := range nodes {
		if n.Status.State != swarm.NodeStateDown {
			activeNodes[n.ID] = struct{}{}
		}
	}
	log.Printf("[INFO] active nodes: %v", len(activeNodes))
	return activeNodes, nil
}

type progressUpdater interface {
	update(service *swarm.Service, tasks []swarm.Task, activeNodes map[string]struct{}, rollback bool) (bool, error)
}

type replicatedProgressUpdater struct {
	// progressOut progress.Output

	// used for mapping slots to a contiguous space
	// this also causes progress bars to appear in order
	slotMap map[int]int

	initialized bool
	done        bool
}

func (u *replicatedProgressUpdater) update(service *swarm.Service, tasks []swarm.Task, activeNodes map[string]struct{}, rollback bool) (bool, error) {
	if service.Spec.Mode.Replicated == nil || service.Spec.Mode.Replicated.Replicas == nil {
		return false, fmt.Errorf("no replica count")
	}
	replicas := *service.Spec.Mode.Replicated.Replicas

	if !u.initialized {
		u.slotMap = make(map[int]int)
		u.initialized = true
	}

	tasksBySlot := u.tasksBySlot(tasks, activeNodes)

	// If we had reached a converged state, check if we are still converged.
	if u.done {
		for _, task := range tasksBySlot {
			if task.Status.State != swarm.TaskStateRunning {
				u.done = false
				break
			}
		}
	}

	running := uint64(0)

	for _, task := range tasksBySlot {
		mappedSlot := u.slotMap[task.Slot]
		if mappedSlot == 0 {
			mappedSlot = len(u.slotMap) + 1
			u.slotMap[task.Slot] = mappedSlot
		}

		if !terminalState(task.DesiredState) && task.Status.State == swarm.TaskStateRunning {
			running++
			log.Printf("[INFO] got running replica: %v for task %v", running, task.ID)
		}

		// u.writeTaskProgress(task, mappedSlot, replicas, rollback) TODO
	}

	if !u.done {
		// writeOverallProgress(u.progressOut, int(running), int(replicas), rollback)

		if running == replicas {
			log.Printf("[INFO] got all %v replicas up and running", running)
			u.done = true
		}
	}

	return running == replicas, nil
}

func (u *replicatedProgressUpdater) tasksBySlot(tasks []swarm.Task, activeNodes map[string]struct{}) map[int]swarm.Task {
	// If there are multiple tasks with the same slot number, favor the one
	// with the *lowest* desired state. This can happen in restart
	// scenarios.
	tasksBySlot := make(map[int]swarm.Task)
	for _, task := range tasks {
		if numberedStates[task.DesiredState] == 0 || numberedStates[task.Status.State] == 0 {
			continue
		}
		if existingTask, ok := tasksBySlot[task.Slot]; ok {
			if numberedStates[existingTask.DesiredState] < numberedStates[task.DesiredState] {
				continue
			}
			// If the desired states match, observed state breaks
			// ties. This can happen with the "start first" service
			// update mode.
			if numberedStates[existingTask.DesiredState] == numberedStates[task.DesiredState] &&
				numberedStates[existingTask.Status.State] <= numberedStates[task.Status.State] {
				continue
			}
		}
		if task.NodeID != "" {
			if _, nodeActive := activeNodes[task.NodeID]; !nodeActive {
				continue
			}
		}
		tasksBySlot[task.Slot] = task
	}

	return tasksBySlot
}

func terminalState(state swarm.TaskState) bool {
	return numberedStates[state] > numberedStates[swarm.TaskStateRunning]
}

//////////
func deleteService(serviceID string, client *dc.Client) error {
	removeOpts := dc.RemoveServiceOptions{
		ID: serviceID,
	}

	if err := client.RemoveService(removeOpts); err != nil {
		return fmt.Errorf("Error deleting service %s: %s", serviceID, err)
	}

	return nil
}

func fetchDockerService(ID string, name string, client *dc.Client) (*swarm.Service, error) {
	apiServices, err := client.ListServices(dc.ListServicesOptions{})

	if err != nil {
		return nil, fmt.Errorf("Error fetching service information from Docker: %s", err)
	}

	for _, apiService := range apiServices {
		if apiService.ID == ID || apiService.Spec.Name == name {
			return &apiService, nil
		}
	}

	return nil, nil
}

func createServiceSpec(d *schema.ResourceData) (swarm.ServiceSpec, error) {

	serviceSpec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: d.Get("name").(string),
		},
		TaskTemplate: swarm.TaskSpec{},
	}

	if v, ok := d.GetOk("replicas"); ok {
		replicas := uint64(v.(int))
		serviceSpec.Mode = swarm.ServiceMode{}
		serviceSpec.Mode.Replicated = &swarm.ReplicatedService{}
		serviceSpec.Mode.Replicated.Replicas = &replicas
	}

	// == start Container Spec
	containerSpec := swarm.ContainerSpec{
		Image: d.Get("image").(string),
	}

	if v, ok := d.GetOk("hostname"); ok {
		containerSpec.Hostname = v.(string)
	}

	if v, ok := d.GetOk("stop_grace_period"); ok {
		parsed, _ := time.ParseDuration(v.(string))
		containerSpec.StopGracePeriod = &parsed
	}

	if v, ok := d.GetOk("command"); ok {
		containerSpec.Command = stringListToStringSlice(v.([]interface{}))
		for _, v := range containerSpec.Command {
			if v == "" {
				return swarm.ServiceSpec{}, fmt.Errorf("values for command may not be empty")
			}
		}
	}

	if v, ok := d.GetOk("env"); ok {
		containerSpec.Env = stringSetToStringSlice(v.(*schema.Set))
	}

	if v, ok := d.GetOk("hosts"); ok {
		containerSpec.Hosts = extraHostsSetToDockerExtraHosts(v.(*schema.Set))
	}

	endpointSpec := swarm.EndpointSpec{}

	if v, ok := d.GetOk("network_mode"); ok {
		endpointSpec.Mode = swarm.ResolutionMode(v.(string))
	}

	portBindings := []swarm.PortConfig{}

	if v, ok := d.GetOk("networks"); ok {
		networks := []swarm.NetworkAttachmentConfig{}

		for _, rawNetwork := range v.(*schema.Set).List() {
			network := swarm.NetworkAttachmentConfig{
				Target: rawNetwork.(string),
			}
			networks = append(networks, network)
		}
		serviceSpec.TaskTemplate.Networks = networks
		serviceSpec.Networks = networks
	}

	if v, ok := d.GetOk("mounts"); ok {
		mounts := []mount.Mount{}

		for _, rawMount := range v.(*schema.Set).List() {
			rawMount := rawMount.(map[string]interface{})
			mountType := mount.Type(rawMount["type"].(string))
			mountInstance := mount.Mount{
				Type:     mountType,
				Source:   rawMount["source"].(string),
				Target:   rawMount["target"].(string),
				ReadOnly: rawMount["read_only"].(bool),
			}

			if w, ok := d.GetOk("volume_labels"); ok {
				mountInstance.VolumeOptions.Labels = mapTypeMapValsToString(w.(map[string]interface{}))
			}

			if mountType == mount.TypeBind {
				if w, ok := d.GetOk("bind_propagation"); ok {
					mountInstance.BindOptions = &mount.BindOptions{
						Propagation: mount.Propagation(w.(string)),
					}
				}
			} else if mountType == mount.TypeVolume {
				mountInstance.VolumeOptions = &mount.VolumeOptions{}

				if w, ok := d.GetOk("volume_no_copy"); ok {
					mountInstance.VolumeOptions.NoCopy = w.(bool)
				}

				mountInstance.VolumeOptions.DriverConfig = &mount.Driver{}
				if w, ok := d.GetOk("volume_driver_name"); ok {
					mountInstance.VolumeOptions.DriverConfig.Name = w.(string)
				}

				if w, ok := d.GetOk("volume_driver_options"); ok {
					mountInstance.VolumeOptions.DriverConfig.Options = w.(map[string]string)
				}
			} else if mountType == mount.TypeTmpfs {
				mountInstance.TmpfsOptions = &mount.TmpfsOptions{}

				if w, ok := d.GetOk("tmpfs_size_bytes"); ok {
					mountInstance.TmpfsOptions.SizeBytes = w.(int64)
				}

				if w, ok := d.GetOk("tmpfs_mode"); ok {
					mountInstance.TmpfsOptions.Mode = os.FileMode(w.(int))
				}
			}

			mounts = append(mounts, mountInstance)
		}

		containerSpec.Mounts = mounts
	}

	serviceSpec.TaskTemplate.ContainerSpec = &containerSpec

	if v, ok := d.GetOk("configs"); ok {
		configs := []*swarm.ConfigReference{}

		for _, rawConfig := range v.(*schema.Set).List() {
			rawConfig := rawConfig.(map[string]interface{})
			config := swarm.ConfigReference{
				ConfigID:   rawConfig["config_id"].(string),
				ConfigName: rawConfig["config_name"].(string),
				File: &swarm.ConfigReferenceFileTarget{
					Name: rawConfig["file_name"].(string),
					GID:  "0",
					UID:  "0",
					Mode: os.FileMode(0444),
				},
			}
			configs = append(configs, &config)
		}
		serviceSpec.TaskTemplate.ContainerSpec.Configs = configs
	}

	if v, ok := d.GetOk("secrets"); ok {
		secrets := []*swarm.SecretReference{}

		for _, rawSecret := range v.(*schema.Set).List() {
			rawSecret := rawSecret.(map[string]interface{})
			secret := swarm.SecretReference{
				SecretID:   rawSecret["secret_id"].(string),
				SecretName: rawSecret["secret_name"].(string),
				File: &swarm.SecretReferenceFileTarget{
					Name: rawSecret["file_name"].(string),
					GID:  "0",
					UID:  "0",
					Mode: os.FileMode(0444),
				},
			}
			secrets = append(secrets, &secret)
		}
		serviceSpec.TaskTemplate.ContainerSpec.Secrets = secrets
	}
	// == end Container Spec

	// == start Endpoint Spec
	if v, ok := d.GetOk("ports"); ok {
		portBindings = portSetToServicePorts(v.(*schema.Set))
	}
	if len(portBindings) != 0 {
		endpointSpec.Ports = portBindings
	}

	serviceSpec.EndpointSpec = &endpointSpec
	// == end Endpoint Spec

	// == start TaskTemplate Spec
	placement := swarm.Placement{}
	if v, ok := d.GetOk("constraints"); ok {
		placement.Constraints = stringSetToStringSlice(v.(*schema.Set))
	}

	if v, ok := d.GetOk("placement_prefs"); ok {
		placement.Preferences = stringSetToPlacementPrefs(v.(*schema.Set))
	}

	if v, ok := d.GetOk("placement_platform"); ok {
		placement.Platforms = mapSetToPlacementPlatforms(v.(*schema.Set))
	}

	serviceSpec.TaskTemplate.Placement = &placement

	if v, ok := d.GetOk("logging"); ok {
		serviceSpec.TaskTemplate.LogDriver = &swarm.Driver{}
		for _, rawLogging := range v.([]interface{}) {
			rawLogging := rawLogging.(map[string]interface{})
			serviceSpec.TaskTemplate.LogDriver.Name = rawLogging["driver_name"].(string)

			if rawOptions, ok := rawLogging["options"]; ok {
				serviceSpec.TaskTemplate.LogDriver.Options = mapTypeMapValsToString(rawOptions.(map[string]interface{}))
			}
		}
	}

	if v, ok := d.GetOk("healthcheck"); ok {
		containerSpec.Healthcheck = &container.HealthConfig{}
		if len(v.([]interface{})) > 0 {
			for _, rawHealthCheck := range v.([]interface{}) {
				rawHealthCheck := rawHealthCheck.(map[string]interface{})
				if testCommand, ok := rawHealthCheck["test"]; ok {
					containerSpec.Healthcheck.Test = stringListToStringSlice(testCommand.([]interface{}))
				}
				if rawInterval, ok := rawHealthCheck["interval"]; ok {
					containerSpec.Healthcheck.Interval, _ = time.ParseDuration(rawInterval.(string))
				}
				if rawTimeout, ok := rawHealthCheck["timeout"]; ok {
					containerSpec.Healthcheck.Timeout, _ = time.ParseDuration(rawTimeout.(string))
				}
				if rawStartPeriod, ok := rawHealthCheck["start_period"]; ok {
					containerSpec.Healthcheck.StartPeriod, _ = time.ParseDuration(rawStartPeriod.(string))
				}
				if rawRetries, ok := rawHealthCheck["retries"]; ok {
					containerSpec.Healthcheck.Retries, _ = rawRetries.(int)
				}
			}
		}
	}

	if v, ok := d.GetOk("dns_config"); ok {
		containerSpec.DNSConfig = &swarm.DNSConfig{}
		if len(v.([]interface{})) > 0 {
			for _, rawDNSConfig := range v.([]interface{}) {
				rawDNSConfig := rawDNSConfig.(map[string]interface{})
				if nameservers, ok := rawDNSConfig["nameservers"]; ok {
					containerSpec.DNSConfig.Nameservers = stringListToStringSlice(nameservers.([]interface{}))
				}
				if search, ok := rawDNSConfig["search"]; ok {
					containerSpec.DNSConfig.Search = stringListToStringSlice(search.([]interface{}))
				}
				if options, ok := rawDNSConfig["options"]; ok {
					containerSpec.DNSConfig.Options = stringListToStringSlice(options.([]interface{}))
				}
			}
		}
	}

	// == end TaskTemplate Spec

	return serviceSpec, nil
}

func createUpdateOrRollbackConfig(config []interface{}) (*swarm.UpdateConfig, error) {
	updateConfig := swarm.UpdateConfig{}
	if len(config) > 0 {
		sc := config[0].(map[string]interface{})
		if v, ok := sc["parallelism"]; ok {
			updateConfig.Parallelism = uint64(v.(int))
		}
		if v, ok := sc["delay"]; ok {
			updateConfig.Delay, _ = time.ParseDuration(v.(string))
		}
		if v, ok := sc["failure_action"]; ok {
			updateConfig.FailureAction = v.(string)
		}
		if v, ok := sc["monitor"]; ok {
			updateConfig.Monitor, _ = time.ParseDuration(v.(string))
		}
		if v, ok := sc["max_failure_ratio"]; ok {
			updateConfig.MaxFailureRatio = float32(v.(float64))
		}
		if v, ok := sc["order"]; ok {
			updateConfig.Order = v.(string)
		}
	}

	return &updateConfig, nil
}

func portSetToServicePorts(ports *schema.Set) []swarm.PortConfig {
	retPortConfigs := []swarm.PortConfig{}

	for _, portInt := range ports.List() {
		port := portInt.(map[string]interface{})
		internal := port["internal"].(int)
		protocol := port["protocol"].(string)
		external := port["external"].(int)

		portConfig := swarm.PortConfig{
			TargetPort:    uint32(internal),
			PublishedPort: uint32(external),
			Protocol:      swarm.PortConfigProtocol(protocol),
		}

		mode, modeOk := port["mode"].(string)
		if modeOk {
			portConfig.PublishMode = swarm.PortConfigPublishMode(mode)
		}

		retPortConfigs = append(retPortConfigs, portConfig)
	}

	return retPortConfigs
}

func authToServiceAuth(auth map[string]interface{}) dc.AuthConfiguration {
	if auth["username"] != nil && len(auth["username"].(string)) > 0 && auth["password"] != nil && len(auth["password"].(string)) > 0 {
		return dc.AuthConfiguration{
			Username:      auth["username"].(string),
			Password:      auth["password"].(string),
			ServerAddress: auth["server_address"].(string),
		}
	}

	return dc.AuthConfiguration{}
}

func fromRegistryAuth(image string, configs map[string]dc.AuthConfiguration) dc.AuthConfiguration {
	// Remove normalized prefixes to simlify substring
	image = strings.Replace(strings.Replace(image, "http://", "", 1), "https://", "", 1)
	// Get the registry with optional port
	lastBin := strings.Index(image, "/")
	// No auth given and image name has no slash like 'alpine:3.1'
	if lastBin != -1 {
		serverAddress := image[0:lastBin]
		if fromRegistryAuth, ok := configs[normalizeRegistryAddress(serverAddress)]; ok {
			return fromRegistryAuth
		}
	}

	return dc.AuthConfiguration{}
}

func stringSetToPlacementPrefs(stringSet *schema.Set) []swarm.PlacementPreference {
	ret := []swarm.PlacementPreference{}
	if stringSet == nil {
		return ret
	}
	for _, envVal := range stringSet.List() {
		ret = append(ret, swarm.PlacementPreference{
			Spread: &swarm.SpreadOver{
				SpreadDescriptor: envVal.(string),
			},
		})
	}
	return ret
}

func mapSetToPlacementPlatforms(stringSet *schema.Set) []swarm.Platform {
	ret := []swarm.Platform{}
	if stringSet == nil {
		return ret
	}

	for _, rawPlatform := range stringSet.List() {
		rawPlatform := rawPlatform.(map[string]interface{})
		ret = append(ret, swarm.Platform{
			Architecture: rawPlatform["architecture"].(string),
			OS:           rawPlatform["os"].(string),
		})
	}

	return ret
}
