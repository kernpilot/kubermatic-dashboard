/*
Copyright 2020 The Kubermatic Kubernetes Platform contributors.

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

package common

import (
	"context"
	"errors"
	"fmt"
	"strings"

	semverlib "github.com/Masterminds/semver/v3"

	"k8c.io/dashboard/v2/pkg/provider"
	kubermaticv1 "k8c.io/kubermatic/sdk/v2/apis/kubermatic/v1"
	"k8c.io/kubermatic/v2/pkg/validation/nodeupdate"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KubehzNodepoolsAnnotation on a KKP Cluster marks how the kubehz KKP fork
	// renders the cluster's node side.
	KubehzNodepoolsAnnotation = "kubehz.cloud/nodepools"
	// KubehzNodepoolsOff is the KubehzNodepoolsAnnotation value for a
	// bring-your-own cluster: the fork renders no machine-controller and no
	// operating-system-manager, so the user cluster has no cluster.k8s.io API.
	KubehzNodepoolsOff = "off"
)

// HasNodepoolsOff reports whether the kubehz KKP fork renders the given
// cluster without a machine-controller. Such a cluster serves no cluster.k8s.io
// API, so a machine-based skew check against it fails, and it has no
// KKP-managed machine that can be skewed: the customer joins the nodes and owns
// their kubelet versions. The caller must read this from the STORED cluster,
// never from a cluster built out of a request body. The annotation is the
// operator's to write, and a patch body carries the annotations of whoever
// sends it.
func HasNodepoolsOff(cluster *kubermaticv1.Cluster) bool {
	return cluster.Annotations[KubehzNodepoolsAnnotation] == KubehzNodepoolsOff
}

// CheckClusterVersionSkew returns a list of machines and/or machine deployments
// that are running kubelet at a version incompatible with the cluster's control plane.
func CheckClusterVersionSkew(ctx context.Context, userInfoGetter provider.UserInfoGetter, clusterProvider provider.ClusterProvider, cluster *kubermaticv1.Cluster, projectID string) ([]string, error) {
	client, err := GetClusterClient(ctx, userInfoGetter, clusterProvider, cluster, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create a machine client: %w", err)
	}

	// get deduplicated list of all used kubelet versions
	kubeletVersions, err := getKubeletVersions(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("failed to get the list of kubelet versions used in the cluster: %w", err)
	}

	// this is where the incompatible versions shall be saved
	incompatibleVersionsSet := map[string]bool{}

	clusterVersion := cluster.Spec.Version.Semver()
	for _, ver := range kubeletVersions {
		kubeletVersion, parseErr := semverlib.NewVersion(ver)
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse kubelet version: %w", parseErr)
		}

		if err = nodeupdate.EnsureVersionCompatible(clusterVersion, kubeletVersion); err != nil {
			// VersionSkewError says it's incompatible
			if errors.Is(err, nodeupdate.VersionSkewError{}) {
				incompatibleVersionsSet[kubeletVersion.String()] = true
				continue
			}

			// other error types
			return nil, fmt.Errorf("failed to check compatibility between kubelet %q and control plane %q: %w", kubeletVersion, clusterVersion, err)
		}
	}

	// collect the deduplicated map entries into a slice
	var incompatibleVersionsList []string
	for ver := range incompatibleVersionsSet {
		incompatibleVersionsList = append(incompatibleVersionsList, ver)
	}

	return incompatibleVersionsList, nil
}

// machineAPIAbsent reports whether err says that the cluster.k8s.io API is not
// served by the user cluster. A cluster without a machine-controller has no
// Machine and no MachineDeployment resource: the apiserver answers NotFound,
// and the client answers NoKindMatch when its discovery already knows the group
// is gone. Both mean "no machines", not a failure.
//
// The group is part of the test. A NotFound from any other group says nothing
// about the machines, and reading it as "no machines" would pass the skew check
// on a cluster whose machines were never read.
func machineAPIAbsent(err error) bool {
	var noKindMatchErr *meta.NoKindMatchError
	if errors.As(err, &noKindMatchErr) {
		return noKindMatchErr.GroupKind.Group == clusterv1alpha1.GroupName
	}

	var statusErr *apierrors.StatusError
	if apierrors.IsNotFound(err) && errors.As(err, &statusErr) && statusErr.ErrStatus.Details != nil {
		return statusErr.ErrStatus.Details.Group == clusterv1alpha1.GroupName
	}

	return false
}

// getKubeletVersions returns the list of all kubelet versions used by a given cluster's Machines and MachineDeployments.
func getKubeletVersions(ctx context.Context, client ctrlruntimeclient.Client) ([]string, error) {
	// An absent resource leaves its list empty and the loops below read no
	// version from it. The other list still contributes what it holds.
	machineList := &clusterv1alpha1.MachineList{}
	if err := client.List(ctx, machineList); err != nil && !machineAPIAbsent(err) {
		return nil, fmt.Errorf("failed to load machines from cluster: %w", err)
	}

	machineDeployments := &clusterv1alpha1.MachineDeploymentList{}
	if err := client.List(ctx, machineDeployments); err != nil && !machineAPIAbsent(err) {
		return nil, KubernetesErrorToHTTPError(err)
	}

	kubeletVersionsSet := map[string]bool{}

	// first let's go through the legacy non-MD nodes
	for _, m := range machineList.Items {
		// Only list Machines that are not controlled, i.e. by Machine Set.
		if len(m.OwnerReferences) == 0 {
			ver := strings.TrimSpace(m.Spec.Versions.Kubelet)
			kubeletVersionsSet[ver] = true
		}
	}

	// now the deployments
	for _, md := range machineDeployments.Items {
		ver := strings.TrimSpace(md.Spec.Template.Spec.Versions.Kubelet)
		kubeletVersionsSet[ver] = true
	}

	// deduplicated list
	kubeletVersionList := []string{}
	for ver := range kubeletVersionsSet {
		kubeletVersionList = append(kubeletVersionList, ver)
	}

	return kubeletVersionList, nil
}
