package etcd

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"strings"
	"sync"
)

type ShardItem[T any] struct {
	ShardKey string
	ID       string
	Value    T
}

type ShardItemChange[T any] struct {
	ShardKey string
	ID       string
	Previous T
	Current  T
}

type ShardItemsHandlers[T any] struct {
	OnChanged func(changes []ShardItemChange[T])
	OnAdded   func(added []ShardItem[T])
	OnRemoved func(removed []ShardItem[T])
}

// TODO: Make use of ReadOnlyStore and WriteOnlyStore internally for the shards, instead of re-implementing all that logic

// ShardedDistributedStore Notes:
// - Even though items are placed below the shard keys, itemIDs need to be unique across all shards
// - Because this performs a two-way sync, the handlers will be called even for items we add, update, delete or sync
// These handlers will be called synchronously from within the method, at the end, after all internal and etcd state
// have been properly brought into sync
type ShardedDistributedStore[T common.Equaler] interface {
	GetAll() map[string]map[string]T // shardKey -> itemID -> item
	GetLocalShardKey() string
	GetShard(key string) (shardItems map[string]T, shardExists bool) // itemID -> item
	GetItem(itemID string) (shardKey string, item T, itemExists bool)
	// AddOrUpdateItem Will always add to the local shard
	AddOrUpdateItem(itemID string, item T) error
	DeleteItem(itemID string) error
	Sync(localShardItems map[string]T) error
	Init(localShardItems map[string]T) error
}

type shardedDistributedStore[T common.Equaler] struct {
	client         Client
	localShardKey  string
	handlers       ShardItemsHandlers[T]
	shardedData    *common.ConcurrentMap[string, *common.ConcurrentMap[string, T]] // shardKey -> itemID -> item
	data           *common.ConcurrentMap[string, T]                                // itemID -> item
	itemToShardKey *common.ConcurrentMap[string, string]                           // itemID -> shardKey
	sync.Mutex
}

func NewShardedDistributedStore[T common.Equaler](client Client, localShardKey string, handlers ShardItemsHandlers[T]) ShardedDistributedStore[T] {
	shardedData := common.NewConcurrentMap[string, *common.ConcurrentMap[string, T]]()
	shardedData.Set(localShardKey, common.NewConcurrentMap[string, T]())
	return &shardedDistributedStore[T]{
		client:         client,
		localShardKey:  localShardKey,
		handlers:       handlers,
		shardedData:    shardedData,
		data:           common.NewConcurrentMap[string, T](),
		itemToShardKey: common.NewConcurrentMap[string, string](),
	}
}

func (s *shardedDistributedStore[T]) Init(localShardItems map[string]T) error {
	err := s.sync(localShardItems, false)
	if err != nil {
		return err
	}

	_, _, err = s.client.Watch(s.client.GetKey(), true, s.handleWatchEvents)
	if err != nil {
		return errors.WithMessagef(err, "Couldn't start watcher for %s", s.client.GetKey())
	}

	return nil
}

func (s *shardedDistributedStore[T]) GetAll() map[string]map[string]T {
	result := make(map[string]map[string]T)
	for _, key := range s.shardedData.Keys() {
		result[key] = make(map[string]T)
		item, _ := s.shardedData.Get(key)
		for _, subKey := range item.Keys() {
			subItem, _ := item.Get(subKey)
			result[key][subKey] = subItem
		}
	}
	return result
}

func (s *shardedDistributedStore[T]) GetLocalShardKey() string { return s.localShardKey }

func (s *shardedDistributedStore[T]) GetShard(key string) (shardItems map[string]T, shardExists bool) {
	item, exists := s.shardedData.Get(key)
	if !exists {
		return nil, false
	}
	result := make(map[string]T)
	for _, subKey := range item.Keys() {
		subItem, _ := item.Get(subKey)
		result[subKey] = subItem
	}
	return result, true
}

func (s *shardedDistributedStore[T]) GetItem(itemID string) (shardKey string, item T, itemExists bool) {
	s.Lock()
	defer s.Unlock()

	item, itemExists = s.data.Get(itemID)
	shardKey, _ = s.itemToShardKey.Get(itemID)
	return
}

func (s *shardedDistributedStore[T]) AddOrUpdateItem(itemID string, item T) error {
	s.Lock()
	defer s.Unlock()

	shardKey := s.localShardKey
	shardItems, _ := s.shardedData.Get(shardKey)

	previousItem, exists := shardItems.Get(itemID)

	shardItems.Set(itemID, item)
	s.data.Set(itemID, item)
	s.itemToShardKey.Set(itemID, shardKey)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		_, err := s.storeItem(connection, shardKey, itemID, item)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to store item %s for shard %s", itemID, shardKey)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	fmt.Printf("AddOrUpdateItem %s %v", itemID, exists)
	if exists {
		if !previousItem.Equals(item) {
			fmt.Printf("AddOrUpdateItem: not equal", itemID, exists)
			if s.handlers.OnChanged != nil {
				s.handlers.OnChanged([]ShardItemChange[T]{{ShardKey: shardKey, ID: itemID, Previous: previousItem, Current: item}})
			}
		}
	} else {
		if s.handlers.OnAdded != nil {
			s.handlers.OnAdded([]ShardItem[T]{{ShardKey: shardKey, ID: itemID, Value: item}})
		}
	}

	return nil
}

func (s *shardedDistributedStore[T]) DeleteItem(itemID string) error {
	s.Lock()
	defer s.Unlock()

	shardKey := s.localShardKey
	shardItems, _ := s.shardedData.Get(shardKey)

	previousItem, exists := shardItems.TryRemove(itemID)
	s.data.Remove(itemID)
	s.itemToShardKey.Remove(itemID)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		key := s.client.GetKey(shardKey, itemID)
		_, err := connection.Client.Delete(connection.Ctx, key)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to delete item %s", key)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	if exists {
		if s.handlers.OnRemoved != nil {
			s.handlers.OnRemoved([]ShardItem[T]{{ID: itemID, Value: previousItem, ShardKey: shardKey}})
		}
	}

	return nil
}

func (s *shardedDistributedStore[T]) Sync(localShardItems map[string]T) error {
	return s.sync(localShardItems, true)
}

func (s *shardedDistributedStore[T]) sync(localShardItems map[string]T, callHandlers bool) error {
	s.Lock()
	defer s.Unlock()

	changes, added, removed := s.syncToInternalState(s.localShardKey, localShardItems)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		etcdData, err := s.loadData(connection)
		if err != nil {
			return struct{}{}, err
		}

		for shardKey, shardItems := range etcdData {
			if shardKey != s.localShardKey {
				localChanges, localAdded, localRemoved := s.syncToInternalState(shardKey, shardItems)
				changes = append(changes, localChanges...)
				added = append(added, localAdded...)
				removed = append(removed, localRemoved...)
			} else {
				toBeDeletedFromEtcd, _ := lo.Difference(lo.Keys(shardItems), lo.Keys(localShardItems))
				for id, item := range localShardItems {
					_, err = s.storeItem(connection, shardKey, id, item)
					if err != nil {
						return struct{}{}, errors.WithMessagef(err, "failed to store item %s for shard %s", id, shardKey)
					}
				}
				for _, id := range toBeDeletedFromEtcd {
					key := s.client.GetKey(shardKey, id)
					_, err = connection.Client.Delete(connection.Ctx, key)
					if err != nil {
						return struct{}{}, errors.WithMessagef(err, "failed to delete item %s", key)
					}
				}
			}
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	if callHandlers {
		if len(added) > 0 && s.handlers.OnAdded != nil {
			s.handlers.OnAdded(added)
		}
		if len(changes) > 0 && s.handlers.OnChanged != nil {
			s.handlers.OnChanged(changes)
		}
		if len(removed) > 0 && s.handlers.OnRemoved != nil {
			s.handlers.OnRemoved(removed)
		}
	}

	return nil
}

func (s *shardedDistributedStore[T]) syncToInternalState(shardKey string, truth map[string]T) (changes []ShardItemChange[T], added []ShardItem[T], removed []ShardItem[T]) {
	changes = []ShardItemChange[T]{}
	added = []ShardItem[T]{}
	removed = []ShardItem[T]{}

	shardItems, _, _ := s.shardedData.GetOrAdd(shardKey, func() (*common.ConcurrentMap[string, T], error) {
		return common.NewConcurrentMap[string, T](), nil
	})

	for id, item := range truth {
		previousItem, exists := shardItems.Get(id)
		if exists {
			if !previousItem.Equals(item) {
				changes = append(changes, ShardItemChange[T]{
					ID:       id,
					ShardKey: shardKey,
					Previous: previousItem,
					Current:  item,
				})
			}
		} else {
			added = append(added, ShardItem[T]{
				ShardKey: shardKey,
				ID:       id,
				Value:    item,
			})
		}
	}

	toBeDeletedFromInternalState, _ := lo.Difference(shardItems.Keys(), lo.Keys(truth))
	for _, id := range toBeDeletedFromInternalState {
		previousItem, _ := shardItems.Get(id)
		removed = append(removed, ShardItem[T]{ID: id, Value: previousItem, ShardKey: shardKey})
	}

	clonedTruth := common.NewConcurrentMap[string, T]()
	for key, value := range truth {
		clonedTruth.Set(key, value)
	}
	s.shardedData.Set(shardKey, clonedTruth)
	s.data = common.NewConcurrentMap[string, T]()
	for _, shardKey := range s.shardedData.Keys() {
		shardItems, _ := s.shardedData.Get(shardKey)
		for _, id := range shardItems.Keys() {
			item, _ := shardItems.Get(id)
			s.data.Set(id, item)
			s.itemToShardKey.Set(id, shardKey)
		}
	}

	return changes, added, removed
}

func (s *shardedDistributedStore[T]) loadData(connection *Connection) (map[string]map[string]T, error) {
	prefix := s.client.GetKey()
	resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, errors.WithMessagef(err, "Error getting data from etcd for prefix %s", prefix)
	}

	result := map[string]map[string]T{}

	for _, kv := range resp.Kvs {
		shardKey, itemID, ignored := parseItemKey(kv, prefix)
		if ignored {
			continue
		}

		item, err := s.parseItem(kv)
		if err != nil {
			return nil, err
		}

		setShardedItem(result, shardKey, itemID, item)
	}

	return result, nil
}

func (s *shardedDistributedStore[T]) storeItem(connection *Connection, shardKey, itemID string, item T) (wasWritten bool, err error) {
	bytes, err := json.Marshal(item)
	if err != nil {
		return false, errors.WithMessagef(err, "Failed to serialize item %s for shard %s. value: %+v", itemID, shardKey, item)
	}
	wasWritten, err = connection.PutIfNewOrChanged(s.client.GetKey(shardKey, itemID), string(bytes))
	return
}

func parseItemKey(kv *mvccpb.KeyValue, prefix string) (shardKey, itemID string, ignored bool) {
	key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		fmt.Printf("Found unexpected key %s under prefix %s. Ignoring...\n", key, prefix)
		ignored = true
		return
	}
	shardKey = parts[0]
	itemID = parts[1]
	ignored = false
	return
}

func (s *shardedDistributedStore[T]) parseItem(kv *mvccpb.KeyValue) (item T, err error) {
	err = json.Unmarshal(kv.Value, &item)
	if err != nil {
		err = errors.WithMessagef(err, "error parsing item %s, value: %+v, err: %+v", string(kv.Key), string(kv.Value), err)
		return
	}

	return
}

func (s *shardedDistributedStore[T]) handleWatchEvents(watcher clientv3.WatchChan, prefix string) {
	for wresp := range watcher {
		for _, ev := range wresp.Events {
			shardKey, itemID, ignored := parseItemKey(ev.Kv, prefix)
			if ignored {
				fmt.Printf("Ignored watch event for key %s\n", string(ev.Kv.Key))
				continue
			}
			if shardKey == s.localShardKey {
				// fmt.Printf("Ignored watch event for item %s, because it is from the local shard\n", itemID)
				// Ignore for now
				// TODO: Check if item change matches our internal state
				// - if yes: ignore
				// - if not: print error message as this shouldn't happen
			} else {
				if ev.Type == mvccpb.DELETE {
					s.Lock()
					shardedItems, exists := s.shardedData.Get(shardKey)
					if exists {
						shardedItems.Remove(itemID)
					}
					item, exists := s.data.TryRemove(itemID)
					s.itemToShardKey.Remove(itemID)
					s.Unlock()
					if exists && s.handlers.OnRemoved != nil {
						s.handlers.OnRemoved([]ShardItem[T]{{ID: itemID, Value: item, ShardKey: shardKey}})
					} else {
						fmt.Printf("Handlers not called for item %s from shard %s. Item: %+v\n", itemID, shardKey, item)
					}
				} else if ev.Type == mvccpb.PUT {
					item, err := s.parseItem(ev.Kv)
					if err != nil {
						log.Printf("Error in event watcher: %+v", err)
						continue
					}

					s.Lock()
					shardedItems, shardExists := s.shardedData.Get(shardKey)
					if !shardExists {
						shardedItems = common.NewConcurrentMap[string, T]()
						s.shardedData.Set(shardKey, shardedItems)
					}
					previousItem, exists := shardedItems.Get(itemID)
					shardedItems.Set(itemID, item)
					s.data.Set(itemID, item)
					s.Unlock()
					if exists {
						if s.handlers.OnChanged != nil && !previousItem.Equals(item) {
							s.handlers.OnChanged([]ShardItemChange[T]{{
								ShardKey: shardKey,
								ID:       itemID,
								Previous: previousItem,
								Current:  item,
							}})
						}
					} else {
						if s.handlers.OnAdded != nil {
							s.handlers.OnAdded([]ShardItem[T]{{ShardKey: shardKey, ID: itemID, Value: item}})
						}
					}
				}
			}
		}
	}
}

func setShardedItem[T any](dst map[string]map[string]T, shardKey, itemID string, item T) {
	shard, exists := dst[shardKey]
	if !exists {
		shard = map[string]T{}
		dst[shardKey] = shard
	}
	shard[itemID] = item
}
