# Build context is the repository root. Stand-in until milestone 7's
# distroless Dockerfile; used only by the benchmark compose file.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /out/cacheparty ./cmd/cacheparty && \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /out/bench ./bench

FROM alpine:3 AS cacheparty
COPY --from=build /out/cacheparty /cacheparty
ENTRYPOINT ["/cacheparty"]

FROM alpine:3 AS bench
COPY --from=build /out/bench /bench
ENTRYPOINT ["/bench"]
