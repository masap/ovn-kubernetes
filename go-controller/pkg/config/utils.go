package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	utilnet "k8s.io/utils/net"
)

// HostPort is the object that holds the definition for a host and port tuple
type HostPort struct {
	Host *net.IP
	Port int32
}

// CIDRNetworkEntry is the object that holds the definition for a single network CIDR range
type CIDRNetworkEntry struct {
	CIDR             *net.IPNet
	HostSubnetLength int
}

// ParseClusterSubnetEntries returns the parsed set of CIDRNetworkEntries passed by the user on the command line
// These entries define the clusters network space by specifying a set of CIDR and netmasks the SDN can allocate
// addresses from.
func ParseClusterSubnetEntries(clusterSubnetCmd string) ([]CIDRNetworkEntry, error) {
	var parsedClusterList []CIDRNetworkEntry
	clusterEntriesList := strings.Split(clusterSubnetCmd, ",")

	for _, clusterEntry := range clusterEntriesList {
		var parsedClusterEntry CIDRNetworkEntry

		splitClusterEntry := strings.Split(clusterEntry, "/")

		if len(splitClusterEntry) < 2 || len(splitClusterEntry) > 3 {
			return nil, fmt.Errorf("CIDR %q not properly formatted", clusterEntry)
		}

		var err error
		_, parsedClusterEntry.CIDR, err = net.ParseCIDR(fmt.Sprintf("%s/%s", splitClusterEntry[0], splitClusterEntry[1]))
		if err != nil {
			return nil, err
		}

		ipv6 := utilnet.IsIPv6(parsedClusterEntry.CIDR.IP)
		entryMaskLength, _ := parsedClusterEntry.CIDR.Mask.Size()
		if len(splitClusterEntry) == 3 {
			tmp, err := strconv.Atoi(splitClusterEntry[2])
			if err != nil {
				return nil, err
			}
			parsedClusterEntry.HostSubnetLength = tmp

			if ipv6 && parsedClusterEntry.HostSubnetLength != 64 {
				return nil, fmt.Errorf("IPv6 only supports /64 host subnets")
			}
		} else {
			if ipv6 {
				parsedClusterEntry.HostSubnetLength = 64
			} else {
				// default for backward compatibility
				parsedClusterEntry.HostSubnetLength = 24
			}
		}

		if parsedClusterEntry.HostSubnetLength <= entryMaskLength {
			return nil, fmt.Errorf("cannot use a host subnet length mask shorter than or equal to the cluster subnet mask. "+
				"host subnet length: %d, cluster subnet length: %d", parsedClusterEntry.HostSubnetLength, entryMaskLength)
		}

		parsedClusterList = append(parsedClusterList, parsedClusterEntry)
	}

	if len(parsedClusterList) == 0 {
		return nil, fmt.Errorf("failed to parse any CIDRs from %q", clusterSubnetCmd)
	}

	return parsedClusterList, nil
}

// ParseFlowCollectors returns the parsed set of HostPorts passed by the user on the command line
// These entries define the flow collectors OVS will send flow metadata by using NetFlow/SFlow/IPFIX.
func ParseFlowCollectors(flowCollectors string) ([]HostPort, error) {
	var parsedFlowsCollectors []HostPort

	collectors := strings.Split(flowCollectors, ",")
	for _, v := range collectors {
		host, port, err := net.SplitHostPort(v)
		if err != nil {
			return nil, fmt.Errorf("cannot parse hostport: %v", err)
		}
		var ipp *net.IP
		// If the host IP is not provided, we keep it nil and later will assume the Node IP
		if host != "" {
			ip := net.ParseIP(host)
			if ip == nil {
				return nil, fmt.Errorf("collector IP %s is not a valid IP", host)
			}
			ipp = &ip
		}
		parsedPort, err := strconv.ParseInt(port, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("collector port %s is not a valid port: %v", port, err)
		}
		parsedFlowsCollectors = append(parsedFlowsCollectors, HostPort{Host: ipp, Port: int32(parsedPort)})
	}

	return parsedFlowsCollectors, nil
}

type configSubnetType string

const (
	configSubnetJoin    configSubnetType = "built-in join subnet"
	configSubnetCluster configSubnetType = "cluster subnet"
	configSubnetService configSubnetType = "service subnet"
	configSubnetHybrid  configSubnetType = "hybrid overlay subnet"
)

type configSubnet struct {
	subnetType configSubnetType
	subnet     *net.IPNet
}

// configSubnets represents a set of configured subnets (and their names)
type configSubnets struct {
	subnets []configSubnet
	v4      map[configSubnetType]bool
	v6      map[configSubnetType]bool
}

// newConfigSubnets returns a new configSubnets
func newConfigSubnets() *configSubnets {
	return &configSubnets{
		v4: make(map[configSubnetType]bool),
		v6: make(map[configSubnetType]bool),
	}
}

// append adds a single subnet to cs
func (cs *configSubnets) append(subnetType configSubnetType, subnet *net.IPNet) {
	cs.subnets = append(cs.subnets, configSubnet{subnetType: subnetType, subnet: subnet})
	if subnetType != configSubnetJoin {
		if utilnet.IsIPv6CIDR(subnet) {
			cs.v6[subnetType] = true
		} else {
			cs.v4[subnetType] = true
		}
	}
}

// checkForOverlaps checks if any of the subnets in cs overlap
func (cs *configSubnets) checkForOverlaps() error {
	for i, si := range cs.subnets {
		for j := 0; j < i; j++ {
			sj := cs.subnets[j]
			if si.subnet.Contains(sj.subnet.IP) || sj.subnet.Contains(si.subnet.IP) {
				return fmt.Errorf("illegal network configuration: %s %q overlaps %s %q",
					si.subnetType, si.subnet.String(),
					sj.subnetType, sj.subnet.String())
			}
		}
	}
	return nil
}

func (cs *configSubnets) describeSubnetType(subnetType configSubnetType) string {
	ipv4 := cs.v4[subnetType]
	ipv6 := cs.v6[subnetType]
	var familyType string
	switch {
	case ipv4 && !ipv6:
		familyType = "IPv4"
	case !ipv4 && ipv6:
		familyType = "IPv6"
	case ipv4 && ipv6:
		familyType = "dual-stack"
	default:
		familyType = "unknown type"
	}
	return familyType + " " + string(subnetType)
}

// checkIPFamilies determines if cs contains a valid single-stack IPv4 configuration, a
// valid single-stack IPv6 configuration, a valid dual-stack configuration, or none of the
// above.
func (cs *configSubnets) checkIPFamilies() (usingIPv4, usingIPv6 bool, err error) {
	if len(cs.v6) == 0 {
		// Single-stack IPv4
		return true, false, nil
	} else if len(cs.v4) == 0 {
		// Single-stack IPv6
		return false, true, nil
	} else if reflect.DeepEqual(cs.v4, cs.v6) {
		// Dual-stack
		return true, true, nil
	}

	netConfig := cs.describeSubnetType(configSubnetCluster)
	netConfig += ", " + cs.describeSubnetType(configSubnetService)
	if cs.v4[configSubnetHybrid] || cs.v6[configSubnetHybrid] {
		netConfig += ", " + cs.describeSubnetType(configSubnetHybrid)
	}

	return false, false, fmt.Errorf("illegal network configuration: %s", netConfig)
}
