package common

import (
	"net"
	"strings"
)

type ContainerInfo struct {
	ID          string            `json:"ContainerID"`
	Name        string            `json:"ContainerName"`
	ServiceID   string            `json:"ServiceID"`
	ServiceName string            `json:"ServiceName"`
	IPs         map[string]net.IP `json:"IPs"`     // networkID -> IP
	IpamIPs     map[string]net.IP `json:"IpamIPs"` // networkID -> IP
}

type ServiceInfo struct {
	ID       string            `json:"ServiceID"`
	Name     string            `json:"ServiceName"`
	IpamVIPs map[string]net.IP `json:"IpamVIPs"` // networkID -> VIP
}

type NetworkInfo struct {
	FlannelID    string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

func SubnetToKey(subnet string) string {
	return strings.ReplaceAll(subnet, "/", "-")
}
func GetPtrFromMap[K comparable, V any](m map[K]V, key K) *V {
	if val, ok := m[key]; ok {
		return &val
	}
	return nil
}

type Equaler interface {
	Equals(other Equaler) bool
}

func (c ContainerInfo) Equals(other Equaler) bool {
	// Type assertion to *ContainerInfo
	o, ok := other.(ContainerInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name || c.ServiceID != o.ServiceID || c.ServiceName != o.ServiceName {
		return false
	}
	if !CompareIPMaps(c.IPs, o.IPs) {
		return false
	}
	if !CompareIPMaps(c.IpamIPs, o.IpamIPs) {
		return false
	}

	return true
}

func (c ServiceInfo) Equals(other Equaler) bool {
	// Type assertion to *ContainerInfo
	o, ok := other.(ServiceInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name {
		return false
	}
	if !CompareIPMaps(c.IpamVIPs, o.IpamVIPs) {
		return false
	}

	return true
}

func CompareIPMaps(a, b map[string]net.IP) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists {
			return false
		}
		if !valA.Equal(valB) {
			return false
		}
	}

	return true
}

type Ordered interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64 | ~string
}

// Generic Max function
func Max[T Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}
