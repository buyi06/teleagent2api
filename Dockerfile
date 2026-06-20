FROM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o teleagent2api .

FROM alpine:3.21
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /build/teleagent2api .
EXPOSE 10000 7823
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD sh -c 'port="${TELEAGENT2API_LISTEN#:}"; wget -qO- "http://localhost:${port:-10000}/health" || exit 1'
CMD ["./teleagent2api"]
