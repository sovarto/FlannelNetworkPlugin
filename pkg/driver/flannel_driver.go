package driver

import (
	"fmt"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/sdk"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/api"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/dns"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/docker"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/flannel_network"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/service_lb"
	"golang.org/x/exp/maps"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

type FlannelDriver interface {
	Serve() error
	Init() error
}
type etcdClients struct {
	root         etcd.Client
	dockerData   etcd.Client
	serviceLbs   etcd.Client
	addressSpace etcd.Client
	networks     etcd.Client
}
type flannelDriver struct {
	globalAddressSpace      ipam.AddressSpace
	defaultFlannelOptions   []string
	defaultHostSubnetSize   int
	networksByFlannelID     map[string]flannel_network.Network // flannel network ID -> network
	networksByDockerID      map[string]flannel_network.Network // docker network ID -> network
	serviceLbsManagement    service_lb.ServiceLbsManagement
	services                map[string]common.Service // service ID -> service
	dockerData              docker.Data
	completeAddressSpace    []net.IPNet
	networkSubnetSize       int
	vniStart                int
	isInitialized           bool
	nameserversBySandboxKey map[string]dns.Nameserver
	nameserversByEndpointID map[string]dns.Nameserver
	dnsResolver             dns.Resolver
	etcdClients             etcdClients
	sync.Mutex
}

func NewFlannelDriver(
	etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, completeSpace []net.IPNet,
	networkSubnetSize int, defaultHostSubnetSize int, vniStart int, dnsDockerCompatibilityMode bool) FlannelDriver {

	driver := &flannelDriver{
		defaultFlannelOptions:   defaultFlannelOptions,
		defaultHostSubnetSize:   defaultHostSubnetSize,
		networksByFlannelID:     make(map[string]flannel_network.Network),
		networksByDockerID:      make(map[string]flannel_network.Network),
		services:                make(map[string]common.Service),
		vniStart:                vniStart,
		isInitialized:           false,
		completeAddressSpace:    completeSpace,
		networkSubnetSize:       networkSubnetSize,
		nameserversBySandboxKey: make(map[string]dns.Nameserver),
		nameserversByEndpointID: make(map[string]dns.Nameserver),
		dnsResolver:             dns.NewResolver(dnsDockerCompatibilityMode),
		etcdClients: etcdClients{
			root:         getEtcdClient(etcdPrefix, "", etcdEndPoints),
			dockerData:   getEtcdClient(etcdPrefix, "docker-data", etcdEndPoints),
			serviceLbs:   getEtcdClient(etcdPrefix, "service-lbs", etcdEndPoints),
			addressSpace: getEtcdClient(etcdPrefix, "address-space", etcdEndPoints),
			networks:     getEtcdClient(etcdPrefix, "networks", etcdEndPoints),
		},
	}

	numNetworks := countPoolSizeSubnets(completeSpace, networkSubnetSize)
	numIPsPerNode := int(math.Pow(2, float64(32-defaultHostSubnetSize)))
	numNodesPerNetwork := int(math.Pow(2, float64(defaultHostSubnetSize-networkSubnetSize)))
	fmt.Printf("The address space settings result in support for a total of %d docker networks, %d nodes and %d IP addresses per node and docker network (including service VIPs)\n", numNetworks, numNodesPerNetwork, numIPsPerNode)

	return driver
}

func (d *flannelDriver) IsInitialized() bool {
	return d.isInitialized
}

func (d *flannelDriver) Serve() error {
	handler := sdk.NewHandler(`{"Implements": ["IpamDriver", "NetworkDriver"]}`)
	api.InitIpamMux(&handler, d)
	api.InitNetworkMux(&handler, d)

	if err := handler.ServeUnix("flannel-np", 0); err != nil {
		return errors.WithMessagef(err, "Failed to start flannel plugin server")
	}

	return nil
}

func (d *flannelDriver) Init() error {
	err := d.etcdClients.root.WaitUntilAvailable(5*time.Second, 6)

	globalAddressSpace, err := ipam.NewEtcdBasedAddressSpace(d.completeAddressSpace, d.networkSubnetSize, d.etcdClients.addressSpace)
	if err != nil {
		return errors.WithMessage(err, "Failed to create address space")
	}
	d.globalAddressSpace = globalAddressSpace
	fmt.Println("Initialized address space")

	containerCallbacks := etcd.ShardItemsHandlers[docker.ContainerInfo]{
		OnAdded:   d.handleContainersAdded,
		OnChanged: d.handleContainersChanged,
		OnRemoved: d.handleContainersRemoved,
	}

	serviceCallbacks := etcd.ItemsHandlers[docker.ServiceInfo]{
		OnAdded:   d.handleServicesAdded,
		OnChanged: d.handleServicesChanged,
		OnRemoved: d.handleServicesRemoved,
	}

	networkCallbacks := etcd.ItemsHandlers[common.NetworkInfo]{
		OnAdded:   d.handleNetworksAdded,
		OnChanged: d.handleNetworksChanged,
		OnRemoved: d.handleNetworksRemoved,
	}

	serviceLbsManagement, err := service_lb.NewServiceLbManagement(d.etcdClients.serviceLbs)
	if err != nil {
		return errors.WithMessage(err, "Failed to create service lbs management")
	}
	d.serviceLbsManagement = serviceLbsManagement
	fmt.Println("Initialized service load balancer management")

	dockerData, err := docker.NewData(d.etcdClients.dockerData, containerCallbacks, serviceCallbacks, networkCallbacks)
	if err != nil {
		return errors.WithMessage(err, "Failed to create docker data handler")
	}
	d.dockerData = dockerData

	err = dockerData.Init()
	if err != nil {
		return errors.WithMessage(err, "Failed to initialize docker data handler")
	}
	fmt.Println("Initialized docker data handler")
	existingNetworks := maps.Values(dockerData.GetNetworks().GetAll())
	existingServices := maps.Values(dockerData.GetServices().GetAll())

	fmt.Printf("Existing networks: %v\n", existingNetworks)
	fmt.Printf("Existing services: %v\n", existingServices)

	if err := flannel_network.CleanupStaleNetworks(d.etcdClients.networks, existingNetworks); err != nil {
		return errors.WithMessage(err, "Failed to cleanup stale flannel network data")
	}

	if err := service_lb.CleanUpStaleLoadBalancers(d.etcdClients.serviceLbs, lo.Map(existingServices, func(item docker.ServiceInfo, index int) string {
		return item.ID
	})); err != nil {
		return errors.WithMessage(err, "Failed to cleanup stale service load balancers")
	}

	//networkCallbacks.OnAdded(lo.Map(maps.Values(dockerData.GetNetworks().GetAll()),
	//	func(item common.NetworkInfo, index int) etcd.Item[common.NetworkInfo] {
	//		return etcd.Item[common.NetworkInfo]{
	//			ID:    item.DockerID,
	//			Value: item,
	//		}
	//	}))
	//
	//serviceCallbacks.OnAdded(lo.Map(maps.Values(dockerData.GetServices().GetAll()),
	//	func(item docker.ServiceInfo, index int) etcd.Item[docker.ServiceInfo] {
	//		return etcd.Item[docker.ServiceInfo]{
	//			ID:    item.ID,
	//			Value: item,
	//		}
	//	}))
	//
	//containerCallbacks.OnAdded(lo.Map(maps.Values(dockerData.GetContainers().GetAll()),
	//	func(item map[string]docker.ContainerInfo, index int) etcd.Item[docker.ContainerInfo] {
	//		return etcd.Item[docker.ContainerInfo]{
	//			ID:    item.ID,
	//			Value: item,
	//		}
	//	}))

	d.injectNameserverIntoAlreadyRunningContainers()

	d.isInitialized = true

	return nil
}

func getEtcdClient(rootPrefix, prefix string, endPoints []string) etcd.Client {
	return etcd.NewEtcdClient(endPoints, 5*time.Second, fmt.Sprintf("%s/%s", rootPrefix, prefix))
}

func (d *flannelDriver) getEndpoint(dockerNetworkID, endpointID string) (flannel_network.Network, flannel_network.Endpoint, error) {
	flannelNetwork, exists := d.networksByDockerID[dockerNetworkID]
	if !exists {
		return nil, nil, fmt.Errorf("no flannel network found for ID %s", dockerNetworkID)
	}

	endpoint := flannelNetwork.GetEndpoint(endpointID)

	if endpoint == nil {
		return nil, nil, types.ForbiddenErrorf("endpoint %s does not exist for network %s", endpointID, dockerNetworkID)
	}

	return flannelNetwork, endpoint, nil
}

func (d *flannelDriver) handleServicesAdded(added []etcd.Item[docker.ServiceInfo]) {
	for _, addedItem := range added {
		serviceInfo := addedItem.Value
		service := d.createService(serviceInfo.ID, serviceInfo.Name)
		service.SetEndpointMode(serviceInfo.EndpointMode)
		service.SetNetworks(serviceInfo.Networks, serviceInfo.IpamVIPs)
	}
}

func (d *flannelDriver) handleServicesChanged(changed []etcd.ItemChange[docker.ServiceInfo]) {
	for _, changedItem := range changed {
		serviceInfo := changedItem.Current
		service, exists := d.services[serviceInfo.ID]
		if !exists {
			log.Printf("Received a change event for unknown service %s\n", serviceInfo.ID)
			service = d.createService(serviceInfo.ID, serviceInfo.Name)
		}
		service.SetEndpointMode(serviceInfo.EndpointMode)
		service.SetNetworks(serviceInfo.Networks, serviceInfo.IpamVIPs)
	}
}

func (d *flannelDriver) handleServicesRemoved(removed []etcd.Item[docker.ServiceInfo]) {
	for _, removedItem := range removed {
		serviceInfo := removedItem.Value
		service, exists := d.services[serviceInfo.ID]
		if !exists {
			log.Printf("Received a remove event for unknown service %s\n", serviceInfo.ID)
		} else {
			if serviceInfo.EndpointMode == common.ServiceEndpointModeVip {
				err := d.serviceLbsManagement.DeleteLoadBalancer(serviceInfo.ID)
				if err != nil {
					log.Printf("Failed to remove load balancer for service %s: %+v\n", serviceInfo.ID, err)
				}
			}
			d.dnsResolver.RemoveService(service)
			delete(d.services, serviceInfo.ID)
		}
	}
}

func (d *flannelDriver) getNetwork(dockerNetworkID string, flannelNetworkID string) (flannel_network.Network, bool) {
	var network flannel_network.Network
	var exists bool
	if dockerNetworkID != "" {
		network, exists = d.networksByDockerID[dockerNetworkID]
	}
	if flannelNetworkID != "" {
		if !exists {
			network, exists = d.networksByFlannelID[flannelNetworkID]
			if dockerNetworkID != "" && exists {
				d.networksByDockerID[dockerNetworkID] = network
			}
		} else {
			d.networksByFlannelID[flannelNetworkID] = network
		}
	}

	return network, exists
}

func (d *flannelDriver) getOrCreateNetwork(dockerNetworkID string, flannelNetworkID string) (flannel_network.Network, error) {
	network, exists := d.getNetwork(dockerNetworkID, flannelNetworkID)
	if !exists {
		if flannelNetworkID == "" {
			return nil, fmt.Errorf("no flannel network ID provided when creating network")
		}

		networkSubnet, err := d.globalAddressSpace.GetNewOrExistingPool(flannelNetworkID)
		if err != nil {
			return nil, errors.WithMessagef(err, "failed to get network subnet pool for network '%s'", flannelNetworkID)
		}

		vni := d.vniStart + common.Max(len(d.networksByFlannelID), len(d.networksByDockerID)) + 1
		network = flannel_network.NewNetwork(d.etcdClients.networks, flannelNetworkID, *networkSubnet, d.defaultHostSubnetSize, d.defaultFlannelOptions, vni)

		if err := network.Init(d.dockerData); err != nil {
			return nil, errors.WithMessagef(err, "failed to ensure network '%s' is operational", flannelNetworkID)
		}
	}

	if dockerNetworkID != "" {
		err := d.serviceLbsManagement.SetNetwork(dockerNetworkID, network)
		if err != nil {
			return nil, errors.WithMessagef(err, "Failed to add network '%s' to service load balancer management", flannelNetworkID)
		}

		d.networksByDockerID[dockerNetworkID] = network
	}

	d.networksByFlannelID[flannelNetworkID] = network

	return network, nil
}

func (d *flannelDriver) handleNetworksAdded(added []etcd.Item[common.NetworkInfo]) {
	for _, addedItem := range added {
		networkInfo := addedItem.Value
		d.dnsResolver.AddNetwork(networkInfo)
		_, err := d.getOrCreateNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if err != nil {
			log.Printf("Failed to handle added or changed network %s / %s: %s\n", networkInfo.DockerID, networkInfo.FlannelID, err)
		}
	}
}

func (d *flannelDriver) handleNetworksChanged(changed []etcd.ItemChange[common.NetworkInfo]) {
	for _, changedItem := range changed {
		networkInfo := changedItem.Current
		_, err := d.getOrCreateNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if err != nil {
			log.Printf("Failed to handle added or changed network %s / %s: %s\n", networkInfo.DockerID, networkInfo.FlannelID, err)
		}
	}
}

func (d *flannelDriver) handleNetworksRemoved(removed []etcd.Item[common.NetworkInfo]) {
	for _, removedItem := range removed {
		networkInfo := removedItem.Value
		d.dnsResolver.RemoveNetwork(networkInfo)
		if err := d.serviceLbsManagement.DeleteNetwork(networkInfo.DockerID); err != nil {
			log.Printf("Error handling deleted network %s, err: %v", networkInfo.FlannelID, err)
		}
		network, exists := d.getNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if exists {
			fmt.Printf("Deleting network %s\n", networkInfo.FlannelID)
			err := network.Delete()
			if err != nil {
				log.Printf("Failed to remove network '%s': %+v\n", networkInfo.FlannelID, err)
			}
		}
		if err := d.globalAddressSpace.ReleasePool(networkInfo.FlannelID); err != nil {
			log.Printf("Failed to release pool for network '%s': %+v\n", networkInfo.FlannelID, err)
		}
		delete(d.networksByDockerID, networkInfo.DockerID)
		delete(d.networksByFlannelID, networkInfo.FlannelID)
	}
}

func (d *flannelDriver) handleContainersAdded(added []etcd.ShardItem[docker.ContainerInfo]) {
	for _, addedItem := range added {
		containerInfo := addedItem.Value
		d.dnsResolver.AddContainer(containerInfo.ContainerInfo)
		for dockerNetworkID, ipamIP := range containerInfo.IpamIPs {

			network, exists := d.networksByDockerID[dockerNetworkID]
			if !exists {
				continue
			}

			if !ipamIP.Equal(containerInfo.IPs[dockerNetworkID]) {
				if network.GetInfo().HostSubnet.Contains(ipamIP) {
					fmt.Printf("Releasing IPAM IP %s of container %s\n", ipamIP, containerInfo.ID)
					err := network.GetPool().ReleaseIP(ipamIP.String())
					if err != nil {
						log.Printf("Failed to release IPAM IP %s for network %s: %v", ipamIP.String(), dockerNetworkID, err)
					}
				}
			}
		}

		if containerInfo.ServiceID != "" {
			service, exists := d.services[containerInfo.ServiceID]
			if !exists {
				service = d.createService(containerInfo.ServiceID, containerInfo.ServiceName)
			}
			service.AddContainer(containerInfo.ContainerInfo)
		}
	}
}

func (d *flannelDriver) handleContainersChanged(changed []etcd.ShardItemChange[docker.ContainerInfo]) {
	for _, changedItem := range changed {
		log.Printf("Received container changed info for container %s on host %s. Currently not handling it. Previous: %+v, Current: %+v\n", changedItem.ID, changedItem.ShardKey, changedItem.Previous, changedItem.Current)
	}
}

func (d *flannelDriver) handleContainersRemoved(removed []etcd.ShardItem[docker.ContainerInfo]) {
	for _, removedItem := range removed {
		containerInfo := removedItem.Value
		d.dnsResolver.RemoveContainer(containerInfo.ContainerInfo)
		service, exists := d.services[containerInfo.ServiceID]
		if exists {
			service.RemoveContainer(containerInfo.ID)
		}
		nameserver, exists := d.nameserversBySandboxKey[containerInfo.SandboxKey]
		if exists {
			err := nameserver.DeactivateAndCleanup()
			if err != nil {
				log.Printf("Error deactivating nameserver for container %s: %v\n", containerInfo.ID, err)
			}
			delete(d.nameserversBySandboxKey, containerInfo.SandboxKey)
		}
	}
}

// TODO: Properly handle startup

func countPoolSizeSubnets(completeSpace []net.IPNet, poolSize int) int {
	total := 0

	for _, subnet := range completeSpace {
		maskSize, bits := subnet.Mask.Size()

		if maskSize > poolSize || bits != 32 {
			continue
		}

		delta := poolSize - maskSize
		subnets := 1 << delta

		total += subnets
	}

	return total
}

func (d *flannelDriver) getOrAddNameserver(sandboxKey string) (dns.Nameserver, error) {
	nameserver, exists := d.nameserversBySandboxKey[sandboxKey]
	if exists {
		return nameserver, nil
	}
	nameserver = dns.NewNameserver(sandboxKey, d.dnsResolver)
	err := nameserver.Activate()
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to activate nameserver in namespace %s", sandboxKey)
	}

	d.nameserversBySandboxKey[sandboxKey] = nameserver
	return nameserver, nil
}

func (d *flannelDriver) createService(id, name string) common.Service {
	service := common.NewService(id, name)

	// TODO: Store unsubscribe functions and use them upon service deletion
	// or not? because when the service is being deleted, it is gone, no events will be raised anyway
	service.Events().OnInitialized.Subscribe(func(s common.Service) {
		d.dnsResolver.AddService(s)
		if s.GetInfo().EndpointMode == common.ServiceEndpointModeVip {
			err := d.serviceLbsManagement.CreateLoadBalancer(s)
			if err != nil {
				log.Printf("Failed to create load balancer of service %s: %v\n", name, err)
			} else {
				fmt.Printf("Created load balancer of service %s\n", name)
			}
		}
	})

	d.services[id] = service

	return service
}

func (d *flannelDriver) injectNameserverIntoAlreadyRunningContainers() {
	ourNetworkIDs := maps.Keys(d.dockerData.GetNetworks().GetAll())
	containersStore := d.dockerData.GetContainers()
	for shardKey, shardContainers := range containersStore.GetAll() {
		if shardKey != containersStore.GetLocalShardKey() {
			continue
		}
		for _, container := range shardContainers {
			if lo.Some(ourNetworkIDs, maps.Keys(container.IPs)) {
				nameserver, err := d.getOrAddNameserver(adjustSandboxKey(container.SandboxKey))
				if err != nil {
					log.Printf("Error getting nameserver for container %s: %v\n", container, err)
					continue
				}

				for networkID, endpointID := range container.Endpoints {
					d.nameserversByEndpointID[endpointID] = nameserver
					nameserver.AddValidNetworkID(networkID)
				}
			}
		}
	}
}

func adjustSandboxKey(sandboxKey string) string {
	if strings.Index(sandboxKey, "/hostfs") == 0 {
		return sandboxKey
	}

	return "/hostfs" + sandboxKey
}
