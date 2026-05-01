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
	UnitDir    string // e.g. /etc/systemd/system
	UnitPrefix string // e.g. "streamctl-"
	RunUser    string
	VideoDir   string
}

func (m *Manager) serviceName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d.service", m.UnitPrefix, streamID)
}

func (m *Manager) timerName(streamID int64) string {
	return fmt.Sprintf("%sstream-%d.timer", m.UnitPrefix, streamID)
}

// Sync writes (or removes) the unit files for a single stream and reloads systemd.
// If the stream is disabled or has no endpoints, units are removed.
func (m *Manager) Sync(s *db.Stream) error {
	svcPath := filepath.Join(m.UnitDir, m.serviceName(s.ID))
	timerPath := filepath.Join(m.UnitDir, m.timerName(s.ID))

	// Disabled or no endpoints → remove and stop.
	if !s.Enabled || len(enabledEndpoints(s.Endpoints)) == 0 {
		return m.removeUnits(s.ID, svcPath, timerPath)
	}

	svcContents := m.renderService(s)
	timerContents := m.renderTimer(s)

	if err := writeFile(svcPath, svcContents); err != nil {
		return fmt.Errorf("writing service: %w", err)
	}
	if err := writeFile(timerPath, timerContents); err != nil {
		return fmt.Errorf("writing timer: %w", err)
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", m.timerName(s.ID)); err != nil {
		return err
	}
	return nil
}

// Remove tears down units for a stream that's being deleted.
func (m *Manager) Remove(streamID int64) error {
	svcPath := filepath.Join(m.UnitDir, m.serviceName(streamID))
	timerPath := filepath.Join(m.UnitDir, m.timerName(streamID))
	return m.removeUnits(streamID, svcPath, timerPath)
}

func (m *Manager) removeUnits(streamID int64, svcPath, timerPath string) error {
	// Best-effort: don't fail if units aren't there.
	_ = systemctl("disable", "--now", m.timerName(streamID))
	_ = systemctl("stop", m.serviceName(streamID))
	_ = os.Remove(svcPath)
	_ = os.Remove(timerPath)
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

// Stop terminates a running stream.
func (m *Manager) Stop(streamID int64) error {
	return systemctl("stop", m.serviceName(streamID))
}

// ---------- Unit rendering ----------

func (m *Manager) renderService(s *db.Stream) string {
	videoPath := filepath.Join(m.VideoDir, s.VideoFile)
	teeArg := buildTeeArg(s.Endpoints)

	// We escape any % in the OnCalendar/etc by passing args directly via ExecStart.
	// Stream keys are embedded in the unit file — file mode 0600 protects them.
	return fmt.Sprintf(`[Unit]
Description=streamctl: %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=/usr/bin/ffmpeg -hide_banner -loglevel warning -re -i %s -c copy -f tee -map 0:v -map 0:a %s
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
`,
		shellEscape(s.Name),
		m.RunUser,
		shellQuote(videoPath),
		shellQuote(teeArg),
		m.VideoDir,
	)
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

// shellQuote wraps a value in single quotes for safe inclusion in an ExecStart line.
// systemd does its own quoting; we use this for argv values containing spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellEscape strips problematic chars from human-facing description strings.
func shellEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}
