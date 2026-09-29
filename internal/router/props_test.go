package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// llama.cpp servers commonly omit n_ctx from /models but publish the
// serving window on /props. The probe must find it there.
func TestProbeFallsBackToLlamaProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model"}]}`))
		case "/props":
			w.Write([]byte(`{"n_ctx":32768,"webui":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	caps, err := ProbeOpenAI(context.Background(), srv.URL+"/v1", "", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ContextLength != 32768 {
		t.Fatalf("want n_ctx 32768 from /props, got %d", caps.ContextLength)
	}
}

func TestProbePropsAbsentIsZeroNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model"}]}`))
			return
		}
		http.NotFound(w, r) // no /props (e.g. OpenRouter)
	}))
	defer srv.Close()

	caps, err := ProbeOpenAI(context.Background(), srv.URL+"/v1", "", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ContextLength != 0 {
		t.Fatalf("want 0, got %d", caps.ContextLength)
	}
}

func TestProbeModelsFieldBeatsProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","n_ctx":8192}]}`))
		case "/props":
			t.Error("/props must not be consulted when /models already answers")
		}
	}))
	defer srv.Close()

	caps, err := ProbeOpenAI(context.Background(), srv.URL+"/v1", "", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ContextLength != 8192 {
		t.Fatalf("want 8192 from /models, got %d", caps.ContextLength)
	}
}

// Real llama.cpp builds may only publish n_ctx nested under
// default_generation_settings.params — the probe must find it there.
func TestProbePropsNestedParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model"}]}`))
		case "/props":
			w.Write([]byte(`{"default_generation_settings":{"params":{"n_ctx":16384,"seed":42}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	caps, err := ProbeOpenAI(context.Background(), srv.URL+"/v1", "", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ContextLength != 16384 {
		t.Fatalf("want nested n_ctx 16384, got %d", caps.ContextLength)
	}
}
