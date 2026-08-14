# Contributing to cerbix

Thank you for your interest in contributing to **cerbix**! We welcome contributions from developers, DevOps engineers, and technical writers of all skill levels.

This document provides a set of guidelines and best practices for contributing to cerbix. Following these guidelines helps ensure a smooth, efficient process for everyone involved.

---

## 📜 Table of Contents

- [Code of Conduct](#-code-of-conduct)
- [How to Contribute](#-how-to-contribute)
  - [Reporting Bugs](#reporting-bugs)
  - [Suggesting Features](#suggesting-features)
  - [Submitting Pull Requests](#submitting-pull-requests)
- [Repository Structure](#-repository-structure)
- [Development Environment Setup](#-development-environment-setup)
- [Coding & Quality Guidelines](#-coding--quality-guidelines)
  - [Go Backend Standards](#go-backend-standards)
  - [Vue 3 & Frontend Standards](#vue-3--frontend-standards)
  - [OpenAPI & API First Development](#openapi--api-first-development)
  - [Database Migrations](#database-migrations)
- [Commit & Pull Request Conventions](#-commit--pull-request-conventions)
- [License Notice](#-license-notice)

---

## 🤝 Code of Conduct

This project and everyone participating in it is governed by the [cerbix Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code. Please report unacceptable behavior to `security@example.com`.

---

## 💡 How to Contribute

### Reporting Bugs

Before creating a bug report, please check existing issues to avoid duplicates. When filing a bug report, please include:

- **A clear, descriptive title**.
- **Steps to reproduce** the behavior.
- **Expected vs. actual behavior**.
- **Environment details**: OS, cerbix version (`cerbix version`), PostgreSQL version, and role configuration (`--role`).
- **Relevant log output** (using `log.format: json` or human-readable logs).

### Suggesting Features

We welcome feature requests! When proposing a new feature or architectural change:

- Check `docs/decisions.md` and `docs/status.md` to see if the feature has already been discussed or implemented.
- Describe the **use case** and the problem it solves.
- Outline the proposed design or workflow.

### Submitting Pull Requests

1. **Fork the repository** and create your branch from `main`:
   ```bash
   git checkout -b feature/my-new-feature
   ```
2. **Make your changes** following our coding guidelines.
3. **Run tests** and verify linting:
   ```bash
   # Backend tests
   cd backend && go test ./... -race
   
   # Frontend typecheck
   cd ../frontend && npm run build
   ```
4. **Commit your changes** using clear, imperative commit messages.
5. **Push to your fork** and submit a Pull Request targeting `main`.

---

## 📁 Repository Structure

cerbix is structured as a Go + Vue 3 monorepo:

```text
├── backend/                  # Go 1.25 backend module
│   ├── cmd/cerbix/           # CLI entrypoint (main.go)
│   ├── internal/             # Core service packages (api, auth, prober, scheduler, worker, etc.)
│   └── packaging/            # Default config example (config.example.yaml)
├── frontend/                 # Vue 3 + TypeScript + Vite SPA
│   ├── src/                  # Components, views, Pinia stores, and OpenAPI client
│   └── public/               # Static assets
├── docker/                   # Docker Compose local dev stack & environment files
├── docs/                     # Documentation
│   ├── architecture.md       # Detailed system topology and workflow diagrams
│   ├── overview.md           # Architecture overview & competitor comparison
│   ├── decisions.md          # Architectural Decision Log (ADR)
│   ├── status.md             # Live FR/NFR requirements checklist
│   └── traceability.md       # Requirement to code/test traceability matrix
├── openapi.yaml              # OpenAPI 3.0 API Specification (Source of Truth)
└── README.md                 # Project quickstart and overview
```

> **Note**: For detailed system topology and sequence diagrams, refer to [`docs/architecture.md`](docs/architecture.md).

---

## 🛠️ Development Environment Setup

### Prerequisites

- **Go**: Version 1.25 or higher
- **Node.js**: Version 20 or higher & `npm`
- **Docker & Docker Compose**: For running PostgreSQL and RabbitMQ
- **PostgreSQL**: Version 16 or higher

### Local Development (Single Process Mode)

1. **Clone the repository**:
   ```bash
   git clone https://github.com/your-org/cerbix.git
   cd cerbix
   ```

2. **Start the local dev infrastructure**:
   ```bash
   cp docker/.env.dev.example docker/.env.dev
   docker compose --env-file docker/.env.dev -f docker/docker-compose.yml --profile single --profile sso up --build
   ```
   This spins up PostgreSQL, RabbitMQ, Keycloak, and the cerbix monolith (`--role all`) on `http://localhost:8080`.

3. **Frontend Development with Hot-Reload**:
   ```bash
   cd frontend
   npm install
   npm run dev
   # Vite dev server runs on http://localhost:5173 with API proxying
   ```

---

## 📏 Coding & Quality Guidelines

### Go Backend Standards

- **Formatting**: Always format changed files using `gofmt -w <files>` or `goimports`.
- **Package Layout**: All internal business logic belongs in `backend/internal/<package>`. Do not introduce a `pkg/` folder.
- **Strict Configuration**: Configuration is strict-only (`internal/config`). Unknown YAML keys must fail fast. Do not introduce runtime self-healing or silent fallbacks for invalid configuration.
- **Tenant Isolation**: Tenant isolation is a domain invariant. Every query touching monitors, heartbeats, incidents, or status pages must be constrained to the authorized `org_id` / `project_id`.
- **Structured Logging**: Use `log/slog`. Never log secrets, bearer tokens, or session passwords.
- **Metrics**: Metrics keep the `cerbix_` prefix and low-cardinality labels.

### Vue 3 & Frontend Standards

- **Composition API**: Use Vue 3 `<script setup lang="ts">`.
- **Type Safety**: Strictly type props, emits, and API responses.
- **Styling**: Use Vanilla CSS / Tailwind CSS matching the modern dark-themed design system.
- **Bespoke Inline Charts**: Custom charts use lightweight inline SVG components for maximum rendering speed.

### OpenAPI & API First Development

- [`openapi.yaml`](openapi.yaml) is the single source of truth for REST endpoints.
- If you modify or add a REST endpoint:
  1. Update `openapi.yaml`.
  2. Regenerate frontend TypeScript types:
     ```bash
     cd frontend && npm run generate:api
     ```

### Database Migrations

- Migrations live in `backend/internal/store/migrations/*.sql` and are embedded via `go:embed`.
- Use standard SQL migration syntax supported by `goose`.
- Always test both forward migrations and rollback where applicable.

---

## 📝 Commit & Pull Request Conventions

Use imperative, scoped commit messages that describe what was done:

- `Add postgres and redis probers`
- `Fix sla rolling window calculation`
- `Implement transactional outbox for webhooks`
- `Update docs for geo worker pools`

### PR Checklist

Before submitting a Pull Request, ensure:

- [ ] `go test ./... -race` passes in `backend/`.
- [ ] `npm run build` succeeds in `frontend/`.
- [ ] Code is formatted with `gofmt`.
- [ ] Updated endpoints match `openapi.yaml`.
- [ ] Relevant documentation under `docs/` is updated if behavior changed.
- [ ] No secrets or temporary credentials are committed.

---

## 📄 License Notice

By contributing to **cerbix**, you agree that your contributions will be licensed under the project's [MIT License](LICENSE).
