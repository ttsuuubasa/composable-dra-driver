/*
Copyright 2025 The CoHDI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manager

import (
	"cdi_dra/pkg/client"
	"cdi_dra/pkg/config"
	"cdi_dra/pkg/kube_utils"
	"context"
	"fmt"
	"log/slog"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
	"k8s.io/utils/strings/slices"
)

const (
	configMapName = "composable-dra/composable-dra-dds"
	GpuDeviceType = "gpu"
)

type CDIManager struct {
	coreClient           kube_client.Interface
	machineClient        dynamic.Interface
	discoveryClient      discovery.DiscoveryInterface
	namedDriverResources map[string]*resourceslice.DriverResources
	deviceInfos          []config.DeviceInfo
	labelPrefix          string
	cdiClient            *client.CDIClient
	kubecontrollers      *kube_utils.KubeControllers
	cdiOptions           CDIOptions
}

type CDIOptions struct {
	useCapiBmh     bool
	useCM          bool
	bindingTimeout *int64
}

type machine struct {
	nodeName      string
	machineUUID   string
	fabricID      *int
	deviceList    deviceList
	nodeGroupUUID string
}

type deviceList map[string]*device

type device struct {
	k8sDeviceName        string
	driverName           string
	draAttributes        map[string]string
	availableDeviceCount int
	minDeviceCount       *int
	maxDeviceCount       *int
	bindingTimeout       *int64
}

func StartCDIManager(ctx context.Context, cfg *config.Config) error {
	kconfig, err := kube_utils.NewClientConfig()
	if err != nil {
		return err
	}

	coreclient, err := kube_client.NewForConfig(kconfig)
	if err != nil {
		slog.Error("Failed to create core client", "error", err)
		return err
	}

	machineclient, err := dynamic.NewForConfig(kconfig)
	if err != nil {
		slog.Error("Failed to create machine client", "error", err)
		return err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(kconfig)
	if err != nil {
		slog.Error("Failed to create discovery client", "error", err)
		return err
	}

	// Create k8s controllers for Nodes, ConfigMap, Secret, Machine and BMH
	kc, err := kube_utils.CreateKubeControllers(coreclient, machineclient, discoveryClient, cfg.UseCapiBmh, ctx.Done())
	if err != nil {
		slog.Error("Failed to create kube controllers")
		return err
	}

	// Run k8s controllers
	if err := kc.Run(); err != nil {
		slog.Error("Failed to run kube controllers")
		return err
	}

	// Build client to connect CDI components like FM, IM and CM
	cdiclient, err := client.BuildCDIClient(cfg, kc)
	if err != nil {
		return err
	}

	// Get DeviceInfo from ConfigMap
	cm, err := kc.GetConfigMap(configMapName)
	if err != nil {
		slog.Error("Cannot get config map for device config", "error", err)
		return err
	}
	var devInfos []config.DeviceInfo
	var labelPrefix string
	if cm != nil {
		devInfos, err = config.GetDeviceInfos(cm)
		if err != nil {
			return err
		}
		labelPrefix, err = config.GetLabelPrefix(cm)
		if err != nil {
			return err
		}
	}

	// Init DriverResource for every driver name
	ndr := initDriverResources(devInfos)

	options := CDIOptions{
		useCapiBmh:     cfg.UseCapiBmh,
		useCM:          cfg.UseCM,
		bindingTimeout: cfg.BindingTimout,
	}

	m := &CDIManager{
		coreClient:           coreclient,
		machineClient:        machineclient,
		discoveryClient:      discoveryClient,
		namedDriverResources: ndr,
		deviceInfos:          devInfos,
		labelPrefix:          labelPrefix,
		cdiClient:            cdiclient,
		kubecontrollers:      kc,
		cdiOptions:           options,
	}

	controllers, err := m.startResourceSliceController(ctx)
	if err != nil {
		return err
	}

	wait.Until(func() {
		slog.Info("Loop Start")
		err := m.startCheckResourcePoolLoop(ctx, controllers)
		if err != nil {
			slog.Error("Loop Failed", "error", err)
		} else {
			slog.Info("Loop Successful")
		}
	}, cfg.ScanInterval, ctx.Done())
	return nil
}

func (m *CDIManager) startResourceSliceController(ctx context.Context) (map[string]*resourceslice.Controller, error) {
	if !kube_utils.IsDRAEnabled(m.discoveryClient) {
		return nil, fmt.Errorf("not enabled feature gate of Dynamic Resource Allocation")
	}
	controllers := make(map[string]*resourceslice.Controller)
	for driverName, driverResource := range m.namedDriverResources {
		options := resourceslice.Options{
			DriverName: driverName,
			KubeClient: m.coreClient,
			Resources:  driverResource,
		}
		slog.Debug("Start publishing ResourceSlices for CDI fabric devices...", "driverName", driverName)
		controller, err := resourceslice.StartController(ctx, options)
		if err != nil {
			slog.Error("error starting resource slice controller", "error", err)
			return nil, err
		}
		controllers[driverName] = controller
	}
	return controllers, nil
}

func (m *CDIManager) startCheckResourcePoolLoop(ctx context.Context, controllers map[string]*resourceslice.Controller) error {
	// Get the map of node name vs machine uuid
	muuids, err := m.getMachineUUIDs()
	if err != nil {
		slog.Error("failed to get machine UUID")
		return err
	}
	if len(muuids) == 0 {
		return fmt.Errorf("not any machine uuid is found")
	}
	// Get list of machine
	mList, err := m.getMachineList(ctx)
	if err != nil {
		return err
	}

	var ngInfos []*client.CMNodeGroupInfo
	if m.cdiOptions.useCM {
		// Get node groups
		nodeGroups, err := m.getNodeGroups(ctx)
		if err != nil {
			return err
		}
		// Get node group info
		for _, nodeGroup := range nodeGroups.NodeGroups {
			ngInfo, err := m.getNodeGroupInfo(ctx, nodeGroup)
			if err != nil {
				return err
			}
			ngInfos = append(ngInfos, ngInfo)
		}
	}

	// Create machine which have information of node and devices
	var machines []*machine
	for nodeName, muuid := range muuids {
		// Get fabric id of every machine
		fabricID := getFabricID(mList, muuid)
		if fabricID == nil {
			slog.Warn("not found fabric id for the machine", "machineUUID", muuid, "nodeName", nodeName)
			continue
		}
		foundInNodeGroup := make(map[string]string)
		if m.cdiOptions.useCM {
			for _, ngInfo := range ngInfos {
				if slices.Contains(ngInfo.MachineIDs, muuid) {
					foundInNodeGroup[muuid] = ngInfo.UUID
				}
			}
			if _, exist := foundInNodeGroup[muuid]; !exist {
				slog.Warn("the machine is not found in all node groups, so not set max/min device num", "nodeName", nodeName, "machineUUID", muuid)
			}
		}
		machine := &machine{
			nodeName:      nodeName,
			machineUUID:   muuid,
			fabricID:      fabricID,
			nodeGroupUUID: foundInNodeGroup[muuid],
		}
		machines = append(machines, machine)
	}

	if len(machines) == 0 {
		return fmt.Errorf("not any machine is found to process")
	}

	// Get the number of free devices in a fabric pool
	// It is executed per a fabric for reducing API calls
	fabricFound := make(map[int]deviceList)
	for _, machine := range machines {
		if _, exists := fabricFound[*machine.fabricID]; exists {
			continue
		}
		var deviceList deviceList = make(map[string]*device)
		for _, deviceInfo := range m.deviceInfos {
			availableNum, err := m.getAvailableNums(ctx, machine.machineUUID, deviceInfo.CDIModelName)
			if err != nil {
				return err
			}
			deviceList[deviceInfo.CDIModelName] = &device{
				k8sDeviceName:        deviceInfo.K8sDeviceName,
				driverName:           deviceInfo.DriverName,
				draAttributes:        deviceInfo.DRAAttributes,
				availableDeviceCount: availableNum,
				bindingTimeout:       m.cdiOptions.bindingTimeout,
			}
		}
		fabricFound[*machine.fabricID] = deviceList
	}

	// Copy device list per a fabric into all machines
	for fabricID, deviceList := range fabricFound {
		for _, machine := range machines {
			if *machine.fabricID != fabricID {
				continue
			}
			machine.deviceList = deviceList.DeepCopy()
		}
	}

	// Get the minimum and maximum number of devices in the node group
	if m.cdiOptions.useCM {
		type limit struct {
			min *int
			max *int
		}
		type deviceMinMax map[string]limit
		nodeGroupFound := make(map[string]deviceMinMax)
		for _, machine := range machines {
			if _, exists := nodeGroupFound[machine.nodeGroupUUID]; exists {
				continue
			}
			var deviceMinMax deviceMinMax = make(map[string]limit)
			for model := range machine.deviceList {
				min, max, err := m.getMinMaxNums(ctx, machine.machineUUID, model)
				if err != nil {
					return err
				}
				deviceMinMax[model] = limit{min: min, max: max}
			}
			nodeGroupFound[machine.nodeGroupUUID] = deviceMinMax
		}

		// Copy device min/max into machine in same node group
		for ngUUID, deviceMinMax := range nodeGroupFound {
			for _, machine := range machines {
				if machine.nodeGroupUUID != ngUUID {
					continue
				}
				for model, limit := range deviceMinMax {
					if limit.min != nil {
						machine.deviceList[model].minDeviceCount = limit.min
					}
					if limit.max != nil {
						machine.deviceList[model].maxDeviceCount = limit.max
					}
				}
			}
		}
	}

	for _, machine := range machines {
		slog.Debug("machine information", "nodeName", machine.nodeName, "machineUUID", machine.machineUUID, "fabricID", safeReference(machine.fabricID))
		for modelName, device := range machine.deviceList {
			slog.Debug("device information", "nodeName", machine.nodeName, "modelName", modelName, "available", device.availableDeviceCount, "min", safeReference(device.minDeviceCount), "max", safeReference(device.maxDeviceCount))
		}
	}

	// Update ResourceSlice using machineInfos
	m.manageCDIResourceSlices(machines, controllers)

	// Add labels to Node
	err = m.manageCDINodeLabel(ctx, machines)
	if err != nil {
		return err
	}
	return nil
}

func (m *CDIManager) getMachineUUIDs() (map[string]string, error) {
	uuids := make(map[string]string)

	providerIDs, err := m.kubecontrollers.ListProviderIDs()
	if err != nil {
		return nil, err
	}
	for _, providerID := range providerIDs {
		nodeName, err := m.kubecontrollers.FindNodeNameByProviderID(providerID)
		if err != nil {
			slog.Error("failed to get node name", "error", err)
			return nil, err
		} else if nodeName == "" {
			slog.Warn("missing node for providerID", "providerID", providerID)
			continue
		}
		var uuid string
		if !m.cdiOptions.useCapiBmh {
			// If not using cluster-api and BareMetalHost in a cluster, provider id itself is machine uuid
			uuid = string(providerID)
		} else if m.cdiOptions.useCapiBmh {
			// If using cluster-api and BareMetalHost, machine uuid must be derived from annotation of BareMetalHost
			uuid, err = m.kubecontrollers.FindMachineUUIDByProviderID(providerID)
			if err != nil {
				slog.Error("failed to get machine uuid", "error", err)
				return nil, err
			} else if uuid == "" {
				slog.Warn("missing machine uuid for providerID, so this machine is not created", "providerID", providerID)
				continue
			}
		}
		uuids[nodeName] = uuid
	}
	for nodeName, uuid := range uuids {
		slog.Debug("got machine uuids", "nodeName", nodeName, "machineUUID", uuid)
	}
	return uuids, nil
}

func (m *CDIManager) getMachineList(ctx context.Context) (*client.FMMachineList, error) {
	ctx = context.WithValue(ctx, client.RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying to get machine list from FabricManager", "requestID", client.GetRequestIdFromContext(ctx))

	// Publish API to get a machine list from FabricManager
	mList, err := m.cdiClient.GetFMMachineList(ctx)
	if err != nil {
		return nil, fmt.Errorf("FM machine list API failed, requestID=%s", client.GetRequestIdFromContext(ctx))
	}
	slog.Debug("FM machine list API completed successfully", "requestID", client.GetRequestIdFromContext(ctx))
	for _, machine := range mList.Data.Machines {
		slog.Debug("got machine list", "machineUUID", machine.MachineUUID, "fabricID", safeReference(machine.FabricID))
	}
	return mList, nil
}

func (m *CDIManager) getAvailableNums(ctx context.Context, muuid string, modelName string) (int, error) {
	ctx = context.WithValue(ctx, client.RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying to get available reserved resources from FabricManager", "machineUUID", muuid, "modelName", modelName, "requestID", client.GetRequestIdFromContext(ctx))

	// Publish API to get available reserved resources from FabricManager
	availableResources, err := m.cdiClient.GetFMAvailableReservedResources(ctx, muuid, modelName)
	if err != nil {
		return 0, fmt.Errorf("FM available reserved resources API failed, requestID=%s", client.GetRequestIdFromContext(ctx))
	}
	slog.Debug("FM available reserved resources API completed successfully", "requestID", client.GetRequestIdFromContext(ctx))
	return availableResources.ReservedResourceNum, nil
}

func (m *CDIManager) getNodeGroups(ctx context.Context) (*client.CMNodeGroups, error) {
	ctx = context.WithValue(ctx, client.RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying to get node groups from ClusterManager", "requestID", client.GetRequestIdFromContext(ctx))

	// Publish API to get node groups from ClusterManager
	nodeGroups, err := m.cdiClient.GetCMNodeGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("CM node groups API failed, requestID=%s", client.GetRequestIdFromContext(ctx))
	}
	slog.Debug("CM node groups API completed successfully", "requestID", client.GetRequestIdFromContext(ctx))
	for _, nodeGroup := range nodeGroups.NodeGroups {
		slog.Debug("got node groups", "NodeGroupName", nodeGroup.Name, "UUID", nodeGroup.UUID)
	}
	return nodeGroups, nil
}

func (m *CDIManager) getNodeGroupInfo(ctx context.Context, nodeGroup client.CMNodeGroup) (*client.CMNodeGroupInfo, error) {
	ctx = context.WithValue(ctx, client.RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying to get node group info from ClusterManager", "nodeGroupName", nodeGroup.Name, "requestID", client.GetRequestIdFromContext(ctx))

	// Publish API to get a node group info from ClusterManager
	nodeGroupInfo, err := m.cdiClient.GetCMNodeGroupInfo(ctx, nodeGroup)
	if err != nil {
		return nil, fmt.Errorf("CM node group info API failed, requestID=%s", client.GetRequestIdFromContext(ctx))
	}
	slog.Debug("CM node group info API completed successfully", "requestID", client.GetRequestIdFromContext(ctx))
	for _, machineID := range nodeGroupInfo.MachineIDs {
		slog.Debug("got node group info, the machine belongs in the node group", "machineUUID", machineID, "NodeGroupName", nodeGroupInfo.Name)
	}
	return nodeGroupInfo, nil
}

func (m *CDIManager) getMinMaxNums(ctx context.Context, muuid string, modelName string) (min *int, max *int, error error) {
	ctx = context.WithValue(ctx, client.RequestIDKey{}, config.RandomString(6))
	slog.Debug("trying to get node details from ClusterManager", "machineUUID", muuid, "modelName", modelName, "requestID", client.GetRequestIdFromContext(ctx))

	// Publish API to get node details from ClusterManager
	nodeDetails, err := m.cdiClient.GetCMNodeDetails(ctx, muuid)
	if err != nil {
		return nil, nil, fmt.Errorf("CM node details API failed, requestID=%s", client.GetRequestIdFromContext(ctx))
	}
	slog.Debug("CM node details API completed successfully", "requestID", client.GetRequestIdFromContext(ctx))
	for _, resspec := range nodeDetails.Data.Cluster.Machine.ResSpecs {
		for _, condition := range resspec.Selector.Expression.Conditions {
			if condition.Column == "model" && condition.Operator == "eq" && condition.Value == modelName {
				if resspec.MinResSpecCount != nil {
					min = resspec.MinResSpecCount
				}
				if resspec.MaxResSpecCount != nil {
					max = resspec.MaxResSpecCount
				}
			}
		}
	}
	return min, max, nil
}

func (m *CDIManager) manageCDIResourceSlices(machines []*machine, controlles map[string]*resourceslice.Controller) {
	needUpdate := make(map[string]bool)
	fabricFound := make(map[int]bool)
	for _, machine := range machines {
		if !fabricFound[*machine.fabricID] {
			for _, device := range machine.deviceList {
				if _, exist := m.namedDriverResources[device.driverName]; exist {
					poolName := device.k8sDeviceName + "-fabric" + strconv.Itoa(*machine.fabricID)
					updated := m.updatePool(device.driverName, poolName, device, *machine.fabricID)
					if updated {
						needUpdate[device.driverName] = true
					}
				}
			}
			fabricFound[*machine.fabricID] = true
		}
	}
	for driverName, driverResources := range m.namedDriverResources {
		if needUpdate[driverName] {
			c := controlles[driverName]
			for poolName := range driverResources.Pools {
				slog.Info("pool update", "poolName", poolName, "generation", m.namedDriverResources[driverName].Pools[poolName].Generation, "driver", driverName)
			}
			c.Update(driverResources)
		}
	}
}

func (m *CDIManager) updatePool(driverName string, poolName string, device *device, fabricID int) (updated bool) {
	var generation int64 = 1
	pool := m.namedDriverResources[driverName].Pools[poolName]
	if len(pool.Slices) == 0 {
		m.namedDriverResources[driverName].Pools[poolName] = m.generatePool(device, fabricID, generation)
		return true
	} else {
		if len(pool.Slices[0].Devices) != device.availableDeviceCount {
			generation = pool.Generation
			generation++
			m.namedDriverResources[driverName].Pools[poolName] = m.generatePool(device, fabricID, generation)
			return true
		}
	}
	return false
}

func (m *CDIManager) generatePool(device *device, fabricID int, generation int64) resourceslice.Pool {
	var devices []resourceapi.Device
	for i := 0; i < device.availableDeviceCount; i++ {
		d := resourceapi.Device{
			Name: fmt.Sprintf("%s-gpu%d", device.k8sDeviceName, i),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type": {
					StringValue: ptr.To(GpuDeviceType),
				},
			},
			UsageRestrictedToNode:    ptr.To(true),
			BindingConditions:        []string{"FabricDeviceReady"},
			BindingFailureConditions: []string{"FabricDeviceReschedule", "FabricDeviceFailed"},
			BindingTimeoutSeconds:    device.bindingTimeout,
		}
		for key, value := range device.draAttributes {
			d.Attributes[resourceapi.QualifiedName(key)] = resourceapi.DeviceAttribute{StringValue: ptr.To(value)}
		}
		devices = append(devices, d)
	}
	pool := resourceslice.Pool{
		NodeSelector: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      m.labelPrefix + "/" + device.k8sDeviceName,
							Operator: corev1.NodeSelectorOpIn,
							Values: []string{
								"true",
							},
						},
						{
							Key:      m.labelPrefix + "/" + "fabric",
							Operator: corev1.NodeSelectorOpIn,
							Values: []string{
								strconv.Itoa(fabricID),
							},
						},
					},
				},
			},
		},
		Slices: []resourceslice.Slice{
			{
				Devices: devices,
			},
		},
		Generation: generation,
	}
	return pool
}

func (m *CDIManager) manageCDINodeLabel(ctx context.Context, machines []*machine) error {
	for _, machine := range machines {
		node, err := m.kubecontrollers.GetNode(machine.nodeName)
		if err != nil {
			slog.Error("failed to get node", "nodeName", machine.nodeName)
			return err
		}
		// Label for fabric
		fabricLabelKey := m.labelPrefix + "/" + "fabric"
		if node != nil {
			node.Labels[fabricLabelKey] = strconv.Itoa(*machine.fabricID)
			slog.Debug("set labels for fabric", "nodeName", machine.nodeName, "label", fabricLabelKey+"="+node.Labels[fabricLabelKey])
			if m.cdiOptions.useCM {
				// Label for the min and max number of devices
				for _, device := range machine.deviceList {
					maxLabelKey := m.labelPrefix + "/" + device.k8sDeviceName + "-size-max"
					if device.maxDeviceCount != nil {
						max := strconv.Itoa(*device.maxDeviceCount)
						if node.Labels[maxLabelKey] != max {
							node.Labels[maxLabelKey] = max
							slog.Info("set labels for max of devices", "nodeName", machine.nodeName, "label", maxLabelKey+"="+max)
						}
					} else {
						delete(node.Labels, maxLabelKey)
					}
					minLabelKey := m.labelPrefix + "/" + device.k8sDeviceName + "-size-min"
					if device.minDeviceCount != nil {
						min := strconv.Itoa(*device.minDeviceCount)
						if node.Labels[minLabelKey] != min {
							node.Labels[minLabelKey] = min
							slog.Info("set labels for min of devices", "nodeName", machine.nodeName, "label", minLabelKey+"="+min)
						}
					} else {
						delete(node.Labels, minLabelKey)
					}
				}
			}
			_, err = m.coreClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			if err != nil {
				slog.Error("failed to update node label", "nodeName", machine.nodeName)
				return err
			}
		}
	}
	return nil
}

func initDriverResources(devInfos []config.DeviceInfo) map[string]*resourceslice.DriverResources {
	foundDriver := make(map[string]bool)
	result := make(map[string]*resourceslice.DriverResources)
	for _, devInfo := range devInfos {
		if foundDriver[devInfo.DriverName] {
			continue
		} else {
			foundDriver[devInfo.DriverName] = true
		}
		driverResources := &resourceslice.DriverResources{
			Pools: make(map[string]resourceslice.Pool),
		}
		result[devInfo.DriverName] = driverResources
	}
	return result
}

func getFabricID(mList *client.FMMachineList, muuid string) (fabricID *int) {
	for _, machine := range mList.Data.Machines {
		if machine.MachineUUID == muuid {
			return machine.FabricID
		}
	}
	return nil
}

func safeReference(ptr *int) string {
	if ptr != nil {
		return strconv.Itoa(*ptr)
	}
	return "<nil>"
}

func (in deviceList) DeepCopy() (out deviceList) {
	out = make(map[string]*device)
	for modelName, inDevice := range in {
		newDevice := new(device)
		*newDevice = *inDevice
		out[modelName] = newDevice
	}
	return out
}
