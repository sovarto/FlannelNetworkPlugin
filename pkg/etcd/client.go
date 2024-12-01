package etcd

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"strings"
	"time"
)

type Client interface {
	NewConnection(withTimeout bool) (*Connection, error)
	GetKey(subKeys ...string) string
	CreateSubClient(subKeys ...string) Client
	GetEndpoints() []string
	Watch(key string, withPrefix bool, handler func(watcher clientv3.WatchChan, key string)) (clientv3.WatchChan, *Connection, error)
}

func (c *etcdClient) Watch(key string, withPrefix bool, handler func(watchChan clientv3.WatchChan, key string)) (clientv3.WatchChan, *Connection, error) {
	connection, err := c.NewConnection(false)

	if err != nil {
		return nil, nil, errors.WithMessage(err, "error connecting to etcd")
	}

	opts := []clientv3.OpOption{}
	if withPrefix {
		opts = append(opts, clientv3.WithPrefix())
	}
	watcher := connection.Client.Watch(connection.Ctx, key, opts...)
	go handler(watcher, key)

	return watcher, connection, nil
}

type etcdClient struct {
	dialTimeout time.Duration
	prefix      string
	endpoints   []string
}

func (c *etcdClient) NewConnection(withTimeout bool) (*Connection, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.endpoints,
		DialTimeout: c.dialTimeout,
	})

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return &Connection{Client: cli, Ctx: nil, Cancel: nil}, err
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if withTimeout {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	return &Connection{Client: cli, Ctx: ctx, Cancel: cancel}, nil
}

// GetKey subKey should not start with a slash
func (c *etcdClient) GetKey(subKeys ...string) string {
	if len(subKeys) == 0 {
		return c.prefix
	}
	return fmt.Sprintf("%s/%s", c.prefix, strings.Join(subKeys, "/"))
}

func (c *etcdClient) GetEndpoints() []string {
	return c.endpoints
}

func (c *etcdClient) CreateSubClient(subKeys ...string) Client {
	return NewEtcdClient(c.endpoints, c.dialTimeout, c.GetKey(subKeys...))
}

func NewEtcdClient(endpoints []string, dialTimeout time.Duration, prefix string) Client {
	return &etcdClient{
		endpoints:   endpoints,
		dialTimeout: dialTimeout,
		prefix:      strings.TrimRight(prefix, "/"),
	}
}