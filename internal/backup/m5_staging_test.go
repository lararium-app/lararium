package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestM5_PlantedMarkerUntrustedStaging: markers whose staging paths are hostile
// (/home or <parent>/<unrelated-dir>) must be quarantined to .rejected without
// deleting anything per PENATUS-SPEC §4.5.4 (M5).
func TestM5_PlantedMarkerUntrustedStaging(t *testing.T) {
	parent := t.TempDir()
	targetRoot := filepath.Join(parent, "hearth")
	if err := os.Mkdir(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Planted marker with Staging pointing to a simulated "/home" dir
	fakeHome := t.TempDir()
	homeFile := filepath.Join(fakeHome, "precious.txt")
	if err := os.WriteFile(homeFile, []byte("vital data in home"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker1Path := filepath.Join(parent, ".restore-home.json")
	m1 := RestoreMarker{
		Staging: fakeHome,
		Target:  targetRoot,
		TS:      "home",
	}
	m1Bytes, err := json.Marshal(m1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker1Path, m1Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap failed: %v", err)
	}

	// Verify no deletion occurred
	if _, err := os.Stat(homeFile); err != nil {
		t.Fatalf("file in staging /home was deleted: %v", err)
	}
	if _, err := os.Stat(fakeHome); err != nil {
		t.Fatalf("staging /home directory was deleted: %v", err)
	}

	// Verify marker quarantined (renamed to .rejected)
	if _, err := os.Stat(marker1Path); !os.IsNotExist(err) {
		t.Fatalf("original marker %s still exists", marker1Path)
	}
	if _, err := os.Stat(marker1Path + ".rejected"); err != nil {
		t.Fatalf("quarantined marker %s.rejected does not exist: %v", marker1Path, err)
	}

	// 1b. Planted marker with literal Staging="/home"
	markerHomeLiteralPath := filepath.Join(parent, ".restore-literal-home.json")
	mHomeLit := RestoreMarker{
		Staging: "/home",
		Target:  targetRoot,
		TS:      "literal-home",
	}
	mHomeLitBytes, err := json.Marshal(mHomeLit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerHomeLiteralPath, mHomeLitBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap failed: %v", err)
	}
	if _, err := os.Stat(markerHomeLiteralPath + ".rejected"); err != nil {
		t.Fatalf("quarantined marker %s.rejected does not exist: %v", markerHomeLiteralPath, err)
	}

	// 2. Planted marker with Staging pointing to <parent>/<unrelated-dir>
	unrelatedDir := filepath.Join(parent, "unrelated-dir")
	if err := os.Mkdir(unrelatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelatedFile := filepath.Join(unrelatedDir, "critical.txt")
	if err := os.WriteFile(unrelatedFile, []byte("vital data in unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker2Path := filepath.Join(parent, ".restore-unrelated.json")
	m2 := RestoreMarker{
		Staging: unrelatedDir,
		Target:  targetRoot,
		TS:      "unrelated",
	}
	m2Bytes, err := json.Marshal(m2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker2Path, m2Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap failed: %v", err)
	}

	// Verify no deletion occurred
	if _, err := os.Stat(unrelatedFile); err != nil {
		t.Fatalf("file in <parent>/<unrelated-dir> was deleted: %v", err)
	}
	if _, err := os.Stat(unrelatedDir); err != nil {
		t.Fatalf("<parent>/<unrelated-dir> was deleted: %v", err)
	}

	// Verify marker quarantined (renamed to .rejected)
	if _, err := os.Stat(marker2Path); !os.IsNotExist(err) {
		t.Fatalf("original marker %s still exists", marker2Path)
	}
	if _, err := os.Stat(marker2Path + ".rejected"); err != nil {
		t.Fatalf("quarantined marker %s.rejected does not exist: %v", marker2Path, err)
	}

	// 3. Legitimate staging directory IS cleaned up normally (Row 2 recovery: no pre_swap)
	legitStaging := filepath.Join(parent, "hearth.staging-12345")
	if err := os.Mkdir(legitStaging, 0o700); err != nil {
		t.Fatal(err)
	}
	tempPayload := filepath.Join(legitStaging, "temp.bin")
	if err := os.WriteFile(tempPayload, []byte("temp bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker3Path := filepath.Join(parent, ".restore-legit.json")
	m3 := RestoreMarker{
		Staging: legitStaging,
		Target:  targetRoot,
		TS:      "legit",
	}
	m3Bytes, err := json.Marshal(m3)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker3Path, m3Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap failed on legit marker: %v", err)
	}

	// Legitimate staging and marker should be removed per row 2
	if _, err := os.Stat(legitStaging); !os.IsNotExist(err) {
		t.Fatalf("legitimate staging %s was not cleaned up", legitStaging)
	}
	if _, err := os.Stat(marker3Path); !os.IsNotExist(err) {
		t.Fatalf("legitimate marker %s was not removed", marker3Path)
	}
}
