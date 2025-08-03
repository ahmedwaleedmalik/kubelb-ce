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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	pb "k8c.io/kubelb/proto/tunnel"

	"k8s.io/klog/v2"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ConnectionManager manages tunnel connections and provides gRPC service
type ConnectionManager struct {
	// Configuration
	grpcAddr       string
	httpAddr       string
	requestTimeout time.Duration
	kubeClient     ctrlruntimeclient.Client

	// Core components
	registry      *Registry
	tunnelService *ServiceServer

	// Servers
	grpcServer *grpc.Server
	httpServer *http.Server
}

// ConnectionManagerConfig holds configuration for the connection manager
type ConnectionManagerConfig struct {
	GRPCAddr       string
	HTTPAddr       string
	RequestTimeout time.Duration
	KubeClient     ctrlruntimeclient.Client
	// TLS configuration
	TLSCertFile    string
	TLSKeyFile     string
	CACertFile     string
	EnableMTLS     bool
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(config *ConnectionManagerConfig) (*ConnectionManager, error) {
	// Create registry
	registry := NewRegistry()

	// Create tunnel service with timeout and Kubernetes client
	tunnelService := NewServiceServer(registry, config.RequestTimeout, config.KubeClient)

	return &ConnectionManager{
		grpcAddr:       config.GRPCAddr,
		httpAddr:       config.HTTPAddr,
		requestTimeout: config.RequestTimeout,
		kubeClient:     config.KubeClient,
		registry:       registry,
		tunnelService:  tunnelService,
	}, nil
}

// Start starts the connection manager
func (cm *ConnectionManager) Start(ctx context.Context) error {
	log := klog.FromContext(ctx)

	// Start gRPC server
	if err := cm.startGRPCServer(ctx); err != nil {
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}

	// Start HTTP server for Envoy
	if err := cm.startHTTPServer(ctx); err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}

	log.Info("Connection manager started", "grpcAddr", cm.grpcAddr, "httpAddr", cm.httpAddr)

	return nil
}

// startGRPCServer starts the gRPC server for tunnel connections
func (cm *ConnectionManager) startGRPCServer(ctx context.Context) error {
	log := klog.FromContext(ctx)

	// Listen on gRPC address
	lc := &net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", cm.grpcAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cm.grpcAddr, err)
	}

	// Create gRPC server options with production settings
	opts := []grpc.ServerOption{
		// Keepalive parameters for connection health
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second, // Send ping every 30 seconds
			Timeout: 10 * time.Second, // Wait 10 seconds for ping response
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second, // Minimum time between pings
			PermitWithoutStream: true,             // Allow pings without active streams
		}),
		// Connection limits
		grpc.MaxConcurrentStreams(1000),       // Allow up to 1000 concurrent streams
		grpc.MaxRecvMsgSize(64 * 1024 * 1024), // 64MB max receive message size
		grpc.MaxSendMsgSize(64 * 1024 * 1024), // 64MB max send message size
	}

	// Configure TLS with client certificate requirement if certificates are available
	tlsConfig, err := cm.createTLSConfig(ctx)
	if err != nil {
		log.V(2).Info("TLS certificates not available, using plain gRPC", "error", err)
		// Create gRPC server without TLS
		cm.grpcServer = grpc.NewServer(opts...)
	} else {
		log.Info("Enabling mTLS for gRPC server")
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		cm.grpcServer = grpc.NewServer(opts...)
	}

	// Register tunnel service with generated registration
	pb.RegisterTunnelServiceServer(cm.grpcServer, cm.tunnelService)

	// Start serving in goroutine
	go func() {
		log.Info("Starting gRPC server", "addr", cm.grpcAddr)
		if err := cm.grpcServer.Serve(listener); err != nil {
			log.Error(err, "gRPC server stopped")
		}
	}()

	// Handle graceful shutdown
	go func() {
		<-ctx.Done()
		log.Info("Shutting down gRPC server")

		// Graceful stop with timeout
		stopped := make(chan struct{})
		go func() {
			cm.grpcServer.GracefulStop()
			close(stopped)
		}()

		// Force stop after 30 seconds
		select {
		case <-stopped:
			log.Info("gRPC server stopped gracefully")
		case <-time.After(30 * time.Second):
			log.Info("Force stopping gRPC server after timeout")
			cm.grpcServer.Stop()
		}
	}()

	return nil
}

// startHTTPServer starts the HTTP server for receiving Envoy requests
func (cm *ConnectionManager) startHTTPServer(ctx context.Context) error {
	log := klog.FromContext(ctx)

	// Create HTTP mux
	mux := http.NewServeMux()

	// Handle all requests to forward through tunnels
	mux.HandleFunc("/", cm.handleTunnelRequest)

	// Health check endpoint
	mux.HandleFunc("/health", cm.handleHealth)

	// Create HTTP server
	cm.httpServer = &http.Server{
		Addr:         cm.httpAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start serving in goroutine
	go func() {
		log.Info("Starting HTTP server", "addr", cm.httpAddr)
		if err := cm.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err, "HTTP server stopped")
		}
	}()

	// Handle graceful shutdown
	go func() {
		<-ctx.Done()
		log.Info("Shutting down HTTP server")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := cm.httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "Error shutting down HTTP server")
		} else {
			log.Info("HTTP server stopped gracefully")
		}
	}()

	return nil
}

// handleTunnelRequest handles incoming HTTP requests and forwards them through tunnels
func (cm *ConnectionManager) handleTunnelRequest(w http.ResponseWriter, r *http.Request) {
	// Extract host from x-tunnel-host header first (from Envoy), fallback to Host header
	host := r.Header.Get("X-Tunnel-Host")
	if host == "" {
		host = r.Host
		if host == "" {
			host = r.Header.Get("Host")
		}
	}

	// Remove port if present
	if colonIndex := strings.Index(host, ":"); colonIndex > 0 {
		host = host[:colonIndex]
	}

	// Extract tunnel token from x-tunnel-token header (from Envoy)
	tunnelToken := r.Header.Get("X-Tunnel-Token")

	log := klog.FromContext(r.Context()).WithValues("host", host, "method", r.Method, "path", r.URL.Path)

	// Find tunnel for this host
	conn, exists := cm.registry.GetActiveTunnel(host)
	if !exists {
		log.V(2).Info("No tunnel found for host")
		http.Error(w, "Tunnel not found", http.StatusNotFound)
		return
	}

	// Validate tunnel token if provided
	if tunnelToken != "" && tunnelToken != conn.Token {
		log.V(2).Info("Invalid tunnel token")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error(err, "Failed to read request body")
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Convert headers to map
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0] // Take first value for simplicity
		}
	}

	// Ensure X-Forwarded headers are included
	if xForwardedFor := r.Header.Get("X-Forwarded-For"); xForwardedFor != "" {
		headers["X-Forwarded-For"] = xForwardedFor
	}
	if xForwardedProto := r.Header.Get("X-Forwarded-Proto"); xForwardedProto != "" {
		headers["X-Forwarded-Proto"] = xForwardedProto
	}

	// Create forward request message
	req := &pb.ForwardRequestMessage{
		TunnelHost:  host,
		TunnelToken: conn.Token,
		Request: &pb.HttpRequest{
			RequestId: fmt.Sprintf("%d", time.Now().UnixNano()), // Simple request ID
			Method:    r.Method,
			Path:      r.URL.RequestURI(),
			Headers:   headers,
			Body:      body,
		},
	}

	// Send request through tunnel and wait for response
	resp, err := cm.tunnelService.ForwardRequest(r.Context(), req)
	if err != nil {
		log.Error(err, "Failed to forward request through tunnel")
		http.Error(w, "Tunnel request failed", http.StatusBadGateway)
		return
	}

	// Write response
	if resp.Response != nil {
		// Set response headers
		for key, value := range resp.Response.Headers {
			w.Header().Set(key, value)
		}

		// Set status code
		w.WriteHeader(int(resp.Response.StatusCode))

		// Write response body
		if len(resp.Response.Body) > 0 {
			if _, err := w.Write(resp.Response.Body); err != nil {
				log.Error(err, "Failed to write response body")
			}
		}
	} else {
		log.Error(nil, "Received tunnel response without response data")
		http.Error(w, "Invalid tunnel response", http.StatusBadGateway)
	}
}

// handleHealth handles health check requests
func (cm *ConnectionManager) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"status":"healthy","tunnels":` + fmt.Sprintf("%d", len(cm.registry.GetAllTunnels())) + `}`)); err != nil {
		// Log error but don't fail the health check
		klog.V(2).Info("Failed to write health check response", "error", err)
	}
}

// Stop stops the connection manager
func (cm *ConnectionManager) Stop() {
	if cm.grpcServer != nil {
		cm.grpcServer.GracefulStop()
	}
	if cm.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cm.httpServer.Shutdown(ctx); err != nil {
			klog.V(2).Info("Failed to shutdown HTTP server gracefully", "error", err)
		}
	}
}

// GetActiveTunnels returns the list of active tunnel hostnames
func (cm *ConnectionManager) GetActiveTunnels() []string {
	return cm.registry.GetAllTunnels()
}

// GetTunnelService returns the tunnel service for testing
func (cm *ConnectionManager) GetTunnelService() *ServiceServer {
	return cm.tunnelService
}

// GetRegistry returns the tunnel registry for testing
func (cm *ConnectionManager) GetRegistry() *Registry {
	return cm.registry
}

// createTLSConfig creates TLS configuration for mTLS
func (cm *ConnectionManager) createTLSConfig(_ context.Context) (*tls.Config, error) {
	// Load server certificate from mounted secret
	serverCert, err := cm.loadServerCertificate()
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate: %w", err)
	}
	
	// Load CA certificate for client validation
	caCertPool, err := cm.loadCACertPool()
	if err != nil {
		return nil, fmt.Errorf("failed to load CA certificate: %w", err)
	}
	
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert, // Require and verify client certificates
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
		ClientCAs: caCertPool,
	}
	
	return tlsConfig, nil
}

// loadServerCertificate loads the server certificate from mounted secret
func (cm *ConnectionManager) loadServerCertificate() (tls.Certificate, error) {
	certPath := "/etc/certs/tls.crt"
	keyPath := "/etc/certs/tls.key"
	
	// Check if certificate files exist
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		return tls.Certificate{}, fmt.Errorf("server certificate not found at %s", certPath)
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		return tls.Certificate{}, fmt.Errorf("server private key not found at %s", keyPath)
	}
	
	// Load certificate and private key
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load server certificate: %w", err)
	}
	
	return cert, nil
}

// loadCACertPool loads the CA certificate pool for client validation
func (cm *ConnectionManager) loadCACertPool() (*x509.CertPool, error) {
	caPath := "/etc/certs/ca.crt"
	
	// Check if CA certificate file exists
	if _, err := os.Stat(caPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("CA certificate not found at %s", caPath)
	}
	
	// Read CA certificate
	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	
	// Create certificate pool and add CA
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}
	
	return caCertPool, nil
}
