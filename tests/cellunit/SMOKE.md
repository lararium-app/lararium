# SMOKE.md — Live smoke test for T4 cell-core

Run these commands on a bench system with root access.

## Prerequisites

- Root access (euid 0)
- systemd >= 254, systemd-nspawn, systemd-machined running
- cgroup2 unified hierarchy
- subuid/subgid ranges for root
- Kernel >= 5.19
- debootstrap installed

## Commands

```bash
# Build the binary
go build -o /tmp/cell ./cmd/cell

# 1. Doctor — verify environment
/tmp/cell doctor

# 2. Build template (requires root, takes a few minutes)
/tmp/cell build-template

# 3. Create a test cell
/tmp/cell create demo

# 4. Start the cell
/tmp/cell start demo

# 5. Run a command inside the cell
/tmp/cell run demo -- echo "hello from cell"

# 6. Verify uid_map was recorded
cat /var/lib/lararium/cells/demo/cell.json
# Should show subuid_base > 0 (not 0)

# 7. Check status
/tmp/cell status demo
# Should show: running, mounted=true

# 8. List cells
/tmp/cell list
# Should show: demo

# 9. Stop the cell
/tmp/cell stop demo

# 10. Verify stopped
/tmp/cell status demo
# Should show: stopped, mounted=false

# 11. Start again (idempotent check)
/tmp/cell start demo
/tmp/cell run demo -- id
# Should show uid=1000(keeper)

# 12. Snapshot
/tmp/cell snapshot demo

# 13. Destroy
/tmp/cell destroy demo

# 14. Verify destroyed
/tmp/cell status demo
# Should error: cell not found

# 15. Create again (verify rebuild works)
/tmp/cell create demo
/tmp/cell start demo
/tmp/cell run demo -- echo "rebuild works"
/tmp/cell stop demo
/tmp/cell destroy demo
```

## Expected results

- All commands exit 0 (except status after destroy, which exits 1 with "cell not found")
- `cell.json` shows non-zero `subuid_base` after start
- `cell run` output matches the echo command
- Destroy removes all cell artifacts
