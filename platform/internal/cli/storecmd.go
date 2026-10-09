package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/prjvvl/agen/platform/internal/store"
)

// cmdStore: agen store host-role
func (e *env) cmdStore(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "host-role" {
		return usageErr("agen store host-role --store postgres://<admin>@host/db [--role agen_host]  (password from AGEN_HOST_DB_PASSWORD)")
	}
	fs := flag.NewFlagSet("store host-role", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	storeURL := fs.String("store", os.Getenv("AGEN_STORE"), "admin store URL (a role that may create roles)")
	role := fs.String("role", "agen_host", "name of the role to create or update")
	var namespaces listFlag
	fs.Var(&namespaces, "namespace", "limit the role to this namespace's rows (repeatable; default: all namespaces)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	password := os.Getenv("AGEN_HOST_DB_PASSWORD")
	if *storeURL == "" || password == "" {
		return errors.New("set --store (or AGEN_STORE) and AGEN_HOST_DB_PASSWORD")
	}
	st, err := store.Open(ctx, *storeURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CreateHostRole(ctx, *role, password, namespaces); err != nil {
		return err
	}
	scope := "all namespaces"
	if len(namespaces) > 0 {
		scope = "namespaces " + strings.Join(namespaces, ", ")
	}
	fmt.Fprintf(e.stdout, "role %s can use the run-data tables only (%v), rows of %s\n", *role, store.HostTables, scope)
	if u, err := url.Parse(*storeURL); err == nil && u.Host != "" {
		u.User = url.UserPassword(*role, "PASSWORD")
		fmt.Fprintf(e.stdout, "give Nests this store URL (--store): %s\n", u.Redacted())
	}
	return nil
}
