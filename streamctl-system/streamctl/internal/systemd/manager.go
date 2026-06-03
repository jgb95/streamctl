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

type UnitLog struct {
	Label       string
	UnitName    string
	LoadedState string
	ActiveState string
	SubState    string
	Result      string
	Since       string
	Journal     string
	Error       string
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

func (m *Manager) prefetchScriptName(streamID int64) string {
	return fmt.Sprintf("%sprefetch-%d.sh", m.UnitPrefix, streamID)
}

func (m *Manager) probeScriptName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d-probe.sh", m.UnitPrefix, streamID)
}

func (m *Manager) cleanupScriptName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d-cleanup.sh", m.UnitPrefix, streamID)
}

func (m *Manager) playlistPath(streamID int64) string {
	return filepath.Join(m.UnitDir, m.playlistName(streamID))
}

func (m *Manager) prefetchScriptPath(streamID int64) string {
	return filepath.Join(m.UnitDir, m.prefetchScriptName(streamID))
}

func (m *Manager) probeScriptPath(streamID int64) string {
	return filepath.Join(m.UnitDir, m.probeScriptName(streamID))
}

func (m *Manager) cleanupScriptPath(streamID int64) string {
	return filepath.Join(m.UnitDir, m.cleanupScriptName(streamID))
}

// Sync writes (or removes) the unit files for a single stream and reloads systemd.
// If the stream is disabled or has no endpoints, units are removed.
func (m *Manager) Sync(s *db.Stream) error {
	svcPath := filepath.Join(m.UnitDir, m.serviceName(s.ID))
	prefetchPath := filepath.Join(m.UnitDir, m.prefetchName(s.ID))
	timerPath := filepath.Join(m.UnitDir, m.timerName(s.ID))
	plistPath := m.playlistPath(s.ID)
	prefetchScriptPath := m.prefetchScriptPath(s.ID)
	probeScriptPath := m.probeScriptPath(s.ID)
	cleanupScriptPath := m.cleanupScriptPath(s.ID)

	// Disabled, no endpoints, or no clips → remove and stop.
	if !s.Enabled || len(enabledEndpoints(s.Endpoints)) == 0 || len(s.Videos) == 0 {
		return m.removeUnits(s.ID, svcPath, prefetchPath, timerPath, plistPath, prefetchScriptPath, probeScriptPath, cleanupScriptPath)
	}

	if m.needsPrefetch(s) && strings.TrimSpace(m.Remote) == "" {
		return fmt.Errorf("stream has remote clips but no rclone remote configured")
	}

	if err := writeFileMode(plistPath, m.renderPlaylist(s), 0644); err != nil {
		return fmt.Errorf("writing playlist: %w", err)
	}
	if err := writeFileMode(probeScriptPath, m.renderProbeScript(s), 0755); err != nil {
		return fmt.Errorf("writing probe script: %w", err)
	}
	if err := writeFile(svcPath, m.renderService(s)); err != nil {
		return fmt.Errorf("writing service: %w", err)
	}
	if m.needsPrefetch(s) {
		if err := writeFileMode(prefetchScriptPath, m.renderPrefetchScript(s), 0755); err != nil {
			return fmt.Errorf("writing prefetch script: %w", err)
		}
		if err := writeFile(prefetchPath, m.renderPrefetchService(s)); err != nil {
			return fmt.Errorf("writing prefetch service: %w", err)
		}
	} else {
		_ = os.Remove(prefetchPath)
		_ = os.Remove(prefetchScriptPath)
	}
	if m.CleanupCache && m.needsPrefetch(s) {
		if err := writeFileMode(cleanupScriptPath, m.renderCleanupScript(s), 0755); err != nil {
			return fmt.Errorf("writing cleanup script: %w", err)
		}
	} else {
		_ = os.Remove(cleanupScriptPath)
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
	return m.removeUnits(
		streamID,
		svcPath,
		prefetchPath,
		timerPath,
		plistPath,
		m.prefetchScriptPath(streamID),
		m.probeScriptPath(streamID),
		m.cleanupScriptPath(streamID),
	)
}

func (m *Manager) removeUnits(streamID int64, paths ...string) error {
	// Best-effort: don't fail if units aren't there.
	_ = systemctl("disable", "--now", m.timerName(streamID))
	_ = systemctl("stop", m.prefetchName(streamID))
	_ = systemctl("stop", m.serviceName(streamID))
	for _, path := range paths {
		_ = os.Remove(path)
	}
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

func (m *Manager) Logs(streamID int64) []UnitLog {
	units := []UnitLog{
		{Label: "Timer", UnitName: m.timerName(streamID)},
		{Label: "Prefetch", UnitName: m.prefetchName(streamID)},
		{Label: "Stream", UnitName: m.serviceName(streamID)},
	}
	for i := range units {
		unit := &units[i]
		if err := m.populateUnitState(unit); err != nil {
			unit.Error = appendError(unit.Error, err)
		}
		if err := m.populateUnitJournal(unit); err != nil {
			unit.Error = appendError(unit.Error, err)
		}
	}
	return units
}

func (m *Manager) populateUnitState(unit *UnitLog) error {
	out, err := exec.Command(
		"systemctl",
		"show",
		unit.UnitName,
		"--property=LoadedState",
		"--property=ActiveState",
		"--property=SubState",
		"--property=Result",
		"--property=ActiveEnterTimestamp",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadedState":
			unit.LoadedState = val
		case "ActiveState":
			unit.ActiveState = val
		case "SubState":
			unit.SubState = val
		case "Result":
			unit.Result = val
		case "ActiveEnterTimestamp":
			unit.Since = val
		}
	}
	return nil
}

func (m *Manager) populateUnitJournal(unit *UnitLog) error {
	out, err := exec.Command(
		"journalctl",
		"-u", unit.UnitName,
		"--no-pager",
		"--output=short-iso",
		"-n", "200",
	).CombinedOutput()
	unit.Journal = strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("journalctl: %w: %s", err, unit.Journal)
	}
	if unit.Journal == "" {
		unit.Journal = "No journal entries found."
	}
	return nil
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
ExecStartPre=%s
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
ReadWritePaths=%s
`,
		shellEscape(s.Name),
		afterPrefetch,
		m.RunUser,
		systemdQuote(m.probeScriptPath(s.ID)),
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
ExecStart=%s
TimeoutStartSec=1h
StandardOutput=journal
StandardError=journal
`,
		shellEscape(s.Name),
		m.RunUser,
		env,
		systemdQuote(m.prefetchScriptPath(s.ID)),
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
		normalizeOnCalendar(s.OnCalendar),
		m.serviceName(s.ID),
		persistent,
	)
}

func normalizeOnCalendar(onCalendar string) string {
	onCalendar = strings.TrimSpace(onCalendar)
	if len(onCalendar) > len("UTC") && strings.HasSuffix(onCalendar, "UTC") {
		i := len(onCalendar) - len("UTC") - 1
		if i >= 0 && onCalendar[i] != ' ' && onCalendar[i] != '\t' {
			return onCalendar[:i+1] + " UTC"
		}
	}
	return onCalendar
}

func (m *Manager) renderPrefetchScript(s *db.Stream) string {
	var b strings.Builder
	b.WriteString("#!/run/current-system/sw/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("export PATH=/run/current-system/sw/bin:/bin\n")
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
		b.WriteString("if [ -f ")
		b.WriteString(shellQuote(local))
		b.WriteString(" ] && /run/current-system/sw/bin/ffprobe -v error -show_streams ")
		b.WriteString(shellQuote(local))
		b.WriteString(" >/dev/null; then\n")
		b.WriteString("  echo ")
		b.WriteString(shellQuote("prefetch: using cached " + v))
		b.WriteString("\n")
		b.WriteString("else\n")
		b.WriteString("/run/current-system/sw/bin/rclone copyto ")
		b.WriteString(shellQuote(strings.TrimRight(m.Remote, "/") + slashForRemote(m.Remote) + v))
		b.WriteString(" ")
		b.WriteString(shellQuote(local))
		b.WriteString("\n")
		b.WriteString("fi\n")
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
	b.WriteString("#!/run/current-system/sw/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("export PATH=/run/current-system/sw/bin:/bin\n")
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
	return "ExecStartPost=" + systemdQuote(m.cleanupScriptPath(s.ID))
}

func (m *Manager) renderCleanupScript(s *db.Stream) string {
	var b strings.Builder
	b.WriteString("#!/run/current-system/sw/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("export PATH=/run/current-system/sw/bin:/bin\n")
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

func appendError(existing string, err error) string {
	if err == nil {
		return existing
	}
	msg := strings.TrimSpace(err.Error())
	if existing == "" {
		return msg
	}
	return existing + "\n" + msg
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
