package models

import "time"

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleAnalyst Role = "analyst"
)

// User represents an authenticated GitHub user stored in the DB.
type User struct {
	ID          string     `json:"id"`
	GithubID    string     `json:"github_id"`
	Username    string     `json:"username"`
	Email       string     `json:"email"`
	AvatarURL   string     `json:"avatar_url"`
	Role        Role       `json:"role"`
	IsActive    bool       `json:"is_active"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Profile represents a name intelligence record enriched from external APIs.
type Profile struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Gender             string  `json:"gender"`
	GenderProbability  float64 `json:"gender_probability"`
	SampleSize         int     `json:"sample_size"`
	Age                int     `json:"age"`
	AgeGroup           string  `json:"age_group"`
	CountryID          string  `json:"country_id"`
	CountryName        string  `json:"country_name"`
	CountryProbability float64 `json:"country_probability"`
	CreatedAt          string  `json:"created_at"`
}

// PaginatedResponse is the standard list response shape for all profile endpoints.
type PaginatedResponse struct {
	Status     string      `json:"status"`
	Page       int         `json:"page"`
	Limit      int         `json:"limit"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
	Links      PagingLinks `json:"links"`
	Data       []Profile   `json:"data"`
}

type PagingLinks struct {
	Self string  `json:"self"`
	Next *string `json:"next"`
	Prev *string `json:"prev"`
}

// ctxKey is an unexported type for context keys to avoid collisions.
type ctxKey string

const UserCtxKey ctxKey = "authenticated_user"
