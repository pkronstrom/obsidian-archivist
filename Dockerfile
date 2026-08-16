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
RUN MOD="$(go list -m)" && CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X ${MOD}/internal/version.Version=${VERSION}" \
    -o /archivist-server ./cmd/archivist-server

# No base image. archivist-server opens files, listens on a socket and speaks git's
# object format itself; it needs nothing else from userspace.
FROM scratch
COPY --from=build /archivist-server /archivist-server
EXPOSE 8090
ENTRYPOINT ["/archivist-server"]
