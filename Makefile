BINARY        := sentinel
IMAGE         := chain-registry-sentinel
REGISTRY_URL  ?= https://github.com/ny4rl4th0t3p/chain-registry
CONCURRENCY   ?= 250
TIMEOUT       ?= 30s
CHAINS        ?= cosmoshub,osmosis,juno
VERBOSE       ?= false
STATE_PATH    ?=
MIN_FAILURES  ?= 14
DRY_RUN       ?= false
GITHUB_TOKEN  ?=
GITHUB_REPO   ?=

# v1.2.3 on a tag, v1.2.3-4-gabcdef between tags, gabcdef with no tags
VERSION   := $(shell git describe --tags --dirty --always 2>/dev/null || echo "dev")
LDFLAGS   := -X main.Version=$(VERSION)

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/sentinel/

.PHONY: test
test:
	go test -count=1 -race ./...

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: integration-build
integration-build:
	docker build -f test/integration/Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg REGISTRY_URL=$(REGISTRY_URL) \
		-t $(IMAGE)-integration .

.PHONY: integration-clean
integration-clean:
	docker rmi -f $(IMAGE)-integration 2>/dev/null || true

.PHONY: integration
integration: integration-clean integration-build
	$(if $(STATE_PATH),mkdir -p $(STATE_PATH))
	docker run --rm \
		-e INPUT_CONCURRENCY=$(CONCURRENCY) \
		-e INPUT_TIMEOUT=$(TIMEOUT) \
		-e INPUT_VERBOSE=$(VERBOSE) \
		-e INPUT_MIN_FAILURES=$(MIN_FAILURES) \
		-e INPUT_DRY_RUN=$(DRY_RUN) \
		$(if $(GITHUB_TOKEN),-e INPUT_GITHUB_TOKEN=$(GITHUB_TOKEN)) \
		$(if $(GITHUB_REPO),-e INPUT_GITHUB_REPO=$(GITHUB_REPO)) \
		$(if $(STATE_PATH),-v $(abspath $(STATE_PATH)):/state -e INPUT_STATE_PATH=/state) \
		$(IMAGE)-integration --chains $(CHAINS) || true

.PHONY: lint
lint:
	golangci-lint run

.PHONY: lint-clean
lint-clean:
	golangci-lint cache clean
	golangci-lint run