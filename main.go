// caddyport — allocate free ports and wire them into a user-level "master" Caddy
// so local dev services get clean port-less URLs: http://<name>.<project>.localhost
//
// It writes per-project route files into ~/master_caddy/conf.d/<project>.caddy and
// reloads Caddy (admin API, no sudo). Caddy on :80 routes each hostname straight to
// 127.0.0.1:<port>. No proxy of its own — that's the whole point (a previous tool,
// dockportless, stalled ~5s on reused connections; Caddy does not).
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown(os.Args[2:])
	case "ls", "list":
		err = cmdLs()
	case "get-port":
		p, e := freePort()
		if e != nil {
			err = e
		} else {
			fmt.Println(p)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "caddyport: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "caddyport:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `caddyport — port-less local dev URLs via a user-level Caddy

Usage:
  caddyport serve <name> [--project P] -- <cmd...>   run a host process on a free port,
                                                      exposed at http://<name>.<project>.localhost
                                                      (name "@" = apex: http://<project>.localhost)
  caddyport sync [--project P] [-- <compose args>]   register routes for a running docker compose stack
  caddyport up [--project P]                          orchestrate caddyport.json (compose up + serves)
  caddyport down [project] [--project P]              stop compose (if configured) and remove routes
  caddyport ls                                        list registered projects/routes
  caddyport get-port                                  print one free TCP port

Env exported to `+"`serve`"+` processes: PORT, <NAME>_PORT, <NAME>_URL, CADDYPORT_URL
Routes live in ~/master_caddy/conf.d/<project>.caddy
`)
}

// ---------- commands ----------

func cmdServe(args []string) error {
	name, project, cmdParts, err := parseServe(args)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("serve: missing <name>")
	}
	if len(cmdParts) == 0 {
		return fmt.Errorf("serve: missing command after --")
	}
	project = resolveProject(project)
	if err := ensureMaster(); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	host := hostFor(name, project)
	if err := applyRoutes(project, func(m map[string]int) { m[host] = port }); err != nil {
		return err
	}
	fmt.Printf("caddyport: http://%s  ->  127.0.0.1:%d\n", host, port)
	defer applyRoutes(project, func(m map[string]int) { delete(m, host) })

	env := serveEnv(name, project, port, host)
	return runShell(strings.Join(cmdParts, " "), env)
}

func cmdSync(args []string) error {
	project, passthrough := parseProjectAndPassthrough(args)
	project = resolveProject(project)
	if err := ensureMaster(); err != nil {
		return err
	}
	services, err := dockerPs(passthrough)
	if err != nil {
		return fmt.Errorf("docker compose ps failed: %w (is the stack up?)", err)
	}
	routes := dockerRoutes(project, services)
	if len(routes) == 0 {
		return fmt.Errorf("no published ports found for this compose stack")
	}
	if err := applyRoutes(project, func(m map[string]int) {
		for h, p := range routes {
			m[h] = p
		}
	}); err != nil {
		return err
	}
	for _, h := range sortedKeys(routes) {
		fmt.Printf("caddyport: http://%s  ->  127.0.0.1:%d\n", h, routes[h])
	}
	return nil
}

func cmdUp(args []string) error {
	projectFlag, _ := parseProjectAndPassthrough(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	project := resolveProject(firstNonEmpty(projectFlag, cfg.Project))
	if err := ensureMaster(); err != nil {
		return err
	}

	routes := map[string]int{}

	// 1. docker compose up + discover published ports
	if len(cfg.Compose) > 0 {
		composeArgs := append([]string{"-p", project}, cfg.Compose...)
		if err := runInherit("docker", append([]string{"compose"}, append(composeArgs, "up", "-d")...)...); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}
		services, err := dockerPs(composeArgs)
		if err != nil {
			return fmt.Errorf("docker compose ps failed: %w", err)
		}
		for h, p := range dockerRoutes(project, services) {
			routes[h] = p
		}
	}

	// 2. allocate free ports for each host process (deterministic order)
	type child struct {
		name, host, cmd string
		port            int
	}
	var children []child
	for _, name := range sortedKeys(toIntKeys(cfg.Serve)) {
		port, err := freePort()
		if err != nil {
			return err
		}
		host := hostFor(name, project)
		routes[host] = port
		children = append(children, child{name: name, host: host, cmd: cfg.Serve[name], port: port})
	}

	// 3. write all routes at once + reload
	if err := applyRoutes(project, func(m map[string]int) {
		for h, p := range routes {
			m[h] = p
		}
	}); err != nil {
		return err
	}

	fmt.Printf("\ncaddyport: '%s' is up\n", project)
	for _, h := range sortedKeys(routes) {
		fmt.Printf("  http://%s\n", h)
	}

	if len(children) == 0 {
		return nil // compose-only; leave routes in place
	}

	// 4. launch host processes; clean up only THEIR routes on exit (docker stays up)
	fmt.Println("\n(Ctrl+C stops the frontends; docker stays up)")
	procs := make([]*exec.Cmd, 0, len(children))
	var wg sync.WaitGroup
	for _, c := range children {
		cmd := shellCmd(c.cmd, serveEnv(c.name, project, c.port, c.host))
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "caddyport: failed to start %q: %v\n", c.name, err)
			continue
		}
		procs = append(procs, cmd)
		wg.Add(1)
		go func(cmd *exec.Cmd) { defer wg.Done(); cmd.Wait() }(cmd)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		for _, p := range procs {
			if p.Process != nil {
				syscall.Kill(-p.Process.Pid, syscall.SIGTERM)
			}
		}
	}()

	wg.Wait()
	serveHosts := make([]string, 0, len(children))
	for _, c := range children {
		serveHosts = append(serveHosts, c.host)
	}
	return applyRoutes(project, func(m map[string]int) {
		for _, h := range serveHosts {
			delete(m, h)
		}
	})
}

func cmdDown(args []string) error {
	projectFlag, _ := parseProjectAndPassthrough(args)
	// allow a bare positional project name
	for _, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") && projectFlag == "" {
			projectFlag = a
			break
		}
	}
	// compose down if a config is present
	cfg, cfgErr := loadConfig()
	project := resolveProject(firstNonEmpty(projectFlag, func() string {
		if cfgErr == nil {
			return cfg.Project
		}
		return ""
	}()))
	if cfgErr == nil && len(cfg.Compose) > 0 {
		composeArgs := append([]string{"compose", "-p", project}, cfg.Compose...)
		runInherit("docker", append(composeArgs, "down")...)
	}
	return removeProject(project)
}

func cmdLs() error {
	entries, err := filepath.Glob(filepath.Join(confDir(), "*.caddy"))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("(no active projects)")
		return nil
	}
	for _, f := range entries {
		project := strings.TrimSuffix(filepath.Base(f), ".caddy")
		m, _ := readState(project)
		fmt.Printf("%s:\n", project)
		for _, h := range sortedKeys(m) {
			fmt.Printf("  http://%-40s -> 127.0.0.1:%d\n", h, m[h])
		}
	}
	return nil
}

// ---------- routes / caddy state ----------

func home() string              { h, _ := os.UserHomeDir(); return h }
func masterDir() string         { return filepath.Join(home(), "master_caddy") }
func confDir() string           { return filepath.Join(masterDir(), "conf.d") }
func caddyfile() string         { return filepath.Join(masterDir(), "Caddyfile") }
func statePath(p string) string { return filepath.Join(confDir(), "."+p+".state.json") }
func routePath(p string) string { return filepath.Join(confDir(), p+".caddy") }

func ensureMaster() error {
	if err := os.MkdirAll(confDir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(caddyfile()); os.IsNotExist(err) {
		content := "{\n\tauto_https off\n}\n\nimport conf.d/*.caddy\n"
		if err := os.WriteFile(caddyfile(), []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "caddyport: created", caddyfile(),
			"\n  start the proxy once with: sudo brew services start caddy")
	}
	return nil
}

func readState(project string) (map[string]int, error) {
	b, err := os.ReadFile(statePath(project))
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]int{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]int{}, nil // corrupt → start fresh
	}
	return m, nil
}

func renderRoutes(project string, m map[string]int) error {
	if len(m) == 0 {
		os.Remove(routePath(project))
		os.Remove(statePath(project))
		return nil
	}
	var b strings.Builder
	for _, h := range sortedKeys(m) {
		fmt.Fprintf(&b, "http://%s {\n\treverse_proxy 127.0.0.1:%d\n}\n", h, m[h])
	}
	tmp := routePath(project) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, routePath(project)); err != nil {
		return err
	}
	sb, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(statePath(project), sb, 0o644)
}

// applyRoutes: lock the project, read-modify-write its routes, then reload Caddy.
func applyRoutes(project string, mutate func(map[string]int)) error {
	return withLock(project, func() error {
		m, err := readState(project)
		if err != nil {
			return err
		}
		mutate(m)
		if err := renderRoutes(project, m); err != nil {
			return err
		}
		reloadCaddy()
		return nil
	})
}

func removeProject(project string) error {
	return withLock(project, func() error {
		os.Remove(routePath(project))
		os.Remove(statePath(project))
		reloadCaddy()
		return nil
	})
}

func withLock(project string, fn func() error) error {
	if err := os.MkdirAll(confDir(), 0o755); err != nil {
		return err
	}
	lp := filepath.Join(confDir(), "."+project+".lock")
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func reloadCaddy() {
	cmd := exec.Command("caddy", "reload", "--config", caddyfile())
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "caddyport: could not reload Caddy — is it running? (sudo brew services start caddy)")
	}
}

// ---------- docker ----------

type publisher struct {
	URL           string
	TargetPort    int
	PublishedPort int
	Protocol      string
}
type composePs struct {
	Service    string
	Publishers []publisher
}

func dockerPs(composeArgs []string) ([]composePs, error) {
	args := append([]string{"compose"}, composeArgs...)
	args = append(args, "ps", "--format", "json")
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return nil, err
	}
	return parseComposePs(out)
}

func parseComposePs(out []byte) ([]composePs, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	// Newer compose emits a JSON array; older emits JSON-lines.
	var arr []composePs
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		return arr, nil
	}
	var res []composePs
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p composePs
		if json.Unmarshal([]byte(line), &p) == nil {
			res = append(res, p)
		}
	}
	return res, nil
}

// Common non-HTTP container ports — skipped when auto-routing so DB/SMTP/redis
// services don't get bogus HTTP routes. (A service exposing only these is skipped.)
var nonHTTPTargetPorts = map[int]bool{
	25: true, 465: true, 587: true, // SMTP
	3306: true, 5432: true, 6379: true, 27017: true, // MySQL/Postgres/Redis/Mongo
	1025: true, 11333: true, // Mailpit SMTP / rspamd worker
}

// dockerRoutes maps <service>.<project>.localhost to the service's first published
// web-ish TCP port (skipping well-known non-HTTP ports).
func dockerRoutes(project string, services []composePs) map[string]int {
	routes := map[string]int{}
	for _, s := range services {
		port := 0
		for _, pub := range s.Publishers {
			if pub.PublishedPort <= 0 || (pub.Protocol != "" && pub.Protocol != "tcp") {
				continue
			}
			if nonHTTPTargetPorts[pub.TargetPort] {
				continue
			}
			if port == 0 || pub.PublishedPort < port {
				port = pub.PublishedPort
			}
		}
		if port > 0 && s.Service != "" {
			routes[fmt.Sprintf("%s.%s.localhost", s.Service, project)] = port
		}
	}
	return routes
}

// ---------- process execution ----------

func shellCmd(cmdStr string, extraEnv []string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// runShell runs a single command in the foreground, forwarding signals to its group.
func runShell(cmdStr string, extraEnv []string) error {
	cmd := shellCmd(cmdStr, extraEnv)
	if err := cmd.Start(); err != nil {
		return err
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for s := range sigCh {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
			}
		}
	}()
	err := cmd.Wait()
	if _, ok := err.(*exec.ExitError); ok {
		return nil // child exited non-zero; not a caddyport error
	}
	return err
}

func runInherit(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// ---------- config ----------

type Config struct {
	Project string            `json:"project"`
	Compose []string          `json:"compose"`
	Serve   map[string]string `json:"serve"`
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile("caddyport.json")
	if err != nil {
		return nil, fmt.Errorf("no caddyport.json in %s", mustCwd())
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("caddyport.json: %w", err)
	}
	return &c, nil
}

// ---------- env ----------

func serveEnv(name, project string, port int, host string) []string {
	up := envName(name)
	return []string{
		fmt.Sprintf("PORT=%d", port),
		fmt.Sprintf("%s_PORT=%d", up, port),
		fmt.Sprintf("%s_URL=http://%s", up, host),
		fmt.Sprintf("CADDYPORT_URL=http://%s", host),
	}
}

func envName(s string) string {
	if s == "@" {
		return "APEX"
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ---------- small utilities ----------

// hostFor maps a service name to its hostname; "@" (DNS apex convention) is the bare project host.
func hostFor(name, project string) string {
	if name == "@" {
		return project + ".localhost"
	}
	return name + "." + project + ".localhost"
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func resolveProject(explicit string) string {
	if explicit != "" {
		return sanitize(explicit)
	}
	return sanitize(filepath.Base(mustCwd()))
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toIntKeys[V any](m map[string]V) map[string]int {
	out := make(map[string]int, len(m))
	for k := range m {
		out[k] = 0
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------- arg parsing ----------

func parseServe(args []string) (name, project string, cmd []string, err error) {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--project" || a == "-p":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--project needs a value")
			}
			project = args[i+1]
			i += 2
		case a == "--":
			cmd = args[i+1:]
			return name, project, cmd, nil
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
			i++
		default:
			i++
		}
	}
	return name, project, cmd, nil
}

func parseProjectAndPassthrough(args []string) (project string, passthrough []string) {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--project" || a == "-p":
			if i+1 < len(args) {
				project = args[i+1]
				i += 2
				continue
			}
			i++
		case a == "--":
			passthrough = args[i+1:]
			return project, passthrough
		default:
			i++
		}
	}
	return project, passthrough
}
