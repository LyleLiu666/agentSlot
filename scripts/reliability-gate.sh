#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)

stages='agentslot:format
agentslot:fault-injection
agentslot:race
agentslot:vet
agentslot:build'

if [ "${1:-}" = "--list" ]; then
	printf '%s\n' "$stages"
	exit 0
fi
if [ "$#" -ne 0 ]; then
	echo "usage: $0 [--list]" >&2
	exit 2
fi

cd "$repository_dir"

echo "[agentslot:format] checking Go formatting"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "unformatted Go files:" >&2
	printf '%s\n' "$unformatted" >&2
	exit 1
fi

echo "[agentslot:fault-injection] running deterministic crash and recovery matrix"
go test ./session ./model/modeltest ./standardagent \
	-run 'TestFileStoreFaultInjection|TestMemoryStoreRecovery|TestRunChecksFakeExecutor|TestRuntimeRejectsModelStreamClosed|TestRuntimeCancellation|TestAgentLoopReturnCancels' \
	-count=1

echo "[agentslot:race] running complete race-enabled suite"
go test -race ./...

echo "[agentslot:vet] running static analysis"
go vet ./...

echo "[agentslot:build] building every package"
go build ./...
