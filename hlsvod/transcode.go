package hlsvod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path"
	"strings"
	"sync"
)

type TranscodeConfig struct {
	InputFilePath string // Transcoded video input.
	OutputDirPath string // Segments output path.
	SegmentPrefix string // e.g. prefix-000001.ts
	SegmentOffset int    // Start segment number.

	SegmentTimes []float64
	VideoProfile *VideoProfile
	AudioProfile *AudioProfile
}

type VideoProfile struct {
	Width   int
	Height  int
	Bitrate int // in kilobytes
}

type AudioProfile struct {
	Bitrate int // in kilobytes
}

type VideoInfo struct {
	PixelFormat string `json:"pix_fmt"`
}

type FFProbeOutput struct {
	Streams []VideoInfo `json:"streams"`
}

func detectVideoFormat(ctx context.Context, ffprobeBinary string, inputPath string) (string, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-select_streams", "v:0",
		inputPath,
	}

	cmd := exec.CommandContext(ctx, ffprobeBinary, args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	var probeOutput FFProbeOutput
	if err := json.Unmarshal(output, &probeOutput); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	if len(probeOutput.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	return probeOutput.Streams[0].PixelFormat, nil
}

func is422Format(pixelFormat string) bool {
	format422 := []string{
		// Standard planar 4:2:2 formats
		"yuv422p",     // 8-bit planar YUV 4:2:2
		"yuv422p10le", // 10-bit planar YUV 4:2:2 little endian
		"yuv422p12le", // 12-bit planar YUV 4:2:2 little endian
		"yuv422p16le", // 16-bit planar YUV 4:2:2 little endian
		"yuv422p9le",  // 9-bit planar YUV 4:2:2 little endian
		"yuv422p10be", // 10-bit planar YUV 4:2:2 big endian
		"yuv422p12be", // 12-bit planar YUV 4:2:2 big endian
		"yuv422p16be", // 16-bit planar YUV 4:2:2 big endian
		"yuv422p9be",  // 9-bit planar YUV 4:2:2 big endian
		"yuv422p14le", // 14-bit planar YUV 4:2:2 little endian
		"yuv422p14be", // 14-bit planar YUV 4:2:2 big endian

		// Packed 4:2:2 formats
		"yuyv422", // YUYV 4:2:2 packed format
		"uyvy422", // UYVY 4:2:2 packed format

		// JPEG-range 4:2:2 format
		"yuvj422p", // JPEG-range (full range 0-255) YUV 4:2:2

		// 4:2:2 with alpha channel
		"yuva422p",                     // 8-bit planar YUV 4:2:2 with alpha
		"yuva422p9le", "yuva422p9be",   // 9-bit YUV 4:2:2 with alpha
		"yuva422p10le", "yuva422p10be", // 10-bit YUV 4:2:2 with alpha
		"yuva422p12le", "yuva422p12be", // 12-bit YUV 4:2:2 with alpha
		"yuva422p16le", "yuva422p16be", // 16-bit YUV 4:2:2 with alpha

		// Professional/broadcast 4:2:2 formats
		"v210", // 10-bit 4:2:2 packed format (Avid, Final Cut Pro)
		"v216", // 16-bit 4:2:2 packed format (QuickTime)

		// Semi-planar 4:2:2 formats (chroma components interleaved)
		"p210le", "p210be", // 10-bit semi-planar 4:2:2
		"p216le", "p216be", // 16-bit semi-planar 4:2:2
	}

	for _, fmt := range format422 {
		if pixelFormat == fmt {
			return true
		}
	}

	return false
}

// returns a channel, that delivers name of the segments as they are encoded
func TranscodeSegments(ctx context.Context, ffmpegBinary string, config TranscodeConfig) (chan string, error) {
	totalSegments := len(config.SegmentTimes)
	if totalSegments < 2 {
		return nil, fmt.Errorf("minimum 2 segment times needed")
	}

	// set time bountary
	var startAt, endAt float64
	if totalSegments > 0 {
		startAt = config.SegmentTimes[0]
		endAt = config.SegmentTimes[totalSegments-1]
	}

	// convet to comma separated segment times
	fmtSegTimes := []string{}
	for _, segmentTime := range config.SegmentTimes {
		fmtSegTimes = append(
			fmtSegTimes,
			fmt.Sprintf("%.6f", segmentTime),
		)
	}
	commaSeparatedSegTimes := strings.Join(fmtSegTimes[1:], ",")

	args := []string{
		"-loglevel", "warning",
	}

	// Seek to start point. Note there is a bug(?) in ffmpeg: https://github.com/FFmpeg/FFmpeg/blob/fe964d80fec17f043763405f5804f397279d6b27/fftools/ffmpeg_opt.c#L1240
	// can possible set `seek_timestamp` to a negative value, which will cause `avformat_seek_file` to reject the input timestamp.
	// To prevent this, the first break point, which we know will be zero, will not be fed to `-ss`.
	if startAt > 0 {
		args = append(args, []string{
			"-ss", fmt.Sprintf("%.6f", startAt),
		}...)
	}

	// Input specs
	args = append(args, []string{
		"-i", config.InputFilePath, // Input file
		"-to", fmt.Sprintf("%.6f", endAt),
		"-copyts", // So the "-to" refers to the original TS
		"-force_key_frames", commaSeparatedSegTimes,
		"-sn", // No subtitles
	}...)

	// Detect video format to determine appropriate profile
	var useHigh422Profile bool
	if config.VideoProfile != nil {
		ffprobeBinary := strings.Replace(ffmpegBinary, "ffmpeg", "ffprobe", 1)
		pixelFormat, err := detectVideoFormat(ctx, ffprobeBinary, config.InputFilePath)
		if err != nil {
			log.Printf("Warning: Could not detect video format, using default profile: %v", err)
		} else {
			log.Printf("Detected pixel format: %s", pixelFormat)
			useHigh422Profile = is422Format(pixelFormat)
			if useHigh422Profile {
				log.Printf("Detected 4:2:2 format (%s), using high422 profile", pixelFormat)
			} else {
				log.Printf("Using default profile for format: %s", pixelFormat)
			}
		}
	}

	// Video specs
	if config.VideoProfile != nil {
		profile := config.VideoProfile

		var scale string
		if profile.Width >= profile.Height {
			scale = fmt.Sprintf("scale=-2:%d", profile.Height)
		} else {
			scale = fmt.Sprintf("scale=%d:-2", profile.Width)
		}

		videoProfile := "high"
		if useHigh422Profile {
			videoProfile = "high422"
		}

		args = append(args, []string{
			"-vf", scale,
			"-c:v", "libx264",
			"-preset", "faster",
			"-profile:v", videoProfile,
			"-level:v", "4.0",
			"-b:v", fmt.Sprintf("%dk", profile.Bitrate),
		}...)
	}

	// Audio specs
	if config.AudioProfile != nil {
		profile := config.AudioProfile

		args = append(args, []string{
			"-c:a", "aac",
			"-b:a", fmt.Sprintf("%dk", profile.Bitrate),
		}...)
	}

	// Segmenting specs
	args = append(args, []string{
		"-f", "segment",
		"-segment_time_delta", "0.2",
		"-segment_format", "mpegts",
		"-segment_times", commaSeparatedSegTimes,
		"-segment_start_number", fmt.Sprintf("%d", config.SegmentOffset),
		"-segment_list_type", "flat",
		"-segment_list", "pipe:1", // Output completed segments to stdout.
		path.Join(config.OutputDirPath, fmt.Sprintf("%s-%%05d.ts", config.SegmentPrefix)),
	}...)

	cmd := exec.CommandContext(ctx, ffmpegBinary, args...)
	log.Println("Starting FFmpeg process with args", strings.Join(cmd.Args[:], " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	segments := make(chan string, 1)

	// handle stdout
	go func() {
		defer func() {
			wg.Wait()

			close(segments)
		}()

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			segments <- scanner.Text()
		}

		if err := scanner.Err(); err != nil {
			log.Println("Error while reading FFmpeg stdout:", err)
		}
	}()

	// handle stderr
	go func() {
		defer wg.Done()

		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Println(scanner.Text())
		}

		if err := scanner.Err(); err != nil {
			log.Println("Error while reading FFmpeg stderr:", err)
		}
	}()

	// start execution
	err = cmd.Start()

	// wait until execution finishes
	go func() {
		defer wg.Done()

		err := cmd.Wait()
		if err != nil {
			log.Println("FFmpeg process exited with error:", err)
		} else {
			log.Println("FFmpeg process successfully finished.")
		}
	}()

	return segments, err
}
