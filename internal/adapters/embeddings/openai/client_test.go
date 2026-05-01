package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedDimServer returns a stub /v1/embeddings server that emits a
// deterministic vector of the requested dimensionality for each input.
func fixedDimServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model == "" {
			t.Errorf("expected model in request")
		}

		resp := embedResponse{Object: "list", Model: req.Model}
		for i, txt := range req.Input {
			vec := make([]float32, dim)
			// Deterministic but distinct: vec[0] depends on the input,
			// rest is constant. Useful for asserting which input got
			// which vector.
			vec[0] = float32(len(txt))
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Object: "embedding", Index: i, Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestEmbedSingle(t *testing.T) {
	srv := fixedDimServer(t, 4)
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "test-model", time.Second, nil)
	vecs, err := c.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("want 1 vector, got %d", len(vecs))
	}
	if len(vecs[0]) != 4 {
		t.Errorf("dim: want 4 got %d", len(vecs[0]))
	}
	if vecs[0][0] != 5 { // len("hello")
		t.Errorf("vec[0]: want 5 got %f", vecs[0][0])
	}
}

func TestEmbedBatch(t *testing.T) {
	srv := fixedDimServer(t, 3)
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m", time.Second, nil)
	vecs, err := c.Embed(context.Background(), []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(vecs))
	}
	if vecs[0][0] != 1 || vecs[1][0] != 2 || vecs[2][0] != 3 {
		t.Errorf("ordering broken: vec[0][0]=%v vec[1][0]=%v vec[2][0]=%v",
			vecs[0][0], vecs[1][0], vecs[2][0])
	}
}

func TestEmbedEmptyInput(t *testing.T) {
	c := New("http://localhost:1", "", "m", time.Second, nil)
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil {
		t.Errorf("empty input should not error, got %v", err)
	}
	if vecs != nil {
		t.Errorf("expected nil result, got %v", vecs)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always returns one entry regardless of input length.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m", time.Second, nil)
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("expected count mismatch error, got %v", err)
	}
}

func TestEmbedAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "secret-key", "m", time.Second, nil)
	_, _ = c.Embed(context.Background(), []string{"x"})
	if gotAuth != "Bearer secret-key" {
		t.Errorf("auth header: got %q want %q", gotAuth, "Bearer secret-key")
	}
}

func TestEmbedNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m", time.Second, nil)
	_, _ = c.Embed(context.Background(), []string{"x"})
	if gotAuth != "" {
		t.Errorf("auth header should be unset when key is empty, got %q", gotAuth)
	}
}

func TestEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"auth"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m", time.Second, nil)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("expected wrapped server message, got %v", err)
	}
}

func TestEmbedReturnsOrderedByIndex(t *testing.T) {
	// Server returns data in reversed order with explicit indices;
	// client must reorder.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object":"list","model":"m",
			"data":[
				{"object":"embedding","index":2,"embedding":[3.0]},
				{"object":"embedding","index":0,"embedding":[1.0]},
				{"object":"embedding","index":1,"embedding":[2.0]}
			]
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "m", time.Second, nil)
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if vecs[0][0] != 1 || vecs[1][0] != 2 || vecs[2][0] != 3 {
		t.Errorf("expected vectors reordered by index, got %v", vecs)
	}
}

func TestProbeSetsDim(t *testing.T) {
	srv := fixedDimServer(t, 1536)
	defer srv.Close()

	c := New(srv.URL+"/v1", "", "test-embedding-3-small", time.Second, nil)
	if c.Dim() != 0 {
		t.Errorf("dim before probe: want 0 got %d", c.Dim())
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if c.Dim() != 1536 {
		t.Errorf("dim after probe: want 1536 got %d", c.Dim())
	}
}

func TestProbeFailsOnUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1/v1", "", "m", 100*time.Millisecond, nil)
	err := c.Probe(context.Background())
	if err == nil {
		t.Errorf("expected probe failure on unreachable host")
	}
}
