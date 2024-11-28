package driver

import (
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/pkg/errors"
	"log"
	"net"
)

func (d *flannelDriver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	return &network.CapabilitiesResponse{
		Scope:             "global",
		ConnectivityScope: "global",
	}, nil
}

func (d *flannelDriver) CreateNetwork(request *network.CreateNetworkRequest) error {
	return nil
}

func (d *flannelDriver) AllocateNetwork(request *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	return &network.AllocateNetworkResponse{}, nil
}

func (d *flannelDriver) DeleteNetwork(request *network.DeleteNetworkRequest) error {
	return nil
}

func (d *flannelDriver) FreeNetwork(request *network.FreeNetworkRequest) error {
	return nil
}

func (d *flannelDriver) CreateEndpoint(request *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	d.Lock()
	defer d.Unlock()

	if request.Interface == nil || request.Interface.Address == "" || request.Interface.MacAddress == "" {
		log.Println("Received no interface info or interface info without address or mac address. This is not supported")
		return nil, types.InvalidParameterErrorf("Need interface info with IPv4 address and MAC address as input for endpoint %s for network %s.", request.EndpointID, request.NetworkID)
	}

	flannelNetwork, err := d.getFlannelNetworkFromDockerNetworkID(request.NetworkID)

	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(request.Interface.Address)
	_, err = flannelNetwork.AddEndpoint(request.EndpointID, ip, request.Interface.MacAddress)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create endpoint %s for flannel network %s", request.EndpointID, flannelNetwork.GetInfo().ID)
	}

	// Don't return the interface we got passed in. Even without changing any values, it will lead
	// to an error, saying values can't be changed
	return &network.CreateEndpointResponse{}, nil
}

func (d *flannelDriver) DeleteEndpoint(request *network.DeleteEndpointRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetwork, err := d.getFlannelNetworkFromDockerNetworkID(request.NetworkID)

	if err != nil {
		return err
	}

	err = flannelNetwork.DeleteEndpoint(request.EndpointID)
	if err != nil {
		return errors.Wrapf(err, "failed to delete endpoint %s", request.EndpointID)
	}

	return nil
}

func (d *flannelDriver) EndpointInfo(request *network.InfoRequest) (*network.InfoResponse, error) {
	d.Lock()
	defer d.Unlock()

	_, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return nil, err
	}

	value := make(map[string]string)

	value["ip_address"] = endpoint.GetInfo().IpAddress.String()
	value["mac_address"] = endpoint.GetInfo().MacAddress

	resp := &network.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (d *flannelDriver) Join(request *network.JoinRequest) (*network.JoinResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetwork, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return nil, err
	}

	err = endpoint.Join(request.EndpointID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to join endpoint %s to network %s", request.EndpointID, request.NetworkID)
	}

	networkInfo := flannelNetwork.GetInfo()
	endpointInfo := endpoint.GetInfo()

	return &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   endpointInfo.VethInside,
			DstPrefix: "eth",
		},
		StaticRoutes: []*network.StaticRoute{
			{
				Destination: networkInfo.HostSubnet.String(),
				RouteType:   types.CONNECTED,
			},
			{
				Destination: networkInfo.Network.String(),
				RouteType:   types.NEXTHOP,
				NextHop:     networkInfo.LocalGateway.String(),
			},
		},
		DisableGatewayService: false,
	}, nil
}

func (d *flannelDriver) Leave(request *network.LeaveRequest) error {
	d.Lock()
	defer d.Unlock()

	_, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return err
	}

	return endpoint.Leave()
}

func (d *flannelDriver) DiscoverNew(notification *network.DiscoveryNotification) error {
	return nil
}

func (d *flannelDriver) DiscoverDelete(notification *network.DiscoveryNotification) error {
	return nil
}

func (d *flannelDriver) ProgramExternalConnectivity(request *network.ProgramExternalConnectivityRequest) error {
	return nil
}

func (d *flannelDriver) RevokeExternalConnectivity(request *network.RevokeExternalConnectivityRequest) error {
	return nil
}