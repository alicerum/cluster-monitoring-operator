// Copyright 2020 The Cluster Monitoring Operator Authors
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

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-monitoring-operator/test/e2e/framework"
	"github.com/pkg/errors"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clusterMonitorConfigMapName = "cluster-monitoring-config"
)

func TestClusterMonitoringOperatorConfiguration(t *testing.T) {
	// Enable user workload monitoring to assess that an invalid configuration
	// doesn't rollback the last known and valid configuration.
	validCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-monitoring-config",
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `enableUserWorkload: true
`,
		},
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(validCM); err != nil {
		t.Fatal(err)
	}

	err := framework.Poll(time.Second, 5*time.Minute, func() error {
		_, err := f.KubeClient.AppsV1().StatefulSets(f.UserWorkloadMonitoringNs).Get(f.Ctx, "prometheus-user-workload", metav1.GetOptions{})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("asserting that CMO is healthy")
	assertOperatorCondition(t, configv1.OperatorDegraded, configv1.ConditionFalse)
	assertOperatorCondition(t, configv1.OperatorAvailable, configv1.ConditionTrue)

	// Push an invalid configuration.
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterMonitorConfigMapName,
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `cannot be deserialized`,
		},
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(cm); err != nil {
		t.Fatal(err)
	}

	t.Log("asserting that CMO goes degraded after an invalid configuration is pushed")
	assertOperatorCondition(t, configv1.OperatorDegraded, configv1.ConditionTrue)
	assertOperatorCondition(t, configv1.OperatorAvailable, configv1.ConditionFalse)
	// Check that the previous setup hasn't been reverted
	_, err = f.KubeClient.AppsV1().StatefulSets(f.UserWorkloadMonitoringNs).Get(f.Ctx, "prometheus-user-workload", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Restore the first configuration.
	if err := f.OperatorClient.CreateOrUpdateConfigMap(validCM); err != nil {
		t.Fatal(err)
	}

	t.Log("asserting that CMO goes back healthy after the configuration is fixed")
	assertOperatorCondition(t, configv1.OperatorDegraded, configv1.ConditionFalse)
	assertOperatorCondition(t, configv1.OperatorAvailable, configv1.ConditionTrue)
}

func assertOperatorCondition(t *testing.T, conditionType configv1.ClusterStatusConditionType, conditionStatus configv1.ConditionStatus) {
	t.Helper()

	reporter := f.OperatorClient.StatusReporter()
	err := framework.Poll(time.Second, 5*time.Minute, func() error {
		co, err := reporter.Get()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range co.Status.Conditions {
			if c.Type == conditionType {
				if c.Status == conditionStatus {
					return nil
				}
				return errors.Errorf("expecting condition %q to be %q, got %q", conditionType, conditionStatus, c.Status)
			}
		}
		return errors.Errorf("failed to find condition %q", conditionType)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClusterMonitorPrometheusOperatorConfig(t *testing.T) {
	const (
		component     = "prom-operator"
		containerName = "prometheus-operator"
	)

	data := `prometheusOperator:
  logLevel: info
  tolerations:
    - operator: "Exists"
`
	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "assert pod configuration is as expected",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/name=prometheus-operator",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectContainerArg("--log-level=info", containerName),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorPrometheusK8Config(t *testing.T) {
	const (
		component       = "prom-k8s"
		pvcClaimName    = "prometheus-k8s-db-prometheus-k8s-0"
		statefulsetName = "prometheus-k8s"
		cpu             = "1m"
		mem             = "3Mi"
		storage         = "2Gi"
		podName         = "prometheus-k8s-0"
		containerName   = "prometheus"
		labelSelector   = "app.kubernetes.io/component=prometheus"
		crName          = "k8s"
	)

	data := fmt.Sprintf(`prometheusK8s:
  logLevel: debug
  retention: 10h
  tolerations:
    - operator: "Exists"
  externalLabels:
    datacenter: eu-west
  remoteWrite:
  - url: "https://test.remotewrite.com/api/write"
  volumeClaimTemplate:
    spec:
      resources:
        requests:
          storage: %s
  resources:
    requests:
      cpu: %s
      memory: %s
`, storage, cpu, mem)

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "set configurations for prom operator CR, assert that PVC is created",
			f: assertVolumeClaimsConfigAndRollout(rolloutParams{
				component:       component,
				namespace:       f.Ns,
				claimName:       pvcClaimName,
				statefulSetName: statefulsetName,
			}),
		},
		{
			name: "assert that resource requests are created",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: labelSelector,
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectMatchingRequests(podName, containerName, mem, cpu),
					expectContainerArg("--log.level=debug", containerName),
					expectContainerArg("--storage.tsdb.retention.time=10h", containerName),
				},
			),
		},
		{
			name: "assert external labels are present on the CR",
			f:    assertExternalLabelExists(f.Ns, crName, "datacenter", "eu-west"),
		},
		{
			name: "assert remote write url value in set in CR",
			f:    assertRemoteWriteWasSet(f.Ns, crName, "https://test.remotewrite.com/api/write"),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorAlertManagerConfig(t *testing.T) {
	const (
		component       = "alertmanager"
		pvcClaimName    = "alertmanager-main-db-alertmanager-main-0"
		statefulsetName = "alertmanager-main"
		cpu             = "10m"
		mem             = "13Mi"
		storage         = "2Gi"
		podName         = "alertmanager-main-0"
		containerName   = "alertmanager"
		labelSelector   = "alertmanager=main"
	)

	data := fmt.Sprintf(`alertmanagerMain:
  resources:
    requests:
      cpu: %s
      memory: %s
  volumeClaimTemplate:
    spec:
      resources:
        requests:
          storage: %s
  tolerations:
    - operator: "Exists"
`, cpu, mem, storage)

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "set configurations for alert manager CR, assert that PVC is created",
			f: assertVolumeClaimsConfigAndRollout(rolloutParams{
				component:       component,
				namespace:       f.Ns,
				claimName:       pvcClaimName,
				statefulSetName: statefulsetName,
			}),
		},
		{
			name: "assert that resource requests are created",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: labelSelector,
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectMatchingRequests(podName, containerName, mem, cpu),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorKSMConfig(t *testing.T) {
	const (
		component = "kube-state-metrics"
	)

	data := `kubeStateMetrics:
  tolerations:
    - operator: "Exists"
`

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the kube-state-metrics deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that resource requests are correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/name=kube-state-metrics",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorOSMConfig(t *testing.T) {
	const (
		component = "openshift-state-metrics"
	)

	data := `openshiftStateMetrics:
  tolerations:
    - operator: "Exists"
`

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the openshift-state-metrics deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that resource requests are correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "k8s-app=openshift-state-metrics",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorGrafanaConfig(t *testing.T) {
	const (
		component = "grafana"
	)

	data := `grafana:
  tolerations:
    - operator: "Exists"
`

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the grafana deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that resource requests are correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/component=grafana",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorTelemeterClientConfig(t *testing.T) {
	const (
		component = "telemeter-client"
	)

	data := `telemeterClient:
  tolerations:
    - operator: "Exists"
`

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the telemeter-client deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that pod config correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/component=grafana",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorK8sPromAdapterConfig(t *testing.T) {
	const (
		component = "prometheus-adapter"
	)

	data := `k8sPrometheusAdapter:
  tolerations:
    - operator: "Exists"
`

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the prometheus-adapter deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that pod config is correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/component=metrics-adapter",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestClusterMonitorThanosQuerierConfig(t *testing.T) {
	const (
		component     = "thanos-querier"
		containerName = "thanos-query"
		cpu           = "1m"
		mem           = "3Mi"
	)

	data := fmt.Sprintf(`thanosQuerier:
  logLevel: debug
  tolerations:
    - operator: "Exists"
  resources:
    requests:
      cpu: %s
      memory: %s
`, cpu, mem)

	if err := f.OperatorClient.CreateOrUpdateConfigMap(updateConfigMap(t, data)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "test the thanos-querier deployment is rolled out",
			f: assertDeploymentRollout(deploymentRolloutParams{
				namespace: f.Ns,
				name:      component,
			}),
		},
		{
			name: "assert that pod config is correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.Ns,
					labelSelector: "app.kubernetes.io/name=thanos-query",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectMatchingRequests("*", containerName, mem, cpu),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestUserWorkloadMonitorPromOperatorConfig(t *testing.T) {
	const (
		component     = "prom-operator"
		containerName = "prometheus-operator"
	)
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-monitoring-config",
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `enableUserWorkload: true
`,
		},
	}

	uwmCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-workload-monitoring-config",
			Namespace: f.UserWorkloadMonitoringNs,
		},
		Data: map[string]string{
			"config.yaml": `prometheusOperator:
  logLevel: debug
  tolerations:
    - operator: "Exists"
`,
		},
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(cm); err != nil {
		t.Fatal(err)
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(uwmCM); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "assert pod configuration is as expected",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.UserWorkloadMonitoringNs,
					labelSelector: "app.kubernetes.io/name=prometheus-operator",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectContainerArg("--log-level=debug", containerName),
				},
			),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestUserWorkloadMonitorPrometheusK8Config(t *testing.T) {
	const (
		component       = "prom-k8s"
		pvcClaimName    = "prometheus-user-workload-db-prometheus-user-workload-0"
		statefulsetName = "prometheus-user-workload"
		cpu             = "1m"
		mem             = "3Mi"
		storage         = "2Gi"
		podName         = "prometheus-user-workload-0"
		containerName   = "prometheus"
		labelSelector   = "app.kubernetes.io/component=prometheus"
		crName          = "user-workload"
	)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-monitoring-config",
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `enableUserWorkload: true
`,
		},
	}

	uwmCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-workload-monitoring-config",
			Namespace: f.UserWorkloadMonitoringNs,
		},
		Data: map[string]string{
			"config.yaml": fmt.Sprintf(`prometheus:
  logLevel: debug
  retention: 10h
  tolerations:
    - operator: "Exists"
  externalLabels:
    datacenter: eu-west
  remoteWrite:
  - url: "https://test.remotewrite.com/api/write"
  volumeClaimTemplate:
    spec:
      resources:
        requests:
          storage: %s
  resources:
    requests:
      cpu: %s
      memory: %s
`, storage, cpu, mem),
		},
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(cm); err != nil {
		t.Fatal(err)
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(uwmCM); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "set configurations for prom CR, assert that PVC is created",
			f: assertVolumeClaimsConfigAndRollout(rolloutParams{
				component:       component,
				namespace:       f.UserWorkloadMonitoringNs,
				claimName:       pvcClaimName,
				statefulSetName: statefulsetName,
			}),
		},
		{
			name: "assert that resource requests are created",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.UserWorkloadMonitoringNs,
					labelSelector: labelSelector,
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectMatchingRequests(podName, containerName, mem, cpu),
					expectContainerArg("--log.level=debug", containerName),
					expectContainerArg("--storage.tsdb.retention.time=10h", containerName),
				},
			),
		},
		{
			name: "assert external labels are present on the CR",
			f:    assertExternalLabelExists(f.UserWorkloadMonitoringNs, crName, "datacenter", "eu-west"),
		},
		{
			name: "assert remote write url value in set in CR",
			f:    assertRemoteWriteWasSet(f.UserWorkloadMonitoringNs, crName, "https://test.remotewrite.com/api/write"),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

func TestUserWorkloadMonitorThanosRulerConfig(t *testing.T) {
	const (
		component       = "thanos-ruler"
		pvcClaimName    = "thanos-ruler-user-workload-data-thanos-ruler-user-workload-0"
		statefulsetName = "thanos-ruler-user-workload"
		cpu             = "1m"
		mem             = "3Mi"
		storage         = "2Gi"
	)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-monitoring-config",
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `enableUserWorkload: true
`,
		},
	}

	uwmCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-workload-monitoring-config",
			Namespace: f.UserWorkloadMonitoringNs,
		},
		Data: map[string]string{
			"config.yaml": fmt.Sprintf(`thanosRuler:
  logLevel: debug
  tolerations:
    - operator: "Exists"
  volumeClaimTemplate:
    spec:
      resources:
        requests:
          storage: %s
  resources:
    requests:
      cpu: %s
      memory: %s
`, storage, cpu, mem),
		},
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(cm); err != nil {
		t.Fatal(err)
	}

	if err := f.OperatorClient.CreateOrUpdateConfigMap(uwmCM); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		f    func(*testing.T)
	}{
		{
			name: "assert that PVC is created and ss rolled out",
			f: assertVolumeClaimsConfigAndRollout(rolloutParams{
				component:       component,
				namespace:       f.UserWorkloadMonitoringNs,
				claimName:       pvcClaimName,
				statefulSetName: statefulsetName,
			}),
		},
		{
			name: "assert that pod config is correct",
			f: assertPodConfiguration(
				podConfigParams{
					component:     component,
					namespace:     f.UserWorkloadMonitoringNs,
					labelSelector: "app.kubernetes.io/name=thanos-ruler",
				},
				[]podAssertionCB{
					expectCatchAllToleration(),
					expectMatchingRequests("*", component, mem, cpu),
				},
			),
		},
		{
			name: "assert assets are deleted when user workload monitoring is disabled",
			f:    assertDeletedUserWorkloadAssets(cm),
		},
	} {
		if ok := t.Run(scenario.name, scenario.f); !ok {
			t.Fatalf("scenario %q failed", scenario.name)
		}
	}
}

type deploymentRolloutParams struct {
	namespace, name string
}

func assertDeploymentRollout(params deploymentRolloutParams) func(*testing.T) {
	return func(t *testing.T) {
		err := f.OperatorClient.WaitForDeploymentRollout(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      params.name,
				Namespace: params.namespace,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type rolloutParams struct {
	component, namespace, claimName, statefulSetName string
}

func assertVolumeClaimsConfigAndRollout(params rolloutParams) func(*testing.T) {
	return func(t *testing.T) {
		// Wait for persistent volume claim
		err := framework.Poll(time.Second, 5*time.Minute, func() error {
			_, err := f.KubeClient.CoreV1().PersistentVolumeClaims(params.namespace).Get(context.TODO(), params.claimName, metav1.GetOptions{})
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("getting %s persistent volume claim failed", params.component))

			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		err = framework.Poll(time.Second, 5*time.Minute, func() error {
			_, err := f.KubeClient.AppsV1().StatefulSets(params.namespace).Get(context.TODO(), params.statefulSetName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			t.Fatal(err)
		}

		err = f.OperatorClient.WaitForStatefulsetRollout(&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      params.statefulSetName,
				Namespace: params.namespace,
			},
		})

		if err != nil {
			t.Fatal(err)
		}
	}
}

// podConfigParams sets pod metadata
type podConfigParams struct {
	component, namespace, labelSelector string
}

func assertPodConfiguration(params podConfigParams, asserts []podAssertionCB) func(*testing.T) {
	return func(t *testing.T) {
		err := framework.Poll(time.Second, 5*time.Minute, func() error {
			pods, err := f.KubeClient.CoreV1().Pods(params.namespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: params.labelSelector,
				FieldSelector: "status.phase=Running"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("getting %s pods failed", params.component))
			}

			// for each pod in the list of matching labels run each assertion
			for _, p := range pods.Items {
				for _, assertion := range asserts {
					if err := assertion(p); err != nil {
						return fmt.Errorf("failed assertion for "+params.component, err)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func updateConfigMap(t *testing.T, addData string) *v1.ConfigMap {
	t.Helper()
	cm, err := f.OperatorClient.GetConfigmap(f.Ns, clusterMonitorConfigMapName)
	if err != nil {
		t.Fatal("failed to get required configMap", err)
	}
	if cm == nil {
		emptyConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterMonitorConfigMapName,
				Namespace: f.Ns,
			},
			Data: map[string]string{
				"config.yaml": addData,
			},
		}

		return emptyConfigMap
	}
	cm.Data["config.yaml"] = addData
	return cm
}

type podAssertionCB func(pod v1.Pod) error

// checks that the toleration is set accordingly
// this toleration will match all so will not affect rolling out workloads
func expectCatchAllToleration() podAssertionCB {
	return func(pod v1.Pod) error {
		var hasToleration bool
		for _, toleration := range pod.Spec.Tolerations {
			if toleration.Operator == "Exists" {
				hasToleration = true
				break
			}
		}

		if !hasToleration {
			return fmt.Errorf("expected 'Exists' operator toleration but found none")
		}
		return nil
	}
}

// checks that the container name has the same request cpu,mem as expected
// pass "*" as podName t match all
func expectMatchingRequests(podName, containerName, expectMem, expectCPU string) podAssertionCB {
	return func(pod v1.Pod) error {
		if podName == "*" || pod.Name == podName {
			for _, container := range pod.Spec.Containers {
				if container.Name == containerName {
					containerMemory := container.Resources.Requests[v1.ResourceMemory]
					actualMemory := containerMemory.String()
					if actualMemory != expectMem {
						return fmt.Errorf("memory requests %s does not match actual %s", expectMem, actualMemory)
					}
					containerCPU := container.Resources.Requests[v1.ResourceCPU]
					actualCPU := containerCPU.String()
					if actualCPU != expectCPU {
						return fmt.Errorf("CPU requests %s does not match actual %s", expectCPU, actualCPU)
					}
				}
			}
		}
		return nil
	}
}

func expectContainerArg(arg string, containerName string) podAssertionCB {
	return func(pod v1.Pod) error {
		for _, container := range pod.Spec.Containers {
			if container.Name == containerName {
				for _, a := range container.Args {
					if a == arg {
						return nil
					}
				}
				return fmt.Errorf("arg %s not propagated from manifest", arg)
			}
		}
		return nil
	}
}

func assertExternalLabelExists(namespace, crName, expectKey, expectValue string) func(t *testing.T) {
	return func(t *testing.T) {
		err := framework.Poll(time.Second, time.Minute*5, func() error {
			prom, err := f.MonitoringClient.Prometheuses(namespace).Get(context.Background(), crName, metav1.GetOptions{})
			if err != nil {
				t.Fatal("failed to get required prometheus cr", err)
			}

			if prom.Spec.ExternalLabels == nil {
				return fmt.Errorf("external labels map is nil")
			}

			gotValue, ok := prom.Spec.ExternalLabels[expectKey]
			if !ok {
				return fmt.Errorf("expected key %s is missing", expectKey)
			}

			if gotValue != expectValue {
				return fmt.Errorf("expected value %s but got %s", expectValue, gotValue)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func assertRemoteWriteWasSet(namespace, crName, urlValue string) func(t *testing.T) {
	return func(t *testing.T) {
		err := framework.Poll(time.Second, time.Minute*5, func() error {
			prom, err := f.MonitoringClient.Prometheuses(namespace).Get(context.Background(), crName, metav1.GetOptions{})
			if err != nil {
				t.Fatal("failed to get required prometheus cr", err)
			}

			if len(prom.Spec.RemoteWrite) == 0 {
				return fmt.Errorf("remote write spec not set")
			}

			for _, gotValue := range prom.Spec.RemoteWrite {
				if gotValue.URL == urlValue {
					return nil
				}
			}
			return fmt.Errorf("expected remote write url value not found")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
