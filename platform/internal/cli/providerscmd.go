package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdProviders lists provider accounts held after an account error, or
// resumes one (agen providers resume ACCOUNT).
func (e *env) cmdProviders(ctx context.Context, args []string) error {
	var cf clientFlags
	if len(args) > 0 && args[0] == "resume" {
		rest, err := parse(e.flags("providers resume", &cf), args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return usageErr("agen providers resume ACCOUNT")
		}
		c, err := e.client(cf)
		if err != nil {
			return err
		}
		if _, err := c.ResumeProvider(ctx, connect.NewRequest(&agenv1.ResumeProviderRequest{Account: rest[0]})); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "%s resumed\n", rest[0])
		return nil
	}
	if _, err := parse(e.flags("providers", &cf), args); err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListProviderHolds(ctx, connect.NewRequest(&agenv1.ListProviderHoldsRequest{}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	if len(r.Msg.Holds) == 0 {
		fmt.Fprintln(e.stdout, "no provider account is held")
		return nil
	}
	w := e.table()
	fmt.Fprintln(w, "ACCOUNT\tHELD SINCE\tNEXT TRY\tFAILURES\tREASON")
	for _, h := range r.Msg.Holds {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", h.Account, h.HeldAt.AsTime().Local().Format(time.RFC3339),
			h.RetryAt.AsTime().Local().Format(time.RFC3339), h.Failures, h.Reason)
	}
	return w.Flush()
}
