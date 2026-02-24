package threexui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Name     string
	BaseURL  string
	Username string
	Password string
	client   *http.Client
	mu       sync.Mutex
}

type ClientData struct {
	InboundID int    `json:"inboundId"`
	ID        string `json:"id"` // UUID
	Email     string `json:"email"`
	Enable    bool   `json:"enable"`
	Limit     int64  `json:"totalGB"` // Traffic limit in bytes
	LimitIp   int    `json:"limitIp"` // IP limit count
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	ExpiryTime int64 `json:"expiryTime"`
	Remark    string `json:"remark"`
	// Internal fields for update
	RawSettings map[string]interface{} `json:"-"`
}

func NewClient(name, baseURL, username, password string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	return &Client{
		Name:     name,
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		client: &http.Client{
			Jar:     jar,
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (c *Client) Login() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	values := url.Values{}
	values.Set("username", c.Username)
	values.Set("password", c.Password)

	resp, err := c.client.PostForm(c.BaseURL+"/login", values)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("login failed with status: %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if success, ok := result["success"].(bool); !ok || !success {
		return fmt.Errorf("login failed: %v", result["msg"])
	}

	return nil
}

func (c *Client) ensureLoggedIn() error {
	// Simple check: try to list inbounds, if 401/403 then login
	// Optimistic approach: assume logged in, retry if failed
	return nil
}

type apiResponse struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

func (c *Client) GetClients() ([]ClientData, error) {
	return c.getClientsWithRetry(true)
}

func (c *Client) getClientsWithRetry(retry bool) ([]ClientData, error) {
	resp, err := c.client.Get(c.BaseURL + "/panel/api/inbounds/list")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		if retry {
			if err := c.Login(); err != nil {
				return nil, fmt.Errorf("re-login failed: %w", err)
			}
			return c.getClientsWithRetry(false)
		}
		if resp.StatusCode == 404 {
			return nil, fmt.Errorf("api endpoint not found (404), check url or auth")
		}
		return nil, fmt.Errorf("unauthorized")
	}

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if !result.Success {
		return nil, fmt.Errorf("api error: %s", result.Msg)
	}

	var inbounds []map[string]interface{}
	if err := json.Unmarshal(result.Obj, &inbounds); err != nil {
		return nil, err
	}

	var clients []ClientData

	for _, inbound := range inbounds {
		inboundID := int(inbound["id"].(float64))
		
		settingsStr, ok := inbound["settings"].(string)
		if !ok {
			continue
		}

		var settings map[string]interface{}
		if err := json.Unmarshal([]byte(settingsStr), &settings); err != nil {
			continue
		}

		clientsInterface, ok := settings["clients"].([]interface{})
		if !ok {
			continue
		}

		// Map client stats by email/id
		clientStats, _ := inbound["clientStats"].([]interface{})
		statsMap := make(map[string]map[string]interface{})
		for _, s := range clientStats {
			if stat, ok := s.(map[string]interface{}); ok {
				email, _ := stat["email"].(string)
				statsMap[email] = stat
			}
		}

		for _, cli := range clientsInterface {
			clientMap := cli.(map[string]interface{})
			email, _ := clientMap["email"].(string)
			id, _ := clientMap["id"].(string)
			enable, _ := clientMap["enable"].(bool)
			// Handle case where enable is missing (default true)
			if _, exists := clientMap["enable"]; !exists {
				enable = true
			}
			
			limit, _ := clientMap["totalGB"].(float64)
			limitIp, _ := clientMap["limitIp"].(float64)
			expiry, _ := clientMap["expiryTime"].(float64)
			
			// Stats
			var up, down int64
			if stat, ok := statsMap[email]; ok {
				up = int64(stat["up"].(float64))
				down = int64(stat["down"].(float64))
				// Prefer stats total if available/reliable? No, settings limit is limit.
			}

			clients = append(clients, ClientData{
				InboundID:   inboundID,
				ID:          id,
				Email:       email,
				Enable:      enable,
				Limit:       int64(limit),
				LimitIp:     int(limitIp),
				Up:          up,
				Down:        down,
				ExpiryTime:  int64(expiry),
				RawSettings: clientMap,
			})
		}
	}

	return clients, nil
}

func (c *Client) UpdateClient(inboundID int, clientUUID string, clientSettings map[string]interface{}) error {
	return c.updateClientWithRetry(inboundID, clientUUID, clientSettings, true)
}

func (c *Client) updateClientWithRetry(inboundID int, clientUUID string, clientSettings map[string]interface{}, retry bool) error {
	settingsBytes, err := json.Marshal(clientSettings)
	if err != nil {
		return err
	}

	payload := map[string]interface{}{
		"id":       inboundID,
		"settings": string(settingsBytes),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/panel/api/inbounds/updateClient/%s", c.BaseURL, clientUUID)
	resp, err := c.client.Post(url, "application/json", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		if retry {
			if err := c.Login(); err != nil {
				return fmt.Errorf("re-login failed: %w", err)
			}
			return c.updateClientWithRetry(inboundID, clientUUID, clientSettings, false)
		}
		if resp.StatusCode == 404 {
			return fmt.Errorf("api endpoint not found (404), check url or auth")
		}
		return fmt.Errorf("unauthorized")
	}

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("api error: %s", result.Msg)
	}

	return nil
}

func (c *Client) EnableClient(inboundID int, clientUUID string, currentSettings map[string]interface{}) error {
	currentSettings["enable"] = true
	return c.UpdateClient(inboundID, clientUUID, currentSettings)
}

func (c *Client) DisableClient(inboundID int, clientUUID string, currentSettings map[string]interface{}) error {
	currentSettings["enable"] = false
	return c.UpdateClient(inboundID, clientUUID, currentSettings)
}
