#!/usr/bin/env bash

set -e

echo "test"
go test --timeout 5m ./...

echo "test purego"
go test --timeout 5m -tags purego ./...

echo "test -race"
go test --timeout 5m -race ./...
