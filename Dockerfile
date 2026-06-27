FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates upx

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -tags netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -o /out/webdav3s \
    ./cmd/webdav3s

RUN upx --best --lzma /out/webdav3s

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/webdav3s /webdav3s

EXPOSE 9000 9001

ENTRYPOINT ["/webdav3s"]
CMD []
