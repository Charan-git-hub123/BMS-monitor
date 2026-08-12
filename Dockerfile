# Step 1: Build the Go binary
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o app .

# Step 2: Create a minimal runner with Chromium installed
FROM alpine:latest
RUN apk add --no-cache \
    chromium \
    nss \
    freetype \
    harfbuzz \
    ca-certificates \
    ttf-freefont

ENV CHROME_PATH=/usr/bin/chromium-browser

WORKDIR /root/
COPY --from=builder /app/app .

EXPOSE 10000
CMD ["./app"]