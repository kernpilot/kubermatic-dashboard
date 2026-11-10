/*
Copyright 2026 The Kubermatic Kubernetes Platform contributors.

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
	"slices"
	"testing"

	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// kubehz: a bring-your-own cluster has no machine-controller, so the user
// cluster serves no cluster.k8s.io API. getKubeletVersions must read that as
// "no machines" instead of failing the whole PATCH request.
func TestGetKubeletVersions(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clusterv1alpha1.AddToScheme(scheme))

	machineResource := schema.GroupResource{Group: clusterv1alpha1.GroupName, Resource: "machines"}
	machineDeploymentResource := schema.GroupResource{Group: clusterv1alpha1.GroupName, Resource: "machinedeployments"}
	machineKind := schema.GroupKind{Group: clusterv1alpha1.GroupName, Kind: "Machine"}
	podResource := schema.GroupResource{Group: "", Resource: "pods"}

	machine := &clusterv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: metav1.NamespaceSystem},
		Spec: clusterv1alpha1.MachineSpec{
			Versions: clusterv1alpha1.MachineVersionInfo{Kubelet: "9.9.9"},
		},
	}
	// ownedMachine belongs to a MachineSet and runs an old kubelet.
	ownedMachine := &clusterv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-owned",
			Namespace: metav1.NamespaceSystem,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1alpha1.SchemeGroupVersion.String(),
				Kind:       "MachineSet",
				Name:       "md-1-abc",
				UID:        "6a1c4b2e-0d1f-4a54-9b0e-3c2d1e0f9a87",
			}},
		},
		Spec: clusterv1alpha1.MachineSpec{
			Versions: clusterv1alpha1.MachineVersionInfo{Kubelet: "9.7.0"},
		},
	}
	// machineDeployment already asks for a newer kubelet than ownedMachine runs.
	machineDeployment := &clusterv1alpha1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "md-1", Namespace: metav1.NamespaceSystem},
		Spec: clusterv1alpha1.MachineDeploymentSpec{
			Template: clusterv1alpha1.MachineTemplateSpec{
				Spec: clusterv1alpha1.MachineSpec{
					Versions: clusterv1alpha1.MachineVersionInfo{Kubelet: "9.9.9"},
				},
			},
		},
	}

	testcases := []struct {
		name string
		// objects are the objects of the fake cluster; nil means machine only.
		objects []ctrlruntimeclient.Object
		// listErr, when set, is returned by the fake client for every list, or
		// only for the MachineDeployment list when machineDeploymentsOnly is
		// set.
		listErr                error
		machineDeploymentsOnly bool
		expectedVersions       []string
		expectedErr            bool
	}{
		{
			name:             "machines are listed when the API is served",
			expectedVersions: []string{"9.9.9"},
		},
		{
			name:    "a NotFound on the cluster.k8s.io group means no machines",
			listErr: apierrors.NewNotFound(machineResource, ""),
		},
		{
			name:    "a missing kind on the cluster.k8s.io group means no machines",
			listErr: &meta.NoKindMatchError{GroupKind: machineKind},
		},
		{
			name:                   "an absent MachineDeployment resource keeps the versions of the machines",
			listErr:                apierrors.NewNotFound(machineDeploymentResource, ""),
			machineDeploymentsOnly: true,
			expectedVersions:       []string{"9.9.9"},
		},
		{
			name:                   "an owned machine keeps its old kubelet version when the MachineDeployment resource is absent",
			objects:                []ctrlruntimeclient.Object{ownedMachine},
			listErr:                apierrors.NewNotFound(machineDeploymentResource, ""),
			machineDeploymentsOnly: true,
			expectedVersions:       []string{"9.7.0"},
		},
		{
			name:             "an owned machine counts next to the newer template of its MachineDeployment",
			objects:          []ctrlruntimeclient.Object{ownedMachine, machineDeployment},
			expectedVersions: []string{"9.7.0", "9.9.9"},
		},
		{
			name:        "a NotFound on another group is still an error",
			listErr:     apierrors.NewNotFound(podResource, "some-pod"),
			expectedErr: true,
		},
		{
			name:                   "a NotFound on another group in the MachineDeployment list is still an error",
			listErr:                apierrors.NewNotFound(podResource, "some-pod"),
			machineDeploymentsOnly: true,
			expectedErr:            true,
		},
		{
			name:                   "any other MachineDeployment list error is still an error",
			listErr:                errors.New("connection refused"),
			machineDeploymentsOnly: true,
			expectedErr:            true,
		},
		{
			name:        "a missing kind in another group is still an error",
			listErr:     &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}},
			expectedErr: true,
		},
		{
			name:        "any other list error is still an error",
			listErr:     errors.New("connection refused"),
			expectedErr: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			objects := tc.objects
			if objects == nil {
				objects = []ctrlruntimeclient.Object{machine}
			}
			builder := ctrlruntimefake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
			if tc.listErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, client ctrlruntimeclient.WithWatch, list ctrlruntimeclient.ObjectList, opts ...ctrlruntimeclient.ListOption) error {
						if _, isMD := list.(*clusterv1alpha1.MachineDeploymentList); tc.machineDeploymentsOnly && !isMD {
							return client.List(ctx, list, opts...)
						}
						return tc.listErr
					},
				})
			}

			versions, err := getKubeletVersions(context.Background(), builder.Build())
			if tc.expectedErr {
				if err == nil {
					t.Fatalf("expected an error, got versions %v", versions)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			slices.Sort(versions)
			if !slices.Equal(versions, tc.expectedVersions) {
				t.Fatalf("expected kubelet versions %v, got %v", tc.expectedVersions, versions)
			}
		})
	}
}

// kubehz: the kubehz.cloud/ annotations are the operator's. A request may
// carry them unchanged, and every other difference is reported by key.
func TestKubehzAnnotationChanges(t *testing.T) {
	t.Parallel()

	stored := map[string]string{
		KubehzNodepoolsAnnotation: KubehzNodepoolsOff,
		"kubehz.cloud/datastore":  "kine",
		"example.com/owner":       "team-a",
	}

	testcases := []struct {
		name      string
		stored    map[string]string
		requested map[string]string
		want      []string
	}{
		{
			name:      "the same annotations are no change",
			stored:    stored,
			requested: map[string]string{KubehzNodepoolsAnnotation: KubehzNodepoolsOff, "kubehz.cloud/datastore": "kine", "example.com/owner": "team-a"},
			want:      []string{},
		},
		{
			name:      "other annotations may change",
			stored:    stored,
			requested: map[string]string{KubehzNodepoolsAnnotation: KubehzNodepoolsOff, "kubehz.cloud/datastore": "kine", "example.com/owner": "team-b", "example.com/new": "x"},
			want:      []string{},
		},
		{
			name:      "an added kubehz annotation is a change",
			stored:    map[string]string{"example.com/owner": "team-a"},
			requested: map[string]string{"example.com/owner": "team-a", KubehzNodepoolsAnnotation: KubehzNodepoolsOff},
			want:      []string{KubehzNodepoolsAnnotation},
		},
		{
			name:      "a removed kubehz annotation is a change",
			stored:    stored,
			requested: map[string]string{"kubehz.cloud/datastore": "kine", "example.com/owner": "team-a"},
			want:      []string{KubehzNodepoolsAnnotation},
		},
		{
			name:      "a changed value is a change, every key is reported once, sorted",
			stored:    stored,
			requested: map[string]string{KubehzNodepoolsAnnotation: "on", "kubehz.cloud/etcd-snapshot-count": "1"},
			want:      []string{"kubehz.cloud/datastore", "kubehz.cloud/etcd-snapshot-count", KubehzNodepoolsAnnotation},
		},
		{
			name:      "nil maps are no change",
			stored:    nil,
			requested: nil,
			want:      []string{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := KubehzAnnotationChanges(tc.stored, tc.requested); !slices.Equal(got, tc.want) {
				t.Fatalf("KubehzAnnotationChanges() = %v, want %v", got, tc.want)
			}
		})
	}
}
