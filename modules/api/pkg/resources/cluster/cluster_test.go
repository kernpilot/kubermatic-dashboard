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

package cluster

import (
	"context"
	"crypto/x509"
	"testing"

	"go.uber.org/zap"

	apiv1 "k8c.io/dashboard/v2/pkg/api/v1"
	kubermaticv1 "k8c.io/kubermatic/sdk/v2/apis/kubermatic/v1"
	"k8c.io/kubermatic/sdk/v2/semver"
	"k8c.io/kubermatic/v2/pkg/defaulting"
	"k8c.io/kubermatic/v2/pkg/features"
)

// The process-wide feature-gate map must NEVER leak writes between creates
// (kubehz B51): Spec used to alias the caller's map straight into
// spec.Features, so the encryptionAtRest handling here — and the create
// mutation's externalCloudProvider write downstream — poisoned the shared map
// for every later create served by the same process.
func TestSpecDoesNotAliasTheSharedFeatureGateMap(t *testing.T) {
	const dcName = "byo-dc"

	dc := &kubermaticv1.Datacenter{
		Spec: kubermaticv1.DatacenterSpec{
			BringYourOwn: &kubermaticv1.DatacenterSpecBringYourOwn{},
		},
	}
	seed := &kubermaticv1.Seed{
		Spec: kubermaticv1.SeedSpec{
			Datacenters: map[string]kubermaticv1.Datacenter{dcName: *dc},
		},
	}
	apiCluster := apiv1.Cluster{
		Spec: apiv1.ClusterSpec{
			Cloud: kubermaticv1.CloudSpec{
				DatacenterName: dcName,
				BringYourOwn:   &kubermaticv1.BringYourOwnCloudSpec{},
			},
			Version: *semver.NewSemverOrDie("1.33.7"),
			EncryptionConfiguration: &kubermaticv1.EncryptionConfiguration{
				Enabled: true,
			},
		},
	}

	config, err := defaulting.DefaultConfiguration(&kubermaticv1.KubermaticConfiguration{}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("defaulting the KubermaticConfiguration failed: %v", err)
	}

	shared := features.FeatureGate{"OIDCKubeCfgEndpoint": true, "OpenIDAuthPlugin": true}

	spec, _, err := Spec(context.Background(), apiCluster, nil, seed, dc, config, nil, x509.NewCertPool(), shared)
	if err != nil {
		t.Fatalf("Spec failed: %v", err)
	}

	// The per-cluster map got the request's feature...
	if !spec.Features["encryptionAtRest"] {
		t.Fatal("expected the returned spec to carry encryptionAtRest=true")
	}
	// ...but the shared map must be untouched by the request.
	if _, ok := shared["encryptionAtRest"]; ok {
		t.Fatal("the request's encryptionAtRest write leaked into the shared feature-gate map")
	}

	// Downstream mutation writes into spec.Features (MutateCreate sets
	// externalCloudProvider=true for CCM providers) — simulate one and prove
	// the shared map cannot be poisoned through the spec.
	spec.Features["externalCloudProvider"] = true
	if _, ok := shared["externalCloudProvider"]; ok {
		t.Fatal("a spec.Features write reached the shared feature-gate map — creates poison each other")
	}
	if len(shared) != 2 || !shared["OIDCKubeCfgEndpoint"] || !shared["OpenIDAuthPlugin"] {
		t.Fatalf("the shared feature-gate map changed: %v", shared)
	}

	// A later create must start from the clean gates, not the first create's
	// residue (the exact B51 poisoning shape).
	apiCluster.Spec.EncryptionConfiguration = nil
	spec2, _, err := Spec(context.Background(), apiCluster, nil, seed, dc, config, nil, x509.NewCertPool(), shared)
	if err != nil {
		t.Fatalf("second Spec failed: %v", err)
	}
	if _, ok := spec2.Features["externalCloudProvider"]; ok {
		t.Fatal("the second create inherited the first create's externalCloudProvider write")
	}
	if _, ok := spec2.Features["encryptionAtRest"]; ok {
		t.Fatal("the second create inherited the first create's encryptionAtRest write")
	}
}
