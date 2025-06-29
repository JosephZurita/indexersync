# syntax=docker/dockerfile:1

########################################
# 1) Build stage
########################################
FROM golang:1.24-alpine AS builder

# set working directory
WORKDIR /src

# cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# copy source
COPY . .

# build a statically-linked binary
RUN CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/indexersync

########################################
# 2) Final image
########################################
FROM alpine:3.18

# install CA certs for HTTPS
RUN apk add --no-cache ca-certificates

# copy binary from builder
COPY --from=builder /out/indexersync /usr/local/bin/indexersync

# create a non-root user
RUN addgroup -S app && adduser -S -G app app
USER app

# optional config volume
VOLUME /config

# run the binary
ENTRYPOINT ["indexersync"]
CMD []
