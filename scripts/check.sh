#!/usr/bin/env sh
# Runs the checks CI does, plus gofmt. Run it before committing: sh scripts/check.sh
set -e
cd "$(dirname "$0")/.."

go tool templ generate
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "Not formatted (run gofmt -w .):" && echo "$unformatted" && exit 1
fi
go vet ./...
go test ./...
echo "All checks passed. Commit the generated *_templ.go files along with any .templ changes."
