FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.25.4-alpine3.22 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build
ADD go.mod go.sum *.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM64="v9.0" go build -trimpath -ldflags '-extldflags "-static" -buildid=' -o gcsproxy *.go

FROM scratch
COPY --from=builder /etc/ssl /etc/ssl
COPY --from=builder /build/gcsproxy /gcsproxy
