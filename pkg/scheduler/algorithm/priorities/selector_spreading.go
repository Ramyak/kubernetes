/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package priorities

import (
	"fmt"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/pkg/scheduler/algorithm"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/api"
	"k8s.io/kubernetes/pkg/scheduler/schedulercache"
	utilnode "k8s.io/kubernetes/pkg/util/node"

	"github.com/golang/glog"
)

// When zone information is present, give 2/3 of the weighting to zone spreading, 1/3 to node spreading
// TODO: Any way to justify this weighting?
const zoneWeighting float64 = 2.0 / 3.0

// SelectorSpread contains information to calculate selector spread priority.
type SelectorSpread struct {
	serviceLister     algorithm.ServiceLister
	controllerLister  algorithm.ControllerLister
	replicaSetLister  algorithm.ReplicaSetLister
	statefulSetLister algorithm.StatefulSetLister
	podLister         algorithm.PodLister
}

// NewSelectorSpreadPriority creates a SelectorSpread.
func NewSelectorSpreadPriority(
	serviceLister algorithm.ServiceLister,
	controllerLister algorithm.ControllerLister,
	replicaSetLister algorithm.ReplicaSetLister,
	statefulSetLister algorithm.StatefulSetLister,
	podLister algorithm.PodLister) (algorithm.PriorityMapFunction, algorithm.PriorityReduceFunction) {
	selectorSpread := &SelectorSpread{
		serviceLister:     serviceLister,
		controllerLister:  controllerLister,
		replicaSetLister:  replicaSetLister,
		statefulSetLister: statefulSetLister,
		podLister:         podLister,
	}
	return selectorSpread.CalculateSpreadPriorityMap, selectorSpread.CalculateSpreadPriorityReduce
}

// CalculateSpreadPriorityMap spreads pods across hosts, considering pods
// belonging to the same service,RC,RS or StatefulSet.
// When a pod is scheduled, it looks for services, RCs,RSs and StatefulSets that match the pod,
// then finds existing pods that match those selectors.
// It favors nodes that have fewer existing matching pods.
// i.e. it pushes the scheduler towards a node where there's the smallest number of
// pods which match the same service, RC,RSs or StatefulSets selectors as the pod being scheduled.
func (s *SelectorSpread) CalculateSpreadPriorityMap(pod *v1.Pod, meta interface{}, nodeInfo *schedulercache.NodeInfo) (schedulerapi.HostPriority, error) {
	var selectors []labels.Selector
	node := nodeInfo.Node()
	if node == nil {
		return schedulerapi.HostPriority{}, fmt.Errorf("node not found")
	}

	priorityMeta, ok := meta.(*priorityMetadata)
	if ok {
		selectors = priorityMeta.podSelectors
	} else {
		selectors = getSelectors(pod, s.serviceLister, s.controllerLister, s.replicaSetLister, s.statefulSetLister)
	}

	if len(selectors) == 0 {
		return schedulerapi.HostPriority{
			Host:  node.Name,
			Score: int(0),
		}, nil
	}

	filteredPods := filteredPodsMatchAllSelectors(pod.Namespace, selectors, nodeInfo.Pods())
	count := len(filteredPods)
	return schedulerapi.HostPriority{
		Host:  node.Name,
		Score: int(count),
	}, nil
}

// CalculateSpreadPriorityReduce calculates the source of each node
// based on the number of existing matching pods on the node
// where zone information is included on the nodes, it favors nodes
// in zones with fewer existing matching pods.
func (s *SelectorSpread) CalculateSpreadPriorityReduce(pod *v1.Pod, meta interface{}, nodeNameToInfo map[string]*schedulercache.NodeInfo, result schedulerapi.HostPriorityList) error {
	var selectors []labels.Selector
	priorityMeta, ok := meta.(*priorityMetadata)
	if ok {
		selectors = priorityMeta.podSelectors
	} else {
		selectors = getSelectors(pod, s.serviceLister, s.controllerLister, s.replicaSetLister, s.statefulSetLister)
	}

	countsByZone := make(map[string]int, 10)
	countsByNodeName := make(map[string]int, 10)
	maxCountByZone := int(0)
	maxCountByNodeName := int(0)

	if len(selectors) > 0 {
		pods, _ := s.podLister.List(selectors[0])
		filteredPods := filteredPodsMatchAllSelectors(pod.Namespace, selectors, pods)

		for _, pod := range filteredPods {
			if pod.Spec.NodeName == "" {
				continue
			}
			countsByNodeName[pod.Spec.NodeName]++
			if maxCountByNodeName < countsByNodeName[pod.Spec.NodeName] {
				maxCountByNodeName = countsByNodeName[pod.Spec.NodeName]
			}

			if nodeNameToInfo[pod.Spec.NodeName] == nil {
				continue
			}
			zoneID := utilnode.GetZoneKey(nodeNameToInfo[pod.Spec.NodeName].Node())
			if zoneID != "" {
				countsByZone[zoneID]++
				if countsByZone[zoneID] > maxCountByZone {
					maxCountByZone = countsByZone[zoneID]
				}
			}
		}

		if glog.V(10) {
			for zone, cnt := range countsByZone {
				glog.Infof("pod: %s namespace: %s countsByZone(%s): %d", pod.Name, pod.Namespace, zone, cnt)
			}
		}
	}
	haveZones := len(countsByZone) != 0

	maxCountByNodeNameFloat64 := float64(maxCountByNodeName)
	maxCountByZoneFloat64 := float64(maxCountByZone)
	MaxPriorityFloat64 := float64(schedulerapi.MaxPriority)

	for i := range result {
		// initializing to the default/max node score of maxPriority
		fScore := MaxPriorityFloat64
		if maxCountByNodeName > 0 {
			fScore = MaxPriorityFloat64 * (float64(maxCountByNodeName-result[i].Score) / maxCountByNodeNameFloat64)
		}
		// If there is zone information present, incorporate it
		if haveZones {
			zoneID := utilnode.GetZoneKey(nodeNameToInfo[result[i].Host].Node())
			if zoneID != "" {
				zoneScore := MaxPriorityFloat64
				if maxCountByZone > 0 {
					zoneScore = MaxPriorityFloat64 * (float64(maxCountByZone-countsByZone[zoneID]) / maxCountByZoneFloat64)
				}
				fScore = (fScore * (1.0 - zoneWeighting)) + (zoneWeighting * zoneScore)
			}
		}
		result[i].Score = int(fScore)
		if glog.V(10) {
			glog.Infof(
				"%v -> %v: SelectorSpreadPriority, Score: (%d)", pod.Name, result[i].Host, int(fScore),
			)
		}
	}
	return nil
}

// ServiceAntiAffinity contains information to calculate service anti-affinity priority.
type ServiceAntiAffinity struct {
	podLister     algorithm.PodLister
	serviceLister algorithm.ServiceLister
	label         string
}

// NewServiceAntiAffinityPriority creates a ServiceAntiAffinity.
func NewServiceAntiAffinityPriority(podLister algorithm.PodLister, serviceLister algorithm.ServiceLister, label string) (algorithm.PriorityMapFunction, algorithm.PriorityReduceFunction) {
	antiAffinity := &ServiceAntiAffinity{
		podLister:     podLister,
		serviceLister: serviceLister,
		label:         label,
	}
	return antiAffinity.CalculateAntiAffinityPriorityMap, antiAffinity.CalculateAntiAffinityPriorityReduce
}

// Classifies nodes into ones with labels and without labels.
func (s *ServiceAntiAffinity) getNodeClassificationByLabels(nodes []*v1.Node) (map[string]string, []string) {
	labeledNodes := map[string]string{}
	nonLabeledNodes := []string{}
	for _, node := range nodes {
		if labels.Set(node.Labels).Has(s.label) {
			label := labels.Set(node.Labels).Get(s.label)
			labeledNodes[node.Name] = label
		} else {
			nonLabeledNodes = append(nonLabeledNodes, node.Name)
		}
	}
	return labeledNodes, nonLabeledNodes
}

// filteredPodsMatchAllSelectors get pods based on namespace and matching all selectors
func filteredPodsMatchAllSelectors(namespace string, selectors []labels.Selector, pods []*v1.Pod) (filteredPods []*v1.Pod) {
	if pods == nil || len(pods) == 0 || selectors == nil {
		return []*v1.Pod{}
	}
	for _, pod := range pods {
		// Ignore pods being deleted for spreading purposes
		// Ignore pods that do not match all selectors
		if namespace == pod.Namespace && pod.DeletionTimestamp == nil {
			matches := true
			for _, selector := range selectors {
				if !selector.Matches(labels.Set(pod.ObjectMeta.Labels)) {
					matches = false
					break
				}
			}
			if matches {
				filteredPods = append(filteredPods, pod)
			}
		}
	}
	return
}

// filteredPod get pods based on namespace and selector
func filteredPod(namespace string, selector labels.Selector, nodeInfo *schedulercache.NodeInfo) (pods []*v1.Pod) {
	if nodeInfo.Pods() == nil || len(nodeInfo.Pods()) == 0 || selector == nil {
		return []*v1.Pod{}
	}
	for _, pod := range nodeInfo.Pods() {
		if namespace == pod.Namespace && selector.Matches(labels.Set(pod.Labels)) {
			pods = append(pods, pod)
		}
	}
	return
}

// CalculateAntiAffinityPriorityMap spreads pods by minimizing the number of pods belonging to the same service
// on given machine
func (s *ServiceAntiAffinity) CalculateAntiAffinityPriorityMap(pod *v1.Pod, meta interface{}, nodeInfo *schedulercache.NodeInfo) (schedulerapi.HostPriority, error) {
	var firstServiceSelector labels.Selector

	node := nodeInfo.Node()
	if node == nil {
		return schedulerapi.HostPriority{}, fmt.Errorf("node not found")
	}
	priorityMeta, ok := meta.(*priorityMetadata)
	if ok {
		firstServiceSelector = priorityMeta.podFirstServiceSelector
	} else {
		firstServiceSelector = getFirstServiceSelector(pod, s.serviceLister)
	}
	//pods matched namespace,selector on current node
	matchedPodsOfNode := filteredPod(pod.Namespace, firstServiceSelector, nodeInfo)

	return schedulerapi.HostPriority{
		Host:  node.Name,
		Score: int(len(matchedPodsOfNode)),
	}, nil
}

// CalculateAntiAffinityPriorityReduce computes each node score with the same value for a particular label.
// The label to be considered is provided to the struct (ServiceAntiAffinity).
func (s *ServiceAntiAffinity) CalculateAntiAffinityPriorityReduce(pod *v1.Pod, meta interface{}, nodeNameToInfo map[string]*schedulercache.NodeInfo, result schedulerapi.HostPriorityList) error {
	var numServicePods int
	var label string
	podCounts := map[string]int{}
	labelNodesStatus := map[string]string{}
	maxPriorityFloat64 := float64(schedulerapi.MaxPriority)

	for _, hostPriority := range result {
		numServicePods += hostPriority.Score
		if !labels.Set(nodeNameToInfo[hostPriority.Host].Node().Labels).Has(s.label) {
			continue
		}
		label = labels.Set(nodeNameToInfo[hostPriority.Host].Node().Labels).Get(s.label)
		labelNodesStatus[hostPriority.Host] = label
		podCounts[label] += hostPriority.Score
	}

	//score int - scale of 0-maxPriority
	// 0 being the lowest priority and maxPriority being the highest
	for i, hostPriority := range result {
		label, ok := labelNodesStatus[hostPriority.Host]
		if !ok {
			result[i].Host = hostPriority.Host
			result[i].Score = int(0)
			continue
		}
		// initializing to the default/max node score of maxPriority
		fScore := maxPriorityFloat64
		if numServicePods > 0 {
			fScore = maxPriorityFloat64 * (float64(numServicePods-podCounts[label]) / float64(numServicePods))
		}
		result[i].Host = hostPriority.Host
		result[i].Score = int(fScore)
	}

	return nil
}
