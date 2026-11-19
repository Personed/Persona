package worker

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Main runs `persona worker serve --token <token>` on stdin and stdout.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: persona worker serve --token <token>")
	}
	flags := flag.NewFlagSet("worker serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	token := flags.String("token", "", "sync token printed before the first frame")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *token == "" {
		return fmt.Errorf("persona worker serve: --token is required")
	}
	return Serve(ctx, stdin, stdout, *token)
}
