# Multi-stage build for the cerbix binary. The Vue SPA is built here and embedded
# into the Go binary (internal/web/dist via go:embed), so this single image serves
# the whole app — REST API, /auth, public status pages, and the SPA — with no nginx.
#
# Build context is the REPO ROOT (see deploy/docker-compose.yml: context ..),
# because this Dockerfile needs both frontend/ and the Go module at the root.

# Stage 1 — build the SPA.
FROM node:22-alpine AS spa
WORKDIR /app
COPY frontend/package.json ./
RUN npm install
COPY frontend/ ./
RUN npm run build

# Stage 2 — build the Go binary with the freshly-built SPA embedded.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# Replace the committed SPA snapshot with the just-built one before embedding.
RUN rm -rf internal/web/dist
COPY --from=spa /app/dist ./internal/web/dist
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X github.com/teamlead-com/cerbix/internal/buildinfo.Version=${VERSION} -X github.com/teamlead-com/cerbix/internal/buildinfo.Commit=${COMMIT}" \
    -o /out/cerbix ./cmd/cerbix

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cerbix /usr/local/bin/cerbix
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cerbix"]
CMD ["serve", "--config", "/etc/cerbix/config.yaml", "--role", "all"]
