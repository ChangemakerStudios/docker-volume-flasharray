package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/config"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/driver"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
)

const adoptUsage = `usage: docker-volume-flasharray adopt [flags]

Tag existing FlashArray volumes so this driver serves them under their old
Docker names, without renaming or copying anything. Typical migration from
the Pure plugin, whose volumes are named <PURE_DOCKER_NAMESPACE>-<docker name>:

  docker-volume-flasharray adopt --prefix prod- --dry-run
  docker-volume-flasharray adopt --prefix prod-

Or a single explicit mapping:

  docker-volume-flasharray adopt --volume prod-db-1=db-1

Reads the same FA_* environment and credentials file as the plugin
(FA_CONFIG, FA_NAMESPACE, ...). Volumes already tagged are skipped; a Docker
name that already maps to a different array volume is an error.

flags:
`

// runAdopt implements the `adopt` subcommand.
func runAdopt(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "adopt every live array volume whose name starts with this; docker name = name minus prefix")
	var singles multiFlag
	fs.Var(&singles, "volume", "explicit <array volume>=<docker name> mapping (repeatable)")
	dryRun := fs.Bool("dry-run", false, "print what would be tagged and exit")
	fs.Usage = func() { fmt.Fprint(stderr, adoptUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prefix == "" && len(singles) == 0 {
		fs.Usage()
		return errors.New("adopt: --prefix or --volume is required")
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	array, err := flasharray.New(ctx, flasharray.Options{
		Endpoint:           cfg.Array.Endpoint,
		APIToken:           cfg.Array.APIToken,
		APIVersion:         cfg.Array.APIVersion,
		InsecureSkipVerify: cfg.Array.InsecureSkipVerify,
		Logger:             log,
	})
	if err != nil {
		return fmt.Errorf("flasharray client: %w", err)
	}
	return adopt(ctx, array, cfg.Namespace, *prefix, singles, *dryRun, stdout, log)
}

// adopt is the testable core of runAdopt.
func adopt(ctx context.Context, array flasharray.Array, namespace, prefix string, singles []string, dryRun bool, out io.Writer, log *slog.Logger) error {
	type pair struct{ arrayName, dockerName string }
	var plan []pair

	for _, s := range singles {
		an, dn, ok := strings.Cut(s, "=")
		if !ok || an == "" || dn == "" {
			return fmt.Errorf("--volume %q: want <array volume>=<docker name>", s)
		}
		plan = append(plan, pair{an, dn})
	}
	if prefix != "" {
		vols, err := array.ListVolumes(ctx, prefix)
		if err != nil {
			return fmt.Errorf("list volumes with prefix %q: %w", prefix, err)
		}
		for _, v := range vols {
			plan = append(plan, pair{v.Name, strings.TrimPrefix(v.Name, prefix)})
		}
	}
	if len(plan) == 0 {
		fmt.Fprintln(out, "nothing to adopt")
		return nil
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].dockerName < plan[j].dockerName })

	already, err := array.ListNameTags(ctx, namespace+"/")
	if err != nil {
		return err
	}
	// claimed is tag value -> array volume, so dry-run catches a Docker name
	// already mapped elsewhere, including by an earlier entry in this plan.
	claimed := make(map[string]string, len(already))
	for an, val := range already {
		claimed[val] = an
	}

	var failed int
	for _, p := range plan {
		want := namespace + "/" + p.dockerName
		switch {
		case already[p.arrayName] == want:
			fmt.Fprintf(out, "skip    %-40s already %s\n", p.arrayName, want)
			continue
		case already[p.arrayName] != "":
			fmt.Fprintf(out, "CONFLICT %-40s tagged %s, wanted %s\n", p.arrayName, already[p.arrayName], want)
			failed++
			continue
		case claimed[want] != "" && claimed[want] != p.arrayName:
			fmt.Fprintf(out, "CONFLICT %-40s %s already maps to %s\n", p.arrayName, want, claimed[want])
			failed++
			continue
		}
		claimed[want] = p.arrayName
		if dryRun {
			fmt.Fprintf(out, "would   %-40s -> %s\n", p.arrayName, want)
			continue
		}
		if err := driver.Adopt(ctx, array, namespace, p.dockerName, p.arrayName, log); err != nil {
			fmt.Fprintf(out, "FAILED  %-40s %v\n", p.arrayName, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "adopted %-40s -> %s\n", p.arrayName, want)
	}
	if failed > 0 {
		return fmt.Errorf("%d volume(s) not adopted", failed)
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }
