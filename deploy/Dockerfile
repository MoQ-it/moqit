# Builds moqit-server, meet and the moqit CLI with the Meet web app.
# Context: the moqit directory:  docker build -f deploy/Dockerfile -t moqit .
# moqt, the Go module and its TS package, is the version pinned in go.mod.
FROM golang:1.26 AS moqt
WORKDIR /src/moqit
COPY go.mod go.sum ./
RUN go mod download && cp -r "$(go list -m -f '{{.Dir}}' github.com/MoQ-it/moqt)" /src/moqt && chmod -R u+w /src/moqt

FROM node:22-bookworm AS web
WORKDIR /src/moqit/web
COPY --from=moqt /src/moqt/ts /src/moqt/ts
COPY web/package.json web/package-lock.json ./
COPY web/packages packages
COPY web/apps apps
RUN npm ci && npm run build -w meet

FROM moqt AS go
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/moqit-server ./cmd/meet ./cmd/moqit

FROM gcr.io/distroless/static-debian12
COPY --from=go /out/ /usr/local/bin/
COPY --from=web /src/moqit/web/apps/meet/dist /srv/meet
USER nonroot
ENTRYPOINT ["/usr/local/bin/moqit-server"]
