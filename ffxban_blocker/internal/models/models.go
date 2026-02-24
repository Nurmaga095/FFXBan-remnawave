package models

// BlockingPayload представляет структуру входящих сообщений из RabbitMQ.
type BlockingPayload struct {
	Action   string   `json:"action,omitempty"`
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// BlockerReport представляет отчет blocker о выполненном действии или heartbeat.
type BlockerReport struct {
	Type      string `json:"type"` // action | heartbeat
	Action    string `json:"action,omitempty"`
	IP        string `json:"ip,omitempty"`
	Duration  string `json:"duration,omitempty"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
	Timestamp string `json:"timestamp"`
}
