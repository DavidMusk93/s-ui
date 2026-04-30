# File Descriptor Management in S-UI

> This document explains how **S-UI** acquires, wraps, and releases file descriptors (FDs) across its subsystems: web servers, subscription servers, the sing-box proxy core, SQLite, and logging. All diagrams use ASCII art.

---

## 1. Overview: Who Owns FDs?

```
+-------------------------------------------------------------+
|                        S-UI Process                         |
|  +-------------------+  +-------------------+               |
|  |   Web Server      |  |  Sub Server       |               |
|  |  (panel :2095)    |  |  (sub   :2096)    |               |
|  |  TCP Listener FD  |  |  TCP Listener FD  |               |
|  +---------+---------+  +---------+---------+               |
|            |                      |                         |
|  +---------v---------+  +---------v---------+               |
|  | TLS Listener FD   |  | TLS Listener FD   |  optional    |
|  +---------+---------+  +---------+---------+               |
|            |                      |                         |
|  +---------v----------------------v---------+               |
|  |           HTTP Conns (per-request FDs)   |               |
|  +------------------------------------------+               |
|                                                             |
|  +------------------------------------------+               |
|  |         sing-box Core (Box)              |               |
|  |  Inbound/Outbound/Endpoint listeners     |               |
|  |  Routed TCP/UDP Conn FDs                 |               |
|  +------------------------------------------+               |
|                                                             |
|  +-------------------+  +-------------------+               |
|  |   SQLite (WAL)    |  |   Log File        |               |
|  |  db + wal + shm   |  |  (optional)       |               |
|  +-------------------+  +-------------------+               |
+-------------------------------------------------------------+
```

---

## 2. Web & Subscription Server Listeners

Both servers follow the same pattern in `web/web.go` and `sub/sub.go`.

### 2.1 Startup (FD Acquisition)

```
   net.Listen("tcp", ":2095")
          |
          v
   +-------------+
   | TCP Listener|  <-- kernel FD allocated
   +------+------+
          |
   [TLS configured?]
          |
    +-----+-----+
    |YES        |NO
    v           v
+---------+  +--------+
|tls.New  |  | plain  |
|Listener |  | TCP    |
|(wraps)  |  |        |
+----+----+  +----+---+
     |            |
     |  +---------+
     |  |
     v  v
  +------------------+
  |  AutoHttpsListener|  optional: unwrap HTTP-on-HTTPS
  +------------------+
     |
     v
  http.Server.Serve(listener)
     |
     v
  go routine accepting conn FDs
```

### 2.2 Shutdown (FD Release)

```
Stop()
  |
  +---> http.Server.Shutdown(ctx, 30s)
  |           |
  |           +---> graceful: close idle conns
  |           +---> wait for handlers to finish
  |
  +---> on error or fallback:
  |           |
  |           +---> listener.Close()  <-- kernel FD released
  |
  +---> context.Cancel()
```

Key points:
- `Shutdown` is preferred; it releases the **listener FD** and closes active **connection FDs** gracefully.
- If `Shutdown` times out, `listener.Close()` is forced.
- The wrapped `AutoHttpsListener` delegates `Close()` to the underlying TCP listener.

---

## 3. Auto-HTTPS Connection Redirect (Per-Request FD Dance)

When TLS is enabled but a client sends plain HTTP, `network/auto_https_conn.go` handles the single connection FD before it reaches the TLS layer.

```
TCP Accept
   |
   v
+-----------------+
| AutoHttpsConn   |  wraps net.Conn (holds client FD)
|  firstBuf [2KB] |
+--------+--------+
         |
         v
   Read(firstBuf)  <-- peeks first bytes on the FD
         |
         v
   [Valid HTTP request?]
         |
   +-----+-----+
   |YES        |NO
   v           v
Write 307    Return firstBuf
Redirect     to TLS layer
+ Close()    (normal HTTPS)
   |
   v
Conn FD closed by kernel
```

Why it matters for FD management:
- The connection FD is **closed inside the wrapper** (`c.Conn.Close()`) when plain HTTP is detected.
- No FD leak occurs because the wrapper does not return the connection upward; the redirect response is written synchronously.

---

## 4. sing-box Core Lifecycle

`core/box.go` implements a custom `Box` that mirrors `sing-box` but adds S-UI trackers. Close order is strictly reverse of startup to avoid use-after-close of shared FDs.

### 4.1 Close Order (FD Release Sequence)

```
Box.Close()
   |
   |   (1) close(s.done)  -- signals goroutines to exit
   |
   +---> service.Close()
   +---> endpoint.Close()
   +---> inbound.Close()     <-- releases proxy listen FDs
   +---> outbound.Close()    <-- closes dialer pools / idle conns
   +---> router.Close()
   +---> connection.Close()  <-- closes tracked routed conns
   +---> dns-router.Close()
   +---> dns-transport.Close()
   +---> network.Close()
   |
   +---> internal services (Clash API, V2Ray API, cache-file)
   +---> logFactory.Close()  <-- closes log file FD
   |
   +---> statsTracker.Reset()
   +---> connTracker.Reset() <-- force-closes any remaining conn FDs
```

Each step uses `defer recover()` so a panic in one subsystem does not prevent subsequent FDs from being closed.

---

## 5. Connection Tracking & Inbound Restart

`core/tracker_conn.go` wraps every routed TCP/UDP connection to allow **scoped closure** when an inbound is reconfigured.

### 5.1 Wrap & Track

```
Inbound Accept
   |
   v
ConnTracker.RoutedConnection(conn, metadata)
   |
   +---> uuid.NewV4() = connID
   |
   +---> map[connID] = &ConnectionInfo{
   |         Conn:    conn,
   |         Inbound: "vmess-in",
   |         Type:    "tcp"
   |     }
   |
   +---> return &wrappedConn{
            Conn:   conn,
            connID: connID
         }
```

### 5.2 Automatic Untrack (Normal Close)

```
wrappedConn.Read/Write/Close
   |
   +---> detects EOF or non-temporary net.Error
   |       |
   |       +---> sync.Once --> untrackConnection(connID)
   |               |
   |               +---> delete(map, connID)
   |
   +---> underlying Conn.Close()
```

### 5.3 Bulk Close on Inbound Restart

```
RestartInbounds(tag="vmess-in")
   |
   +---> corePtr.RemoveInbound(tag)
   |
   +---> corePtr.GetInstance()
   |       .ConnTracker()
   |       .CloseConnByInbound(tag)
   |
   |           iterate map
   |               |
   |       [inbound == tag?]
   |               |
   |          +----+----+
   |          |YES      |NO
   |          v         v
   |     conn.Close()   skip
   |     packetConn.Close()
   |     delete(map, id)
   |
   +---> corePtr.AddInbound(newConfig)
```

This ensures old client FDs tied to a reloaded inbound are evicted before the new listener starts.

---

## 6. SQLite Database FDs

`database/db.go` opens SQLite via GORM with WAL (Write-Ahead Logging). WAL mode keeps three file descriptors:

```
OpenDB(path)
   |
   v
DSN = path + "?_busy_timeout=10000&_journal_mode=WAL"
   |
   v
+--------------------------------+
|  SQLite DB File Descriptor     |  main data file
|  SQLite WAL File Descriptor    |  `-wal` journal
|  SQLite SHM File Descriptor    |  `-shm` shared memory
+--------------------------------+
   |
   +---> SetMaxOpenConns(25)
   +---> SetMaxIdleConns(5)
   +---> SetConnMaxLifetime(1h)
```

GORM/SQLite connection pooling means:
- Up to **25 concurrent FDs** to the same DB file (connection pool).
- Idle connections are reused; old ones are closed after 1 hour.
- WAL checkpoint (`PRAGMA wal_checkpoint`) runs every 10 minutes via cronjob to truncate the WAL file and reduce disk FD pressure.

### 6.1 Backup & Import (Temporary FDs)

```
GetDb()
   |
   +---> create temp.db  (new FD)
   +---> copy all tables
   +---> wal_checkpoint(temp.db)
   +---> close temp.db   (FD released)
   +---> os.Open(temp.db)
   +---> read bytes
   +---> os.Remove(temp.db)  <-- file deleted, FD gone

ImportDB(upload)
   |
   +---> close old DB pool   (all DB FDs released)
   +---> write upload -> temp.db
   +---> validate temp.db
   +---> os.Rename(current.db, current.db.backup)
   +---> os.Rename(temp.db, current.db)
   +---> reopen DB           (new FDs allocated)
   +---> remove backup file
```

---

## 7. Log File Descriptor

`core/log.go` creates an optional file-backed log factory for sing-box.

```
NewFactory()
   |
   +---> [output == "file/path"?]
              |
              +---> filemanager.OpenFile(path, APPEND|CREATE|WRONLY)
                         |
                         v
                    +-----------+
                    | os.File   |  <-- log file FD
                    +-----------+

Close()
   |
   +---> common.Close(file, subscriber)
              |
              +---> file.Close()  <-- FD released
```

The application-level logger (`logger/logger.go`) writes to **syslog** (Unix domain socket FD managed by the OS/syslog daemon) or **stderr** (inherited FD 2, never closed by the application).

---

## 8. Application-Level Start / Stop / Restart

`app/app.go` coordinates all subsystems. The FD lifecycle at the process level:

### 8.1 Start

```
Init()
   |
   +---> logger.InitLogger()        (syslog/stderr FD ready)
   +---> database.InitDB()          (SQLite FDs ready)
   +---> core.NewCore()             (no FDs yet)
   +---> cronjob.NewCronJob()
   +---> web.NewServer()
   +---> sub.NewServer()

Start()
   |
   +---> cronJob.Start()            (timer goroutines, no net FDs)
   +---> webServer.Start()          (+1 TCP listener FD)
   +---> subServer.Start()          (+1 TCP listener FD)
   +---> configService.StartCore()  (sing-box inbounds/outbounds FDs)
```

### 8.2 Stop (Signal-Triggered)

```
SIGTERM / SIGINT
   |
   v
app.Stop()
   |
   +---> cronJob.Stop()             (goroutines exit)
   +---> subServer.Stop()           (sub listener FD closed)
   +---> webServer.Stop()           (web listener FD closed)
   +---> configService.StopCore()   (all sing-box FDs closed)
   |
   +---> process exits
   +---> kernel reclaims all remaining FDs
```

### 8.3 Restart (SIGHUP)

```
SIGHUP
   |
   v
app.RestartApp()
   |
   +---> app.Stop()     (close everything, see 8.2)
   |
   +---> app.Start()    (re-open listeners, restart core)
   |
   Note: DB is NOT closed on restart;
         SQLite FDs stay open across SIGHUP.
```

---

## 9. Summary Checklist for FD Leak Prevention

| Subsystem | FD Owner | Close Path | Safe Guards |
|-----------|----------|------------|-------------|
| Web listener | `web.Server` | `Stop()` -> `Shutdown()` -> `listener.Close()` | 30s timeout, fallback close |
| Sub listener | `sub.Server` | same as web | same as web |
| Auto-HTTPS conn | `AutoHttpsConn` | `readRequest()` -> `c.Conn.Close()` | always closed on HTTP detection |
| sing-box inbounds | `Box.inbound` | `Box.Close()` -> `inbound.Close()` | panic recover per subsystem |
| sing-box outbounds | `Box.outbound` | `Box.Close()` -> `outbound.Close()` | panic recover per subsystem |
| Routed conns | `ConnTracker` | `Reset()` or `CloseConnByInbound()` | wrappedConn untracks on I/O error |
| SQLite main | `sql.DB` pool | `old_db.Close()` on import | max 25 open, 1h lifetime |
| SQLite backup | temporary | `bdb.Close()` then `os.Remove()` | deferred cleanup |
| Log file | `defaultFactory` | `Close()` -> `file.Close()` | `common.Close()` helper |

---

## 10. Edge Cases

1. **Double Close**: `Box.Close()` uses `select { case <-s.done: return nil; default: close(s.done) }` to make subsequent closes no-ops.
2. **Panic During Close**: Every close item in `Box.Close()` runs inside `defer recover()`, ensuring one panicking subsystem does not orphan FDs in another.
3. **Restart Without DB Reopen**: `app.RestartApp()` does **not** close the SQLite pool, so WAL state and connection FDs are preserved across SIGHUP.
4. **Import Race**: `ImportDB()` explicitly closes the old `sql.DB` pool before renaming files, ensuring no lingering FDs block the file move on Unix.
