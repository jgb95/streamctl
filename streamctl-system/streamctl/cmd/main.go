package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"streamctl/internal/db"
	"streamctl/internal/handlers"
	"streamctl/internal/nostrpub"
	"streamctl/internal/systemd"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "nostr-publish" {
		if err := runNostrPublish(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	var (
		listen             = flag.String("listen", ":8080", "address to listen on")
		dbPath             = flag.String("db", "/var/lib/streamctl/streamctl.db", "path to SQLite database")
		videoDir           = flag.String("video-dir", "/var/lib/streamctl/videos", "directory containing video files")
		cacheDir           = flag.String("cache-dir", "/var/lib/streamctl/cache", "directory for prefetched remote video files")
		hlsDir             = flag.String("hls-dir", "/var/lib/streamctl/hls", "directory for generated HLS playlists and segments")
		remote             = flag.String("remote", "", "rclone remote for Spaces objects, e.g. spaces:bucket")
		rcloneCfg          = flag.String("rclone-config", "", "path to rclone config file for generated prefetch services")
		notifyEmail        = flag.String("notify-email", "", "email address for prefetch success/failure notifications")
		sendmailPath       = flag.String("sendmail", "/run/current-system/sw/bin/sendmail", "sendmail-compatible binary for notifications")
		cleanupCache       = flag.Bool("cleanup-cache", true, "delete cached remote files after successful streaming")
		normalize          = flag.Bool("normalize-prefetch", true, "re-encode prefetched remote clips to streaming-friendly CBR before streaming")
		videoBitrate       = flag.String("normalize-video-bitrate", "6800k", "video bitrate for normalized prefetched clips")
		audioBitrate       = flag.String("normalize-audio-bitrate", "160k", "audio bitrate for normalized prefetched clips")
		nostrKeyDir        = flag.String("nostr-key-dir", "/var/lib/streamctl/nostr-keys", "directory for stored Nostr private keys")
		publicBaseURL      = flag.String("public-base-url", "", "public base URL used in Nostr live events, e.g. https://stream.example.com")
		gpuWorkerHost      = flag.String("gpu-worker-host", "", "SSH target for GPU transcode worker, e.g. ubuntu@1.2.3.4")
		gpuWorkerCommand   = flag.String("gpu-worker-command", "/root/transcode-nvenc.sh", "command path on GPU worker used to process one Spaces path")
		doTokenFile        = flag.String("do-token-file", "", "DigitalOcean API token file for managed GPU workers")
		gpuDropletName     = flag.String("gpu-droplet-name", "streamctl-gpu-worker", "managed GPU Droplet name")
		gpuDropletRegion   = flag.String("gpu-droplet-region", "nyc2", "managed GPU Droplet region")
		gpuDropletSize     = flag.String("gpu-droplet-size", "gpu-h100x1-80gb", "managed GPU Droplet size slug")
		gpuDropletImage    = flag.String("gpu-droplet-image", "ubuntu-24-04-x64", "managed GPU Droplet image slug")
		gpuSSHKeyName      = flag.String("gpu-ssh-key-name", "streamctl-deploy", "DigitalOcean SSH key name to install on managed GPU Droplets")
		gpuWorkerUser      = flag.String("gpu-worker-user", "root", "SSH user for managed GPU Droplets")
		gpuDestroyAfterJob = flag.Bool("gpu-destroy-after-job", false, "destroy managed GPU worker after a successful transcode job")
		unitDir            = flag.String("unit-dir", "/etc/systemd/system", "directory for generated systemd units")
		unitPrefix         = flag.String("unit-prefix", "streamctl-", "prefix for generated systemd unit names")
		runUser            = flag.String("run-user", "streamctl", "user to run streams as")
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
		UnitDir:       *unitDir,
		UnitPrefix:    *unitPrefix,
		RunUser:       *runUser,
		VideoDir:      *videoDir,
		CacheDir:      *cacheDir,
		HLSDir:        *hlsDir,
		Remote:        *remote,
		RcloneConfig:  *rcloneCfg,
		NotifyEmail:   *notifyEmail,
		SendmailPath:  *sendmailPath,
		CleanupCache:  *cleanupCache,
		Normalize:     *normalize,
		VideoBitrate:  *videoBitrate,
		AudioBitrate:  *audioBitrate,
		NostrKeyDir:   *nostrKeyDir,
		PublicBaseURL: strings.TrimRight(*publicBaseURL, "/"),
		SelfPath:      os.Args[0],
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
		DB:                 database,
		Secret:             secret,
		VideoDir:           *videoDir,
		CacheDir:           *cacheDir,
		HLSDir:             *hlsDir,
		Remote:             *remote,
		RcloneConfig:       *rcloneCfg,
		NostrKeyDir:        *nostrKeyDir,
		NostrKeyOwner:      *runUser,
		GPUWorkerHost:      strings.TrimSpace(*gpuWorkerHost),
		GPUWorkerCommand:   strings.TrimSpace(*gpuWorkerCommand),
		DOTokenFile:        strings.TrimSpace(*doTokenFile),
		GPUDropletName:     strings.TrimSpace(*gpuDropletName),
		GPUDropletRegion:   strings.TrimSpace(*gpuDropletRegion),
		GPUDropletSize:     strings.TrimSpace(*gpuDropletSize),
		GPUDropletImage:    strings.TrimSpace(*gpuDropletImage),
		GPUSSHKeyName:      strings.TrimSpace(*gpuSSHKeyName),
		GPUWorkerUser:      strings.TrimSpace(*gpuWorkerUser),
		GPUDestroyAfterJob: *gpuDestroyAfterJob,
		Systemd:            sysd,
	}

	mux := http.NewServeMux()
	h.Register(mux)

	log.Printf("streamctl listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

func runNostrPublish(args []string) error {
	fs := flag.NewFlagSet("nostr-publish", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "path to stored hex private key")
	relays := fs.String("relays", "", "comma-separated relay URLs")
	status := fs.String("status", "", "planned, live, or ended")
	dTag := fs.String("d", "", "NIP-53 d tag")
	title := fs.String("title", "", "live event title")
	summary := fs.String("summary", "", "live event summary")
	streamingURL := fs.String("streaming", "", "HLS playlist URL")
	starts := fs.Int64("starts", 0, "Unix start timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}
	results, err := nostrpub.PublishLiveEvent(context.Background(), nostrpub.PublishOptions{
		KeyFile:      *keyFile,
		Relays:       strings.Split(*relays, ","),
		Status:       *status,
		DTag:         *dTag,
		Title:        *title,
		Summary:      *summary,
		StreamingURL: *streamingURL,
		Starts:       *starts,
	})
	for _, res := range results {
		if res.Error != nil {
			log.Printf("nostr publish %s failed: %v", res.RelayURL, res.Error)
		} else {
			log.Printf("nostr publish %s ok", res.RelayURL)
		}
	}
	if err != nil {
		return fmt.Errorf("nostr publish: %w", err)
	}
	return nil
}
