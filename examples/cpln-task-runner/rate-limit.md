# Rate Limiting Implementation Guide

## Overview

This document provides a comprehensive step-by-step guide for implementing per-client rate limiting in the Task Dispatcher service. The implementation adds two-dimensional rate limiting (requests per minute and concurrent tasks) with client-specific configurations, override support, and a full Admin API for management.

## Architecture

### Rate Limiting Dimensions

1. **Time-Window Rate Limiting**: Limits requests per minute using Redis counters with sliding window
2. **Concurrency Control**: Limits concurrent task execution per client using Asynq Groups

### Data Flow

```
┌─────────────┐
│   Client    │
│  Request    │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────────────┐
│  1. Validate Client ID                  │
│     - Check if exists                   │
│     - Auto-create if new (default tier) │
│     - Verify enabled status             │
└──────┬──────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│  2. Determine Rate Limit                │
│     - Check for per-request override    │
│     - Lookup client config in Redis     │
│     - Apply tier defaults               │
│     - Fall back to "default" tier       │
└──────┬──────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│  3. Check Time-Window Limit             │
│     - Increment Redis counter           │
│     - Key: ratelimit:{client}:{minute}  │
│     - Compare to requests_per_min       │
│     - Return 429 if exceeded            │
└──────┬──────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│  4. Enqueue with Concurrency Control    │
│     - Use Asynq Group: client:{id}      │
│     - Set GroupMaxSize (max_concurrent) │
│     - Task queued successfully          │
└─────────────────────────────────────────┘
```

## Implementation Steps

### Step 1: Extend Data Structures

**Location**: `main.go` after line 35 (TaskPayload struct)

Add new type definitions:

```go
// RateConfig defines rate limiting parameters
type RateConfig struct {
    MaxConcurrent  int `json:"max_concurrent"`   // Max concurrent tasks
    RequestsPerMin int `json:"requests_per_min"` // Max tasks per minute
}

// ClientConfig stores per-client settings
type ClientConfig struct {
    ClientID      string      `json:"client_id"`
    Tier          string      `json:"tier"`           // free, basic, premium, enterprise
    RateLimit     *RateConfig `json:"rate_limit"`
    AllowedQueues []string    `json:"allowed_queues"` // Optional queue restrictions
    Enabled       bool        `json:"enabled"`
    CreatedAt     time.Time   `json:"created_at"`
    UpdatedAt     time.Time   `json:"updated_at"`
}
```

**Modify EnqueueRequest** (line 38-43):

```go
type EnqueueRequest struct {
    Queue             string      `json:"queue"`
    DelaySec          int         `json:"delay"`
    Task              TaskPayload `json:"task"`
    ClientID          string      `json:"client_id"`      // NEW: Required client identifier
    RateLimitOverride *RateConfig `json:"rate_limit"`     // NEW: Optional per-request override
}
```

### Step 2: Add Redis Client and Default Configs

**Location**: After line 21 (Port variable)

```go
var (
    RedisAddr   = getEnv("REDIS_ADDR", "localhost:6379")
    Port        = getEnv("PORT", "8080")
    redisClient *redis.Client // NEW: For rate limiting
)

// Default rate limits per tier
var DefaultRateLimits = map[string]*RateConfig{
    "free":       {MaxConcurrent: 1, RequestsPerMin: 10},
    "basic":      {MaxConcurrent: 5, RequestsPerMin: 100},
    "premium":    {MaxConcurrent: 20, RequestsPerMin: 1000},
    "enterprise": {MaxConcurrent: 50, RequestsPerMin: 5000},
    "default":    {MaxConcurrent: 3, RequestsPerMin: 50},
}
```

**Add import**:

```go
import (
    // ... existing imports
    "github.com/redis/go-redis/v9"
)
```

### Step 3: Initialize Redis Client

**Location**: Beginning of main() function (after line 45)

```go
func main() {
    // 1. Initialize Redis client for rate limiting
    redisClient = redis.NewClient(&redis.Options{
        Addr: RedisAddr,
    })
    defer redisClient.Close()

    // Verify Redis connection
    ctx := context.Background()
    if err := redisClient.Ping(ctx).Err(); err != nil {
        log.Fatalf("Failed to connect to Redis: %v", err)
    }
    log.Printf(" [*] Connected to Redis at %s", RedisAddr)

    // 2. Setup Asynq Redis Connection
    redisOpt := asynq.RedisClientOpt{Addr: RedisAddr}
    // ... rest of existing code
}
```

### Step 4: Implement Rate Limiting Functions

**Location**: Add before main() function

```go
// getRateLimitForClient determines the rate limit to apply
func getRateLimitForClient(clientID string, override *RateConfig) (*RateConfig, error) {
    // 1. If explicit override provided, validate and use it
    if override != nil && override.MaxConcurrent > 0 && override.RequestsPerMin > 0 {
        return override, nil
    }

    // 2. Look up client config from Redis
    config, err := getClientConfig(clientID)
    if err == nil && config.RateLimit != nil {
        return config.RateLimit, nil
    }

    // 3. If client has a tier, use tier defaults
    if err == nil && config.Tier != "" {
        if limit, ok := DefaultRateLimits[config.Tier]; ok {
            return limit, nil
        }
    }

    // 4. Fall back to default tier
    return DefaultRateLimits["default"], nil
}

// checkRateLimit validates if client is within rate limit using sliding window
func checkRateLimit(clientID string, requestsPerMin int) error {
    ctx := context.Background()
    key := fmt.Sprintf("ratelimit:%s:%d", clientID, time.Now().Unix()/60)
    
    // Increment counter for this minute
    count, err := redisClient.Incr(ctx, key).Result()
    if err != nil {
        return fmt.Errorf("rate limit check failed: %v", err)
    }

    // Set expiry on first request (2 minutes to handle edge cases)
    if count == 1 {
        redisClient.Expire(ctx, key, 2*time.Minute)
    }

    // Check if over limit
    if count > int64(requestsPerMin) {
        return fmt.Errorf("exceeded %d requests per minute (current: %d)", requestsPerMin, count)
    }

    return nil
}

// validateClient checks if client exists and is enabled
func validateClient(clientID string) error {
    if clientID == "" {
        return fmt.Errorf("client_id is required")
    }

    config, err := getClientConfig(clientID)
    if err != nil {
        // Client doesn't exist, create with default tier
        newConfig := ClientConfig{
            ClientID:  clientID,
            Tier:      "default",
            RateLimit: DefaultRateLimits["default"],
            Enabled:   true,
            CreatedAt: time.Now(),
            UpdatedAt: time.Now(),
        }
        if err := setClientConfig(newConfig); err != nil {
            return fmt.Errorf("failed to create client: %v", err)
        }
        return nil
    }

    if !config.Enabled {
        return fmt.Errorf("client is disabled")
    }

    return nil
}
```

### Step 5: Implement Client Config Storage Functions

**Location**: Add before main() function

```go
// setClientConfig stores client configuration in Redis
func setClientConfig(config ClientConfig) error {
    ctx := context.Background()
    config.UpdatedAt = time.Now()
    
    data, err := json.Marshal(config)
    if err != nil {
        return fmt.Errorf("failed to marshal config: %v", err)
    }
    
    key := fmt.Sprintf("client_config:%s", config.ClientID)
    if err := redisClient.Set(ctx, key, data, 0).Err(); err != nil {
        return fmt.Errorf("failed to store config: %v", err)
    }
    
    return nil
}

// getClientConfig retrieves client configuration from Redis
func getClientConfig(clientID string) (*ClientConfig, error) {
    ctx := context.Background()
    key := fmt.Sprintf("client_config:%s", clientID)
    
    data, err := redisClient.Get(ctx, key).Result()
    if err == redis.Nil {
        return nil, fmt.Errorf("client not found")
    }
    if err != nil {
        return nil, fmt.Errorf("failed to get config: %v", err)
    }
    
    var config ClientConfig
    if err := json.Unmarshal([]byte(data), &config); err != nil {
        return nil, fmt.Errorf("failed to unmarshal config: %v", err)
    }
    
    return &config, nil
}

// listAllClients retrieves all client configurations
func listAllClients() ([]ClientConfig, error) {
    ctx := context.Background()
    
    // Scan for all client config keys
    var cursor uint64
    var keys []string
    
    for {
        var scanKeys []string
        var err error
        scanKeys, cursor, err = redisClient.Scan(ctx, cursor, "client_config:*", 100).Result()
        if err != nil {
            return nil, err
        }
        keys = append(keys, scanKeys...)
        if cursor == 0 {
            break
        }
    }
    
    // Fetch all configs
    clients := make([]ClientConfig, 0, len(keys))
    for _, key := range keys {
        data, err := redisClient.Get(ctx, key).Result()
        if err != nil {
            continue
        }
        
        var config ClientConfig
        if err := json.Unmarshal([]byte(data), &config); err != nil {
            continue
        }
        clients = append(clients, config)
    }
    
    return clients, nil
}

// deleteClientConfig removes a client configuration
func deleteClientConfig(clientID string) error {
    ctx := context.Background()
    key := fmt.Sprintf("client_config:%s", clientID)
    
    if err := redisClient.Del(ctx, key).Err(); err != nil {
        return fmt.Errorf("failed to delete config: %v", err)
    }
    
    return nil
}
```

### Step 6: Update handleEnqueue with Rate Limiting

**Location**: Replace existing handleEnqueue function (starting at line 97)

```go
func handleEnqueue(w http.ResponseWriter, r *http.Request, client *asynq.Client) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var req EnqueueRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "Invalid body", http.StatusBadRequest)
        return
    }

    // Validate client
    if err := validateClient(req.ClientID); err != nil {
        http.Error(w, fmt.Sprintf("Client validation failed: %v", err), http.StatusUnauthorized)
        return
    }

    // Determine rate limit (override or configured)
    rateLimit, err := getRateLimitForClient(req.ClientID, req.RateLimitOverride)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to get rate limit: %v", err), http.StatusInternalServerError)
        return
    }

    // Check time-window rate limit (requests per minute)
    if err := checkRateLimit(req.ClientID, rateLimit.RequestsPerMin); err != nil {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusTooManyRequests)
        json.NewEncoder(w).Encode(map[string]interface{}{
            "error":            "Rate limit exceeded",
            "message":          err.Error(),
            "client_id":        req.ClientID,
            "requests_per_min": rateLimit.RequestsPerMin,
        })
        return
    }

    // Serialize the payload
    payload, err := json.Marshal(req.Task)
    if err != nil {
        http.Error(w, "Failed to marshal task", http.StatusInternalServerError)
        return
    }

    // Define Task Options with Group-based concurrency control
    opts := []asynq.Option{
        asynq.Queue(req.Queue),
        asynq.MaxRetry(5),
        asynq.Timeout(30 * time.Minute),
        // Group by client_id for per-client concurrency control
        asynq.Group(fmt.Sprintf("client:%s", req.ClientID)),
        asynq.GroupMaxSize(rateLimit.MaxConcurrent),
    }

    if req.DelaySec > 0 {
        opts = append(opts, asynq.ProcessIn(time.Duration(req.DelaySec)*time.Second))
    }

    // Enqueue the task
    task := asynq.NewTask(TypeHttpRequest, payload, opts...)
    info, err := client.Enqueue(task)
    if err != nil {
        http.Error(w, fmt.Sprintf("Could not enqueue task: %v", err), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated)
    json.NewEncoder(w).Encode(map[string]interface{}{
        "status":     "enqueued",
        "task_id":    info.ID,
        "queue":      info.Queue,
        "client_id":  req.ClientID,
        "rate_limit": rateLimit,
    })
}
```

### Step 7: Create Admin API Handlers

**Location**: Add before main() function

```go
// Admin API: Create or update client configuration
func handleAdminSetClient(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost && r.Method != http.MethodPut {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var config ClientConfig
    if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
        http.Error(w, "Invalid body", http.StatusBadRequest)
        return
    }

    // Validate required fields
    if config.ClientID == "" {
        http.Error(w, "client_id is required", http.StatusBadRequest)
        return
    }

    // Set timestamps
    existingConfig, err := getClientConfig(config.ClientID)
    if err == nil {
        config.CreatedAt = existingConfig.CreatedAt
    } else {
        config.CreatedAt = time.Now()
    }
    config.UpdatedAt = time.Now()

    // Apply tier defaults if rate_limit not specified
    if config.RateLimit == nil && config.Tier != "" {
        if limit, ok := DefaultRateLimits[config.Tier]; ok {
            config.RateLimit = limit
        }
    }

    // Default to enabled if not specified
    if existingConfig == nil {
        config.Enabled = true
    }

    if err := setClientConfig(config); err != nil {
        http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(config)
}

// Admin API: Get client configuration
func handleAdminGetClient(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    clientID := r.URL.Query().Get("client_id")
    if clientID == "" {
        http.Error(w, "client_id query parameter is required", http.StatusBadRequest)
        return
    }

    config, err := getClientConfig(clientID)
    if err != nil {
        http.Error(w, fmt.Sprintf("Client not found: %v", err), http.StatusNotFound)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(config)
}

// Admin API: List all clients
func handleAdminListClients(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    clients, err := listAllClients()
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to list clients: %v", err), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "clients": clients,
        "count":   len(clients),
    })
}

// Admin API: Delete client
func handleAdminDeleteClient(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodDelete {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    clientID := r.URL.Query().Get("client_id")
    if clientID == "" {
        http.Error(w, "client_id query parameter is required", http.StatusBadRequest)
        return
    }

    if err := deleteClientConfig(clientID); err != nil {
        http.Error(w, fmt.Sprintf("Failed to delete client: %v", err), http.StatusInternalServerError)
        return
    }

    w.WriteHeader(http.StatusNoContent)
}

// Admin API: Get available tiers
func handleAdminGetTiers(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "tiers": DefaultRateLimits,
    })
}
```

### Step 8: Register Admin API Routes

**Location**: In main() function after the /health endpoint (around line 85)

```go
// Admin API endpoints
http.HandleFunc("/admin/clients", handleAdminListClients)
http.HandleFunc("/admin/clients/set", handleAdminSetClient)
http.HandleFunc("/admin/clients/get", handleAdminGetClient)
http.HandleFunc("/admin/clients/delete", handleAdminDeleteClient)
http.HandleFunc("/admin/tiers", handleAdminGetTiers)
```

### Step 9: Update Dependencies

Run these commands:

```bash
go get github.com/redis/go-redis/v9
go mod tidy
```

### Step 10: Docker Configuration

No changes needed to `docker-compose.yml` - Redis is already configured with persistence.

## API Reference

### 1. Enqueue Task (with Rate Limiting)

**Endpoint**: `POST /v1/enqueue`

**Request Body**:
```json
{
  "queue": "default",
  "delay": 0,
  "client_id": "my-service-123",
  "task": {
    "url": "https://api.example.com/webhook",
    "method": "POST",
    "headers": {
      "Authorization": "Bearer token123"
    },
    "body": "{\"event\": \"user.created\"}"
  }
}
```

**Optional Per-Request Override**:
```json
{
  "queue": "default",
  "client_id": "my-service-123",
  "rate_limit": {
    "max_concurrent": 10,
    "requests_per_min": 200
  },
  "task": {
    "url": "https://api.example.com/webhook",
    "method": "POST"
  }
}
```

**Success Response** (201 Created):
```json
{
  "status": "enqueued",
  "task_id": "abc123-xyz789",
  "queue": "default",
  "client_id": "my-service-123",
  "rate_limit": {
    "max_concurrent": 3,
    "requests_per_min": 50
  }
}
```

**Rate Limit Exceeded** (429 Too Many Requests):
```json
{
  "error": "Rate limit exceeded",
  "message": "exceeded 50 requests per minute (current: 51)",
  "client_id": "my-service-123",
  "requests_per_min": 50
}
```

**Client Validation Failed** (401 Unauthorized):
```json
{
  "error": "Client validation failed: client is disabled"
}
```

### 2. Create/Update Client Configuration

**Endpoint**: `POST /admin/clients/set`

**Request Body**:
```json
{
  "client_id": "premium-customer-456",
  "tier": "premium",
  "enabled": true
}
```

**Or with custom rate limits**:
```json
{
  "client_id": "custom-client-789",
  "tier": "custom",
  "rate_limit": {
    "max_concurrent": 15,
    "requests_per_min": 500
  },
  "allowed_queues": ["default", "critical"],
  "enabled": true
}
```

**Response** (200 OK):
```json
{
  "client_id": "premium-customer-456",
  "tier": "premium",
  "rate_limit": {
    "max_concurrent": 20,
    "requests_per_min": 1000
  },
  "allowed_queues": null,
  "enabled": true,
  "created_at": "2024-01-15T10:30:00Z",
  "updated_at": "2024-01-15T10:30:00Z"
}
```

### 3. Get Client Configuration

**Endpoint**: `GET /admin/clients/get?client_id=my-service-123`

**Response** (200 OK):
```json
{
  "client_id": "my-service-123",
  "tier": "default",
  "rate_limit": {
    "max_concurrent": 3,
    "requests_per_min": 50
  },
  "allowed_queues": null,
  "enabled": true,
  "created_at": "2024-01-15T10:00:00Z",
  "updated_at": "2024-01-15T10:00:00Z"
}
```

### 4. List All Clients

**Endpoint**: `GET /admin/clients`

**Response** (200 OK):
```json
{
  "clients": [
    {
      "client_id": "service-a",
      "tier": "basic",
      "rate_limit": {
        "max_concurrent": 5,
        "requests_per_min": 100
      },
      "enabled": true,
      "created_at": "2024-01-15T09:00:00Z",
      "updated_at": "2024-01-15T09:00:00Z"
    },
    {
      "client_id": "service-b",
      "tier": "premium",
      "rate_limit": {
        "max_concurrent": 20,
        "requests_per_min": 1000
      },
      "enabled": true,
      "created_at": "2024-01-15T10:00:00Z",
      "updated_at": "2024-01-15T10:00:00Z"
    }
  ],
  "count": 2
}
```

### 5. Delete Client

**Endpoint**: `DELETE /admin/clients/delete?client_id=my-service-123`

**Response**: 204 No Content

### 6. Get Available Tiers

**Endpoint**: `GET /admin/tiers`

**Response** (200 OK):
```json
{
  "tiers": {
    "free": {
      "max_concurrent": 1,
      "requests_per_min": 10
    },
    "basic": {
      "max_concurrent": 5,
      "requests_per_min": 100
    },
    "premium": {
      "max_concurrent": 20,
      "requests_per_min": 1000
    },
    "enterprise": {
      "max_concurrent": 50,
      "requests_per_min": 5000
    },
    "default": {
      "max_concurrent": 3,
      "requests_per_min": 50
    }
  }
}
```

## Rate Limiting Behavior

### Time-Window Rate Limiting

- Uses Redis INCR command with per-minute keys
- Key format: `ratelimit:{client_id}:{unix_minute}`
- Counters expire after 2 minutes (safety margin)
- Sliding window behavior - resets every minute
- Returns 429 when limit exceeded

### Concurrency Control

- Uses Asynq Groups: `client:{client_id}`
- GroupMaxSize limits concurrent task execution
- Tasks queue up when limit reached (not rejected)
- Fair scheduling across clients
- Per-client isolation

### Override Priority

1. **Per-request override** (highest priority) - if provided in request body
2. **Client-specific config** - from Redis storage
3. **Tier defaults** - based on client's tier
4. **Default tier** - fallback if nothing else applies

### Auto-Client Creation

When a new `client_id` is encountered:
- Automatically creates client config
- Assigns "default" tier
- Sets enabled = true
- Logs creation event

## Testing Guide

### Unit Tests

Create `main_test.go`:

```go
package main

import (
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestGetRateLimitForClient(t *testing.T) {
    // Test with override
    override := &RateConfig{MaxConcurrent: 10, RequestsPerMin: 200}
    result, err := getRateLimitForClient("test-client", override)
    assert.NoError(t, err)
    assert.Equal(t, 10, result.MaxConcurrent)
    assert.Equal(t, 200, result.RequestsPerMin)

    // Test fallback to default
    result, err = getRateLimitForClient("nonexistent-client", nil)
    assert.NoError(t, err)
    assert.Equal(t, DefaultRateLimits["default"], result)
}

func TestCheckRateLimit(t *testing.T) {
    // Setup Redis client for testing
    // Test within limit
    // Test exceeded limit
}
```

### Integration Tests

Test script `test_rate_limiting.sh`:

```bash
#!/bin/bash

API_URL="http://localhost:8080"
CLIENT_ID="test-client-$(date +%s)"

# 1. Create a client with low limits
echo "Creating test client..."
curl -X POST $API_URL/admin/clients/set \
  -H "Content-Type: application/json" \
  -d "{
    \"client_id\": \"$CLIENT_ID\",
    \"tier\": \"free\",
    \"enabled\": true
  }"

# 2. Test successful enqueue
echo -e "\n\nTest 1: Successful enqueue"
curl -X POST $API_URL/v1/enqueue \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$CLIENT_ID\",
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\",
      \"body\": \"{\\\"test\\\": true}\"
    }
  }"

# 3. Test rate limit (free tier = 10 req/min)
echo -e "\n\nTest 2: Testing rate limit..."
for i in {1..12}; do
  response=$(curl -s -w "\n%{http_code}" -X POST $API_URL/v1/enqueue \
    -H "Content-Type: application/json" \
    -d "{
      \"queue\": \"default\",
      \"client_id\": \"$CLIENT_ID\",
      \"task\": {
        \"url\": \"https://httpbin.org/post\",
        \"method\": \"POST\"
      }
    }")
  
  http_code=$(echo "$response" | tail -n1)
  echo "Request $i: HTTP $http_code"
  
  if [ "$http_code" = "429" ]; then
    echo "✓ Rate limit enforced at request $i"
    break
  fi
done

# 4. Test per-request override
echo -e "\n\nTest 3: Per-request override"
curl -X POST $API_URL/v1/enqueue \
  -H "Content-Type: application/json" \
  -d "{
    \"queue\": \"default\",
    \"client_id\": \"$CLIENT_ID\",
    \"rate_limit\": {
      \"max_concurrent\": 100,
      \"requests_per_min\": 1000
    },
    \"task\": {
      \"url\": \"https://httpbin.org/post\",
      \"method\": \"POST\"
    }
  }"

# 5. Cleanup
echo -e "\n\nCleaning up..."
curl -X DELETE "$API_URL/admin/clients/delete?client_id=$CLIENT_ID"
echo -e "\nTests complete!"
```

### Load Testing

Use Apache Bench or k6:

```bash
# Test with ab
ab -n 1000 -c 10 -p payload.json -T application/json \
  http://localhost:8080/v1/enqueue
```

Or with k6:

```javascript
// load_test.js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  stages: [
    { duration: '30s', target: 10 },
    { duration: '1m', target: 50 },
    { duration: '30s', target: 0 },
  ],
};

export default function () {
  const payload = JSON.stringify({
    queue: 'default',
    client_id: 'load-test-client',
    task: {
      url: 'https://httpbin.org/post',
      method: 'POST',
    },
  });

  const res = http.post('http://localhost:8080/v1/enqueue', payload, {
    headers: { 'Content-Type': 'application/json' },
  });

  check(res, {
    'status is 201 or 429': (r) => r.status === 201 || r.status === 429,
  });
}
```

Run: `k6 run load_test.js`

## Monitoring and Observability

### Key Metrics to Track

1. **Rate Limit Hits**
   - Count of 429 responses per client
   - Percentage of requests rate-limited
   - Peak usage times

2. **Client Metrics**
   - Active clients count
   - Tasks enqueued per client
   - Average tasks per minute per client
   - Client tier distribution

3. **System Metrics**
   - Redis operation latency
   - Redis connection pool usage
   - Task queue depths per client group
   - Worker utilization

4. **Business Metrics**
   - Revenue per tier
   - Tier upgrade candidates (hitting limits)
   - Disabled clients count

### Logging Recommendations

Add structured logging:

```go
// When rate limit is exceeded
log.Printf("[RATE_LIMIT] client=%s requests_per_min=%d current=%d",
    clientID, limit, currentCount)

// When client is auto-created
log.Printf("[CLIENT_CREATED] client=%s tier=default", clientID)

// Admin API operations
log.Printf("[ADMIN] action=set_client client=%s tier=%s",
    config.ClientID, config.Tier)

// Redis errors
log.Printf("[ERROR] redis_operation=get_config client=%s error=%v",
    clientID, err)
```

### Prometheus Metrics (Optional Enhancement)

Consider adding Prometheus metrics:

```go
import "github.com/prometheus/client_golang/prometheus"

var (
    rateLimitExceeded = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "rate_limit_exceeded_total",
            Help: "Total number of rate limit exceeded errors",
        },
        []string{"client_id", "tier"},
    )
    
    tasksEnqueued = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "tasks_enqueued_total",
            Help: "Total number of tasks enqueued",
        },
        []string{"client_id", "queue"},
    )
)
```

### Dashboard Queries (Asynqmon)

Monitor via Asynqmon UI (http://localhost:8081):

- View tasks per group (client)
- Check queue depths
- Monitor success/failure rates
- Inspect retry counts

## Migration Guide

### For New Deployments

1. Deploy with rate limiting enabled
2. All clients must provide `client_id` in requests
3. Clients auto-created with default tier on first request
4. Use Admin API to upgrade clients to appropriate tiers

### For Existing Deployments

#### Option 1: Breaking Change (Recommended)

1. **Announcement**: Notify all API consumers 2 weeks in advance
2. **Update clients**: All must add `client_id` to requests
3. **Deploy**: Roll out rate limiting
4. **Monitor**: Watch for failed requests, assist clients

#### Option 2: Backward Compatible (Temporary)

Add fallback in `handleEnqueue`:

```go
// Add at start of function, after decoding request
if req.ClientID == "" {
    req.ClientID = "legacy-default"
    log.Printf("[WARNING] Request without client_id, using legacy-default")
}
```

This allows existing clients to continue working while they migrate.

#### Migration Steps

1. **Pre-deployment**:
   ```bash
   # Create legacy client with generous limits
   curl -X POST http://localhost:8080/admin/clients/set \
     -H "Content-Type: application/json" \
     -d '{
       "client_id": "legacy-default",
       "tier": "premium",
       "enabled": true
     }'
   ```

2. **Deploy** rate limiting code with fallback

3. **Monitor** `legacy-default` usage:
   ```bash
   # Check Redis
   redis-cli GET client_config:legacy-default
   redis-cli KEYS "ratelimit:legacy-default:*"
   ```

4. **Migrate clients** one by one:
   - Provide each with unique `client_id`
   - Assign appropriate tier
   - Update their code to include `client_id`

5. **Remove fallback** after all clients migrated

6. **Disable legacy client**:
   ```bash
   curl -X POST http://localhost:8080/admin/clients/set \
     -H "Content-Type: application/json" \
     -d '{
       "client_id": "legacy-default",
       "enabled": false
     }'
   ```

### Populating Initial Clients

Create script `seed_clients.sh`:

```bash
#!/bin/bash

ADMIN_URL="http://localhost:8080"

# Array of clients to create
declare -a CLIENTS=(
  "service-a:premium"
  "service-b:basic"
  "service-c:free"
  "internal-api:enterprise"
)

for client in "${CLIENTS[@]}"; do
  IFS=':' read -r client_id tier <<< "$client"
  
  echo "Creating $client_id with $tier tier..."
  curl -X POST $ADMIN_URL/admin/clients/set \
    -H "Content-Type: application/json" \
    -d "{
      \"client_id\": \"$client_id\",
      \"tier\": \"$tier\",
      \"enabled\": true
    }"
  echo ""
done

echo "Client seeding complete!"
```

## Security Considerations

### Admin API Protection

The Admin API is powerful and should be protected. Choose one or more:

#### 1. Environment-Based API Key (Simplest)

Add to `main.go`:

```go
// Admin auth middleware
func adminAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
    adminKey := getEnv("ADMIN_API_KEY", "")
    return func(w http.ResponseWriter, r *http.Request) {
        if adminKey != "" {
            providedKey := r.Header.Get("X-Admin-Key")
            if providedKey != adminKey {
                log.Printf("[SECURITY] Unauthorized admin API access attempt from %s", r.RemoteAddr)
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
        }
        next(w, r)
    }
}
```

Update route registration:

```go
// Protected admin endpoints
http.HandleFunc("/admin/clients", adminAuthMiddleware(handleAdminListClients))
http.HandleFunc("/admin/clients/set", adminAuthMiddleware(handleAdminSetClient))
http.HandleFunc("/admin/clients/get", adminAuthMiddleware(handleAdminGetClient))
http.HandleFunc("/admin/clients/delete", adminAuthMiddleware(handleAdminDeleteClient))
http.HandleFunc("/admin/tiers", adminAuthMiddleware(handleAdminGetTiers))
```

Set in `docker-compose.yml`:

```yaml
environment:
  - ADMIN_API_KEY=your-secure-random-key-here
```

Usage:

```bash
curl -X GET http://localhost:8080/admin/clients \
  -H "X-Admin-Key: your-secure-random-key-here"
```

#### 2. Network-Level Restriction

In `docker-compose.yml`, don't expose admin endpoints externally:

```yaml
services:
  dispatcher:
    ports:
      - "8080:8080"  # Only public API
    networks:
      - internal
  
  admin:
    build: .
    command: ["./task-dispatcher", "--admin-only"]
    ports:
      - "127.0.0.1:8081:8081"  # Admin API on localhost only
    networks:
      - internal
```

#### 3. Separate Admin Service

Run admin API on different port/service accessible only via VPN or internal network.

#### 4. IP Whitelisting

Add IP filtering:

```go
func ipWhitelistMiddleware(allowedIPs []string) func(http.HandlerFunc) http.HandlerFunc {
    return func(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
            clientIP := r.RemoteAddr
            // Extract IP without port
            if idx := strings.LastIndex(clientIP, ":"); idx != -1 {
                clientIP = clientIP[:idx]
            }
            
            allowed := false
            for _, ip := range allowedIPs {
                if ip == clientIP {
                    allowed = true
                    break
                }
            }
            
            if !allowed {
                log.Printf("[SECURITY] Blocked admin access from %s", clientIP)
                http.Error(w, "Forbidden", http.StatusForbidden)
                return
            }
            
            next(w, r)
        }
    }
}
```

### Client ID Validation

Consider adding validation rules:

```go
func isValidClientID(clientID string) bool {
    // Only allow alphanumeric and hyphens
    matched, _ := regexp.MatchString(`^[a-zA-Z0-9-_]+$`, clientID)
    return matched && len(clientID) >= 3 && len(clientID) <= 64
}
```

Add to `validateClient`:

```go
if !isValidClientID(clientID) {
    return fmt.Errorf("invalid client_id format")
}
```

### Rate Limit Bypass Prevention

- Store rate limits in Redis (not in request)
- Validate overrides against max allowed values
- Log all override attempts
- Monitor for abuse patterns

### Redis Security

1. **Password protection**: Set Redis password
2. **Network isolation**: Redis not exposed externally
3. **AOF/RDB encryption**: Encrypt persistence files
4. **Key expiration**: Set TTLs appropriately

Update `docker-compose.yml`:

```yaml
redis:
  command: redis-server --requirepass ${REDIS_PASSWORD}
  environment:
    - REDIS_PASSWORD=${REDIS_PASSWORD}
```

## Performance Considerations

### Redis Connection Pool

Configure connection pooling:

```go
redisClient = redis.NewClient(&redis.Options{
    Addr:         RedisAddr,
    PoolSize:     100,
    MinIdleConns: 10,
    MaxRetries:   3,
    DialTimeout:  5 * time.Second,
    ReadTimeout:  3 * time.Second,
    WriteTimeout: 3 * time.Second,
})
```

### Rate Limit Cache

Consider caching client configs in memory:

```go
var clientConfigCache sync.Map

func getCachedClientConfig(clientID string) (*ClientConfig, error) {
    // Check cache first
    if cached, ok := clientConfigCache.Load(clientID); ok {
        return cached.(*ClientConfig), nil
    }
    
    // Fetch from Redis
    config, err := getClientConfig(clientID)
    if err != nil {
        return nil, err
    }
    
    // Cache for 1 minute
    clientConfigCache.Store(clientID, config)
    go func() {
        time.Sleep(1 * time.Minute)
        clientConfigCache.Delete(clientID)
    }()
    
    return config, nil
}
```

### Database Offloading (Future Enhancement)

For very large deployments, consider:

1. **PostgreSQL for configs**: Store client configs in DB
2. **Redis for rate limiting**: Keep counters in Redis
3. **Cache layer**: Memory cache + Redis + DB

## Troubleshooting

### Common Issues

#### 1. All Requests Return 429

**Symptoms**: Every request rate limited immediately

**Causes**:
- Redis time drift
- Incorrect rate limit calculation
- Redis key collision

**Debug**:
```bash
# Check Redis keys
redis-cli KEYS "ratelimit:*"

# Check values
redis-cli GET "ratelimit:my-client:12345"

# Check TTL
redis-cli TTL "ratelimit:my-client:12345"
```

**Fix**: Verify time synchronization, check key format

#### 2. Tasks Not Processing

**Symptoms**: Tasks enqueued but never execute

**Causes**:
- GroupMaxSize too low
- All workers busy
- Queue priorities misconfigured

**Debug**:
```bash
# Check Asynqmon UI
open http://localhost:8081

# Check Redis queues
redis-cli LLEN "asynq:queues:default"

# Check groups
redis-cli KEYS "asynq:groups:*"
```

**Fix**: Increase MaxConcurrent, add more workers, check Asynq config

#### 3. Client Auto-Creation Fails

**Symptoms**: Validation errors for new clients

**Causes**:
- Redis connection issues
- Insufficient permissions
- Invalid client_id format

**Debug**:
```bash
# Test Redis connection
redis-cli PING

# Check error logs
docker logs task-dispatcher | grep ERROR
```

**Fix**: Verify Redis connectivity, check permissions

#### 4. Admin API Not Responding

**Symptoms**: Admin endpoints return errors

**Causes**:
- Authentication failure
- Routes not registered
- Middleware blocking requests

**Debug**:
```bash
# Test without auth
curl -v http://localhost:8080/admin/tiers

# Check routes
grep "HandleFunc" main.go
```

**Fix**: Verify ADMIN_API_KEY, check route registration

## Future Enhancements

### 1. Dynamic Rate Limit Adjustment

Auto-scale limits based on usage patterns:

```go
func adjustRateLimits() {
    clients, _ := listAllClients()
    for _, client := range clients {
        usage := getClientUsage(client.ClientID)
        if usage > 0.9 * client.RateLimit.RequestsPerMin {
            // Notify or auto-upgrade
        }
    }
}
```

### 2. Rate Limit Querying API

Let clients check their limits:

```go
// GET /v1/rate-limit?client_id=xxx
func handleGetMyRateLimit(w http.ResponseWriter, r *http.Request) {
    clientID := r.URL.Query().Get("client_id")
    config, _ := getClientConfig(clientID)
    
    // Get current usage
    currentMinute := time.Now().Unix() / 60
    key := fmt.Sprintf("ratelimit:%s:%d", clientID, currentMinute)
    current, _ := redisClient.Get(context.Background(), key).Int64()
    
    json.NewEncoder(w).Encode(map[string]interface{}{
        "client_id":        clientID,
        "rate_limit":       config.RateLimit,
        "current_usage":    current,
        "remaining":        config.RateLimit.RequestsPerMin - int(current),
    })
}
```

### 3. Usage Analytics

Track and visualize client usage over time.

### 4. Webhooks for Limit Events

Notify clients when approaching limits:

```go
if usage > 0.8 * limit {
    sendWebhook(client.WebhookURL, "rate_limit_warning", usage)
}
```

### 5. Multi-Region Support

Distribute rate limiting across regions with eventual consistency.

### 6. Cost-Based Rate Limiting

Different costs for different operations:

```go
type Task struct {
    // ...
    Cost int // 1 for simple, 5 for complex
}
```

Deduct `cost * requests` instead of just `requests`.

## Summary

This implementation provides:

- ✅ Two-dimensional rate limiting (time + concurrency)
- ✅ Per-client configuration with tier system
- ✅ Full Admin API for management
- ✅ Auto-client creation for easy onboarding
- ✅ Per-request override capability
- ✅ Redis-based storage (fast and persistent)
- ✅ Fair scheduling across clients
- ✅ Comprehensive testing strategy
- ✅ Security considerations
- ✅ Migration path for existing systems

**Estimated Implementation Time**: 6-9 hours

**Key Files**:
- `main.go` - All implementation code
- `go.mod` - Add redis dependency
- `docker-compose.yml` - Already configured
- `rate-limit.md` - This documentation

## Quick Start Checklist

- [ ] Step 1: Add new data structures to `main.go`
- [ ] Step 2: Add Redis client variable and defaults
- [ ] Step 3: Initialize Redis in main()
- [ ] Step 4: Implement rate limiting functions
- [ ] Step 5: Implement storage functions
- [ ] Step 6: Update handleEnqueue function
- [ ] Step 7: Add admin API handlers
- [ ] Step 8: Register admin routes
- [ ] Step 9: Run `go get` and `go mod tidy`
- [ ] Step 10: Test locally with `docker-compose up`
- [ ] Step 11: Run integration tests
- [ ] Step 12: Deploy to production
- [ ] Step 13: Monitor metrics and logs
- [ ] Step 14: Document for your team

Good luck with the implementation!

