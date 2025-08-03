/*
Copyright 2025 The KubeLB Authors.

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

package tunnel

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/credentials"

	pb "k8c.io/kubelb/proto/tunnel"
	kubelbv1alpha1 "k8c.io/kubelb/api/ce/kubelb.k8c.io/v1alpha1"

	"k8s.io/klog/v2"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Default timeout for forwarded requests
const defaultRequestTimeout = 30 * time.Second

// ServiceServer implements the gRPC tunnel service
type ServiceServer struct {
	pb.UnimplementedTunnelServiceServer
	registry         *Registry
	responseChannels sync.Map // map[requestID]chan *pb.HttpResponse
	requestTimeout   time.Duration
	kubeClient       ctrlruntimeclient.Client
}

// NewServiceServer creates a new tunnel service server
func NewServiceServer(registry *Registry, requestTimeout time.Duration, kubeClient ctrlruntimeclient.Client) *ServiceServer {
	if requestTimeout == 0 {
		requestTimeout = defaultRequestTimeout
	}
	return &ServiceServer{
		registry:       registry,
		requestTimeout: requestTimeout,
		kubeClient:     kubeClient,
	}
}

// CreateTunnel implements the bidirectional streaming RPC for tunnel clients
func (s *ServiceServer) CreateTunnel(stream grpc.BidiStreamingServer[pb.TunnelMessage, pb.TunnelMessage]) error {
	log := klog.FromContext(stream.Context())
	log.V(4).Info("New tunnel connection initiated")

	var hostname string
	var authenticated bool

	// Cleanup function
	defer func() {
		if hostname != "" {
			s.registry.UnregisterTunnel(hostname)
			log.Info("Tunnel disconnected", "hostname", hostname)
		}
	}()

	for {
		// Receive message from client
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.V(4).Info("Tunnel stream closed by client", "hostname", hostname)
			} else {
				log.Error(err, "Error receiving message from tunnel", "hostname", hostname)
			}
			return err
		}

		// Handle message based on type
		switch payload := msg.GetPayload().(type) {
		case *pb.TunnelMessage_Auth:
			if authenticated {
				return status.Error(codes.AlreadyExists, "tunnel already authenticated")
			}

			auth := payload.Auth
			hostname = auth.Hostname

			// TODO: Extract and validate client certificate when TLS is enabled
			// For now, rely on token validation only
			// clientCert, err := s.extractClientCertificate(stream.Context())
			// if err != nil {
			// 	log.Error(err, "Failed to extract client certificate")
			// 	return status.Errorf(codes.Unauthenticated, "client certificate required: %v", err)
			// }
			// if err := s.validateClientCertificate(clientCert, hostname); err != nil {
			// 	log.Error(err, "Client certificate validation failed", "hostname", hostname)
			// 	return status.Errorf(codes.Unauthenticated, "invalid client certificate: %v", err)
			// }

			// Validate token against Kubernetes
			if err := s.validateTunnelToken(stream.Context(), hostname, auth.Token); err != nil {
				log.Error(err, "Token validation failed", "hostname", hostname)
				return status.Errorf(codes.Unauthenticated, "invalid tunnel token: %v", err)
			}

			// Register tunnel
			if err := s.registry.RegisterTunnel(hostname, stream, auth.Token, auth.TargetPort); err != nil {
				return status.Errorf(codes.Internal, "failed to register tunnel: %v", err)
			}

			authenticated = true
			log.Info("Tunnel authenticated", "hostname", hostname, "targetPort", auth.TargetPort)

		case *pb.TunnelMessage_Response:
			if !authenticated {
				return status.Error(codes.Unauthenticated, "tunnel not authenticated")
			}

			// Forward response to waiting request
			s.handleTunnelResponse(payload.Response)

		case *pb.TunnelMessage_Control:
			if !authenticated {
				return status.Error(codes.Unauthenticated, "tunnel not authenticated")
			}

			// Handle control messages
			if err := s.handleControl(stream, payload.Control); err != nil {
				return err
			}

		case *pb.TunnelMessage_Error:
			log.Error(nil, "Received error from tunnel",
				"hostname", hostname,
				"error", payload.Error.ErrorMessage,
				"code", payload.Error.ErrorCode)

			// Forward error to waiting request if applicable
			if payload.Error.RequestId != "" {
				s.handleError(payload.Error)
			}
		}
	}
}

// ForwardRequest handles incoming HTTP requests from Envoy
func (s *ServiceServer) ForwardRequest(ctx context.Context, req *pb.ForwardRequestMessage) (*pb.ForwardResponseMessage, error) {
	log := klog.FromContext(ctx)

	// Validate request
	if req.TunnelHost == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel_host is required")
	}

	if req.Request == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	// Get active tunnel
	tunnel, exists := s.registry.GetActiveTunnel(req.TunnelHost)
	if !exists {
		log.V(4).Info("No active tunnel", "hostname", req.TunnelHost)
		return nil, status.Errorf(codes.Unavailable, "no active tunnel for hostname: %s", req.TunnelHost)
	}

	// Validate token
	if err := s.registry.ValidateToken(req.TunnelToken, req.TunnelHost); err != nil {
		log.V(4).Info("Token validation failed", "hostname", req.TunnelHost, "error", err)
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}

	// Generate request ID if not present
	if req.Request.RequestId == "" {
		req.Request.RequestId = uuid.New().String()
	}

	// Create response channel
	respChan := make(chan interface{}, 1) // Can receive either *pb.HttpResponse or error
	s.responseChannels.Store(req.Request.RequestId, respChan)
	defer s.responseChannels.Delete(req.Request.RequestId)

	// Send request to tunnel
	msg := &pb.TunnelMessage{
		Payload: &pb.TunnelMessage_Request{
			Request: req.Request,
		},
	}

	if err := tunnel.Stream.Send(msg); err != nil {
		log.Error(err, "Failed to send request to tunnel", "hostname", req.TunnelHost)
		return nil, status.Errorf(codes.Internal, "failed to send request to tunnel: %v", err)
	}

	// Wait for response with timeout
	select {
	case resp := <-respChan:
		switch v := resp.(type) {
		case *pb.HttpResponse:
			return &pb.ForwardResponseMessage{Response: v}, nil
		case error:
			return nil, v
		default:
			return nil, status.Error(codes.Internal, "unexpected response type")
		}

	case <-time.After(s.requestTimeout):
		return nil, status.Errorf(codes.DeadlineExceeded, "request timeout after %v", s.requestTimeout)

	case <-ctx.Done():
		return nil, status.Error(codes.Canceled, "request canceled")

	case <-tunnel.Context.Done():
		return nil, status.Error(codes.Unavailable, "tunnel disconnected during request")
	}
}

// Health implements the health check RPC
func (s *ServiceServer) Health(_ context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	activeTunnels := s.registry.GetAllTunnels()
	return &pb.HealthResponse{
		Healthy: true,
		Message: fmt.Sprintf("Tunnel service is healthy. Active tunnels: %d", len(activeTunnels)),
	}, nil
}

// handleTunnelResponse forwards a response from tunnel to waiting request
func (s *ServiceServer) handleTunnelResponse(response *pb.HttpResponse) {
	if response.RequestId == "" {
		klog.Info("Received response without request ID")
		return
	}

	if ch, ok := s.responseChannels.Load(response.RequestId); ok {
		respChan := ch.(chan interface{})
		select {
		case respChan <- response:
			// Response sent successfully
		default:
			klog.Info("Response channel full, dropping response", "requestId", response.RequestId)
		}
	} else {
		klog.V(4).Info("No waiting request for response", "requestId", response.RequestId)
	}
}

// handleError forwards an error from tunnel to waiting request
func (s *ServiceServer) handleError(tunnelError *pb.TunnelError) {
	if ch, ok := s.responseChannels.Load(tunnelError.RequestId); ok {
		respChan := ch.(chan interface{})

		// Convert tunnel error to gRPC status
		code := codes.Internal
		if tunnelError.ErrorCode == 404 {
			code = codes.NotFound
		} else if tunnelError.ErrorCode >= 500 {
			code = codes.Unavailable
		}

		err := status.Error(code, tunnelError.ErrorMessage)

		select {
		case respChan <- err:
			// Error sent successfully
		default:
			klog.Info("Response channel full, dropping error", "requestId", tunnelError.RequestId)
		}
	}
}

// handleControl handles control messages like ping/pong
func (s *ServiceServer) handleControl(stream grpc.BidiStreamingServer[pb.TunnelMessage, pb.TunnelMessage], control *pb.TunnelControl) error {
	switch control.Type {
	case pb.TunnelControl_PING:
		// Respond with PONG
		pong := &pb.TunnelMessage{
			Payload: &pb.TunnelMessage_Control{
				Control: &pb.TunnelControl{
					Type:    pb.TunnelControl_PONG,
					Message: "pong",
				},
			},
		}
		return stream.Send(pong)

	case pb.TunnelControl_DISCONNECT:
		return status.Error(codes.Canceled, "client requested disconnect")

	default:
		// Ignore other control messages
		return nil
	}
}

// validateTunnelToken validates the provided token against the tunnel resource in Kubernetes
func (s *ServiceServer) validateTunnelToken(ctx context.Context, hostname, providedToken string) error {
	// Find all tunnels to match by hostname
	var tunnelList kubelbv1alpha1.TunnelList
	if err := s.kubeClient.List(ctx, &tunnelList); err != nil {
		return fmt.Errorf("failed to list tunnels: %w", err)
	}

	// Find tunnel with matching hostname
	for _, tunnel := range tunnelList.Items {
		if tunnel.Status.Hostname == hostname {
			// Check if the provided token matches the expected token
			if tunnel.Status.Token == "" {
				return fmt.Errorf("tunnel %s/%s has no token configured", tunnel.Namespace, tunnel.Name)
			}
			if tunnel.Status.Token != providedToken {
				return fmt.Errorf("invalid token for tunnel %s/%s", tunnel.Namespace, tunnel.Name)
			}
			// Token is valid
			return nil
		}
	}

	return fmt.Errorf("no tunnel found for hostname: %s", hostname)
}

// extractClientCertificate extracts the client certificate from gRPC context
func (s *ServiceServer) extractClientCertificate(ctx context.Context) (*x509.Certificate, error) {
	// Get peer info from gRPC context
	peer, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no peer information in context")
	}

	// Extract TLS info
	tlsInfo, ok := peer.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, fmt.Errorf("no TLS information in peer context")
	}

	// Get client certificates
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no client certificates provided")
	}

	// Return the first (leaf) certificate
	return tlsInfo.State.PeerCertificates[0], nil
}

// validateClientCertificate validates that the client certificate matches the tunnel
func (s *ServiceServer) validateClientCertificate(cert *x509.Certificate, hostname string) error {
	// Extract tunnel name from certificate Subject CN
	// Expected format: tunnel-{tunnel-name}-{namespace}
	cn := cert.Subject.CommonName
	if !strings.HasPrefix(cn, "tunnel-") {
		return fmt.Errorf("certificate CN does not have tunnel- prefix: %s", cn)
	}

	// Parse the tunnel name from CN
	// Format: tunnel-{tunnel-name}-{namespace}
	parts := strings.Split(cn, "-")
	if len(parts) < 3 {
		return fmt.Errorf("invalid certificate CN format: %s", cn)
	}

	// Extract tunnel name (everything between "tunnel-" and the last "-{namespace}")
	tunnelName := strings.Join(parts[1:len(parts)-1], "-")
	
	// TODO: In a more sophisticated implementation, we could:
	// 1. Extract tunnel name from certificate extensions (more reliable)
	// 2. Cross-reference with tunnel hostname to validate the certificate
	// 3. Check certificate expiry
	
	// For now, just log the extracted tunnel name for debugging
	klog.V(4).Info("Client certificate validation", 
		"cn", cn, 
		"extractedTunnel", tunnelName, 
		"requestedHostname", hostname)

	// Basic validation: certificate must not be expired
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return fmt.Errorf("certificate is expired or not yet valid")
	}

	return nil
}
