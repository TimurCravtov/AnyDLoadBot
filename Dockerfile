FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# YouTube support needs yt-dlp and ffmpeg at runtime, so this image can't be distroless.
FROM debian:12-slim

# yt-dlp is pinned to one version, and the download is verified against its
# SHA-256 (from the release's SHA2-256SUMS file). To upgrade, change BOTH:
#   version:  https://github.com/yt-dlp/yt-dlp/releases
#   checksum: the "yt-dlp" line of that release's SHA2-256SUMS
ARG YTDLP_VERSION=2026.10.26
ARG YTDLP_SHA256=e51e69e89489f199530f13d32a4342da3ddb2cf34842f364603a066db05afbf3

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg python3 \
    && curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp" -o /usr/local/bin/yt-dlp \
    && echo "${YTDLP_SHA256}  /usr/local/bin/yt-dlp" | sha256sum -c - \
    && chmod 0755 /usr/local/bin/yt-dlp \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/bot /usr/local/bin/bot
RUN useradd --system --create-home bot
USER bot

# Non-secret defaults. Everything else (BOT_TOKEN, STORAGE_CHAT_ID, ...) is
# passed at run time, so no secrets are baked into the image:
#   docker run --env-file .env anydloadbot
ENV LOG_FORMAT=json \
    YTDLP_PATH=/usr/local/bin/yt-dlp

ENTRYPOINT ["bot"]
