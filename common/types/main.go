package types

import "time"

// ASTokenInfo holds token information
type ASTokenInfo struct {
	ApiKey       string
	RefreshToken string
	AccessToken  string
}

// RateLimitCookie holds rate limiting information
type RateLimitCookie struct {
	ExpirationTime time.Time
}

// TokenResponse represents the output from the token endpoint
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    string `json:"expires_in"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	UserID       string `json:"user_id"`
	ProjectID    string `json:"project_id"`
}

// RefreshTokenRequest represents the input parameters
type RefreshTokenRequest struct {
	Key          string
	RefreshToken string
}
