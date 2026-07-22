package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func TestListModelsCachesSuccessfulFetch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "model-a", "object": "model"},
			},
		})
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		client:      client,
		modelClient: make(map[string]*openai.Client),
	}

	ctx := context.Background()
	first, err := p.listModels(ctx)
	if err != nil {
		t.Fatalf("first listModels: %v", err)
	}
	if len(first) != 1 || first[0].ID != "model-a" {
		t.Fatalf("unexpected first result: %+v", first)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 hit after first call, got %d", got)
	}

	second, err := p.listModels(ctx)
	if err != nil {
		t.Fatalf("second listModels: %v", err)
	}
	if len(second) != 1 || second[0].ID != "model-a" {
		t.Fatalf("unexpected second result: %+v", second)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected cache hit (still 1 request), got %d", got)
	}
}

func TestFetchModelsQueriesOpenCodeEndpointsInParallel(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	handler := func(id string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				http.NotFound(w, r)
				return
			}
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": id, "object": "model"},
				},
			})
		}
	}

	zenSrv := httptest.NewServer(handler("zen-model"))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(handler("go-model"))
	defer goSrv.Close()

	zenClient := openai.NewClient(
		option.WithBaseURL(zenSrv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(zenSrv.Client()),
	)
	goClient := openai.NewClient(
		option.WithBaseURL(goSrv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(goSrv.Client()),
	)
	p := &OpenAIProvider{
		zenClient:   &zenClient,
		goClient:    &goClient,
		modelClient: make(map[string]*openai.Client),
	}

	errCh := make(chan error, 1)
	var models []openai.Model
	go func() {
		var err error
		models, _, err = p.fetchModels(context.Background())
		errCh <- err
	}()

	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("endpoints were not queried concurrently")
		}
	}
	close(release)

	if err := <-errCh; err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d (%v)", len(models), models)
	}
}

func TestListModelsHonorsCallerDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		client:      client,
		modelClient: make(map[string]*openai.Client),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.listModels(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected deadline error from hung /models")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("listModels ignored caller deadline: took %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server never saw /models request")
	}
}
