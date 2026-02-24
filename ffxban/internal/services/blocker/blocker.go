package blocker

import (
	"ffxban/internal/services/publisher"
	"ffxban/internal/services/threexui"
	"log"
	"time"
)

// Blocker defines the interface for blocking actions.
type Blocker interface {
	// BlockUser blocks a user by identifier (e.g. email).
	// Used primarily in 3x-ui mode.
	BlockUser(user string, duration string) error

	// UnblockUser unblocks a user by identifier.
	// Used primarily in 3x-ui mode.
	UnblockUser(user string) error

	// BlockIPs blocks a set of IPs for a duration.
	// Used primarily in Remnawave/RabbitMQ mode.
	BlockIPs(ips []string, duration string) error

	// UnblockIPs unblocks a set of IPs immediately.
	// Used primarily in Remnawave/RabbitMQ mode.
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
	// RabbitMQ mode doesn't support blocking users directly, only IPs.
	// Log warning or ignore.
	// log.Printf("Warning: BlockUser called in RabbitMQ mode for %s (not supported, only IP blocking available)", user)
	return nil
}

func (b *RabbitMQBlocker) UnblockUser(user string) error {
	log.Printf("Warning: UnblockUser called in RabbitMQ mode for %s (not supported)", user)
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

// ThreexuiBlocker implements Blocker using 3x-ui API (User-based).
type ThreexuiBlocker struct {
	mgr *threexui.Manager
}

func NewThreexuiBlocker(mgr *threexui.Manager) *ThreexuiBlocker {
	return &ThreexuiBlocker{mgr: mgr}
}

func (b *ThreexuiBlocker) BlockUser(user string, duration string) error {
	err := b.mgr.BlockUser(user)
	if err != nil {
		return err
	}

	// Handle duration if not permanent
	// TODO: Implement persistent scheduling for unblocking.
	// For now using time.AfterFunc which is lost on restart.
	if duration != "permanent" && duration != "" {
		d, err := time.ParseDuration(duration)
		if err == nil {
			time.AfterFunc(d, func() {
				b.UnblockUser(user)
			})
		}
	}
	return nil
}

func (b *ThreexuiBlocker) UnblockUser(user string) error {
	return b.mgr.UnblockUser(user)
}

func (b *ThreexuiBlocker) BlockIPs(ips []string, duration string) error {
	// 3x-ui mode typically doesn't block IPs on firewall level (unless hybrid).
	// Log info.
	// log.Printf("Info: BlockIPs called in 3x-ui mode for %d IPs (ignored)", len(ips))
	return nil
}

func (b *ThreexuiBlocker) UnblockIPs(ips []string) error {
	return nil
}

func (b *ThreexuiBlocker) Ping() error {
	// Simple health check: try to list clients from one server?
	// Or just return nil since HTTP client is stateless.
	return nil
}

// ComboBlocker implements Blocker using both.
type ComboBlocker struct {
	rabbit  *RabbitMQBlocker
	threexui *ThreexuiBlocker
}

func NewComboBlocker(rabbit *RabbitMQBlocker, threexui *ThreexuiBlocker) *ComboBlocker {
	return &ComboBlocker{rabbit: rabbit, threexui: threexui}
}

func (b *ComboBlocker) UnblockUser(user string) error {
	return b.threexui.UnblockUser(user)
}

func (b *ComboBlocker) BlockUser(user string, duration string) error {
	return b.threexui.BlockUser(user, duration)
}

func (b *ComboBlocker) BlockIPs(ips []string, duration string) error {
	return b.rabbit.BlockIPs(ips, duration)
}

func (b *ComboBlocker) UnblockIPs(ips []string) error {
	return b.rabbit.UnblockIPs(ips)
}

func (b *ComboBlocker) Ping() error {
	// Check both?
	if err := b.rabbit.Ping(); err != nil {
		return err
	}
	return b.threexui.Ping()
}
