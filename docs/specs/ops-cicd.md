# Spec: CI/CD (ops-cicd)

> Skeleton. Reflects the current CI; extended as needed.

## Purpose

Build, quality checks and tests in GitLab CI for the monorepo (backend + frontend).

## Scope

- Split `.ci/.gitlab-ci-*.yml` files, included from `.gitlab-ci.yml`; stages `lint`/`test`/`build`.
- Backend jobs (`.backend-changes`) run on changes to `backend/**`, `.ci/**`,
  `.gitlab-ci.yml`; image `golang:1.24-bookworm`; `GOFLAGS=-mod=readonly -buildvcs=false`;
  `GOTOOLCHAIN=local`; cache `.cache/go-build`,`.cache/go-mod`.
- `backend-lint` (golangci-lint v2), `backend-test` (`go test -race` + coverage gate
  `COVERAGE_GATE=70`), `backend-build` (`scripts/build-native-binary.sh` + sha256 + verify).
- `backend-test` runs a **`postgres:16-alpine` service** and sets
  `CERBIX_TEST_DATABASE_DSN`, so the opt-in store integration tests run in CI. Locally the
  same tests skip unless that env var is set (default `go test ./...` stays hermetic).
- Frontend jobs are added in Phase 4 (by `frontend/**` paths): the Vue build (`frontend/dist`)
  and copying the result into `backend/internal/web/dist/` before `go build`, so the SPA is embedded
  into the binary via `embed.FS`. A placeholder shell is committed in the repository, so the Go module
  builds without the frontend as well. The API contract is `openapi.yaml` (repo root), from which the
  TS client is generated.
- Deployment is outside CI (not considered yet).

## Requirements (draft)

- NFR: lint and tests are mandatory on MR/main; coverage not below the gate.
- NFR: the build is reproducible, version/commit are embedded into the binary and verified.
