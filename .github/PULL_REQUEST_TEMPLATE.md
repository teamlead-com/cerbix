## 📌 Description

Briefly describe the problem solved or feature added by this PR. Include a link to any related GitHub Issue(s).

Fixes # (issue)

---

## 🛠️ Changes Proposed

- [ ] Changed...
- [ ] Added...
- [ ] Fixed...

---

## 🧪 Verification & Testing

Describe the tests run to verify your changes:

- [ ] **Backend Unit/Integration Tests**: `go test ./... -race` passed at the repo root.
- [ ] **Frontend Typecheck & Build**: `npm run build` passed in `frontend/`.
- [ ] **Manual / E2E Verification**: Verified locally using `docker compose -f deploy/docker-compose.yml up --build`.

---

## 📋 Checklist

Before requesting a review, please ensure:

- [ ] My code follows the project's coding style (`gofmt` applied).
- [ ] I have performed a self-review of my code.
- [ ] I have updated [`openapi.yaml`](../openapi.yaml) if API routes or schemas changed.
- [ ] I have updated relevant documentation under `docs/` (`architecture.md`, `decisions.md`, `status.md`).
- [ ] My changes do not commit secrets, bearer tokens, or sensitive credentials.
- [ ] All new and existing tests pass cleanly.
