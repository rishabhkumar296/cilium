// Copyright 2016-2019 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/common/addressing"
	"github.com/cilium/cilium/pkg/client"
	"github.com/cilium/cilium/pkg/datapath/route"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/endpoint/connector"
	endpointid "github.com/cilium/cilium/pkg/endpoint/id"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/uuid"
	"github.com/cilium/cilium/pkg/version"

	"github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	cniTypesVer "github.com/containernetworking/cni/pkg/types/current"
	cniVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"golang.org/x/sys/unix"
)

var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "cilium-cni")
)

func init() {
	logging.SetLogLevel(logrus.DebugLevel)
	runtime.LockOSThread()
}

type CmdState struct {
	Endpoint  *models.EndpointChangeRequest
	IP6       addressing.CiliumIPv6
	IP6routes []route.Route
	IP4       addressing.CiliumIPv4
	IP4routes []route.Route
	Client    *client.Client
	HostAddr  *models.NodeAddressing
}

type netConf struct {
	cniTypes.NetConf
	MTU  int  `json:"mtu"`
	Args Args `json:"args"`
}

type cniArgsSpec struct {
	cniTypes.CommonArgs
	IP                         net.IP
	K8S_POD_NAME               cniTypes.UnmarshallableString
	K8S_POD_NAMESPACE          cniTypes.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID cniTypes.UnmarshallableString
}

// Args contains arbitrary information a scheduler
// can pass to the cni plugin
type Args struct {
	Mesos Mesos `json:"org.apache.mesos,omitempty"`
}

// Mesos contains network-specific information from the scheduler to the cni plugin
type Mesos struct {
	NetworkInfo NetworkInfo `json:"network_info"`
}

// NetworkInfo supports passing only labels from mesos
type NetworkInfo struct {
	Name   string `json:"name"`
	Labels struct {
		Labels []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"labels,omitempty"`
	} `json:"labels,omitempty"`
}

func main() {
	skel.PluginMain(cmdAdd,
		nil,
		cmdDel,
		cniVersion.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1"),
		"Cilium CNI plugin "+version.Version)
}

func ipv6IsEnabled(ipam *models.IPAMResponse) bool {
	if ipam == nil || ipam.Address.IPV6 == "" {
		return false
	}

	if ipam.HostAddressing != nil {
		return ipam.HostAddressing.IPV6.Enabled
	}

	return true
}

func ipv4IsEnabled(ipam *models.IPAMResponse) bool {
	if ipam == nil || ipam.Address.IPV4 == "" {
		return false
	}

	if ipam.HostAddressing != nil {
		return ipam.HostAddressing.IPV4.Enabled
	}

	return true
}

func loadNetConf(bytes []byte) (*netConf, string, error) {
	n := &netConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %s", err)
	}
	return n, n.CNIVersion, nil
}

func removeIfFromNSIfExists(netNs ns.NetNS, ifName string) error {
	return netNs.Do(func(_ ns.NetNS) error {
		l, err := netlink.LinkByName(ifName)
		if err != nil {
			if strings.Contains(err.Error(), "Link not found") {
				return nil
			}
			return err
		}
		return netlink.LinkDel(l)
	})
}

func releaseIP(client *client.Client, ip string) {
	if ip != "" {
		if err := client.IPAMReleaseIP(ip); err != nil {
			log.WithError(err).WithField(logfields.IPAddr, ip).Warn("Unable to release IP")
		}
	}
}

func releaseIPs(client *client.Client, addr *models.AddressPair) {
	releaseIP(client, addr.IPV6)
	releaseIP(client, addr.IPV4)
}

func addIPConfigToLink(ip addressing.CiliumIP, routes []route.Route, link netlink.Link, ifName string) error {
	log.WithFields(logrus.Fields{
		logfields.IPAddr:    ip,
		"netLink":           logfields.Repr(link),
		logfields.Interface: ifName,
	}).Debug("Configuring link")

	addr := &netlink.Addr{IPNet: ip.EndpointPrefix()}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add addr to %q: %v", ifName, err)
	}

	// ipvlan needs to be UP before we add routes, and can only be UPed after
	// we added an IP address.
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set %q UP: %v", ifName, err)
	}

	// Sort provided routes to make sure we apply any more specific
	// routes first which may be used as nexthops in wider routes
	sort.Sort(route.ByMask(routes))

	for _, r := range routes {
		log.WithField("route", logfields.Repr(r)).Debug("Adding route")
		rt := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       &r.Prefix,
			MTU:       r.MTU,
		}

		if r.Nexthop == nil {
			rt.Scope = netlink.SCOPE_LINK
		} else {
			rt.Gw = *r.Nexthop
		}

		if err := netlink.RouteAdd(rt); err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("failed to add route '%s via %v dev %v': %v",
					r.Prefix.String(), r.Nexthop, ifName, err)
			}
		}
	}

	return nil
}

func configureIface(ipam *models.IPAMResponse, ifName string, state *CmdState) (string, error) {
	l, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", fmt.Errorf("failed to lookup %q: %v", ifName, err)
	}

	if err := netlink.LinkSetUp(l); err != nil {
		return "", fmt.Errorf("failed to set %q UP: %v", ifName, err)
	}

	if ipv4IsEnabled(ipam) {
		if err := addIPConfigToLink(state.IP4, state.IP4routes, l, ifName); err != nil {
			return "", fmt.Errorf("error configuring IPv4: %s", err.Error())
		}
	}

	if ipv6IsEnabled(ipam) {
		if err := addIPConfigToLink(state.IP6, state.IP6routes, l, ifName); err != nil {
			return "", fmt.Errorf("error configuring IPv6: %s", err.Error())
		}
	}

	if err := netlink.LinkSetUp(l); err != nil {
		return "", fmt.Errorf("failed to set %q UP: %v", ifName, err)
	}

	if l.Attrs() != nil {
		return l.Attrs().HardwareAddr.String(), nil
	}

	return "", nil
}

func newCNIRoute(r route.Route) *cniTypes.Route {
	rt := &cniTypes.Route{
		Dst: r.Prefix,
	}
	if r.Nexthop != nil {
		rt.GW = *r.Nexthop
	}

	return rt
}

func prepareIP(ipAddr string, isIPv6 bool, state *CmdState, mtu int) (*cniTypesVer.IPConfig, []*cniTypes.Route, error) {
	var (
		routes    []route.Route
		err       error
		gw        string
		ipVersion string
		ip        addressing.CiliumIP
	)

	if isIPv6 {
		if state.IP6, err = addressing.NewCiliumIPv6(ipAddr); err != nil {
			return nil, nil, err
		}
		if state.IP6routes, err = connector.IPv6Routes(state.HostAddr, mtu); err != nil {
			return nil, nil, err
		}
		routes = state.IP6routes
		ip = state.IP6
		gw = connector.IPv6Gateway(state.HostAddr)
		ipVersion = "6"
	} else {
		if state.IP4, err = addressing.NewCiliumIPv4(ipAddr); err != nil {
			return nil, nil, err
		}
		if state.IP4routes, err = connector.IPv4Routes(state.HostAddr, mtu); err != nil {
			return nil, nil, err
		}
		routes = state.IP4routes
		ip = state.IP4
		gw = connector.IPv4Gateway(state.HostAddr)
		ipVersion = "4"
	}

	rt := []*cniTypes.Route{}
	for _, r := range routes {
		rt = append(rt, newCNIRoute(r))
	}

	gwIP := net.ParseIP(gw)
	if gwIP == nil {
		return nil, nil, fmt.Errorf("Invalid gateway address: %s", gw)
	}

	return &cniTypesVer.IPConfig{
		Address: *ip.EndpointPrefix(),
		Gateway: gwIP,
		Version: ipVersion,
	}, rt, nil
}

func cmdAdd(args *skel.CmdArgs) (err error) {
	logger := log.WithField("eventUUID", uuid.NewUUID())
	logger.WithField("args", args).Debug("Processing CNI ADD request")

	n, cniVer, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	cniArgs := cniArgsSpec{}
	if err := cniTypes.LoadArgs(args.Args, &cniArgs); err != nil {
		return fmt.Errorf("unable to extract CNI arguments: %s", err)
	}

	c, err := client.NewDefaultClientWithTimeout(defaults.ClientConnectTimeout)
	if err != nil {
		return fmt.Errorf("unable to connect to Cilium daemon: %s", err)
	}

	netNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %s", args.Netns, err)
	}
	defer netNs.Close()

	if len(n.NetConf.RawPrevResult) != 0 {
		err := cniVersion.ParsePrevResult(&n.NetConf)
		if err != nil {
			return fmt.Errorf("unable to understand network config: %s", err)
		}
		str := strings.Split(args.Netns, "/")
		pid, err := strconv.Atoi(str[2])
		if err != nil {
			return err
		}
		// TODO: detect host device from flannel result
		ep, err := connector.DeriveEndpointFrom("cni0", args.ContainerID, pid)
		if err != nil {
			logger.WithError(err).WithFields(logrus.Fields{
				logfields.ContainerID: args.ContainerID}).Warn("Unable to derive endpoint")
			return fmt.Errorf("unable to derive endpoint: %s", err)
		}
		err = c.EndpointCreate(ep)
		if err != nil {
			logger.WithError(err).WithFields(logrus.Fields{
				logfields.ContainerID: ep.ContainerID}).Warn("Unable to create endpoint")
			return fmt.Errorf("Unable to create endpoint: %s", err)
		}

		logger.WithFields(logrus.Fields{
			logfields.ContainerID: ep.ContainerID}).Debug("Endpoint successfully created")
		return cniTypes.PrintResult(&cniTypesVer.Result{}, cniVer)
	}

	if err := removeIfFromNSIfExists(netNs, args.IfName); err != nil {
		return fmt.Errorf("failed removing interface %q from namespace %q: %s",
			args.IfName, args.Netns, err)
	}

	addLabels := models.Labels{}

	for _, label := range n.Args.Mesos.NetworkInfo.Labels.Labels {
		addLabels = append(addLabels, fmt.Sprintf("%s:%s=%s", labels.LabelSourceMesos, label.Key, label.Value))
	}

	configResult, err := c.ConfigGet()
	if err != nil {
		return fmt.Errorf("unable to retrieve configuration from cilium-agent: %s", err)
	}

	if configResult == nil || configResult.Status == nil {
		return fmt.Errorf("did not receive configuration from cilium-agent")
	}

	conf := *configResult.Status

	ep := &models.EndpointChangeRequest{
		ContainerID:  args.ContainerID,
		Labels:       addLabels,
		State:        models.EndpointStateWaitingForIdentity,
		Addressing:   &models.AddressPair{},
		K8sPodName:   string(cniArgs.K8S_POD_NAME),
		K8sNamespace: string(cniArgs.K8S_POD_NAMESPACE),
	}

	switch conf.DatapathMode {
	case option.DatapathModeVeth:
		veth, peer, tmpIfName, err := connector.SetupVeth(ep.ContainerID, int(conf.DeviceMTU), ep)
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
				if err = netlink.LinkDel(veth); err != nil {
					logger.WithError(err).WithField(logfields.Veth, veth.Name).Warn("failed to clean up and delete veth")
				}
			}
		}()

		if err = netlink.LinkSetNsFd(*peer, int(netNs.Fd())); err != nil {
			return fmt.Errorf("unable to move veth pair '%v' to netns: %s", peer, err)
		}

		_, _, err = connector.SetupVethRemoteNs(netNs, tmpIfName, args.IfName)
		if err != nil {
			return err
		}
	case option.DatapathModeIpvlan:
		var mode netlink.IPVlanMode

		ipvlanConf := *conf.IpvlanConfiguration
		index := int(ipvlanConf.MasterDeviceIndex)
		switch ipvlanConf.OperationMode {
		case option.OperationModeL3:
			mode = netlink.IPVLAN_MODE_L3
		case option.OperationModeL3S:
			mode = netlink.IPVLAN_MODE_L3S
		default:
			return fmt.Errorf("invalid or unsupported ipvlan operation mode: %s", ipvlanConf.OperationMode)
		}

		ipvlan, link, tmpIfName, err := connector.SetupIpvlan(ep.ContainerID, int(conf.DeviceMTU), index, mode, ep)
		if err != nil {
			return err
		}

		defer func() {
			if err != nil {
				if err = netlink.LinkDel(ipvlan); err != nil {
					logger.WithError(err).WithField(logfields.Ipvlan, ipvlan.Name).Warn("failed to clean up and delete ipvlan")
				}
			}
		}()

		if err = netlink.LinkSetNsFd(*link, int(netNs.Fd())); err != nil {
			return fmt.Errorf("unable to move ipvlan slave '%v' to netns: %s", link, err)
		}

		mapFD, mapID, err := connector.SetupIpvlanRemoteNs(netNs, tmpIfName, args.IfName)
		if err != nil {
			return err
		}

		ep.DataPathMapID = int64(mapID)
		defer func() {
			unix.Close(mapFD)
		}()
	}

	ipam, err := c.IPAMAllocate("")
	if err != nil {
		return err
	}

	if ipam.Address == nil {
		return fmt.Errorf("Invalid IPAM response, missing addressing")
	}

	// release addresses on failure
	defer func() {
		if err != nil {
			releaseIPs(c, ep.Addressing)
		}
	}()

	if err = connector.SufficientAddressing(ipam.HostAddressing); err != nil {
		return fmt.Errorf("%s", err)
	}

	state := CmdState{
		Endpoint: ep,
		Client:   c,
		HostAddr: ipam.HostAddressing,
	}

	res := &cniTypesVer.Result{}

	if !ipv6IsEnabled(ipam) && !ipv4IsEnabled(ipam) {
		return fmt.Errorf("IPAM did not provide IPv4 or IPv6 address")
	}

	if ipv6IsEnabled(ipam) {
		ep.Addressing.IPV6 = ipam.Address.IPV6

		ipConfig, routes, err := prepareIP(ep.Addressing.IPV6, true, &state, int(conf.RouteMTU))
		if err != nil {
			return err
		}
		res.IPs = append(res.IPs, ipConfig)
		res.Routes = append(res.Routes, routes...)
	}

	if ipv4IsEnabled(ipam) {
		ep.Addressing.IPV4 = ipam.Address.IPV4

		ipConfig, routes, err := prepareIP(ep.Addressing.IPV4, false, &state, int(conf.RouteMTU))
		if err != nil {
			return err
		}
		res.IPs = append(res.IPs, ipConfig)
		res.Routes = append(res.Routes, routes...)
	}

	var macAddrStr string
	if err = netNs.Do(func(_ ns.NetNS) error {
		allInterfacesPath := filepath.Join("/proc", "sys", "net", "ipv6", "conf", "all", "disable_ipv6")
		err = connector.WriteSysConfig(allInterfacesPath, "0\n")
		if err != nil {
			logger.WithError(err).Warn("unable to enable ipv6 on all interfaces")
		}
		macAddrStr, err = configureIface(ipam, args.IfName, &state)
		return err
	}); err != nil {
		return err
	}

	res.Interfaces = append(res.Interfaces, &cniTypesVer.Interface{
		Name:    args.IfName,
		Mac:     macAddrStr,
		Sandbox: "/proc/" + args.Netns + "/ns/net",
	})

	// Specify that endpoint must be regenerated synchronously. See GH-4409.
	ep.SyncBuildEndpoint = true
	if err = c.EndpointCreate(ep); err != nil {
		logger.WithError(err).WithFields(logrus.Fields{
			logfields.ContainerID: ep.ContainerID}).Warn("Unable to create endpoint")
		return fmt.Errorf("Unable to create endpoint: %s", err)
	}

	logger.WithFields(logrus.Fields{
		logfields.ContainerID: ep.ContainerID}).Debug("Endpoint successfully created")
	return cniTypes.PrintResult(res, cniVer)
}

func cmdDel(args *skel.CmdArgs) error {
	log.WithField("args", args).Debug("Processing CNI DEL request")

	c, err := client.NewDefaultClientWithTimeout(defaults.ClientConnectTimeout)
	if err != nil {
		return fmt.Errorf("unable to connect to Cilium daemon: %s", err)
	}

	id := endpointid.NewID(endpointid.ContainerIdPrefix, args.ContainerID)
	if ep, err := c.EndpointGet(id); err != nil {
		// Ignore endpoints not found
		log.WithError(err).WithField(logfields.EndpointID, id).Debug("Agent is not aware of endpoint")
		return nil
	} else if ep == nil {
		log.WithError(err).WithField(logfields.EndpointID, id).Debug("Agent is not aware of endpoint")
		return nil
	} else {
		for _, address := range ep.Status.Networking.Addressing {
			releaseIPs(c, address)
		}
	}

	if err := c.EndpointDelete(id); err != nil {
		log.WithError(err).Warn("Deletion of endpoint failed")
	}

	netNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %s", args.Netns, err)
	}
	defer netNs.Close()

	return removeIfFromNSIfExists(netNs, args.IfName)
}
