package cmd

import "errors"

// errNotImplemented is the placeholder return for stub subcommands while
// they're being staged across phases.
var errNotImplemented = errors.New("not implemented yet")
