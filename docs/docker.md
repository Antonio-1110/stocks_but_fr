# Running the radar with Docker

This is how the pipeline runs on the Raspberry Pi (or any Linux box, Mac or
Windows PC with Docker). It's also meant as a small Docker tutorial, so each
piece is explained below.

## Quick start

On the Pi (64-bit Raspberry Pi OS, Pi 4 or 5):

```sh
curl -fsSL https://get.docker.com | sh          # install Docker (once)
sudo usermod -aG docker $USER                   # use docker without sudo; log out and back in

git clone https://github.com/Antonio-1110/stocks_but_fr.git
cd stocks_but_fr
docker compose up -d                            # build the image and start everything
```

Then:

| Want to...                         | Run                                   |
|------------------------------------|---------------------------------------|
| see what it's doing                | `docker compose logs -f`              |
| browse the site                    | open `http://<pi-address>:8080`       |
| run one collect right now          | `docker compose exec radar radar collect` |
| pick up new code after `git pull`  | `docker compose up -d --build`        |
| stop everything                    | `docker compose down`                 |

The database ends up in `./data/radar.db` and the site in `./public/`, right
next to the code, so you can copy them off the Pi or open the DB with any
SQLite tool. The Python backtester reads that same file.

## The three words you need

- **Image**: a frozen, read-only snapshot of a tiny Linux filesystem with our
  program in it. Built from the `Dockerfile`. Like a `.zip` of an installed app.
- **Container**: a running copy of an image. You can start many containers
  from one image; when a container is deleted, anything it wrote inside
  itself is gone. That's why we use volumes.
- **Volume / bind mount**: a folder from your real machine plugged into the
  container. Whatever the container writes there survives restarts.

## The Dockerfile, piece by piece

The file is a recipe. Each instruction makes a layer, and Docker caches layers,
so a rebuild only redoes the steps after the first thing that changed.

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
```
Start from an official image that already has Go installed, and call this
stage `build`. `--platform=$BUILDPLATFORM` means "run this stage on the
machine doing the build", so building the Pi image on a laptop doesn't have
to emulate an ARM CPU (slow). It cross-compiles instead, see below.

```dockerfile
ARG TARGETOS TARGETARCH
```
Docker fills these in with the platform we're building **for**: `linux` and
`amd64` on a PC, `linux` and `arm64` on a Pi.

```dockerfile
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
```
`WORKDIR` is `cd` (creating the folder if needed). We copy only the
dependency list first and download dependencies. Because these two files
rarely change, Docker reuses this cached layer on most rebuilds and skips the
download.

```dockerfile
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/radar ./cmd/radar
```
Copy the whole repo in (minus what `.dockerignore` excludes) and compile.
- `CGO_ENABLED=0`: a fully static binary that needs no C libraries. Works
  because our SQLite driver is pure Go (that's why AGENTS.md says no cgo).
- `GOOS/GOARCH`: Go's built-in cross-compiling: produce an ARM binary on an
  x86 machine.
- `-tags timetzdata`: bake the time-zone database into the binary so
  `Asia/Taipei` works in the small runtime image.
- `-trimpath -ldflags="-s -w"`: smaller binary, no local paths inside.

```dockerfile
FROM alpine:3.22
```
A **second** `FROM` starts a fresh, empty stage: this is the multi-stage
trick. The Go compiler (hundreds of MB) stays behind in the `build` stage;
only what we copy over ships. The final image is about 25 MB. Alpine is a
tiny Linux that still has a shell, which the loop in `compose.yaml` needs.

```dockerfile
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENV TZ=Asia/Taipei
```
Borrow the list of trusted HTTPS certificate authorities from the build
stage, so collectors can talk to `https://` sites. `TZ` sets the time zone
so "today" means a Taiwan trading day and logs show Taiwan time.

```dockerfile
WORKDIR /app
COPY --from=build /out/radar /usr/local/bin/radar
COPY radar.toml ./
ENTRYPOINT ["radar"]
```
Put the binary on the `PATH`, include a default config, and make `radar` the
program the container runs. `radar.toml` uses relative paths
(`data/radar.db`, `public`), so from `/app` they become `/app/data` and
`/app/public`: exactly where `compose.yaml` plugs in the host folders.

## .dockerignore

Lists what **not** to send to Docker when building, like `.gitignore`. It
keeps `.env` (secrets) and `data/` (a big database) out of the image and makes
builds faster.

## compose.yaml, piece by piece

`docker run` with a dozen flags gets old fast. Compose writes those flags
down in one file and starts several containers together.

```yaml
services:
  radar:
    build: .
    image: stocks-radar:latest
```
A service named `radar`, whose image is built from the `Dockerfile` in this
folder and tagged `stocks-radar:latest`.

```yaml
    restart: unless-stopped
```
If the program crashes, or the Pi reboots, Docker starts it again. Only a
`docker compose down` / `stop` keeps it off.

```yaml
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        while true; do
          radar collect
          radar render
          ...
          sleep "$${RADAR_INTERVAL_SECONDS}"
        done
```
The scheduler. Instead of a separate cron container, the container runs a
small shell loop: collect, render, sleep, repeat. `$$` is how you write a
literal `$` in compose, so the variable is read by the shell inside the
container rather than by compose. A collector that fails is logged and
skipped by `radar` itself, so the loop keeps going.

```yaml
    environment:
      RADAR_INTERVAL_SECONDS: ${RADAR_INTERVAL_SECONDS:-86400}
```
How long to sleep between runs: 86400 seconds (one day) unless you set
`RADAR_INTERVAL_SECONDS` in your shell or in `.env`. Here `${...}` has one
`$`, so compose fills it in, using the value after `:-` as the default.

```yaml
    env_file:
      - path: .env
        required: false
```
API keys and other secrets go in a file named `.env` next to `compose.yaml`
(one `NAME=value` per line; it's git-ignored). Every line becomes an
environment variable inside the container. `required: false` means it's fine
if the file doesn't exist yet. The variables the code reads are listed in
the README.

```yaml
    volumes:
      - ./data:/app/data
      - ./public:/app/public
      - ./radar.toml:/app/radar.toml:ro
```
Bind mounts, written `host-path:container-path`. The database and the site
live on the Pi's disk, so they survive the container being rebuilt. Your
`radar.toml` is mounted over the baked-in one, read-only (`:ro`), so you can
change broker discount or strategy settings and they take effect on the next
run, without a rebuild. Folders that don't exist yet are created by Docker;
the container runs as root, so they'll be owned by root on the host (use
`sudo` to delete them).

```yaml
  web:
    image: nginx:1.29-alpine
    restart: unless-stopped
    ports:
      - "8080:80"
    volumes:
      - ./public:/usr/share/nginx/html:ro
```
A second service: the off-the-shelf nginx web server, serving the same
`public/` folder read-only. `ports` maps `host:container`, so port 8080 on
the Pi reaches nginx's port 80 inside. Open `http://<pi-address>:8080` from
any device on your home network. (The public copy of the site still goes to
GitHub Pages; this is just for looking at it at home.) Don't want it? Delete
this block, or run `docker compose up -d radar` to start only the pipeline.

## Building for the Pi on another machine

`docker compose up` on the Pi builds natively, which is all you need. If you
want to build an image for both kinds of machine at once (for example to
push it to a registry), use buildx:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t stocks-radar .
```

## Troubleshooting

- **`permission denied ... docker.sock`**: you skipped the `usermod` step or
  haven't logged out and back in.
- **`exec format error`**: the image was built for the wrong CPU (e.g. an
  amd64 image copied to the Pi). Rebuild on the Pi or use `--platform`.
- **Site is empty**: `public/` only fills once the dashboard renderer exists
  and data has been collected; check `docker compose logs radar`.
- **Check the Pi is 64-bit**: `uname -m` should print `aarch64`.
