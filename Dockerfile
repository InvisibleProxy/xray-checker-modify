# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.21

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG GIT_TAG
ARG GIT_COMMIT
ARG USERNAME=kutovoys
ARG REPOSITORY_NAME=xray-checker
ARG ENABLE_UPX=false

ENV CGO_ENABLED=0
ENV GO111MODULE=on

WORKDIR /go/src/github.com/${USERNAME}/${REPOSITORY_NAME}

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
  go build -trimpath -ldflags="-s -w -X main.version=${GIT_TAG} -X main.commit=${GIT_COMMIT}" -o /usr/bin/xray-checker . && \
  if [ "${ENABLE_UPX}" = "true" ]; then \
    apk add --no-cache upx && \
    upx --best --lzma /usr/bin/xray-checker; \
  fi

FROM alpine:${ALPINE_VERSION}

ARG USERNAME=kutovoys
ARG REPOSITORY_NAME=xray-checker

LABEL org.opencontainers.image.source=https://github.com/${USERNAME}/${REPOSITORY_NAME}

RUN apk add --no-cache ca-certificates curl tzdata && \
    adduser -D -u 1000 appuser && \
    mkdir -p /app/geo /app/data && \
    chown -R appuser:appuser /app

WORKDIR /app
COPY --from=builder /usr/bin/xray-checker /usr/bin/xray-checker

USER appuser

ENTRYPOINT ["/usr/bin/xray-checker"]
