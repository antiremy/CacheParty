# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
# VERSION is shown on the dashboard; the deploy workflow sets it to the commit.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/cacheparty ./cmd/cacheparty && \
    mkdir -p /out/data/cache /out/data/logs /out/data/cachedomains

# static includes CA certificates (needed to fetch cache-domains from GitHub)
# and tzdata. It runs as root so :80 and :443 can be bound without extra
# capabilities, as the original image does.
FROM gcr.io/distroless/static-debian13
COPY --from=build /out/cacheparty /cacheparty
COPY --from=build /out/data /data
VOLUME /data
EXPOSE 80 443 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD ["/cacheparty", "healthcheck"]
ENTRYPOINT ["/cacheparty"]
