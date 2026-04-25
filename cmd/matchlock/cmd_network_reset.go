package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

var networkResetCmd = &cobra.Command{
	Use:   "network-reset <id>",
	Short: "Rebuild a running sandbox's network stack (recovers wedged DNS)",
	Long: `Rebuild the gVisor network stack of a running sandbox without
restarting the VM. Used to recover from the macOS DNS forwarder wedge where
the matchlock host process has leaked UDP socket FDs and silently dropped
guest DNS queries (see dev-docs/matchlock/dns-death.md).

In-flight TCP connections die and apps must retry. The guest sees no NIC
link bounce: same IP, same MAC, same routes.`,
	Args: cobra.ExactArgs(1),
	RunE: runNetworkReset,
}

func init() {
	rootCmd.AddCommand(networkResetCmd)
}

func runNetworkReset(cmd *cobra.Command, args []string) error {
	vmID := args[0]

	execSocketPath, err := resolveAllowListExecSocket(vmID)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	if err := sandbox.NetworkResetViaRelay(ctx, execSocketPath); err != nil {
		return errx.Wrap(ErrNetworkReset, err)
	}

	fmt.Printf("Network stack reset for %s\n", vmID)
	return nil
}
