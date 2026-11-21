# Stage 1: pregenerate logo PNGs once at build time (removes sharp from the runtime image)
FROM node:20-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY scripts ./scripts
COPY public ./public
RUN node scripts/convert-logo.js

# Stage 2: Bun runtime (no sharp, no native deps, small image)
FROM oven/bun:1-alpine

WORKDIR /app

COPY --from=builder --chown=bun:bun /app/public ./public
COPY --chown=bun:bun server.js ./
COPY --chown=bun:bun scripts ./scripts
COPY --chown=bun:bun package.json ./

# Production deps only (express, cookie-parser, cors)
RUN bun install --production

# Data directory for the volume mount
RUN mkdir -p /app/data && chown bun:bun /app/data

USER bun

EXPOSE 3000

# Healthcheck against the public /health endpoint (no wget package needed)
HEALTHCHECK --interval=20s --timeout=5s --start-period=10s --retries=3 \
    CMD bun -e "fetch('http://127.0.0.1:3000/health').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"

CMD ["bun", "server.js"]
