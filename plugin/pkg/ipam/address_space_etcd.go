package ipam

import (
	"fmt"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
	"strings"
)

func subnetsKey(e etcd.Client) string {
	return e.GetKey()
}

func subnetKey(e etcd.Client, subnet string) string {
	return e.GetKey(common.SubnetToKey(subnet))
}

func getUsedSubnets(client etcd.Client) (*common.ConcurrentMap[string, net.IPNet], error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (*common.ConcurrentMap[string, net.IPNet], error) {
		prefix := subnetsKey(client)
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}

		result := common.NewConcurrentMap[string, net.IPNet]()
		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			if strings.Contains(key, "/") {
				continue
			}
			networkID := string(kv.Value)
			_, ipNet, err := net.ParseCIDR(strings.ReplaceAll(key, "-", "/"))

			if err != nil {
				fmt.Printf("couldn't parse %s as CIDR for network %s. Skipping...\n", key, networkID)
				continue
			}

			result.Set(networkID, *ipNet)
		}

		return result, nil
	})
}

type PoolSubnetLeaseResult struct {
	Success bool
	PoolID  string
}

func reservePoolSubnet(client etcd.Client, subnet, id string) (PoolSubnetLeaseResult, error) {
	return etcd.WithConnection(client, func(conn *etcd.Connection) (PoolSubnetLeaseResult, error) {
		key := subnetKey(client, subnet)
		resp, err := conn.Client.Txn(conn.Ctx).
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, id)).
			Else(clientv3.OpGet(key)).
			Commit()

		if err != nil {
			return PoolSubnetLeaseResult{Success: false}, err
		}

		if resp.Succeeded {
			return PoolSubnetLeaseResult{Success: true}, nil
		}

		poolID := string(resp.Responses[0].GetResponseRange().Kvs[0].Value)
		if poolID == id {
			return PoolSubnetLeaseResult{Success: true}, nil
		}

		return PoolSubnetLeaseResult{Success: false, PoolID: poolID}, nil
	})
}

func releasePoolSubnet(client etcd.Client, subnet, id string) (PoolSubnetLeaseResult, error) {
	return etcd.WithConnection(client, func(conn *etcd.Connection) (PoolSubnetLeaseResult, error) {
		key := subnetKey(client, subnet)
		resp, err := conn.Client.Txn(conn.Ctx).
			If(clientv3.Compare(clientv3.Value(key), "=", id)).
			Then(clientv3.OpDelete(key)).
			Else(clientv3.OpGet(key)).
			Commit()

		if err != nil {
			return PoolSubnetLeaseResult{Success: false}, err
		}

		if resp.Succeeded {
			return PoolSubnetLeaseResult{Success: true}, nil
		}

		getResponse := resp.Responses[0].GetResponseRange()
		if getResponse.Kvs == nil || len(getResponse.Kvs) == 0 {
			// It probably has already been deleted by another node
			return PoolSubnetLeaseResult{Success: true}, nil
		}

		return PoolSubnetLeaseResult{Success: false, PoolID: string(getResponse.Kvs[0].Value)}, nil
	})
}
