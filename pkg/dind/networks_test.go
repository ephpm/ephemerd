package dind

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
)

func TestNetworkListDefaultBridge(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/networks")
	if err != nil {
		t.Fatalf("GET /networks: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var networks []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&networks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 default network, got %d", len(networks))
	}
	if name, ok := networks[0]["Name"].(string); !ok || name != "bridge" {
		t.Errorf("Name = %v, want bridge", networks[0]["Name"])
	}
	if driver, ok := networks[0]["Driver"].(string); !ok || driver != "bridge" {
		t.Errorf("Driver = %v, want bridge", networks[0]["Driver"])
	}
}

func TestNetworkListVersioned(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/v1.45/networks")
	if err != nil {
		t.Fatalf("GET /v1.45/networks: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNetworkCreate(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{
		"Name":   "kind",
		"Driver": "bridge",
	})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /networks/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body: %s", resp.StatusCode, b)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := result["Id"].(string); !ok {
		t.Error("response missing Id field")
	}
}

func TestNetworkCreateDuplicate(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Name": "mynet"})

	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201", resp.StatusCode)
	}

	body, _ = json.Marshal(map[string]any{"Name": "mynet"})
	resp, err = client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second create: status = %d, want 409", resp.StatusCode)
	}
}

func TestNetworkCreateWithIPAM(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{
		"Name":   "kind",
		"Driver": "bridge",
		"IPAM": map[string]any{
			"Driver": "default",
			"Config": []map[string]string{
				{"Subnet": "172.19.0.0/16", "Gateway": "172.19.0.1"},
			},
		},
	})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /networks/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// Inspect to verify IPAM config was stored.
	resp2, err := client.Get("http://docker/networks/kind")
	if err != nil {
		t.Fatalf("GET /networks/kind: %v", err)
	}
	defer func() {
		if err := resp2.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	var nw map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&nw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ipam, ok := nw["IPAM"].(map[string]any)
	if !ok {
		t.Fatal("missing IPAM")
	}
	configs, ok := ipam["Config"].([]any)
	if !ok || len(configs) == 0 {
		t.Fatal("missing IPAM Config")
	}
	cfg := configs[0].(map[string]any)
	if cfg["Subnet"] != "172.19.0.0/16" {
		t.Errorf("Subnet = %v, want 172.19.0.0/16", cfg["Subnet"])
	}
	if cfg["Gateway"] != "172.19.0.1" {
		t.Errorf("Gateway = %v, want 172.19.0.1", cfg["Gateway"])
	}
}

func TestNetworkInspectByName(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Name": "testnet"})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}

	resp, err = client.Get("http://docker/networks/testnet")
	if err != nil {
		t.Fatalf("GET /networks/testnet: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var nw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&nw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if nw["Name"] != "testnet" {
		t.Errorf("Name = %v, want testnet", nw["Name"])
	}
	if nw["Driver"] != "bridge" {
		t.Errorf("Driver = %v, want bridge", nw["Driver"])
	}
}

func TestNetworkInspectNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/networks/nonexistent")
	if err != nil {
		t.Fatalf("GET /networks/nonexistent: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNetworkRemove(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Name": "removeme"})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, "http://docker/networks/removeme", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /networks/removeme: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	// Verify it's gone.
	resp2, err := client.Get("http://docker/networks/removeme")
	if err != nil {
		t.Fatalf("inspect after remove: %v", err)
	}
	defer func() {
		if err := resp2.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("after remove: status = %d, want 404", resp2.StatusCode)
	}
}

func TestNetworkRemoveDefaultBridge(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	req, _ := http.NewRequest(http.MethodDelete, "http://docker/networks/bridge", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /networks/bridge: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestNetworkListFilter(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// Create a network named "kind".
	body, _ := json.Marshal(map[string]any{"Name": "kind"})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}

	// Filter for ^kind$ — should match "kind" but not "bridge".
	filters := url.QueryEscape(`{"name":["^kind$"]}`)
	resp, err = client.Get("http://docker/networks?filters=" + filters)
	if err != nil {
		t.Fatalf("GET /networks?filters=...: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var networks []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&networks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(networks))
	}
	if networks[0]["Name"] != "kind" {
		t.Errorf("Name = %v, want kind", networks[0]["Name"])
	}
}

func TestNetworkListFilterNoMatch(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	filters := url.QueryEscape(`{"name":["^nonexistent$"]}`)
	resp, err := client.Get("http://docker/networks?filters=" + filters)
	if err != nil {
		t.Fatalf("GET /networks?filters=...: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	var networks []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&networks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(networks) != 0 {
		t.Errorf("expected 0 networks, got %d", len(networks))
	}
}

func TestNetworkConnectDisconnect(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// Create a network.
	body, _ := json.Marshal(map[string]any{
		"Name": "testnet",
		"IPAM": map[string]any{
			"Config": []map[string]string{
				{"Subnet": "172.20.0.0/16", "Gateway": "172.20.0.1"},
			},
		},
	})
	resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}

	// Insert a fake container entry directly (no containerd needed).
	fakeID := "aabbccdd11223344"
	s.mu.Lock()
	s.containers[fakeID] = &containerEntry{
		ID:       fakeID,
		Name:     "mycontainer",
		Status:   "created",
		Networks: make(map[string]containerNetworkInfo),
	}
	s.mu.Unlock()

	// Connect the container to the network.
	body, _ = json.Marshal(map[string]any{"Container": fakeID})
	resp, err = client.Post("http://docker/networks/testnet/connect", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: status = %d, want 200", resp.StatusCode)
	}

	// Inspect network to verify container is listed.
	resp, err = client.Get("http://docker/networks/testnet")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var nw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&nw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing response body: %v", err)
	}
	containers, ok := nw["Containers"].(map[string]any)
	if !ok {
		t.Fatal("Containers not a map")
	}
	if _, ok := containers[fakeID]; !ok {
		t.Errorf("container %s not found in network inspect", fakeID)
	}

	// Verify container has network info.
	s.mu.Lock()
	entry := s.containers[fakeID]
	info, hasNet := entry.Networks["testnet"]
	s.mu.Unlock()
	if !hasNet {
		t.Fatal("container missing testnet in Networks map")
	}
	if info.Gateway != "172.20.0.1" {
		t.Errorf("Gateway = %s, want 172.20.0.1", info.Gateway)
	}

	// Disconnect.
	body, _ = json.Marshal(map[string]any{"Container": fakeID})
	resp, err = client.Post("http://docker/networks/testnet/disconnect", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disconnect: status = %d, want 200", resp.StatusCode)
	}

	// Verify container no longer in network.
	s.mu.Lock()
	_, hasNet = s.containers[fakeID].Networks["testnet"]
	s.mu.Unlock()
	if hasNet {
		t.Error("container should not be in testnet after disconnect")
	}
}

func TestNetworkPrune(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// Create two networks.
	for _, name := range []string{"unused1", "unused2"} {
		body, _ := json.Marshal(map[string]any{"Name": name})
		resp, err := client.Post("http://docker/networks/create", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}

	// Prune should remove both (no containers connected).
	resp, err := client.Post("http://docker/networks/prune", "", nil)
	if err != nil {
		t.Fatalf("POST /networks/prune: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	deleted, ok := result["NetworksDeleted"].([]any)
	if !ok {
		t.Fatal("NetworksDeleted not an array")
	}
	if len(deleted) != 2 {
		t.Errorf("expected 2 deleted networks, got %d", len(deleted))
	}

	// Bridge should still exist.
	resp2, err := client.Get("http://docker/networks/bridge")
	if err != nil {
		t.Fatalf("inspect bridge: %v", err)
	}
	defer func() {
		if err := resp2.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("bridge inspect: status = %d, want 200", resp2.StatusCode)
	}
}

func TestIPAllocation(t *testing.T) {
	entry := &networkEntry{
		Subnet: "172.20.0.0/16",
		nextIP: 2,
	}

	ip1, err := allocateIP(entry)
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if ip1 != "172.20.0.2" {
		t.Errorf("first IP = %s, want 172.20.0.2", ip1)
	}

	ip2, err := allocateIP(entry)
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if ip2 != "172.20.0.3" {
		t.Errorf("second IP = %s, want 172.20.0.3", ip2)
	}
}

func TestGenerateMAC(t *testing.T) {
	mac := generateMAC("172.19.0.2")
	if mac != "02:42:ac:13:00:02" {
		t.Errorf("MAC = %s, want 02:42:ac:13:00:02", mac)
	}
}

func TestPrefixLen(t *testing.T) {
	if pl := prefixLen("172.19.0.0/16"); pl != 16 {
		t.Errorf("prefixLen = %d, want 16", pl)
	}
	if pl := prefixLen("10.0.0.0/24"); pl != 24 {
		t.Errorf("prefixLen = %d, want 24", pl)
	}
}

func TestDeriveGateway(t *testing.T) {
	gw := deriveGateway("172.19.0.0/16")
	if gw != "172.19.0.1" {
		t.Errorf("gateway = %s, want 172.19.0.1", gw)
	}
}

func TestContainerCreateAssignsNetwork(t *testing.T) {
	s := newTestServer(t)

	// Create a network named "kind".
	s.mu.Lock()
	kindNet := &networkEntry{
		ID:         "kind-net-id",
		Name:       "kind",
		Driver:     "bridge",
		Scope:      "local",
		Subnet:     "172.19.0.0/16",
		Gateway:    "172.19.0.1",
		Labels:     map[string]string{},
		Containers: map[string]string{},
		nextIP:     2,
	}
	s.networks[kindNet.ID] = kindNet
	s.mu.Unlock()

	// Simulate a container create with NetworkingConfig.
	entry := &containerEntry{
		ID:       "test-container-1",
		Name:     "kind-control-plane",
		Networks: make(map[string]containerNetworkInfo),
	}
	s.mu.Lock()
	s.containers[entry.ID] = entry
	s.assignContainerNetwork(entry, createRequest{
		NetworkingConfig: &networkingConfig{
			EndpointsConfig: map[string]*endpointSettings{
				"kind": {},
			},
		},
	})
	s.mu.Unlock()

	s.mu.Lock()
	info, ok := entry.Networks["kind"]
	s.mu.Unlock()
	if !ok {
		t.Fatal("container not assigned to 'kind' network")
	}
	if info.IPAddress != "172.19.0.2" {
		t.Errorf("IP = %s, want 172.19.0.2", info.IPAddress)
	}
	if info.Gateway != "172.19.0.1" {
		t.Errorf("Gateway = %s, want 172.19.0.1", info.Gateway)
	}
	if info.PrefixLen != 16 {
		t.Errorf("PrefixLen = %d, want 16", info.PrefixLen)
	}
}

func TestContainerCreateWithNetworkMode(t *testing.T) {
	s := newTestServer(t)

	// Create a network named "custom".
	s.mu.Lock()
	customNet := &networkEntry{
		ID:         "custom-net-id",
		Name:       "custom",
		Driver:     "bridge",
		Scope:      "local",
		Subnet:     "172.21.0.0/16",
		Gateway:    "172.21.0.1",
		Labels:     map[string]string{},
		Containers: map[string]string{},
		nextIP:     2,
	}
	s.networks[customNet.ID] = customNet
	s.mu.Unlock()

	entry := &containerEntry{
		ID:       "test-container-2",
		Name:     "worker",
		Networks: make(map[string]containerNetworkInfo),
	}
	s.mu.Lock()
	s.containers[entry.ID] = entry
	s.assignContainerNetwork(entry, createRequest{
		HostConfig: &hostConfig{NetworkMode: "custom"},
	})
	s.mu.Unlock()

	s.mu.Lock()
	info, ok := entry.Networks["custom"]
	s.mu.Unlock()
	if !ok {
		t.Fatal("container not assigned to 'custom' network")
	}
	if info.IPAddress != "172.21.0.2" {
		t.Errorf("IP = %s, want 172.21.0.2", info.IPAddress)
	}
}
