/*
Copyright 2020 The KubeLB Authors.

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

package envoy

import (
	"context"
	"fmt"
	"time"

	envoyAccessLog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	envoyCluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoyCore "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoyEndpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoyListener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoyRoute "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoyFileAccessLog "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	envoy_filter_http_router_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	envoyHttpManager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	envoyTcpProxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	envoyUdpProxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	envoytypev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	envoycache "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	kubelbv1alpha1 "k8c.io/kubelb/api/ce/kubelb.k8c.io/v1alpha1"
	"k8c.io/kubelb/internal/kubelb"
	portlookup "k8c.io/kubelb/internal/port-lookup"

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	endpointAddressReferencePattern = "%s-address-%s"

	// Health check configuration constants
	defaultHealthCheckTimeoutSeconds           = 5
	defaultHealthCheckIntervalSeconds          = 5
	defaultHealthCheckUnhealthyThreshold       = 3
	defaultHealthCheckHealthyThreshold         = 2
	defaultHealthCheckNoTrafficIntervalSeconds = 5
)

func MapSnapshot(ctx context.Context, client ctrlclient.Client, loadBalancers []kubelbv1alpha1.LoadBalancer, routes []kubelbv1alpha1.Route, tunnels []kubelbv1alpha1.Tunnel, portAllocator *portlookup.PortAllocator, globalEnvoyProxyTopology bool, kubelbNamespace string) (*envoycache.Snapshot, error) {
	var listener []types.Resource
	var cluster []types.Resource

	// Check if we need HTTP listeners for tunnel services
	if len(tunnels) > 0 {
		// Create HTTP listener for tunnel traffic
		httpListener := makeHTTPListener("tunnel_http_listener", tunnels, 80)
		listener = append(listener, httpListener)

		// Create cluster for tunnel connection manager
		tunnelCluster := makeTunnelCluster("tunnel-connection-manager", kubelbNamespace)
		cluster = append(cluster, tunnelCluster)
	}

	addressesMap := make(map[string][]kubelbv1alpha1.EndpointAddress)
	for _, lb := range loadBalancers {
		// multiple endpoints represent multiple clusters
		for i, lbEndpoint := range lb.Spec.Endpoints {
			if lbEndpoint.AddressesReference != nil {
				// Check if map already contains the key
				if val, ok := addressesMap[fmt.Sprintf(endpointAddressReferencePattern, lb.Namespace, lbEndpoint.AddressesReference.Name)]; ok {
					lb.Spec.Endpoints[i].Addresses = val
					lbEndpoint.Addresses = val
				} else {
					// Load addresses from reference
					var addresses kubelbv1alpha1.Addresses
					if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: lb.Namespace, Name: lbEndpoint.AddressesReference.Name}, &addresses); err != nil {
						return nil, fmt.Errorf("failed to get addresses: %w", err)
					}
					addressesMap[fmt.Sprintf(endpointAddressReferencePattern, lb.Namespace, lbEndpoint.AddressesReference.Name)] = addresses.Spec.Addresses
					lb.Spec.Endpoints[i].Addresses = addresses.Spec.Addresses
					lbEndpoint.Addresses = addresses.Spec.Addresses
				}
			}

			for _, lbEndpointPort := range lbEndpoint.Ports {
				var lbEndpoints []*envoyEndpoint.LbEndpoint
				key := fmt.Sprintf(kubelb.EnvoyResourceIdentifierPattern, lb.Namespace, lb.Name, i, lbEndpointPort.Port, lbEndpointPort.Protocol)

				// each address -> one port
				for _, lbEndpointAddress := range lbEndpoint.Addresses {
					lbEndpoints = append(lbEndpoints, makeEndpoint(lbEndpointAddress.IP, uint32(lbEndpointPort.Port)))
				}

				port := uint32(lbEndpointPort.Port)
				if globalEnvoyProxyTopology && portAllocator != nil {
					endpointKey := fmt.Sprintf(kubelb.EnvoyEndpointPattern, lb.Namespace, lb.Name, i)
					portKey := fmt.Sprintf(kubelb.EnvoyListenerPattern, lbEndpointPort.Port, lbEndpointPort.Protocol)
					if value, exists := portAllocator.Lookup(endpointKey, portKey); exists {
						port = uint32(value)
					}
				}

				switch lbEndpointPort.Protocol {
				case corev1.ProtocolTCP:
					listener = append(listener, makeTCPListener(key, key, port))
				case corev1.ProtocolUDP:
					listener = append(listener, makeUDPListener(key, key, port))
				}
				cluster = append(cluster, makeCluster(key, lbEndpoints, lbEndpointPort.Protocol))
			}
		}
	}

	for _, route := range routes {
		if route.Spec.Source.Kubernetes == nil {
			continue
		}
		for i, routeendpoint := range route.Spec.Endpoints {
			if routeendpoint.AddressesReference != nil {
				// Check if map already contains the key
				if val, ok := addressesMap[fmt.Sprintf(endpointAddressReferencePattern, route.Namespace, routeendpoint.AddressesReference.Name)]; ok {
					route.Spec.Endpoints[i].Addresses = val
					continue
				}

				// Load addresses from reference
				var addresses kubelbv1alpha1.Addresses
				if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: route.Namespace, Name: routeendpoint.AddressesReference.Name}, &addresses); err != nil {
					return nil, fmt.Errorf("failed to get addresses: %w", err)
				}
				addressesMap[fmt.Sprintf(endpointAddressReferencePattern, route.Namespace, routeendpoint.AddressesReference.Name)] = addresses.Spec.Addresses
				route.Spec.Endpoints[i].Addresses = addresses.Spec.Addresses
			}
		}
		source := route.Spec.Source.Kubernetes
		for _, svc := range source.Services {
			endpointKey := fmt.Sprintf(kubelb.EnvoyEndpointRoutePattern, route.Namespace, svc.Namespace, svc.Name)
			for _, port := range svc.Spec.Ports {
				portLookupKey := fmt.Sprintf(kubelb.EnvoyListenerPattern, port.Port, port.Protocol)
				var lbEndpoints []*envoyEndpoint.LbEndpoint
				for _, address := range route.Spec.Endpoints {
					for _, routeEndpoints := range address.Addresses {
						lbEndpoints = append(lbEndpoints, makeEndpoint(routeEndpoints.IP, uint32(port.NodePort)))
					}
				}

				listenerPort := uint32(port.Port)
				if value, exists := portAllocator.Lookup(endpointKey, portLookupKey); exists {
					listenerPort = uint32(value)
				}

				key := fmt.Sprintf(kubelb.EnvoyRoutePortIdentifierPattern, route.Namespace, svc.Namespace, svc.Name, svc.UID, port.Port, port.Protocol)

				switch port.Protocol {
				case corev1.ProtocolTCP:
					listener = append(listener, makeTCPListener(key, key, listenerPort))
				case corev1.ProtocolUDP:
					listener = append(listener, makeUDPListener(key, key, listenerPort))
				}
				cluster = append(cluster, makeCluster(key, lbEndpoints, port.Protocol))
			}
		}
	}

	var content []byte
	var resources []types.Resource
	resources = append(resources, cluster...)
	resources = append(resources, listener...)
	for _, r := range resources {
		mr, err := envoycache.MarshalResource(r)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal resource: %w", err)
		}
		content = append(content, mr...)
	}
	version := envoycache.HashResource(content)

	return envoycache.NewSnapshot(
		version,
		map[resource.Type][]types.Resource{
			resource.ClusterType:  cluster,
			resource.ListenerType: listener,
		},
	)
}

func makeCluster(clusterName string, lbEndpoints []*envoyEndpoint.LbEndpoint, protocol corev1.Protocol) *envoyCluster.Cluster {
	defaultHealthCheck := []*envoyCore.HealthCheck{
		{
			Timeout:            &durationpb.Duration{Seconds: defaultHealthCheckTimeoutSeconds},
			Interval:           &durationpb.Duration{Seconds: defaultHealthCheckIntervalSeconds},
			UnhealthyThreshold: &wrapperspb.UInt32Value{Value: defaultHealthCheckUnhealthyThreshold},
			HealthyThreshold:   &wrapperspb.UInt32Value{Value: defaultHealthCheckHealthyThreshold},
			// Start sending health checks after 5 seconds to a new cluster. The default is 60 seconds.
			NoTrafficInterval: &durationpb.Duration{Seconds: defaultHealthCheckNoTrafficIntervalSeconds},
			HealthChecker: &envoyCore.HealthCheck_TcpHealthCheck_{
				TcpHealthCheck: &envoyCore.HealthCheck_TcpHealthCheck{
					// This will use empty payload to perform connect-only health check.
					Send:    nil,
					Receive: []*envoyCore.HealthCheck_Payload{},
				}},
		},
	}

	if protocol == corev1.ProtocolUDP {
		// UDP health checks are not supported in Envoy, so we set defaultHealthCheck to nil
		defaultHealthCheck = nil
	}

	return &envoyCluster.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &envoyCluster.Cluster_Type{Type: envoyCluster.Cluster_STRICT_DNS},
		LbPolicy:             envoyCluster.Cluster_ROUND_ROBIN,
		LoadAssignment: &envoyEndpoint.ClusterLoadAssignment{
			ClusterName: clusterName,
			Endpoints: []*envoyEndpoint.LocalityLbEndpoints{{
				LbEndpoints: lbEndpoints,
			}},
		},
		DnsLookupFamily: envoyCluster.Cluster_V4_ONLY,
		HealthChecks:    defaultHealthCheck,
		CommonLbConfig: &envoyCluster.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &envoytypev3.Percent{Value: 0},
		},
	}
}

func makeEndpoint(address string, port uint32) *envoyEndpoint.LbEndpoint {
	return &envoyEndpoint.LbEndpoint{
		HostIdentifier: &envoyEndpoint.LbEndpoint_Endpoint{
			Endpoint: &envoyEndpoint.Endpoint{
				Address: &envoyCore.Address{
					Address: &envoyCore.Address_SocketAddress{
						SocketAddress: &envoyCore.SocketAddress{
							Protocol: envoyCore.SocketAddress_TCP,
							Address:  address,
							PortSpecifier: &envoyCore.SocketAddress_PortValue{
								PortValue: port,
							},
						},
					},
				},
			},
		},
	}
}

func makeTCPListener(clusterName string, listenerName string, listenerPort uint32) *envoyListener.Listener {
	tcpProxyAccessLog := &envoyFileAccessLog.FileAccessLog{
		Path: "/dev/stdout",
	}
	tcpProxyAccessLogAny, err := anypb.New(tcpProxyAccessLog)
	if err != nil {
		panic(err)
	}

	tcpProxy := &envoyTcpProxy.TcpProxy{
		StatPrefix: listenerName,
		ClusterSpecifier: &envoyTcpProxy.TcpProxy_Cluster{
			Cluster: clusterName,
		},
		AccessLog: []*envoyAccessLog.AccessLog{
			{
				Name: "envoy.file_access_log",
				ConfigType: &envoyAccessLog.AccessLog_TypedConfig{
					TypedConfig: tcpProxyAccessLogAny,
				},
			},
		},
	}
	pbst, err := anypb.New(tcpProxy)
	if err != nil {
		panic(err)
	}

	return &envoyListener.Listener{
		Name: listenerName,
		Address: &envoyCore.Address{
			Address: &envoyCore.Address_SocketAddress{
				SocketAddress: &envoyCore.SocketAddress{
					Protocol: envoyCore.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &envoyCore.SocketAddress_PortValue{
						PortValue: listenerPort,
					},
				},
			},
		},
		FilterChains: []*envoyListener.FilterChain{{
			Filters: []*envoyListener.Filter{{
				Name: wellknown.TCPProxy,
				ConfigType: &envoyListener.Filter_TypedConfig{
					TypedConfig: pbst,
				},
			}},
		}},
	}
}

func makeUDPListener(clusterName string, listenerName string, listenerPort uint32) *envoyListener.Listener {
	udpProxy := &envoyUdpProxy.UdpProxyConfig{
		StatPrefix: listenerName,
		RouteSpecifier: &envoyUdpProxy.UdpProxyConfig_Cluster{
			Cluster: clusterName,
		},
	}

	pbst, err := anypb.New(udpProxy)
	if err != nil {
		panic(err)
	}

	return &envoyListener.Listener{
		Name: listenerName,
		Address: &envoyCore.Address{
			Address: &envoyCore.Address_SocketAddress{
				SocketAddress: &envoyCore.SocketAddress{
					Protocol: envoyCore.SocketAddress_UDP,
					Address:  "0.0.0.0",
					PortSpecifier: &envoyCore.SocketAddress_PortValue{
						PortValue: listenerPort,
					},
				},
			},
		},
		ListenerFilters: []*envoyListener.ListenerFilter{
			{
				Name: "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &envoyListener.ListenerFilter_TypedConfig{
					TypedConfig: pbst,
				},
			},
		},
		ReusePort: true,
	}
}

// makeHTTPListener creates an HTTP listener for tunnel traffic
func makeHTTPListener(listenerName string, tunnels []kubelbv1alpha1.Tunnel, listenerPort uint32) *envoyListener.Listener {
	// Create virtual hosts based on tunnel hostnames
	var virtualHosts []*envoyRoute.VirtualHost

	for _, tunnel := range tunnels {
		if tunnel.Status.Hostname != "" {
			// Create a virtual host for each tunnel hostname
			virtualHost := &envoyRoute.VirtualHost{
				Name:    fmt.Sprintf("tunnel-%s-%s", tunnel.Namespace, tunnel.Name),
				Domains: []string{tunnel.Status.Hostname},
				Routes: []*envoyRoute.Route{
					{
						Match: &envoyRoute.RouteMatch{
							PathSpecifier: &envoyRoute.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &envoyRoute.Route_Route{
							Route: &envoyRoute.RouteAction{
								ClusterSpecifier: &envoyRoute.RouteAction_Cluster{
									Cluster: "tunnel-connection-manager",
								},
								// Preserve the original host header for the connection manager
								HostRewriteSpecifier: &envoyRoute.RouteAction_AutoHostRewrite{
									AutoHostRewrite: &wrapperspb.BoolValue{Value: false},
								},
							},
						},
						// Add request headers for tunnel processing
						RequestHeadersToAdd: []*envoyCore.HeaderValueOption{
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-tunnel-host",
									Value: "%REQ(:authority)%",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-tunnel-token",
									Value: "%REQ(authorization)%",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-forwarded-for",
									Value: "%DOWNSTREAM_REMOTE_ADDRESS%",
								},
								AppendAction: envoyCore.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
							},
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-forwarded-proto",
									Value: "https",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
						},
						// Add security headers to responses
						ResponseHeadersToAdd: []*envoyCore.HeaderValueOption{
							{
								Header: &envoyCore.HeaderValue{
									Key:   "strict-transport-security",
									Value: "max-age=31536000; includeSubDomains",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-content-type-options",
									Value: "nosniff",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &envoyCore.HeaderValue{
									Key:   "x-frame-options",
									Value: "DENY",
								},
								AppendAction: envoyCore.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
						},
					},
				},
			}
			virtualHosts = append(virtualHosts, virtualHost)
		}
	}

	// If no virtual hosts, create a default catch-all
	if len(virtualHosts) == 0 {
		virtualHosts = []*envoyRoute.VirtualHost{
			{
				Name:    "tunnel-default",
				Domains: []string{"*"},
				Routes: []*envoyRoute.Route{
					{
						Match: &envoyRoute.RouteMatch{
							PathSpecifier: &envoyRoute.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &envoyRoute.Route_Route{
							Route: &envoyRoute.RouteAction{
								ClusterSpecifier: &envoyRoute.RouteAction_Cluster{
									Cluster: "tunnel-connection-manager",
								},
							},
						},
					},
				},
			},
		}
	}

	// Create route configuration
	routeConfig := &envoyRoute.RouteConfiguration{
		Name:         listenerName + "_route",
		VirtualHosts: virtualHosts,
	}

	// Create access log configuration
	accessLog := &envoyFileAccessLog.FileAccessLog{
		Path: "/dev/stdout",
	}
	accessLogAny, err := anypb.New(accessLog)
	if err != nil {
		panic(err)
	}

	// Create HTTP connection manager - simplified without transcoding
	httpConnManager := &envoyHttpManager.HttpConnectionManager{
		StatPrefix: listenerName,
		RouteSpecifier: &envoyHttpManager.HttpConnectionManager_RouteConfig{
			RouteConfig: routeConfig,
		},
		HttpFilters: []*envoyHttpManager.HttpFilter{{
			Name: wellknown.Router,
			ConfigType: &envoyHttpManager.HttpFilter_TypedConfig{
				TypedConfig: MustMarshalAny(&envoy_filter_http_router_v3.Router{}),
			},
		}},
		AccessLog: []*envoyAccessLog.AccessLog{
			{
				Name: "envoy.access_loggers.file",
				ConfigType: &envoyAccessLog.AccessLog_TypedConfig{
					TypedConfig: accessLogAny,
				},
			},
		},
	}

	httpConnManagerAny, err := anypb.New(httpConnManager)
	if err != nil {
		panic(err)
	}

	return &envoyListener.Listener{
		Name: listenerName,
		Address: &envoyCore.Address{
			Address: &envoyCore.Address_SocketAddress{
				SocketAddress: &envoyCore.SocketAddress{
					Protocol: envoyCore.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &envoyCore.SocketAddress_PortValue{
						PortValue: listenerPort,
					},
				},
			},
		},
		FilterChains: []*envoyListener.FilterChain{
			{
				Filters: []*envoyListener.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &envoyListener.Filter_TypedConfig{
							TypedConfig: httpConnManagerAny,
						},
					},
				},
			},
		},
	}
}

// makeTunnelCluster creates a cluster for the tunnel connection manager
func makeTunnelCluster(clusterName string, kubelbNamespace string) *envoyCluster.Cluster {
	// Create endpoint for the tunnel connection manager service
	// This will point to the connection manager HTTP service for Envoy traffic
	endpoint := makeEndpoint(fmt.Sprintf("tunnel-connection-manager.%s.svc.cluster.local", kubelbNamespace), 8080)

	return &envoyCluster.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &envoyCluster.Cluster_Type{Type: envoyCluster.Cluster_STRICT_DNS},
		LbPolicy:             envoyCluster.Cluster_ROUND_ROBIN,
		LoadAssignment: &envoyEndpoint.ClusterLoadAssignment{
			ClusterName: clusterName,
			Endpoints: []*envoyEndpoint.LocalityLbEndpoints{
				{
					LbEndpoints: []*envoyEndpoint.LbEndpoint{endpoint},
				},
			},
		},
		DnsLookupFamily: envoyCluster.Cluster_V4_ONLY,
		// Use HTTP health checks since we'll implement an HTTP endpoint on the gRPC server
		HealthChecks: []*envoyCore.HealthCheck{
			{
				Timeout:            &durationpb.Duration{Seconds: defaultHealthCheckTimeoutSeconds},
				Interval:           &durationpb.Duration{Seconds: defaultHealthCheckIntervalSeconds},
				UnhealthyThreshold: &wrapperspb.UInt32Value{Value: defaultHealthCheckUnhealthyThreshold},
				HealthyThreshold:   &wrapperspb.UInt32Value{Value: defaultHealthCheckHealthyThreshold},
				NoTrafficInterval:  &durationpb.Duration{Seconds: defaultHealthCheckNoTrafficIntervalSeconds},
				HealthChecker: &envoyCore.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &envoyCore.HealthCheck_HttpHealthCheck{
						Path: "/health",
					},
				},
			},
		},
		CommonLbConfig: &envoyCluster.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &envoytypev3.Percent{Value: 0},
		},
	}
}

func MustMarshalAny(pb proto.Message) *anypb.Any {
	a, err := anypb.New(pb)
	if err != nil {
		panic(err.Error())
	}

	return a
}
