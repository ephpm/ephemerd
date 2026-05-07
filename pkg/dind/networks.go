package dind

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// networkEntry tracks a Docker network created through the fake Docker socket.
type networkEntry struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Created    time.Time
	Subnet     string
	Gateway    string
	Labels     map[string]string
	Containers map[string]string // containerID → IP on this network
	nextIP     uint32            // monotonic counter for IP allocation within the subnet
}

// containerNetworkInfo holds per-network metadata for a container.
type containerNetworkInfo struct {
	NetworkID  string
	IPAddress  string
	Gateway    string
	MacAddress string
	PrefixLen  int
}

// initDefaultBridgeNetwork creates the default "bridge" network entry
// that Docker always exposes. Uses the real CNI subnet if a networking
// manager is configured, otherwise falls back to 10.88.0.0/16.
func (s *Server) initDefaultBridgeNetwork() {
	subnet := "10.88.0.0/16"
	gateway := "10.88.0.1"
	if s.network != nil {
		if gw := s.network.GatewayIP(); gw != "" {
			gateway = gw
			// Derive /16 subnet from gateway (e.g. 10.88.0.1 → 10.88.0.0/16).
			ip := net.ParseIP(gw)
			if ip != nil {
				ip = ip.To4()
				if ip != nil {
					subnet = fmt.Sprintf("%d.%d.0.0/16", ip[0], ip[1])
				}
			}
		}
	}

	entry := &networkEntry{
		ID:         generateContainerID(),
		Name:       "bridge",
		Driver:     "bridge",
		Scope:      "local",
		Created:    time.Now(),
		Subnet:     subnet,
		Gateway:    gateway,
		Labels:     map[string]string{},
		Containers: map[string]string{},
		nextIP:     2,
	}
	s.networks[entry.ID] = entry
}

// resolveNetwork finds a network by exact ID, name, or ID prefix.
// Must be called with s.mu held.
func (s *Server) resolveNetwork(idOrName string) *networkEntry {
	if entry, ok := s.networks[idOrName]; ok {
		return entry
	}
	for _, entry := range s.networks {
		if entry.Name == idOrName {
			return entry
		}
	}
	for id, entry := range s.networks {
		if strings.HasPrefix(id, idOrName) {
			return entry
		}
	}
	return nil
}

// defaultNetwork returns the "bridge" network entry.
// Must be called with s.mu held.
func (s *Server) defaultNetwork() *networkEntry {
	for _, entry := range s.networks {
		if entry.Name == "bridge" {
			return entry
		}
	}
	return nil
}

// allocateIP returns the next available IP address from a network's subnet.
func allocateIP(entry *networkEntry) (string, error) {
	_, ipNet, err := net.ParseCIDR(entry.Subnet)
	if err != nil {
		return "", fmt.Errorf("parsing subnet %s: %w", entry.Subnet, err)
	}

	base := binary.BigEndian.Uint32(ipNet.IP.To4())
	ip := base + entry.nextIP
	entry.nextIP++

	result := make(net.IP, 4)
	binary.BigEndian.PutUint32(result, ip)
	return result.String(), nil
}

// generateMAC derives a Docker-style MAC address from an IP (02:42:xx:xx:xx:xx).
func generateMAC(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "02:42:00:00:00:02"
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "02:42:00:00:00:02"
	}
	return fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// prefixLen extracts the CIDR prefix length from a subnet string.
func prefixLen(subnet string) int {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return 16
	}
	ones, _ := ipNet.Mask.Size()
	return ones
}

// routeNetwork dispatches /networks/{id}/{action} requests.
func (s *Server) routeNetwork(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/networks/")
	parts := strings.SplitN(rest, "/", 2)
	idOrName := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleNetworkInspect(w, r, idOrName)
		case http.MethodDelete:
			s.handleNetworkRemove(w, r, idOrName)
		default:
			s.handleNotImplemented(w, r)
		}
		return
	}

	action := parts[1]
	switch {
	case action == "connect" && r.Method == http.MethodPost:
		s.handleNetworkConnect(w, r, idOrName)
	case action == "disconnect" && r.Method == http.MethodPost:
		s.handleNetworkDisconnect(w, r, idOrName)
	default:
		s.handleNotImplemented(w, r)
	}
}

func (s *Server) handleNetworkList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Parse filters from query string. Docker CLI sends:
	//   ?filters={"name":["^kind$"]}
	var nameFilters []string
	if raw := r.URL.Query().Get("filters"); raw != "" {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(raw), &filters); err == nil {
			nameFilters = filters["name"]
		}
	}

	result := make([]map[string]any, 0, len(s.networks))
	for _, entry := range s.networks {
		if len(nameFilters) > 0 && !matchesAny(entry.Name, nameFilters) {
			continue
		}
		result = append(result, networkToJSON(entry))
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string            `json:"Name"`
		Driver         string            `json:"Driver"`
		CheckDuplicate bool              `json:"CheckDuplicate"`
		Labels         map[string]string `json:"Labels"`
		IPAM           *struct {
			Driver string `json:"Driver"`
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
				IPRange string `json:"IPRange"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "network name is required",
		})
		return
	}

	if req.Driver == "" {
		req.Driver = "bridge"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range s.networks {
		if entry.Name == req.Name {
			writeJSON(w, http.StatusConflict, map[string]string{
				"message": fmt.Sprintf("network with name %s already exists", req.Name),
			})
			return
		}
	}

	subnet := "172.18.0.0/16"
	var gateway string
	if req.IPAM != nil && len(req.IPAM.Config) > 0 {
		if req.IPAM.Config[0].Subnet != "" {
			subnet = req.IPAM.Config[0].Subnet
		}
		if req.IPAM.Config[0].Gateway != "" {
			gateway = req.IPAM.Config[0].Gateway
		} else {
			gateway = deriveGateway(subnet)
		}
	} else {
		// Auto-assign a subnet that doesn't conflict with existing networks.
		subnet, gateway = s.pickFreeSubnet()
	}

	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	entry := &networkEntry{
		ID:         generateContainerID(),
		Name:       req.Name,
		Driver:     req.Driver,
		Scope:      "local",
		Created:    time.Now(),
		Subnet:     subnet,
		Gateway:    gateway,
		Labels:     labels,
		Containers: map[string]string{},
		nextIP:     2,
	}
	s.networks[entry.ID] = entry

	s.log.Info("network created", "name", req.Name, "id", entry.ID[:12], "subnet", subnet)

	writeJSON(w, http.StatusCreated, map[string]any{
		"Id":      entry.ID,
		"Warning": "",
	})
}

func (s *Server) handleNetworkInspect(w http.ResponseWriter, r *http.Request, idOrName string) {
	idOrName, _ = url.PathUnescape(idOrName)

	s.mu.Lock()
	entry := s.resolveNetwork(idOrName)
	if entry == nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("network %s not found", idOrName),
		})
		return
	}

	// Build container map with endpoint details.
	containers := make(map[string]any, len(entry.Containers))
	pLen := prefixLen(entry.Subnet)
	for cID, ip := range entry.Containers {
		name := ""
		if ce, ok := s.containers[cID]; ok {
			name = ce.Name
		}
		containers[cID] = map[string]any{
			"Name":        name,
			"EndpointID":  generateContainerID(),
			"MacAddress":  generateMAC(ip),
			"IPv4Address": fmt.Sprintf("%s/%d", ip, pLen),
			"IPv6Address": "",
		}
	}
	s.mu.Unlock()

	resp := map[string]any{
		"Id":         entry.ID,
		"Name":       entry.Name,
		"Created":    entry.Created.Format(time.RFC3339Nano),
		"Scope":      entry.Scope,
		"Driver":     entry.Driver,
		"EnableIPv6": false,
		"IPAM": map[string]any{
			"Driver":  "default",
			"Options": map[string]any{},
			"Config": []map[string]any{
				{
					"Subnet":  entry.Subnet,
					"Gateway": entry.Gateway,
				},
			},
		},
		"Internal":   false,
		"Attachable": false,
		"Ingress":    false,
		"Containers": containers,
		"Options":    map[string]any{},
		"Labels":     entry.Labels,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNetworkConnect(w http.ResponseWriter, r *http.Request, idOrName string) {
	var req struct {
		Container      string `json:"Container"`
		EndpointConfig *struct {
			IPAMConfig *struct {
				IPv4Address string `json:"IPv4Address"`
			} `json:"IPAMConfig"`
		} `json:"EndpointConfig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nw := s.resolveNetwork(idOrName)
	if nw == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("network %s not found", idOrName),
		})
		return
	}

	containerID := s.resolveContainerIDLocked(req.Container)
	entry, ok := s.containers[containerID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", req.Container),
		})
		return
	}

	// Determine IP: use requested IP or allocate one.
	var ip string
	if req.EndpointConfig != nil && req.EndpointConfig.IPAMConfig != nil && req.EndpointConfig.IPAMConfig.IPv4Address != "" {
		ip = req.EndpointConfig.IPAMConfig.IPv4Address
	} else {
		var err error
		ip, err = allocateIP(nw)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"message": fmt.Sprintf("allocating IP: %v", err),
			})
			return
		}
	}

	nw.Containers[containerID] = ip

	if entry.Networks == nil {
		entry.Networks = map[string]containerNetworkInfo{}
	}
	entry.Networks[nw.Name] = containerNetworkInfo{
		NetworkID:  nw.ID,
		IPAddress:  ip,
		Gateway:    nw.Gateway,
		MacAddress: generateMAC(ip),
		PrefixLen:  prefixLen(nw.Subnet),
	}

	// Update the legacy IP field to the most recently connected network.
	entry.IP = ip

	s.log.Info("container connected to network", "container", containerID[:12], "network", nw.Name, "ip", ip)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleNetworkDisconnect(w http.ResponseWriter, r *http.Request, idOrName string) {
	var req struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nw := s.resolveNetwork(idOrName)
	if nw == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("network %s not found", idOrName),
		})
		return
	}

	containerID := s.resolveContainerIDLocked(req.Container)
	entry, ok := s.containers[containerID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("container %s not found", req.Container),
		})
		return
	}

	delete(nw.Containers, containerID)
	delete(entry.Networks, nw.Name)

	s.log.Info("container disconnected from network", "container", containerID[:12], "network", nw.Name)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleNetworkRemove(w http.ResponseWriter, r *http.Request, idOrName string) {
	idOrName, _ = url.PathUnescape(idOrName)

	s.mu.Lock()
	defer s.mu.Unlock()

	nw := s.resolveNetwork(idOrName)
	if nw == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"message": fmt.Sprintf("network %s not found", idOrName),
		})
		return
	}

	if nw.Name == "bridge" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"message": "bridge is a pre-defined network and cannot be removed",
		})
		return
	}

	// Remove network associations from connected containers.
	for cID := range nw.Containers {
		if ce, ok := s.containers[cID]; ok {
			delete(ce.Networks, nw.Name)
		}
	}

	delete(s.networks, nw.ID)
	s.log.Info("network removed", "name", nw.Name, "id", nw.ID[:12])

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNetworkPrune(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deleted []string
	for id, entry := range s.networks {
		if entry.Name == "bridge" {
			continue
		}
		if len(entry.Containers) == 0 {
			deleted = append(deleted, entry.Name)
			delete(s.networks, id)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"NetworksDeleted": deleted,
	})
}

// resolveContainerIDLocked resolves a name or short ID to a full container ID.
// Must be called with s.mu held.
func (s *Server) resolveContainerIDLocked(nameOrID string) string {
	if _, ok := s.containers[nameOrID]; ok {
		return nameOrID
	}
	for id, entry := range s.containers {
		if entry.Name == nameOrID {
			return id
		}
	}
	for id := range s.containers {
		if strings.HasPrefix(id, nameOrID) {
			return id
		}
	}
	return nameOrID
}

// removeContainerFromNetworks removes a container from all network entries.
// Must be called with s.mu held.
func (s *Server) removeContainerFromNetworks(containerID string) {
	for _, nw := range s.networks {
		delete(nw.Containers, containerID)
	}
}

// networkToJSON converts a networkEntry to the Docker API JSON format.
func networkToJSON(entry *networkEntry) map[string]any {
	return map[string]any{
		"Id":         entry.ID,
		"Name":       entry.Name,
		"Created":    entry.Created.Format(time.RFC3339Nano),
		"Scope":      entry.Scope,
		"Driver":     entry.Driver,
		"EnableIPv6": false,
		"IPAM": map[string]any{
			"Driver":  "default",
			"Options": map[string]any{},
			"Config": []map[string]any{
				{
					"Subnet":  entry.Subnet,
					"Gateway": entry.Gateway,
				},
			},
		},
		"Internal":   false,
		"Attachable": false,
		"Ingress":    false,
		"Containers": entry.Containers,
		"Options":    map[string]any{},
		"Labels":     entry.Labels,
	}
}

// matchesAny returns true if name matches any of the given regex patterns.
func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := regexp.MatchString(pattern, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}

// deriveGateway returns the first usable IP (.1) from a CIDR subnet.
func deriveGateway(subnet string) string {
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "172.18.0.1"
	}
	v4 := ip.To4()
	if v4 == nil {
		return "172.18.0.1"
	}
	v4[3] = 1
	return v4.String()
}

// pickFreeSubnet returns a subnet/gateway pair that doesn't conflict with
// any existing network in the store. Must be called with s.mu held.
func (s *Server) pickFreeSubnet() (subnet, gateway string) {
	used := map[string]bool{}
	for _, entry := range s.networks {
		used[entry.Subnet] = true
	}
	for octet := 18; octet < 255; octet++ {
		candidate := fmt.Sprintf("172.%d.0.0/16", octet)
		if !used[candidate] {
			return candidate, fmt.Sprintf("172.%d.0.1", octet)
		}
	}
	return "172.255.0.0/16", "172.255.0.1"
}
