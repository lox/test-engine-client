#!/usr/bin/env bash
set -euo pipefail

# Install pact-go
go install github.com/pact-foundation/pact-go/v2@latest
pact-go -l DEBUG install

# Install dependencies for js runner tests
cd internal/runner/testdata
yarn install
cd playwright
yarn playwright install
cd ../../../..

echo '+++ Running tests'
gotestsum --junitfile "junit-${BUILDKITE_JOB_ID}.xml" -- -count=1 -coverprofile=cover.out -failfast "$@" ./...

echo 'Producing coverage report'
go tool cover -html cover.out -o cover.html
