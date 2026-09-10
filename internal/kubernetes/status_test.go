package kubernetes

import (
	"bytes"
	"context"
	"io"
	"testing"

	corefake "k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestPodStatusReaderUsesCNPGHTTPSStatusPort(t *testing.T) {
	t.Parallel()
	client := corefake.NewSimpleClientset()
	client.PrependProxyReactor("pods", func(action k8stesting.Action) (bool, restclient.ResponseWrapper, error) {
		proxy, ok := action.(k8stesting.ProxyGetAction)
		if !ok {
			t.Fatalf("action = %T, want ProxyGetAction", action)
		}
		if got, want := proxy.GetScheme(), "https"; got != want {
			t.Errorf("scheme = %q, want %q", got, want)
		}
		if got, want := proxy.GetPort(), "8000"; got != want {
			t.Errorf("port = %q, want %q", got, want)
		}
		if got, want := proxy.GetPath(), "pg/status"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		return true, staticResponse{raw: []byte(`{"isWalReceiverActive":true}`)}, nil
	})

	active, err := NewPodStatusReader(client.CoreV1()).WalReceiverActive(context.Background(), "reporting", "replica-1")
	if err != nil {
		t.Fatalf("WalReceiverActive() error = %v", err)
	}
	if !active {
		t.Fatal("WalReceiverActive() = false, want true")
	}
}

type staticResponse struct{ raw []byte }

func (r staticResponse) DoRaw(context.Context) ([]byte, error) { return r.raw, nil }

func (r staticResponse) Stream(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.raw)), nil
}
