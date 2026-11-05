package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdMemory: agen memory set|ls — memory blocks (durable, named text an
// agent reads and updates across runs; see x-agen/config.json "memory").
// Values are read from stdin, like secrets.
func (e *env) cmdMemory(ctx context.Context, args []string) error {
	const usage = "agen memory set <id> [--title T] [--description D] [--limit-chars N] (value on stdin) | agen memory ls"
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageErr(usage)
	}
	var cf clientFlags
	fs := e.flags("memory "+args[0], &cf)
	switch args[0] {
	case "set":
		title := fs.String("title", "", "a short name for the block")
		description := fs.String("description", "", "what it is for")
		limitChars := fs.Int("limit-chars", 0, "refuse an update that would make it longer than this (0: no limit)")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr(usage)
		}
		b, err := io.ReadAll(io.LimitReader(e.stdinReader(), 1<<20+1))
		if err != nil {
			return err
		}
		value := strings.TrimRight(string(b), "\r\n")
		if value == "" {
			return errors.New("pass the block's value on stdin, e.g. printf %s \"notes\" | agen memory set NAME")
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.SetMemoryBlock(ctx, connect.NewRequest(&agenv1.SetMemoryBlockRequest{Namespace: cf.namespace, Id: pos[0],
			Label: *title, Description: *description, Value: value, LimitChars: int64(*limitChars)}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		fmt.Fprintf(e.stdout, "memory block %s set (version %d)\n", pos[0], r.Msg.Block.Version)
		return nil
	case "ls":
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		r, err := c.ListMemoryBlocks(ctx, connect.NewRequest(&agenv1.ListMemoryBlocksRequest{Namespace: cf.namespace}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "NAMESPACE\tID\tTITLE\tCHARS\tVERSION\tUPDATED")
		for _, b := range r.Msg.Blocks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n", b.Namespace, b.Id, b.Label, len(b.Value), b.Version, b.UpdatedAt.AsTime().Local().Format(time.RFC3339))
		}
		return w.Flush()
	default:
		return usageErr(usage)
	}
}
