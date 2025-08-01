package turn

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{
			name:    "valid https URL",
			baseURL: "https://api.example.com",
			wantErr: false,
		},
		{
			name:    "valid http URL",
			baseURL: "http://localhost:8080",
			wantErr: false,
		},
		{
			name:    "URL with trailing slash",
			baseURL: "https://api.example.com/",
			wantErr: false,
		},
		{
			name:    "invalid URL",
			baseURL: "not a url",
			wantErr: true,
		},
		{
			name:    "invalid scheme",
			baseURL: "ftp://example.com",
			wantErr: true,
		},
		{
			name:    "empty URL",
			baseURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.baseURL)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && client == nil {
				t.Error("NewClient() returned nil client without error")
			}
			if !tt.wantErr && client != nil {
				// Verify trailing slash is removed
				if strings.HasSuffix(client.baseURL, "/") {
					t.Errorf("baseURL has trailing slash: %s", client.baseURL)
				}
			}
		})
	}
}

func TestClient_Check(t *testing.T) {
	tests := []struct {
		name         string
		prURL        string
		username     string
		response     CheckResponse
		statusCode   int
		wantErr      bool
		errorMessage string
	}{
		{
			name:     "successful check - not blocked",
			prURL:    "https://github.com/owner/repo/pull/123",
			username: "testuser",
			response: CheckResponse{
				PRState: PRState{
					UnblockAction:      map[string]Action{},
					Checks:             Checks{},
					UnresolvedComments: 0,
					ReadyToMerge:       true,
				},
			},
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name:     "successful check - blocked",
			prURL:    "https://github.com/owner/repo/pull/456",
			username: "testuser",
			response: CheckResponse{
				PRState: PRState{
					UnblockAction: map[string]Action{
						"testuser": {Kind: "REVIEW", Critical: true, Reason: "needs to review"},
					},
					Checks:             Checks{},
					UnresolvedComments: 0,
					ReadyToMerge:       false,
				},
			},
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name:         "empty PR URL",
			prURL:        "",
			username:     "testuser",
			wantErr:      true,
			errorMessage: "PR URL cannot be empty",
		},
		{
			name:         "server error",
			prURL:        "https://github.com/owner/repo/pull/789",
			username:     "testuser",
			statusCode:   http.StatusInternalServerError,
			wantErr:      true,
			errorMessage: "send request:", // Changed due to retry wrapper
		},
		{
			name:         "not found",
			prURL:        "https://github.com/owner/repo/pull/999",
			username:     "testuser",
			statusCode:   http.StatusNotFound,
			wantErr:      true,
			errorMessage: "api request failed with status 404",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request
				if r.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", r.Method)
				}
				if r.URL.Path != "/v1/validate" {
					t.Errorf("expected /v1/validate, got %s", r.URL.Path)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("expected Content-Type: application/json, got %s", r.Header.Get("Content-Type"))
				}
				if r.Header.Get("User-Agent") != "turnclient/1.0" {
					t.Errorf("expected User-Agent: turnclient/1.0, got %s", r.Header.Get("User-Agent"))
				}

				// Decode request body
				var req CheckRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("failed to decode request: %v", err)
				}

				// Send response
				if tt.statusCode != 0 && tt.statusCode != http.StatusOK {
					w.WriteHeader(tt.statusCode)
					w.Write([]byte("error message"))
					return
				}

				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(tt.response); err != nil {
					t.Errorf("failed to encode response: %v", err)
				}
			}))
			defer server.Close()

			// Create client
			client, err := NewClient(server.URL)
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.SetLogger(log.New(io.Discard, "", 0)) // Silence logs during tests

			// Perform check
			ctx := context.Background()
			result, err := client.Check(ctx, tt.prURL, tt.username, time.Now())

			// Verify results
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil && tt.errorMessage != "" {
				if !strings.Contains(err.Error(), tt.errorMessage) {
					t.Errorf("Check() error = %v, want error containing %q", err, tt.errorMessage)
				}
			}

			if !tt.wantErr && result != nil {
				if len(result.PRState.UnblockAction) != len(tt.response.PRState.UnblockAction) {
					t.Errorf("UnblockAction length = %d, want %d", len(result.PRState.UnblockAction), len(tt.response.PRState.UnblockAction))
				}
				if result.PRState.ReadyToMerge != tt.response.PRState.ReadyToMerge {
					t.Errorf("ReadyToMerge = %v, want %v", result.PRState.ReadyToMerge, tt.response.PRState.ReadyToMerge)
				}
			}
		})
	}
}

func TestClient_CheckWithAuth(t *testing.T) {
	token := "test-token-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		authHeader := r.Header.Get("Authorization")
		expectedAuth := "Bearer " + token
		if authHeader != expectedAuth {
			t.Errorf("Authorization header = %s, want %s", authHeader, expectedAuth)
		}

		// Send success response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CheckResponse{
			PRState: PRState{
				UnblockAction:      map[string]Action{},
				Checks:             Checks{},
				UnresolvedComments: 0,
				ReadyToMerge:       true,
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetLogger(log.New(io.Discard, "", 0))
	client.SetAuthToken(token)

	ctx := context.Background()
	_, err = client.Check(ctx, "https://github.com/owner/repo/pull/123", "testuser", time.Now())
	if err != nil {
		t.Errorf("Check() with auth failed: %v", err)
	}
}

func TestClient_CheckTimeout(t *testing.T) {
	// Create a server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetLogger(log.New(io.Discard, "", 0))

	// Use a context with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err = client.Check(ctx, "https://github.com/owner/repo/pull/123", "testuser", time.Now())
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestCurrentUser(t *testing.T) {
	tests := []struct {
		name         string
		hasToken     bool
		wantErr      bool
		errorMessage string
	}{
		{
			name:         "no auth token",
			hasToken:     false,
			wantErr:      true,
			errorMessage: "no auth token set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient("https://api.example.com")
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			if tt.hasToken {
				client.SetAuthToken("test-token")
			}

			ctx := context.Background()
			_, err = client.CurrentUser(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CurrentUser() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil && tt.errorMessage != "" {
				if !strings.Contains(err.Error(), tt.errorMessage) {
					t.Errorf("CurrentUser() error = %v, want error containing %q", err, tt.errorMessage)
				}
			}
		})
	}
}
