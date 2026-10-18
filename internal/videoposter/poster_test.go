package videoposter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVideoPosterProcessHelper(t *testing.T) {
	if os.Getenv("FILESAGE_VIDEOPOSTER_HELPER") == "" {
		return
	}
	args := os.Args
	marker := -1
	for i, arg := range args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		os.Exit(81)
	}
	if file := os.Getenv("FILESAGE_VIDEOPOSTER_ARGS_FILE"); file != "" {
		_ = os.WriteFile(file, []byte(strings.Join(args[marker+1:], "\x00")), 0600)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(82)
	}
	switch os.Getenv("FILESAGE_VIDEOPOSTER_MODE") {
	case "timeout":
		time.Sleep(10 * time.Second)
	case "output":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 128; i++ {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return
			}
		}
	case "stderr":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), 128<<10))
		os.Exit(7)
	case "concurrency":
		log := os.Getenv("FILESAGE_VIDEOPOSTER_EVENTS_FILE")
		appendEvent(t, log, "start")
		release := os.Getenv("FILESAGE_VIDEOPOSTER_RELEASE_FILE")
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(85)
			}
			time.Sleep(5 * time.Millisecond)
		}
		appendEvent(t, log, "done")
		writeFakeJPEG()
	case "success":
		writeFakeJPEG()
	default:
		os.Exit(83)
	}
	os.Exit(0)
}

func TestGenerateUsesStdinAndRestrictedFFmpegArguments(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args.bin")
	generator := helperGenerator(t, "success", map[string]string{"FILESAGE_VIDEOPOSTER_ARGS_FILE": argsPath}, Options{})
	got, err := generator.Generate(context.Background(), strings.NewReader("video payload"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeJPEG) {
		t.Fatalf("unexpected JPEG bytes: %v", got)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(data), "\x00")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-protocol_whitelist pipe", "-format_whitelist", "-i pipe:0", "-frames:v 1", "min(480,iw)", "-threads 1", "-f image2pipe", "pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ffmpeg args missing %q: %v", want, args)
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "video payload") {
			t.Fatal("video bytes were passed as an argument")
		}
	}
}

func TestGenerateReturnsRecognizableDisabledError(t *testing.T) {
	generator := New(Options{Binary: filepath.Join(t.TempDir(), "no-ffmpeg")})
	_, err := generator.Generate(context.Background(), strings.NewReader("video"))
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestGenerateStopsAtTimeout(t *testing.T) {
	generator := helperGenerator(t, "timeout", nil, Options{Timeout: 100 * time.Millisecond})
	started := time.Now()
	_, err := generator.Generate(context.Background(), strings.NewReader("video"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("timeout was not enforced: %s", time.Since(started))
	}
}

func TestGenerateStopsAtOutputLimit(t *testing.T) {
	generator := helperGenerator(t, "output", nil, Options{MaxOutputBytes: 1024, Timeout: 3 * time.Second})
	_, err := generator.Generate(context.Background(), strings.NewReader("video"))
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("want ErrOutputLimit, got %v", err)
	}
}

func TestGenerateBoundsStderr(t *testing.T) {
	generator := helperGenerator(t, "stderr", nil, Options{MaxStderrBytes: 64, Timeout: 3 * time.Second})
	_, err := generator.Generate(context.Background(), strings.NewReader("video"))
	if err == nil || !strings.Contains(err.Error(), "stderr truncated") {
		t.Fatalf("expected a truncated diagnostic error, got %v", err)
	}
	if len(err.Error()) > 512 {
		t.Fatalf("stderr was not bounded: %d bytes", len(err.Error()))
	}
}

func TestGenerateLimitsConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.log")
	release := filepath.Join(dir, "release")
	generator := helperGenerator(t, "concurrency", map[string]string{
		"FILESAGE_VIDEOPOSTER_EVENTS_FILE":  events,
		"FILESAGE_VIDEOPOSTER_RELEASE_FILE": release,
	}, Options{})
	start := make(chan struct{})
	errs := make(chan error, 3)
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := generator.Generate(context.Background(), strings.NewReader("video"))
			errs <- err
		}()
	}
	close(start)
	if err := waitForEventCount(events, "start", 2, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if countEvents(t, events, "start") != 2 {
		t.Fatalf("more than the default two processes started concurrently: %s", readEvents(t, events))
	}
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	active, maximum := 0, 0
	for _, event := range readEvents(t, events) {
		switch event {
		case "start":
			active++
			if active > maximum {
				maximum = active
			}
		case "done":
			active--
		}
	}
	if maximum > defaultConcurrency {
		t.Fatalf("maximum concurrent processes = %d", maximum)
	}
}

func TestGenerateRealFFmpegWhenAvailable(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	command := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=red:s=640x360:d=0.2", "-an", "-c:v", "mpeg4", "-f", "matroska", "pipe:1")
	video, err := command.Output()
	if err != nil {
		t.Skipf("ffmpeg lavfi test source is unavailable: %v", err)
	}
	got, err := New(Options{Binary: ffmpeg}).Generate(context.Background(), bytes.NewReader(video))
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := imageDecodeConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || config.Width > 480 || config.Height > 480 {
		t.Fatalf("unexpected preview: %s %dx%d", format, config.Width, config.Height)
	}
}

var fakeJPEG = []byte{0xff, 0xd8, 0xff, 0xd9}

func helperGenerator(t *testing.T, mode string, extraEnv map[string]string, options Options) *Generator {
	t.Helper()
	options.Binary = os.Args[0]
	generator := New(options)
	generator.commandPrefix = []string{"-test.run=^TestVideoPosterProcessHelper$", "--"}
	env := []string{"FILESAGE_VIDEOPOSTER_HELPER=1", "FILESAGE_VIDEOPOSTER_MODE=" + mode}
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	generator.commandEnv = env
	return generator
}

func writeFakeJPEG() {
	_, _ = os.Stdout.Write(fakeJPEG)
}

func appendEvent(t *testing.T, path, event string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(84)
	}
	_, _ = file.Write([]byte(event + "\n"))
	_ = file.Close()
}

func waitForEventCount(path, event string, count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Count(string(data), event+"\n") >= count {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %d %q events: %s", count, event, readEvents(nil, path))
}

func countEvents(t *testing.T, path, event string) int {
	t.Helper()
	return strings.Count(strings.Join(readEvents(t, path), "\n"), event)
}

func readEvents(t *testing.T, path string) []string {
	if t != nil {
		t.Helper()
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if t != nil {
			t.Fatal(err)
		}
		return nil
	}
	return strings.Fields(string(data))
}

func imageDecodeConfig(data []byte) (image.Config, string, error) {
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	return config, "jpeg", err
}
