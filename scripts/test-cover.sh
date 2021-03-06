#!/usr/bin/env bash

set -eou pipefail

# this will only work on CircleCI/in build container
cd /go/src/github.com/drausin/libri
mkdir -p .cover

PKGS=$(go list ./... | grep -v /vendor/)
echo ${PKGS} | sed 's| |\n|g' | xargs -I {} bash -c '
    COVER_FILE=.cover/$(echo {} | sed -r "s|github.com/drausin/libri/||g" | sed "s|/|-|g").cov &&
    go test -race -coverprofile=${COVER_FILE} {}
'

# merge profiles together, removing results from auto-generated code
gocovmerge .cover/*.cov | grep -v '.pb.go:' > .cover/test-coverage-merged.cov
cp .cover/test-coverage-merged.cov test-coverage-merged.cov
