FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# YouTube support needs yt-dlp and ffmpeg at runtime, so this image can't be distroless.
FROM debian:12-slim

# yt-dlp is pinned to one version, and the download is verified against its
# SHA-256 (from the release's SHA2-256SUMS file). It is the "yt-dlp_linux.zip"
# build, which bundles Python, curl_cffi and the YouTube challenge solver:
# TikTok (and other sites) need curl_cffi's browser impersonation, which the
# plain script lacks. The zip is the unpacked form of the single-file
# "yt-dlp_linux": the same program, but it starts about a second faster on
# every run, because it doesn't unpack itself into /tmp first.
# To upgrade, change BOTH:
#   version:  https://github.com/yt-dlp/yt-dlp/releases
#   checksum: the "yt-dlp_linux.zip" line of that release's SHA2-256SUMS
ARG YTDLP_VERSION=2026.11.23
ARG YTDLP_SHA256=cc4a3aa98fb438a1dac04a6fb006fe57acbeed00fc12db9ec84e3036dbbe6ccb

# yt-dlp needs a JavaScript runtime to solve YouTube's challenges; without one,
# stream URLs often fail with "HTTP Error 403". Deno is the runtime yt-dlp uses
# by default. Pinned and verified the same way; to upgrade, change BOTH:
#   version:  https://github.com/denoland/deno/releases
#   checksum: that release's deno-x86_64-unknown-linux-gnu.zip.sha256sum
ARG DENO_VERSION=2.9.8
ARG DENO_SHA256=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609

# YouTube withholds the streams from logged-in servers without a PO token. This
# yt-dlp plugin gets the tokens from the "pot" service in docker-compose.yml
# (see YTDLP_POT_URL). Pinned and verified the same way; keep it the same
# version as that service's image. To upgrade, change BOTH:
#   version:  https://github.com/Brainicism/bgutil-ytdlp-pot-provider/releases
#   checksum: the sha256 of that release's bgutil-ytdlp-pot-provider.zip
ARG BGUTIL_VERSION=2.0.2
ARG BGUTIL_SHA256=a26b980bf6d8e8400d8f21445daa4d8381bf6183d20f0be309391f86b31e27ef

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip ffmpeg \
    && curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux.zip" -o /tmp/yt-dlp.zip \
    && echo "${YTDLP_SHA256}  /tmp/yt-dlp.zip" | sha256sum -c - \
    && unzip -q /tmp/yt-dlp.zip -d /opt/yt-dlp \
    && chmod -R a+rX /opt/yt-dlp \
    && chmod 0755 /opt/yt-dlp/yt-dlp_linux \
    && ln -s /opt/yt-dlp/yt-dlp_linux /usr/local/bin/yt-dlp \
    && rm /tmp/yt-dlp.zip \
    && curl -fsSL "https://github.com/denoland/deno/releases/download/v${DENO_VERSION}/deno-x86_64-unknown-linux-gnu.zip" -o /tmp/deno.zip \
    && echo "${DENO_SHA256}  /tmp/deno.zip" | sha256sum -c - \
    && unzip -q /tmp/deno.zip -d /usr/local/bin \
    && chmod 0755 /usr/local/bin/deno \
    && rm /tmp/deno.zip \
    && mkdir -p /etc/yt-dlp/plugins \
    && curl -fsSL "https://github.com/Brainicism/bgutil-ytdlp-pot-provider/releases/download/${BGUTIL_VERSION}/bgutil-ytdlp-pot-provider.zip" -o /etc/yt-dlp/plugins/bgutil-ytdlp-pot-provider.zip \
    && echo "${BGUTIL_SHA256}  /etc/yt-dlp/plugins/bgutil-ytdlp-pot-provider.zip" | sha256sum -c - \
    && chmod 0644 /etc/yt-dlp/plugins/bgutil-ytdlp-pot-provider.zip \
    && apt-get purge -y curl unzip && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/bot /usr/local/bin/bot
RUN useradd --system --create-home bot
USER bot

# Non-secret defaults. Everything else (BOT_TOKEN, STORAGE_CHAT_ID, ...) is
# passed at run time, so no secrets are baked into the image:
#   docker run --env-file .env anydloadbot
ENV LOG_FORMAT=json \
    YTDLP_PATH=/opt/yt-dlp/yt-dlp_linux

ENTRYPOINT ["bot"]
