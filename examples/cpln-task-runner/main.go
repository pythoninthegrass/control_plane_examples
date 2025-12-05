package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// --- Prometheus Metrics ---
var (
	tasksEnqueued = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "task_dispatcher_tasks_enqueued_total",
			Help: "Total number of tasks enqueued",
		},
		[]string{"queue", "client_id"},
	)

	tasksProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "task_dispatcher_tasks_processed_total",
			Help: "Total number of tasks processed",
		},
		[]string{"status"}, // success, error, client_error
	)

	rateLimitHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "task_dispatcher_rate_limit_hits_total",
			Help: "Total number of rate limit hits",
		},
		[]string{"client_id"},
	)

	taskDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "task_dispatcher_task_duration_seconds",
			Help:    "Duration of task execution",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1s to ~100s
		},
		[]string{"status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "task_dispatcher_http_request_duration_seconds",
			Help:    "Duration of HTTP API requests",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "method", "status_code"},
	)

	circuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "task_dispatcher_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=half-open, 2=open)",
		},
		[]string{"host"},
	)

	// Queue metrics (updated on each scrape via collector)
	queueSize = prometheus.NewDesc(
		"task_dispatcher_queue_size",
		"Number of tasks in queue by state",
		[]string{"queue", "state"},
		nil,
	)
	queueLatency = prometheus.NewDesc(
		"task_dispatcher_queue_latency_seconds",
		"Time since oldest pending task was enqueued",
		[]string{"queue"},
		nil,
	)
)

func init() {
	prometheus.MustRegister(tasksEnqueued)
	prometheus.MustRegister(tasksProcessed)
	prometheus.MustRegister(rateLimitHits)
	prometheus.MustRegister(taskDuration)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(circuitBreakerState)
}

// QueueMetricsCollector collects queue metrics from Asynq on each Prometheus scrape
type QueueMetricsCollector struct{}

func (c *QueueMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- queueSize
	ch <- queueLatency
}

func (c *QueueMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	if asynqInspector == nil {
		return
	}

	// Get list of queues
	queues, err := asynqInspector.Queues()
	if err != nil {
		slog.Debug("Failed to get queues for metrics", "error", err)
		return
	}

	for _, queue := range queues {
		info, err := asynqInspector.GetQueueInfo(queue)
		if err != nil {
			slog.Debug("Failed to get queue info for metrics", "queue", queue, "error", err)
			continue
		}

		// Report queue sizes by state
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Pending), queue, "pending")
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Active), queue, "active")
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Scheduled), queue, "scheduled")
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Retry), queue, "retry")
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Archived), queue, "archived")
		ch <- prometheus.MustNewConstMetric(queueSize, prometheus.GaugeValue, float64(info.Completed), queue, "completed")

		// Report latency (time since oldest pending task)
		if info.Latency > 0 {
			ch <- prometheus.MustNewConstMetric(queueLatency, prometheus.GaugeValue, info.Latency.Seconds(), queue)
		}
	}
}

// --- Build Info (set via ldflags) ---
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// --- Configuration ---
var (
	RedisPassword      = getEnv("REDIS_PASSWORD", "")
	RedisMasterName    = getEnv("REDIS_MASTER_NAME", "mymaster")
	RedisSentinelAddr  = getEnv("REDIS_SENTINEL_ADDR", "localhost:26379")
	RedisSentinelPass  = getEnv("REDIS_SENTINEL_PASSWORD", "")
	Port             = getEnv("PORT", "8080")
	AdminAPIKey      = getEnv("ADMIN_API_KEY", "")
	Concurrency      = getEnvInt("WORKER_CONCURRENCY", 10)
	Mode             = getEnv("MODE", "both")              // "api", "worker", or "both"
	TaskTimeout      = getEnvInt("TASK_TIMEOUT_SEC", 1800) // 30 minutes default
	MaxRetry         = getEnvInt("MAX_RETRY", 5)
	AllowPrivateURLs = getEnv("ALLOW_PRIVATE_URLS", "false") == "true" // SSRF protection
	LogLevel         = getEnv("LOG_LEVEL", "info")                     // debug, info, warn, error
	OTLPEndpoint     = getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "")       // OpenTelemetry collector endpoint
	TracingEnabled   = getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "" // Enable tracing if endpoint set
	redisClient      *redis.Client                                     // For rate limiting

	// Global HTTP client with connection pooling for task execution
	// Note: otelhttp transport is added in initTaskHTTPClient() after tracing is initialized
	taskHTTPClient *http.Client

	// Worker health status (for health checks when running in worker mode)
	workerHealthy = true

	// Maximum response body size to read (10MB)
	maxResponseBodySize int64 = 10 * 1024 * 1024

	// Circuit breaker configuration
	cbMaxRequests   = uint32(getEnvInt("CB_MAX_REQUESTS", 3))    // Max requests in half-open state
	cbInterval      = time.Duration(getEnvInt("CB_INTERVAL_SEC", 60)) * time.Second  // Interval to clear counts
	cbTimeout       = time.Duration(getEnvInt("CB_TIMEOUT_SEC", 30)) * time.Second   // Time to wait before half-open
	cbFailureThreshold = getEnvInt("CB_FAILURE_THRESHOLD", 5)   // Failures before opening

	// Circuit breaker registry (per-host)
	circuitBreakers   = make(map[string]*gobreaker.CircuitBreaker[*http.Response])
	circuitBreakersMu sync.RWMutex

	// Asynq inspector for queue metrics
	asynqInspector *asynq.Inspector

	// OpenTelemetry tracer
	tracer trace.Tracer
)

// Default rate limits per tier
var DefaultRateLimits = map[string]*RateConfig{
	"free":       {MaxConcurrent: 1, RequestsPerMin: 10},
	"basic":      {MaxConcurrent: 5, RequestsPerMin: 100},
	"premium":    {MaxConcurrent: 20, RequestsPerMin: 1000},
	"enterprise": {MaxConcurrent: 50, RequestsPerMin: 5000},
	"default":    {MaxConcurrent: 3, RequestsPerMin: 50},
}

const (
	TypeHttpRequest = "http:request"
)

// --- Task Payload ---
// TaskPayload defines the structure of the job data stored in Redis.
// This mimics the Cloud Tasks API requirements.
type TaskPayload struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // Base64 or plain text string
}

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

// --- API Request ---
// EnqueueRequest defines what the client sends to our API.
type EnqueueRequest struct {
	Queue             string      `json:"queue"` // e.g., "default", "critical"
	DelaySec          int         `json:"delay"` // Delay in seconds (ScheduleTime)
	Task              TaskPayload `json:"task"`
	ClientID          string      `json:"client_id"`      // Required client identifier
	RateLimitOverride *RateConfig `json:"rate_limit"`     // Optional per-request override
}

// --- Client Config Storage Functions ---

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

	if len(keys) == 0 {
		return []ClientConfig{}, nil
	}

	// Batch fetch all configs using MGET to avoid N+1 queries
	values, err := redisClient.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to batch fetch client configs: %v", err)
	}

	clients := make([]ClientConfig, 0, len(keys))
	for _, val := range values {
		if val == nil {
			continue
		}

		data, ok := val.(string)
		if !ok {
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

// --- Rate Limiting Functions ---

// getRateLimitForClient determines the rate limit to apply
func getRateLimitForClient(clientID string, override *RateConfig) (*RateConfig, error) {
	// Look up client config from Redis to get tier limits
	config, err := getClientConfig(clientID)

	// Determine the maximum allowed limits based on client tier
	var maxAllowed *RateConfig
	if err == nil && config.Tier != "" {
		if tierLimit, ok := DefaultRateLimits[config.Tier]; ok {
			maxAllowed = tierLimit
		}
	}
	if maxAllowed == nil {
		maxAllowed = DefaultRateLimits["default"]
	}

	// 1. If explicit override provided, validate against tier maximum
	if override != nil && override.MaxConcurrent > 0 && override.RequestsPerMin > 0 {
		// Clamp override values to tier maximum
		validatedOverride := &RateConfig{
			MaxConcurrent:  min(override.MaxConcurrent, maxAllowed.MaxConcurrent),
			RequestsPerMin: min(override.RequestsPerMin, maxAllowed.RequestsPerMin),
		}
		return validatedOverride, nil
	}

	// 2. Use client-specific rate limit if configured
	if err == nil && config.RateLimit != nil {
		return config.RateLimit, nil
	}

	// 3. Fall back to tier default
	return maxAllowed, nil
}

// RateLimitResult contains the result of a rate limit check
type RateLimitResult struct {
	Allowed   bool
	Current   int64
	Limit     int
	Remaining int
	ResetAt   int64 // Unix timestamp when the window resets
}

// checkRateLimit validates if client is within rate limit using sliding window
// Returns the rate limit result with current usage information
func checkRateLimit(clientID string, requestsPerMin int) (*RateLimitResult, error) {
	ctx := context.Background()
	now := time.Now()
	windowStart := now.Unix() / 60
	key := fmt.Sprintf("ratelimit:%s:%d", clientID, windowStart)

	// Increment counter for this minute
	count, err := redisClient.Incr(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("rate limit check failed: %v", err)
	}

	// Set expiry on first request (2 minutes to handle edge cases)
	if count == 1 {
		if err := redisClient.Expire(ctx, key, 2*time.Minute).Err(); err != nil {
			slog.Warn("Failed to set expiry on rate limit key", "key", key, "error", err)
		}
	}

	// Calculate reset time (start of next minute window)
	resetAt := (windowStart + 1) * 60

	// Calculate remaining
	remaining := int(int64(requestsPerMin) - count)
	if remaining < 0 {
		remaining = 0
	}

	result := &RateLimitResult{
		Allowed:   count <= int64(requestsPerMin),
		Current:   count,
		Limit:     requestsPerMin,
		Remaining: remaining,
		ResetAt:   resetAt,
	}

	return result, nil
}

// validateClient checks if client exists and is enabled
func validateClient(clientID string) error {
	if clientID == "" {
		return fmt.Errorf("client_id is required")
	}

	if !isValidClientID(clientID) {
		return fmt.Errorf("invalid client_id format: must be 3-64 alphanumeric characters, hyphens, or underscores")
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
		slog.Info("Client auto-created", "client_id", clientID, "tier", "default")
		return nil
	}

	if !config.Enabled {
		return fmt.Errorf("client is disabled")
	}

	return nil
}

// validateConfig checks all configuration values at startup
func validateConfig() error {
	var errors []string

	// Validate worker concurrency
	if Concurrency <= 0 {
		errors = append(errors, fmt.Sprintf("WORKER_CONCURRENCY must be positive (got %d)", Concurrency))
	}

	// Validate task timeout
	if TaskTimeout <= 0 {
		errors = append(errors, fmt.Sprintf("TASK_TIMEOUT_SEC must be positive (got %d)", TaskTimeout))
	}

	// Validate max retry (0 is valid - means no retries)
	if MaxRetry < 0 {
		errors = append(errors, fmt.Sprintf("MAX_RETRY must be non-negative (got %d)", MaxRetry))
	}

	// Validate circuit breaker settings
	if cbFailureThreshold <= 0 {
		errors = append(errors, fmt.Sprintf("CB_FAILURE_THRESHOLD must be positive (got %d)", cbFailureThreshold))
	}

	if cbTimeout <= 0 {
		errors = append(errors, fmt.Sprintf("CB_TIMEOUT_SEC must be positive (got %d seconds)", int(cbTimeout.Seconds())))
	}

	if cbInterval <= 0 {
		errors = append(errors, fmt.Sprintf("CB_INTERVAL_SEC must be positive (got %d seconds)", int(cbInterval.Seconds())))
	}

	if cbMaxRequests <= 0 {
		errors = append(errors, fmt.Sprintf("CB_MAX_REQUESTS must be positive (got %d)", cbMaxRequests))
	}

	// Validate port
	port, err := strconv.Atoi(Port)
	if err != nil || port <= 0 || port > 65535 {
		errors = append(errors, fmt.Sprintf("PORT must be a valid port number 1-65535 (got %s)", Port))
	}

	// Validate mode
	if Mode != "api" && Mode != "worker" && Mode != "both" {
		errors = append(errors, fmt.Sprintf("MODE must be 'api', 'worker', or 'both' (got %s)", Mode))
	}

	// Validate log level
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[strings.ToLower(LogLevel)] {
		errors = append(errors, fmt.Sprintf("LOG_LEVEL must be debug, info, warn, or error (got %s)", LogLevel))
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration errors:\n  - %s", strings.Join(errors, "\n  - "))
	}

	return nil
}

// initLogger sets up structured JSON logging
func initLogger() {
	var level slog.Level
	switch strings.ToLower(LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

// initTaskHTTPClient creates the HTTP client for task execution with optional tracing
func initTaskHTTPClient() {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	var rt http.RoundTripper = transport
	if TracingEnabled {
		rt = otelhttp.NewTransport(transport)
	}

	taskHTTPClient = &http.Client{
		Timeout:   60 * time.Second,
		Transport: rt,
	}
}

// initTracer sets up OpenTelemetry tracing
func initTracer(ctx context.Context) (func(context.Context) error, error) {
	if !TracingEnabled {
		// Return a no-op tracer
		tracer = otel.Tracer("task-dispatcher")
		return func(context.Context) error { return nil }, nil
	}

	// Create OTLP HTTP exporter
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(OTLPEndpoint),
		otlptracehttp.WithInsecure(), // Use WithInsecure for non-TLS endpoints
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Create resource with service info
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("task-dispatcher"),
			semconv.ServiceVersion(Version),
			attribute.String("service.mode", Mode),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Set global trace provider and propagator
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Get tracer
	tracer = tp.Tracer("task-dispatcher")

	slog.Info("OpenTelemetry tracing initialized", "endpoint", OTLPEndpoint)

	return tp.Shutdown, nil
}

func main() {
	initLogger()

	// Validate all configuration at startup
	if err := validateConfig(); err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	// Initialize OpenTelemetry tracing
	ctx := context.Background()
	shutdownTracer, err := initTracer(ctx)
	if err != nil {
		slog.Error("Failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	defer shutdownTracer(ctx)

	// Initialize HTTP client for task execution (after tracing so it can use otelhttp)
	initTaskHTTPClient()

	slog.Info("Starting Task Dispatcher",
		"mode", Mode,
		"port", Port,
		"version", Version,
		"commit", Commit,
		"tracing", TracingEnabled,
	)

	// 1. Initialize Redis client for rate limiting with connection pooling (via Sentinel)
	redisClient = redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:       RedisMasterName,
		SentinelAddrs:    []string{RedisSentinelAddr},
		Password:         RedisPassword,
		SentinelPassword: RedisSentinelPass,
		DB:               0,
		PoolSize:         100,
		MinIdleConns:     10,
		DialTimeout:      5 * time.Second,
		ReadTimeout:      3 * time.Second,
		WriteTimeout:     3 * time.Second,
	})
	defer redisClient.Close()

	// Verify Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		slog.Error("Failed to connect to Redis via Sentinel", "master", RedisMasterName, "sentinel", RedisSentinelAddr, "error", err)
		os.Exit(1)
	}
	slog.Info("Connected to Redis via Sentinel", "master", RedisMasterName, "sentinel", RedisSentinelAddr)

	// 2. Setup Asynq Redis Connection (via Sentinel)
	asynqOpt := asynq.RedisFailoverClientOpt{
		MasterName:       RedisMasterName,
		SentinelAddrs:    []string{RedisSentinelAddr},
		Password:         RedisPassword,
		SentinelPassword: RedisSentinelPass,
	}

	// 3. Create Asynq Inspector for queue metrics
	asynqInspector = asynq.NewInspector(asynqOpt)
	defer asynqInspector.Close()

	// Register queue metrics collector
	prometheus.MustRegister(&QueueMetricsCollector{})

	// Variables for conditional initialization
	var client *asynq.Client
	var srv *asynq.Server
	var httpServer *http.Server

	// 4. Setup Asynq Client (needed for API mode)
	if Mode == "api" || Mode == "both" {
		client = asynq.NewClient(asynqOpt)
		defer client.Close()
	}

	// 5. Setup Asynq Server (needed for Worker mode)
	if Mode == "worker" || Mode == "both" {
		srv = asynq.NewServer(
			asynqOpt,
			asynq.Config{
				Concurrency: Concurrency,
				Queues: map[string]int{
					"critical": 6,
					"default":  3,
					"low":      1,
				},
			},
		)
	}

	// 5. Setup HTTP routes
	mux := http.NewServeMux()

	// Liveness probe - is the process alive?
	// Returns 200 as long as the HTTP server is responding
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Readiness probe - is the service ready to accept traffic?
	// Checks Redis connectivity and worker health
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		// Check worker health (if in worker mode)
		if (Mode == "worker" || Mode == "both") && !workerHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Worker unhealthy"))
			return
		}

		// Check Redis connectivity
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := redisClient.Ping(ctx).Err(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Redis unhealthy"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Legacy health endpoint (backwards compatible, same as readiness)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// Check worker health
		if (Mode == "worker" || Mode == "both") && !workerHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Worker unhealthy"))
			return
		}

		// Check Redis connectivity
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := redisClient.Ping(ctx).Err(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Redis unhealthy"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// Detailed health endpoint for monitoring
	mux.HandleFunc("/health/detailed", func(w http.ResponseWriter, r *http.Request) {
		status := map[string]interface{}{
			"status":    "healthy",
			"mode":      Mode,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"version":   Version,
			"commit":    Commit,
			"buildTime": BuildTime,
		}

		// Check Redis
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		redisHealthy := redisClient.Ping(ctx).Err() == nil
		status["redis"] = redisHealthy

		// Check worker (if applicable)
		if Mode == "worker" || Mode == "both" {
			status["worker"] = workerHealthy
		}

		// Set overall status
		if !redisHealthy || (Mode == "worker" && !workerHealthy) {
			status["status"] = "unhealthy"
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	// Version endpoint
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"version":   Version,
			"commit":    Commit,
			"buildTime": BuildTime,
		})
	})

	// API endpoints (only in api or both mode)
	if Mode == "api" || Mode == "both" {
		mux.HandleFunc("/v1/enqueue", func(w http.ResponseWriter, r *http.Request) {
			handleEnqueue(w, r, client)
		})

		// Admin API endpoints (protected by admin authentication)
		mux.HandleFunc("/admin/clients", adminAuth(handleAdminListClients))
		mux.HandleFunc("/admin/clients/set", adminAuth(handleAdminSetClient))
		mux.HandleFunc("/admin/clients/get", adminAuth(handleAdminGetClient))
		mux.HandleFunc("/admin/clients/delete", adminAuth(handleAdminDeleteClient))
		mux.HandleFunc("/admin/tiers", adminAuth(handleAdminGetTiers))
	}

	// Create HTTP server with timeouts and tracing
	var handler http.Handler = mux
	if TracingEnabled {
		handler = otelhttp.NewHandler(mux, "task-dispatcher",
			otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
				return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
			}),
		)
	}

	httpServer = &http.Server{
		Addr:         ":" + Port,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 6. Start Worker (in worker or both mode)
	if Mode == "worker" || Mode == "both" {
		workerMux := asynq.NewServeMux()
		workerMux.HandleFunc(TypeHttpRequest, handleHTTPRequestWithRecovery)

		go func() {
			slog.Info("Worker started", "concurrency", Concurrency)
			if err := srv.Run(workerMux); err != nil {
				slog.Error("Worker stopped", "error", err)
				workerHealthy = false
			}
		}()
	}

	// 7. Start HTTP server in a goroutine
	go func() {
		slog.Info("HTTP Server listening", "port", Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "port", Port, "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down gracefully")

	// Create a deadline for shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown Asynq server first (stops processing new tasks)
	if srv != nil {
		srv.Shutdown()
		slog.Info("Worker stopped")
	}

	// Shutdown HTTP server (waits for in-flight requests)
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	slog.Info("HTTP server stopped")

	slog.Info("Shutdown complete")
}

// handleHTTPRequestWithRecovery wraps handleHTTPRequest with panic recovery
func handleHTTPRequestWithRecovery(ctx context.Context, t *asynq.Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Task panicked", "task_type", t.Type(), "panic", r)
			err = fmt.Errorf("task panicked: %v", r)
		}
	}()
	return handleHTTPRequest(ctx, t)
}

// handleEnqueue receives a request from your app and puts it into Redis with rate limiting
func handleEnqueue(w http.ResponseWriter, r *http.Request, client *asynq.Client) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body size to 1MB to prevent DoS attacks
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req EnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, "Request body too large (max 1MB)", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}

	// Validate client
	if err := validateClient(req.ClientID); err != nil {
		http.Error(w, fmt.Sprintf("Client validation failed: %v", err), http.StatusUnauthorized)
		return
	}

	// Validate task URL (SSRF protection)
	if err := isValidTaskURL(req.Task.URL); err != nil {
		http.Error(w, fmt.Sprintf("Invalid task URL: %v", err), http.StatusBadRequest)
		return
	}

	// Validate queue name
	if req.Queue == "" {
		req.Queue = "default"
	}
	if !isValidQueue(req.Queue) {
		http.Error(w, "Invalid queue name: must be 1-64 alphanumeric characters, hyphens, or underscores", http.StatusBadRequest)
		return
	}

	// Check if client is allowed to use this queue
	if allowed, err := isQueueAllowed(req.ClientID, req.Queue); !allowed {
		http.Error(w, fmt.Sprintf("Queue access denied: %v", err), http.StatusForbidden)
		return
	}

	// Validate HTTP method
	validMethods := map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}
	if req.Task.Method == "" {
		req.Task.Method = "GET"
	}
	if !validMethods[strings.ToUpper(req.Task.Method)] {
		http.Error(w, "Invalid HTTP method", http.StatusBadRequest)
		return
	}
	req.Task.Method = strings.ToUpper(req.Task.Method)

	// Determine rate limit (override or configured)
	rateLimit, err := getRateLimitForClient(req.ClientID, req.RateLimitOverride)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get rate limit: %v", err), http.StatusInternalServerError)
		return
	}

	// Check time-window rate limit (requests per minute)
	rateLimitResult, err := checkRateLimit(req.ClientID, rateLimit.RequestsPerMin)
	if err != nil {
		http.Error(w, fmt.Sprintf("Rate limit check failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Set rate limit headers (always, for visibility)
	secondsUntilReset := rateLimitResult.ResetAt - time.Now().Unix()
	if secondsUntilReset < 0 {
		secondsUntilReset = 0
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rateLimitResult.Limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rateLimitResult.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(rateLimitResult.ResetAt, 10))

	if !rateLimitResult.Allowed {
		slog.Warn("Rate limit exceeded", "client_id", req.ClientID, "requests_per_min", rateLimit.RequestsPerMin, "current", rateLimitResult.Current)
		rateLimitHits.WithLabelValues(req.ClientID).Inc()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.FormatInt(secondsUntilReset, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":            "Rate limit exceeded",
			"message":          fmt.Sprintf("exceeded %d requests per minute (current: %d)", rateLimitResult.Limit, rateLimitResult.Current),
			"client_id":        req.ClientID,
			"requests_per_min": rateLimitResult.Limit,
			"retry_after":      secondsUntilReset,
		})
		return
	}

	// Serialize the payload
	payload, err := json.Marshal(req.Task)
	if err != nil {
		http.Error(w, "Failed to marshal task", http.StatusInternalServerError)
		return
	}

	// Define Task Options
	opts := []asynq.Option{
		asynq.Queue(req.Queue),
		asynq.MaxRetry(MaxRetry),
		asynq.Timeout(time.Duration(TaskTimeout) * time.Second),
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

	// Record metric
	tasksEnqueued.WithLabelValues(req.Queue, req.ClientID).Inc()

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

// --- Admin API Handlers ---

// handleAdminSetClient creates or updates client configuration
func handleAdminSetClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

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

	// Validate client ID format
	if !isValidClientID(config.ClientID) {
		http.Error(w, "invalid client_id format: must be 3-64 alphanumeric characters, hyphens, or underscores", http.StatusBadRequest)
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

	slog.Info("Admin set client", "action", "set_client", "client_id", config.ClientID, "tier", config.Tier)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(config)
}

// handleAdminGetClient retrieves client configuration
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

// handleAdminListClients lists all clients
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

// handleAdminDeleteClient deletes a client
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

	slog.Info("Admin delete client", "action", "delete_client", "client_id", clientID)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminGetTiers returns available tiers
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

// getCircuitBreaker returns or creates a circuit breaker for the given host
func getCircuitBreaker(host string) *gobreaker.CircuitBreaker[*http.Response] {
	circuitBreakersMu.RLock()
	cb, exists := circuitBreakers[host]
	circuitBreakersMu.RUnlock()

	if exists {
		return cb
	}

	// Create new circuit breaker for this host
	circuitBreakersMu.Lock()
	defer circuitBreakersMu.Unlock()

	// Double-check after acquiring write lock
	if cb, exists = circuitBreakers[host]; exists {
		return cb
	}

	settings := gobreaker.Settings{
		Name:        fmt.Sprintf("cb-%s", host),
		MaxRequests: cbMaxRequests,
		Interval:    cbInterval,
		Timeout:     cbTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Open circuit after consecutive failures
			return counts.ConsecutiveFailures >= uint32(cbFailureThreshold)
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			slog.Warn("Circuit breaker state changed",
				"name", name,
				"host", host,
				"from", from.String(),
				"to", to.String(),
			)
			// Update Prometheus metric
			var stateValue float64
			switch to {
			case gobreaker.StateClosed:
				stateValue = 0
			case gobreaker.StateHalfOpen:
				stateValue = 1
			case gobreaker.StateOpen:
				stateValue = 2
			}
			circuitBreakerState.WithLabelValues(host).Set(stateValue)
		},
		IsSuccessful: func(err error) bool {
			return err == nil
		},
	}

	cb = gobreaker.NewCircuitBreaker[*http.Response](settings)
	circuitBreakers[host] = cb
	return cb
}

// handleHTTPRequest is the "Worker" logic. It mimics Cloud Tasks by making the HTTP call.
func handleHTTPRequest(ctx context.Context, t *asynq.Task) error {
	startTime := time.Now()

	var p TaskPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		tasksProcessed.WithLabelValues("error").Inc()
		return fmt.Errorf("json.Unmarshal failed: %v: %w", err, asynq.SkipRetry)
	}

	taskID := t.ResultWriter().TaskID()

	// Start a span for task execution
	ctx, span := tracer.Start(ctx, "task.execute",
		trace.WithAttributes(
			attribute.String("task.id", taskID),
			attribute.String("task.type", t.Type()),
			attribute.String("http.method", p.Method),
			attribute.String("http.url", p.URL),
		),
	)
	defer span.End()

	slog.Debug("Executing task", "task_id", taskID, "method", p.Method, "url", p.URL, "trace_id", span.SpanContext().TraceID().String())

	// Parse URL to get host for circuit breaker
	parsedURL, err := url.Parse(p.URL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to parse URL")
		tasksProcessed.WithLabelValues("error").Inc()
		taskDuration.WithLabelValues("error").Observe(time.Since(startTime).Seconds())
		return fmt.Errorf("failed to parse URL: %v: %w", err, asynq.SkipRetry)
	}
	span.SetAttributes(attribute.String("http.host", parsedURL.Host))
	host := parsedURL.Host

	// Get circuit breaker for this host
	cb := getCircuitBreaker(host)

	// Create request with context for proper cancellation
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, bytes.NewBufferString(p.Body))
	if err != nil {
		tasksProcessed.WithLabelValues("error").Inc()
		taskDuration.WithLabelValues("error").Observe(time.Since(startTime).Seconds())
		return fmt.Errorf("failed to create request: %v", err) // Will retry
	}

	// Add headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CloudTasks-TaskName", taskID)
	req.Header.Set("X-Request-ID", taskID)
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}

	// Execute using the circuit breaker
	resp, err := cb.Execute(func() (*http.Response, error) {
		resp, err := taskHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		// Treat 5xx as circuit breaker failures
		if resp.StatusCode >= 500 {
			// Read and close body to reuse connection, but return error for circuit breaker
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
		}
		return resp, nil
	})

	// Handle circuit breaker errors
	if err != nil {
		tasksProcessed.WithLabelValues("error").Inc()
		taskDuration.WithLabelValues("error").Observe(time.Since(startTime).Seconds())

		// Check if circuit is open
		if err == gobreaker.ErrOpenState {
			slog.Warn("Circuit breaker open, skipping request", "task_id", taskID, "host", host)
			return fmt.Errorf("circuit breaker open for host %s: %w", host, err) // Will retry later
		}
		if err == gobreaker.ErrTooManyRequests {
			slog.Warn("Circuit breaker half-open, too many requests", "task_id", taskID, "host", host)
			return fmt.Errorf("circuit breaker limiting requests for host %s: %w", host, err)
		}

		// Check if context was cancelled
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled: %v", ctx.Err())
		}

		// Server error (5xx) - will retry
		if strings.Contains(err.Error(), "server error") {
			slog.Warn("Task server error (will retry)", "task_id", taskID, "host", host, "error", err)
			return err
		}

		return fmt.Errorf("http call failed: %v", err) // Will retry
	}
	defer resp.Body.Close()

	// Check Status Code
	// Cloud Tasks considers 2xx a success. Anything else is a failure/retry.
	// Note: 5xx errors are handled by circuit breaker above
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		span.SetStatus(codes.Ok, "success")
		slog.Info("Task completed successfully", "task_id", taskID, "url", p.URL, "status_code", resp.StatusCode, "duration_ms", time.Since(startTime).Milliseconds())
		tasksProcessed.WithLabelValues("success").Inc()
		taskDuration.WithLabelValues("success").Observe(time.Since(startTime).Seconds())
		return nil
	}

	// Read error response body with size limit to prevent memory issues
	limitedReader := io.LimitReader(resp.Body, maxResponseBodySize)
	body, _ := io.ReadAll(limitedReader)

	// For 429 (rate limited), retry
	if resp.StatusCode == 429 {
		span.SetStatus(codes.Error, "rate limited")
		slog.Warn("Task rate limited (will retry)", "task_id", taskID, "url", p.URL, "status_code", resp.StatusCode, "duration_ms", time.Since(startTime).Milliseconds())
		tasksProcessed.WithLabelValues("error").Inc()
		taskDuration.WithLabelValues("error").Observe(time.Since(startTime).Seconds())
		return fmt.Errorf("rate limited by endpoint: %d", resp.StatusCode)
	}

	// For other 4xx client errors, don't retry
	span.SetStatus(codes.Error, "client error")
	slog.Warn("Task client error (no retry)", "task_id", taskID, "url", p.URL, "status_code", resp.StatusCode, "duration_ms", time.Since(startTime).Milliseconds())
	tasksProcessed.WithLabelValues("client_error").Inc()
	taskDuration.WithLabelValues("client_error").Observe(time.Since(startTime).Seconds())
	return fmt.Errorf("client error %d: %s: %w", resp.StatusCode, string(body), asynq.SkipRetry)
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return fallback
}

// adminAuth is a middleware that validates the admin API key
func adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if AdminAPIKey == "" {
			slog.Warn("Admin API key not configured - admin endpoints are unprotected")
			next(w, r)
			return
		}

		key := r.Header.Get("X-Admin-Key")
		if key == "" {
			key = r.URL.Query().Get("admin_key")
		}

		if key != AdminAPIKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// isValidClientID validates client ID format to prevent injection attacks
var validClientIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,64}$`)

func isValidClientID(id string) bool {
	return validClientIDRegex.MatchString(id)
}

// isValidTaskURL validates a URL for SSRF protection
func isValidTaskURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("URL is required")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}

	// Only allow http and https schemes
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid URL scheme: must be http or https")
	}

	// Require a host
	if parsed.Host == "" {
		return fmt.Errorf("URL must have a host")
	}

	// If private URLs are allowed, skip the rest of validation
	if AllowPrivateURLs {
		return nil
	}

	// Extract hostname (without port)
	hostname := parsed.Hostname()

	// Block localhost variations
	lowerHost := strings.ToLower(hostname)
	if lowerHost == "localhost" || lowerHost == "127.0.0.1" || lowerHost == "::1" {
		return fmt.Errorf("localhost URLs are not allowed")
	}

	// Block metadata endpoints (cloud provider metadata services)
	if lowerHost == "169.254.169.254" || lowerHost == "metadata.google.internal" {
		return fmt.Errorf("metadata service URLs are not allowed")
	}

	// Resolve hostname to check for private IPs
	ips, err := net.LookupIP(hostname)
	if err != nil {
		// If DNS resolution fails, allow the request (it will fail at HTTP level)
		return nil
	}

	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("private/internal IP addresses are not allowed")
		}
	}

	return nil
}

// isPrivateIP checks if an IP is in a private/reserved range
func isPrivateIP(ip net.IP) bool {
	// Check for loopback
	if ip.IsLoopback() {
		return true
	}

	// Check for link-local
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Check for private ranges
	privateRanges := []string{
		"10.0.0.0/8",     // RFC 1918
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918
		"169.254.0.0/16", // Link-local
		"127.0.0.0/8",    // Loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}

	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

// isValidQueue checks if a queue name is valid
func isValidQueue(queue string) bool {
	if queue == "" {
		return false
	}
	// Allow alphanumeric, hyphens, underscores, max 64 chars
	matched, _ := regexp.MatchString(`^[a-zA-Z0-9_-]{1,64}$`, queue)
	return matched
}

// isQueueAllowed checks if client is allowed to use the specified queue
func isQueueAllowed(clientID, queue string) (bool, error) {
	config, err := getClientConfig(clientID)
	if err != nil {
		return true, nil // If client not found, allow (will be created)
	}

	// If no queue restrictions, allow all
	if len(config.AllowedQueues) == 0 {
		return true, nil
	}

	// Check if queue is in allowed list
	for _, allowed := range config.AllowedQueues {
		if allowed == queue {
			return true, nil
		}
	}

	return false, fmt.Errorf("queue '%s' not in allowed queues for client", queue)
}
