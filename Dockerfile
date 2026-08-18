# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Everything is pure Go: no cgo, no SQLite, no git binary. That is what allows
# the scratch image below -- keep it that way.
ARG VERSION=dev
# The module path is read from go.mod rather than written out here. A -X flag
# naming a symbol that does not exist is silently IGNORED by the linker, so a
# stale path does not fail the build -- it just leaves the version at "dev"
# forever. That is exactly what the rename to obsidian-archivist did, and it
# went unnoticed because nothing ever errored.
# CA certificates, for the one thing this binary talks to over HTTPS: the ntfy
# endpoint the write guards alert on. A scratch image has no trust store, so
# without this every HTTPS request fails with "certificate signed by unknown
# authority" -- and because notification delivery is best-effort and must never
# fail a write, that failure is silent. The guards then fire into a log nobody
# reads, which is the same as not having them. Found by deploying and watching
# the topic stay empty, not by any test.
RUN apk add --no-cache ca-certificates
RUN MOD="$(go list -m)" && CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X ${MOD}/internal/version.Version=${VERSION}" \
    -o /archivist-server ./cmd/archivist-server

# No base image. archivist-server opens files, listens on a socket and speaks git's
# object format itself; it needs nothing else from userspace.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /archivist-server /archivist-server
EXPOSE 8090
ENTRYPOINT ["/archivist-server"]
