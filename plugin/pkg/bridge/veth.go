package bridge

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"log"
	"net"
	"strings"
)

const (
	vethPrefix = "veth"
	vethLen    = 8
)

type VethPair interface {
	GetOutside() string
	GetInside() string
	Delete() error
}

type vethPair struct {
	insideName  string
	outsideName string
}

func randomVethName() string {
	randomUuid, _ := uuid.NewRandom()

	return vethPrefix + strings.Replace(randomUuid.String(), "-", "", -1)[:vethLen]
}

func HydrateVethPair(insideName, outsideName string) VethPair {
	return &vethPair{
		insideName:  insideName,
		outsideName: outsideName,
	}
}

func (b *bridgeInterface) CreateAttachedVethPair(macAddress string) (VethPair, error) {
	vethInsideName := randomVethName()
	vethOutsideName := randomVethName()

	parsedMacAddress, err := net.ParseMAC(macAddress)

	if err != nil {
		return nil, err
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = vethInsideName
	linkAttrs.HardwareAddr = parsedMacAddress
	linkAttrs.MTU = b.network.MTU

	veth := &netlink.Veth{
		LinkAttrs: linkAttrs,
		PeerName:  vethOutsideName,
	}
	err = netlink.LinkAdd(veth)

	if err != nil {
		return nil, errors.WithMessagef(err, "failed to create veth pair %s / %s for bridge %s", vethOutsideName, vethInsideName, b.interfaceName)
	}

	err = b.attachInterfaceToBridge(vethOutsideName)
	if err != nil {
		err2 := netlink.LinkDel(veth)
		if err2 != nil {
			log.Printf("Failed to delete veth %s, after attaching it to bridge %s failed, err: %v", vethOutsideName, b.interfaceName, err2)
		}
		return nil, errors.WithMessagef(err, "failed to attach veth %s to bridge %s", vethOutsideName, b.interfaceName)
	}

	return &vethPair{
		insideName:  vethInsideName,
		outsideName: vethOutsideName,
	}, nil
}

func (b *bridgeInterface) attachInterfaceToBridge(interfaceName string) error {
	bridge, err := netlink.LinkByName(b.interfaceName)
	if err != nil {
		links, err2 := netlink.LinkList()
		if err2 != nil {
			return errors.WithMessagef(err, "failed to find bridge interface %s. Error when trying to get list of all interfaces: %v", b.interfaceName, err2)
		}
		return errors.WithMessagef(err, "failed to find bridge interface %s. Available interfaces: %s", b.interfaceName, spew.Sdump(links))
	}

	iface, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return errors.WithMessagef(err, "failed to find interface %s", interfaceName)
	}

	if err := netlink.LinkSetMaster(iface, bridge); err != nil {
		return errors.WithMessagef(err, "failed to set master of interface %s to bridge %s", interfaceName, b.interfaceName)
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return errors.WithMessagef(err, "failed to set interface %s as UP", interfaceName)
	}

	return nil
}

func (v *vethPair) GetOutside() string {
	return v.outsideName
}

func (v *vethPair) GetInside() string {
	return v.insideName
}

func (v *vethPair) Delete() error {
	iface, err := netlink.LinkByName(v.outsideName)
	if err != nil {
		return errors.WithMessagef(err, "failed to find interface %s", v.outsideName)
	}

	if err := netlink.LinkDel(iface); err != nil {
		return errors.WithMessagef(err, "failed to delete interface %s", v.outsideName)
	}

	return nil
}
