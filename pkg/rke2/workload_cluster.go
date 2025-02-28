/*
Copyright 2022 SUSE.

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

package rke2

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	controlplanev1 "github.com/rancher-sandbox/cluster-api-provider-rke2/controlplane/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubeProxyKey              = "kube-proxy"
	rke2ConfigKey             = "rke2-config"
	labelNodeRoleControlPlane = "node-role.kubernetes.io/master"
)

var (
	ErrControlPlaneMinNodes = errors.New("cluster has fewer than 2 control plane nodes; removing an etcd member is not supported")
)

// WorkloadCluster defines all behaviors necessary to upgrade kubernetes on a workload cluster
//
// TODO: Add a detailed description to each of these method definitions.
type WorkloadCluster interface {
	// Basic health and status checks.
	ClusterStatus(ctx context.Context) (ClusterStatus, error)
	UpdateAgentConditions(ctx context.Context, controlPlane *ControlPlane)
	UpdateEtcdConditions(ctx context.Context, controlPlane *ControlPlane)
	// Upgrade related tasks.

	//	RemoveEtcdMemberForMachine(ctx context.Context, machine *clusterv1.Machine) error

	//	ForwardEtcdLeadership(ctx context.Context, machine *clusterv1.Machine, leaderCandidate *clusterv1.Machine) error
	//	AllowBootstrapTokensToGetNodes(ctx context.Context) error

	// State recovery tasks.
	//	ReconcileEtcdMembers(ctx context.Context, nodeNames []string) ([]string, error)
}

// Workload defines operations on workload clusters.
type Workload struct {
	Client ctrlclient.Client
	//etcdClientGenerator etcdClientFor
}

// ClusterStatus holds stats information about the cluster.
type ClusterStatus struct {
	// Nodes are a total count of nodes
	Nodes int32
	// ReadyNodes are the count of nodes that are reporting ready
	ReadyNodes int32
}

func (w *Workload) getControlPlaneNodes(ctx context.Context) (*corev1.NodeList, error) {
	nodes := &corev1.NodeList{}
	labels := map[string]string{
		labelNodeRoleControlPlane: "true",
	}
	if err := w.Client.List(ctx, nodes, ctrlclient.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	return nodes, nil
}

// ClusterStatus returns the status of the cluster.
func (w *Workload) ClusterStatus(ctx context.Context) (ClusterStatus, error) {
	status := ClusterStatus{}

	// count the control plane nodes
	nodes, err := w.getControlPlaneNodes(ctx)
	if err != nil {
		return status, err
	}

	for _, node := range nodes.Items {
		nodeCopy := node
		status.Nodes++
		if util.IsNodeReady(&nodeCopy) {
			status.ReadyNodes++
		}
	}

	return status, nil
}

func hasProvisioningMachine(machines collections.Machines) bool {
	for _, machine := range machines {
		if machine.Status.NodeRef == nil {
			return true
		}
	}
	return false
}

// nodeHasUnreachableTaint returns true if the node has is unreachable from the node controller.
func nodeHasUnreachableTaint(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeUnreachable && taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// UpdateAgentConditions is responsible for updating machine conditions reflecting the status of all the control plane
// components running in a static pod generated by RKE2. This operation is best effort, in the sense that in case
// of problems in retrieving the pod status, it sets the condition to Unknown state without returning any error.
func (w *Workload) UpdateAgentConditions(ctx context.Context, controlPlane *ControlPlane) {
	allMachinePodConditions := []clusterv1.ConditionType{
		controlplanev1.MachineAgentHealthyCondition,
	}

	/** TODO: figure out etcd
	if controlPlane.IsEtcdManaged() {
		allMachinePodConditions = append(allMachinePodConditions, controlplanev1.MachineEtcdPodHealthyCondition)
	}
	**/

	// NOTE: this fun uses control plane nodes from the workload cluster as a source of truth for the current state.
	controlPlaneNodes, err := w.getControlPlaneNodes(ctx)
	if err != nil {
		//for i := range controlPlane.Machines {
		//machine := controlPlane.Machines[i]
		//for _, condition := range allMachinePodConditions {
		//conditions.MarkUnknown(machine, condition, controlplanev1.PodInspectionFailedReason, "Failed to get the node which is hosting this component")
		//}
		//}
		conditions.MarkUnknown(controlPlane.RCP, controlplanev1.ControlPlaneComponentsHealthyCondition, controlplanev1.ControlPlaneComponentsInspectionFailedReason, "Failed to list nodes which are hosting control plane components")
		return
	}

	// Update conditions for control plane components hosted as static pods on the nodes.
	var rcpErrors []string

	for _, node := range controlPlaneNodes.Items {
		// Search for the machine corresponding to the node.
		var machine *clusterv1.Machine
		for _, m := range controlPlane.Machines {
			if m.Status.NodeRef != nil && m.Status.NodeRef.Name == node.Name {
				machine = m
				break
			}
		}

		// If there is no machine corresponding to a node, determine if this is an error or not.
		if machine == nil {
			// If there are machines still provisioning there is the chance that a chance that a node might be linked to a machine soon,
			// otherwise report the error at RCP level given that there is no machine to report on.
			if hasProvisioningMachine(controlPlane.Machines) {
				continue
			}
			rcpErrors = append(rcpErrors, fmt.Sprintf("Control plane node %s does not have a corresponding machine", node.Name))
			continue
		}

		// If the machine is deleting, report all the conditions as deleting
		if !machine.ObjectMeta.DeletionTimestamp.IsZero() {
			for _, condition := range allMachinePodConditions {
				conditions.MarkFalse(machine, condition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
			}
			continue
		}

		// If the node is Unreachable, information about static pods could be stale so set all conditions to unknown.
		if nodeHasUnreachableTaint(node) {
			// NOTE: We are assuming unreachable as a temporary condition, leaving to MHC
			// the responsibility to determine if the node is unhealthy or not.
			for _, condition := range allMachinePodConditions {
				conditions.MarkUnknown(machine, condition, controlplanev1.PodInspectionFailedReason, "Node is unreachable")
			}
			continue
		}

		targetnode := corev1.Node{}
		nodeKey := ctrlclient.ObjectKey{
			Namespace: metav1.NamespaceSystem,
			Name:      node.Name,
		}

		if err := w.Client.Get(ctx, nodeKey, &targetnode); err != nil {
			// If there is an error getting the Pod, do not set any conditions.
			if apierrors.IsNotFound(err) {
				conditions.MarkFalse(machine, controlplanev1.MachineAgentHealthyCondition, controlplanev1.PodMissingReason, clusterv1.ConditionSeverityError, "Node %s is missing", nodeKey.Name)

				return
			}
			conditions.MarkUnknown(machine, controlplanev1.MachineAgentHealthyCondition, controlplanev1.PodInspectionFailedReason, "Failed to get node status")
			return
		}

		for _, condition := range targetnode.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				conditions.MarkTrue(machine, controlplanev1.MachineAgentHealthyCondition)
			}
		}

	}

	// If there are provisioned machines without corresponding nodes, report this as a failing conditions with SeverityError.
	for i := range controlPlane.Machines {
		machine := controlPlane.Machines[i]
		if machine.Status.NodeRef == nil {
			continue
		}
		found := false
		for _, node := range controlPlaneNodes.Items {
			if machine.Status.NodeRef.Name == node.Name {
				found = true
				break
			}
		}
		if !found {
			for _, condition := range allMachinePodConditions {
				conditions.MarkFalse(machine, condition, controlplanev1.PodFailedReason, clusterv1.ConditionSeverityError, "Missing node")
			}
		}
	}

	// Aggregate components error from machines at RCP level.
	aggregateFromMachinesToRCP(aggregateFromMachinesToRCPInput{
		controlPlane:      controlPlane,
		machineConditions: allMachinePodConditions,
		rcpErrors:         rcpErrors,
		condition:         controlplanev1.ControlPlaneComponentsHealthyCondition,
		unhealthyReason:   controlplanev1.ControlPlaneComponentsUnhealthyReason,
		unknownReason:     controlplanev1.ControlPlaneComponentsUnknownReason,
		note:              "control plane",
	})
}

// updateStaticPodCondition is responsible for updating machine conditions reflecting the status of a component running
// in a static pod generated by RKE2. This operation is best effort, in the sense that in case of problems
// in retrieving the pod status, it sets the condition to Unknown state without returning any error.
//func (w *Workload) updateStaticPodCondition(ctx context.Context, machine *clusterv1.Machine, node corev1.Node, component string, staticPodCondition clusterv1.ConditionType) {
//// not really needed for rke2 so just set true
//conditions.MarkTrue(machine, staticPodCondition)
//}

type aggregateFromMachinesToRCPInput struct {
	controlPlane      *ControlPlane
	machineConditions []clusterv1.ConditionType
	rcpErrors         []string
	condition         clusterv1.ConditionType
	unhealthyReason   string
	unknownReason     string
	note              string
}

// aggregateFromMachinesToRCP aggregates a group of conditions from machines to RCP.
// NOTE: this func follows the same aggregation rules used by conditions.Merge thus giving priority to
// errors, then warning, info down to unknown.
func aggregateFromMachinesToRCP(input aggregateFromMachinesToRCPInput) {
	// Aggregates machines for condition status.
	// NB. A machine could be assigned to many groups, but only the group with the highest severity will be reported.
	rcpMachinesWithErrors := sets.NewString()
	rcpMachinesWithWarnings := sets.NewString()
	rcpMachinesWithInfo := sets.NewString()
	rcpMachinesWithTrue := sets.NewString()
	rcpMachinesWithUnknown := sets.NewString()

	for i := range input.controlPlane.Machines {
		machine := input.controlPlane.Machines[i]
		for _, condition := range input.machineConditions {
			if machineCondition := conditions.Get(machine, condition); machineCondition != nil {
				switch machineCondition.Status {
				case corev1.ConditionTrue:
					rcpMachinesWithTrue.Insert(machine.Name)
				case corev1.ConditionFalse:
					switch machineCondition.Severity {
					case clusterv1.ConditionSeverityInfo:
						rcpMachinesWithInfo.Insert(machine.Name)
					case clusterv1.ConditionSeverityWarning:
						rcpMachinesWithWarnings.Insert(machine.Name)
					case clusterv1.ConditionSeverityError:
						rcpMachinesWithErrors.Insert(machine.Name)
					}
				case corev1.ConditionUnknown:
					rcpMachinesWithUnknown.Insert(machine.Name)
				}
			}
		}
	}

	// In case of at least one machine with errors or RCP level errors (nodes without machines), report false, error.
	if len(rcpMachinesWithErrors) > 0 {
		input.rcpErrors = append(input.rcpErrors, fmt.Sprintf("Following machines are reporting %s errors: %s", input.note, strings.Join(rcpMachinesWithErrors.List(), ", ")))
	}
	if len(input.rcpErrors) > 0 {
		conditions.MarkFalse(input.controlPlane.RCP, input.condition, input.unhealthyReason, clusterv1.ConditionSeverityError, strings.Join(input.rcpErrors, "; "))
		return
	}

	// In case of no errors and at least one machine with warnings, report false, warnings.
	if len(rcpMachinesWithWarnings) > 0 {
		conditions.MarkFalse(input.controlPlane.RCP, input.condition, input.unhealthyReason, clusterv1.ConditionSeverityWarning, "Following machines are reporting %s warnings: %s", input.note, strings.Join(rcpMachinesWithWarnings.List(), ", "))
		return
	}

	// In case of no errors, no warning, and at least one machine with info, report false, info.
	if len(rcpMachinesWithWarnings) > 0 {
		conditions.MarkFalse(input.controlPlane.RCP, input.condition, input.unhealthyReason, clusterv1.ConditionSeverityWarning, "Following machines are reporting %s info: %s", input.note, strings.Join(rcpMachinesWithInfo.List(), ", "))
		return
	}

	// In case of no errors, no warning, no Info, and at least one machine with true conditions, report true.
	if len(rcpMachinesWithTrue) > 0 {
		conditions.MarkTrue(input.controlPlane.RCP, input.condition)
		return
	}

	// Otherwise, if there is at least one machine with unknown, report unknown.
	if len(rcpMachinesWithUnknown) > 0 {
		conditions.MarkUnknown(input.controlPlane.RCP, input.condition, input.unknownReason, "Following machines are reporting unknown %s status: %s", input.note, strings.Join(rcpMachinesWithUnknown.List(), ", "))
		return
	}

	// This last case should happen only if there are no provisioned machines, and thus without conditions.
	// So there will be no condition at RCP level too.
}

// UpdateEtcdConditions is responsible for updating machine conditions reflecting the status of all the etcd members.
// This operation is best effort, in the sense that in case of problems in retrieving member status, it sets
// the condition to Unknown state without returning any error.
func (w *Workload) UpdateEtcdConditions(ctx context.Context, controlPlane *ControlPlane) {
	w.updateManagedEtcdConditions(ctx, controlPlane)
}

func (w *Workload) updateManagedEtcdConditions(ctx context.Context, controlPlane *ControlPlane) {
	// NOTE: This methods uses control plane nodes only to get in contact with etcd but then it relies on etcd
	// as ultimate source of truth for the list of members and for their health.
	controlPlaneNodes, err := w.getControlPlaneNodes(ctx)
	if err != nil {
		conditions.MarkUnknown(controlPlane.RCP, controlplanev1.EtcdClusterHealthyCondition, controlplanev1.EtcdClusterInspectionFailedReason, "Failed to list nodes which are hosting the etcd members")
		for _, m := range controlPlane.Machines {
			conditions.MarkUnknown(m, controlplanev1.MachineEtcdMemberHealthyCondition, controlplanev1.EtcdMemberInspectionFailedReason, "Failed to get the node which is hosting the etcd member")
		}
		return
	}

	for _, node := range controlPlaneNodes.Items {
		var machine *clusterv1.Machine
		for _, m := range controlPlane.Machines {
			if m.Status.NodeRef != nil && m.Status.NodeRef.Name == node.Name {
				machine = m
			}
		}

		if machine == nil {
			// If there are machines still provisioning there is the chance that a chance that a node might be linked to a machine soon,
			// otherwise report the error at RCP level given that there is no machine to report on.
			if hasProvisioningMachine(controlPlane.Machines) {
				continue
			}
			continue
		}

		// If the machine is deleting, report all the conditions as deleting
		if !machine.ObjectMeta.DeletionTimestamp.IsZero() {
			conditions.MarkFalse(machine, controlplanev1.MachineEtcdMemberHealthyCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
			continue
		}

		conditions.MarkTrue(machine, controlplanev1.MachineEtcdMemberHealthyCondition)
	}

}
