package custos_test

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestCPHelperProcess performs a ChangePassphrase and is SIGKILLed at the
// requested CUSTOS kill point. Invoked by runCPSubprocessWithKill.
func TestCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	stateDir := os.Getenv("CUSTOS_STATE_DIR")
	oldPass := os.Getenv("CUSTOS_PASSPHRASE")
	newPass := os.Getenv("CUSTOS_NEW_PASSPHRASE")
	killPoint := os.Getenv("CUSTOS_KILL_POINT")

	custos.SetTestAgeWorkFactor(10)
	custos.SetKillPointHook(func(point string) {
		if point == killPoint {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	})

	v := custos.NewVault(stateDir, 5*time.Second)
	_ = v.ChangePassphrase(oldPass, newPass)
	os.Exit(0)
}

// runCPSubprocessWithKill runs ChangePassphrase in a subprocess that dies
// at killPoint, leaving the on-disk state mid-change.
func runCPSubprocessWithKill(t *testing.T, stateDir, oldPass, newPass, killPoint string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCPHelperProcess", "--") //nolint:gosec // test helper
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"CUSTOS_STATE_DIR="+stateDir,
		"CUSTOS_PASSPHRASE="+oldPass,
		"CUSTOS_NEW_PASSPHRASE="+newPass,
		"CUSTOS_KILL_POINT="+killPoint,
	)
	_ = cmd.Run()
}
