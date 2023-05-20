DOCKER_REPO ?= ghcr.io/galleybytes
IMAGE_NAME ?= terraform-operator-remote-controller
VERSION ?= $(shell  git describe --tags --dirty)
ifeq ($(VERSION),)
VERSION := 0.0.0
endif
IMG ?= ${DOCKER_REPO}/${IMAGE_NAME}:${VERSION}

kind-release: build
	docker build . -t ${IMG}
	kind load docker-image ${IMG}

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -v -o bin/ctrl main.go

ghactions-release:
	CGO_ENABLED=0 go build -v -o bin/ctrl main.go
	docker build . -t ${IMG}
	docker push ${IMG}

server:
	go run cmd/main.go

.PHONY: server release
