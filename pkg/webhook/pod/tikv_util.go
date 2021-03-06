// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pod

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	memberUtil "github.com/pingcap/tidb-operator/pkg/manager/member"
	"github.com/pingcap/tidb-operator/pkg/pdapi"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
)

// checkFormerTiKVPodStatus would check all the former tikv pods whether their store state were UP during Upgrading
// check need both  check former pod is ready ,store up, and no evict leader
func checkFormerTiKVPodStatus(podLister corelisters.PodLister, tc *v1alpha1.TidbCluster, ordinal int32, replicas int32, storesInfo *pdapi.StoresInfo) error {

	tcName := tc.Name
	namespace := tc.Namespace

	for i := replicas - 1; i > ordinal; i-- {
		podName := memberUtil.TikvPodName(tcName, i)
		pod, err := podLister.Pods(namespace).Get(podName)
		if err != nil {
			return err
		}
		revision, exist := pod.Labels[apps.ControllerRevisionHashLabelKey]
		if !exist {
			return fmt.Errorf("tidbcluster: [%s/%s]'s pd pod: [%s] has no label: %s", namespace, tcName, podName, apps.ControllerRevisionHashLabelKey)
		}

		if revision != tc.Status.TiKV.StatefulSet.UpdateRevision {
			return fmt.Errorf("tc[%s/%s]'s tikv pod[%s/%s] is not upgraded yet", namespace, tcName, namespace, podName)
		}

		storeInfo, err := getStoreByPod(pod, tc, storesInfo)
		if err != nil {
			return err
		}
		klog.Infof("pod[%s/%s] store[%d]'s state is %s", namespace, podName, storeInfo.Store.Id, storeInfo.Store.StateName)
		if storeInfo.Store.StateName != v1alpha1.TiKVStateUp {
			return fmt.Errorf("tc[%s/%s]'s tikv pod[%s/%s] state is not up", namespace, tcName, namespace, podName)
		}
	}
	return nil
}

func addEvictLeaderAnnotation(kubeCli kubernetes.Interface, pod *core.Pod) error {

	name := pod.Name
	namespace := pod.Namespace

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	now := time.Now().Format(time.RFC3339)
	pod.Annotations[EvictLeaderBeginTime] = now
	_, err := kubeCli.CoreV1().Pods(namespace).Update(pod)
	if err != nil {
		return err
	}
	klog.Infof("tikv upgrader: set pod %s/%s annotation %s to %s successfully",
		namespace, name, EvictLeaderBeginTime, now)

	return nil
}

func isTiKVReadyToUpgrade(upgradePod *core.Pod, store *pdapi.StoreInfo) bool {

	if store.Status.LeaderCount == 0 {
		klog.Infof("pod[%s/%s] has no region leader in store[%d]", upgradePod.Namespace, upgradePod.Name, store.Store.Id)
		return true
	}

	if evictLeaderBeginTimeStr, evicting := upgradePod.Annotations[EvictLeaderBeginTime]; evicting {
		evictLeaderBeginTime, err := time.Parse(time.RFC3339, evictLeaderBeginTimeStr)
		if err != nil {
			klog.Errorf("parse annotation:[%s] to time failed.", EvictLeaderBeginTime)
			return false
		}
		if time.Now().After(evictLeaderBeginTime.Add(EvictLeaderTimeout)) {
			return true
		}
	}
	return false
}

func beginEvictLeader(kubeCli kubernetes.Interface, storeID uint64, pod *core.Pod, pdClient pdapi.PDClient) error {

	name := pod.Name
	namespace := pod.Namespace

	err := pdClient.BeginEvictLeader(storeID)
	if err != nil {
		klog.Errorf("tikv upgrader: failed to begin evict leader: %d, %s/%s, %v",
			storeID, namespace, name, err)
		return err
	}

	klog.Infof("tikv upgrader: begin evict leader: %d, %s/%s successfully", storeID, namespace, name)
	err = addEvictLeaderAnnotation(kubeCli, pod)
	if err != nil {
		return err
	}
	return nil
}

func endEvictLeader(storeInfo *pdapi.StoreInfo, pdClient pdapi.PDClient) error {
	storeID := storeInfo.Store.Id
	err := pdClient.EndEvictLeader(storeID)
	if err != nil {
		klog.Errorf("tikv upgrader: failed to end evict leader storeID: %d, %v", storeID, err)
		return err
	}
	klog.Infof("tikv upgrader: end evict leader storeID %d successfully", storeID)
	return nil
}

func getStoreByPod(pod *core.Pod, tc *v1alpha1.TidbCluster, storesInfo *pdapi.StoresInfo) (*pdapi.StoreInfo, error) {

	name := pod.Name
	namespace := pod.Namespace
	tcName := tc.Name

	for _, store := range storesInfo.Stores {
		ip := strings.Split(store.Store.GetAddress(), ":")[0]
		podName := strings.Split(ip, ".")[0]
		if podName == name {
			return store, nil
		}
	}

	return nil, fmt.Errorf("failed to find store in tc[%s/%s] pod [%s/%s]", namespace, tcName, namespace, name)
}
