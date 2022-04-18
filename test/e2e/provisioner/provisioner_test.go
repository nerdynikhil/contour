// Copyright Project Contour Authors
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

//go:build e2e
// +build e2e

package provisioner

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	contour_api_v1alpha1 "github.com/projectcontour/contour/apis/projectcontour/v1alpha1"
	"github.com/projectcontour/contour/internal/gatewayapi"
	"github.com/projectcontour/contour/internal/k8s"
	"github.com/projectcontour/contour/test/e2e"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayapi_v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

var f = e2e.NewFramework(true)

func TestProvisioner(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway provisioner tests")
}

var _ = BeforeSuite(func() {
	require.NoError(f.T(), f.Provisioner.EnsureResourcesForInclusterProvisioner())

	gc := &gatewayapi_v1alpha2.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "contour",
		},
		Spec: gatewayapi_v1alpha2.GatewayClassSpec{
			ControllerName: gatewayapi_v1alpha2.GatewayController("projectcontour.io/gateway-controller"),
		},
	}

	_, ok := f.CreateGatewayClassAndWaitFor(gc, gatewayClassAccepted)
	require.True(f.T(), ok)

})

var _ = AfterSuite(func() {
	// Delete resources individually instead of deleting the entire contour
	// namespace as a performance optimization, because deleting non-empty
	// namespaces can take up to a couple minutes to complete.
	require.NoError(f.T(), f.Provisioner.DeleteResourcesForInclusterProvisioner())

	gc := &gatewayapi_v1alpha2.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "contour",
		},
	}
	require.NoError(f.T(), f.DeleteGatewayClass(gc, false))
})

var _ = Describe("Gateway provisioner", func() {
	f.NamespacedTest("basic-provisioned-gateway", func(namespace string) {
		Specify("A basic one-listener HTTP gateway can be provisioned and routes traffic correctly", func() {
			gateway := &gatewayapi_v1alpha2.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "http",
					Namespace: namespace,
				},
				Spec: gatewayapi_v1alpha2.GatewaySpec{
					GatewayClassName: gatewayapi_v1alpha2.ObjectName("contour"),
					Listeners: []gatewayapi_v1alpha2.Listener{
						{
							Name:     "http",
							Protocol: gatewayapi_v1alpha2.HTTPProtocolType,
							Port:     gatewayapi_v1alpha2.PortNumber(80),
							AllowedRoutes: &gatewayapi_v1alpha2.AllowedRoutes{
								Namespaces: &gatewayapi_v1alpha2.RouteNamespaces{
									From: gatewayapi.FromNamespacesPtr(gatewayapi_v1alpha2.NamespacesFromSame),
								},
							},
						},
					},
				},
			}

			gateway, ok := f.CreateGatewayAndWaitFor(gateway, func(gw *gatewayapi_v1alpha2.Gateway) bool {
				return gatewayReady(gw) && gatewayHasAddress(gw)
			})
			require.True(f.T(), ok)

			f.Fixtures.Echo.Deploy(namespace, "echo")

			route := &gatewayapi_v1alpha2.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      "httproute-1",
				},
				Spec: gatewayapi_v1alpha2.HTTPRouteSpec{
					Hostnames: []gatewayapi_v1alpha2.Hostname{"provisioner.projectcontour.io"},
					CommonRouteSpec: gatewayapi_v1alpha2.CommonRouteSpec{
						ParentRefs: []gatewayapi_v1alpha2.ParentRef{
							gatewayapi.GatewayParentRef("", gateway.Name),
						},
					},
					Rules: []gatewayapi_v1alpha2.HTTPRouteRule{
						{
							Matches:     gatewayapi.HTTPRouteMatch(gatewayapi_v1alpha2.PathMatchPathPrefix, "/prefix"),
							BackendRefs: gatewayapi.HTTPBackendRef("echo", 80, 1),
						},
					},
				},
			}
			_, ok = f.CreateHTTPRouteAndWaitFor(route, httpRouteAccepted)
			require.True(f.T(), ok)

			res, ok := f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
				OverrideURL: "http://" + gateway.Status.Addresses[0].Value,
				Host:        string(route.Spec.Hostnames[0]),
				Path:        "/prefix/match",
				Condition:   e2e.HasStatusCode(200),
			})
			require.NotNil(f.T(), res)
			require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)

			body := f.GetEchoResponseBody(res.Body)
			assert.Equal(f.T(), namespace, body.Namespace)
			assert.Equal(f.T(), "echo", body.Service)
		})
	})

	f.NamespacedTest("multiple-gateways-per-namespace", func(namespace string) {
		Specify("Multiple basic one-listener HTTP gateways can be provisioned in a single namespace and route traffic correctly", func() {
			gatewayCount := 2

			// Create two Gateways and wait for them to be provisioned with addresses.
			var gateways []*gatewayapi_v1alpha2.Gateway
			for i := 0; i < gatewayCount; i++ {
				gw := &gatewayapi_v1alpha2.Gateway{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("http-%d", i),
						Namespace: namespace,
					},
					Spec: gatewayapi_v1alpha2.GatewaySpec{
						GatewayClassName: gatewayapi_v1alpha2.ObjectName("contour"),
						Listeners: []gatewayapi_v1alpha2.Listener{
							{
								Name:     "http",
								Protocol: gatewayapi_v1alpha2.HTTPProtocolType,
								Port:     gatewayapi_v1alpha2.PortNumber(80),
								AllowedRoutes: &gatewayapi_v1alpha2.AllowedRoutes{
									Namespaces: &gatewayapi_v1alpha2.RouteNamespaces{
										From: gatewayapi.FromNamespacesPtr(gatewayapi_v1alpha2.NamespacesFromSame),
									},
								},
							},
						},
					},
				}

				res, ok := f.CreateGatewayAndWaitFor(gw, func(gw *gatewayapi_v1alpha2.Gateway) bool {
					return gatewayReady(gw) && gatewayHasAddress(gw)
				})
				require.True(f.T(), ok)

				gateways = append(gateways, res)
			}

			// Deploy two backend services to test routing.
			for i := 0; i < gatewayCount; i++ {
				f.Fixtures.Echo.Deploy(namespace, fmt.Sprintf("echo-%d", i))
			}

			// Create two HTTPRoutes, one for each Gateway, and wait for them to be accepted
			var routes []*gatewayapi_v1alpha2.HTTPRoute
			for i := 0; i < gatewayCount; i++ {
				route := &gatewayapi_v1alpha2.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      fmt.Sprintf("httproute-%d", i),
					},
					Spec: gatewayapi_v1alpha2.HTTPRouteSpec{
						Hostnames: []gatewayapi_v1alpha2.Hostname{
							gatewayapi_v1alpha2.Hostname(fmt.Sprintf("http-%d.provisioner.projectcontour.io", i)),
						},
						CommonRouteSpec: gatewayapi_v1alpha2.CommonRouteSpec{
							ParentRefs: []gatewayapi_v1alpha2.ParentRef{
								gatewayapi.GatewayParentRef("", fmt.Sprintf("http-%d", i)),
							},
						},
						Rules: []gatewayapi_v1alpha2.HTTPRouteRule{
							{
								Matches:     gatewayapi.HTTPRouteMatch(gatewayapi_v1alpha2.PathMatchPathPrefix, fmt.Sprintf("/http-%d", i)),
								BackendRefs: gatewayapi.HTTPBackendRef(fmt.Sprintf("echo-%d", i), 80, 1),
							},
						},
					},
				}
				res, ok := f.CreateHTTPRouteAndWaitFor(route, httpRouteAccepted)
				require.True(f.T(), ok)

				routes = append(routes, res)
			}

			// Make requests against each HTTPRoute, verify response and backend service.
			for i := 0; i < gatewayCount; i++ {
				res, ok := f.HTTP.RequestUntil(&e2e.HTTPRequestOpts{
					OverrideURL: "http://" + gateways[i].Status.Addresses[0].Value,
					Host:        string(routes[i].Spec.Hostnames[0]),
					Path:        fmt.Sprintf("/http-%d/match", i),
					Condition:   e2e.HasStatusCode(200),
				})
				require.NotNil(f.T(), res)
				require.Truef(f.T(), ok, "expected 200 response code, got %d", res.StatusCode)

				body := f.GetEchoResponseBody(res.Body)
				assert.Equal(f.T(), namespace, body.Namespace)
				assert.Equal(f.T(), fmt.Sprintf("echo-%d", i), body.Service)
			}
		})
	})

	f.NamespacedTest("provisioner-gatewayclass-params", func(namespace string) {
		Specify("GatewayClass parameters are handled correctly", func() {
			// Create GatewayClass with a reference to a nonexistent ContourDeployment,
			// it should be set to "Accepted: false" since the ref is invalid.
			gatewayClass := &gatewayapi_v1alpha2.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "contour-with-params",
				},
				Spec: gatewayapi_v1alpha2.GatewayClassSpec{
					ControllerName: gatewayapi_v1alpha2.GatewayController("projectcontour.io/gateway-controller"),
					ParametersRef: &gatewayapi_v1alpha2.ParametersReference{
						Group:     "projectcontour.io",
						Kind:      "ContourDeployment",
						Namespace: gatewayapi.NamespacePtr(namespace),
						Name:      "contour-params",
					},
				},
			}
			_, ok := f.CreateGatewayClassAndWaitFor(gatewayClass, gatewayClassNotAccepted)
			require.True(f.T(), ok)

			// Create a Gateway using that GatewayClass, it should not be scheduled
			// since the GatewayClass is not accepted.
			gateway := &gatewayapi_v1alpha2.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "http",
					Namespace: namespace,
				},
				Spec: gatewayapi_v1alpha2.GatewaySpec{
					GatewayClassName: gatewayapi_v1alpha2.ObjectName("contour-with-params"),
					Listeners: []gatewayapi_v1alpha2.Listener{
						{
							Name:     "http",
							Protocol: gatewayapi_v1alpha2.HTTPProtocolType,
							Port:     gatewayapi_v1alpha2.PortNumber(80),
							AllowedRoutes: &gatewayapi_v1alpha2.AllowedRoutes{
								Namespaces: &gatewayapi_v1alpha2.RouteNamespaces{
									From: gatewayapi.FromNamespacesPtr(gatewayapi_v1alpha2.NamespacesFromSame),
								},
							},
						},
					},
				},
			}
			require.NoError(f.T(), f.Client.Create(context.Background(), gateway))

			require.Never(f.T(), func() bool {
				gw := &gatewayapi_v1alpha2.Gateway{}
				if err := f.Client.Get(context.Background(), k8s.NamespacedNameOf(gateway), gw); err != nil {
					return false
				}

				return gatewayScheduled(gw)
			}, 10*time.Second, time.Second)

			// Now create the ContourDeployment to match the parametersRef.
			params := &contour_api_v1alpha1.ContourDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      "contour-params",
				},
			}
			require.NoError(f.T(), f.Client.Create(context.Background(), params))

			// Now the GatewayClass should be accepted.
			require.Eventually(f.T(), func() bool {
				gc := &gatewayapi_v1alpha2.GatewayClass{}
				if err := f.Client.Get(context.Background(), k8s.NamespacedNameOf(gatewayClass), gc); err != nil {
					return false
				}

				return gatewayClassAccepted(gc)
			}, time.Minute, time.Second)

			// And now the Gateway should be scheduled.
			require.Eventually(f.T(), func() bool {
				gw := &gatewayapi_v1alpha2.Gateway{}
				if err := f.Client.Get(context.Background(), k8s.NamespacedNameOf(gateway), gw); err != nil {
					return false
				}

				return gatewayScheduled(gw)
			}, time.Minute, time.Second)
		})
	})
})

// gatewayClassAccepted returns true if the gateway has a .status.conditions
// entry of Accepted: true".
func gatewayClassAccepted(gatewayClass *gatewayapi_v1alpha2.GatewayClass) bool {
	if gatewayClass == nil {
		return false
	}

	for _, cond := range gatewayClass.Status.Conditions {
		if cond.Type == string(gatewayapi_v1alpha2.GatewayClassConditionStatusAccepted) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}

	return false
}

// gatewayClassNotAccepted returns true if the gateway has a .status.conditions
// entry of Accepted: false".
func gatewayClassNotAccepted(gatewayClass *gatewayapi_v1alpha2.GatewayClass) bool {
	if gatewayClass == nil {
		return false
	}

	return conditionExists(
		gatewayClass.Status.Conditions,
		string(gatewayapi_v1alpha2.GatewayClassConditionStatusAccepted),
		metav1.ConditionFalse,
	)
}

// gatewayScheduled returns true if the gateway has a .status.conditions
// entry of Scheduled: true".
func gatewayScheduled(gateway *gatewayapi_v1alpha2.Gateway) bool {
	if gateway == nil {
		return false
	}

	return conditionExists(
		gateway.Status.Conditions,
		string(gatewayapi_v1alpha2.GatewayConditionScheduled),
		metav1.ConditionTrue,
	)
}

// gatewayReady returns true if the gateway has a .status.conditions
// entry of Ready: true".
func gatewayReady(gateway *gatewayapi_v1alpha2.Gateway) bool {
	if gateway == nil {
		return false
	}

	return conditionExists(
		gateway.Status.Conditions,
		string(gatewayapi_v1alpha2.GatewayConditionReady),
		metav1.ConditionTrue,
	)
}

// gatewayHasAddress returns true if the gateway has a non-empty
// .status.addresses entry.
func gatewayHasAddress(gateway *gatewayapi_v1alpha2.Gateway) bool {
	if gateway == nil {
		return false
	}

	return len(gateway.Status.Addresses) > 0 && gateway.Status.Addresses[0].Value != ""
}

// httpRouteAccepted returns true if the route has a .status.conditions
// entry of "Accepted: true".
func httpRouteAccepted(route *gatewayapi_v1alpha2.HTTPRoute) bool {
	if route == nil {
		return false
	}

	for _, gw := range route.Status.Parents {
		if conditionExists(gw.Conditions, string(gatewayapi_v1alpha2.ConditionRouteAccepted), metav1.ConditionTrue) {
			return true
		}
	}

	return false
}

func conditionExists(conditions []metav1.Condition, conditionType string, conditionStatus metav1.ConditionStatus) bool {
	for _, cond := range conditions {
		if cond.Type == conditionType && cond.Status == conditionStatus {
			return true
		}
	}

	return false
}