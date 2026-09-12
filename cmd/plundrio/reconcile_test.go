package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/plundrio/internal/reconcile"
	"github.com/spf13/cobra"
)

func TestWriteDeleteReportPreservesJSONOnPartialFailure(t *testing.T) {
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	report := reconcile.DeleteReport{
		SchemaVersion: reconcile.SchemaVersion,
		Results:       []reconcile.DeleteResult{{ID: "putio:2", Source: "putio", Status: "failed", Error: "denied"}},
		Summary:       reconcile.DeleteSummary{SelectedCount: 1, Failed: 1},
	}

	err := writeDeleteReport(cmd, report)
	var partial partialDeleteError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want partialDeleteError", err)
	}
	var decoded reconcile.DeleteReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not complete JSON: %v\n%s", err, output.String())
	}
	if decoded.Results[0].Error != "denied" {
		t.Fatalf("decoded result = %+v", decoded.Results[0])
	}
}

func TestReconcileCategorySettingMatchesRun(t *testing.T) {
	for _, source := range []string{"default", "flag", "environment", "config"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PLDR_TOKEN", "test-token")
			t.Setenv("PLDR_USE_CATEGORIES_PUTIO", "false")
			cmd := newReconcileCmd()
			args := []string{"report", "--target", root}
			switch source {
			case "flag":
				args = append(args, "--use-categories-putio")
			case "environment":
				t.Setenv("PLDR_USE_CATEGORIES_PUTIO", "true")
			case "config":
				// An empty environment value does not override the configuration file.
				t.Setenv("PLDR_USE_CATEGORIES_PUTIO", "")
				config := filepath.Join(root, "config.yaml")
				if err := os.WriteFile(config, []byte("use-categories-putio: true\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--config", config)
			}
			var got reconcileConfig
			for _, child := range cmd.Commands() {
				if child.Name() == "report" {
					child.RunE = func(command *cobra.Command, _ []string) error {
						var err error
						got, err = loadReconcileConfig(command)
						return err
					}
				}
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.useCategoriesPutio != (source != "default") {
				t.Fatalf("%s category setting = %v", source, got.useCategoriesPutio)
			}
		})
	}
}
