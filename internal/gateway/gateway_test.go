package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGatewayForwardsBidirectionally(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()

	backendDone := make(chan error, 1)
	go func() {
		conn, acceptErr := backendListener.Accept()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		defer conn.Close()
		if _, writeErr := io.WriteString(conn, "backend-ready"); writeErr != nil {
			backendDone <- writeErr
			return
		}
		buffer := make([]byte, len("client-data"))
		if _, readErr := io.ReadFull(conn, buffer); readErr != nil {
			backendDone <- readErr
			return
		}
		if string(buffer) != "client-data" {
			backendDone <- fmt.Errorf("backend received %q", string(buffer))
			return
		}
		_, writeErr := io.WriteString(conn, "backend-data")
		backendDone <- writeErr
	}()

	proxy, err := New(Config{
		ListenAddress:  "127.0.0.1:0",
		BackendAddress: backendListener.Addr().String(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve() }()
	defer func() {
		_ = proxy.Close()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("gateway serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("gateway did not stop")
		}
	}()

	client, err := net.Dial("tcp", proxy.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(time.Second))

	ready := make([]byte, len("backend-ready"))
	if _, err := io.ReadFull(client, ready); err != nil {
		t.Fatal(err)
	}
	if string(ready) != "backend-ready" {
		t.Fatalf("client received %q", string(ready))
	}
	if _, err := io.WriteString(client, "client-data"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("backend-data"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "backend-data" {
		t.Fatalf("client received %q", string(response))
	}
	if err := <-backendDone; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayHealth(t *testing.T) {
	proxy, err := New(Config{ListenAddress: "127.0.0.1:0", BackendAddress: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Listen(); err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/healthz", want: 200},
		{path: "/readyz", want: 200},
	} {
		recorder := httptest.NewRecorder()
		proxy.healthHandler().ServeHTTP(recorder, httptest.NewRequest("GET", test.path, nil))
		if recorder.Code != test.want {
			t.Errorf("GET %s: status %d, want %d", test.path, recorder.Code, test.want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	healthDone := make(chan error, 1)
	go func() { healthDone <- proxy.ServeHealth(ctx, "127.0.0.1:0") }()

	// ServeHealth uses an ephemeral address only for the unit's lifecycle
	// check; the HTTP integration behavior is exercised by the handler in the
	// real Deployment. The listener lifecycle must still stop cleanly.
	cancel()
	select {
	case err := <-healthDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health server did not stop")
	}
}
