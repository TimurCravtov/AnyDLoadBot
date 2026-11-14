# Cloudflare WARP as a SOCKS5 proxy container, built from Cloudflare's official
# apt package. The bot reaches it at socks5://warp:1080 over the compose network.
FROM debian:12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg socat dbus \
    && curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg \
       | gpg --yes --dearmor --output /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ bookworm main" \
       > /etc/apt/sources.list.d/cloudflare-client.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends cloudflare-warp \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

# warp-cli's proxy only listens on the container's loopback; socat republishes it
# on 1080 so other containers on the compose network can use it.
EXPOSE 1080
ENTRYPOINT ["/entrypoint.sh"]
