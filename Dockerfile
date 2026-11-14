FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# YouTube support needs yt-dlp and ffmpeg at runtime, so this image can't be distroless.
FROM debian:12-slim

# yt-dlp is pinned to one version, and the download is verified against its
# SHA-256 (from the release's SHA2-256SUMS file). It is the standalone
# "yt-dlp_linux" build, which bundles Python and curl_cffi: TikTok (and other
# sites) need curl_cffi's browser impersonation, which the plain script lacks.
# To upgrade, change BOTH:
#   version:  https://github.com/yt-dlp/yt-dlp/releases
#   checksum: the "yt-dlp_linux" line of that release's SHA2-256SUMS
ARG YTDLP_VERSION=2026.10.26
ARG YTDLP_SHA256=767ba9dce7052aa196f8b12652f433d76de128ba168966fa84b6394d5e6c6795

# yt-dlp needs a JavaScript runtime to solve YouTube's challenges; without one,
# stream URLs often fail with "HTTP Error 403". Deno is the runtime yt-dlp uses
# by default. Pinned and verified the same way; to upgrade, change BOTH:
#   version:  https://github.com/denoland/deno/releases
#   checksum: that release's deno-x86_64-unknown-linux-gnu.zip.sha256sum
ARG DENO_VERSION=2.9.8
ARG DENO_SHA256=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip ffmpeg \
    && curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux" -o /usr/local/bin/yt-dlp \
    && echo "${YTDLP_SHA256}  /usr/local/bin/yt-dlp" | sha256sum -c - \
    && chmod 0755 /usr/local/bin/yt-dlp \
    && curl -fsSL "https://github.com/denoland/deno/releases/download/v${DENO_VERSION}/deno-x86_64-unknown-linux-gnu.zip" -o /tmp/deno.zip \
    && echo "${DENO_SHA256}  /tmp/deno.zip" | sha256sum -c - \
    && unzip -q /tmp/deno.zip -d /usr/local/bin \
    && chmod 0755 /usr/local/bin/deno \
    && rm /tmp/deno.zip \
    && apt-get purge -y curl unzip && apt-get autoremove -y \
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
