/*
 * Copyright 2022- IBM Inc. All rights reserved
 * SPDX-License-Identifier: Apache2.0
 */

package controllers

import (
	"context"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	netcogadvisoriov1 "github.com/foundation-model-stack/multi-nic-cni/api/v1"
	"github.com/foundation-model-stack/multi-nic-cni/compute"
	"github.com/foundation-model-stack/multi-nic-cni/plugin"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sort"

	"errors"
	"sync"
)

const (
	BASE_IPAM_TYPE = "multi-nic-ipam"
)

// CIDRHandler handles CIDR object
// - general handling: Get, List, Delete
// - compute VLAN CIDR and create CIDR
type CIDRHandler struct {
	client.Client
	*kubernetes.Clientset
	compute.CIDRCompute
	*HostInterfaceHandler
	*IPPoolHandler
	*MultiNicNetworkHandler
	sync.Mutex
	Log logr.Logger
	RouteHandler
}

func NewCIDRHandler(client client.Client, config *rest.Config, logger logr.Logger, hifLog logr.Logger, ippoolLog logr.Logger, networkLog logr.Logger) *CIDRHandler {
	clientset, _ := kubernetes.NewForConfig(config)
	cidrCompute := compute.CIDRCompute{}

	handler := &CIDRHandler{
		Client:      client,
		Clientset:   clientset,
		Log:         logger,
		CIDRCompute: cidrCompute,
		HostInterfaceHandler: &HostInterfaceHandler{
			Client: client,
			Log:    hifLog,
		},
		IPPoolHandler: &IPPoolHandler{
			Client: client,
			Log:    ippoolLog,
		},
		MultiNicNetworkHandler: &MultiNicNetworkHandler{
			Client: client,
			Log:    networkLog,
		},
		RouteHandler: RouteHandler{
			DaemonConnector: DaemonConnector{
				Clientset: clientset,
			},
			Log: logger,
		},
	}
	return handler
}

/////////////////////////////////////////////////////////////////////////////////////////////////////////
//
// General handling: Get, List, Delete
//
/////////////////////////////////////////////////////////////////////////////////////////////////////////

// GetCIDR gets CIDR from CIDR name
func (h *CIDRHandler) GetCIDR(name string) (*netcogadvisoriov1.CIDR, error) {
	instance := &netcogadvisoriov1.CIDR{}
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: metav1.NamespaceAll,
	}
	err := h.Client.Get(context.TODO(), namespacedName, instance)
	return instance, err
}

// ListCIDR returns a map from CIDR name to instance
func (h *CIDRHandler) ListCIDR() (map[string]netcogadvisoriov1.CIDR, error) {
	cidrList := &netcogadvisoriov1.CIDRList{}
	err := h.Client.List(context.TODO(), cidrList)
	cidrSpecMap := make(map[string]netcogadvisoriov1.CIDR)
	if err == nil {
		for _, cidr := range cidrList.Items {
			cidrName := cidr.GetName()
			cidrSpecMap[cidrName] = cidr
		}
	}
	return cidrSpecMap, err
}

// CleanPreviousCIDR deletes CIDRs if corresponding NetworkAttachmentDefinition does not exist
//                   deletes IPPools if corresponding CIDR does not exist
// note: CIDR name = NetworkAttachmentDefinition name
func (h *CIDRHandler) CleanPreviousCIDR(config *rest.Config, defHandler *plugin.NetAttachDefHandler) {
	cidrMap, err := h.ListCIDR()
	h.Log.Info(fmt.Sprintf("Clean cidr: err=%v", err))
	if err == nil {
		h.Log.Info(fmt.Sprintf("Checking %d cidrs", len(cidrMap)))
		for name, cidr := range cidrMap {
			defName := cidr.Spec.Config.Name
			err := defHandler.Delete(defName, metav1.NamespaceAll)
			if err != nil {
				// corresponding NetworkAttachmentDefinition does not exist, delete CIDR
				h.DeleteCIDR(cidr)
				h.Log.Info(fmt.Sprintf("%v, CIDR %s deleted", err, name))
			}
		}
	}

	poolMap, err := h.IPPoolHandler.ListIPPool()
	if err == nil {
		for _, ippool := range poolMap {
			defName := ippool.Spec.NetAttachDefName
			_, err := h.GetCIDR(defName)
			if err != nil {
				// corresponding CIDR does not exist, delete CIDR
				h.DeleteIPPool(defName, ippool.Spec.PodCIDR)
			}
		}
	}
}

// DeleteCIDR deletes corresponding routes and IPPools, then deletes CIDR
func (h *CIDRHandler) DeleteCIDR(cidr netcogadvisoriov1.CIDR) error {
	errorMsg := ""
	// delete corresponding routes
	h.deleteRoutesFromCIDR(cidr.Spec)
	// delete corresponding IPPools
	for _, entry := range cidr.Spec.CIDRs {
		for _, host := range entry.Hosts {
			podCIDR := host.PodCIDR
			err := h.IPPoolHandler.DeleteIPPool(cidr.GetName(), podCIDR)
			if err != nil {
				errorMsg = errorMsg + fmt.Sprintf("%v,", err)
			}
		}
	}
	err := h.Client.Delete(context.Background(), &cidr)
	if err != nil {
		errorMsg = errorMsg + fmt.Sprintf("%v,", err)
	}
	if len(errorMsg) == 0 {
		return nil
	}
	return fmt.Errorf("%s", errorMsg)
}

// deleteRoutesFromCIDR deletes routes from CIDR
func (h *CIDRHandler) deleteRoutesFromCIDR(cidrInfo netcogadvisoriov1.CIDRSpec) {
	if h.IsL3Mode(cidrInfo.Config) {
		h.RouteHandler.DeleteRoutes(cidrInfo)
	}
}

// GetAllNetAddrs returns all common network address from hiflist
func (h *CIDRHandler) GetAllNetAddrs() []string {
	netAddrSet := []string{}
	netAddressMap := make(map[string]bool)
	hifList, _ := h.HostInterfaceHandler.ListHostInterface()
	for _, hif := range hifList {
		for _, iface := range hif.Spec.Interfaces {
			netAddr := iface.NetAddress
			if _, exist := netAddressMap[netAddr]; !exist {
				netAddrSet = append(netAddrSet, netAddr)
				netAddressMap[netAddr] = true
			}
		}
	}
	return netAddrSet
}

/////////////////////////////////////////////////////////////////////////////////////////////////////////
//
// Compute VLAN CIDR and create CIDR
//
/////////////////////////////////////////////////////////////////////////////////////////////////////////

// NewCIDRWithNewConfig creates new CIDR by computing interface indexes from master networks
func (h *CIDRHandler) NewCIDRWithNewConfig(def netcogadvisoriov1.PluginConfig, namespace string) (bool, error) {
	h.Log.Info("NewCIDRWithNewConfig")
	entries := []netcogadvisoriov1.CIDREntry{}
	interfaceNetIndexMap := make(map[string]int)
	masterIndex := int(0)
	// maxInterfaceIndex = 2^(interface bits) - 1
	maxInterfaceIndex := int(math.Pow(2, float64(def.InterfaceBlock)) - 1)
	// loop over defined network addresses
	for _, master := range def.MasterNetAddrs {
		vlanCIDR := ""
		// find available VLAN CIDR
		for vlanCIDR == "" {
			vlanInByte, err := h.CIDRCompute.ComputeNet(def.Subnet, masterIndex, def.InterfaceBlock)
			if err != nil {
				// invalid VLAN value (out of range), find next interface index
				masterIndex = masterIndex + 1
				continue
			}
			// check if computed vlan in exclude ranges
			tabu := h.CIDRCompute.CheckIfTabuIndex(def.Subnet, masterIndex, def.InterfaceBlock, def.ExcludeCIDRs)
			if !tabu {
				vlanCIDR = h.CIDRCompute.GetCIDRFromByte(vlanInByte, def.Subnet, def.InterfaceBlock)
				break
			}
			// if tabu, find next interface index
			masterIndex = masterIndex + 1
			if masterIndex > maxInterfaceIndex {
				return false, errors.New("wrong request (overflow interface index)")
			}
		}
		interfaceNetIndexMap[master] = masterIndex

		entry := netcogadvisoriov1.CIDREntry{
			NetAddress:     master,
			InterfaceIndex: masterIndex,
			VlanCIDR:       vlanCIDR,
			Hosts:          []netcogadvisoriov1.HostInterfaceInfo{},
		}
		entries = append(entries, entry)
		masterIndex = masterIndex + 1
	}

	cidrSpec := netcogadvisoriov1.CIDRSpec{
		Config: def,
		CIDRs:  entries,
	}
	return h.UpdateCIDR(cidrSpec, true)
}

// UpdateCIDR computes host indexes and coresponding pod VLAN from host interface list
func (h *CIDRHandler) UpdateCIDR(cidrSpec netcogadvisoriov1.CIDRSpec, new bool) (bool, error) {
	h.Mutex.Lock()

	def := cidrSpec.Config
	entries := cidrSpec.CIDRs
	h.Log.Info(fmt.Sprintf("Update CIDR %s", def.Name))

	// sort and convert exclude object to string
	excludes := compute.SortAddress(def.ExcludeCIDRs)
	excludesInStr := []string{}
	for _, exclude := range excludes {
		excludesInStr = append(excludesInStr, exclude.Address)
	}

	changed := new

	entriesMap := make(map[string]netcogadvisoriov1.CIDREntry)
	for _, entry := range entries {
		var newHostList []netcogadvisoriov1.HostInterfaceInfo
		for _, host := range entry.Hosts {
			if _, exists := HostInterfaceCache[host.HostName]; exists {
				newHostList = append(newHostList, host)
			} else {
				// host not exist anymore
				changed = true
			}
		}
		entry.Hosts = newHostList
		entriesMap[entry.NetAddress] = entry
	}

	// maxHostIndex = 2^(host bits) - 1
	maxHostIndex := int(math.Pow(2, float64(def.HostBlock)) - 1)

	// compute host indexes over host interface list
	for _, hif := range HostInterfaceCache {
		hostName := hif.Spec.HostName
		ifaces := hif.Spec.Interfaces

		// assign interface index to each host
		for _, iface := range ifaces {
			interfaceNetAddress := iface.NetAddress
			interfaceName := iface.InterfaceName
			hostIP := iface.HostIP

			if entry, exists := entriesMap[interfaceNetAddress]; exists {
				vlanCIDR := entry.VlanCIDR
				existingHosts := entry.Hosts

				// check if host index computed before
				itemIndex := h.getHostIndex(existingHosts, hostName)
				if itemIndex == -1 {
					// compute new host index
					entry, changed = h.tryAddNewHost(existingHosts, entry, maxHostIndex, def, hostName, interfaceName, hostIP)
				} else {
					// refer to previous host index
					host := existingHosts[itemIndex]
					nodeIndex := existingHosts[itemIndex].HostIndex
					nodeBlock := def.HostBlock
					podInByte, err := h.CIDRCompute.ComputeNet(entry.VlanCIDR, host.HostIndex, def.HostBlock)
					if err != nil {
						// invalid pod VLAN
						// remove from existing list
						entry.Hosts = append(entry.Hosts[0:itemIndex], entry.Hosts[itemIndex+1:]...)
						// recompute host index
						entry, changed = h.tryAddNewHost(existingHosts, entry, maxHostIndex, def, hostName, interfaceName, hostIP)
					} else {
						// recheck is computed pod VLAN tabu
						tabu := h.CIDRCompute.CheckIfTabuIndex(vlanCIDR, nodeIndex, nodeBlock, excludesInStr)
						if !tabu {
							podCIDR := h.CIDRCompute.GetCIDRFromByte(podInByte, vlanCIDR, nodeBlock)
							// check if recomputed pod VLAN equal to the computed pod VLAN in  CIDR resource
							if podCIDR != host.PodCIDR {
								entry.Hosts[itemIndex].PodCIDR = podCIDR
								changed = true
							}
							if interfaceName != host.InterfaceName {
								entry.Hosts[itemIndex].InterfaceName = interfaceName
								changed = true
							}
							if hostIP != host.HostIP {
								entry.Hosts[itemIndex].HostIP = hostIP
								changed = true
							}
						} else {
							// tabu, recompute host index
							entry.Hosts = append(entry.Hosts[0:itemIndex], entry.Hosts[itemIndex+1:]...)
							entry, changed = h.tryAddNewHost(existingHosts, entry, maxHostIndex, def, hostName, interfaceName, hostIP)
						}
					}
				}
				entriesMap[interfaceNetAddress] = entry
			}
		}
	}

	// if pod CIDR changes, update CIDR and create corresponding IPPools and routes
	if changed {
		newEntries := []netcogadvisoriov1.CIDREntry{}
		for _, entry := range entriesMap {
			newEntries = append(newEntries, entry)
		}

		spec := netcogadvisoriov1.CIDRSpec{
			Config: def,
			CIDRs:  newEntries,
		}
		mapObj := &netcogadvisoriov1.CIDR{
			ObjectMeta: metav1.ObjectMeta{
				Name: def.Name,
			},
			Spec: spec,
		}

		// create/update CIDR
		existCIDR, err := h.GetCIDR(def.Name)
		if err == nil {
			updateCIDR := existCIDR.DeepCopy()
			updateCIDR.Spec = spec
			h.cleanPendingIPPools(def.Name, existCIDR, updateCIDR)
			err = h.Client.Update(context.TODO(), updateCIDR)
		} else {
			err = h.Client.Create(context.TODO(), mapObj)
		}

		if err != nil {
			h.Log.Info(fmt.Sprintf("Cannot create or update CIDR %s: error=%v", def.Name, err))
			h.Mutex.Unlock()
			return false, err
		}
		// initialize the MultiNicNetwork status
		h.MultiNicNetworkHandler.UpdateStatus(*mapObj, netcogadvisoriov1.ApplyingRoute)

		// update IPPools
		for _, entry := range newEntries {
			for _, host := range entry.Hosts {
				err = h.IPPoolHandler.UpdateIPPool(cidrSpec.Config.Name, host.PodCIDR, entry.VlanCIDR, host.HostName, host.InterfaceName, excludes)
				if err != nil {
					h.Log.Info(fmt.Sprintf("Cannot update IPPools for host %s: error=%v", host.HostName, err))
				}
			}
		}
		h.Log.Info(fmt.Sprintf("CIDR %s changed", def.Name))
	}
	h.Mutex.Unlock()
	return changed, nil
}

// SyncCIDRRoute try adding routes by CIDR
func (h *CIDRHandler) SyncCIDRRoute(cidrSpec netcogadvisoriov1.CIDRSpec, forceDelete bool) (status netcogadvisoriov1.RouteStatus) {
	def := cidrSpec.Config
	// try re-adding routes
	if h.IsL3Mode(def) {
		h.Mutex.Lock()
		entries := cidrSpec.CIDRs
		hostInterfaceInfoMap := h.GetHostInterfaceIndexMap(entries)
		h.Log.Info(fmt.Sprintf("Sync routes from CIDR (force delete: %v)", forceDelete))
		success, noConnection := h.RouteHandler.AddRoutes(cidrSpec, entries, hostInterfaceInfoMap, forceDelete)
		h.Mutex.Unlock()
		if noConnection {
			return netcogadvisoriov1.RouteUnknown
		}
		if forceDelete && !success {
			return netcogadvisoriov1.SomeRouteFailed
		}
		return netcogadvisoriov1.AllRouteApplied
	} else {
		return netcogadvisoriov1.RouteNoApplied
	}
}

func (h *CIDRHandler) SyncCIDRRouteToHost(daemon corev1.Pod) {
	for name, cidrSpec := range CIDRCache {
		def := cidrSpec.Config
		if h.IsL3Mode(def) {
			h.Mutex.Lock()
			entries := cidrSpec.CIDRs
			hostInterfaceInfoMap := h.GetHostInterfaceIndexMap(entries)
			hostName := daemon.Spec.NodeName
			if _, ok := hostInterfaceInfoMap[hostName]; ok {
				change, connectFail := h.AddRoutesToHost(cidrSpec, hostName, daemon, entries, hostInterfaceInfoMap, false)
				h.Log.Info(fmt.Sprintf("Add route to host %s change:%v, connectionFail: %v)", hostName, change, connectFail))
				if connectFail {
					routeStatus := netcogadvisoriov1.RouteUnknown
					err := h.MultiNicNetworkHandler.SyncStatus(name, cidrSpec, routeStatus)
					if err != nil {
						h.Log.Info(fmt.Sprintf("failed to update route status of %s: %v", name, err))
					}
				}
			}
			h.Mutex.Unlock()
		}
	}
}

// DeleteOldRoutes forcefully deletes old routes from CIDR
func (h *CIDRHandler) DeleteOldRoutes(cidrSpec netcogadvisoriov1.CIDRSpec) {
	def := cidrSpec.Config
	if h.IsL3Mode(def) {
		h.RouteHandler.DeleteRoutes(cidrSpec)
	}
}

// cleanPendingIPPools clean ippools in case that cidr is updated with new subnet entry
func (h *CIDRHandler) cleanPendingIPPools(defName string, oldCIDR *netcogadvisoriov1.CIDR, newCIDR *netcogadvisoriov1.CIDR) {
	newPoolMap := make(map[string]bool)

	for _, entry := range newCIDR.Spec.CIDRs {
		for _, host := range entry.Hosts {
			ippoolName := h.IPPoolHandler.GetIPPoolName(defName, host.PodCIDR)
			newPoolMap[ippoolName] = true
		}
	}
	for _, entry := range oldCIDR.Spec.CIDRs {
		for _, host := range entry.Hosts {
			ippoolName := h.IPPoolHandler.GetIPPoolName(defName, host.PodCIDR)
			if _, exist := newPoolMap[ippoolName]; !exist {
				ippool, err := h.IPPoolHandler.GetIPPool(ippoolName)
				if err == nil {
					// corresponding CIDR does not exist, delete CIDR
					h.DeleteIPPool(defName, ippool.Spec.PodCIDR)
				}
			}
		}
	}
}

// addNewHost finds new available host index
func (h *CIDRHandler) addNewHost(hosts []netcogadvisoriov1.HostInterfaceInfo, maxHostIndex int, vlanCIDR string, nodeBlock int, excludes []string) (string, int, error) {
	nodeIndex := 0
	// excludedIndexes = previously-assigned host indexes
	excludedIndexes := []int{}
	for _, host := range hosts {
		excludedIndexes = append(excludedIndexes, host.HostIndex)
	}
	// find new available host index
	for {
		if len(excludedIndexes) > 0 {
			sort.Ints(excludedIndexes)
			// set nodeIndex to the next number from the last assigned index
			nodeIndex = excludedIndexes[len(excludedIndexes)-1] + 1
			if nodeIndex > maxHostIndex {
				// next number is too large, find unassigned index
				nodeIndex = h.CIDRCompute.FindAvailableIndex(excludedIndexes, 0, 0)
				if nodeIndex == -1 {
					// no index available, return error
					return "", -1, errors.New("wrong request (no available host index)")
				}
			}
		}
		vlanInByte, err := h.CIDRCompute.ComputeNet(vlanCIDR, nodeIndex, nodeBlock)
		if err == nil {
			// valid VLAN, check tabu ranges in definition
			tabu := h.CIDRCompute.CheckIfTabuIndex(vlanCIDR, nodeIndex, nodeBlock, excludes)
			if !tabu {
				// not tabu, return valid pod CIDR
				podCIDR := h.CIDRCompute.GetCIDRFromByte(vlanInByte, vlanCIDR, nodeBlock)
				return podCIDR, nodeIndex, nil
			}
		} else {
			// invalid VLAN
			h.Log.Info(fmt.Sprintf("Cannot assign nodeIndex %d, %v", nodeIndex, err))
		}
		// VLAN in tabu ranges or invalid, try next
		excludedIndexes = append(excludedIndexes, nodeIndex)
	}
}

// tryAddNewHost creates new entry of HostInterfaceInfo in CIDR and computes corresponding pod VLAN
func (h *CIDRHandler) tryAddNewHost(existingHosts []netcogadvisoriov1.HostInterfaceInfo, entry netcogadvisoriov1.CIDREntry, maxHostIndex int, def netcogadvisoriov1.PluginConfig, hostName, interfaceName, hostIP string) (netcogadvisoriov1.CIDREntry, bool) {
	h.Log.Info(fmt.Sprintf("TryAddNewHost %v %d,%s,%s,%s", entry, maxHostIndex, hostName, interfaceName, hostIP))
	podCIDR, hostIndex, err := h.addNewHost(existingHosts, maxHostIndex, entry.VlanCIDR, def.HostBlock, def.ExcludeCIDRs)
	if err == nil {
		// successfully compute pod VLAN, create and append new entry of HostInterfaceInfo orderly
		newHost := netcogadvisoriov1.HostInterfaceInfo{
			HostIndex:     hostIndex,
			HostName:      hostName,
			InterfaceName: interfaceName,
			HostIP:        hostIP,
			PodCIDR:       podCIDR,
		}
		hosts := append(existingHosts, newHost)
		sort.SliceStable(hosts, func(i, j int) bool {
			return hosts[i].HostIndex < hosts[j].HostIndex
		})
		entry.Hosts = hosts
		return entry, true
	} else {
		h.Log.Info(fmt.Sprintf("Cannot add new host %s, %s: %v", hostName, interfaceName, err))
		return entry, false
	}
}

// IsL3Mode checkes L3 VLAN mode (to add/delete L3 routes automatically)
func (h *CIDRHandler) IsL3Mode(def netcogadvisoriov1.PluginConfig) bool {
	mode := def.VlanMode
	switch mode {
	case "", "l2":
		return false
	case "l3":
		return true
	case "l3s":
		return true
	default:
		return false
	}
}

// getHostIndex finds assigned host index from the HostInterfaceInfo list
func (h *CIDRHandler) getHostIndex(hosts []netcogadvisoriov1.HostInterfaceInfo, hostName string) int {
	for index, host := range hosts {
		if hostName == host.HostName {
			return index
		}
	}
	return -1
}

// GetHostInterfaceIndexMap finds a map from (host name, interface index) to HostInterfaceInfo of CIDR
func (h *CIDRHandler) GetHostInterfaceIndexMap(entries []netcogadvisoriov1.CIDREntry) map[string]map[int]netcogadvisoriov1.HostInterfaceInfo {
	hostInterfaceIndexMap := make(map[string]map[int]netcogadvisoriov1.HostInterfaceInfo)
	for _, entry := range entries {
		ifaceIndex := entry.InterfaceIndex
		for _, host := range entry.Hosts {
			hostName := host.HostName
			if _, exists := hostInterfaceIndexMap[hostName]; !exists {
				hostInterfaceIndexMap[hostName] = make(map[int]netcogadvisoriov1.HostInterfaceInfo)
			}
			hostInterfaceIndexMap[hostName][ifaceIndex] = host
		}
	}
	return hostInterfaceIndexMap
}
