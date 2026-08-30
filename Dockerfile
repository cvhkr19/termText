# syntax=docker/dockerfile:1

# --- builder ---
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 works because modernc.org/sqlite is a pure-Go SQLite
# driver (no cgo, no libsqlite3) — that's what makes a fully static
# binary, and the empty "scratch" final stage below, possible at all.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/termtext-server ./server

# --- final ---
# scratch, not alpine/distroless: the server makes no outbound TLS calls
# (no CA bundle needed) and logs/timestamps in UTC (no tzdata needed), so
# there's nothing here for an attacker to abuse even with code execution —
# no shell, no package manager, not even a coreutils.
FROM scratch

COPY --from=builder /out/termtext-server /termtext-server

# /data is where the SQLite file and uploaded files live — mount a
# volume here (see docker-compose.yml) so they survive a container
# recreate. The binary creates /data/uploads itself at startup.
VOLUME ["/data"]

ENV PORT=8080
ENV DATABASE_PATH=/data/termtext.db
ENV UPLOAD_DIR=/data/uploads

EXPOSE 8080
ENTRYPOINT ["/termtext-server"]
