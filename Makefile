GO ?= go
BINARY ?= bin/cerbix
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
PKG := github.com/teamlead-com/cerbix
LDFLAGS := -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)

# The dev facade is deliberately fixed to non-production manifests. Do not turn
# these into caller-selectable COMPOSE_FILE/ENV variables: D-0158 keeps stateful
# test helpers incapable of targeting production by accident.
override DEV_ENV_FILE := docker/.env.dev
override DEV_ENV_EXAMPLE := docker/.env.dev.example
override GEO_ENV_FILE := docker/.env.geo
override GEO_ENV_EXAMPLE := docker/.env.geo.example
override DEV_COMPOSE_FILE := docker/docker-compose.yml
override GEO_COMPOSE_FILE := docker/docker-compose.geo.yml
override COMPOSE := docker compose
# The selected broker image comes only from the persisted topology env file;
# an exported shell value must never override the retained-volume pin. Explicit
# project names likewise defeat a stray COMPOSE_PROJECT_NAME.
override DEV_DC := env -u CERBIX_RABBITMQ_IMAGE $(COMPOSE) --project-name cerbix --env-file $(DEV_ENV_FILE) -f $(DEV_COMPOSE_FILE)
override GEO_DC := env -u CERBIX_RABBITMQ_IMAGE $(COMPOSE) --project-name cerbix-geo --env-file $(GEO_ENV_FILE) -f $(GEO_COMPOSE_FILE)
READY_IMAGE ?= alpine:3.22
READY_RETRIES ?= 90
DEV_E2E_ARGS ?=
DISTRIBUTED_E2E_ARGS ?= tests/file-providers.spec.ts tests/monitors.spec.ts tests/probers.spec.ts
GEO_E2E_ARGS ?= tests/topology-geo.spec.ts tests/monitors.spec.ts tests/probers.spec.ts

.PHONY: build spa-snapshot test race lint docs-check version clean \
	dev-init geo-init dev-compose-check geo-compose-check \
	dev-build dev-build-single dev-build-distributed geo-build \
	dev-up dev-up-single dev-up-distributed geo-up geo-up-all \
	dev-ready dev-ready-single dev-ready-distributed geo-ready geo-ready-all \
	dev-test dev-test-single dev-test-distributed geo-test \
	secret-smoke mac-smoke \
	dev-down geo-down

.NOTPARALLEL: dev-up dev-up-single dev-up-distributed geo-up geo-up-all dev-down geo-down

define assert_no_geo_stack
	@running="$$(docker ps --filter label=com.docker.compose.project=cerbix-geo -q)" || { \
			echo "cannot inspect the geo topology; refusing to continue" >&2; \
			exit 2; \
		}; \
		test -z "$$running" || { \
			echo "geo stack is running; run 'make geo-down' before switching topology" >&2; \
			exit 2; \
		}
endef

define assert_no_base_stack
	@running="$$(docker ps --filter label=com.docker.compose.project=cerbix -q)" || { \
			echo "cannot inspect the base topology; refusing to continue" >&2; \
			exit 2; \
		}; \
		test -z "$$running" || { \
			echo "base dev stack is running; run 'make dev-down' before switching topology" >&2; \
			exit 2; \
		}
endef

define assert_no_single_role
	@running="$$(docker ps --filter label=com.docker.compose.project=cerbix \
		--filter label=com.docker.compose.service=cerbix -q)" || { \
			echo "cannot inspect the single-process topology; refusing to continue" >&2; \
			exit 2; \
		}; \
		test -z "$$running" || { \
			echo "single-process role is running; run 'make dev-down' before starting distributed roles" >&2; \
			exit 2; \
		}
endef

define assert_no_distributed_roles
	@scheduler="$$(docker ps --filter label=com.docker.compose.project=cerbix \
		--filter label=com.docker.compose.service=scheduler -q)" || { \
			echo "cannot inspect the distributed scheduler; refusing to continue" >&2; \
			exit 2; \
		}; \
	api="$$(docker ps \
		--filter label=com.docker.compose.project=cerbix \
		--filter label=com.docker.compose.service=api -q)" || { \
			echo "cannot inspect the distributed API; refusing to continue" >&2; \
			exit 2; \
		}; \
	worker="$$(docker ps \
		--filter label=com.docker.compose.project=cerbix \
		--filter label=com.docker.compose.service=worker -q)" || { \
			echo "cannot inspect the distributed worker; refusing to continue" >&2; \
			exit 2; \
		}; \
		test -z "$$scheduler$$api$$worker" || { \
			echo "distributed roles are running; run 'make dev-down' before starting the single process" >&2; \
			exit 2; \
		}
endef

define wait_ready
	@echo "waiting for $(2) readiness on Docker network $(1)"
	@docker run --rm --network $(1) $(READY_IMAGE) sh -ec '\
		n=0; \
		until wget -q -T 3 -O /dev/null http://$(2):8080/readyz; do \
			n=$$((n + 1)); \
			if [ "$$n" -ge "$(READY_RETRIES)" ]; then \
				echo "$(2) did not become ready after $(READY_RETRIES) attempts" >&2; \
				exit 1; \
			fi; \
			sleep 1; \
		done'
endef

build:
	@mkdir -p bin $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cerbix

## spa-snapshot: rebuild the committed SPA snapshot that `go build`/`go install` embed
##
## The Docker image builds the SPA itself and REPLACES this snapshot, so a stale snapshot
## is invisible on every `make dev-*` path — and then `go install .../cmd/cerbix@latest`
## serves whatever UI was last committed here. The snapshot went three weeks stale exactly
## because refreshing it was a manual step with no command; this is that command.
##
## `assets/placeholder.txt` is a FIXTURE, not build output: internal/web/web_test.go asserts
## the embedded asset route through it, so it is restored after every refresh.
## `--user` matters: without it the container writes `node_modules/` and its caches as
## root, and every later tool that runs as the developer — vitest creating
## `node_modules/.vite-temp`, eslint, a plain `npm run build` — then fails with EACCES on
## a tree it owns nothing in. That is not theoretical: it stopped an independent reviewer
## from running the frontend tests at all in iter-0167, and the fix belongs here rather
## than in a chown someone has to remember. A tree that was ALREADY built as root needs one
## cleanup before this works — `docker run --rm -v "$(CURDIR)/frontend":/app alpine sh -c
## 'chown -R $(shell id -u):$(shell id -g) /app/node_modules /app/dist'` — because `npm ci`
## has to delete what root wrote.
spa-snapshot:
	docker run --rm --network=host --user "$(shell id -u):$(shell id -g)" -e npm_config_cache=/tmp/.npm -v "$(CURDIR)/frontend":/app -w /app node:22-alpine sh -c "npm ci && npm run build"
	rm -rf internal/web/dist
	cp -r frontend/dist internal/web/dist
	printf 'cerbix SPA asset placeholder\n' > internal/web/dist/assets/placeholder.txt
	@echo "SPA snapshot refreshed — commit internal/web/dist/ with the change that produced it"

test:
	@mkdir -p $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

## race: the full suite under the race detector, with the timeout CI uses
##
## -timeout 40m is not padding: internal/store runs the fencing and contention tests
## serially against a live database and needs ~11 minutes under -race on a developer
## machine, which is PAST go test's 10 m default. Without the flag the package dies
## with "panic: test timed out" naming whichever test happened to be running, and the
## reader debugs the wrong thing (iter-0163 hit this in CI; .github/workflows/tests.yml
## has carried the flag since).
race:
	@mkdir -p $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test -race -count=1 -timeout 40m ./...

lint:
	golangci-lint run ./...

## docs-check: fail when a LIVING document cites a file or Test* name the tree does not have
##
## The drift is silent and was found by hand once: `docs/status.md` cited two test names that had
## never existed, and five renamed tests were cited by `docs/traceability.md` — evidence that reads
## as runnable and is not. Iteration reports, review snapshots and `docs/decisions.md` are excluded
## on purpose: they are immutable or historical, and a later rename does not make them wrong.
## It runs in well under a second. It used to take 79, which is long enough that a developer
## stops running it — and a guard nobody runs guards nothing. Two causes, both measured rather
## than guessed (2026-09-03): one recursive `**/` glob PER citation (47 s) and one regex over the
## whole concatenated source PER cited test name (60 s of the 107 s under a profiler). Both are
## now single passes; if this creeps back, profile before optimising.
docs-check:
	python3 -m unittest -q scripts/check_docs_references_test.py
	python3 scripts/check-docs-references.py

version: build
	$(BINARY) version

clean:
	rm -f $(BINARY)

# Explicit one-time bootstrap for a genuinely fresh dev broker. If the known
# broker volume already exists, its image series must be recovered deliberately
# instead of guessed from the fresh-install template (D-0157/D-0158).
dev-init:
	@test ! -e $(DEV_ENV_FILE) || { \
		echo "$(DEV_ENV_FILE) already exists; refusing to overwrite its broker pin" >&2; \
		exit 2; \
	}
	@docker info >/dev/null 2>&1 || { echo "Docker daemon is unavailable" >&2; exit 2; }
	@if docker volume inspect cerbix_rabbit_volume >/dev/null 2>&1; then \
		echo "a dev RabbitMQ volume already exists; create $(DEV_ENV_FILE) with the image matching that volume" >&2; \
		exit 2; \
	fi
	cp $(DEV_ENV_EXAMPLE) $(DEV_ENV_FILE)
	@echo "created $(DEV_ENV_FILE) for a fresh dev broker volume"

geo-init:
	@test ! -e $(GEO_ENV_FILE) || { \
		echo "$(GEO_ENV_FILE) already exists; refusing to overwrite its broker pin" >&2; \
		exit 2; \
	}
	@docker info >/dev/null 2>&1 || { echo "Docker daemon is unavailable" >&2; exit 2; }
	@if docker volume inspect cerbix-geo_rabbit_volume >/dev/null 2>&1; then \
		echo "a geo RabbitMQ volume already exists; create $(GEO_ENV_FILE) with the image matching that volume" >&2; \
		exit 2; \
	fi
	cp $(GEO_ENV_EXAMPLE) $(GEO_ENV_FILE)
	@echo "created $(GEO_ENV_FILE) for a fresh geo broker volume"

dev-compose-check:
	@test -r $(DEV_ENV_FILE) || { \
		echo "missing $(DEV_ENV_FILE); run 'make dev-init' only for a fresh broker volume" >&2; \
		exit 2; \
	}
	@$(DEV_DC) --profile single --profile distributed --profile sso --profile mail config --quiet

geo-compose-check:
	@test -r $(GEO_ENV_FILE) || { \
		echo "missing $(GEO_ENV_FILE); run 'make geo-init' only for a fresh geo broker volume" >&2; \
		exit 2; \
	}
	@$(GEO_DC) --profile geo1 --profile geo2 config --quiet

# The base profiles share one image, so single/distributed build aliases are
# intentionally equivalent and benefit from the same Docker cache.
dev-build: dev-compose-check
	$(DEV_DC) --profile single build cerbix

dev-build-single: dev-build

dev-build-distributed: dev-build

geo-build: geo-compose-check
	$(GEO_DC) build api

# Default dev topology: full single-process browser-test dependencies.
dev-up: dev-up-single

dev-up-single: dev-compose-check
	$(call assert_no_geo_stack)
	$(call assert_no_distributed_roles)
	@$(MAKE) --no-print-directory dev-build
	$(DEV_DC) --profile single stop cerbix
	$(DEV_DC) --profile sso --profile mail up -d --wait --wait-timeout 180 postgres rabbitmq mariadb keycloak mailpit
	$(DEV_DC) --profile single run --rm --no-deps cerbix migrate --config /etc/cerbix/config.yaml
	$(DEV_DC) --profile single up -d --no-deps cerbix
	@$(MAKE) --no-print-directory dev-ready-single

dev-up-distributed: dev-compose-check
	$(call assert_no_geo_stack)
	$(call assert_no_single_role)
	@$(MAKE) --no-print-directory dev-build
	$(DEV_DC) --profile distributed stop scheduler api worker
	$(DEV_DC) --profile sso --profile mail up -d --wait --wait-timeout 180 postgres rabbitmq mariadb keycloak mailpit
	$(DEV_DC) --profile distributed run --rm --no-deps api migrate --config /etc/cerbix/config.yaml
	$(DEV_DC) --profile distributed up -d --no-deps scheduler api worker
	@$(MAKE) --no-print-directory dev-ready-distributed

# Geo central and the two remote transports use their own Compose project and
# volumes. Fixed ports/networks make it mutually exclusive with the base stack.
geo-up: geo-compose-check
	$(call assert_no_base_stack)
	@$(MAKE) --no-print-directory geo-build
	$(GEO_DC) --profile geo1 --profile geo2 stop scheduler api worker-core worker-geo1 worker-geo2
	$(GEO_DC) up -d --wait --wait-timeout 180 postgres rabbitmq
	$(GEO_DC) run --rm --no-deps api migrate --config /etc/cerbix/config.yaml
	$(GEO_DC) up -d --no-deps scheduler api worker-core
	@$(MAKE) --no-print-directory geo-ready

geo-up-all: geo-up
	$(GEO_DC) --profile geo1 --profile geo2 up -d --no-deps worker-geo1 worker-geo2
	@$(MAKE) --no-print-directory geo-ready-all

dev-ready: dev-ready-single

dev-ready-single: dev-compose-check
	$(call assert_no_geo_stack)
	$(call assert_no_distributed_roles)
	$(call wait_ready,cerbix,cerbix)

dev-ready-distributed: dev-compose-check
	$(call assert_no_geo_stack)
	$(call assert_no_single_role)
	$(call wait_ready,cerbix,api)
	$(call wait_ready,cerbix,scheduler)
	$(call wait_ready,cerbix,worker)

geo-ready: geo-compose-check
	$(call assert_no_base_stack)
	$(call wait_ready,cerbix-geo-central,api)
	$(call wait_ready,cerbix-geo-central,scheduler)
	$(call wait_ready,cerbix-geo-central,worker-core)

geo-ready-all: geo-ready
	$(call wait_ready,cerbix-geo-geo1,worker-geo1)
	$(call wait_ready,cerbix-geo-geo2,worker-geo2)

# E2E goals never start, stop, or redirect a stack. They verify the named
# topology is already ready and use hard-coded loopback URLs only.
dev-test: dev-test-single

dev-test-single: dev-ready-single
	CERBIX_TOPOLOGY=single CERBIX_URL=http://localhost:8080 ./e2e/run.sh $(DEV_E2E_ARGS)

dev-test-distributed: dev-ready-distributed
	CERBIX_TOPOLOGY=distributed CERBIX_URL=http://localhost:8082 ./e2e/run.sh $(DISTRIBUTED_E2E_ARGS)

geo-test: geo-ready-all
	CERBIX_TOPOLOGY=geo CERBIX_URL=http://localhost:8082 ./e2e/run.sh $(GEO_E2E_ARGS)

# Self-contained live smokes. Unlike the dev-test goals these own their whole world — they
# build the binary, provision a throwaway database and tear both down again — so they never
# touch a dev stack and are not wired into dev-test, whose contract (D-0158) is that an E2E
# goal never starts, stops or redirects a stack.
secret-smoke:
	./e2e/secret-inventory-smoke.sh

mac-smoke:
	./e2e/mac-smoke.sh

# Teardown is intentionally narrow and non-destructive: only services declared
# by the fixed manifests, with no orphan or named-volume removal.
dev-down: dev-compose-check
	$(DEV_DC) --profile single --profile distributed --profile sso --profile mail down --timeout 30

geo-down: geo-compose-check
	$(GEO_DC) --profile geo1 --profile geo2 down --timeout 30
