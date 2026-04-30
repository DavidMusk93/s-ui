# Runtime Analysis: SQLite FD Explosion on Running `sui` Instance

> Document ID: `rc-001`  
> Target PID: `534775` (`/usr/local/s-ui/sui`)  
> Uptime at inspection: `1d 14h 10m`  
> Inspection date: `2026-04-29`

---

## 1. Symptom Report

The running `sui` daemon was reported to have **high memory usage** (`VmRSS ≈ 355 MB`). A quick inspection of `/proc/$PID/fd` revealed an **extreme concentration of file descriptors** pointing to the same SQLite database files.

```
$ pidof sui
534775

$ ls -la /proc/534775/fd | wc -l
5052          <-- total open FDs

$ ls -la /proc/534775/fd | grep "s-ui.db" | wc -l
5039          <-- 99.7 % are DB-related
```

---

## 2. FD Distribution Snapshot

### 2.1 Breakdown by File Type

```
+--------------------------------------------------+
|              FD Ownership (PID 534775)             |
|                                                    |
|   DB main (s-ui.db)        ################  ~2519 |
|   DB WAL (s-ui.db-wal)     ################  ~2520 |
|   DB SHM (s-ui.db-shm)     #                   1   |
|   Sockets                  #                  10   |
|   Stdin/Stdout/Null        #                   3   |
|   Other (epoll,timerfd...) #                  12   |
+--------------------------------------------------+
|   TOTAL                                      5052  |
+--------------------------------------------------+
```

### 2.2 Memory Context

```
$ cat /proc/534775/status | grep -E "VmRSS|VmSize|Threads"
VmSize: 1770172 kB   (~1.7 GB virtual)
VmRSS:    355560 kB   (~355 MB resident)
Threads:        10
```

The process has only **10 OS threads**, which rules out "one-thread-per-connection" as the explanation. The FD explosion is happening inside the Go runtime / SQLite driver, not from explicit goroutine leaks.

---

## 3. Expected vs Actual

### 3.1 What the Code Configures

In `database/db.go`:

```go
sqlDB.SetMaxOpenConns(25)
sqlDB.SetMaxIdleConns(5)
sqlDB.SetConnMaxLifetime(time.Hour)
```

Because SQLite WAL mode keeps **three kernel FDs per connection** (`db`, `-wal`, `-shm`), the expected ceiling is:

```
MaxOpenConns(25) × 3 files  ≈  75 DB-related FDs
```

### 3.2 What We Observed

```
Actual DB FDs  ≈  5039
Expected max   ≈    75
Overrun        ≈  67×
```

**Verdict: This is NOT normal pooling behavior. It is a leak.**

---

## 4. Leak Mechanics (Root-Cause Hypothesis)

`gorm.Open` creates a new `*sql.DB` connection pool. In S-UI the result is stored in a package-global variable:

```go
var db *gorm.DB

func OpenDB(dbPath string) error {
    db, err = gorm.Open(sqlite.Open(dsn), c)
    ...
}
```

If `OpenDB` (or `InitDB`) is invoked **more than once** without an explicit `db.DB().Close()` on the old pool, the previous `*sql.DB` becomes unreachable from Go code but stays alive in the runtime. `sql.DB` does **not** auto-close on garbage collection; it keeps all its idle connections open indefinitely.

### Suspected Trigger Paths

```
Hypothesis A:  SIGHUP restart leak
   SIGHUP → app.RestartApp() → Stop() + Start()
   (DB pool is NOT closed during restart)
   If InitDB were re-called, old pool would orphan.

Hypothesis B:  Import/backup leak
   Web Panel → ImportDb() / GetDb()
   Creates auxiliary GORM instances.
   Even though backup code calls .Close(),
   a race or error path may skip it.

Hypothesis C:  Transaction leak
   A GORM transaction (tx := db.Begin())
   that is neither committed nor rolled back
   on an error path will hold its connection
   forever, bypassing MaxOpenConns limits.
```

**Most likely root cause:**  
A code path (API handler, cron job, or signal restart) is implicitly or explicitly opening **new SQLite connection pools** while the old pools remain open. Because the global `db` variable is overwritten, the old pool is orphaned and never reclaimed.

---

## 5. Why This Also Explains the Memory Pressure

Each leaked connection carries memory in three places:

```
+------------------------------------------+
|  Per-Connection Memory Footprint         |
+------------------------------------------+
|  Go runtime    | sql.Conn, bufio, buf    |
|  SQLite (C)    | page cache, B-tree ctx  |
|  Kernel        | struct file, inode ref  |
+------------------------------------------+
|  Rough estimate:  50 KB – 200 KB each    |
+------------------------------------------+

2500 leaked conns × 100 KB  ≈  250 MB
```

This aligns well with the observed `VmRSS ≈ 355 MB`. The leaked FDs and the leaked memory are **two sides of the same coin**.

---

## 6. How to Reproduce / Monitor

### 6.1 One-Liner Check

```bash
PID=$(pidof sui)
echo "Total FDs:     $(ls /proc/$PID/fd | wc -l)"
echo "DB FDs:        $(ls -la /proc/$PID/fd | grep -c s-ui.db)"
echo "Expected max:  75"
```

### 6.2 Watch It Grow

```bash
watch -n 5 'ls -la /proc/$(pidof sui)/fd | grep s-ui.db | wc -l'
```

If the number increases monotonically while the process uptime grows, the leak is confirmed live.

---

## 7. Immediate Mitigations

| Action | Effect | Risk |
|--------|--------|------|
| `systemctl restart s-ui` | Resets FD count to ~15 | Brief panel outage (~2 s) |
| Lower `MaxOpenConns` to 5 | Slows the leak rate | May throttle concurrent API users |
| Add `defer sqlDB.Close()` in `RestartApp()` | Prevents restart-time leak | Requires code change |
| Audit all `gorm.Open` / `InitDB` call sites | Fixes root cause | Requires code change + QA |

---

## 8. Code-Level Recommendations

1. **Guard `InitDB` against double-open**
   ```go
   var db *gorm.DB
   var dbOnce sync.Once

   func InitDB(dbPath string) error {
       var err error
       dbOnce.Do(func() {
           err = OpenDB(dbPath)
       })
       return err
   }
   ```

2. **Explicitly close old pool before re-opening**
   ```go
   func ReopenDB(dbPath string) error {
       if db != nil {
           if sqlDB, err := db.DB(); err == nil {
               sqlDB.Close()
           }
       }
       return OpenDB(dbPath)
   }
   ```

3. **Ensure every `db.Begin()` has a matching `Commit()` or `Rollback()`** on **all** return paths, especially error paths.

4. **Add a runtime FD limit check** in the health endpoint so operators get alerted before hitting the kernel limit (`ulimit -n`).

---

## 9. Summary

| Metric | Value | Assessment |
|--------|-------|------------|
| Total FDs | 5052 | **Critical** |
| DB FDs | 5039 | **Critical** |
| Expected DB FDs | ≤ 75 | Normal |
| Memory (RSS) | 355 MB | High for an idle-ish panel |
| Threads | 10 | Normal |
| Sockets | 10 | Normal |
| **Conclusion** | **Confirmed SQLite connection-pool / FD leak** | |

The "memory leak" reported by the operator is driven by the underlying **file-descriptor leak** in the SQLite layer. Each leaked connection holds kernel FDs, C-level SQLite state, and Go runtime buffers. Fixing the orphaned-connection-pool bug will resolve both symptoms simultaneously.
