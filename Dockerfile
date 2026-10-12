# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): installs only common tools and drops in a **precompiled Linux single binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU) and placed by target
# architecture at dist/<TARGETARCH>/artex in the build context. This way, for multi-arch builds, arm64 only
# emulates the apt layer and no longer emulates the Next/Go compilation, which is much faster.
#
# When building the image manually in your local environment, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of recon essentials (add/remove as needed).
# Node is installed from NodeSource as 20.x: the apt nodejs bundled with bookworm is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Pre-install Playwright MCP and CLI (globally) so there's no npx network download at runtime.
# @playwright/mcp: the browser MCP runs directly via `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; after install, run --help to verify it's executable.
# Then install playwright (which provides browser management); after install, use --with-deps to preseed chromium and its system dependencies,
# so the MCP/CLI inside the container works on first launch without downloading the browser over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Precompiled binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Supervisor startup script: after the process exits, it decides whether to relaunch based on the exit code, and the one-click update in the UI relies on it to complete the binary swap.
# It is also responsible for forwarding SIGTERM to artex -- docker stop only sends the signal to PID 1,
# and without forwarding, artex never receives it and can't shut down gracefully, so it gets hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
