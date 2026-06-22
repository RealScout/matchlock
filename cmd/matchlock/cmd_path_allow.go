package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

var pathAllowCmd = &cobra.Command{
	Use:   "path-allow",
	Short: "Manage runtime per-host URL path allow-list for a running sandbox",
}

var pathAllowSetCmd = &cobra.Command{
	Use:   "set <id> <host> <prefix1,prefix2...>",
	Short: "Restrict a host to the given URL path prefixes on a running sandbox",
	Args:  cobra.MinimumNArgs(3),
	RunE:  runPathAllowSet,
}

var pathAllowClearCmd = &cobra.Command{
	Use:   "clear <id> <host>",
	Short: "Remove a host's path restriction on a running sandbox",
	Args:  cobra.ExactArgs(2),
	RunE:  runPathAllowClear,
}

func init() {
	pathAllowCmd.AddCommand(pathAllowSetCmd)
	pathAllowCmd.AddCommand(pathAllowClearCmd)
	rootCmd.AddCommand(pathAllowCmd)
}

func runPathAllowSet(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	host := strings.TrimSpace(args[1])
	prefixes := splitPathAllowPrefixes(args[2:])
	if len(prefixes) == 0 {
		return fmt.Errorf("expected one or more path prefixes (comma-separated)")
	}

	execSocketPath, err := resolveAllowListExecSocket(vmID)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	result, err := sandbox.PathAllowSetViaRelay(ctx, execSocketPath, host, prefixes)
	if err != nil {
		return errx.Wrap(ErrAllowListUpdate, err)
	}

	fmt.Printf("Set path allow-list for %s on %s: %s\n", host, vmID, strings.Join(prefixes, ","))
	fmt.Printf("Current path allow-list: %s\n", formatPathAllow(result.PathAllow))
	return nil
}

func runPathAllowClear(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	host := strings.TrimSpace(args[1])

	execSocketPath, err := resolveAllowListExecSocket(vmID)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	result, err := sandbox.PathAllowClearViaRelay(ctx, execSocketPath, host)
	if err != nil {
		return errx.Wrap(ErrAllowListUpdate, err)
	}

	fmt.Printf("Cleared path restriction for %s on %s\n", host, vmID)
	fmt.Printf("Current path allow-list: %s\n", formatPathAllow(result.PathAllow))
	return nil
}

func splitPathAllowPrefixes(parts []string) []string {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		for _, token := range strings.Split(part, ",") {
			v := strings.TrimSpace(token)
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func formatPathAllow(pathAllow map[string][]string) string {
	if len(pathAllow) == 0 {
		return "(empty: no host path restrictions)"
	}
	hosts := make([]string, 0, len(pathAllow))
	for host := range pathAllow {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	parts := make([]string, 0, len(hosts))
	for _, host := range hosts {
		parts = append(parts, fmt.Sprintf("%s=[%s]", host, strings.Join(pathAllow[host], " ")))
	}
	return strings.Join(parts, "; ")
}
