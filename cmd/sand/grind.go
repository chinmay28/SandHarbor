package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/chinmay28/SandHarbor/internal/compress"
	"github.com/chinmay28/SandHarbor/internal/grind"
	"github.com/chinmay28/SandHarbor/internal/version"
	"github.com/spf13/cobra"
)

// grindHeadroom is what the runtime is allowed past a file's budget before
// the collector works hard to stay under it: the grinder's own goroutines and
// buffers, and the slack a soft limit needs to be one.
const grindHeadroom = 64 << 20

// grindCmd is the far end of making parts on the machine a file is on. The
// vault runs it over SSH; it is nothing a person runs by hand, which is why it
// is hidden, and its standard output is the protocol and nothing else.
func grindCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "grind",
		Short:  "Make a file's encrypted parts here, for a vault importing from this machine",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()

			// Below the machine's own work: whoever is sitting at it should
			// not feel a film being cut in the background.
			lowerPriority()

			g := &grind.Grinder{
				In: os.Stdin, Out: os.Stdout, Version: version.String(),
				OnBudget: func(b grind.Budget) { debug.SetMemoryLimit(b.Total + grindHeadroom) },
				// The vault's level, and one thread per chunk: the window is
				// the parallelism, and it is already held to the cores.
				OnJob: func(j grind.Job) { compress.Configure(compress.Options{Level: j.Level, Threads: 1}) },
			}
			return g.Serve(ctx)
		},
	}
}
