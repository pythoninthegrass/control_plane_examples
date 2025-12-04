package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// --- Client ID Validation Tests ---

func TestIsValidClientID(t *testing.T) {
	tests := []struct {
		name     string
		clientID string
		want     bool
	}{
		{"valid simple", "my-client", true},
		{"valid with underscore", "my_client_123", true},
		{"valid with numbers", "client123", true},
		{"valid minimum length", "abc", true},
		{"valid 64 chars", strings.Repeat("a", 64), true},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", 65), false},
		{"empty string", "", false},
		{"contains space", "my client", false},
		{"contains special char", "my@client", false},
		{"contains dot", "my.client", false},
		{"contains colon", "my:client", false},
		{"injection attempt", "client:*", false},
		{"path traversal", "../etc/passwd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidClientID(tt.clientID)
			if got != tt.want {
				t.Errorf("isValidClientID(%q) = %v, want %v", tt.clientID, got, tt.want)
			}
		})
	}
}

// --- Admin Authentication Tests ---

func TestAdminAuth(t *testing.T) {
	// Save original value and restore after test
	originalKey := AdminAPIKey
	defer func() { AdminAPIKey = originalKey }()

	handler := adminAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	tests := []struct {
		name           string
		adminKey       string
		headerKey      string
		queryKey       string
		expectedStatus int
	}{
		{
			name:           "no admin key configured - allows access",
			adminKey:       "",
			headerKey:      "",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid header key",
			adminKey:       "secret123",
			headerKey:      "secret123",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid query key",
			adminKey:       "secret123",
			queryKey:       "secret123",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid header key",
			adminKey:       "secret123",
			headerKey:      "wrongkey",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "missing key when required",
			adminKey:       "secret123",
			headerKey:      "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "header takes precedence over query",
			adminKey:       "secret123",
			headerKey:      "secret123",
			queryKey:       "wrongkey",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			AdminAPIKey = tt.adminKey

			url := "/admin/test"
			if tt.queryKey != "" {
				url += "?admin_key=" + tt.queryKey
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tt.headerKey != "" {
				req.Header.Set("X-Admin-Key", tt.headerKey)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("got status %d, want %d", rr.Code, tt.expectedStatus)
			}
		})
	}
}

// --- Rate Limit Override Validation Tests ---

func TestGetRateLimitForClient_OverrideValidation(t *testing.T) {
	// Skip if Redis is not available
	if redisClient == nil {
		setupTestRedis(t)
	}

	ctx := context.Background()

	// Create a test client with "basic" tier
	testClientID := "test-rate-limit-client"
	testConfig := ClientConfig{
		ClientID: testClientID,
		Tier:     "basic", // max_concurrent: 5, requests_per_min: 100
		Enabled:  true,
	}

	if err := setClientConfig(testConfig); err != nil {
		t.Fatalf("Failed to set client config: %v", err)
	}
	defer redisClient.Del(ctx, "client_config:"+testClientID)

	tests := []struct {
		name           string
		override       *RateConfig
		expectedMax    int
		expectedPerMin int
	}{
		{
			name:           "no override uses tier defaults",
			override:       nil,
			expectedMax:    5,
			expectedPerMin: 100,
		},
		{
			name:           "override within limits",
			override:       &RateConfig{MaxConcurrent: 3, RequestsPerMin: 50},
			expectedMax:    3,
			expectedPerMin: 50,
		},
		{
			name:           "override exceeds tier - should be clamped",
			override:       &RateConfig{MaxConcurrent: 100, RequestsPerMin: 10000},
			expectedMax:    5,   // clamped to tier max
			expectedPerMin: 100, // clamped to tier max
		},
		{
			name:           "partial override exceeds - should be clamped",
			override:       &RateConfig{MaxConcurrent: 3, RequestsPerMin: 5000},
			expectedMax:    3,   // within limit
			expectedPerMin: 100, // clamped
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := getRateLimitForClient(testClientID, tt.override)
			if err != nil {
				t.Fatalf("getRateLimitForClient() error = %v", err)
			}

			if result.MaxConcurrent != tt.expectedMax {
				t.Errorf("MaxConcurrent = %d, want %d", result.MaxConcurrent, tt.expectedMax)
			}
			if result.RequestsPerMin != tt.expectedPerMin {
				t.Errorf("RequestsPerMin = %d, want %d", result.RequestsPerMin, tt.expectedPerMin)
			}
		})
	}
}

// --- Request Body Size Limit Tests ---

func TestRequestBodySizeLimit(t *testing.T) {
	// Create a handler that wraps with MaxBytesReader (simulating handleEnqueue)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit

		var req EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if err.Error() == "http: request body too large" {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name           string
		bodySize       int
		expectedStatus int
	}{
		{
			name:           "small body - OK",
			bodySize:       1024, // 1KB
			expectedStatus: http.StatusOK,
		},
		{
			name:           "under 1MB - OK",
			bodySize:       (1 << 20) - 1024, // Just under 1MB
			expectedStatus: http.StatusOK,
		},
		{
			name:           "over 1MB - rejected",
			bodySize:       (1 << 20) + 1024,
			expectedStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:           "2MB - rejected",
			bodySize:       2 << 20,
			expectedStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a valid JSON body of the specified size
			payload := EnqueueRequest{
				Queue:    "default",
				ClientID: "test-client",
				Task: TaskPayload{
					URL:    "http://example.com",
					Method: "GET",
					Body:   strings.Repeat("x", tt.bodySize-100), // Subtract for JSON overhead
				},
			}

			body, _ := json.Marshal(payload)
			// Pad to exact size if needed
			if len(body) < tt.bodySize {
				padding := make([]byte, tt.bodySize-len(body))
				body = append(body[:len(body)-1], padding...)
				body = append(body, '}')
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/enqueue", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("got status %d, want %d", rr.Code, tt.expectedStatus)
			}
		})
	}
}

// --- Config Helper Tests ---

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name     string
		envKey   string
		envValue string
		fallback int
		want     int
	}{
		{
			name:     "use fallback when not set",
			envKey:   "TEST_NOT_SET",
			envValue: "",
			fallback: 10,
			want:     10,
		},
		{
			name:     "use env value when set",
			envKey:   "TEST_SET",
			envValue: "25",
			fallback: 10,
			want:     25,
		},
		{
			name:     "use fallback for invalid int",
			envKey:   "TEST_INVALID",
			envValue: "not-a-number",
			fallback: 10,
			want:     10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				os.Setenv(tt.envKey, tt.envValue)
				defer os.Unsetenv(tt.envKey)
			}

			got := getEnvInt(tt.envKey, tt.fallback)
			if got != tt.want {
				t.Errorf("getEnvInt() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- Health Check Test ---

func TestHealthEndpoint(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("health check returned %d, want %d", rr.Code, http.StatusOK)
	}

	if rr.Body.String() != "OK" {
		t.Errorf("health check body = %q, want %q", rr.Body.String(), "OK")
	}
}

// --- Test Helpers ---

func setupTestRedis(t *testing.T) {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	// Always create a fresh client for testing
	testClient := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: os.Getenv("REDIS_PASSWORD"),
	})

	ctx := context.Background()
	if err := testClient.Ping(ctx).Err(); err != nil {
		testClient.Close()
		t.Skipf("Redis not available at %s: %v", addr, err)
	}

	// Set the global client only if connection succeeded
	if redisClient != nil {
		redisClient.Close()
	}
	redisClient = testClient
}

// --- Admin Endpoint Method Tests ---

func TestAdminEndpointMethods(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		handler        http.HandlerFunc
		expectedStatus int
	}{
		{
			name:   "list clients GET allowed",
			method: http.MethodGet,
			path:   "/admin/clients",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "list clients POST rejected",
			method: http.MethodPost,
			path:   "/admin/clients",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:   "set client POST allowed",
			method: http.MethodPost,
			path:   "/admin/clients/set",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost && r.Method != http.MethodPut {
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "set client PUT allowed",
			method: http.MethodPut,
			path:   "/admin/clients/set",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost && r.Method != http.MethodPut {
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "delete client DELETE allowed",
			method: http.MethodDelete,
			path:   "/admin/clients/delete",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			},
			expectedStatus: http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rr := httptest.NewRecorder()
			tt.handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("got status %d, want %d", rr.Code, tt.expectedStatus)
			}
		})
	}
}

// --- Validate Client Tests ---

// --- URL Validation Tests (SSRF Protection) ---

func TestIsValidTaskURL(t *testing.T) {
	// Save and restore original value
	originalAllowPrivate := AllowPrivateURLs
	defer func() { AllowPrivateURLs = originalAllowPrivate }()
	AllowPrivateURLs = false

	tests := []struct {
		name    string
		url     string
		wantErr bool
		errMsg  string
	}{
		{"valid https url", "https://example.com/webhook", false, ""},
		{"valid http url", "http://api.example.com/task", false, ""},
		{"valid url with port", "https://example.com:8080/path", false, ""},
		{"empty url", "", true, "URL is required"},
		{"invalid scheme ftp", "ftp://example.com", true, "invalid URL scheme"},
		{"invalid scheme file", "file:///etc/passwd", true, "invalid URL scheme"},
		{"no scheme", "example.com/path", true, "invalid URL scheme"},
		{"localhost blocked", "http://localhost:8080", true, "localhost URLs are not allowed"},
		{"127.0.0.1 blocked", "http://127.0.0.1/admin", true, "localhost URLs are not allowed"},
		{"metadata endpoint blocked", "http://169.254.169.254/latest/meta-data", true, "metadata service URLs are not allowed"},
		{"google metadata blocked", "http://metadata.google.internal/computeMetadata/v1/", true, "metadata service URLs are not allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := isValidTaskURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestIsValidTaskURL_AllowPrivate(t *testing.T) {
	originalAllowPrivate := AllowPrivateURLs
	defer func() { AllowPrivateURLs = originalAllowPrivate }()
	AllowPrivateURLs = true

	// With AllowPrivateURLs=true, localhost should be allowed
	err := isValidTaskURL("http://localhost:8080/test")
	if err != nil {
		t.Errorf("expected localhost to be allowed when AllowPrivateURLs=true, got: %v", err)
	}
}

// --- Queue Validation Tests ---

func TestIsValidQueue(t *testing.T) {
	tests := []struct {
		name  string
		queue string
		want  bool
	}{
		{"valid simple", "default", true},
		{"valid with underscore", "my_queue", true},
		{"valid with hyphen", "my-queue", true},
		{"valid with numbers", "queue123", true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", 65), false},
		{"contains space", "my queue", false},
		{"contains special char", "queue@name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidQueue(tt.queue)
			if got != tt.want {
				t.Errorf("isValidQueue(%q) = %v, want %v", tt.queue, got, tt.want)
			}
		})
	}
}

// --- Private IP Tests ---

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback ipv4", "127.0.0.1", true},
		{"loopback ipv6", "::1", true},
		{"private 10.x", "10.0.0.1", true},
		{"private 172.16.x", "172.16.0.1", true},
		{"private 192.168.x", "192.168.1.1", true},
		{"link-local", "169.254.1.1", true},
		{"public ip", "8.8.8.8", false},
		{"public ip 2", "1.1.1.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("invalid IP: %s", tt.ip)
			}
			got := isPrivateIP(ip)
			if got != tt.want {
				t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestValidateClient_InvalidFormat(t *testing.T) {
	tests := []struct {
		name         string
		clientID     string
		wantError    bool
		errorMsg     string
		requireRedis bool
	}{
		{
			name:         "empty client ID",
			clientID:     "",
			wantError:    true,
			errorMsg:     "client_id is required",
			requireRedis: false,
		},
		{
			name:         "invalid format - too short",
			clientID:     "ab",
			wantError:    true,
			errorMsg:     "invalid client_id format",
			requireRedis: false,
		},
		{
			name:         "invalid format - special chars",
			clientID:     "client@test",
			wantError:    true,
			errorMsg:     "invalid client_id format",
			requireRedis: false,
		},
		{
			name:         "valid format",
			clientID:     "valid-client-123",
			wantError:    false,
			requireRedis: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.requireRedis {
				setupTestRedis(t)
			}

			err := validateClient(tt.clientID)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errorMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Clean up auto-created client
				if redisClient != nil {
					redisClient.Del(context.Background(), "client_config:"+tt.clientID)
				}
			}
		})
	}
}

// --- Config Validation Tests ---

func TestValidateConfig(t *testing.T) {
	// Save original values
	origConcurrency := Concurrency
	origTaskTimeout := TaskTimeout
	origMaxRetry := MaxRetry
	origPort := Port
	origMode := Mode
	origLogLevel := LogLevel
	origCbFailureThreshold := cbFailureThreshold

	// Restore after test
	defer func() {
		Concurrency = origConcurrency
		TaskTimeout = origTaskTimeout
		MaxRetry = origMaxRetry
		Port = origPort
		Mode = origMode
		LogLevel = origLogLevel
		cbFailureThreshold = origCbFailureThreshold
	}()

	tests := []struct {
		name      string
		setup     func()
		wantError bool
		errorContains string
	}{
		{
			name: "valid config",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: false,
		},
		{
			name: "zero concurrency",
			setup: func() {
				Concurrency = 0
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "WORKER_CONCURRENCY must be positive",
		},
		{
			name: "negative concurrency",
			setup: func() {
				Concurrency = -1
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "WORKER_CONCURRENCY must be positive",
		},
		{
			name: "zero task timeout",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 0
				MaxRetry = 5
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "TASK_TIMEOUT_SEC must be positive",
		},
		{
			name: "negative max retry",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = -1
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "MAX_RETRY must be non-negative",
		},
		{
			name: "zero max retry is valid",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 0
				Port = "8080"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: false,
		},
		{
			name: "invalid port",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "invalid"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "PORT must be a valid port number",
		},
		{
			name: "port out of range",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "99999"
				Mode = "both"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "PORT must be a valid port number",
		},
		{
			name: "invalid mode",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "8080"
				Mode = "invalid"
				LogLevel = "info"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "MODE must be",
		},
		{
			name: "invalid log level",
			setup: func() {
				Concurrency = 10
				TaskTimeout = 1800
				MaxRetry = 5
				Port = "8080"
				Mode = "both"
				LogLevel = "invalid"
				cbFailureThreshold = 5
			},
			wantError: true,
			errorContains: "LOG_LEVEL must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			err := validateConfig()

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}
