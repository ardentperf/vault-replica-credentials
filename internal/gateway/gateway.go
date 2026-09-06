// Package gateway implements the test-only TCP gateway used to connect the
// two independent Kind networks without exposing PostgreSQL through a
// Kubernetes NodePort.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Config controls a Gateway. ListenAddress is the host-network listener and
// BackendAddress is normally the source Cluster's stable -rw Service address.
type Config struct {
	ListenAddress  string
	BackendAddress string
	DialTimeout    time.Duration
	Logger         *slog.Logger
}

// Gateway is a small bidirectional TCP forwarder. It contains no PostgreSQL
// protocol logic and never logs connection contents.
type Gateway struct {
	config Config
	logger *slog.Logger

	listener net.Listener
	server   *http.Server

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// New validates configuration and constructs a Gateway. It does not open a
// listener or make a network request.
func New(config Config) (*Gateway, error) {
	if config.ListenAddress == "" {
		return nil, errors.New("gateway listen address is required")
	}
	if config.BackendAddress == "" {
		return nil, errors.New("gateway backend address is required")
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Gateway{
		config: config,
		logger: config.Logger,
		conns:  make(map[net.Conn]struct{}),
	}, nil
}

// Listen opens the TCP listener and returns the bound address. Calling Listen
// twice is an error. Starting the serving loops is the caller's responsibility
// via Serve.
func (g *Gateway) Listen() (net.Addr, error) {
	if g.listener != nil {
		return nil, errors.New("gateway listener is already open")
	}
	listener, err := net.Listen("tcp", g.config.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", g.config.ListenAddress, err)
	}
	g.listener = listener
	return listener.Addr(), nil
}

// Serve accepts connections until the listener is closed. Each connection is
// forwarded to a freshly resolved backend address, allowing Kubernetes DNS and
// the stable CNPG -rw Service to manage endpoint changes.
func (g *Gateway) Serve() error {
	if g.listener == nil {
		return errors.New("gateway listener is not open")
	}
	for {
		client, err := g.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept gateway connection: %w", err)
		}
		g.track(client)
		go g.proxy(client)
	}
}

func (g *Gateway) proxy(client net.Conn) {
	defer g.untrack(client)
	defer client.Close()

	dialer := net.Dialer{Timeout: g.config.DialTimeout}
	backend, err := dialer.Dial("tcp", g.config.BackendAddress)
	if err != nil {
		g.logger.Warn("gateway backend connection failed", "error", err)
		return
	}
	g.track(backend)
	defer g.untrack(backend)
	defer backend.Close()

	copyDone := make(chan struct{}, 2)
	go copyAndClose(backend, client, copyDone)
	go copyAndClose(client, backend, copyDone)
	<-copyDone
	// Closing both connections interrupts the other direction if one side
	// closes first. This also bounds goroutine lifetime during teardown.
	_ = client.Close()
	_ = backend.Close()
	<-copyDone
}

func copyAndClose(dst io.Writer, src io.Reader, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	done <- struct{}{}
}

func (g *Gateway) track(conn net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conns[conn] = struct{}{}
}

func (g *Gateway) untrack(conn net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns, conn)
}

// ServeHealth serves a minimal health endpoint on a separate listener. It is
// intended for a Kubernetes HTTP probe and exposes no gateway state.
func (g *Gateway) ServeHealth(ctx context.Context, address string) error {
	if address == "" {
		return errors.New("gateway health address is required")
	}

	g.server = &http.Server{Addr: address, Handler: g.healthHandler()}

	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.server.Shutdown(shutdownContext)
	}()

	err := g.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve gateway health endpoint: %w", err)
}

func (g *Gateway) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if g.listener == nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	return mux
}

// Close stops the TCP and health listeners and closes active connections.
func (g *Gateway) Close() error {
	var errs []error
	if g.listener != nil {
		errs = append(errs, g.listener.Close())
	}
	if g.server != nil {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		errs = append(errs, g.server.Shutdown(shutdownContext))
		cancel()
	}

	g.mu.Lock()
	for conn := range g.conns {
		errs = append(errs, conn.Close())
	}
	g.conns = make(map[net.Conn]struct{})
	g.mu.Unlock()

	return errors.Join(errs...)
}
