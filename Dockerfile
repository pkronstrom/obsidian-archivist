# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Everything is pure Go: no cgo, no SQLite, no git binary. That is what allows
# the scratch image below -- keep it that way.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /vaultsync-server ./cmd/vaultsync-server

# No base image. vaultsync opens files, listens on a socket and speaks git's
# object format itself; it needs nothing else from userspace.
FROM scratch
COPY --from=build /vaultsync-server /vaultsync-server
EXPOSE 8090
ENTRYPOINT ["/vaultsync-server"]
