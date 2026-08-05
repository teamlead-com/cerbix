# Spec: Logging (ops-logging)

> Skeleton.

## Purpose

Structured logging of the service.

## Scope

- `log/slog`; format `json` (default) or `text`; levels `debug|info|error|critical`
  (custom `CRITICAL`).
- Level/format — from the strict config (`internal/config`), not from inline env.
- Secrets/tokens/cookies/`client_secret`/bearer are NOT logged (enforced by `forbidigo`).

## Requirements (draft)

- NFR: a single logger, configurable level; critical errors at `CRITICAL`.
- NFR (**security**): the ban on logging secrets is verified by the linter and a test.
