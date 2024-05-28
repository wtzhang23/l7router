package istio

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/structpb"
	extensionsv1alpha1 "istio.io/api/extensions/v1alpha1"
	networkingv1alpha3 "istio.io/api/networking/v1alpha3"
	typev1beta1 "istio.io/api/type/v1beta1"
	apiextensionsv1alpha1 "istio.io/client-go/pkg/apis/extensions/v1alpha1"
	apinetworkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
	istioscheme "istio.io/client-go/pkg/clientset/versioned/scheme"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const dependencyLearnerComponentLabelValue = "dependency-learner"

func TestDependencyLearner(t *testing.T) {
	clientNamespace := envconf.RandomName("client", 16)
	serverNamespace := envconf.RandomName("server", 16)
	clientName := "client"
	serverName := "server"
	containerName := "testapp"
	fallbackName := envconf.RandomName("fallback", 16)
	responseHeader := "detected-dependency"
	determineDependency := features.New("determine dependency").
		WithLabel("component", dependencyLearnerComponentLabelValue).
		Setup(
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				// create resources client
				r, err := resources.New(c.Client().RESTConfig())
				if !assert.NoError(t, err) {
					return ctx
				}
				if err := istioscheme.AddToScheme(r.GetScheme()); !assert.NoError(t, err) {
					return ctx
				}

				// create client namespace
				clientNamespaceObj := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: clientNamespace,
						Labels: map[string]string{
							"istio-injection": "enabled",
						},
					},
				}
				if err := r.Create(ctx, clientNamespaceObj); !assert.NoError(t, err) {
					return ctx
				}

				// create server namespace
				serverNamespaceObj := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: serverNamespace,
						Labels: map[string]string{
							"istio-injection": "enabled",
						},
					},
				}
				if err := r.Create(ctx, serverNamespaceObj); !assert.NoError(t, err) {
					return ctx
				}

				// setup gateway
				fallbackGatewayObj := &apinetworkingv1alpha3.Gateway{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
					Spec: networkingv1alpha3.Gateway{
						Servers: []*networkingv1alpha3.Server{
							{
								Name: fallbackName,
								Hosts: []string{
									"*.svc",
									"*.svc.cluster.local",
								},
								Port: &networkingv1alpha3.Port{
									Number:   443,
									Protocol: "https",
									Name:     "https",
								},
								Tls: &networkingv1alpha3.ServerTLSSettings{
									Mode: networkingv1alpha3.ServerTLSSettings_ISTIO_MUTUAL,
								},
							},
						},
						Selector: map[string]string{
							gatewaySelectorKey: gatewaySelectorValue,
						},
					},
				}
				if err := r.Create(ctx, fallbackGatewayObj); !assert.NoError(t, err) {
					return ctx
				}

				// setup virtual service redirect
				fallbackVsObj := &apinetworkingv1alpha3.VirtualService{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
					Spec: networkingv1alpha3.VirtualService{
						Hosts: []string{
							"*.svc",
							"*.svc.cluster.local",
						},
						Http: []*networkingv1alpha3.HTTPRoute{
							{
								Match: []*networkingv1alpha3.HTTPMatchRequest{
									{
										Authority: &networkingv1alpha3.StringMatch{
											MatchType: &networkingv1alpha3.StringMatch_Prefix{
												Prefix: fmt.Sprintf("%s.%s.svc", serverName, serverNamespace),
											},
										},
									},
								},
								Route: []*networkingv1alpha3.HTTPRouteDestination{
									{
										Destination: &networkingv1alpha3.Destination{
											Host: fmt.Sprintf("%s.%s.svc.cluster.local", serverName, serverNamespace),
										},
									},
								},
							},
						},
						Gateways: []string{
							fallbackName,
						},
						ExportTo: []string{"."},
					},
				}
				if err := r.Create(ctx, fallbackVsObj); !assert.NoError(t, err) {
					return ctx
				}

				// deploy wasm plugin
				pluginConfig, err := structpb.NewStruct(map[string]interface{}{
					"response_header": responseHeader,
				})
				if !assert.NoError(t, err) {
					return ctx
				}
				fallbackWasmObj := &apiextensionsv1alpha1.WasmPlugin{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
					Spec: extensionsv1alpha1.WasmPlugin{
						Selector: &typev1beta1.WorkloadSelector{
							MatchLabels: map[string]string{
								gatewaySelectorKey: gatewaySelectorValue,
							},
						},
						Url:          "file://" + path.Join(dependencyLearnerMountPath, dependencyLearnerWasmRelativePath),
						Type:         extensionsv1alpha1.PluginType_HTTP,
						Phase:        extensionsv1alpha1.PluginPhase_UNSPECIFIED_PHASE,
						PluginConfig: pluginConfig,
					},
				}
				if err := r.Create(ctx, fallbackWasmObj); !assert.NoError(t, err) {
					return ctx
				}

				// setup fallback service entry
				clientFallbackSvcEntryObj := &apinetworkingv1alpha3.ServiceEntry{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: clientNamespace,
						Name:      fallbackName,
					},
					Spec: networkingv1alpha3.ServiceEntry{
						Hosts: []string{
							"*.svc",
							"*.svc.cluster.local",
						},
						Resolution: networkingv1alpha3.ServiceEntry_NONE,
						ExportTo: []string{
							clientNamespace,
						},
					},
				}
				if err := r.Create(ctx, clientFallbackSvcEntryObj); !assert.NoError(t, err) {
					return ctx
				}

				// setup fallback virtual service
				clientFallbackVsObj := &apinetworkingv1alpha3.VirtualService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fallbackName,
						Namespace: clientNamespace,
					},
					Spec: networkingv1alpha3.VirtualService{
						Hosts: []string{
							"*.svc",
							"*.svc.cluster.local",
						},
						Http: []*networkingv1alpha3.HTTPRoute{
							{
								Route: []*networkingv1alpha3.HTTPRouteDestination{
									{
										Destination: &networkingv1alpha3.Destination{
											Host: fmt.Sprintf("%s.%s.svc.cluster.local", gatewayName, istioNamespace),
											Port: &networkingv1alpha3.PortSelector{
												Number: 443,
											},
										},
									},
								},
							},
						},
						Gateways: []string{"mesh"},
						ExportTo: []string{"."},
					},
				}
				if err = r.Create(ctx, clientFallbackVsObj); !assert.NoError(t, err) {
					return ctx
				}

				// deploy client
				clientLabels := map[string]string{
					"app": clientName,
				}
				clientReplicas := int32(1)
				clientDeploymentObj := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      clientName,
						Namespace: clientNamespace,
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{
							MatchLabels: clientLabels,
						},
						Replicas: &clientReplicas,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": clientName,
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  containerName,
										Image: fmt.Sprintf("nginx:%s", nginxVersion),
									},
								},
							},
						},
					},
				}
				if err := r.Create(ctx, clientDeploymentObj); !assert.NoError(t, err) {
					return ctx
				}

				// deploy server
				serverLabels := map[string]string{
					"app": serverName,
				}
				serverReplicas := int32(1)
				serverDeploymentObj := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serverName,
						Namespace: serverNamespace,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: &serverReplicas,
						Selector: &metav1.LabelSelector{
							MatchLabels: serverLabels,
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: serverLabels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  containerName,
										Image: fmt.Sprintf("nginx:%s", nginxVersion),
									},
								},
							},
						},
					},
				}
				if err := r.Create(ctx, serverDeploymentObj); !assert.NoError(t, err) {
					return ctx
				}

				// create service for server
				serverSvcObj := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serverName,
						Namespace: serverNamespace,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{
								Name: "http",
								Port: 80,
							},
						},
						Selector: map[string]string{
							"app": serverName,
						},
					},
				}
				if err = r.Create(ctx, serverSvcObj); !assert.NoError(t, err) {
					return ctx
				}

				// wait for client deployment
				err = wait.For(conditions.New(r).DeploymentConditionMatch(
					clientDeploymentObj,
					appsv1.DeploymentAvailable,
					corev1.ConditionTrue,
				), wait.WithContext(ctx))
				if assert.NoError(t, err) {
					return ctx
				}

				// wait for server deployment
				err = wait.For(conditions.New(r).DeploymentConditionMatch(
					serverDeploymentObj,
					appsv1.DeploymentAvailable,
					corev1.ConditionTrue,
				), wait.WithContext(ctx))
				if assert.NoError(t, err) {
					return ctx
				}

				return ctx
			},
		).
		Assess(
			"send sample request and check headers",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				client, err := c.NewClient()
				if !assert.NoError(t, err) {
					return ctx
				}

				pods := &corev1.PodList{}
				if err := client.Resources(clientNamespace).List(ctx, pods); !assert.NoError(t, err) ||
					!assert.NotEmpty(t, pods.Items) {
					return ctx
				}
				var stdout, stderr bytes.Buffer
				podName := pods.Items[0].Name
				command := []string{"curl", "-I", fmt.Sprintf("http://%s.%s.svc", serverName, serverNamespace)}
				err = client.Resources().ExecInPod(ctx, clientNamespace, podName, containerName, command, &stdout, &stderr)
				if !assert.NoError(t, err) {
					return ctx
				}
				stdoutStr := stdout.String()
				t.Logf("got response:\n%s", stdoutStr)

				httpStatus := strings.Split(stdoutStr, "\n")[0]
				if !assert.Contains(t, httpStatus, "200") {
					return ctx
				}

				if !assert.Contains(t, stdoutStr, fmt.Sprintf(
					"%s: spiffe://cluster.local/ns/%s/sa/default -> outbound|80||%s.%s.svc.cluster.local",
					responseHeader, clientNamespace, serverName, serverNamespace,
				)) {
					return ctx
				}

				return ctx
			},
		).
		Assess(
			"check config map to see if dependency updated",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Skip()
				return ctx
			},
		).
		Teardown(
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				// create resources client
				r, err := resources.New(c.Client().RESTConfig())
				if !assert.NoError(t, err) {
					return ctx
				}
				if err := istioscheme.AddToScheme(r.GetScheme()); !assert.NoError(t, err) {
					return ctx
				}

				assert.NoError(t, r.Delete(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: serverNamespace,
					},
				}))

				assert.NoError(t, r.Delete(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: clientNamespace,
					},
				}))

				assert.NoError(t, r.Delete(ctx, &apiextensionsv1alpha1.WasmPlugin{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
				}))

				assert.NoError(t, r.Delete(ctx, &apinetworkingv1alpha3.VirtualService{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
				}))

				assert.NoError(t, r.Delete(ctx, &apinetworkingv1alpha3.Gateway{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: istioNamespace,
						Name:      fallbackName,
					},
				}))
				return ctx
			},
		).
		Feature()
	testenv.Test(t, determineDependency)
}
