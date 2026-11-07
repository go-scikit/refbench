// refbench works on the files the refbench library writes: it renders
// docs/perf-table.md from docs/perf.json, checks that the table has no hand
// edits, drafts one issue per slower row, and reads the host's load gate.
//
// The cases themselves are Go code in each repository's tests; a measurement
// is run there (see the README). This command never times anything.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/go-fleettools/fleettools/loadavg"
	"github.com/go-scikit/refbench"
	"github.com/spf13/cobra"
)

// osExit allows tests to override os.Exit.
var osExit = os.Exit

// readLoad is replaceable by tests.
var readLoad = loadavg.Avg1

func main() {
	if err := RootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		osExit(1)
	}
}

// buildInfo is replaceable by tests.
var buildInfo = debug.ReadBuildInfo

func version() string {
	if bi, ok := buildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// RootCmd returns the top-level command.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "refbench",
		Short:        "Render, check and file the results of a reference benchmark",
		SilenceUsage: true,
		Version:      version(),
	}
	root.AddCommand(tableCmd(), checkCmd(), issuesCmd(), loadCmd())
	return root
}

func readReport(path string) (*refbench.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return refbench.ReadReport(f)
}

func tableCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "table <perf.json>",
		Short: "Render perf-table.md from perf.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := readReport(args[0])
			if err != nil {
				return err
			}
			b := refbench.Table(rep)
			if out == "" {
				_, err = cmd.OutOrStdout().Write(b)
				return err
			}
			return os.WriteFile(out, b, 0o644)
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write the table to this file (default: stdout)")
	return cmd
}

func checkCmd() *cobra.Command {
	var table string
	cmd := &cobra.Command{
		Use:   "check <perf.json>",
		Short: "Check that perf-table.md is what perf.json generates",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := readReport(args[0])
			if err != nil {
				return err
			}
			if table == "" {
				table = filepath.Join(filepath.Dir(args[0]), "perf-table.md")
			}
			got, err := os.ReadFile(table)
			if err != nil {
				return err
			}
			if string(got) != string(refbench.Table(rep)) {
				return fmt.Errorf("%s is not what %s generates (run: refbench table -o %s %s)", table, args[0], table, args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s matches %s\n", table, args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&table, "table", "", "the table to check (default: perf-table.md beside perf.json)")
	return cmd
}

func issuesCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "issues <perf.json>",
		Short: "Draft one issue per slower row (drafts only: nothing is filed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := readReport(args[0])
			if err != nil {
				return err
			}
			issues, err := refbench.Issues(rep)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, is := range issues {
				if out == "" {
					fmt.Fprintf(w, "title: %s\nlabels: %v\n\n%s\n---\n", is.Title, is.Labels, is.Body)
					continue
				}
				// The body file is for `gh issue create --body-file`; the title goes in a sibling file.
				base := filepath.Join(out, refbench.Slug(is.Case))
				if err := os.WriteFile(base+".title.txt", []byte(is.Title+"\n"), 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(base+".md", []byte(is.Body), 0o644); err != nil {
					return err
				}
			}
			fmt.Fprintf(w, "%d issue draft(s)\n", len(issues))
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write <case>.title.txt and <case>.md into this existing directory (default: stdout)")
	return cmd
}

func loadCmd() *cobra.Command {
	var max float64
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Print the 1-minute load; fail at or above --max (the gate)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := readLoad()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%.2f\n", l)
			if l >= max {
				return fmt.Errorf("load %.2f is not below %.2f: timing is deferred", l, max)
			}
			return nil
		},
	}
	cmd.Flags().Float64Var(&max, "max", refbench.DefaultMaxLoad, "the gate")
	return cmd
}
