package api

import (
	"ffxban/internal/services/threexui"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleThreexuiUsers(c *gin.Context) {
	if s.threexui == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "3x-ui integration is not enabled"})
		return
	}

	users, err := s.threexui.GetAllUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Fetch active sessions from Redis to determine real online status.
	// "Online" = IP was seen within last 5 minutes (TTL still close to max).
	activeEmails, err := s.storage.GetAllUserEmails(c.Request.Context())
	activeStats := make(map[string]int)
	onlineRecent := make(map[string]bool)
	userTTLSec := int(s.cfg.UserIPTTL.Seconds())
	if userTTLSec <= 0 {
		userTTLSec = 1200
	}
	const recentWindowSec = 300 // 5 minutes
	if err == nil {
		for _, email := range activeEmails {
			if ips, err := s.storage.GetUserActiveIPs(c.Request.Context(), email); err == nil {
				key := strings.ToLower(email)
				activeStats[key] = len(ips)
				for _, remainingTTL := range ips {
					if userTTLSec-remainingTTL <= recentWindowSec {
						onlineRecent[key] = true
						break
					}
				}
			}
		}
	}

	// Filter/Search?
	query := strings.ToLower(strings.TrimSpace(c.Query("q")))
	onlineOnly := c.Query("online_only") == "true"

	// Deduplicate by email: merge data from multiple servers.
	type merged struct {
		email      string
		uuid       string
		inboundID  int
		servers    []string
		enable     bool
		limitIp    int
		up         int64
		down       int64
		expiryTime int64
		remark     string
		ipCount    int
		isOnline   bool
	}
	mergedMap := make(map[string]*merged)
	order := make([]string, 0)

	for _, u := range users {
		key := strings.ToLower(u.Email)
		if query != "" && !strings.Contains(key, strings.ToLower(query)) && !strings.Contains(strings.ToLower(u.ID), strings.ToLower(query)) {
			continue
		}
		ipCount := activeStats[key]
		isOnline := onlineRecent[key]
		if onlineOnly && !isOnline {
			continue
		}
		if m, exists := mergedMap[key]; exists {
			// Merge: sum traffic, collect servers, OR enable/online
			m.up += u.Up
			m.down += u.Down
			m.enable = m.enable || u.Enable
			m.servers = append(m.servers, u.ServerName)
			if u.LimitIp > 0 && (m.limitIp == 0 || u.LimitIp < m.limitIp) {
				m.limitIp = u.LimitIp
			}
		} else {
			mergedMap[key] = &merged{
				email:      u.Email,
				uuid:       u.ID,
				inboundID:  u.InboundID,
				servers:    []string{u.ServerName},
				enable:     u.Enable,
				limitIp:    u.LimitIp,
				up:         u.Up,
				down:       u.Down,
				expiryTime: u.ExpiryTime,
				remark:     u.Remark,
				ipCount:    ipCount,
				isOnline:   isOnline,
			}
			order = append(order, key)
		}
	}

	result := make([]gin.H, 0)
	for _, key := range order {
		m := mergedMap[key]
		result = append(result, gin.H{
			"email":       m.email,
			"uuid":        m.uuid,
			"inbound_id":  m.inboundID,
			"server_name": strings.Join(m.servers, ", "),
			"enable":      m.enable,
			"limit_ip":    m.limitIp,
			"up":          m.up,
			"down":        m.down,
			"total":       m.up + m.down,
			"expiry_time": m.expiryTime,
			"remark":      m.remark,
			"online":      m.isOnline,
			"ip_count":    m.ipCount,
		})
	}

	// Sort by total traffic desc
	sort.Slice(result, func(i, j int) bool {
		return result[i]["total"].(int64) > result[j]["total"].(int64)
	})

	c.JSON(http.StatusOK, result)
}

func (s *Server) handleThreexuiServers(c *gin.Context) {
	if s.threexui == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "3x-ui integration is not enabled"})
		return
	}

	clients := s.threexui.GetClients()

	type serverResult struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Status string `json:"status"`
	}

	results := make([]serverResult, len(clients))
	var wg sync.WaitGroup

	for i, cli := range clients {
		wg.Add(1)
		go func(i int, cli *threexui.Client) {
			defer wg.Done()
			
			// Simple check: try to login (it handles cookie reuse internally)
			err := cli.Login()
			status := "online"
			if err != nil {
				status = "offline"
			}
			
			results[i] = serverResult{
				Name:   cli.Name,
				URL:    cli.BaseURL,
				Status: status,
			}
		}(i, cli)
	}
	wg.Wait()

	// Sort by name
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	c.JSON(http.StatusOK, results)
}
