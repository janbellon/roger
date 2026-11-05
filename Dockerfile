# syntax=docker/dockerfile:1

FROM golang:1.25-alpine3.24 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/roger .

# The zipapp build of yt-dlp is 3 MB, starts faster than the standalone
# binaries and can update itself in place.
FROM alpine:3.24 AS ytdlp
ARG YTDLP_VERSION=2026.10.26
WORKDIR /dl
RUN wget -q "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp" \
            "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/SHA2-256SUMS" \
 && grep -E ' yt-dlp$' SHA2-256SUMS | sha256sum -c -

FROM alpine:3.24
# deno: yt-dlp needs a JavaScript runtime to solve YouTube's challenges
RUN apk add --no-cache ca-certificates deno ffmpeg python3 tini \
 && adduser -D -u 1000 roger \
 && install -d -o roger -g roger /data /opt/roger/bin

# /opt/roger/bin belongs to the app user so yt-dlp can replace itself
COPY --from=ytdlp --chown=roger:roger --chmod=755 /dl/yt-dlp /opt/roger/bin/yt-dlp
COPY --from=build /out/roger /usr/local/bin/roger

ENV PATH="/opt/roger/bin:${PATH}" \
    ROGER_DATA_DIR=/data \
    ROGER_YTDLP_UPDATE_INTERVAL=24h

USER roger
WORKDIR /data
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q --spider "http://127.0.0.1:${ROGER_PORT:-8080}/healthz" || exit 1

# tini reaps the processes left over when a download is cancelled
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["roger"]
