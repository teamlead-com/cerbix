# All Node tooling runs in Docker — nothing is installed on the host.

# --network=host lets build-time npm reach the registry on hosts whose Docker
# build sandbox has no DNS (see docs). Harmless where DNS already works.
NET := --network=host
COMPOSE_DEV := docker compose -f docker-compose.dev.yml
NODE_RUN := docker run --rm $(NET) -u node -v "$(CURDIR)":/app -w /app node:22-alpine

# The SPA is served by the cerbix backend from embed.FS (no nginx image). `build`
# produces dist/ which the backend build copies into internal/web/dist.

.PHONY: dev install build lint type-check gen-api down clean

## dev: start the Vite dev server (hot reload) in a container on :5173
dev:
	$(COMPOSE_DEV) up

## install: install node_modules into the named volume via a container
install:
	$(COMPOSE_DEV) run --rm frontend-dev npm install

## build: produce dist/ via a one-shot node container (embedded by the backend)
build:
	$(NODE_RUN) sh -c "npm install && npm run build"

## lint / type-check via container
lint:
	$(NODE_RUN) sh -c "npm install && npm run lint"

type-check:
	$(NODE_RUN) sh -c "npm install && npm run type-check"

## gen-api: regenerate the TS types from ../openapi.yaml (mounts repo root so
## the contract at ../openapi.yaml is reachable inside the container)
gen-api:
	docker run --rm $(NET) -u node -v "$(CURDIR)/..":/repo -w /repo/frontend node:22-alpine \
		sh -c "npm install && npx openapi-typescript ../openapi.yaml -o src/api/schema.d.ts"

down:
	$(COMPOSE_DEV) down

clean:
	rm -rf dist node_modules
