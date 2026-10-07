package providertest

import (
	"net/http"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func TestDisposableRequestOriginAndBudgetRefuseBeforeNetwork(t *testing.T) {
	for _, url := range []string{"http://remote.invalid:9000", "https://127.0.0.1:9000", "http://127.0.0.1:9001"} {
		t.Run(url, func(t *testing.T) {
			client := &disposableHTTP{origin: "http://127.0.0.1:9000"}
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(r)
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || client.requests != 0 {
				t.Fatal("disallowed request reached network or request counter", err)
			}
		})
	}
	client := &disposableHTTP{origin: "http://127.0.0.1:9000", requests: 4096}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9000", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(r)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || client.requests != 4096 {
		t.Fatal("exhausted budget opened network or counted an unsent attempt", err)
	}
}
