package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// 'sand cache': files and folders kept on this machine's own disk, as sealed
// parts, until a date. See vault/localcache.go.

func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Keep files or folders on this machine so they open without the clouds",
		Long: `Keeping a file or folder here puts enough of its sealed parts on this
machine's own disk that it opens, streams and seeks without waiting on the
clouds. What is kept is the parts exactly as the clouds hold them — encrypted,
and only put back together, in memory, when the file is opened.

Everything kept has an end date. A folder keeps what is added to it later too.
The cache never takes the disk below a reserve (a tenth of it unless --reserve
says otherwise) or past --max, and what does not fit stays on the clouds.

  sand cache keep /films/Dune.mkv --for 7d
  sand cache keep /photos/2026 --until 2026-12-31
  sand cache ls
  sand cache rm /photos/2026
  sand cache limit --max 200G --reserve 20G

'sand serve' fetches what is kept while the vault is open; 'sand cache fill'
does it here in the foreground.`,
	}
	cmd.AddCommand(cacheKeepCmd(), cacheListCmd(), cacheRemoveCmd(), cacheLimitCmd(), cacheFillCmd())
	return cmd
}

func cacheKeepCmd() *cobra.Command {
	var keepFor, until string
	cmd := &cobra.Command{
		Use:   "keep <path>",
		Short: "Keep a file or folder on this machine until a date",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			when, err := keepUntil(keepFor, until, time.Now())
			if err != nil {
				return err
			}
			v, scope, err := openVaultIn(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			pin, err := v.KeepLocally(scope, args[0], when)
			if err != nil {
				return err
			}
			fmt.Printf("Keeping %s here until %s (%s).\n", pin.Path, pin.Until.Local().Format("2 Jan 2006 15:04"), pin.ID)
			fmt.Println("'sand serve' fetches it while the vault is open, or run 'sand cache fill'.")
			return nil
		},
	}
	cmd.Flags().StringVar(&keepFor, "for", "", "how long to keep it: 12h, 7d, 2w")
	cmd.Flags().StringVar(&until, "until", "", "the date to keep it until, as 2026-12-31 or RFC 3339")
	return cmd
}

// keepUntil turns --for or --until into a moment. Exactly one is wanted.
func keepUntil(keepFor, until string, now time.Time) (time.Time, error) {
	switch {
	case keepFor != "" && until != "":
		return time.Time{}, fmt.Errorf("give --for or --until, not both")
	case keepFor != "":
		d, err := vault.ParseKeepFor(keepFor)
		if err != nil {
			return time.Time{}, fmt.Errorf("--for: %w", err)
		}
		return now.Add(d), nil
	case until != "":
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			return t, nil
		}
		t, err := time.ParseInLocation("2006-01-02", until, time.Local)
		if err != nil {
			return time.Time{}, fmt.Errorf("--until: %q is not a date — write one as 2026-12-31", until)
		}
		// The whole of the day named, not its first second.
		return t.AddDate(0, 0, 1), nil
	}
	return time.Time{}, fmt.Errorf("say how long to keep it, with --for or --until")
}

func cacheListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List what is kept here, how much of it has arrived, and the room left",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			status, err := v.LocalCache()
			if err != nil {
				return err
			}
			printCacheStatus(status)
			return nil
		},
	}
}

func printCacheStatus(status vault.LocalCacheStatus) {
	u := status.Usage
	fmt.Printf("Cache: %s, %s held", status.Dir, formatBytes(u.Used))
	if u.Limit > 0 {
		fmt.Printf(" of at most %s", formatBytes(u.Limit))
	}
	fmt.Printf("; %s more fits", formatBytes(u.Room))
	switch {
	case u.DiskTotal > 0 && u.Reserve > 0:
		fmt.Printf(" (keeping %s of the disk free)", formatBytes(u.Reserve))
	case u.DiskTotal > 0:
		fmt.Print(" (keeping no disk space in reserve)")
	}
	fmt.Println()
	if status.Problem != "" {
		fmt.Println("Last fill:", status.Problem)
	}
	if len(status.Pins) == 0 {
		fmt.Println("Nothing is kept here.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPATH\tUNTIL\tFILES\tHERE\tSTATE")
	for _, p := range status.Pins {
		path := p.Path
		if p.Folder && !strings.HasSuffix(path, "/") {
			path += "/"
		}
		state := "filling"
		switch {
		case !p.Found:
			state = "not in the vault"
		case p.Complete:
			state = "kept"
		}
		if p.Unsupported > 0 {
			state += fmt.Sprintf(", %d need converting first", p.Unsupported)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s / %s\t%s\n", p.ID, path,
			p.Until.Local().Format("2 Jan 2006 15:04"), p.Files,
			formatBytes(p.Cached), formatBytes(p.Bytes), state)
	}
	tw.Flush()
}

func cacheRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id or path>",
		Short: "Stop keeping a file or folder here, and free what only it was keeping",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			if err := v.StopKeeping(args[0]); err != nil {
				return err
			}
			fmt.Printf("No longer keeping %s here.\n", args[0])
			return nil
		},
	}
}

func cacheLimitCmd() *cobra.Command {
	var maxSize, reserve string
	cmd := &cobra.Command{
		Use:   "limit",
		Short: "Set the most the cache may hold and the disk space it leaves free",
		Long: `--max is the most the cache may hold ('none' for no limit of its own).
--reserve is the free space it always leaves on the disk: 'default' for a tenth
of the disk (between 1 and 20 GB), 'none' to leave nothing, or a size.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)
			status, err := v.LocalCache()
			if err != nil {
				return err
			}
			limit, res := status.Limit, status.Reserve
			if cmd.Flags().Changed("max") {
				n, err := parseSizeLimit(maxSize, "--max")
				if err != nil {
					return err
				}
				limit = max(n, 0) // "none" reads as -1, which is no limit
			}
			if cmd.Flags().Changed("reserve") {
				switch strings.ToLower(strings.TrimSpace(reserve)) {
				case "default", "":
					res = 0
				default:
					n, err := parseSizeLimit(reserve, "--reserve")
					if err != nil {
						return err
					}
					res = n // "none" reads as -1, which is no reserve
				}
			}
			if err := v.SetLocalCacheLimits(limit, res); err != nil {
				return err
			}
			status, err = v.LocalCache()
			if err != nil {
				return err
			}
			printCacheStatus(status)
			return nil
		},
	}
	cmd.Flags().StringVar(&maxSize, "max", "", "the most the cache may hold, such as 200G, or none")
	cmd.Flags().StringVar(&reserve, "reserve", "", "the free space to leave on the disk: a size, default or none")
	return cmd
}

func cacheFillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fill",
		Short: "Fetch what is kept here, in the foreground, until all of it has arrived",
		Long: `Under 'sand serve' this happens on its own while the vault is open. This is the
same work in the foreground; Ctrl-C stops it, and what arrived stays.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault(cmd)
			if err != nil {
				return err
			}
			defer closeVault(v)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			var total int64
			for {
				step, err := v.FillLocalCache(ctx, 64<<20)
				if err != nil {
					if ctx.Err() != nil {
						fmt.Printf("Stopped, with %s fetched.\n", formatBytes(total))
						return nil
					}
					return err
				}
				total += step.Fetched
				if step.Swept > 0 {
					fmt.Printf("Let go of %s no longer kept.\n", formatBytes(step.Swept))
				}
				if step.Fetched > 0 {
					fmt.Printf("%s: %s fetched\n", step.Path, formatBytes(total))
				}
				switch {
				case step.Full:
					return fmt.Errorf("the cache is out of room after %s; 'sand cache limit' sets how much it may take", formatBytes(total))
				case step.Idle || step.Fetched == 0:
					fmt.Println("Everything kept here has arrived.")
					return nil
				}
			}
		},
	}
}
