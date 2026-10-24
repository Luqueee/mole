package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Luqueee/mole/internal/tunnel"
)

// installScript is the shell script that installs mole on a node: it fetches
// the installer into a file (piped into sh, a failed download would hand it
// an empty script, which "succeeds") and runs it. The installer builds mole,
// downloading Go first when the node has none.
func installScript(branch string) string {
	url := installerURL("install.sh", branch)
	env := ""
	if branch != "" {
		env = "MOLE_VERSION=" + shellQuote(branch) + " "
	}
	q := shellQuote(url)
	return `tmp="$(mktemp)" || exit 1; ` +
		`if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$tmp" ` + q + `; ` +
		`elif command -v wget >/dev/null 2>&1; then wget -qO "$tmp" ` + q + `; ` +
		`else echo "neither curl nor wget is installed on this node" >&2; rm -f "$tmp"; exit 1; fi; ` +
		`rc=$?; [ $rc -eq 0 ] && { ` + env + `sh "$tmp"; rc=$?; }; rm -f "$tmp"; exit $rc`
}

// nodeInstall runs the installer on a registered node, showing its output.
func nodeInstall(configPath, name, branch string) int {
	if branch != "" {
		if err := validRef(branch); err != nil {
			fmt.Fprintln(os.Stderr, "mole node:", err)
			return 2
		}
	}
	cfg, err := loadNodeConfigAt(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	node, err := lookupNode(cfg, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole node:", err)
		return 1
	}
	mgr, err := dialNode(cfg, node, newLogger("warn"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mole node: cannot reach %s: %v\n", name, err)
		return 255
	}
	defer mgr.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	where := "latest main"
	if branch != "" {
		where = "branch " + branch
	}
	fmt.Fprintf(os.Stdout, "installing mole on %s (%s)\n", name, where)
	cmd := remoteEnv + "sh -c " + shellQuote(installScript(branch))
	code, err := mgr.Exec(ctx, cmd, tunnel.ExecOptions{Stdout: os.Stdout, Stderr: os.Stdout})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mole node: %s: %v\n", name, err)
		return 255
	}
	if code != 0 {
		fmt.Fprintf(os.Stderr, "mole node: the installer failed on %s (exit %d)\n", name, code)
		if strings.TrimSpace(os.Getenv("MOLE_DEBUG")) == "" {
			fmt.Fprintln(os.Stderr, "(it needs git and curl or wget on the node)")
		}
		return code
	}
	fmt.Fprintf(os.Stdout, "mole installed on %s: try `mole node ping %s`\n", name, name)
	return 0
}
