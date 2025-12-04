# Build Stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install git for fetching dependencies and version info
RUN apk add --no-cache git

# Copy Go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY *.go ./

# Build arguments for version info
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Build the binary with version info embedded via ldflags
# CGO_ENABLED=0 ensures a static binary for scratch/alpine
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X 'main.BuildTime=${BUILD_TIME}'" \
    -o task-dispatcher main.go

# Runtime Stage
FROM alpine:latest

# Create non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/task-dispatcher .

# Set ownership and switch to non-root user
RUN chown -R appuser:appgroup /app
USER appuser

# Expose the API port
EXPOSE 8080

# Run the binary
CMD ["./task-dispatcher"]
