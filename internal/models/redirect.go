package models

import "time"

// Redirect represents a managed hostname that only redirects traffic.
type Redirect struct {
	ID           string    `json:"id"`
	SourceDomain string    `json:"source_domain"`
	TargetDomain string    `json:"target_domain"`
	SSL          bool      `json:"ssl"`
	Status       string    `json:"status"`
	ErrorMessage string    `json:"error_message,omitempty"`
	DeployStatus string    `json:"deploy_status,omitempty"`
	DeployError  string    `json:"deploy_error,omitempty"`
	ActiveConfig string    `json:"active_config,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
