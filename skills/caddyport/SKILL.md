---
name: caddyport
description: Wire a project's local dev services to clean port-less URLs (http://<service>.<project>.localhost) using the caddyport CLI + the user's master Caddy. Use when the user asks to set up or use caddyport in a project, wants port-less .localhost dev URLs, wants to run docker-compose and/or host dev servers (vite/next/astro/bun/etc.) behind clean URLs with no port numbers, or wants to run several projects in parallel without port collisions.
---

# caddyport

`caddyport` is the user's own small Go CLI that gives local dev services clean
**port-less URLs** — `http://<service>.<project>.localhost` — by allocating a free
port and writing a route file into a user-level **master Caddy** on `:80`. Caddy
routes each hostname straight to the port (no proxy of its own).

- Source: `~/Projects/caddyport` · binary: `caddyport` (on PATH at `~/.local/bin`)
- Master Caddy config: `~/master_caddy/Caddyfile` (= `{ auto_https off }` + `import conf.d/*.caddy`)
- Route files it manages: `~/master_caddy/conf.d/<project>.caddy` (don't hand-edit)
- Project name defaults to the current directory basename (override with `--project`)

## 0. Preconditions (verify once per machine)

```bash
command -v caddyport || (cd ~/Projects/caddyport && go build -o ~/.local/bin/caddyport .)
command -v caddy || echo "brew install caddy"
caddyport ls   # creates ~/master_caddy/Caddyfile on first run if missing
```

Caddy must own port 80 (privileged — needs sudo once):

```bash
# Point Homebrew's Caddy at the master config, then start it as a root service:
printf 'import %s/master_caddy/Caddyfile\n' "$HOME" > /opt/homebrew/etc/Caddyfile
sudo brew services start caddy
```

`.localhost` resolves to `127.0.0.1` automatically on macOS — no `/etc/hosts` edits.
After the initial start, caddyport reloads Caddy via its admin API (no sudo).

**Linux / WSL2** (no Homebrew, often no systemd): install Caddy from
<https://caddyserver.com/docs/install>, disable the packaged service if one started
(`sudo systemctl disable --now caddy` — on restart it would drop caddyport's routes),
then allow it to bind port 80 once:

```bash
sudo setcap cap_net_bind_service=+ep "$(readlink -f "$(command -v caddy)")"
```

No service is needed there — on Linux caddyport starts the master Caddy on demand
(log: `~/master_caddy/caddy.log`). On WSL2 the URLs open from a Windows browser as is.

## Commands

```
caddyport serve <name> [--project P] -- <cmd...>   host process on a free port at
                                                    http://<name>.<project>.localhost
caddyport sync [--project P] [-- <compose args>]   register a running docker compose stack
caddyport up [--project P]                          run caddyport.json (compose up + serves)
caddyport down [project]                            remove routes (+ compose down if configured)
caddyport ls                                        list projects -> hosts -> ports
caddyport get-port                                  print one free port
```

Env exported to `serve` processes: `PORT`, `<NAME>_PORT`, `<NAME>_URL`, `CADDYPORT_URL`.

**Apex host:** name a service `@` to bind the bare project host `http://<project>.localhost`
(e.g. a marketing site mirroring prod's `example.com`): `caddyport serve @ -- npm run dev`,
or `"@": "npm run dev"` under `serve` in `caddyport.json`. Its env vars are `APEX_PORT` /
`APEX_URL`. Host processes only — compose services can't be named `@`.

## Integrating into a project

Inspect the project first: which parts are **docker-compose** and which are **host
dev servers** (vite/next/astro/bun/…)? Then:

### 1. docker-compose services
- Prefer **ephemeral** host ports so parallel projects/worktrees never collide:
  change `ports: ["3001:3001"]` → `ports: ["3001"]` (Docker picks a free host port).
  If you can't edit the compose (e.g. a locked day-job repo), leave it — routes still
  work as long as its fixed ports don't clash with another running project.
- Register routes after the stack is up:
  ```bash
  docker compose up -d
  caddyport sync -- -p <project> -f docker-compose.yml   # pass the same -p/-f used to bring it up
  ```
- `sync` skips well-known non-HTTP ports (25/587/3306/6379/1025/11333/5432/27017) so
  DB/SMTP/Redis services don't get bogus routes.

### 2. Host dev servers
- The server must bind `$PORT` (many respect it: Next, Rails, CRA). Vite/Astro need a
  one-line config change: `server.port = Number(process.env.PORT) || <default>`.
- Run it wrapped:
  ```bash
  caddyport serve web -- npm run dev        # or: -- vite, -- bun run dev, etc.
  ```
- **Vite HMR behind Caddy** needs `server.hmr.clientPort: 80` in the vite config
  (Astro: `vite: { server: { hmr: { clientPort: 80 } } }`), plus
  `server.allowedHosts: ['.localhost']`. Without this the page loads but hot-reload breaks.

### 3. Wire a frontend to its backend
Point the frontend's API base at the backend's port-less URL, via the app's own env
mechanism (don't hardcode a port). e.g. in the frontend's `.env`:
`API_URL=http://<backend-service>.<project>.localhost`. A worktree can override with a
gitignored `.env.local`.

### 4. One-command startup: `caddyport.json`
Drop this at the project root, then `caddyport up` does compose up + register + launch
all servers on free ports (Ctrl+C stops the servers; docker stays up):

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

Wire the project's dev script to it, e.g. `"dev": "caddyport up"` in package.json.

## Zero-config quick use (no files added)

For a project you can't/won't modify:
```bash
docker compose up -d && caddyport sync -- -p <project> -f <compose-file>   # docker services
caddyport serve web -- <their dev command>                                 # a host server
caddyport ls        # see the URLs
caddyport down      # tear down routes
```

## Verify

```bash
caddyport ls                                             # routes -> ports
curl -s -o /dev/null -w '%{http_code}\n' http://<svc>.<project>.localhost/
# reused-connection sanity (should ALL be fast, ~ backend latency, no multi-second stalls):
U=http://<svc>.<project>.localhost/
curl -s -o /dev/null -w 'r1 %{time_total}s\n' "$U" --next -s -o /dev/null -w 'r2 %{time_total}s\n' "$U" --next -s -o /dev/null -w 'r3 %{time_total}s\n' "$U"
```

## Notes

- If a route 404s / returns empty 200: the host isn't registered — check `caddyport ls`
  and that the process/container is actually up.
- If reload says "could not reload Caddy": Caddy isn't running — `sudo brew services start caddy`.
- On Linux, "Caddy is not allowed to listen on port 80": run the `setcap` command it prints, then re-run.
- Multiple projects coexist: each `<project>` (dir basename) gets its own subdomain and
  free ports, so you can run them all at once.
