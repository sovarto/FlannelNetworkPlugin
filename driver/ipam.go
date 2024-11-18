package driver

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/go-plugins-helpers/ipam"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"strings"
	"time"
)

func (d *FlannelDriver) RequestPool(request *ipam.RequestPoolRequest) (*ipam.RequestPoolResponse, error) {
	log.Printf("Received RequestPool req: %+v\n", request)

	if request.V6 {
		return nil, errors.New("flannel plugin does not support ipv6")
	}

	poolID := "FlannelPool"
	flannelNetworkId, exists := request.Options["id"]
	if exists && flannelNetworkId != "" {
		poolID = fmt.Sprintf("%s-%s", poolID, flannelNetworkId)
	} else {
		return nil, errors.New("the IPAM driver option 'id' needs to be set to a unique ID")
	}

	network, err := d.ensureFlannelIsConfiguredAndRunning(flannelNetworkId)
	if err != nil {
		return nil, err
	}

	return &ipam.RequestPoolResponse{PoolID: poolID, Pool: network.config.Network}, nil
}

func (d *FlannelDriver) ReleasePool(request *ipam.ReleasePoolRequest) error {
	log.Printf("Received ReleasePool req: %+v\n", request)
	//TODO implement me
	//panic("implement me")
	return nil
}

func (d *FlannelDriver) RequestAddress(request *ipam.RequestAddressRequest) (*ipam.RequestAddressResponse, error) {
	log.Printf("Received RequestAddress req: %+v\n", request)

	flannelNetworkId := strings.Join(strings.Split(request.PoolID, "-")[1:], "-")

	network, err := d.ensureFlannelIsConfiguredAndRunning(flannelNetworkId)
	if err != nil {
		return nil, err
	}

	return &ipam.RequestAddressResponse{Address: request.Address}, nil
}

func (d *FlannelDriver) ReleaseAddress(request *ipam.ReleaseAddressRequest) error {
	log.Printf("Received ReleaseAddress req: %+v\n", request)
	//TODO implement me
	//panic("implement me")
	return nil
}
