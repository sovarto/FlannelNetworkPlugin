package flannel_network

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/bridge"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Network interface {
	Ensure() error
	GetInfo() common.NetworkInfo
	Delete() error
	GetPool() ipam.AddressPool
	AddEndpoint(id string, ip net.IP, mac string) (Endpoint, error)
	GetEndpoint(id string) Endpoint
	DeleteEndpoint(id string) error
}

type network struct {
	id                    string
	pid                   int
	etcdClient            etcd.Client
	networkSubnet         net.IPNet
	hostSubnet            net.IPNet
	hostSubnetSize        int
	localGateway          net.IP
	mtu                   int
	defaultFlannelOptions []string
	pool                  ipam.AddressPool
	bridge                bridge.BridgeInterface
	endpoints             map[string]Endpoint
	sync.Mutex
}

func NewNetwork(etcdClient etcd.Client, id string, networkSubnet net.IPNet, hostSubnetSize int, defaultFlannelOptions []string) (Network, error) {
	result := &network{
		id:                    id,
		etcdClient:            etcdClient,
		networkSubnet:         networkSubnet,
		defaultFlannelOptions: defaultFlannelOptions,
		hostSubnetSize:        hostSubnetSize,
	}

	err := result.Ensure()
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (n *network) GetInfo() common.NetworkInfo {
	return common.NetworkInfo{
		ID:           n.id,
		MTU:          n.mtu,
		Network:      &n.networkSubnet,
		HostSubnet:   &n.hostSubnet,
		LocalGateway: n.localGateway,
	}
}

func (n *network) GetPool() ipam.AddressPool {
	return n.pool
}

func (n *network) Delete() error {
	n.Lock()
	defer n.Unlock()

	if n.pid != 0 {
		proc, err := os.FindProcess(n.pid)
		if err == nil {
			err := proc.Kill()
			if err != nil {
				return errors.Wrapf(err, "error killing flanneld process of network %s", n.id)
			}
			n.pid = 0
		}
	}

	_, err := etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		networkConfigKey := flannelConfigKey(n.etcdClient, n.id)

		result, err := n.readExistingNetworkConfig()
		if err != nil {
			return struct{}{}, errors.Wrapf(err, "error reading existing flannel network config for network %s", n.id)
		}
		if result.found {
			if result.config.Network != n.networkSubnet.String() {
				return struct{}{}, fmt.Errorf("the flannel config for network %s has unexpected network %s instead of the expected %s", n.id, result.config.Network, n.networkSubnet.String())
			}
		}
		resp, err := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.ModRevision(networkConfigKey), "=", result.revision)).
			Then(clientv3.OpDelete(networkConfigKey)).
			Commit()

		if err != nil {
			return struct{}{}, errors.Wrapf(err, "error deleting flannel network config for network %s", n.id)
		}

		if !resp.Succeeded {
			resp, err := connection.Client.Get(connection.Ctx, networkConfigKey)
			if err != nil {
				return struct{}{}, errors.Wrapf(err, "error deleting flannel network config for network %s, and error during check if it has since been deleted", n.id)
			}
			if resp.Kvs != nil && len(resp.Kvs) > 0 {
				return struct{}{}, fmt.Errorf("error deleting flannel network config for network %s", n.id)
			}
		}

		return struct{}{}, nil
	})

	if err != nil {
		return errors.Wrapf(err, "deleting network config failed for network %s", n.id)
	}

	err = n.bridge.Delete()
	if err != nil {
		return errors.Wrapf(err, "error deleting bridge interface for network %s", n.id)
	}

	err = n.pool.ReleaseAllIPs()
	if err != nil {
		return errors.Wrapf(err, "error releasing all IPs for network %s", n.id)
	}

	for endpointID, endpoint := range n.endpoints {
		err = endpoint.Delete()
		if err != nil {
			return errors.Wrapf(err, "error deleting endpoint %s for network %s", endpointID, n.id)
		}
	}

	n.endpoints = map[string]Endpoint{}
	n.localGateway = nil
	n.hostSubnet = net.IPNet{}
	n.mtu = 0

	return nil
}

func (n *network) Ensure() error {
	n.Lock()
	defer n.Unlock()

	_, err := n.ensureFlannelConfig()
	if err != nil {
		return err
	}

	if n.pid == 0 || !isProcessRunning(n.pid) {
		err = n.startFlannel()
		return err
	}

	return nil
}

type Config struct {
	Network   string        `json:"Network"`
	SubnetLen int           `json:"SubnetLen"`
	Backend   BackendConfig `json:"Backend"`
}

type BackendConfig struct {
	Type string `json:"Type"`
}

func flannelConfigKey(client etcd.Client, networkID string) string {
	return client.GetKey(networkID)
}

func (n *network) ensureFlannelConfig() (struct{}, error) {
	return etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		networkConfigKey := flannelConfigKey(n.etcdClient, n.id)

		result, err := n.readExistingNetworkConfig()
		if err != nil {
			return struct{}{}, errors.Wrapf(err, "error reading existing flannel network config for network %s", n.id)
		}
		if result.found {
			if result.config.Network == n.networkSubnet.String() {
				return struct{}{}, nil
			}
			return struct{}{}, fmt.Errorf("there already is a flannel config for network %s but it is for network %s instead of the expected %s", n.id, result.config.Network, n.networkSubnet.String())
		}

		configData := Config{
			Network:   n.networkSubnet.String(),
			SubnetLen: n.hostSubnetSize,
			Backend: BackendConfig{
				Type: "vxlan",
			},
		}

		// Serialize the configuration to a JSON string
		configBytes, err := json.Marshal(configData)
		if err != nil {
			log.Println("Failed to serialize configuration:", err)
			return struct{}{}, err
		}

		configString := string(configBytes)

		txn := connection.Client.Txn(connection.Ctx).If(
			clientv3.Compare(clientv3.CreateRevision(networkConfigKey), "=", 0),
		).Then(
			clientv3.OpPut(networkConfigKey, configString),
		)

		resp, err := txn.Commit()
		if err != nil {
			log.Println("Transaction failed:", err)
			return struct{}{}, err
		}

		if !resp.Succeeded {
			fmt.Printf("flannel network config for network %s was created by another node. Trying to reuse", n.id)
			result, err := n.readExistingNetworkConfig()
			if err != nil {
				return struct{}{}, errors.Wrapf(err, "error reading existing flannel network config for network %s", n.id)
			}
			if result.found {
				if result.config.Network == n.networkSubnet.String() {
					return struct{}{}, nil
				}
				return struct{}{}, fmt.Errorf("there already is a flannel config for network %s but it is for network %s instead of the expected %s", n.id, result.config.Network, n.networkSubnet.String())
			}
		}

		return struct{}{}, nil
	})
}

type ReadNetworkConfigResult struct {
	config   Config
	revision int64
	found    bool
}

func (n *network) readExistingNetworkConfig() (ReadNetworkConfigResult, error) {
	return etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (ReadNetworkConfigResult, error) {
		networkConfigKey := flannelConfigKey(n.etcdClient, n.id)

		resp, err := connection.Client.Get(connection.Ctx, networkConfigKey)
		if err != nil {
			log.Printf("Failed to get network config %s:\n%+v", networkConfigKey, err)
			return ReadNetworkConfigResult{found: false}, err
		}
		if len(resp.Kvs) > 0 {
			var configData Config
			err := json.Unmarshal(resp.Kvs[0].Value, &configData)
			if err != nil {
				log.Println("Failed to deserialize configuration:", err)
				return ReadNetworkConfigResult{found: true, revision: resp.Header.Revision}, err
			}

			return ReadNetworkConfigResult{config: configData, found: true, revision: resp.Header.Revision}, nil
		}
		message := fmt.Sprintf("Expected network config '%s' missing", networkConfigKey)
		return ReadNetworkConfigResult{found: false}, errors.New(message)
	})
}

func (n *network) startFlannel() error {
	subnetFile := fmt.Sprintf("/flannel-env/%s.env", n.id)
	etcdPrefix := n.etcdClient.GetKey(n.id)

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
		fmt.Sprintf("-etcd-endpoints=%s", strings.Join(n.etcdClient.GetEndpoints(), ",")),
	}
	args = append(args, n.defaultFlannelOptions...)

	cmd := exec.Command("/flanneld", args...)

	// Capture stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("Failed to get stdout pipe:", err)
		return err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		log.Println("Failed to get stderr pipe:", err)
		return err
	}

	bootstrapDoneChan := make(chan struct{})

	go readPipe(stdoutPipe, bootstrapDoneChan)
	go readPipe(stderrPipe, bootstrapDoneChan)

	// Start the process
	if err := cmd.Start(); err != nil {
		log.Println("Failed to start flanneld:", err)
		return err
	}

	fmt.Printf("flanneld started with PID %d for flannel network id %s\n", cmd.Process.Pid, n.id)

	exitChan := make(chan error, 1)

	// Goroutine to wait for the process to exit
	go func() {
		err := cmd.Wait()
		exitChan <- err
	}()

	// Wait for "bootstrap done", process exit, or timeout
	select {
	case err := <-exitChan:
		// Process exited before "bootstrap done" or timeout
		log.Printf("flanneld process exited prematurely: %v", err)
		return errors.Wrapf(err, "flanneld exited prematurely for network %s", n.id)
	case <-bootstrapDoneChan:
		// "bootstrap done" was found
		fmt.Println("flanneld bootstrap completed successfully")
	case <-time.After(1500 * time.Millisecond):
		// Timeout occurred before "bootstrap done"
		log.Printf("flanneld failed to bootstrap within 1.5 seconds for network %s\n", n.id)
		// Kill the process
		if err := cmd.Process.Kill(); err != nil {
			log.Println("Failed to kill flanneld process:", err)
		}
		return fmt.Errorf("flanneld failed to bootstrap within 1.5 seconds for network %s", n.id)
	}

	err = n.loadFlannelConfig(subnetFile)
	if err != nil {
		if err := cmd.Process.Kill(); err != nil {
			log.Println("Failed to kill flanneld process:", err)
		}
		return err
	}

	n.pid = cmd.Process.Pid
	// network.ReserveAddresses(config.Gateway.String())

	return nil
}

func (n *network) loadFlannelConfig(filename string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForFileWithContext(ctx, filename)
	if err != nil {
		return errors.Wrapf(err, "flannel env %s missing", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return errors.Wrapf(err, "failed to open file: %s", filename)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Printf("Skipping invalid line: %s\n", line)
			continue
		}

		key := parts[0]
		value := parts[1]

		if !strings.HasPrefix(key, "FLANNEL_") {
			fmt.Printf("Skipping unrecognized key: %s\n", key)
			continue
		}

		key = strings.TrimPrefix(key, "FLANNEL_")

		switch key {
		case "NETWORK":
			_, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return errors.Wrapf(err, "invalid CIDR format for network: %s", value)
			}
			n.networkSubnet = *ipNet
		case "SUBNET":
			ip, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return errors.Wrapf(err, "invalid CIDR format for subnet: %s", value)
			}
			pool, err := ipam.NewEtcdBasedAddressPool(n.id,
				*ipNet, n.etcdClient.CreateSubClient("host-subnets", common.SubnetToKey(ipNet.String())))
			if err != nil {
				return errors.Wrapf(err, "can't create address pool for network %s and subnet %s", n.id, ipNet.String())
			}
			n.hostSubnet = *ipNet
			n.localGateway = ip
			n.pool = pool

		case "MTU":
			mtu, err := strconv.Atoi(value)
			if err != nil {
				return errors.Wrapf(err, "invalid MTU value '%s'", value)
			}
			n.mtu = mtu
		case "IPMASQ":
			// Ignore
			break
		default:
			fmt.Printf("Unknown configuration key hwile loading flannel env %s: %s\n", filename, key)
		}
	}

	if err := scanner.Err(); err != nil {
		return errors.Wrapf(err, "error reading file: %s", filename)
	}

	b, err := bridge.NewBridgeInterface(n.GetInfo())
	if err != nil {
		return errors.Wrapf(err, "error creating bridge interface")
	}

	n.bridge = b

	return nil
}

func (n *network) AddEndpoint(id string, ip net.IP, mac string) (Endpoint, error) {
	endpoint, err := NewEndpoint(n.etcdClient.CreateSubClient("endpoints"), id, ip, mac, n.bridge)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating endpoint for network %s", n.id)
	}
	n.endpoints[id] = endpoint

	return endpoint, nil
}

func (n *network) DeleteEndpoint(id string) error {
	endpoint, exists := n.endpoints[id]
	if !exists {
		return errors.Errorf("endpoint %s does not exist", id)
	}

	err := endpoint.Delete()
	if err != nil {
		return errors.Wrapf(err, "error deleting endpoint for network %s", n.id)
	}

	delete(n.endpoints, id)
	return nil
}

func (n *network) GetEndpoint(id string) Endpoint {
	return n.endpoints[id]
}