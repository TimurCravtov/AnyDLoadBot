FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# YouTube support needs yt-dlp and ffmpeg at runtime, so this image can't be distroless.
FROM debian:12-slim

# yt-dlp is pinned to one version and installed from PyPI into its own Python
# environment, with its default extras (including the YouTube challenge
# solver) and curl_cffi, whose browser impersonation TikTok (and other sites)
# need; the "pin-curl-cffi" extra pins curl_cffi to the version yt-dlp tested.
# It is the pip package rather than the self-contained release binary so that
# the yt-dlp server (deploy/ytdlp-server) can import it, which saves yt-dlp's
# ~2 s start-up on every run.
# To upgrade, change the version: https://pypi.org/project/yt-dlp/
ARG YTDLP_VERSION=2026.11.23

# yt-dlp needs a JavaScript runtime to solve YouTube's challenges; without one,
# stream URLs often fail with "HTTP Error 403". Deno is the runtime yt-dlp uses
# by default. Pinned, and verified against its SHA-256; to upgrade, change BOTH:
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
    && apt-get install -y --no-install-recommends ca-certificates curl unzip ffmpeg python3 python3-venv \
    && python3 -m venv /opt/yt-dlp \
    && /opt/yt-dlp/bin/pip install --no-cache-dir --disable-pip-version-check "yt-dlp[default,pin-curl-cffi]==${YTDLP_VERSION}" \
    && /opt/yt-dlp/bin/yt-dlp --version \
    && ln -s /opt/yt-dlp/bin/yt-dlp /usr/local/bin/yt-dlp \
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
COPY deploy/ytdlp-server/server.py /opt/ytdlp-server/server.py
# ~/.cache holds yt-dlp's cache (YouTube's player code, solved challenges);
# docker-compose.yml keeps it in a volume, so redeploys don't start cold.
RUN useradd --system --create-home bot \
    && mkdir -p /home/bot/.cache \
    && chown bot:bot /home/bot/.cache
USER bot

# Non-secret defaults. Everything else (BOT_TOKEN, STORAGE_CHAT_ID, ...) is
# passed at run time, so no secrets are baked into the image:
#   docker run --env-file .env anydloadbot
# YTDLP_SERVER="" turns the yt-dlp server off: every run then starts yt-dlp anew.
ENV LOG_FORMAT=json \
    YTDLP_PATH=/opt/yt-dlp/bin/yt-dlp \
    YTDLP_SERVER="/opt/yt-dlp/bin/python /opt/ytdlp-server/server.py"

ENTRYPOINT ["bot"]
