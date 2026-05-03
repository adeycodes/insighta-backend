package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// GitHubUser holds the fields we need from the GitHub /user endpoint.
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// AuthorizeURL builds the GitHub OAuth redirect URL for a given state value.
func AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", os.Getenv("GITHUB_CLIENT_ID"))
	q.Set("redirect_uri", os.Getenv("BACKEND_URL")+"/auth/github/callback")
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

// ExchangeCode swaps a GitHub authorization code for a GitHub access token.
func ExchangeCode(code string) (string, error) {
	body := url.Values{}
	body.Set("client_id", os.Getenv("GITHUB_CLIENT_ID"))
	body.Set("client_secret", os.Getenv("GITHUB_CLIENT_SECRET"))
	body.Set("code", code)
	body.Set("redirect_uri", os.Getenv("BACKEND_URL")+"/auth/github/callback")

	req, err := http.NewRequest(http.MethodPost, "https://github.com/login/oauth/access_token",
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding github response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("github oauth: %s — %s", result.Error, result.ErrorDesc)
	}
	return result.AccessToken, nil
}

// GetUser fetches the authenticated GitHub user's profile.
func GetUser(githubToken string) (*GitHubUser, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching github user: %w", err)
	}
	defer resp.Body.Close()

	var u GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("decoding user: %w", err)
	}

	if u.Email == "" {
		u.Email, _ = fetchPrimaryEmail(githubToken)
	}
	return &u, nil
}

// fetchPrimaryEmail calls the /user/emails endpoint to get the verified primary email.
func fetchPrimaryEmail(githubToken string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user/emails", nil)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var emails []struct {
		Email   string `json:"email"`
		Primary bool   `json:"primary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", err
	}
	for _, e := range emails {
		if e.Primary {
			return e.Email, nil
		}
	}
	if len(emails) > 0 {
		return emails[0].Email, nil
	}
	return "", nil
}
