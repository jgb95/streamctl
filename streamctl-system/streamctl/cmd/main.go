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
		listen     = flag.String("listen", ":8080", "address to listen on")
		dbPath     = flag.String("db", "/var/lib/streamctl/streamctl.db", "path to SQLite database")
		videoDir   = flag.String("video-dir", "/var/lib/streamctl/videos", "directory containing video files")
		unitDir    = flag.String("unit-dir", "/etc/systemd/system", "directory for generated systemd units")
		unitPrefix = flag.String("unit-prefix", "streamctl-", "prefix for generated systemd unit names")
		runUser    = flag.String("run-user", "streamctl", "user to run streams as")
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
		UnitDir:    *unitDir,
		UnitPrefix: *unitPrefix,
		RunUser:    *runUser,
		VideoDir:   *videoDir,
	}

	h := &handlers.Handler{
		DB:       database,
		Secret:   secret,
		VideoDir: *videoDir,
		Systemd:  sysd,
	}

	mux := http.NewServeMux()
	h.Register(mux)

	log.Printf("streamctl listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}
