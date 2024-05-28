package istio

import (
	"context"
	"fmt"
	"os"
	"testing"

	networkingv1alpha3 "istio.io/api/networking/v1alpha3"
	telemetryv1alpha1 "istio.io/api/telemetry/v1alpha1"
	apinetworkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
	apitelemetryv1alpha1 "istio.io/client-go/pkg/apis/telemetry/v1alpha1"
	istioscheme "istio.io/client-go/pkg/clientset/versioned/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

var testenv env.Environment

const (
	istioVersion                      string = "1.22.0"
	istioNamespace                    string = "istio-system"
	istioBaseChart                    string = "istio/base"
	istioBaseReleaseName              string = "istio-base"
	istiodChart                       string = "istio/istiod"
	istiodReleaseName                 string = "istiod"
	gatewayChart                      string = "istio/gateway"
	gatewayReleaseName                string = "gateway"
	gatewaySelectorKey                string = "istio"
	gatewaySelectorValue              string = "test-gateway"
	gatewayName                       string = "test-gateway"
	dependencyLearnerHostPath         string = "/dependency-learner"
	dependencyLearnerMountPath        string = "/dependency-learner"
	dependencyLearnerWasmRelativePath string = "target/wasm32-wasi/release/dependency_learner.wasm"
	dependencyLearnerVolumeName       string = "dependency-learner"
	nginxVersion                      string = "1.25.5"
)

func TestMain(m *testing.M) {
	testenv = env.New()
	kindClusterName := envconf.RandomName("test-cluster-istio", 16)

	// create a kind cluster prior to test run
	testenv.Setup(
		envfuncs.CreateClusterWithConfig(kind.NewProvider(), kindClusterName, "./cluster.yaml"),
		envfuncs.CreateNamespace(istioNamespace),

		// load images to kind. images must be pulled locally for these to succeed
		envfuncs.LoadDockerImageToCluster(kindClusterName, fmt.Sprintf("istio/proxyv2:%s", istioVersion)),
		envfuncs.LoadDockerImageToCluster(kindClusterName, fmt.Sprintf("istio/pilot:%s", istioVersion)),
		envfuncs.LoadDockerImageToCluster(kindClusterName, fmt.Sprintf("nginx:%s", nginxVersion)),

		func(ctx context.Context, c *envconf.Config) (context.Context, error) {
			manager := helm.New(c.KubeconfigFile())

			// pull istio repo
			err := manager.RunRepo(helm.WithArgs("add", "istio", "https://istio-release.storage.googleapis.com/charts"))
			if err != nil {
				return nil, fmt.Errorf("failed to add istio helm chart: %w", err)
			}
			err = manager.RunRepo(helm.WithArgs("update"))
			if err != nil {
				return nil, fmt.Errorf("failed to upgrade helm repo: %w", err)
			}

			// install istio base (e.g. CRDs)
			err = manager.RunInstall(
				helm.WithName(istioBaseReleaseName),
				helm.WithChart(istioBaseChart),
				helm.WithNamespace(istioNamespace),
				helm.WithVersion(istioVersion),
				helm.WithWait(),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to install %s: %w", istioBaseReleaseName, err)
			}

			// install istiod
			err = manager.RunInstall(
				helm.WithName(istiodReleaseName),
				helm.WithChart(istiodChart),
				helm.WithNamespace(istioNamespace),
				helm.WithVersion(istioVersion),
				helm.WithWait(),
				helm.WithArgs(
					"--set",
					"global.imagePullPolicy=IfNotPresent",
				),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to install %s: %w", istiodReleaseName, err)
			}

			// install gateway
			err = manager.RunInstall(
				helm.WithName(gatewayReleaseName),
				helm.WithChart(gatewayChart),
				helm.WithNamespace(istioNamespace),
				helm.WithVersion(istioVersion),
				helm.WithWait(),
				helm.WithArgs(
					"--set",
					fmt.Sprintf("name=%s", gatewayName),
					"--set",
					"service.type=ClusterIP",
					"--set",
					fmt.Sprintf("labels.%s=%s", gatewaySelectorKey, gatewaySelectorValue),
					"--set",
					fmt.Sprintf("volumes[0].name=%s", dependencyLearnerVolumeName),
					"--set",
					fmt.Sprintf("volumes[0].hostPath.path=%s", dependencyLearnerHostPath),
					"--set",
					fmt.Sprintf("volumeMounts[0].name=%s", dependencyLearnerVolumeName),
					"--set",
					fmt.Sprintf("volumeMounts[0].mountPath=%s", dependencyLearnerMountPath),
					"--set",
					"imagePullPolicy=IfNotPresent",
				),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to install %s: %w", gatewayReleaseName, err)
			}

			// create client
			r, err := resources.New(c.Client().RESTConfig())
			if err != nil {
				return nil, fmt.Errorf("failed to create resources client: %w", err)
			}
			if err := istioscheme.AddToScheme(r.GetScheme()); err != nil {
				return nil, fmt.Errorf("failed to add istio resources to scheme: %w", err)
			}

			// apply mtls for gateway
			gatewayDestRuleObj := &apinetworkingv1alpha3.DestinationRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gatewayName,
					Namespace: istioNamespace,
				},
				Spec: networkingv1alpha3.DestinationRule{
					Host: fmt.Sprintf("%s.%s.svc.cluster.local", gatewayName, istioNamespace),
					TrafficPolicy: &networkingv1alpha3.TrafficPolicy{
						Tls: &networkingv1alpha3.ClientTLSSettings{
							Mode: networkingv1alpha3.ClientTLSSettings_ISTIO_MUTUAL,
							Sni:  fmt.Sprintf("%s.%s.svc.cluster.local", gatewayName, istioNamespace),
						},
					},
				},
			}
			if err := r.Create(ctx, gatewayDestRuleObj); err != nil {
				return nil, fmt.Errorf("failed to create destination rule for gateway: %w", err)
			}

			// enable access logs
			defTelemetryObj := &apitelemetryv1alpha1.Telemetry{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mesh-default",
					Namespace: istioNamespace,
				},
				Spec: telemetryv1alpha1.Telemetry{
					AccessLogging: []*telemetryv1alpha1.AccessLogging{
						{
							Providers: []*telemetryv1alpha1.ProviderRef{
								{
									Name: "envoy",
								},
							},
						},
					},
				},
			}
			if err := r.Create(ctx, defTelemetryObj); err != nil {
				return nil, fmt.Errorf("failed to create default telemetry rule: %w", err)
			}
			return ctx, nil
		},
	)

	// teardown kind cluster after tests
	testenv.Finish(
		func(ctx context.Context, c *envconf.Config) (context.Context, error) {
			manager := helm.New(c.KubeconfigFile())

			// cleanup istio repo
			err := manager.RunRepo(helm.WithArgs("remove", "istio"))
			if err != nil {
				return nil, fmt.Errorf("failed to cleanum istio helm repo: %w", err)
			}
			return ctx, nil
		},
		envfuncs.DeleteNamespace(istioNamespace),
		envfuncs.DestroyCluster(kindClusterName),
	)

	// launch package tests
	os.Exit(testenv.Run(m))
}
