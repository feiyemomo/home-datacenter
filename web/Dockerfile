# ---- Stage 1: build the SPA ----
# Use mirror for faster pull in China
FROM docker.m.daocloud.io/library/node:20-alpine AS builder
WORKDIR /app

# Install dependencies first for better layer caching.
COPY package.json package-lock.json* ./
RUN npm ci

# Build the production bundle.
COPY . .
RUN npm run build

# ---- Stage 2: serve with nginx ----
FROM docker.m.daocloud.io/library/nginx:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
