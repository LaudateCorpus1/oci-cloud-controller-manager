// Copyright 2017 The Oracle Kubernetes Cloud Controller Manager Authors
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

package oci

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	baremetal "github.com/oracle/bmcs-go-sdk"
	"github.com/oracle/oci-cloud-controller-manager/pkg/oci/client"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
)

const (
	// ProtocolTCP is the IANA decimal protocol number for the Transmission
	// Control Protocol (TCP).
	ProtocolTCP = 6
	// ProtocolUDP is the IANA decimal protocol number for the User
	// Datagram Protocol (UDP).
	ProtocolUDP = 17
)

type securityListManager interface {
	Update(lbSubnetIDs []string, sourceCIDRs []string, listener *baremetal.Listener, backends []baremetal.Backend) error
	Delete(lbSubnetIDs []string, listener *baremetal.Listener, backends []baremetal.Backend) error
}

type securityListManagerImpl struct {
	securityListCache cache.Store
	subnetCache       cache.Store
	client            client.Interface
}

func subnetCache(obj interface{}) (string, error) {
	return obj.(*baremetal.Subnet).ID, nil
}

func securityListKeyFunc(obj interface{}) (string, error) {
	return obj.(*baremetal.SecurityList).ID, nil
}

func newSecurityListManager(client client.Interface) securityListManager {
	return &securityListManagerImpl{
		client:            client,
		subnetCache:       cache.NewTTLStore(subnetCache, time.Duration(24)*time.Hour),
		securityListCache: cache.NewTTLStore(securityListKeyFunc, time.Duration(24)*time.Hour),
	}
}

func (s *securityListManagerImpl) getSubnetsForBackends(backends []baremetal.Backend) ([]*baremetal.Subnet, error) {
	ips := make([]string, 0, len(backends))
	for _, backend := range backends {
		ips = append(ips, backend.IPAddress)
	}

	return s.client.GetSubnetsForInternalIPs(ips)
}

func (s *securityListManagerImpl) Update(lbSubnetIDs []string, sourceCIDRs []string, listener *baremetal.Listener, backends []baremetal.Backend) error {

	subnets, err := s.getSubnetsForBackends(backends)
	if err != nil {
		return err
	}

	backendPort := getBackendPort(backends)
	lbSunets := make([]*baremetal.Subnet, 0, len(lbSubnetIDs))

	// First lets update the security rules for ingress/egress of the load balancer subnet
	for _, lbSubnetID := range lbSubnetIDs {

		lbSubnet, err := s.getSubnet(lbSubnetID)
		if err != nil {
			return err
		}

		// Save this for when we do ingress into the nodes
		lbSunets = append(lbSunets, lbSubnet)

		lbSecurityList, err := s.getSecurityList(lbSubnet)
		if err != nil {
			return err
		}

		lbEgressRules := getLoadBalancerEgressRules(lbSecurityList, subnets, backendPort)
		lbIngressRules := getLoadBalancerIngressRules(lbSecurityList, sourceCIDRs, uint64(listener.Port))

		glog.V(4).Infof("Updating lb security list `%s` with %d ingress rules & %d egress rules", lbSecurityList.ID, len(lbIngressRules), len(lbEgressRules))

		err = s.updateSecurityListRules(lbSecurityList.ID, lbIngressRules, lbEgressRules)
		if err != nil {
			return err
		}
	}

	// Now we need to add the ingress rules for the nodes.
	for _, subnet := range subnets {
		securityList, err := s.getSecurityList(subnet)
		if err != nil {
			return err
		}

		ingressRules := getNodeIngressRules(securityList, lbSunets, backendPort)

		glog.V(4).Infof("Updating node subnet security list `%s` with %d ingress rules", securityList.ID, len(ingressRules))

		err = s.updateSecurityListRules(securityList.ID, ingressRules, securityList.EgressSecurityRules)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *securityListManagerImpl) Delete(lbSubnetIDs []string, listener *baremetal.Listener, backends []baremetal.Backend) error {
	subnets, err := s.getSubnetsForBackends(backends)
	if err != nil {
		return err
	}

	backendPort := getBackendPort(backends)
	lbSunets := make([]*baremetal.Subnet, 0, len(lbSubnetIDs))

	for _, lbSubnetID := range lbSubnetIDs {
		lbSubnet, err := s.getSubnet(lbSubnetID)
		if err != nil {
			return err
		}

		// Save this for when we do ingress into the nodes
		lbSunets = append(lbSunets, lbSubnet)

		lbSecurityList, err := s.getSecurityList(lbSubnet)
		if err != nil {
			return err
		}

		var noSubnets []*baremetal.Subnet
		lbEgressRules := getLoadBalancerEgressRules(lbSecurityList, noSubnets, backendPort)

		var noSourceCIDRs []string
		lbIngressRules := getLoadBalancerIngressRules(lbSecurityList, noSourceCIDRs, uint64(listener.Port))

		err = s.updateSecurityListRules(lbSecurityList.ID, lbIngressRules, lbEgressRules)
		if err != nil {
			return err
		}
	}

	// Now we need to remove the ingress rules for the nodes.
	for _, subnet := range subnets {
		securityList, err := s.getSecurityList(subnet)
		if err != nil {
			return err
		}

		var noSubnets []*baremetal.Subnet
		ingressRules := getNodeIngressRules(securityList, noSubnets, backendPort)

		err = s.updateSecurityListRules(securityList.ID, ingressRules, securityList.EgressSecurityRules)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *securityListManagerImpl) getSecurityList(subnet *baremetal.Subnet) (*baremetal.SecurityList, error) {
	for _, id := range subnet.SecurityListIDs {
		item, exists, err := s.securityListCache.GetByKey(id)
		if err != nil {
			return nil, err
		}

		if exists {
			return item.(*baremetal.SecurityList), nil
		}
	}

	securityList, err := s.client.GetDefaultSecurityList(subnet)
	if err != nil {
		return nil, err
	}

	s.securityListCache.Add(securityList)
	return securityList, nil
}

func (s *securityListManagerImpl) updateSecurityListRules(securityListID string, ingressRules []baremetal.IngressSecurityRule, egressRules []baremetal.EgressSecurityRule) error {
	updatedList, err := s.client.UpdateSecurityList(securityListID, &baremetal.UpdateSecurityListOptions{
		EgressRules:  egressRules,
		IngressRules: ingressRules,
	})
	if err != nil {
		return err
	}

	// Update the cache since everything was updated successfully
	return s.securityListCache.Update(updatedList)
}

func (s *securityListManagerImpl) getSubnet(id string) (*baremetal.Subnet, error) {
	item, exists, err := s.subnetCache.GetByKey(id)
	if err != nil {
		return nil, err
	}

	if !exists {
		subnet, err := s.client.GetSubnet(id)
		if err != nil {
			return nil, err
		}

		s.subnetCache.Add(subnet)
		return subnet, nil
	}

	return item.(*baremetal.Subnet), nil
}

func getBackendPort(backends []baremetal.Backend) uint64 {
	// TODO: what happens if this is 0? e.g. we scale the pods to 0 for a deployment
	return uint64(backends[0].Port)
}

func getNodeIngressRules(securityList *baremetal.SecurityList, lbSubnets []*baremetal.Subnet, port uint64) []baremetal.IngressSecurityRule {
	desired := sets.NewString()
	for _, lbSubnet := range lbSubnets {
		desired.Insert(lbSubnet.CIDRBlock)
	}

	var ingressRules []baremetal.IngressSecurityRule

	for _, rule := range securityList.IngressSecurityRules {
		if rule.TCPOptions == nil ||
			(rule.TCPOptions.DestinationPortRange.Min != port &&
				rule.TCPOptions.DestinationPortRange.Max != port) {
			// this rule doesn't apply to this service so nothing to do but keep it
			ingressRules = append(ingressRules, rule)
			continue
		}

		if desired.Has(rule.Source) {
			// This rule still exists so lets keep it
			ingressRules = append(ingressRules, rule)
			desired.Delete(rule.Source)
			continue
		}
		// else the actual cidr no longer exists so we don't need to do
		// anything but ignore / delete it.
	}

	if desired.Len() == 0 {
		// actual is the same as desired so there is nothing to do
		return ingressRules
	}

	// All the remaining node cidr's are new and don't have a corresponding rule
	// so we need to create one for each.
	for _, cidr := range desired.List() {
		ingressRules = append(ingressRules, makeIngressSecurityRule(cidr, port))
	}

	return ingressRules
}

func getLoadBalancerIngressRules(lbSecurityList *baremetal.SecurityList, sourceCIDRs []string, port uint64) []baremetal.IngressSecurityRule {
	desired := sets.NewString(sourceCIDRs...)

	var ingressRules []baremetal.IngressSecurityRule
	for _, rule := range lbSecurityList.IngressSecurityRules {

		if rule.TCPOptions == nil ||
			(rule.TCPOptions.DestinationPortRange.Min != port &&
				rule.TCPOptions.DestinationPortRange.Max != port) {
			// this rule doesn't apply to this service so nothing to do but keep it
			ingressRules = append(ingressRules, rule)
			continue
		}

		if desired.Has(rule.Source) {
			// This rule still exists so lets keep it
			ingressRules = append(ingressRules, rule)
			desired.Delete(rule.Source)
			continue
		}
		// else the actual cidr no longer exists so we don't need to do
		// anything but ignore / delete it.
	}

	if desired.Len() == 0 {
		// actual is the same as desired so there is nothing to do
		return ingressRules
	}

	// All the remaining node cidr's are new and don't have a corresponding rule
	// so we need to create one for each.
	for _, cidr := range desired.List() {
		ingressRules = append(ingressRules, makeIngressSecurityRule(cidr, port))
	}

	return ingressRules
}

func getLoadBalancerEgressRules(lbSecurityList *baremetal.SecurityList, nodeSubnets []*baremetal.Subnet, port uint64) []baremetal.EgressSecurityRule {

	nodeCIDRs := sets.NewString()
	for _, subnet := range nodeSubnets {
		nodeCIDRs.Insert(subnet.CIDRBlock)
	}

	var egressRules []baremetal.EgressSecurityRule

	for _, rule := range lbSecurityList.EgressSecurityRules {
		if rule.TCPOptions == nil ||
			(rule.TCPOptions.DestinationPortRange.Min != port &&
				rule.TCPOptions.DestinationPortRange.Max != port) {
			// this rule doesn't apply to this service so nothing to do but keep it
			egressRules = append(egressRules, rule)
			continue
		}

		if nodeCIDRs.Has(rule.Destination) {
			// This rule still exists so lets keep it
			egressRules = append(egressRules, rule)
			nodeCIDRs.Delete(rule.Destination)
			continue
		}
		// else the actual cidr no longer exists so we don't need to do
		// anything but ignore / delete it.
	}

	if nodeCIDRs.Len() == 0 {
		// actual is the same as desired so there is nothing to do
		return egressRules
	}

	// All the remaining node cidr's are new and don't have a corresponding rule
	// so we need to create one for each.
	for _, desired := range nodeCIDRs.List() {
		egressRules = append(egressRules, makeEgressSecurityRule(desired, port))
	}

	return egressRules
}

// TODO(apryde): UDP support.
func makeEgressSecurityRule(cidrBlock string, port uint64) baremetal.EgressSecurityRule {
	return baremetal.EgressSecurityRule{
		Destination: cidrBlock,
		Protocol:    fmt.Sprintf("%d", ProtocolTCP),
		TCPOptions: &baremetal.TCPOptions{
			DestinationPortRange: baremetal.PortRange{
				Min: port,
				Max: port,
			},
		},
		IsStateless: false,
	}
}

// TODO(apryde): UDP support.
func makeIngressSecurityRule(cidrBlock string, port uint64) baremetal.IngressSecurityRule {
	return baremetal.IngressSecurityRule{
		Source:   cidrBlock,
		Protocol: fmt.Sprintf("%d", ProtocolTCP),
		TCPOptions: &baremetal.TCPOptions{
			DestinationPortRange: baremetal.PortRange{
				Min: port,
				Max: port,
			},
		},
		IsStateless: false,
	}
}

// securityListManagerNOOP implements the securityListManager interface but does
// no logic, so that it can be used to not handle security lists if the user doesn't wish
// to use that feature.
type securityListManagerNOOP struct {
}

func (s *securityListManagerNOOP) Update(lbSubnetIDs []string, sourceCIDRs []string, listener *baremetal.Listener, backends []baremetal.Backend) error {
	return nil
}
func (s *securityListManagerNOOP) Delete(lbSubnetIDs []string, listener *baremetal.Listener, backends []baremetal.Backend) error {
	return nil
}

func newSecurityListManagerNOOP() securityListManager {
	return &securityListManagerNOOP{}
}