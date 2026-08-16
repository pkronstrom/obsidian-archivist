# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Everything is pure Go: no cgo, no SQLite, no git binary. That is what allows
# the scratch image below -- keep it that way.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/pkronstrom/archivist/internal/version.Version=${VERSION}" \
    -o /archivist-server ./cmd/archivist-server

# No base image. archivist-server opens files, listens on a socket and speaks git's
# object format itself; it needs nothing else from userspace.
FROM scratch
COPY --from=build /archivist-server /archivist-server
EXPOSE 8090
ENTRYPOINT ["/archivist-server"]
