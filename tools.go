package main

import (
	"bufio"
	"context"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

type tool struct {
	timeout time.Duration
	args    func(cfg *config, family int, ip netip.Addr) (bin string, args []string)
}

// Arguments are fixed; only the already-validated IP is appended, after "--"
// where the tool supports it. No shell is ever involved.
var tools = map[string]tool{
	"ping": {
		timeout: 20 * time.Second,
		args: func(cfg *config, family int, ip netip.Addr) (string, []string) {
			return cfg.pingBin, []string{"-" + strconv.Itoa(family), "-n", "-c", "5", "-i", "1", "-W", "2", "--", ip.String()}
		},
	},
	"traceroute": {
		timeout: 60 * time.Second,
		args: func(cfg *config, family int, ip netip.Addr) (string, []string) {
			return cfg.tracerouteBin, []string{"-" + strconv.Itoa(family), "-n", "-q", "1", "-w", "2", "-m", "30", "--", ip.String()}
		},
	},
}

var ipToken = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b|\b[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{0,4}){2,7}\b`)

// maskInternal hides hops that are not publicly routable (the provider's
// internal 10.x routers, etc.). They are noise to a visitor and only leak
// internal topology.
func maskInternal(line string) string {
	return ipToken.ReplaceAllStringFunc(line, func(s string) string {
		a, err := netip.ParseAddr(s)
		if err != nil || isPublic(a, nil) {
			return s
		}
		return "[internal]"
	})
}

// run executes the tool and calls emit for every output line. Stdout and
// stderr are merged so the visitor sees errors too.
func run(ctx context.Context, cfg *config, name string, family int, ip netip.Addr, emit func(string)) error {
	t := tools[name]
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	bin, args := t.args(cfg, family, ip)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 4096), 4096)
	for lines := 0; sc.Scan() && lines < 200; lines++ {
		line := sc.Text()
		if cfg.maskInternal {
			line = maskInternal(line)
		}
		emit(line)
	}
	return cmd.Wait()
}
