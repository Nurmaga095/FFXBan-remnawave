package blocker

import (
	"ffxban/internal/services/publisher"
	"log"
)

// Blocker defines the interface for blocking actions.
type Blocker interface {
	// BlockUser blocks a user by identifier (e.g. email).
	// In the current RabbitMQ mode this action is a no-op.
	BlockUser(user string, duration string) error

	// UnblockUser unblocks a user by identifier.
	// In the current RabbitMQ mode this action is a no-op.
	UnblockUser(user string) error

	// BlockIPs blocks a set of IPs for a duration.
	BlockIPs(ips []string, duration string) error

	// UnblockIPs unblocks a set of IPs immediately.
	UnblockIPs(ips []string) error

	// Ping checks the health of the blocker service.
	Ping() error
}

// RabbitMQBlocker implements Blocker using RabbitMQ publisher (IP-based).
type RabbitMQBlocker struct {
	pub publisher.EventPublisher
}

func NewRabbitMQBlocker(pub publisher.EventPublisher) *RabbitMQBlocker {
	return &RabbitMQBlocker{pub: pub}
}

func (b *RabbitMQBlocker) BlockUser(user string, duration string) error {
	// RabbitMQ blocker does not support user-level actions.
	return nil
}

func (b *RabbitMQBlocker) UnblockUser(user string) error {
	log.Printf("Warning: UnblockUser called for %s (not supported in RabbitMQ mode)", user)
	return nil
}

func (b *RabbitMQBlocker) BlockIPs(ips []string, duration string) error {
	return b.pub.PublishBlockMessage(ips, duration)
}

func (b *RabbitMQBlocker) UnblockIPs(ips []string) error {
	return b.pub.PublishUnblockMessage(ips)
}

func (b *RabbitMQBlocker) Ping() error {
	return b.pub.Ping()
}
