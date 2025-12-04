# Task Dispatcher

A **self-hosted task queue and scheduler service** that mimics **Google Cloud Tasks** functionality, built with Go and Redis. This is designed as a drop-in replacement for Cloud Tasks when you want more control, flexibility, or cost savings.

## Overview

Task Dispatcher accepts HTTP requests to enqueue background jobs, stores them in Redis, and processes them asynchronously by making HTTP calls to target endpoints. It supports delayed execution, priority queues, automatic retries, and concurrency control.

## Features

- ✅ **REST API** for task submission (`/v1/enqueue`)
- ✅ **Delayed/Scheduled Tasks** - execute tasks at a future time
- ✅ **Priority Queues** - critical, default, and low priority levels
- ✅ **Automatic Retries** - up to 5 retries with exponential backoff
- ✅ **Task Timeouts** - 30-minute default timeout per task
- ✅ **Web Dashboard** - Asynqmon UI for monitoring (port 8081)
- ✅ **Redis Persistence** - AOF enabled for durability
- ✅ **Docker & Docker Compose** support
- ✅ **Health Check** endpoint

## Technology Stack

- **Go 1.22** with [Asynq](https://github.com/hibiken/asynq) library
- **Redis 7** for task storage and queue management
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

# Install dependencies
go mod download

# Run Redis locally
docker run -d -p 6379:6379 redis:7-alpine

# Run the application
go run main.go## API Usage

### Enqueue a Task

**Endpoint**: `POST /v1/enqueue`

**Request Body**:
{
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
}**Response**:
{
  "status": "enqueued",
  "task_id": "abc123-xyz789",
  "queue": "default"
}### Parameters

| Field | Type | Description |
|-------|------|-------------|
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
| `REDIS_ADDR` | `localhost:6379` | Redis server address |
| `PORT` | `8080` | API server port |

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

### ControlPlane (cpln)

# Build and push images
./cpln-build.sh### Custom Deployment

The application can be deployed to any platform that supports Docker:

1. Build the image: `docker build -t task-dispatcher .`
2. Deploy Redis with persistence enabled
3. Set `REDIS_ADDR` environment variable
4. Expose port 8080

## Architecture
