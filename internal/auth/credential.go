// Package auth stores credentials and implements OpenAI subscription authentication.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const CredentialVersion = 1

type CredentialType string

const (
	CredentialAPIKey CredentialType = "api_key"
	CredentialOAuth  CredentialType = "oauth"
)

// Secret is deliberately redacted by all string formatting. Value should only
// be used at the point where a credential is persisted or sent to its issuer.
type Secret string

func (Secret) String() string  { return "[REDACTED]" }
func (s Secret) Value() string { return string(s) }

type APIKeyCredential struct {
	Key Secret `json:"key"`
}

func (APIKeyCredential) String() string { return "api key [REDACTED]" }

type OAuthCredential struct {
	AccessToken  Secret    `json:"access_token"`
	RefreshToken Secret    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id,omitempty"`
}

func (o OAuthCredential) String() string {
	return fmt.Sprintf("oauth credential (expires %s, tokens [REDACTED])", o.ExpiresAt.UTC().Format(time.RFC3339))
}

// Credential is a versioned tagged union. Exactly one variant must be present.
type Credential struct {
	Version int               `json:"version"`
	Type    CredentialType    `json:"type"`
	APIKey  *APIKeyCredential `json:"api_key,omitempty"`
	OAuth   *OAuthCredential  `json:"oauth,omitempty"`
}

func NewAPIKeyCredential(key string) Credential {
	return Credential{Version: CredentialVersion, Type: CredentialAPIKey, APIKey: &APIKeyCredential{Key: Secret(key)}}
}

func NewOAuthCredential(value OAuthCredential) Credential {
	return Credential{Version: CredentialVersion, Type: CredentialOAuth, OAuth: &value}
}

func (c Credential) String() string {
	switch c.Type {
	case CredentialAPIKey:
		return "credential v1 (api_key, [REDACTED])"
	case CredentialOAuth:
		return "credential v1 (oauth, tokens [REDACTED])"
	default:
		return "credential (invalid, [REDACTED])"
	}
}

func (c Credential) validate() error {
	if c.Version != CredentialVersion {
		return fmt.Errorf("auth: unsupported credential version %d", c.Version)
	}
	switch c.Type {
	case CredentialAPIKey:
		if c.APIKey == nil || c.OAuth != nil || c.APIKey.Key == "" {
			return errors.New("auth: invalid api key credential")
		}
	case CredentialOAuth:
		if c.OAuth == nil || c.APIKey != nil || c.OAuth.AccessToken == "" || c.OAuth.RefreshToken == "" || c.OAuth.ExpiresAt.IsZero() {
			return errors.New("auth: invalid oauth credential")
		}
	default:
		return errors.New("auth: unknown credential type")
	}
	return nil
}

func decodeCredential(data []byte) (Credential, error) {
	var value Credential
	if err := strictJSON(data, &value); err != nil {
		return Credential{}, err
	}
	if err := value.validate(); err != nil {
		return Credential{}, err
	}
	return value, nil
}

func cloneCredential(value Credential) Credential {
	data, _ := json.Marshal(value)
	cloned, _ := decodeCredential(data)
	return cloned
}
