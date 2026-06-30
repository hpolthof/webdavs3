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
    -o /out/webdavs3 \
    ./cmd/webdavs3

RUN upx --best --lzma /out/webdavs3

FROM alpine:3.22

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/webdavs3 /webdavs3

EXPOSE 9000 9001

ENTRYPOINT ["/webdavs3"]
CMD []
