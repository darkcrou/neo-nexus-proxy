// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
)

var authCmd = &cobra.Command{
	Use:   "auth qwen-subscription",
	Short: "Sign in to a Qwen Code subscription",
	Args:  cobra.ExactArgs(1),
	RunE:  runAuth,
}

func runAuth(cmd *cobra.Command, args []string) error {
	if args[0] != qwen.ProviderType {
		return fmt.Errorf("use nexus auth %s", qwen.ProviderType)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path := config.DefaultPath()
	service, err := qwen.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer service.Close()
	flow, err := service.BeginLogin(ctx)
	if err != nil {
		return err
	}
	cmd.Printf("Open this link in your browser and approve sign-in:\n%s\nUser code: %s\nWaiting for Qwen approval…\n", flow.URL, flow.UserCode)
	if err := service.FinishLogin(ctx, flow); err != nil {
		return err
	}
	if err := qwen.EnableProvider(path); err != nil {
		return fmt.Errorf("signed in, but Qwen could not be enabled in config.toml: %w", err)
	}
	cmd.Println("Qwen sign-in saved. If this is your first setup, restart NEXUS to load the provider.")
	return nil
}

func checkProviderHealth(pc config.Provider, provider providers.Provider) error {
	if !strings.EqualFold(pc.Type, qwen.ProviderType) {
		return provider.HealthCheck()
	}
	service, err := qwen.Open(filepath.Dir(config.DefaultPath()))
	if err != nil {
		return err
	}
	defer service.Close()
	spec := specFromConfig(pc)
	spec.QwenTokens = service
	provider, err = providers.New(spec)
	if err != nil {
		return err
	}
	return provider.HealthCheck()
}
