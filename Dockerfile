FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# YouTube support needs yt-dlp and ffmpeg at runtime, so this image can't be distroless.
FROM debian:12-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg python3 \
    && curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
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
