package driver

import (
	"fmt"
	docker_ipam "github.com/docker/go-plugins-helpers/ipam"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"log"
	"strings"
)

func poolIDtoNetworkID(poolID string) string {
	return strings.Join(strings.Split(poolID, "-")[1:], "-")
}

func (d *flannelDriver) GetIpamCapabilities() (*docker_ipam.CapabilitiesResponse, error) {
	return &docker_ipam.CapabilitiesResponse{RequiresMACAddress: true}, nil
}

func (d *flannelDriver) GetDefaultAddressSpaces() (*docker_ipam.AddressSpacesResponse, error) {
	return &docker_ipam.AddressSpacesResponse{
		LocalDefaultAddressSpace:  "FlannelLocal",
		GlobalDefaultAddressSpace: "FlannelGlobal",
	}, nil
}

func (d *flannelDriver) RequestPool(request *docker_ipam.RequestPoolRequest) (*docker_ipam.RequestPoolResponse, error) {
	d.Lock()
	defer d.Unlock()

	if request.V6 {
		return nil, errors.New("flannel plugin does not support ipv6")
	}

	poolID := "FlannelPool"
	flannelNetworkID, exists := request.Options["flannel-id"]
	if exists && flannelNetworkID != "" {
		poolID = fmt.Sprintf("%s-%s", poolID, flannelNetworkID)
	} else {
		return nil, fmt.Errorf("the IPAM driver option 'flannel-id' needs to be set to a unique ID")
	}

	network, err := d.getOrCreateNetwork("", flannelNetworkID)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to ensure network '%s' is operational", flannelNetworkID)
	}

	return &docker_ipam.RequestPoolResponse{
		PoolID: poolID,
		Pool:   network.GetInfo().Network.String(),
	}, nil
}

func (d *flannelDriver) ReleasePool(request *docker_ipam.ReleasePoolRequest) error {
	// Release of pool resources is happening when we receive a docker event that the corresponding
	// docker network has been deleted
	return nil
}

func (d *flannelDriver) RequestAddress(request *docker_ipam.RequestAddressRequest) (*docker_ipam.RequestAddressResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID := poolIDtoNetworkID(request.PoolID)
	network, exists := d.networksByFlannelID[flannelNetworkID]
	if !exists {
		return nil, fmt.Errorf("no network found for pool '%s'", request.PoolID)
	}

	networkInfo := network.GetInfo()

	requestType, exists := request.Options["RequestAddressType"]
	if exists && requestType == "com.docker.network.gateway" {
		return &docker_ipam.RequestAddressResponse{Address: fmt.Sprintf("%s/32", networkInfo.LocalGateway)}, nil
	}

	mac := request.Options["com.docker.network.endpoint.macaddress"]

	reservationType := ipam.ReservationTypeReserved
	if request.Address != "" && mac != "" {
		reservationType = ipam.ReservationTypeContainerIP
	}
	address, err := network.GetPool().AllocateIP(request.Address, mac, reservationType, true)

	if err != nil {
		log.Printf("Failed to reserve address for network %s: %+v", flannelNetworkID, err)
		return nil, err
	}
	ones, _ := networkInfo.HostSubnet.Mask.Size()
	return &docker_ipam.RequestAddressResponse{Address: fmt.Sprintf("%s/%d", address, ones)}, nil
}

func (d *flannelDriver) ReleaseAddress(request *docker_ipam.ReleaseAddressRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID := poolIDtoNetworkID(request.PoolID)
	network, exists := d.networksByFlannelID[flannelNetworkID]
	if !exists {
		return fmt.Errorf("no network found for pool '%s'", request.PoolID)
	}
	return network.GetPool().ReleaseIP(request.Address)
}
