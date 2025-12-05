# Task Dispatcher

A **self-hosted task queue and scheduler service** that mimics **Google Cloud Tasks** functionality, built with Go and Redis Sentinel. This is designed as a drop-in replacement for Cloud Tasks when you want more control, flexibility, or cost savings.

## Overview

Task Dispatcher accepts HTTP requests to enqueue background jobs, stores them in Redis (via Sentinel for high availability), and processes them asynchronously by making HTTP calls to target endpoints. It supports delayed execution, priority queues, automatic retries, and rate limiting per client.

## Features

- ✅ **REST API** for task submission (`/v1/enqueue`)
- ✅ **Delayed/Scheduled Tasks** - execute tasks at a future time
- ✅ **Priority Queues** - critical, default, and low priority levels
- ✅ **Automatic Retries** - up to 5 retries with exponential backoff
- ✅ **Task Timeouts** - 30-minute default timeout per task
- ✅ **Redis Sentinel** - high availability with automatic master failover
- ✅ **Per-Client Rate Limiting** - sliding window rate limits per client
- ✅ **Circuit Breaker** - per-host circuit breaker for downstream protection
- ✅ **Web Dashboard** - Asynqmon UI for monitoring (port 8081)
- ✅ **Docker & Docker Compose** support
- ✅ **Health Check** endpoints (liveness, readiness, detailed)
- ✅ **Prometheus Metrics** - task and queue metrics at `/metrics`
- ✅ **OpenTelemetry Tracing** - distributed tracing support

## Technology Stack

- **Go 1.22** with [Asynq](https://github.com/hibiken/asynq) library
- **Redis Sentinel** for high-availability task storage and queue management
- **Docker** with multi-stage builds
- **Asynqmon** for task monitoring dashboard

## Quick Start

### Using Docker Compose (Recommended)

# Start all services (API, Worker, Redis, Dashboard)
docker-compose up -d

# View logs
docker-compose logs -f dispatcherServices:
- **API**: http://localhost:8080
- **Dashboard**: http://localhost:8081
- **Redis**: localhost:6379

### Local Development

```bash
# Install dependencies
go mod download

# Run Redis Sentinel locally (see redis/sentinel example in this repo)
# Or use docker-compose which includes Redis Sentinel

# Run the application
REDIS_SENTINEL_ADDR=localhost:26379 REDIS_MASTER_NAME=mymaster go run main.go
```

## API Usage

### Enqueue a Task

**Endpoint**: `POST /v1/enqueue`

**Request Body**:
```json
{
  "client_id": "my-service",
  "queue": "default",
  "delay": 0,
  "task": {
    "url": "https://your-service.com/webhook",
    "method": "POST",
    "headers": {
      "Authorization": "Bearer your-token",
      "Custom-Header": "value"
    },
    "body": "{\"message\": \"Hello from Task Dispatcher\"}"
  }
}
```

**Response**:
```json
{
  "status": "enqueued",
  "task_id": "abc123-xyz789",
  "queue": "default",
  "client_id": "my-service"
}
```### Parameters

| Field | Type | Description |
|-------|------|-------------|
| `client_id` | string | **Required.** Client identifier for rate limiting (3-64 alphanumeric, hyphens, underscores) |
| `queue` | string | Queue name: `critical`, `default`, or `low` |
| `delay` | int | Delay in seconds before execution (0 = immediate) |
| `task.url` | string | Target URL to call when task executes |
| `task.method` | string | HTTP method (GET, POST, etc.) |
| `task.headers` | object | Custom headers to include |
| `task.body` | string | Request body (typically JSON string) |

### Health Check

**Endpoint**: `GET /health`

**Response**: `200 OK`

## Priority Queues

The service supports three priority levels with weighted processing:

- **critical** (weight: 6) - Highest priority, processed most frequently
- **default** (weight: 3) - Standard priority
- **low** (weight: 1) - Lowest priority, processed least frequently

Workers will pull tasks from higher priority queues more often.

## Configuration

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_SENTINEL_ADDR` | `localhost:26379` | Redis Sentinel address (use global internal endpoint) |
| `REDIS_MASTER_NAME` | `mymaster` | Redis Sentinel master name |
| `REDIS_PASSWORD` | `` | Redis authentication password |
| `REDIS_SENTINEL_PASSWORD` | `` | Sentinel authentication password |
| `PORT` | `8080` | API server port |
| `MODE` | `both` | Run mode: `api`, `worker`, or `both` |
| `WORKER_CONCURRENCY` | `10` | Number of concurrent workers |
| `TASK_TIMEOUT_SEC` | `1800` | Task timeout in seconds (30 min default) |
| `MAX_RETRY` | `5` | Maximum retry attempts per task |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `ADMIN_API_KEY` | `` | API key for admin endpoints |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `` | OpenTelemetry collector endpoint (enables tracing) |

### Circuit Breaker Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CB_MAX_REQUESTS` | `3` | Max requests in half-open state |
| `CB_INTERVAL_SEC` | `60` | Interval to clear failure counts |
| `CB_TIMEOUT_SEC` | `30` | Time before circuit half-opens |
| `CB_FAILURE_THRESHOLD` | `5` | Consecutive failures to open circuit |

## Task Processing Behavior

- **Success**: HTTP status 2xx (200-299) marks task as complete
- **Retry**: HTTP status 4xx/5xx triggers automatic retry (up to 5 times)
- **Timeout**: Tasks timeout after 30 minutes
- **Concurrency**: 10 concurrent workers by default

Tasks are retried with exponential backoff automatically by Asynq.

## Monitoring

Access the Asynqmon dashboard at http://localhost:8081 to:

- View queued, active, completed, and failed tasks
- Inspect task details and payloads
- Monitor queue statistics
- Retry or delete failed tasks manually

## Deployment

### Control Plane (cpln)

```bash
# Build and push images
./cpln-build.sh
```

A Helm chart is provided in the `helm/` directory for deploying to Control Plane:

```bash
# Deploy using helm template
helm template task-runner ./helm -f ./helm/values.yaml | cpln apply -f -
```

### Custom Deployment

The application can be deployed to any platform that supports Docker:

1. Build the image: `docker build -t task-dispatcher .`
2. Deploy Redis Sentinel (see `redis/sentinel` example in this repo)
3. Set `REDIS_SENTINEL_ADDR` and `REDIS_MASTER_NAME` environment variables
4. Expose port 8080

## Redis Sentinel

This application **requires Redis Sentinel** for high availability. It does not support standalone Redis.

Redis Sentinel provides:
- **Automatic master discovery** - workers connect via Sentinel to find the current master
- **Automatic failover** - if the master fails, Sentinel promotes a replica and workers reconnect automatically
- **High availability** - no single point of failure for Redis

When deploying on Control Plane, use the global internal endpoint for Sentinel (e.g., `redis-sentinel.mygvc.cpln.local:26379`) which load balances across all Sentinel replicas.

## Architecture
