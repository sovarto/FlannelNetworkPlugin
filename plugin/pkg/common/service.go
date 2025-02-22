package common

import (
	"github.com/samber/lo"
	"golang.org/x/exp/maps"
	"net"
	"sync"
)

type ContainerInfo struct {
	ID          string              `json:"ContainerID"`
	Name        string              `json:"ContainerName"`
	ServiceID   string              `json:"ServiceID"`
	ServiceName string              `json:"ServiceName"`
	SandboxKey  string              `json:"SandboxKey"`
	IPs         map[string]net.IP   `json:"IPs"`       // networkID -> IP
	DNSNames    map[string][]string `json:"DNSNames"`  // networkID -> DNS names
	Endpoints   map[string]string   `json:"Endpoints"` // networkID -> endpoint ID
}

var (
	ServiceEndpointModeVip   = "vip"
	ServiceEndpointModeDnsrr = "dnsrr"
)

// Resolver: has to be updated for every change in VIPs and backend IPs. Needs to know the endpoint mode first though
// Load Balancer: reserves the VIPs itself and needs to communicate them outside. Needs to be updated for every change in backend IPs. May only be created for endpoint mode "vip"

type OnContainerData struct {
	Service   Service
	Container ContainerInfo
}

type ServiceEvents struct {
	OnInitialized         EventSubscriber[Service]
	OnVIPsChanged         EventSubscriber[Service]
	OnNetworksChanged     EventSubscriber[Service]
	OnEndpointModeChanged EventSubscriber[Service]
	OnContainerAdded      EventSubscriber[OnContainerData]
	OnContainerRemoved    EventSubscriber[OnContainerData]
}

type serviceEvents struct {
	onInitialized         Event[Service]
	onVIPsChanged         Event[Service]
	onNetworksChanged     Event[Service]
	onEndpointModeChanged Event[Service]
	onContainerAdded      Event[OnContainerData]
	onContainerRemoved    Event[OnContainerData]
}

// Service
// raises events after the service has been fully initialized - this is the case when we know EndpointMode, IpamVIPs
// and Networks.
type Service interface {
	IsInitialized() bool
	GetInfo() ServiceInfo
	// SetNetworks ipamVIPs may be nil or empty, specifically when the service is in dnsrr endpoint mode
	SetNetworks(networks []string, ipamVIPs map[string]net.IP)
	SetEndpointMode(endpoint string)
	SetVIPs(map[string]net.IP)
	AddContainer(container ContainerInfo)
	RemoveContainer(containerID string)
	Events() ServiceEvents
}

type ServiceInfo struct {
	ID           string
	Name         string
	EndpointMode string
	Networks     []string
	VIPs         map[string]net.IP
	IpamVIPs     map[string]net.IP
	Containers   map[string]ContainerInfo
}

type service struct {
	id           string
	name         string
	endpointMode string
	networks     []string
	vips         map[string]net.IP
	ipamVIPs     map[string]net.IP
	containers   map[string]ContainerInfo
	events       serviceEvents
	sync.Mutex
}

func NewService(id, name string) Service {
	events := serviceEvents{
		onInitialized:         NewEvent[Service](),
		onVIPsChanged:         NewEvent[Service](),
		onNetworksChanged:     NewEvent[Service](),
		onEndpointModeChanged: NewEvent[Service](),
		onContainerAdded:      NewEvent[OnContainerData](),
		onContainerRemoved:    NewEvent[OnContainerData](),
	}
	return &service{
		id:         id,
		name:       name,
		networks:   make([]string, 0),
		vips:       map[string]net.IP{},
		ipamVIPs:   map[string]net.IP{},
		containers: map[string]ContainerInfo{},
		events:     events,
	}
}

func (s *service) Events() ServiceEvents {
	return ServiceEvents{
		OnInitialized:         s.events.onInitialized,
		OnVIPsChanged:         s.events.onVIPsChanged,
		OnNetworksChanged:     s.events.onNetworksChanged,
		OnEndpointModeChanged: s.events.onEndpointModeChanged,
		OnContainerAdded:      s.events.onContainerAdded,
		OnContainerRemoved:    s.events.onContainerRemoved,
	}
}

func (s *service) IsInitialized() bool {
	return s.id != "" && s.name != "" && s.networks != nil && len(s.networks) > 0 &&
		(s.endpointMode == ServiceEndpointModeDnsrr ||
			(s.endpointMode == ServiceEndpointModeVip && s.ipamVIPs != nil && len(s.ipamVIPs) > 0))
}

func (s *service) GetInfo() ServiceInfo {
	return ServiceInfo{
		ID:           s.id,
		Name:         s.name,
		EndpointMode: s.endpointMode,
		Networks:     s.networks,
		VIPs:         s.vips,
		IpamVIPs:     s.ipamVIPs,
		Containers:   s.containers,
	}
}

func (s *service) SetVIPs(vips map[string]net.IP) {
	s.Lock()
	vipsChanged := CompareIPMaps(s.vips, vips)
	s.vips = maps.Clone(vips)
	s.Unlock()

	if s.IsInitialized() && vipsChanged {
		s.events.onVIPsChanged.Raise(s)
	}
}

func (s *service) AddContainer(container ContainerInfo) {
	s.Lock()
	s.containers[container.ID] = container
	s.Unlock()

	if s.IsInitialized() {
		s.events.onContainerAdded.Raise(OnContainerData{
			Service:   s,
			Container: container,
		})
	}
}

func (s *service) RemoveContainer(containerID string) {
	s.Lock()

	container, exists := s.containers[containerID]
	if exists {
		delete(s.containers, container.ID)
		s.Unlock()
		if s.IsInitialized() {
			s.events.onContainerRemoved.Raise(OnContainerData{
				Service:   s,
				Container: container,
			})
		}
	} else {
		s.Unlock()
	}
}

func (s *service) SetNetworks(networks []string, ipamVIPs map[string]net.IP) {
	s.Lock()

	wasInitialized := s.IsInitialized()

	deletedNetworks, addedNetworks := lo.Difference(networks, s.networks)
	ipamVIPsChanged := CompareIPMaps(s.ipamVIPs, ipamVIPs)

	s.networks = make([]string, len(networks))
	copy(s.networks, networks)
	s.ipamVIPs = maps.Clone(ipamVIPs)

	s.Unlock()

	if s.IsInitialized() {
		if !wasInitialized {
			s.events.onInitialized.Raise(s)
		} else if ipamVIPsChanged || len(deletedNetworks) > 0 || len(addedNetworks) > 0 {
			s.events.onNetworksChanged.Raise(s)
		}
	}
}

func (s *service) SetEndpointMode(endpointMode string) {
	s.Lock()
	wasInitialized := s.IsInitialized()

	endpointModeChanged := endpointMode != s.endpointMode
	s.endpointMode = endpointMode
	s.Unlock()

	if s.IsInitialized() {
		if !wasInitialized {
			s.events.onInitialized.Raise(s)
		} else if endpointModeChanged {
			s.events.onEndpointModeChanged.Raise(s)
		}
	}
}
