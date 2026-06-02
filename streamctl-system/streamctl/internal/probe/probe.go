// Package probe wraps ffprobe to extract stream parameters and verify that
// a list of clips can be concatenated with `-c copy` (no re-encode).
package probe

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
)

// Profile captures the codec/format parameters that must match across clips
// for ffmpeg's concat demuxer with `-c copy` to work.
type Profile struct {
	VideoCodec string
	Width      int
	Height     int
	PixFmt     string
	FrameRate  string // e.g. "30000/1001"
	AudioCodec string
	SampleRate string
	Channels   int
}

func Probe(videoDir, file string) (*Profile, error) {
	return ProbePath(filepath.Join(videoDir, file), file)
}

func ProbePath(path, label string) (*Profile, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ffprobe %s: %s", label, string(ee.Stderr))
		}
		return nil, fmt.Errorf("ffprobe %s: %w", label, err)
	}

	var raw struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			PixFmt     string `json:"pix_fmt"`
			RFrameRate string `json:"r_frame_rate"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output for %s: %w", label, err)
	}

	p := &Profile{}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if p.VideoCodec == "" {
				p.VideoCodec = s.CodecName
				p.Width = s.Width
				p.Height = s.Height
				p.PixFmt = s.PixFmt
				p.FrameRate = s.RFrameRate
			}
		case "audio":
			if p.AudioCodec == "" {
				p.AudioCodec = s.CodecName
				p.SampleRate = s.SampleRate
				p.Channels = s.Channels
			}
		}
	}
	if p.VideoCodec == "" {
		return nil, fmt.Errorf("%s has no video stream", label)
	}
	return p, nil
}

// ValidatePlaylist probes every file and returns a descriptive error if any
// pair of clips would break a `-c copy` concat (codec/resolution/framerate/
// pixel format / audio params mismatch).
func ValidatePlaylist(videoDir string, files []string) error {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(videoDir, f)
	}
	return ValidatePlaylistPaths(paths, files)
}

func ValidatePlaylistPaths(paths, labels []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("playlist is empty")
	}
	var first *Profile
	var firstFile string
	for i, path := range paths {
		label := path
		if i < len(labels) && labels[i] != "" {
			label = labels[i]
		}
		p, err := ProbePath(path, label)
		if err != nil {
			return err
		}
		if first == nil {
			first = p
			firstFile = label
			continue
		}
		if d := profileDiff(first, p); d != "" {
			return fmt.Errorf("clips %q and %q differ on %s — concat with -c copy requires identical streams", firstFile, label, d)
		}
	}
	return nil
}

// Bitrate returns the format-level bitrate of a file in bits/sec, or 0 if
// ffprobe doesn't report one (some MKV/MOV containers omit it).
func Bitrate(videoDir, file string) (int, error) {
	path := filepath.Join(videoDir, file)
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=bit_rate",
		"-print_format", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return 0, fmt.Errorf("ffprobe %s: %s", file, string(ee.Stderr))
		}
		return 0, fmt.Errorf("ffprobe %s: %w", file, err)
	}
	var raw struct {
		Format struct {
			BitRate string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return 0, fmt.Errorf("parsing ffprobe output for %s: %w", file, err)
	}
	if raw.Format.BitRate == "" {
		return 0, nil
	}
	var bps int
	if _, err := fmt.Sscanf(raw.Format.BitRate, "%d", &bps); err != nil {
		return 0, nil
	}
	return bps, nil
}

func profileDiff(a, b *Profile) string {
	switch {
	case a.VideoCodec != b.VideoCodec:
		return fmt.Sprintf("video codec (%s vs %s)", a.VideoCodec, b.VideoCodec)
	case a.Width != b.Width || a.Height != b.Height:
		return fmt.Sprintf("resolution (%dx%d vs %dx%d)", a.Width, a.Height, b.Width, b.Height)
	case a.PixFmt != b.PixFmt:
		return fmt.Sprintf("pixel format (%s vs %s)", a.PixFmt, b.PixFmt)
	case a.FrameRate != b.FrameRate:
		return fmt.Sprintf("frame rate (%s vs %s)", a.FrameRate, b.FrameRate)
	case a.AudioCodec != b.AudioCodec:
		return fmt.Sprintf("audio codec (%s vs %s)", a.AudioCodec, b.AudioCodec)
	case a.SampleRate != b.SampleRate:
		return fmt.Sprintf("audio sample rate (%s vs %s)", a.SampleRate, b.SampleRate)
	case a.Channels != b.Channels:
		return fmt.Sprintf("audio channels (%d vs %d)", a.Channels, b.Channels)
	}
	return ""
}
