# Builds moqit-server, meet and the moqit CLI with the Meet web app.
# Context: the directory containing both moqit/ and moqt/ (moqt is not
# published yet):  docker build -f moqit/deploy/Dockerfile -t moqit .
FROM node:22-bookworm AS web
WORKDIR /src/moqit/web
COPY moqt/ts /src/moqt/ts
COPY moqit/web/package.json moqit/web/package-lock.json ./
COPY moqit/web/packages packages
COPY moqit/web/apps apps
RUN npm ci && npm run build -w meet

FROM golang:1.26 AS go
WORKDIR /src/moqit
COPY moqt /src/moqt
COPY moqit/go.mod moqit/go.sum ./
RUN go mod download
COPY moqit .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/moqit-server ./cmd/meet ./cmd/moqit

FROM gcr.io/distroless/static-debian12
COPY --from=go /out/ /usr/local/bin/
COPY --from=web /src/moqit/web/apps/meet/dist /srv/meet
USER nonroot
ENTRYPOINT ["/usr/local/bin/moqit-server"]
