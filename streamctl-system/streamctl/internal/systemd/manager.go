package systemd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"streamctl/internal/db"
)

// Manager generates systemd service + timer units for streams,
// and reloads/enables/disables them via systemctl.
type Manager struct {
	UnitDir      string // e.g. /run/systemd/system
	UnitPrefix   string // e.g. "streamctl-"
	RunUser      string
	VideoDir     string
	CacheDir     string
	Remote       string // rclone remote, e.g. spaces:bucket
	RcloneConfig string
	NotifyEmail  string
	SendmailPath string
	CleanupCache bool
}

func (m *Manager) serviceName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d.service", m.UnitPrefix, streamID)
}

func (m *Manager) timerName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d.timer", m.UnitPrefix, streamID)
}

func (m *Manager) prefetchName(streamID int64) string {
	return fmt.Sprintf("%sprefetch-%d.service", m.UnitPrefix, streamID)
}

func (m *Manager) playlistName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d.playlist", m.UnitPrefix, streamID)
}

func (m *Manager) playlistPath(streamID int64) string {
	return filepath.Join(m.UnitDir, m.playlistName(streamID))
}

// Sync writes (or removes) the unit files for a single stream and reloads systemd.
// If the stream is disabled or has no endpoints, units are removed.
func (m *Manager) Sync(s *db.Stream) error {
	svcPath := filepath.Join(m.UnitDir, m.serviceName(s.ID))
	prefetchPath := filepath.Join(m.UnitDir, m.prefetchName(s.ID))
	timerPath := filepath.Join(m.UnitDir, m.timerName(s.ID))
	plistPath := m.playlistPath(s.ID)

	// Disabled, no endpoints, or no clips → remove and stop.
	if !s.Enabled || len(enabledEndpoints(s.Endpoints)) == 0 || len(s.Videos) == 0 {
		return m.removeUnits(s.ID, svcPath, prefetchPath, timerPath, plistPath)
	}

	if m.needsPrefetch(s) && strings.TrimSpace(m.Remote) == "" {
		return fmt.Errorf("stream has remote clips but no rclone remote configured")
	}

	if err := writeFileMode(plistPath, m.renderPlaylist(s), 0644); err != nil {
		return fmt.Errorf("writing playlist: %w", err)
	}
	if err := writeFile(svcPath, m.renderService(s)); err != nil {
		return fmt.Errorf("writing service: %w", err)
	}
	if m.needsPrefetch(s) {
		if err := writeFile(prefetchPath, m.renderPrefetchService(s)); err != nil {
			return fmt.Errorf("writing prefetch service: %w", err)
		}
	} else {
		_ = os.Remove(prefetchPath)
	}
	if err := writeFile(timerPath, m.renderTimer(s)); err != nil {
		return fmt.Errorf("writing timer: %w", err)
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("start", m.timerName(s.ID)); err != nil {
		return err
	}
	if m.needsPrefetch(s) {
		if err := systemctl("start", "--no-block", m.prefetchName(s.ID)); err != nil {
			return fmt.Errorf("starting prefetch: %w", err)
		}
	}
	return nil
}

// Remove tears down units for a stream that's being deleted.
func (m *Manager) Remove(streamID int64) error {
	svcPath := filepath.Join(m.UnitDir, m.serviceName(streamID))
	prefetchPath := filepath.Join(m.UnitDir, m.prefetchName(streamID))
	timerPath := filepath.Join(m.UnitDir, m.timerName(streamID))
	plistPath := m.playlistPath(streamID)
	return m.removeUnits(streamID, svcPath, prefetchPath, timerPath, plistPath)
}

func (m *Manager) removeUnits(streamID int64, svcPath, prefetchPath, timerPath, plistPath string) error {
	// Best-effort: don't fail if units aren't there.
	_ = systemctl("disable", "--now", m.timerName(streamID))
	_ = systemctl("stop", m.prefetchName(streamID))
	_ = systemctl("stop", m.serviceName(streamID))
	_ = os.Remove(svcPath)
	_ = os.Remove(prefetchPath)
	_ = os.Remove(timerPath)
	_ = os.Remove(plistPath)
	return systemctl("daemon-reload")
}

// Status returns "active", "inactive", "failed", or "unknown" for the service.
func (m *Manager) Status(streamID int64) string {
	out, err := exec.Command("systemctl", "is-active", m.serviceName(streamID)).Output()
	if err != nil {
		// is-active returns non-zero for inactive/failed; the output is still useful.
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}

// NextTrigger returns the next scheduled run time (raw systemctl output) or "" if none.
func (m *Manager) NextTrigger(streamID int64) string {
	out, err := exec.Command("systemctl", "show", m.timerName(streamID), "--property=NextElapseRealtime", "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// StartNow runs the stream service immediately, without waiting for the timer.
func (m *Manager) StartNow(streamID int64) error {
	return systemctl("start", m.serviceName(streamID))
}

// Start runs the appropriate service immediately. Remote-backed streams start
// with prefetch; local-only streams start ffmpeg directly.
func (m *Manager) Start(s *db.Stream) error {
	if m.needsPrefetch(s) {
		if err := systemctl("start", m.prefetchName(s.ID)); err != nil {
			return err
		}
	}
	return m.StartNow(s.ID)
}

// Stop terminates a running stream.
func (m *Manager) Stop(streamID int64) error {
	return systemctl("stop", m.serviceName(streamID))
}

// ---------- Unit rendering ----------

func (m *Manager) renderService(s *db.Stream) string {
	teeArg := buildTeeArg(s.Endpoints)
	afterPrefetch := ""
	if m.needsPrefetch(s) {
		afterPrefetch = m.prefetchName(s.ID)
	}

	// Stream keys are embedded in the unit file — file mode 0600 protects them.
	// The playlist is read by the concat demuxer; -safe 0 is required because
	// list entries are absolute paths.
	return fmt.Sprintf(`[Unit]
Description=streamctl: %s
After=network-online.target %s
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStartPre=/bin/sh -ceu %s
ExecStart=/run/current-system/sw/bin/ffmpeg -hide_banner -loglevel warning -re -f concat -safe 0 -i %s -c copy -f tee -map 0:v -map 0:a %s
%s
Restart=no
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ReadOnlyPaths=%s
ReadOnlyPaths=%s
`,
		shellEscape(s.Name),
		afterPrefetch,
		m.RunUser,
		shellQuote(m.renderProbeScript(s)),
		shellQuote(m.playlistPath(s.ID)),
		shellQuote(teeArg),
		m.renderCleanupExec(s),
		m.VideoDir,
		m.CacheDir,
	)
}

func (m *Manager) renderPrefetchService(s *db.Stream) string {
	env := ""
	if strings.TrimSpace(m.RcloneConfig) != "" {
		env = "Environment=RCLONE_CONFIG=" + systemdQuote(m.RcloneConfig) + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=streamctl prefetch: %s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%s
%s
ExecStart=/run/current-system/sw/bin/bash -ceu %s
TimeoutStartSec=1h
StandardOutput=journal
StandardError=journal
`,
		shellEscape(s.Name),
		m.RunUser,
		env,
		shellQuote(m.renderPrefetchScript(s)),
	)
}

func (m *Manager) renderPlaylist(s *db.Stream) string {
	var b strings.Builder
	for _, v := range s.Videos {
		full := m.localClipPath(v)
		b.WriteString("file ")
		b.WriteString(shellQuote(full))
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Manager) renderTimer(s *db.Stream) string {
	persistent := "false"
	if s.ScheduleType == "recurring" {
		persistent = "true"
	}
	return fmt.Sprintf(`[Unit]
Description=streamctl timer: %s

[Timer]
OnCalendar=%s
Unit=%s
Persistent=%s

[Install]
WantedBy=timers.target
`,
		shellEscape(s.Name),
		s.OnCalendar,
		m.serviceName(s.ID),
		persistent,
	)
}

func (m *Manager) renderPrefetchScript(s *db.Stream) string {
	var b strings.Builder
	if strings.TrimSpace(m.NotifyEmail) != "" {
		b.WriteString(m.renderNotifyFunction(s))
		b.WriteString("trap 'rc=$?; notify failure; exit $rc' ERR\n")
	}
	for _, v := range s.Videos {
		if !isRemoteClip(v) {
			continue
		}
		local := m.localClipPath(v)
		b.WriteString("/run/current-system/sw/bin/mkdir -p ")
		b.WriteString(shellQuote(filepath.Dir(local)))
		b.WriteString("\n")
		b.WriteString("/run/current-system/sw/bin/rclone copyto ")
		b.WriteString(shellQuote(strings.TrimRight(m.Remote, "/") + slashForRemote(m.Remote) + v))
		b.WriteString(" ")
		b.WriteString(shellQuote(local))
		b.WriteString("\n")
	}
	b.WriteString(m.renderProbeScript(s))
	if strings.TrimSpace(m.NotifyEmail) != "" {
		b.WriteString("notify success\n")
	}
	return b.String()
}

func (m *Manager) renderNotifyFunction(s *db.Stream) string {
	var b strings.Builder
	b.WriteString("notify() {\n")
	b.WriteString("  status=\"$1\"\n")
	b.WriteString("  subject=\"streamctl prefetch ${status}: ")
	b.WriteString(shellDoubleQuoteContent(s.Name))
	b.WriteString("\"\n")
	b.WriteString("  {\n")
	b.WriteString("    printf 'To: %s\\n' ")
	b.WriteString(shellQuote(m.NotifyEmail))
	b.WriteString("\n")
	b.WriteString("    printf 'Subject: %s\\n' \"$subject\"\n")
	b.WriteString("    printf '\\n'\n")
	b.WriteString("    printf 'Stream: %s\\n' ")
	b.WriteString(shellQuote(s.Name))
	b.WriteString("\n")
	b.WriteString("    printf 'Status: %s\\n' \"$status\"\n")
	b.WriteString("    printf 'Host: %s\\n' \"$(hostname)\"\n")
	b.WriteString("    printf 'Playlist:\\n'\n")
	for _, v := range s.Videos {
		b.WriteString("    printf '  - %s\\n' ")
		b.WriteString(shellQuote(v))
		b.WriteString("\n")
	}
	b.WriteString("  } | ")
	b.WriteString(shellQuote(m.sendmailPath()))
	b.WriteString(" -t || true\n")
	b.WriteString("}\n")
	return b.String()
}

func (m *Manager) renderProbeScript(s *db.Stream) string {
	var b strings.Builder
	for _, v := range s.Videos {
		local := m.localClipPath(v)
		b.WriteString("/run/current-system/sw/bin/ffprobe -v error -show_streams ")
		b.WriteString(shellQuote(local))
		b.WriteString(" >/dev/null\n")
	}
	return b.String()
}

func (m *Manager) renderCleanupExec(s *db.Stream) string {
	if !m.CleanupCache || !m.needsPrefetch(s) {
		return ""
	}
	return "ExecStartPost=/bin/sh -ceu " + shellQuote(m.renderCleanupScript(s))
}

func (m *Manager) renderCleanupScript(s *db.Stream) string {
	var b strings.Builder
	for _, v := range s.Videos {
		if !isRemoteClip(v) {
			continue
		}
		b.WriteString("/run/current-system/sw/bin/rm -f -- ")
		b.WriteString(shellQuote(m.localClipPath(v)))
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Manager) needsPrefetch(s *db.Stream) bool {
	for _, v := range s.Videos {
		if isRemoteClip(v) {
			return true
		}
	}
	return false
}

func (m *Manager) localClipPath(source string) string {
	if isRemoteClip(source) {
		return filepath.Join(m.CacheDir, source)
	}
	return filepath.Join(m.VideoDir, source)
}

func isRemoteClip(source string) bool {
	return strings.Contains(source, "/")
}

func slashForRemote(remote string) string {
	if strings.HasSuffix(remote, "/") || strings.HasSuffix(remote, ":") {
		return ""
	}
	return "/"
}

func buildTeeArg(endpoints []db.Endpoint) string {
	var parts []string
	for _, e := range endpoints {
		if !e.Enabled {
			continue
		}
		url := strings.TrimRight(e.RtmpURL, "/") + "/" + e.StreamKey
		parts = append(parts, fmt.Sprintf("[f=flv:onfail=ignore]%s", url))
	}
	return strings.Join(parts, "|")
}

func enabledEndpoints(eps []db.Endpoint) []db.Endpoint {
	var out []db.Endpoint
	for _, e := range eps {
		if e.Enabled {
			out = append(out, e)
		}
	}
	return out
}

// ---------- helpers ----------

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0600)
}

func writeFileMode(path, contents string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(contents), mode)
}

// shellQuote wraps a value in single quotes for safe inclusion in an ExecStart line.
// systemd does its own quoting; we use this for argv values containing spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellEscape strips problematic chars from human-facing description strings.
func shellEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

func systemdQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func shellDoubleQuoteContent(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "$", `\$`)
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

func (m *Manager) sendmailPath() string {
	if strings.TrimSpace(m.SendmailPath) != "" {
		return m.SendmailPath
	}
	return "/run/current-system/sw/bin/sendmail"
}
