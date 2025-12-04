# Task Runner Template

A self-hosted task queue and scheduler service similar to Google Cloud Tasks, deployed on Control Plane.

## Overview

Task Runner provides HTTP-based task enqueuing with automatic retry, delayed/scheduled execution, per-client rate limiting, and multi-queue support with priority levels.

## Architecture

This template deploys two workloads:

- **API** (`task-runner-api`): HTTP endpoint for enqueuing tasks, managing clients, and health checks
- **Worker** (`task-runner-worker`): Background processor that executes tasks from the queue

Both workloads connect to an external Redis instance for task persistence and coordination.

## Prerequisites

- A Redis instance accessible from Control Plane (deploy separately or use a managed Redis service)
- Container image built and pushed to a registry accessible by Control Plane

## Configuration

### Required Values

| Parameter | Description |
|-----------|-------------|
| `global.image.repository` | Container registry path for the task-runner image |
| `redis.address` | Redis connection address (e.g., `redis:6379`) |

### Optional Values

#### API Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `api.enabled` | `true` | Enable/disable API workload |
| `api.replicas.min` | `1` | Minimum replicas |
| `api.replicas.max` | `3` | Maximum replicas |
| `api.public.enabled` | `true` | Enable public internet access |
| `api.resources.cpu` | `500m` | CPU allocation |
| `api.resources.memory` | `512Mi` | Memory allocation |
| `api.env.logLevel` | `info` | Log level (debug/info/warn/error) |
| `api.env.adminApiKey` | `""` | Admin API key for protected endpoints |
| `api.env.otelEndpoint` | `""` | OpenTelemetry collector endpoint |

#### Worker Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `worker.enabled` | `true` | Enable/disable Worker workload |
| `worker.replicas.min` | `1` | Minimum replicas |
| `worker.replicas.max` | `5` | Maximum replicas |
| `worker.resources.cpu` | `1000m` | CPU allocation |
| `worker.resources.memory` | `1Gi` | Memory allocation |
| `worker.env.logLevel` | `info` | Log level |
| `worker.env.concurrency` | `10` | Concurrent workers per replica |
| `worker.env.taskTimeoutSec` | `1800` | Task timeout (30 min default) |
| `worker.env.maxRetry` | `5` | Maximum retry attempts |
| `worker.env.allowPrivateUrls` | `false` | Allow targeting private URLs |
| `worker.env.cbFailureThreshold` | `5` | Circuit breaker failure threshold |
| `worker.env.cbTimeoutSec` | `30` | Circuit breaker timeout |

#### Redis Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `redis.address` | `redis:6379` | Redis host:port |
| `redis.password` | `""` | Redis password (if authenticated) |

## Usage

### Enqueue a Task

```bash
curl -X POST https://your-api-endpoint/v1/enqueue \
  -H "Content-Type: application/json" \
  -d '{
    "client_id": "my-service",
    "queue": "default",
    "task": {
      "url": "https://api.example.com/webhook",
      "method": "POST",
      "headers": {"Content-Type": "application/json"},
      "body": "{\"event\": \"user.created\"}"
    }
  }'
```

### Delayed Task

```bash
curl -X POST https://your-api-endpoint/v1/enqueue \
  -H "Content-Type: application/json" \
  -d '{
    "client_id": "my-service",
    "delay": 60,
    "task": {
      "url": "https://api.example.com/reminder",
      "method": "POST"
    }
  }'
```

### Admin Endpoints

When `ADMIN_API_KEY` is configured, admin endpoints require the `X-Admin-Key` header:

```bash
# List clients
curl https://your-api-endpoint/admin/clients \
  -H "X-Admin-Key: your-admin-key"

# Create/update client
curl -X POST https://your-api-endpoint/admin/clients/set \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{
    "client_id": "new-service",
    "tier": "premium",
    "enabled": true
  }'
```

## Rate Limiting Tiers

| Tier | Requests/min | Max Concurrent |
|------|-------------|----------------|
| free | 10 | 1 |
| basic | 100 | 5 |
| premium | 1,000 | 20 |
| enterprise | 5,000 | 50 |

## Health Endpoints

| Endpoint | Purpose |
|----------|---------|
| `/health` | Basic health check |
| `/health/live` | Kubernetes liveness probe |
| `/health/ready` | Kubernetes readiness probe |
| `/health/detailed` | Detailed health with component status |
| `/metrics` | Prometheus metrics |
| `/version` | Build version information |

## Monitoring

The service exposes Prometheus metrics at `/metrics` including:

- Task processing counts and durations
- Queue depths by priority
- Rate limit hits
- Circuit breaker state changes
- HTTP request metrics

Configure OpenTelemetry tracing by setting `otelEndpoint` for distributed tracing support.
