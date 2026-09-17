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

	machineGVR := schema.GroupResource{Group: "cluster.k8s.io", Resource: "machines"}
	machineDeploymentGVK := schema.GroupVersionKind{Group: "cluster.k8s.io", Version: "v1alpha1", Kind: "MachineDeployment"}

	machine := &clusterv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: metav1.NamespaceSystem},
		Spec: clusterv1alpha1.MachineSpec{
			Versions: clusterv1alpha1.MachineVersionInfo{Kubelet: "9.9.9"},
		},
	}

	testcases := []struct {
		name             string
		listErr          error
		expectedVersions []string
		expectedErr      bool
	}{
		{
			name:             "machines are listed when the API is served",
			expectedVersions: []string{"9.9.9"},
		},
		{
			name:    "a NotFound on the cluster.k8s.io group means no machines",
			listErr: apierrors.NewNotFound(machineGVR, ""),
		},
		{
			name:    "a missing kind on the cluster.k8s.io group means no machines",
			listErr: &meta.NoKindMatchError{GroupKind: machineDeploymentGVK.GroupKind()},
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

			builder := ctrlruntimefake.NewClientBuilder().WithScheme(scheme).WithObjects(machine)
			if tc.listErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(_ context.Context, _ ctrlruntimeclient.WithWatch, _ ctrlruntimeclient.ObjectList, _ ...ctrlruntimeclient.ListOption) error {
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
			if len(versions) != len(tc.expectedVersions) {
				t.Fatalf("expected kubelet versions %v, got %v", tc.expectedVersions, versions)
			}
			for i, want := range tc.expectedVersions {
				if versions[i] != want {
					t.Fatalf("expected kubelet versions %v, got %v", tc.expectedVersions, versions)
				}
			}
		})
	}
}
