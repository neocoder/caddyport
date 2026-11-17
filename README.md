# caddyport

A tiny CLI that gives local dev services clean **port-less URLs**
(`http://<name>.<project>.localhost`) by allocating a free port and wiring it
into a user-level **master Caddy**. Reusable across any project — docker-compose,
node, bun, in any combination. No per-project config required.

It does *not* proxy traffic itself (a previous tool did, and stalled ~5s on
reused HTTP connections). Caddy routes each hostname straight to the port.

## Setup (once per machine)

### macOS

1. Install Caddy: `brew install caddy`
2. Build caddyport: `go build -o ~/.local/bin/caddyport .` (ensure `~/.local/bin` is on `PATH`)
3. Point Homebrew's Caddy at the master config and start it on `:80`:
   ```sh
   caddyport ls            # creates ~/master_caddy/Caddyfile on first run
   printf 'import %s/master_caddy/Caddyfile\n' "$HOME" > /opt/homebrew/etc/Caddyfile
   sudo brew services start caddy
   ```
   `.localhost` resolves to `127.0.0.1` on macOS automatically — no `/etc/hosts`.

### Linux / WSL2

1. Install Caddy (<https://caddyserver.com/docs/install>) so `caddy` is on `PATH`.
   If the package started its own Caddy service (apt/dnf on systemd distros do), disable it:
   `sudo systemctl disable --now caddy`. Otherwise that service reloads
   `/etc/caddy/Caddyfile` on every restart and drops all caddyport routes.
2. Build caddyport: `go build -o ~/.local/bin/caddyport .`
   No Go installed? Build it in Docker instead:
   ```sh
   docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e CGO_ENABLED=0 \
     -v "$PWD":/src -w /src golang:1.25 go build -buildvcs=false -o caddyport .
   install -m 755 caddyport ~/.local/bin/caddyport
   ```
3. Let Caddy listen on port 80 without root (repeat after upgrading Caddy):
   ```sh
   sudo setcap cap_net_bind_service=+ep "$(readlink -f "$(command -v caddy)")"
   ```

There is no service to set up: on Linux caddyport starts the master Caddy itself
whenever it is not running (log: `~/master_caddy/caddy.log`), and Caddy keeps
running after you stop your dev servers. Stop it with `caddy stop`.

**WSL2 with a browser on Windows:** nothing extra is needed. Windows forwards
`localhost` ports into WSL and browsers resolve `*.localhost` on their own, so
`http://<name>.<project>.localhost` opens from Windows as long as nothing on Windows
uses port 80. Chrome and Edge reach `*.localhost` over IPv6 (`::1`); Caddy answers
on both IPv4 and IPv6, so dev servers that only listen on `127.0.0.1` (Vite's
default) work behind caddyport, even though opening them directly by port from
Windows fails with a connection reset.

## Usage

```sh
# Host process on a free port, exposed at http://web.<cwd>.localhost
# Exports PORT, WEB_PORT, WEB_URL to the command.
caddyport serve web -- npm run dev          # or: vite --port $PORT, etc.

# Register a running docker compose stack's published ports
docker compose up -d
caddyport sync -- -p myproj -f docker-compose.yml

# Orchestrate everything from caddyport.json (compose up + all serves)
caddyport up

caddyport ls          # list projects -> hosts -> ports
caddyport down        # remove routes (+ docker compose down if configured)
caddyport get-port    # print one free port
```

Use `@` as the name to bind the apex host instead (`http://<project>.localhost`),
e.g. `caddyport serve @ -- npm run dev` or `"@": "npm run dev"` in `caddyport.json`.
Its env vars are `APEX_PORT` / `APEX_URL`.

Project name defaults to the current directory's basename (override with `--project`).

### `caddyport.json` (optional, for `up`)

```json
{
  "project": "myapp",
  "compose": ["-f", "docker-compose.dev.yml"],
  "serve": {
    "app": "cd apps/app && npm run dev",
    "api": "cd apps/api && npm run dev"
  }
}
```

`up` runs `docker compose up -d`, registers the stack's web ports, then launches
each `serve` entry on its own free port. Ctrl+C stops the served processes and
removes their routes (docker stays up). Servers should read `$PORT` (or take
`--port $PORT`).

## Notes

- Env exported to `serve` processes: `PORT`, `<NAME>_PORT`, `<NAME>_URL`, `CADDYPORT_URL`.
- `sync` skips well-known non-HTTP ports (25, 587, 3306, 6379, 1025, 11333, …) so
  DB/SMTP/Redis services don't get bogus routes.
- Routes live in `~/master_caddy/conf.d/<project>.caddy` (managed by caddyport).
- Vite HMR behind Caddy needs `server.hmr.clientPort: 80` in the vite config.
