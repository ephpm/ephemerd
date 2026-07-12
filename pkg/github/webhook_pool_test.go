package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const poolURL = "https://ci-linux-amd64.example.com/webhook/github"

func TestRegisterWebhooks_PoolAdoptsExistingRepoHook(t *testing.T) {
	mux := http.NewServeMux()
	edited := false

	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			resp := []map[string]any{
				{"id": 111, "events": []string{"workflow_job"}, "config": map[string]any{"url": "https://other-pool.example.com/webhook/github"}},
				{"id": 222, "events": []string{"workflow_job"}, "config": map[string]any{"url": poolURL}},
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			t.Error("pool mode must adopt the same-URL hook, not create a duplicate")
			w.WriteHeader(422)
		}
	})
	mux.HandleFunc("/repos/testorg/repo1/hooks/222", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		edited = true
		if err := json.NewEncoder(w).Encode(map[string]any{"id": 222}); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true

	hooks, err := c.RegisterWebhooks(context.Background(), poolURL, "shared-secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if len(hooks) != 1 || hooks[0].HookID != 222 {
		t.Fatalf("expected adoption of hook 222, got %+v", hooks)
	}
	if !edited {
		t.Error("adopted hook was not edited to converge config")
	}
}

func TestRegisterWebhooks_PoolCreatesWhenAbsent(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if err := json.NewEncoder(w).Encode([]map[string]any{}); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": 333}); err != nil {
				t.Logf("encoding: %v", err)
			}
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true

	hooks, err := c.RegisterWebhooks(context.Background(), poolURL, "shared-secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if len(hooks) != 1 || hooks[0].HookID != 333 {
		t.Fatalf("expected fresh hook 333, got %+v", hooks)
	}
}

func TestRegisterWebhooks_PoolAdoptsAfterCreateRace(t *testing.T) {
	mux := http.NewServeMux()
	listCalls := 0

	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listCalls++
			if listCalls == 1 {
				// First list: nothing there yet.
				if err := json.NewEncoder(w).Encode([]map[string]any{}); err != nil {
					t.Logf("encoding: %v", err)
				}
				return
			}
			// After the racing pool-mate wins, the hook exists.
			resp := []map[string]any{
				{"id": 444, "events": []string{"workflow_job"}, "config": map[string]any{"url": poolURL}},
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			// GitHub rejects exact-duplicate hook config with 422.
			w.WriteHeader(422)
			if err := json.NewEncoder(w).Encode(map[string]any{"message": "Hook already exists on this repository"}); err != nil {
				t.Logf("encoding: %v", err)
			}
		}
	})
	mux.HandleFunc("/repos/testorg/repo1/hooks/444", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"id": 444}); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true

	hooks, err := c.RegisterWebhooks(context.Background(), poolURL, "shared-secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if len(hooks) != 1 || hooks[0].HookID != 444 {
		t.Fatalf("expected race-adoption of hook 444, got %+v", hooks)
	}
}

func TestDeregisterWebhooks_PoolIsNoop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/hooks/999", func(w http.ResponseWriter, r *http.Request) {
		t.Error("pool mode must not delete the shared webhook on shutdown")
		w.WriteHeader(204)
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true

	c.DeregisterWebhooks(context.Background(), []ManagedWebhook{{Repo: "repo1", HookID: 999}})
}

func TestCleanStaleWebhooks_PoolIsNoop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		t.Error("pool mode must not sweep webhooks (they belong to live pool-mates)")
		if err := json.NewEncoder(w).Encode([]map[string]any{}); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true

	c.CleanStaleWebhooks(context.Background())
}

func TestRegisterWebhooks_PoolOrgLevelAdopts(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/orgs/testorg/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			resp := []map[string]any{
				{"id": 555, "events": []string{"workflow_job"}, "config": map[string]any{"url": poolURL}},
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			t.Error("pool mode must adopt the same-URL org hook, not create a duplicate")
			w.WriteHeader(422)
		}
	})
	mux.HandleFunc("/orgs/testorg/hooks/555", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"id": 555}); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.PoolMode = true
	c.cfg.Repos = nil

	hooks, err := c.RegisterWebhooks(context.Background(), poolURL, "shared-secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if len(hooks) != 1 || hooks[0].HookID != 555 {
		t.Fatalf("expected adoption of org hook 555, got %+v", hooks)
	}
}
