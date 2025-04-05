package google_api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RefreshTokenRequest represents the input parameters
type RefreshTokenRequest struct {
	Key          string
	RefreshToken string
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

// GetFirebaseToken refreshes a Firebase token using the refresh token
func GetFirebaseToken(req RefreshTokenRequest) (*TokenResponse, error) {
	// Prepare request
	apiURL := "https://securetoken.googleapis.com/v1/token"
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", req.RefreshToken)

	request, err := http.NewRequest("POST", apiURL+"?key="+req.Key, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v err: %v", request, err)
	}

	// Set headers exactly as in the curl command
	request.Header.Set("User-Agent", "node-fetch/1.0 (+https://github.com/bitinn/node-fetch)")
	request.Header.Set("Connection", "close")
	request.Header.Set("Accept-Encoding", "deflate")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Client-Version", "Node/JsCore/10.5.2/FirebaseCore-web")
	request.Header.Set("X-Firebase-gmpid", "1:252179682924:web:9c80c6a32cb4682cbfaa49")

	// Send request with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	// Parse JSON response
	var tokenResponse TokenResponse
	err = json.Unmarshal(body, &tokenResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %v", err)
	}

	return &tokenResponse, nil
}
