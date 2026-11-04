# syntax=docker/dockerfile:1.7
# Sway: one static Go binary with the web UI embedded, on a distroless non-root base.

FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS engine
ARG VERSION=dev
WORKDIR /src/engine
COPY engine/go.mod engine/go.sum ./
RUN go mod download
COPY engine/ ./
COPY --from=web /src/web/out ./internal/webui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/vivekyarra/sway/engine/internal/app.Version=${VERSION}" -o /out/sway ./cmd/sway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=engine /out/sway /sway
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=15s --timeout=5s --start-period=40s CMD ["/sway", "healthcheck"]
ENTRYPOINT ["/sway"]
CMD ["serve"]
