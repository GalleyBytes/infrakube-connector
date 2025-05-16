#!/bin/bash -xe
dir="$(dirname $0)"
cd "$dir"
repo=${repo:-ghcr.io/galleybytes/infra3-connector}
tag=$(git describe --tags --dirty)
tag=${tag:-0.0.0}
GOOS=linux GOARCH=amd64 go build -installsuffix cgo -v -o bin/ctrl main.go
docker build . -t "$repo:$tag"
if [[ "$RELEASE_PROJECT" == true ]];then
  docker push "$repo:$tag"
fi
if [[ "$RELEASE_KIND" == true ]]; then
  # Load into my kind cluster for testing
  kind load docker-image "$repo:$tag"
fi

