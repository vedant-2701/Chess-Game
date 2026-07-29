#!/usr/bin/env bash

echo "Running build: go build ./..."
go build ./...
echo "Build successful"

echo "=================================================================================================="

echo "Running vet: go vet ./..."
go vet ./...
echo "Vet successful"

echo "=================================================================================================="

echo "Running tests: go test ./..."
go test ./...
echo "Tests successful"

echo "=================================================================================================="

echo "Running tests with race detector: go test -race ./..."
go test -race ./...
echo "Tests with race detector successful"

echo "=================================================================================================="  

echo "Running integration tests with race detector: go test -race -tags integration ./... -p 1"
go test -race -tags integration ./... -p 1
echo "Integration tests with race detector successful"

echo "=================================================================================================="  

echo "Running integration tests without race detector: go test -tags integration ./... -p 1"
go test -tags integration ./... -p 1
echo "Integration tests without race detector successful"