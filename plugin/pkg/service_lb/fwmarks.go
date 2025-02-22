package service_lb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"hash/crc32"
	"log"
	"strconv"
	"strings"
	"sync"
)

type FwmarksManagement interface {
	Get(serviceID, networkID string) (uint32, error)
	Release(serviceID, networkID string, fwmark uint32) error
}

type fwmarks struct {
	etcdClient etcd.Client
	sync.Mutex
}

func (f *fwmarks) fwmarksListKey(networkID string) string {
	return f.etcdClient.GetKey(networkID, "list")
}

func (f *fwmarks) fwmarkKey(networkID, fwmark string) string {
	return fmt.Sprintf("%s/%s", f.fwmarksListKey(networkID), fwmark)
}

func (f *fwmarks) fwmarkServicesKey(networkID string) string {
	return f.etcdClient.GetKey(networkID, "by-service")
}

func (f *fwmarks) fwmarkServiceKey(networkID, serviceID string) string {
	return fmt.Sprintf("%s/%s", f.fwmarkServicesKey(networkID), serviceID)
}

func NewFwmarksManagement(etcdClient etcd.Client) FwmarksManagement {
	return &fwmarks{
		etcdClient: etcdClient,
	}
}

func (f *fwmarks) Get(serviceID, networkID string) (uint32, error) {
	f.Lock()
	defer f.Unlock()

	return etcd.WithConnection(f.etcdClient, func(connection *etcd.Connection) (uint32, error) {
		serviceKey := f.fwmarkServiceKey(networkID, serviceID)
		resp, err := connection.Client.Get(connection.Ctx, serviceKey)
		if err != nil {
			return 0, err
		}

		if len(resp.Kvs) > 0 {
			existingFwmark := string(resp.Kvs[0].Value)
			parsedFwmark, err := strconv.ParseUint(existingFwmark, 10, 32)
			if err != nil {
				log.Printf("Failed to parse existing fwmark %s, discarding: %v", existingFwmark, err)
			} else {
				return uint32(parsedFwmark), nil
			}
		}

		listKey := f.fwmarksListKey(networkID)

		for {
			resp, err = connection.Client.Get(connection.Ctx, listKey, clientv3.WithPrefix())
			if err != nil {
				return 0, err
			}

			existingFwmarks := []uint32{}

			for _, kv := range resp.Kvs {
				key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), listKey), "/")
				existingFwmark := key
				parsedFwmark, err := strconv.ParseUint(existingFwmark, 10, 32)
				if err != nil {
					log.Printf("Failed to parse existing fwmark %s, skipping: %v", existingFwmark, err)
				} else {
					existingFwmarks = append(existingFwmarks, uint32(parsedFwmark))
				}
			}

			// TODO: move everything into namespace so that only our fwmarks exist
			fwmark, err := GenerateFWMARK(serviceID, networkID, existingFwmarks)
			if err != nil {
				return 0, err
			}

			fwmarkStr := strconv.FormatUint(uint64(fwmark), 10)
			fwmarkKey := f.fwmarkKey(networkID, fwmarkStr)

			txn := connection.Client.Txn(connection.Ctx).
				If(clientv3.Compare(clientv3.CreateRevision(fwmarkKey), "=", 0)).
				Then(
					clientv3.OpPut(serviceKey, fwmarkStr),
					clientv3.OpPut(fwmarkKey, serviceID),
				)

			txnResp, err := txn.Commit()
			if err != nil {
				return 0, fmt.Errorf("etcd transaction failed: %v", err)
			}

			if !txnResp.Succeeded {
				// this fwmark has since been registered. This shouldn't happen, but let's just try the next
				continue
			}

			fmt.Printf("Created new fwmark %d for service %s\n", fwmark, serviceID)
			return fwmark, nil
		}

	})
}

func (f *fwmarks) Release(serviceID, networkID string, fwmark uint32) error {
	f.Lock()
	defer f.Unlock()
	_, err := etcd.WithConnection(f.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		fwmarkStr := strconv.FormatUint(uint64(fwmark), 10)
		serviceKey := f.fwmarkServiceKey(networkID, serviceID)
		fwmarkKey := f.fwmarkKey(networkID, fwmarkStr)
		txn := connection.Client.Txn(connection.Ctx).
			If(
				clientv3.Compare(clientv3.Value(serviceKey), "=", fwmarkStr),
				clientv3.Compare(clientv3.Value(fwmarkKey), "=", serviceID),
			).
			Then(
				clientv3.OpDelete(serviceKey),
				clientv3.OpDelete(fwmarkKey),
			)

		resp, err := txn.Commit()
		if err != nil {
			return struct{}{}, fmt.Errorf("etcd transaction failed: %v", err)
		}

		if !resp.Succeeded {
			return struct{}{}, errors.WithMessagef(err, "failed to release fwmark %d for service %s", fwmark, serviceID)
		}

		return struct{}{}, nil
	})

	return err
}

// GenerateFWMARK generates a unique FWMARK based on the serviceID and the networkID.
// It checks against existingFWMARKs and appends a random suffix to the serviceID-networkID combination
// if a collision is detected. It returns the unique FWMARK, the possibly modified
// serviceID, and an error if a unique FWMARK cannot be found within the maximum attempts.
func GenerateFWMARK(serviceID, networkID string, existingFWMARKs []uint32) (uint32, error) {
	const maxAttempts = 1000
	const suffixLength = 4 // Number of random bytes to append

	// Convert existingFWMARKs slice to a map for efficient lookup
	fwmarkMap := make(map[uint32]struct{}, len(existingFWMARKs))
	for _, mark := range existingFWMARKs {
		fwmarkMap[mark] = struct{}{}
	}

	currentName := fmt.Sprintf("%s-%s", serviceID, networkID)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Generate FWMARK using CRC32 checksum
		fwmark := crc32.ChecksumIEEE([]byte(currentName))

		// Check for collision
		if _, exists := fwmarkMap[fwmark]; !exists {
			// Unique FWMARK found
			return fwmark, nil
		}

		// Collision detected, prepare to modify the serviceID with a random suffix
		suffix, err := generateRandomSuffix(suffixLength)
		if err != nil {
			return 0, fmt.Errorf("failed to generate random suffix: %v", err)
		}

		// Append the random suffix to the original serviceID
		currentName = fmt.Sprintf("%s_%s", serviceID, suffix)
	}

	return 0, errors.New("unable to generate a unique FWMARK after maximum attempts")
}

// generateRandomSuffix creates a random hexadecimal string of length `length` bytes.
func generateRandomSuffix(length int) (string, error) {
	bytes := make([]byte, length)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func cleanUpStaleFwmarks(etcdClient etcd.Client, existingServices []string) ([]uint32, error) {
	return etcd.WithConnection(etcdClient, func(connection *etcd.Connection) ([]uint32, error) {
		fwmarks := map[string]uint32{}
		prefix := etcdClient.GetKey()
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			return nil, errors.WithMessage(err, "failed to get fwmarks data from etcd")
		}

		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			keyParts := strings.Split(key, "/")
			if len(keyParts) == 3 {
				var serviceID string
				var fwmarkStr string
				if keyParts[1] == "by-service" {
					serviceID = keyParts[2]
					fwmarkStr = string(kv.Value)
				} else {
					serviceID = string(kv.Value)
					fwmarkStr = keyParts[2]
				}

				if lo.Some(existingServices, []string{serviceID}) {
					continue
				}

				parsedFwmark, err := strconv.ParseUint(fwmarkStr, 10, 32)
				if err != nil {
					log.Printf("Failed to parse existing fwmark %s, skipping: %v", string(kv.Key), err)
					continue
				}
				fwmarks[serviceID] = uint32(parsedFwmark)

				fmt.Printf("Deleting fwmark data at %s for non-existing service %s\n", string(kv.Key), serviceID)
				_, err = connection.Client.Delete(connection.Ctx, string(kv.Key))
				if err != nil {
					log.Printf("Failed to delete stale fwmark data at %s: %v", string(kv.Key), err)
				}
			} else {
				fmt.Printf("Ignoring unknown key %s\n", string(kv.Key))
			}
		}

		return maps.Values(fwmarks), nil
	})
}
