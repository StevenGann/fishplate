# syntax=docker/dockerfile:1

# Build stage: static, cgo-free binary (modernc.org/sqlite is pure Go).
FROM golang:1.27 AS build

# Injected at build time, e.g.
#   docker build --build-arg VERSION=$(git describe --tags --always) \
#                --build-arg COMMIT=$(git rev-parse --short HEAD) \
#                --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) .
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/StevenGann/fishplate/internal/version.Version=${VERSION} \
        -X github.com/StevenGann/fishplate/internal/version.Commit=${COMMIT} \
        -X github.com/StevenGann/fishplate/internal/version.Date=${DATE}" \
      -o /out/fishplate ./cmd/fishplate

# The runtime image has no shell, so the data directory is prepared here and
# copied in owned by the distroless nonroot user (65532).
RUN mkdir -p /out/data

# Runtime stage: no shell, no package manager, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/fishplate /fishplate
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
VOLUME ["/data"]
EXPOSE 8080
ENV FISHPLATE_DB_PATH=/data/fishplate.db

ENTRYPOINT ["/fishplate"]
CMD ["serve"]
