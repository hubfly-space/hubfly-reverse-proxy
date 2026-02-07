package models

import (
	"time"
)

// Site represents a virtual host configuration.
type Site struct {
	ID              string            `json:"id"`
	Domain          string            `json:"domain"`
	Upstreams       []string          `json:"upstreams"`
	ForceSSL        bool              `json:"force_ssl"` // Redirect HTTP to HTTPS
	SSL             bool              `json:"ssl"`       // Enable SSL (requires cert)
	Templates       []string          `json:"templates"`
	ExtraConfig     string            `json:"extra_config,omitempty"`
	ProxySetHeaders map[string]string `json:"proxy_set_header,omitempty"`

	// Firewall Configuration
	Firewall *FirewallConfig `json:"firewall,omitempty"`

	// Status fields
	Status          string    `json:"status"` // "active", "provisioning", "error"
	ErrorMessage    string    `json:"error_message,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CertIssueStatus string    `json:"cert_issue_status,omitempty"` // "pending", "retrying", "valid", "failed"
	CertRetryCount  int       `json:"cert_retry_count,omitempty"`
	NextCertRetryAt *time.Time `json:"next_cert_retry_at,omitempty"`
	LastCertError   string    `json:"last_cert_error,omitempty"`
}

// APIResponse Standard API response wrapper (optional, but good for consistency)
type APIResponse struct {
	Error string      `json:"error,omitempty"`
	Code  int         `json:"code,omitempty"`
	Data  interface{} `json:"data,omitempty"`
}

// FirewallConfig holds all firewall related settings
type FirewallConfig struct {
	IPRules    []IPRule         `json:"ip_rules,omitempty"`
	BlockRules *BlockRules      `json:"block_rules,omitempty"`
	RateLimit  *RateLimitConfig `json:"rate_limit,omitempty"`
}

// IPRule defines an allow/deny rule for an IP or CIDR
type IPRule struct {
	Value  string `json:"value"`  // IP address or CIDR range
	Action string `json:"action"` // "allow" or "deny"
}

// BlockRules defines patterns to block requests
type BlockRules struct {
	UserAgents  []string            `json:"user_agents,omitempty"` // Regex patterns for User-Agent
	Methods     []string            `json:"methods,omitempty"`     // HTTP Methods to block (e.g., POST, PUT)
	Paths       []string            `json:"paths,omitempty"`       // Regex patterns for URL paths
	PathMethods map[string][]string `json:"path_methods,omitempty"` // Map of Path -> []Methods to block
}

// RateLimitConfig defines rate limiting parameters
type RateLimitConfig struct {
	Enabled        bool   `json:"enabled"`
	Rate           int    `json:"rate"`            // Requests per unit
	Unit           string `json:"unit"`            // "r/s" or "r/m"
	Burst          int    `json:"burst"`           // Max burst size
	ZoneName       string `json:"zone_name"`       // Internal use: Nginx zone name
}
