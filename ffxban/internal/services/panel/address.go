package panel

import "strings"

func extractNodeAddress(node map[string]any) string {
	for _, key := range []string{"address", "host", "domain", "ip", "nodeAddress"} {
		raw, ok := node[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}
