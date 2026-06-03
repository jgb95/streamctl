package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"streamctl/internal/db"
	"streamctl/internal/handlers"
	"streamctl/internal/systemd"
)

func main() {
	var (
		listen       = flag.String("listen", ":8080", "address to listen on")
		dbPath       = flag.String("db", "/var/lib/streamctl/streamctl.db", "path to SQLite database")
		videoDir     = flag.String("video-dir", "/var/lib/streamctl/videos", "directory containing video files")
		cacheDir     = flag.String("cache-dir", "/var/lib/streamctl/cache", "directory for prefetched remote video files")
		remote       = flag.String("remote", "", "rclone remote for Spaces objects, e.g. spaces:bucket")
		rcloneCfg    = flag.String("rclone-config", "", "path to rclone config file for generated prefetch services")
		notifyEmail  = flag.String("notify-email", "", "email address for prefetch success/failure notifications")
		sendmailPath = flag.String("sendmail", "/run/current-system/sw/bin/sendmail", "sendmail-compatible binary for notifications")
		cleanupCache = flag.Bool("cleanup-cache", true, "delete cached remote files after successful streaming")
		unitDir      = flag.String("unit-dir", "/etc/systemd/system", "directory for generated systemd units")
		unitPrefix   = flag.String("unit-prefix", "streamctl-", "prefix for generated systemd unit names")
		runUser      = flag.String("run-user", "streamctl", "user to run streams as")
	)
	flag.Parse()

	secret := os.Getenv("STREAMCTL_SECRET")
	if secret == "" {
		log.Fatal("STREAMCTL_SECRET environment variable must be set")
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(); err != nil {
		log.Fatalf("migrating database: %v", err)
	}

	sysd := &systemd.Manager{
		UnitDir:      *unitDir,
		UnitPrefix:   *unitPrefix,
		RunUser:      *runUser,
		VideoDir:     *videoDir,
		CacheDir:     *cacheDir,
		Remote:       *remote,
		RcloneConfig: *rcloneCfg,
		NotifyEmail:  *notifyEmail,
		SendmailPath: *sendmailPath,
		CleanupCache: *cleanupCache,
	}

	streams, err := database.ListStreams()
	if err != nil {
		log.Printf("listing streams for startup sync: %v", err)
	} else {
		for i := range streams {
			if err := sysd.Sync(&streams[i]); err != nil {
				log.Printf("startup sync for stream %d failed: %v", streams[i].ID, err)
			}
		}
	}

	h := &handlers.Handler{
		DB:           database,
		Secret:       secret,
		VideoDir:     *videoDir,
		CacheDir:     *cacheDir,
		Remote:       *remote,
		RcloneConfig: *rcloneCfg,
		Systemd:      sysd,
	}

	mux := http.NewServeMux()
	h.Register(mux)

	log.Printf("streamctl listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}
