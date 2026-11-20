# syntax=docker/dockerfile:1
#
# One image holding everything the Obsidian end-to-end test needs: the server
# built from this checkout, the plugin built from this checkout, and a real
# Obsidian for Linux. Build context is the repository root; see compose.yaml.

FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /archivist-server ./cmd/archivist-server

FROM node:24-bookworm-slim AS plugin
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json esbuild.config.mjs manifest.json styles.css ./
COPY src src
RUN npm run build

FROM node:24-bookworm-slim
# Obsidian is Electron. These are its shared libraries, a virtual display to
# draw on, and git, which the test uses to search a vault's whole history.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git xvfb xauth dbus \
        libgtk-3-0 libnss3 libasound2 libgbm1 libxss1 libxtst6 libnotify4 \
        libatspi2.0-0 libdrm2 libxkbcommon0 libsecret-1-0 fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

ARG OBSIDIAN_VERSION=1.15.1
ARG TARGETARCH
# The x86-64 archive has no architecture in its name; the arm64 one does.
RUN suffix="$([ "$TARGETARCH" = arm64 ] && echo -arm64 || true)" \
    && mkdir -p /opt/obsidian \
    && curl -fsSL "https://github.com/obsidianmd/obsidian-releases/releases/download/v${OBSIDIAN_VERSION}/obsidian-${OBSIDIAN_VERSION}${suffix}.tar.gz" \
       | tar -xz --strip-components=1 -C /opt/obsidian

WORKDIR /e2e
COPY test/e2e/package.json ./
RUN npm install --no-audit --no-fund
COPY --from=server /archivist-server /usr/local/bin/archivist-server
COPY --from=plugin /src/main.js /src/manifest.json /src/styles.css /plugin/
COPY test/e2e/*.mjs ./

# A vault's work tree is written by the server and read by git here.
RUN git config --system --add safe.directory '*'
CMD ["node", "run.mjs"]
