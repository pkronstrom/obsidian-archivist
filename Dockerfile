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

# distroless/static, not scratch.
#
# Scratch was the original choice and its instinct was right: no shell, no
# package manager, nothing for an attacker to pivot with, no CVE churn from
# userland packages. What it also means is no userspace AT ALL, and this
# service kept discovering that one silent production failure at a time:
#
#   no entrypoint to drop privileges -> root-owned files in the home directory
#                                       and host-side git refusing the repo
#   no zoneinfo                      -> TZ=Europe/Helsinki silently ignored
#   no /tmp                          -> the backup export failed
#   no CA certificates               -> every ntfy alert failed, invisibly,
#                                       because delivery is best-effort
#
# distroless/static keeps every property scratch was chosen for and supplies
# exactly those pieces: ca-certificates, /etc/passwd, /tmp. Verified by
# unpacking the image, not assumed. ~2MB.
#
# The :nonroot tag ships uid 65532, but compose pins user: "1000:1000" because
# the vault on disk is owned by 1000 and the server must be able to write it.
# The pin is what matters; the tag is a safe default for anyone running the
# image without compose.
#
# tzdata is NOT provided here and does not need to be: main.go imports
# _ "time/tzdata" so the zone database lives in the binary, which is the more
# robust fix and survives any base image change. There is a test guarding it.
#
# Everything is still pure Go -- no cgo, no SQLite, no git binary. That remains
# the load-bearing constraint: it is why the CLI ships as a portable release
# asset and why `reclaim --prune` can rewrite history in-binary. Keep it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /archivist-server /archivist-server
EXPOSE 8090
ENTRYPOINT ["/archivist-server"]
