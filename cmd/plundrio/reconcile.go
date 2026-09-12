package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/reconcile"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type reconcileAPI interface {
	reconcile.Client
	Authenticate(context.Context) error
}

func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile Put.io and local download objects",
	}

	cmd.PersistentFlags().String("config", "", "Config file")
	cmd.PersistentFlags().StringP("target", "t", "", "Local download root (required)")
	cmd.PersistentFlags().StringP("folder", "f", "plundrio", "Put.io download folder name")
	cmd.PersistentFlags().StringP("token", "k", "", "Put.io OAuth token (required)")
	cmd.PersistentFlags().Bool("use-categories-putio", false, "Include direct Put.io category folders, matching run")

	cmd.AddCommand(&cobra.Command{
		Use:   "report",
		Short: "Print a read-only JSON report of active and unmanaged objects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadReconcileConfig(cmd)
			if err != nil {
				return err
			}

			service, err := openReconcileService(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			report, err := service.Reconcile(cmd.Context())
			if err != nil {
				return err
			}
			return writeJSON(cmd, report)
		},
	})

	deleteCmd := &cobra.Command{
		Use:   "delete",
		Short: "Dry-run or apply deletion of explicitly selected unmanaged IDs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadReconcileConfig(cmd)
			if err != nil {
				return err
			}
			service, err := openReconcileService(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			ids, _ := cmd.Flags().GetStringArray("id")
			putioSelected, _ := cmd.Flags().GetBool("putio")
			localSelected, _ := cmd.Flags().GetBool("local")
			apply, _ := cmd.Flags().GetBool("apply")
			report, err := service.Delete(cmd.Context(), reconcile.DeleteOptions{
				IDs: ids, Putio: putioSelected, Local: localSelected, Apply: apply,
			})
			if err != nil {
				return err
			}
			return writeDeleteReport(cmd, report)
		},
	}
	deleteCmd.Flags().StringArray("id", nil, "Report object ID to select (repeatable, required)")
	deleteCmd.Flags().Bool("putio", false, "Select Put.io objects for deletion")
	deleteCmd.Flags().Bool("local", false, "Select local objects for deletion")
	deleteCmd.Flags().Bool("apply", false, "Apply deletion; omitted means dry-run")
	cmd.AddCommand(deleteCmd)

	return cmd
}

type partialDeleteError struct{}

func (partialDeleteError) Error() string {
	return "one or more selected objects were skipped or failed"
}

func writeDeleteReport(cmd *cobra.Command, report reconcile.DeleteReport) error {
	if err := writeJSON(cmd, report); err != nil {
		return err
	}
	if report.HasFailures() {
		return partialDeleteError{}
	}
	return nil
}

type reconcileConfig struct {
	target             string
	folder             string
	token              string
	useCategoriesPutio bool
}

func loadReconcileConfig(cmd *cobra.Command) (reconcileConfig, error) {
	v := viper.New()
	v.SetEnvPrefix("PLDR")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return reconcileConfig{}, fmt.Errorf("bind reconciliation flags: %w", err)
	}

	configFile := v.GetString("config")
	if configFile != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return reconcileConfig{}, fmt.Errorf("read config file %q: %w", configFile, err)
		}
	}

	cfg := reconcileConfig{
		target:             v.GetString("target"),
		folder:             strings.ToLower(v.GetString("folder")),
		token:              v.GetString("token"),
		useCategoriesPutio: v.GetBool("use-categories-putio"),
	}
	if cfg.target == "" || cfg.folder == "" || cfg.token == "" {
		return reconcileConfig{}, fmt.Errorf("target, folder, and token are required")
	}
	if info, err := os.Stat(cfg.target); err != nil {
		return reconcileConfig{}, fmt.Errorf("stat local download root: %w", err)
	} else if !info.IsDir() {
		return reconcileConfig{}, fmt.Errorf("local download root %q is not a directory", cfg.target)
	}
	return cfg, nil
}

func resolveReconcileFolder(ctx context.Context, client reconcileAPI, folderName string) (int64, error) {
	if err := client.Authenticate(ctx); err != nil {
		return 0, err
	}
	files, err := client.GetFiles(ctx, 0)
	if err != nil {
		return 0, fmt.Errorf("list Put.io root: %w", err)
	}
	for _, file := range files {
		if file.Name == folderName && file.IsDir() {
			return file.ID, nil
		}
	}
	return 0, fmt.Errorf("Put.io folder %q does not exist", folderName)
}

func openReconcileService(ctx context.Context, cfg reconcileConfig) (*reconcile.Service, error) {
	client := api.NewClient(cfg.token)
	folderID, err := resolveReconcileFolder(ctx, client, cfg.folder)
	if err != nil {
		return nil, err
	}
	return reconcile.New(client, folderID, cfg.target, cfg.useCategoriesPutio), nil
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

var _ reconcileAPI = (*api.Client)(nil)
