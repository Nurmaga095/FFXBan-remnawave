package config

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultRabbitMQURL      = "amqp://guest:guest@localhost/"
	defaultNFTBinary        = "nft"
	defaultNFTFamily        = "inet"
	defaultNFTTable         = "firewall"
	defaultNFTSet           = "user_blacklist"
	defaultStatusExchange   = "blocking_status_exchange"
	defaultHeartbeatSeconds = 30
	reconnectDelay          = 5 * time.Second
)

// Config хранит конфигурацию приложения.
type Config struct {
	RabbitMQURL    string
	ReconnectDelay time.Duration
	NFTBinary      string
	NFTFamily      string
	NFTTable       string
	NFTSet         string
	NodeName       string
	StatusExchange string
	HeartbeatEvery time.Duration
}

// New создает новый экземпляр Config из переменных окружения.
func New() *Config {
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = defaultRabbitMQURL
	}

	nftBinary := os.Getenv("NFT_BINARY")
	if nftBinary == "" {
		nftBinary = defaultNFTBinary
	}

	nftFamily := os.Getenv("NFT_FAMILY")
	if nftFamily == "" {
		nftFamily = defaultNFTFamily
	}

	nftTable := os.Getenv("NFT_TABLE")
	if nftTable == "" {
		nftTable = defaultNFTTable
	}

	nftSet := os.Getenv("NFT_SET")
	if nftSet == "" {
		nftSet = defaultNFTSet
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		if host, err := os.Hostname(); err == nil && host != "" {
			nodeName = host
		} else {
			nodeName = "unknown"
		}
	}

	statusExchange := os.Getenv("BLOCKING_STATUS_EXCHANGE_NAME")
	if statusExchange == "" {
		statusExchange = defaultStatusExchange
	}

	heartbeatSeconds := defaultHeartbeatSeconds
	if raw := os.Getenv("HEARTBEAT_INTERVAL_SECONDS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			heartbeatSeconds = parsed
		}
	}

	return &Config{
		RabbitMQURL:    rabbitmqURL,
		ReconnectDelay: reconnectDelay,
		NFTBinary:      nftBinary,
		NFTFamily:      nftFamily,
		NFTTable:       nftTable,
		NFTSet:         nftSet,
		NodeName:       nodeName,
		StatusExchange: statusExchange,
		HeartbeatEvery: time.Duration(heartbeatSeconds) * time.Second,
	}
}
