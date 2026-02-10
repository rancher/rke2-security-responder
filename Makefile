.PHONY: all build build-compressed test test-unit test-e2e-kind test-e2e-rke2 test-all clean lint helm-lint docker-build install-hooks

BINARY_NAME=bin/security-responder
DOCKER_REPO=rancher/rke2-security-responder
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
ARCH?=amd64

all: build

build:
	CGO_ENABLED=0 go build \
		-ldflags "-s -w -X main.Version=$(VERSION)" \
		-trimpath \
		-o $(BINARY_NAME) \
		main.go

build-compressed: build
	upx $(BINARY_NAME)

test:
	go test -v ./...

test-unit:
	go test -v -race ./...

test-e2e-kind:
	./scripts/e2e-kind.sh

test-e2e-rke2:
	./scripts/e2e-rke2.sh

test-all: test-unit test-e2e-kind

clean:
	rm -rf bin/ dist/

lint:
	golangci-lint run

helm-lint:
	helm lint charts/rke2-security-responder

helm-template:
	helm template rke2-security-responder charts/rke2-security-responder \
		--namespace kube-system

docker-build:
	docker buildx build \
		--platform linux/$(ARCH) \
		--build-arg BUILDARCH=$(ARCH) \
		--build-arg TAG=$(VERSION) \
		--load \
		-t $(DOCKER_REPO):$(VERSION)-$(ARCH) \
		.

docker-build-multi:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg TAG=$(VERSION) \
		-t $(DOCKER_REPO):$(VERSION) \
		.

fmt:
	go fmt ./...

vet:
	go vet ./...

install-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed"

