# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.23-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /app

# Copy go.mod and go.sum for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the binary (static linking)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /llmapibenchmark ./cmd

# ---- Runtime stage ----
FROM alpine:3.20

# Add CA certificates for HTTPS
RUN apk add --no-cache ca-certificates

# Copy the compiled binary from builder
COPY --from=builder /llmapibenchmark /usr/local/bin/llmapibenchmark

# Set entrypoint to the benchmark executable
ENTRYPOINT ["llmapibenchmark"]
