package threexui

import (
	"ffxban/internal/config"
	"fmt"
	"log"
	"sync"
)

type Manager struct {
	clients []*Client
	mu      sync.RWMutex
}

func NewManager(cfg *config.Config) *Manager {
	mgr := &Manager{
		clients: make([]*Client, 0, len(cfg.ThreexuiServers)),
	}

	for _, srv := range cfg.ThreexuiServers {
		client, err := NewClient(srv.Name, srv.URL, srv.Username, srv.Password)
		if err != nil {
			log.Printf("Failed to create 3x-ui client for %s: %v", srv.Name, err)
			continue
		}
		mgr.clients = append(mgr.clients, client)
	}

	return mgr
}

func (m *Manager) GetClients() []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients
}

type AggregatedClient struct {
	ClientData
	ServerName string `json:"serverName"`
}

func (m *Manager) GetAllUsers() ([]AggregatedClient, error) {
	var allUsers []AggregatedClient
	var errs []error

	// TODO: Parallel execution
	for _, client := range m.GetClients() {
		users, err := client.GetClients()
		if err != nil {
			log.Printf("Error fetching users from %s: %v", client.Name, err)
			errs = append(errs, err)
			continue
		}

		for _, u := range users {
			allUsers = append(allUsers, AggregatedClient{
				ClientData: u,
				ServerName: client.Name,
			})
		}
	}

	if len(allUsers) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("failed to fetch users from any server")
	}

	return allUsers, nil
}

// BlockUser disables the user with the given email on ALL servers where they exist.
func (m *Manager) BlockUser(email string) error {
	found := false
	var lastErr error

	for _, client := range m.GetClients() {
		// We need to find the user's UUID and InboundID on this server first
		// This requires fetching the list. Ideally we should cache this.
		// For now, let's fetch.
		users, err := client.GetClients()
		if err != nil {
			log.Printf("Error fetching users from %s during block: %v", client.Name, err)
			lastErr = err
			continue
		}

		for _, u := range users {
			if u.Email == email {
				if !u.Enable {
					continue // Already disabled
				}
				
				// Found user, disable them
				// We need to reconstruct settings map or use the one we got (which might be incomplete in my ClientData struct)
				// Wait, ClientData in client.go has RawSettings.
				// We need to expose RawSettings or pass it through.
				// I added RawSettings to ClientData in previous step.
				
				// Use RawSettings from ClientData
				// Note: u.RawSettings is map[string]interface{}
				
				log.Printf("Disabling user %s on server %s", email, client.Name)
				if err := client.DisableClient(u.InboundID, u.ID, u.RawSettings); err != nil {
					log.Printf("Failed to disable user %s on %s: %v", email, client.Name, err)
					lastErr = err
				} else {
					found = true
				}
			}
		}
	}

	if !found && lastErr != nil {
		return lastErr
	}
	if !found {
		return fmt.Errorf("user %s not found on any active server", email)
	}
	return nil
}

// UnblockUser enables the user with the given email on ALL servers where they exist.
func (m *Manager) UnblockUser(email string) error {
	found := false
	var lastErr error

	for _, client := range m.GetClients() {
		users, err := client.GetClients()
		if err != nil {
			log.Printf("Error fetching users from %s during unblock: %v", client.Name, err)
			lastErr = err
			continue
		}

		for _, u := range users {
			if u.Email == email {
				if u.Enable {
					continue // Already enabled
				}
				
				log.Printf("Enabling user %s on server %s", email, client.Name)
				if err := client.EnableClient(u.InboundID, u.ID, u.RawSettings); err != nil {
					log.Printf("Failed to enable user %s on %s: %v", email, client.Name, err)
					lastErr = err
				} else {
					found = true
				}
			}
		}
	}

	if !found && lastErr != nil {
		return lastErr
	}
	if !found {
		return fmt.Errorf("user %s not found on any active server", email)
	}
	return nil
}
