package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdSecret: agen secret set|ls|rm — platform secrets (bundle secrets with
// source "platform"), per namespace. Values are read from stdin (never from
// the command line) and are never shown.
func (e *env) cmdSecret(ctx context.Context, args []string) error {
	usage := usageErr("agen secret set NAME --for DEPLOYMENT... [-n NS] (value on stdin) | agen secret ls [-n NS] | agen secret rm NAME [-n NS]")
	if len(args) == 0 {
		return usage
	}
	var cf clientFlags
	fs := e.flags("secret "+args[0], &cf)
	var deployments listFlag
	if args[0] == "set" {
		fs.Var(&deployments, "for", "deployment allowed to read the secret (repeatable, at least one)")
	}
	rest, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "set" && len(rest) == 1:
		b, err := io.ReadAll(io.LimitReader(e.stdinReader(), 64<<10+1))
		if err != nil {
			return err
		}
		value := strings.TrimRight(string(b), "\r\n")
		if value == "" {
			return errors.New("pass the secret value on stdin, e.g. printf %s \"$KEY\" | agen secret set NAME")
		}
		if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Namespace: cf.namespace, Name: rest[0], Value: value, Deployments: deployments})); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "secret %s set\n", rest[0])
	case args[0] == "ls" && len(rest) == 0:
		r, err := c.ListSecrets(ctx, connect.NewRequest(&agenv1.ListSecretsRequest{Namespace: cf.namespace}))
		if err != nil {
			return err
		}
		if cf.json {
			return e.printJSON(r.Msg)
		}
		w := e.table()
		fmt.Fprintln(w, "NAMESPACE\tNAME\tDEPLOYMENTS\tUPDATED")
		for _, s := range r.Msg.Secrets {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Namespace, s.Name, strings.Join(s.Deployments, ","), s.UpdatedAt.AsTime().Local().Format(time.RFC3339))
		}
		return w.Flush()
	case args[0] == "rm" && len(rest) == 1:
		if _, err := c.DeleteSecret(ctx, connect.NewRequest(&agenv1.DeleteSecretRequest{Namespace: cf.namespace, Name: rest[0]})); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "secret %s removed\n", rest[0])
	default:
		return usage
	}
	return nil
}

func (e *env) stdinReader() io.Reader {
	if e.stdin != nil {
		return e.stdin
	}
	return os.Stdin
}
