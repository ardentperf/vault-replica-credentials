// cnpg-test-gateway is the E2E-only host-network TCP gateway. It forwards
// connections to a stable CNPG -rw Service and contains no PostgreSQL logic.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ardentperf/vault-replica-credentials/internal/gateway"
)

func main() {
	var (
		listenAddress  string
		backendAddress string
		healthAddress  string
	)
	flag.StringVar(&listenAddress, "listen-address", valueOrEnv("GATEWAY_LISTEN_ADDRESS", ":15432"), "TCP listen address")
	flag.StringVar(&backendAddress, "backend-address", valueOrEnv("GATEWAY_BACKEND_ADDRESS", ""), "CNPG -rw Service address")
	flag.StringVar(&healthAddress, "health-address", valueOrEnv("GATEWAY_HEALTH_ADDRESS", ":18081"), "HTTP health listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	proxy, err := gateway.New(gateway.Config{
		ListenAddress:  listenAddress,
		BackendAddress: backendAddress,
		Logger:         logger,
	})
	if err != nil {
		logger.Error("invalid gateway configuration", "error", err)
		os.Exit(2)
	}
	if _, err := proxy.Listen(); err != nil {
		logger.Error("unable to start gateway listener", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve() }()
	healthDone := make(chan error, 1)
	go func() { healthDone <- proxy.ServeHealth(ctx, healthAddress) }()

	var serveErr error
	select {
	case serveErr = <-serveDone:
		stop()
	case <-ctx.Done():
		_ = proxy.Close()
		serveErr = <-serveDone
	}
	_ = proxy.Close()
	if serveErr != nil {
		logger.Error("gateway stopped", "error", serveErr)
		os.Exit(1)
	}
	if err := <-healthDone; err != nil {
		logger.Error("health server stopped", "error", err)
		os.Exit(1)
	}
}

func valueOrEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
