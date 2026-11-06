#!/usr/bin/env bash

set -e

# test fuzz inputs
go test -tags gofuzz -run TestFuzz -v .

# test with -race
go test -race ./...
