// Package cli implements the executable's flags and startup wiring.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"kubix/internal/agent"
	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/demo"
	"kubix/internal/kube"
	"kubix/internal/tui"
)

const Version = "0.2.0"

type Options struct {
	Demo, AllNamespaces, Version                    bool
	Context, Namespace, Kubeconfig, ModelURL, Model string
	ModelTimeout, InvestigationTimeout              time.Duration
}

func Parse(args []string, output io.Writer) (Options, error) {
	var opts Options
	flags := flag.NewFlagSet("kubix", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.BoolVar(&opts.Demo, "demo", false, "Use the synthetic cluster and offline scripted investigator")
	flags.BoolVar(&opts.Version, "version", false, "Print version")
	flags.StringVar(&opts.Context, "context", "", "Kubeconfig context (does not change current-context)")
	flags.StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	flags.StringVar(&opts.Namespace, "namespace", "default", "Namespace scope")
	flags.StringVar(&opts.Namespace, "n", "default", "Namespace scope (shorthand)")
	flags.BoolVar(&opts.AllNamespaces, "all-namespaces", false, "Allow reads across namespaces")
	flags.BoolVar(&opts.AllNamespaces, "A", false, "Allow reads across namespaces (shorthand)")
	flags.StringVar(&opts.ModelURL, "model-url", "", "OpenAI-compatible base URL, e.g. http://localhost:8000/v1")
	flags.StringVar(&opts.Model, "model", "", "Served model name")
	defaults := config.DefaultModelSettings()
	flags.DurationVar(&opts.ModelTimeout, "model-timeout", defaults.Timeout, "Deadline for each model request, e.g. 300s for CPU inference")
	flags.DurationVar(&opts.InvestigationTimeout, "investigation-timeout", defaults.InvestigationTimeout, "Total investigation deadline; must cover the model timeout")
	flags.Usage = func() {
		fmt.Fprintln(output, "Kubix - ask questions and explore Kubernetes\n\nUsage: kubix [options]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if len(flags.Args()) > 0 {
		return opts, errors.New("unexpected positional arguments; use --help")
	}
	namespaceExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "n" || f.Name == "namespace" {
			namespaceExplicit = true
		}
	})
	if opts.AllNamespaces && namespaceExplicit {
		return opts, errors.New("use --namespace or --all-namespaces, not both")
	}
	if namespaceExplicit && strings.TrimSpace(opts.Namespace) == "" {
		return opts, errors.New("namespace cannot be empty; use --all-namespaces to expand scope")
	}
	if opts.AllNamespaces {
		opts.Namespace = ""
	}
	if opts.ModelTimeout <= 0 || opts.InvestigationTimeout < opts.ModelTimeout {
		return opts, errors.New("model timeout must be positive and within the investigation timeout")
	}
	return opts, nil
}

func Run(args []string, stdout, stderr io.Writer) int {
	opts, err := Parse(args, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if opts.Version {
		fmt.Fprintf(stdout, "kubix %s (Go)\n", Version)
		return 0
	}
	settings, err := config.FromEnv(opts.ModelURL, opts.Model, opts.Demo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	settings.Timeout, settings.InvestigationTimeout = opts.ModelTimeout, opts.InvestigationTimeout
	var backend core.Backend
	if opts.Demo {
		backend = demo.New()
	} else {
		backend, err = kube.New(opts.Context, opts.Namespace, opts.Kubeconfig)
		if err != nil {
			fmt.Fprintln(stderr, "Unable to load Kubernetes credentials/context. Check kubeconfig or run kubix --demo.")
			return 2
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	investigator := agent.New(backend, settings, opts.Demo)
	defer investigator.Close()
	workspace := tui.New(ctx, backend, investigator, opts.Demo)
	defer workspace.Close()
	program := tea.NewProgram(workspace, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "Could not open the terminal UI. Run kubix in an interactive terminal.")
		return 1
	}
	return 0
}
