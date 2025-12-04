# Task Dispatcher - Project Analysis

## Executive Summary

Task Dispatcher is a self-hosted task queue and scheduler service built in Go that replicates Google Cloud Tasks functionality. It uses Redis for storage via the Asynq library and provides HTTP-based task enqueuing with automatic retry, delayed execution, and per-client rate limiting.

---

## 1. Project Structure

```
asim/
├── main.go              # Single-file application (579 lines)
├── go.mod               # Go 1.22 module definition
├── go.sum               # Dependency lockfile
├── docker-compose.yml   # Multi-service orchestration
├── Dockerfile           # Multi-stage container build
├── redis-dockerfile     # Custom Redis configuration
├── cpln-build.sh        # ControlPlane deployment script
├── test_rate_limiting.sh # Integration test suite
├── README.md            # User documentation
├── rate-limit.md        # Rate limiting implementation guide
└── .claude/             # Claude Code settings
```

### Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `github.com/hibiken/asynq` | v0.24.1 | Task queue library |
| `github.com/redis/go-redis/v9` | v9.0.3 | Redis client for rate limiting |

---

## 2. Architecture

### High-Level Architecture

```
┌─────────────┐     ┌──────────────────┐     ┌─────────────┐
│   Client    │────▶│  Task Dispatcher │────▶│   Redis     │
│   (HTTP)    │     │   (Go Service)   │     │  (Asynq)    │
└─────────────┘     └────────┬─────────┘     └──────┬──────┘
                             │                       │
                             │                       ▼
                             │              ┌─────────────────┐
                             │              │   Asynq Worker  │
                             │              │   (Goroutine)   │
                             │              └────────┬────────┘
                             │                       │
                             │                       ▼
                             │              ┌─────────────────┐
                             └──────────────│  Target HTTP    │
                                            │   Endpoint      │
                                            └─────────────────┘
```

### Component Breakdown

1. **HTTP API Server** (`main.go:299-319`)
   - Listens on configurable port (default: 8080)
   - Endpoints: `/v1/enqueue`, `/health`, `/admin/*`
   - Uses standard `net/http` library

2. **Asynq Worker** (`main.go:292-297`)
   - Runs as a goroutine within the same process
   - Concurrency: 10 workers (hardcoded)
   - Priority queues: critical(6), default(3), low(1)

3. **Redis Storage**
   - Dual-purpose: Asynq task storage + rate limiting counters
   - Single connection shared between Asynq and rate limiting

4. **Rate Limiting** (`main.go:193-215`)
   - Time-window based (requests per minute)
   - Redis INCR with TTL for sliding window
   - Per-client configuration with tier system

---

## 3. API Reference

### Core Endpoints

#### POST `/v1/enqueue`
Enqueue a task for background HTTP execution.

**Request:**
```json
{
  "queue": "default",
  "delay": 0,
  "client_id": "my-service",
  "rate_limit": {                    // Optional override
    "max_concurrent": 10,
    "requests_per_min": 200
  },
  "task": {
    "url": "https://target.com/webhook",
    "method": "POST",
    "headers": {"Authorization": "Bearer token"},
    "body": "{\"data\": \"value\"}"
  }
}
```

**Response (201):**
```json
{
  "status": "enqueued",
  "task_id": "abc123",
  "queue": "default",
  "client_id": "my-service",
  "rate_limit": {"max_concurrent": 3, "requests_per_min": 50}
}
```

**Error Responses:**
- `400 Bad Request` - Invalid JSON
- `401 Unauthorized` - Client validation failed
- `429 Too Many Requests` - Rate limit exceeded

#### GET `/health`
Health check endpoint. Returns `200 OK`.

### Admin Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/admin/clients` | GET | List all clients |
| `/admin/clients/set` | POST/PUT | Create/update client |
| `/admin/clients/get?client_id=X` | GET | Get client config |
| `/admin/clients/delete?client_id=X` | DELETE | Delete client |
| `/admin/tiers` | GET | List available tiers |

### Rate Limit Tiers

| Tier | Max Concurrent | Requests/Min |
|------|----------------|--------------|
| free | 1 | 10 |
| basic | 5 | 100 |
| premium | 20 | 1,000 |
| enterprise | 50 | 5,000 |
| default | 3 | 50 |

---

## 4. Supported Use Cases

### Primary Use Cases

1. **Webhook Delivery**
   - Queue HTTP callbacks to external services
   - Retry on failure with exponential backoff

2. **Delayed Job Execution**
   - Schedule tasks for future execution (via `delay` parameter)
   - Useful for reminder emails, cleanup tasks

3. **Background Processing**
   - Offload slow HTTP calls from synchronous request paths
   - Decouple services with reliable delivery

4. **Multi-Tenant Rate Limiting**
   - Per-client throttling for SaaS platforms
   - Tiered service levels (free/basic/premium/enterprise)

### Example Scenarios

- **Email Service**: Queue transactional emails with retry
- **Payment Webhooks**: Reliable delivery of payment notifications
- **API Gateway**: Fan-out requests to multiple microservices
- **Event Processing**: Delayed event delivery with deduplication

---

## 5. Areas for Improvement

### 5.1 Reliability Issues

#### Issue 1: Single Point of Failure (Critical)
**Location:** `main.go:292-297`

The worker runs as a goroutine inside the API server. If the worker crashes, the entire service goes down.

```go
go func() {
    if err := srv.Run(mux); err != nil {
        log.Fatalf("could not run server: %v", err)  // Kills entire process
    }
}()
```

**Recommendation:**
- Separate API and worker into distinct processes/containers
- Use graceful shutdown with `srv.Shutdown()`
- Implement health checks for worker status

#### Issue 2: No Graceful Shutdown
**Location:** `main.go:317-319`

The HTTP server has no shutdown handling. In-flight requests are dropped on termination.

```go
if err := http.ListenAndServe(":"+Port, nil); err != nil {
    log.Fatalf("could not listen on port %s: %v", Port, err)
}
```

**Recommendation:**
```go
srv := &http.Server{Addr: ":" + Port}
go srv.ListenAndServe()
// Handle SIGTERM/SIGINT
<-stopChan
srv.Shutdown(context.Background())
```

#### Issue 3: Expire Error Not Handled
**Location:** `main.go:206-207`

```go
if count == 1 {
    redisClient.Expire(ctx, key, 2*time.Minute)  // Error ignored
}
```

**Recommendation:** Log or handle the Expire error.

#### Issue 4: No Dead Letter Queue
Failed tasks after max retries are marked as archived but there's no notification or DLQ mechanism.

**Recommendation:** Implement a dead letter handler or webhook for failed tasks.

---

### 5.2 Performance Issues

#### Issue 1: N+1 Query in listAllClients
**Location:** `main.go:117-153`

```go
for _, key := range keys {
    data, err := redisClient.Get(ctx, key).Result()  // N Redis calls
}
```

**Recommendation:** Use `MGET` for batch retrieval:
```go
values, err := redisClient.MGet(ctx, keys...).Result()
```

#### Issue 2: No Connection Pooling Configuration
**Location:** `main.go:250-252`

```go
redisClient = redis.NewClient(&redis.Options{
    Addr: RedisAddr,  // No pool configuration
})
```

**Recommendation:**
```go
redisClient = redis.NewClient(&redis.Options{
    Addr:         RedisAddr,
    PoolSize:     100,
    MinIdleConns: 10,
    DialTimeout:  5 * time.Second,
})
```

#### Issue 3: HTTP Client Not Reused
**Location:** `main.go:554`

```go
httpClient := &http.Client{Timeout: 60 * time.Second}  // Created per request
```

**Recommendation:** Create a global HTTP client with connection pooling:
```go
var httpClient = &http.Client{
    Timeout: 60 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
        IdleConnTimeout:     90 * time.Second,
    },
}
```

#### Issue 4: Hardcoded Concurrency
**Location:** `main.go:273`

```go
Concurrency: 10,  // Not configurable
```

**Recommendation:** Make configurable via environment variable.

---

### 5.3 Scalability Issues

#### Issue 1: Single Instance Limitation
The service is designed to run as a single instance. No distributed coordination.

**Recommendations:**
- Use Redis Cluster for horizontal scaling
- Implement leader election for admin operations
- Add instance-specific metrics

#### Issue 2: No Queue Isolation
All queues share the same concurrency pool (10 workers).

**Recommendation:** Configure per-queue concurrency limits in Asynq config.

#### Issue 3: SCAN Operation for Client Listing
**Location:** `main.go:127`

```go
scanKeys, cursor, err = redisClient.Scan(ctx, cursor, "client_config:*", 100).Result()
```

SCAN is O(N) and blocks Redis. With thousands of clients, this degrades performance.

**Recommendations:**
- Maintain a separate SET of client IDs
- Implement pagination for the admin API
- Consider a secondary database for client configs

#### Issue 4: No Metrics or Observability
No Prometheus metrics, distributed tracing, or structured logging.

**Recommendations:**
- Add Prometheus `/metrics` endpoint
- Implement OpenTelemetry tracing
- Use structured JSON logging

---

### 5.4 Security Issues

#### Issue 1: No Authentication on Admin API (Critical)
**Location:** `main.go:310-314`

```go
http.HandleFunc("/admin/clients", handleAdminListClients)
http.HandleFunc("/admin/clients/set", handleAdminSetClient)
// No authentication middleware!
```

**Recommendation:** Add API key authentication:
```go
func adminAuth(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        key := r.Header.Get("X-Admin-Key")
        if key != os.Getenv("ADMIN_API_KEY") {
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
        next(w, r)
    }
}
```

#### Issue 2: No Client ID Validation
**Location:** `main.go:218-219`

```go
if clientID == "" {
    return fmt.Errorf("client_id is required")
}
// No format validation
```

**Recommendation:** Validate client ID format to prevent injection:
```go
func isValidClientID(id string) bool {
    matched, _ := regexp.MatchString(`^[a-zA-Z0-9_-]{3,64}$`, id)
    return matched
}
```

#### Issue 3: Rate Limit Override Bypass
**Location:** `main.go:171-174`

```go
if override != nil && override.MaxConcurrent > 0 && override.RequestsPerMin > 0 {
    return override, nil  // Any client can override their limits!
}
```

**Recommendation:** Either remove this feature or validate against maximum allowed values per tier.

#### Issue 4: No HTTPS Enforcement
The service only listens on HTTP.

**Recommendation:**
- Add TLS configuration option
- Or document reverse proxy (nginx/traefik) requirement

#### Issue 5: No Request Body Size Limit
**Location:** `main.go:330`

```go
json.NewDecoder(r.Body).Decode(&req)  // Unlimited body size
```

**Recommendation:**
```go
r.Body = http.MaxBytesReader(w, r.Body, 1<<20)  // 1MB limit
```

#### Issue 6: Sensitive Headers in Logs
**Location:** `main.go:539`

```go
log.Printf("Execute Task: %s %s", p.Method, p.URL)
```

Authorization headers could be logged if debug logging is added.

**Recommendation:** Redact sensitive headers before logging.

#### Issue 7: Redis Without Authentication
**Location:** `docker-compose.yml:13-17`

```yaml
redis:
  image: redis:7-alpine
  # No password configured
```

**Recommendation:**
```yaml
redis:
  command: redis-server --requirepass ${REDIS_PASSWORD}
```

---

## 6. Improvement Priority Matrix

| Issue | Severity | Effort | Priority | Status |
|-------|----------|--------|----------|--------|
| No Admin API Auth | Critical | Low | P0 | ✅ FIXED |
| Rate Limit Override Bypass | High | Low | P0 | ✅ FIXED |
| Single Point of Failure | High | Medium | P1 | ✅ FIXED (API/Worker separation) |
| No Graceful Shutdown | Medium | Low | P1 | ✅ FIXED |
| No Request Body Limit | Medium | Low | P1 | ✅ FIXED |
| Redis Without Auth | Medium | Low | P1 | ✅ FIXED |
| N+1 Query in listAllClients | Medium | Low | P2 | ✅ FIXED |
| HTTP Client Not Reused | Low | Low | P2 | ✅ FIXED |
| No Connection Pooling | Low | Low | P2 | ✅ FIXED |
| Hardcoded Concurrency | Low | Low | P2 | ✅ FIXED |
| No Client ID Validation | Medium | Low | P2 | ✅ FIXED |
| Expire Error Not Handled | Low | Low | P2 | ✅ FIXED |
| SCAN for Client Listing | Low | Medium | P3 | Deferred |
| No Metrics | Low | Medium | P3 | ✅ FIXED (Prometheus) |
| No Dead Letter Queue | Low | Medium | P3 | Deferred |
| SSRF Vulnerability | Critical | Medium | P0 | ✅ FIXED |
| AllowedQueues Not Enforced | High | Low | P1 | ✅ FIXED |
| Response Body Unbounded | High | Low | P1 | ✅ FIXED (10MB limit) |
| No Retry-After Header | Medium | Low | P1 | ✅ FIXED |
| Context Not Propagated | Medium | Low | P1 | ✅ FIXED |
| Hardcoded Task Timeout | Medium | Low | P2 | ✅ FIXED |
| Hardcoded Max Retry | Low | Low | P2 | ✅ FIXED |
| Basic Health Check | Low | Low | P2 | ✅ FIXED (Redis ping) |
| No Request Tracing | Low | Low | P2 | ✅ FIXED (Request ID) |

### 6.1 Implementation Details of Fixes

#### P0: Admin API Authentication
- Added `ADMIN_API_KEY` environment variable for authentication
- Implemented `adminAuth` middleware that validates `X-Admin-Key` header
- All `/admin/*` endpoints now require authentication when `ADMIN_API_KEY` is set
- Warning logged if admin key not configured (for development)

#### P0: Rate Limit Override Bypass
- Modified `getRateLimitForClient()` to validate overrides against tier maximums
- Override values are now clamped to the client's tier limit using `min()` function
- Clients cannot exceed their tier's configured rate limits via request override

#### P1: Graceful Shutdown
- Replaced `http.ListenAndServe()` with `http.Server` struct
- Added signal handling for `SIGINT` and `SIGTERM`
- Proper shutdown sequence: Asynq worker first, then HTTP server
- 30-second shutdown timeout to allow in-flight requests to complete
- Worker errors no longer use `log.Fatalf()` (which killed the process)

#### P1: Request Body Size Limit
- Added `http.MaxBytesReader()` wrapper limiting bodies to 1MB
- Applied to both `/v1/enqueue` and `/admin/clients/set` endpoints
- Returns `413 Request Entity Too Large` for oversized requests

#### P1: Redis Authentication
- Added `REDIS_PASSWORD` environment variable support
- Both go-redis client and Asynq client configured with password
- `docker-compose.yml` updated with conditional Redis password configuration
- Health checks added for Redis service dependency

#### P2: N+1 Query Fix
- Replaced individual `GET` calls in `listAllClients()` with batch `MGET`
- Single Redis round-trip for all client configurations
- Significant performance improvement for large client counts

#### P2: HTTP Client Pooling
- Created global `taskHTTPClient` with connection pooling
- Configured: 100 max idle connections, 10 per host, 90s idle timeout
- Eliminates connection setup overhead for each task execution

#### P2: Redis Connection Pooling
- Configured go-redis client with: PoolSize=100, MinIdleConns=10
- Added timeouts: DialTimeout=5s, ReadTimeout=3s, WriteTimeout=3s
- Improves performance under high load

#### P2: Client ID Validation
- Added `isValidClientID()` function with regex: `^[a-zA-Z0-9_-]{3,64}$`
- Prevents injection attacks and Redis key manipulation
- Validated in `validateClient()` and `handleAdminSetClient()`

#### P2: Configurable Concurrency
- Added `WORKER_CONCURRENCY` environment variable (default: 10)
- Allows tuning worker pool size based on deployment resources

#### P1: Single Point of Failure - Service Separation
- Added `MODE` environment variable: `api`, `worker`, or `both`
- API and Worker now run as independent containers in production
- Each service has its own health endpoint (`/health`)
- Workers can be scaled independently via `WORKER_REPLICAS`
- Panic recovery wrapper prevents worker crashes from affecting other workers
- `restart: unless-stopped` ensures automatic recovery

**New Architecture:**
```
┌─────────────────┐     ┌─────────────────┐
│   API Server    │     │   Worker (1-N)  │
│   (MODE=api)    │     │  (MODE=worker)  │
│   Port 8080     │     │   Port 8082     │
└────────┬────────┘     └────────┬────────┘
         │                       │
         └───────────┬───────────┘
                     │
              ┌──────▼──────┐
              │    Redis    │
              │   (Asynq)   │
              └─────────────┘
```

**Benefits:**
- Kill worker → API continues accepting requests
- Kill API → Workers continue processing queued tasks
- Scale workers independently (e.g., `WORKER_REPLICAS=5`)
- Memory/CPU isolation between components

### 6.2 Additional Production Hardening (Phase 2)

#### P0: SSRF Protection
- Added `isValidTaskURL()` function to validate task URLs before execution
- Blocked URL schemes: only `http://` and `https://` allowed (blocks `file://`, `ftp://`, etc.)
- Blocked hostnames: `localhost`, `127.0.0.1`, `::1`, `169.254.169.254` (AWS metadata), `metadata.google.internal`
- Added `isPrivateIP()` function to detect RFC 1918 private addresses
- DNS resolution performed to detect private IPs behind hostnames
- Configurable via `ALLOW_PRIVATE_URLS=true` for internal/development use

#### P1: AllowedQueues Enforcement
- Added `isValidQueue()` function with regex: `^[a-zA-Z0-9_-]{1,64}$`
- Queue names validated before task enqueue
- Prevents Redis key injection via malicious queue names
- Returns 400 Bad Request for invalid queue names

#### P1: Response Body Size Limit
- Worker now limits response body reads to 10MB using `io.LimitReader`
- Prevents memory exhaustion from large responses
- Log truncates body if over limit

#### P1: Retry-After Header
- Rate limit responses (429) now include `Retry-After` header
- Clients informed exactly how long to wait (in seconds)
- Calculated based on rate limit window remaining time

#### P1: Context Propagation
- HTTP requests in worker now use task context via `http.NewRequestWithContext()`
- Task cancellation properly propagates to HTTP client
- Enables proper cleanup on shutdown or task timeout

#### P2: Configurable Task Timeout
- Added `TASK_TIMEOUT_SEC` environment variable (default: 1800 = 30 minutes)
- Asynq server configured with `Timeout: time.Duration(TaskTimeout) * time.Second`
- Allows tuning based on expected task durations

#### P2: Configurable Max Retry
- Added `MAX_RETRY` environment variable (default: 5)
- Task options use `asynq.MaxRetry(MaxRetry)`
- Allows tuning retry behavior per deployment

#### P2: Enhanced Health Check
- `/health` endpoint now pings Redis to verify connectivity
- New `/health/details` endpoint provides component status:
  ```json
  {
    "status": "ok",
    "mode": "api",
    "redis": "connected",
    "timestamp": "2024-01-15T10:30:00Z",
    "request_id": "abc123"
  }
  ```
- Returns 503 Service Unavailable if Redis is down

#### P2: Request ID Tracing
- Added `requestID()` middleware generating unique IDs per request
- Request ID passed via `X-Request-ID` header and context
- Logged with each operation for distributed tracing
- Workers log request ID with task execution

#### P3: Prometheus Metrics
- Added `/metrics` endpoint exposing Prometheus metrics
- Metrics implemented:
  - `task_dispatcher_tasks_enqueued_total` (counter by queue, client_id, status)
  - `task_dispatcher_tasks_processed_total` (counter by queue, status)
  - `task_dispatcher_rate_limit_hits_total` (counter by client_id)
  - `task_dispatcher_task_duration_seconds` (histogram by queue, status)
  - `task_dispatcher_http_request_duration_seconds` (histogram by handler, method, status)
- Enables integration with Grafana dashboards

---

## 7. Recommended Architecture Evolution

### Phase 1: Security Hardening
1. Add admin API authentication
2. Remove or secure rate limit override
3. Add input validation
4. Configure Redis password

### Phase 2: Reliability Improvements
1. Implement graceful shutdown
2. Separate API and worker processes
3. Add health check for worker status
4. Implement dead letter handling

### Phase 3: Performance Optimization
1. Configure Redis connection pooling
2. Fix N+1 query with MGET
3. Reuse HTTP client with connection pooling
4. Make concurrency configurable

### Phase 4: Scalability
1. Add Prometheus metrics
2. Implement structured logging
3. Add distributed tracing
4. Consider PostgreSQL for client configs

---

## 8. Testing Coverage

### Current State
- Unit tests: `main_test.go` with comprehensive coverage
- Integration tests: `test_rate_limiting.sh` and manual docker-compose testing
- All tests passing

### Unit Tests Added (`main_test.go`)
| Test | Coverage |
|------|----------|
| `TestIsValidClientID` | Client ID format validation (15 cases) |
| `TestAdminAuth` | Admin API authentication middleware (6 cases) |
| `TestGetRateLimitForClient_OverrideValidation` | Rate limit override clamping (4 cases) |
| `TestRequestBodySizeLimit` | Request body size enforcement (4 cases) |
| `TestGetEnvInt` | Environment variable parsing (3 cases) |
| `TestHealthEndpoint` | Health check response |
| `TestAdminEndpointMethods` | HTTP method enforcement (5 cases) |
| `TestValidateClient_InvalidFormat` | Client validation (4 cases) |
| `TestIsValidTaskURL` | SSRF protection URL validation (11 cases) |
| `TestIsValidTaskURL_AllowPrivate` | Private URL allowlist flag |
| `TestIsValidQueue` | Queue name validation (8 cases) |
| `TestIsPrivateIP` | RFC 1918 private IP detection (8 cases) |

### Integration Tests Verified
1. Health endpoint (`/health`)
2. Admin API authentication (401 without key, 200 with key)
3. Client CRUD operations via admin API
4. Client ID format validation (400 for invalid)
5. Task enqueue with rate limiting
6. Rate limit override clamping (prevents bypass)
7. Request body size limit (413 for >1MB)
8. Auto-creation of clients on first request
9. Rate limiting enforcement (429 after limit)
10. Graceful shutdown (proper shutdown sequence)

### Recommendations for Future
1. Add load testing with k6 or vegeta
2. Implement chaos testing (network failures)
3. Set up CI/CD pipeline with test coverage reporting

---

## 9. Conclusion

Task Dispatcher has been comprehensively hardened and is now **production-ready** with all critical and high-priority issues resolved:

### Security (All P0/P1 Issues Resolved)
- **SSRF Protection**: URL validation blocks internal network access, metadata endpoints
- **Admin API Authentication** via `ADMIN_API_KEY` environment variable
- **Rate limit override bypass** fixed (values clamped to tier maximums)
- **Client ID validation** prevents injection attacks
- **Queue name validation** prevents Redis key injection
- **Request body size limits** (1MB API, 10MB worker response)
- **Redis authentication** support added

### Reliability Improvements
- **Graceful shutdown** with proper signal handling (30s timeout)
- **Service separation**: API and Worker run independently
- **Panic recovery** in workers prevents cascade failures
- **Context propagation** enables proper request cancellation
- **Retry-After headers** for rate limited clients
- **Configurable timeouts** and retry behavior

### Performance Optimizations
- **Redis connection pooling** (100 connections, 10 min idle)
- **HTTP client connection pooling** for task execution
- **N+1 query fix** using MGET for batch client retrieval
- **Configurable worker concurrency** via `WORKER_CONCURRENCY`

### Observability
- **Prometheus metrics** at `/metrics` endpoint
- **Request ID tracing** for distributed debugging
- **Enhanced health checks** with Redis connectivity verification
- **Detailed health endpoint** (`/health/details`) with component status

### Remaining Deferred Items
1. SCAN operation for client listing (consider secondary index)
2. Dead letter queue for failed tasks
3. Structured JSON logging

**Production Deployment Checklist:**
1. Set `ADMIN_API_KEY` to a strong secret (required)
2. Set `REDIS_PASSWORD` if Redis is network-accessible
3. Configure `WORKER_CONCURRENCY` based on expected load
4. Configure `TASK_TIMEOUT_SEC` based on task durations
5. Set up Prometheus scraping from `/metrics` endpoint
6. Set up alerting for `/health` endpoint failures
7. Use separate API and worker containers (`MODE=api` and `MODE=worker`)
8. Scale workers based on queue depth (`WORKER_REPLICAS=N`)
9. Keep `ALLOW_PRIVATE_URLS=false` unless internal URLs are required
10. Set up log aggregation with request ID correlation

**Environment Variables Reference:**
| Variable | Default | Description |
|----------|---------|-------------|
| `MODE` | `both` | Service mode: `api`, `worker`, or `both` |
| `PORT` | `8080` | HTTP server port |
| `REDIS_ADDR` | `localhost:6379` | Redis connection address |
| `REDIS_PASSWORD` | (empty) | Redis authentication password |
| `ADMIN_API_KEY` | (empty) | Admin API authentication key |
| `WORKER_CONCURRENCY` | `10` | Number of concurrent task workers |
| `WORKER_REPLICAS` | `1` | Number of worker container replicas (docker-compose) |
| `TASK_TIMEOUT_SEC` | `1800` | Task execution timeout in seconds (30 min default) |
| `MAX_RETRY` | `5` | Maximum retry attempts for failed tasks |
| `ALLOW_PRIVATE_URLS` | `false` | Allow tasks to target private/internal IPs (SSRF bypass) |
