package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleNodeProvision(c *gin.Context) {
	clientIP := c.ClientIP()

	if s.limitProvider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "panel integration disabled"})
		return
	}

	nodes := s.limitProvider.ListNodes()
	for _, node := range nodes {
		nodeAddr := strings.TrimSpace(node.Address)
		if nodeAddr == "" {
			continue
		}

		// Exact match (IP or Domain string)
		if nodeAddr == clientIP {
			c.JSON(http.StatusOK, gin.H{
				"node_name": node.Name,
				"node_id":   node.ID,
			})
			return
		}

		// If address is a domain, resolve it to check against clientIP
		if net.ParseIP(nodeAddr) == nil {
			ips, err := net.LookupIP(nodeAddr)
			if err == nil {
				for _, ip := range ips {
					if ip.String() == clientIP {
						c.JSON(http.StatusOK, gin.H{
							"node_name": node.Name,
							"node_id":   node.ID,
						})
						return
					}
				}
			}
		}
	}

	c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("node not found for IP %s", clientIP)})
}
