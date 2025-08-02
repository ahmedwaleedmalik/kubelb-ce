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

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8c.io/kubelb/internal/tunnel"
	"k8s.io/klog/v2"
)

func main() {
	var (
		grpcAddr       = flag.String("grpc-addr", ":9090", "gRPC server address for tunnel connections")
		httpAddr       = flag.String("http-addr", ":8080", "HTTP server address for Envoy requests")
		requestTimeout = flag.Duration("request-timeout", 30*time.Second, "Timeout for forwarded requests")
	)

	klog.InitFlags(nil)
	flag.Parse()

	ctx := context.Background()
	log := klog.FromContext(ctx)

	// Create simplified configuration - no TLS, token-based authentication only
	config := &tunnel.ConnectionManagerConfig{
		GRPCAddr:       *grpcAddr,
		HTTPAddr:       *httpAddr,
		RequestTimeout: *requestTimeout,
	}

	// Create connection manager
	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(ctx)

	manager, err := tunnel.NewConnectionManager(config)
	if err != nil {
		log.Error(err, "Failed to create connection manager")
		cancel()
		os.Exit(1)
	}

	// Start connection manager
	if err := manager.Start(ctx); err != nil {
		log.Error(err, "Failed to start connection manager")
		cancel()
		os.Exit(1)
	}

	defer cancel()

	log.Info("Connection manager started successfully",
		"grpcAddr", *grpcAddr,
		"httpAddr", *httpAddr,
		"requestTimeout", *requestTimeout,
		"authentication", "token-based")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Info("Received shutdown signal")

	// Cancel context to trigger graceful shutdown
	cancel()

	// Give some time for graceful shutdown
	time.Sleep(2 * time.Second)

	// Stop connection manager
	manager.Stop()

	log.Info("Connection manager stopped")
}
