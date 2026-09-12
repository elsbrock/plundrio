package main

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestStalledTransferTimeoutFlagWiring(t *testing.T) {
	// CLI initialization owns package globals. Keep mutations in a separate
	// process so this test can run alongside parallel tests safely.
	if os.Getenv("PLUNDRIO_TEST_STALL_FLAG") != "1" {
		t.Parallel()
		cmd := exec.Command(os.Args[0], "-test.run=^TestStalledTransferTimeoutFlagWiring$")
		cmd.Env = append(os.Environ(), "PLUNDRIO_TEST_STALL_FLAG=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("flag wiring subprocess: %v\n%s", err, output)
		}
		return
	}

	flag := runCmd.Flags().Lookup("stalled-transfer-timeout")
	if flag == nil {
		t.Fatal("stalled-transfer-timeout flag is not registered")
	}

	if got, want := flag.DefValue, defaultStalledTransferTimeout.String(); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	if err := runCmd.Flags().Set("stalled-transfer-timeout", "30m"); err != nil {
		t.Fatalf("set stalled-transfer-timeout flag: %v", err)
	}
	if err := viper.BindPFlags(runCmd.Flags()); err != nil {
		t.Fatalf("bind run command flags: %v", err)
	}

	if got, want := viper.GetDuration(stalledTransferTimeoutKey), 30*time.Minute; got != want {
		t.Errorf("configured timeout = %s, want %s", got, want)
	}
}
