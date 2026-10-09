// Command agen is the Agen CLI and all-in-one platform binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/prjvvl/agen/platform/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
