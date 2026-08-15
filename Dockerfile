# syntax=docker/dockerfile:1
#
# Optional container image for NEXUS. The primary install path is still the
# single-binary `curl | sh` (see install.sh) — this exists for users who want
# to run NEXUS under Docker/Compose, e.g. for `restart: unless-stopped` on a
# home server. See docs/docker.md.

FROM --platform=$BUILDPLATFORM node:20-alpine AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG BUILD_TIME=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Real dashboard build, not the committed 3-file fallback.
COPY --from=web /web/dist ./internal/dashboard/dist
# Pre-seed an owned home dir at the exact path the final stage mounts a
# volume over — Docker copies a mount point's image content (and ownership)
# into a new named volume on first creation, which is what lets the nonroot
# process in the final stage actually write to it.
RUN mkdir -p /home/nexus/.nexus && chown -R 65532:65532 /home/nexus
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.Version=$VERSION -X main.BuildTime=$BUILD_TIME" \
    -o /out/nexus ./cmd/nexus

FROM gcr.io/distroless/static-debian12:nonroot
ENV HOME=/home/nexus
COPY --from=build --chown=65532:65532 /home/nexus /home/nexus
COPY --from=build /out/nexus /nexus
COPY LICENSE NOTICE /
USER 65532:65532
EXPOSE 3000 2222
ENTRYPOINT ["/nexus"]
CMD ["start"]
