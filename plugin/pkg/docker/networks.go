package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/network"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"log"
)

func (d *data) initNetworks() error {
	d.Lock()
	defer d.Unlock()

	if d.isManagerNode {
		fmt.Println("Initializing networks on manager node...")
		networksInfos, err := d.getNetworksInfosFromDocker()
		fmt.Printf("Found docker networks: %+v \n", networksInfos)
		err = d.networks.(etcd.WriteOnlyStore[common.NetworkInfo]).Init(networksInfos)
		if err != nil {
			return errors.WithMessage(err, "Error initializing networks")
		}
	} else {
		fmt.Println("Initializing networks on worker node...")
		err := d.networks.(etcd.ReadOnlyStore[common.NetworkInfo]).Init()
		if err != nil {
			return errors.WithMessage(err, "Error initializing networks")
		}
	}

	fmt.Println("Networks initialized")
	return nil
}

func (d *data) syncNetworks() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing networks...")

	if d.isManagerNode {
		networksInfos, err := d.getNetworksInfosFromDocker()

		err = d.networks.(etcd.WriteOnlyStore[common.NetworkInfo]).Sync(networksInfos)
		if err != nil {
			return errors.WithMessage(err, "Error syncing networks")
		}
	} else {
		err := d.networks.(etcd.ReadOnlyStore[common.NetworkInfo]).Sync()
		if err != nil {
			return errors.WithMessage(err, "Error syncing networks")
		}
	}

	return nil
}

func (d *data) getNetworksInfosFromDocker() (networkInfos map[string]common.NetworkInfo, err error) {
	rawNetworks, err := d.dockerClient.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "Error listing docker services")
	}

	networkInfos = map[string]common.NetworkInfo{}

	for _, network := range rawNetworks {
		networkInfo, err := d.getNetworkInfoFromDocker(network.ID)
		if err != nil {
			log.Printf("Error getting network info for network with ID %s. Skipping...\n", network.ID)
			continue
		}
		networkInfos[networkInfo.DockerID] = *networkInfo
	}

	return
}

func (d *data) getNetworkInfoFromDocker(dockerNetworkID string) (networkInfo *common.NetworkInfo, err error) {
	network, err := d.dockerClient.NetworkInspect(context.Background(), dockerNetworkID, network.InspectOptions{})
	if err != nil {
		return nil, errors.WithMessagef(err, "Error inspecting docker network %s", dockerNetworkID)
	}

	flannelNetworkID := network.IPAM.Options["flannel-id"]

	var subnet string
	if len(network.IPAM.Config) > 0 {
		subnet = network.IPAM.Config[0].Subnet
	}
	return &common.NetworkInfo{
		DockerID:  dockerNetworkID,
		FlannelID: flannelNetworkID,
		Name:      network.Name,
		Subnet:    subnet,
	}, nil
}

func (d *data) handleNetwork(dockerNetworkID string) error {
	fmt.Printf("Handling docker network %s\n", dockerNetworkID)
	networkInfo, err := d.getNetworkInfoFromDocker(dockerNetworkID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker network %s", dockerNetworkID)
	}

	err = d.networks.(etcd.WriteOnlyStore[common.NetworkInfo]).AddOrUpdateItem(dockerNetworkID, *networkInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error adding or updating network info %s", dockerNetworkID)
	}
	return nil
}

func (d *data) handleDeletedNetwork(dockerNetworkID string) error {
	fmt.Printf("Deleting network %s\n", dockerNetworkID)
	return d.networks.(etcd.WriteOnlyStore[common.NetworkInfo]).DeleteItem(dockerNetworkID)
}
