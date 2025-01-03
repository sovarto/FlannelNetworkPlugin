package common

import (
	"net"
	"strings"
)

type FlannelNetworkID string
type DockerNetworkID string

type FlannelNetworkInfo struct {
	FlannelID    string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

type NetworkInfo struct {
	DockerID  string `json:"DockerID"`
	FlannelID string `json:"FlannelID"`
	Name      string `json:"Name"`
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

func (c NetworkInfo) Equals(other Equaler) bool {
	o, ok := other.(NetworkInfo)
	if !ok {
		return false
	}
	if c.FlannelID != o.FlannelID || c.Name != o.Name {
		return false
	}

	return true
}

func AddOrUpdate[T any](store map[string]T, id string, valueToAdd T, update func(existing *T)) {
	existing, exists := store[id]
	if exists {
		if update != nil {
			update(&existing)
		}
	} else {
		existing = valueToAdd
	}
	store[id] = existing
}
