package driver

import (
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"golang.org/x/exp/maps"
	"log"
	"net"
)

// CreateNetwork happens when a container is being started that uses this network
func (d *FlannelDriver) CreateNetwork(req *network.CreateNetworkRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetworkId, exists := d.networkIdToFlannelNetworkId[req.NetworkID]

	if !exists {
		err := d.BuildNetworkIdMappings()

		if err != nil {
			return types.UnavailableErrorf("Failed to build network mappings: %s", err)
		}

		flannelNetworkId, exists = d.networkIdToFlannelNetworkId[req.NetworkID]
	}

	if !exists {
		log.Printf("Network %s not managed by us", req.NetworkID)
		return types.ForbiddenErrorf("Network %s not managed by us", req.NetworkID)
	}

	if _, ok := d.networks[flannelNetworkId]; !ok {
		log.Printf("We've no internal state for network %s - flannel network ID: %s - although we should. We've state for these flannel network IDs: %+v", req.NetworkID, flannelNetworkId, maps.Keys(d.networks))
		return types.InternalErrorf("We've no internal state for network %s although we should", req.NetworkID)
	}

	return nil
}

func (d *FlannelDriver) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Skip if not in map */
	if _, ok := d.networks[req.NetworkID]; !ok {
		return nil
	}

	//if err := detectIpTables(); err != nil {
	//	return err
	//}

	err := deleteBridge(req.NetworkID)
	if err != nil {
		return err
	}

	// TODO:
	// Stop flannel process
	// Delete /<options.prefix>/<req.NetworkID> and all children from etcd
	// Mark subnet as free in etcd

	delete(d.networks, req.NetworkID)

	return nil
}

func (d *FlannelDriver) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	// This happens during docker network create
	// Maybe start flannel process?
	return nil, nil
}

func (d *FlannelDriver) FreeNetwork(req *network.FreeNetworkRequest) error {
	// Maybe stop flannel process?
	return nil
}

func (d *FlannelDriver) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetworkId, exists := d.networkIdToFlannelNetworkId[req.NetworkID]

	if !exists {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}
	/* Throw error if not in map */
	if _, ok := d.networks[flannelNetworkId]; !ok {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	interfaceInfo := new(network.EndpointInterface)

	if req.Interface == nil {
		// TODO: Verify that this guarantees uniqueness. If not, use something else

		// Generate the interface MAC Address by concatenating the network id and the endpoint id
		interfaceInfo.MacAddress = generateMacAddressFromID(req.NetworkID + "-" + req.EndpointID)
	}

	// Should we set the IP address here? Should we record it somewhere?

	parsedMac, _ := net.ParseMAC(interfaceInfo.MacAddress)

	endpoint := &FlannelEndpoint{
		macAddress: parsedMac,
	}

	d.networks[flannelNetworkId].endpoints[req.EndpointID] = endpoint

	resp := &network.CreateEndpointResponse{
		Interface: interfaceInfo,
	}

	return resp, nil
}

func (d *FlannelDriver) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Skip if not in map (both network and endpoint) */
	if _, netOk := d.networks[req.NetworkID]; !netOk {
		return nil
	}

	if _, epOk := d.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil
	}

	// Should we notify someone - e.g. service load balancer - about this?

	delete(d.networks[req.NetworkID].endpoints, req.EndpointID)

	return nil
}

func (d *FlannelDriver) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	d.Lock()
	defer d.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := d.networks[req.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := d.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := d.networks[req.NetworkID].endpoints[req.EndpointID]
	value := make(map[string]string)

	value["ip_address"] = ""
	value["mac_address"] = endpointInfo.macAddress.String()

	resp := &network.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (d *FlannelDriver) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetworkId, exists := d.networkIdToFlannelNetworkId[req.NetworkID]

	if !exists {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}
	/* Throw error (both network and endpoint) */
	if _, netOk := d.networks[flannelNetworkId]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := d.networks[flannelNetworkId].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := d.networks[flannelNetworkId].endpoints[req.EndpointID]
	vethInside, vethOutside, err := createVethPair(endpointInfo.macAddress)
	if err != nil {
		return nil, err
	}

	if err := attachInterfaceToBridge("flannel.1", vethOutside); err != nil {
		return nil, err
	}

	d.networks[flannelNetworkId].endpoints[req.EndpointID].vethInside = vethInside
	d.networks[flannelNetworkId].endpoints[req.EndpointID].vethOutside = vethOutside

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   vethInside,
			DstPrefix: "eth",
		},
		DisableGatewayService: true,
	}

	return resp, nil
}

func (d *FlannelDriver) Leave(req *network.LeaveRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := d.networks[req.NetworkID]; !netOk {
		return types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := d.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := d.networks[req.NetworkID].endpoints[req.EndpointID]

	if err := deleteVethPair(endpointInfo.vethOutside); err != nil {
		return err
	}

	return nil
}

func (d *FlannelDriver) DiscoverNew(req *network.DiscoveryNotification) error {
	return nil
}

func (d *FlannelDriver) DiscoverDelete(req *network.DiscoveryNotification) error {
	return nil
}

func (d *FlannelDriver) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	return nil
}

func (k *FlannelDriver) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	return nil
}
