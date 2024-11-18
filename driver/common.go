package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/docker/go-plugins-helpers/sdk"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type FlannelEndpoint struct {
	ipAddress   string
	macAddress  net.HardwareAddr
	vethInside  string
	vethOutside string
}

type FlannelNetwork struct {
	bridgeName        string
	config            FlannelConfig
	endpoints         map[string]*FlannelEndpoint
	reservedAddresses map[string]struct{} // This will contain all addresses that have been reserved in the past, even those that have since been freed. This allows us to only re-use IP addresses when no more un-reserved addresses exist
	pid               int
	sync.Mutex
}

func NewFlannelNetwork() *FlannelNetwork {
	return &FlannelNetwork{endpoints: make(map[string]*FlannelEndpoint), reservedAddresses: make(map[string]struct{})}
}

type FlannelConfig struct {
	Network string // The subnet of the network across all hosts
	Subnet  string // The subnet for this network on the current host. Inside the network subnet
	Gateway string
	MTU     int
	IPMasq  bool
}

type FlannelDriver struct {
	networks                    map[string]*FlannelNetwork
	networkIdToFlannelNetworkId map[string]string
	defaultFlannelOptions       []string
	etcdClient                  *EtcdClient
	sync.Mutex
}

func NewFlannelDriver(etcdClient *EtcdClient, defaultFlannelOptions []string) *FlannelDriver {
	return &FlannelDriver{
		networks:                    make(map[string]*FlannelNetwork),
		networkIdToFlannelNetworkId: make(map[string]string),
		defaultFlannelOptions:       defaultFlannelOptions,
		etcdClient:                  etcdClient,
	}
}

func ServeFlannelDriver(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, availableSubnets []string, defaultHostSubnetSize int) {

	flannelDriver := NewFlannelDriver(NewEtcdClient(etcdEndPoints, 5*time.Second, etcdPrefix, availableSubnets, defaultHostSubnetSize), defaultFlannelOptions)

	handler := sdk.NewHandler(`{"Implements": ["IpamDriver", "NetworkDriver"]}`)
	initIpamMux(&handler, flannelDriver)
	initNetworkMux(&handler, flannelDriver)

	if err := handler.ServeUnix("flannel-np", 0); err != nil {
		log.Fatalf("ERROR: %s init failed, can't open socket: %v", "flannel-np", err)
	}
}

func (d *FlannelDriver) ensureFlannelIsConfiguredAndRunning(flannelNetworkId string) (*FlannelNetwork, error) {
	log.Println("ensureFlannelIsConfiguredAndRunning - before mutex")

	d.Mutex.Lock()
	defer d.Mutex.Unlock()

	log.Println("ensureFlannelIsConfiguredAndRunning - after mutex")

	flannelNetwork, exists := d.networks[flannelNetworkId]
	if !exists {
		log.Println("ensureFlannelIsConfiguredAndRunning - no network entry")
		_, err := d.etcdClient.EnsureFlannelConfig(flannelNetworkId)
		log.Println("ensureFlannelIsConfiguredAndRunning - after EnsureFlannelConfig")
		if err != nil {
			return nil, err
		}

		flannelNetwork = NewFlannelNetwork()

		err = d.startFlannel(flannelNetworkId, flannelNetwork)
		log.Println("ensureFlannelIsConfiguredAndRunning - after startFlannel")
		if err != nil {
			return nil, err
		}

		d.networks[flannelNetworkId] = flannelNetwork

		return flannelNetwork, nil
	} else {
		log.Println("ensureFlannelIsConfiguredAndRunning - has network entry")
		if flannelNetwork.pid == 0 || !isProcessRunning(flannelNetwork.pid) {
			log.Println("ensureFlannelIsConfiguredAndRunning - pid 0 or process not running")
			err := d.startFlannel(flannelNetworkId, flannelNetwork)
			log.Println("ensureFlannelIsConfiguredAndRunning - after startFlannel")
			if err != nil {
				return nil, err
			}
		}

		log.Println("ensureFlannelIsConfiguredAndRunning - flannel is running")
		return flannelNetwork, nil
	}
}

func (d *FlannelDriver) startFlannel(flannelNetworkId string, network *FlannelNetwork) error {
	subnetFile := fmt.Sprintf("/flannel-env/%s.env", flannelNetworkId)
	etcdPrefix := fmt.Sprintf("%s/%s", d.etcdClient.prefix, flannelNetworkId)

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
		fmt.Sprintf("-etcd-endpoints=%s", strings.Join(d.etcdClient.endpoints, ",")),
	}
	args = append(args, d.defaultFlannelOptions...)

	cmd := exec.Command("/flanneld", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Println("Failed to start flanneld:", err)
		return err
	}

	log.Println("flanneld started with PID", cmd.Process.Pid)

	exitChan := make(chan error, 1)

	// Goroutine to wait for the process to exit
	go func() {
		err := cmd.Wait()
		exitChan <- err
	}()

	// Wait for 1.5 seconds
	select {
	case err := <-exitChan:
		// Process exited before 1.5 seconds
		log.Printf("flanneld process exited prematurely: %v", err)
		return fmt.Errorf("flanneld exited prematurely: %v", err)
	case <-time.After(1500 * time.Millisecond):
		// Process is still running after 1.5 seconds
		log.Println("flanneld is running and stable after 1.5 seconds")
	}

	config, err := loadFlannelConfig(subnetFile)
	if err != nil {
		cmd.Process.Kill()
		return err
	}

	network.Mutex.Lock()
	defer network.Mutex.Unlock()

	network.pid = cmd.Process.Pid
	network.config = config

	err = d.etcdClient.EnsureGatewayIsMarkedAsReserved(&config)
	if err != nil {
		return err
	}
	return nil
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if err.Error() == "os: process already finished" {
		return false
	}
	var errno syscall.Errno
	ok := errors.As(err, &errno)
	if !ok {
		return false
	}
	switch {
	case errors.Is(errno, syscall.ESRCH):
		return false
	case errors.Is(errno, syscall.EPERM):
		return true
	}
	return false
}

func waitForFileWithContext(ctx context.Context, path string) error {
	const pollInterval = 100 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			// Context has been canceled or timed out
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		default:
			// Continue to check for the file
		}

		// Attempt to get file info
		_, err := os.Stat(path)
		if err == nil {
			// File exists
			return nil
		}
		if !os.IsNotExist(err) {
			// An error other than "not exists" occurred
			return fmt.Errorf("error checking file %s: %w", path, err)
		}

		// Wait for the next polling interval or context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		case <-time.After(pollInterval):
			// Continue looping
		}
	}
}

func loadFlannelConfig(filename string) (FlannelConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForFileWithContext(ctx, filename)
	if err != nil {
		return FlannelConfig{}, fmt.Errorf("flannel env missing: %w", err)
	}
	file, err := os.Open(filename)
	if err != nil {
		return FlannelConfig{}, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var config FlannelConfig

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
			config.Network = value
		case "SUBNET":
			ip, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid CIDR format: %v", err)
			}
			network := ip.Mask(ipNet.Mask)
			subnet := fmt.Sprintf("%s/%d", network.String(), maskToPrefix(ipNet.Mask))
			config.Subnet = subnet
			config.Gateway = ip.String()
		case "MTU":
			mtu, err := strconv.Atoi(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid MTU value '%s': %w", value, err)
			}
			config.MTU = mtu
		case "IPMASQ":
			ipmasq, err := strconv.ParseBool(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid IPMASQ value '%s': %w", value, err)
			}
			config.IPMasq = ipmasq
		default:
			fmt.Printf("Unknown configuration key: %s\n", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return FlannelConfig{}, fmt.Errorf("error reading file: %w", err)
	}

	return config, nil
}

func maskToPrefix(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}
