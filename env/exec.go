package env

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"golang.org/x/sync/errgroup"
)

const (
	throughputLogInterval = 60 * time.Second
)

var _ Executor = LocalExecutor{}
var Local = LocalExecutor{}

type LocalExecutor struct{}

func (LocalExecutor) Exec(args ...string) ([]string, error) {
	return Exec(args...)
}

func (LocalExecutor) Execf(s string, args ...any) ([]string, error) {
	return Execf(s, args...)
}

func Exec(args ...string) ([]string, error) {
	name, args := args[0], args[1:]
	var arglog []string
	for _, arg := range args {
		if strings.Contains(arg, " ") {
			arglog = append(arglog, fmt.Sprintf(`"%s"`, arg))
		} else {
			arglog = append(arglog, arg)
		}
	}
	log.Printf("%s %s", name, strings.Join(arglog, " "))
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.Join(strings.Split(strings.TrimSpace(string(out)), "\n"), "; ")
		return nil, fmt.Errorf("running '%s': %w: %s", name, err, output)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

func Execf(s string, args ...any) ([]string, error) {
	return Exec(strings.Fields(fmt.Sprintf(s, args...))...)
}

// Fix the context cancelation stuff here.
// - When either command errors, cancel the other commands and the throughput
// logging routine.
// - When both commands have succeeded, cancel the throughput logging routine.

// Pipe runs `from` and `to`, with `from`'s stdout piped into `to`'s stdin.
// It's expected that this is a long running process, taking hours or more.
// The process can be canceled gracefully using the passed-in context.
// While the process runs, we log details each minute about the throughput of
// the pipe.
func Pipe(ctx context.Context, from, to *exec.Cmd) error {
	log.Printf("%s | %s", strings.Join(from.Args, " "), strings.Join(to.Args, " "))

	throughputStat := NewThroughputStat()
	defer throughputStat.Log()

	pw, pr := io.Pipe()
	tee := io.TeeReader(pw, throughputStat)
	from.Stdout = pr
	to.Stdin = tee

	var toOutput bytes.Buffer
	to.Stdout = &toOutput
	to.Stderr = &toOutput

	if err := to.Start(); err != nil {
		return fmt.Errorf("failed to start 'to' command: %w", err)
	}

	if err := from.Start(); err != nil {
		pr.Close()
		pw.Close()
		to.Process.Kill()
		to.Wait()
		return fmt.Errorf("failed to start 'from' command: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)

	g.Go(func() error {
		ticker := time.NewTicker(throughputLogInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				pr.Close()
				return nil
			case <-ticker.C:
				throughputStat.Log()
			}
		}
	})

	g.Go(func() error {
		c := make(chan error)
		go func() { c <- from.Wait() }()

		select {
		case err := <-c:
			if err != nil {
				return fmt.Errorf("'from' command error: %w", err)
			}
			pr.Close()
			return nil
		case <-ctx.Done():
			from.Process.Kill()
			return ctx.Err()
		}
	})

	g.Go(func() error {
		c := make(chan error)
		go func() { c <- to.Wait() }()

		select {
		case err := <-c:
			if err != nil {
				return fmt.Errorf("'to' command error: %w\n%s", err, toOutput.String())
			}
			cancel()
			return nil
		case <-ctx.Done():
			to.Process.Kill()
			return ctx.Err()
		}
	})

	if err := g.Wait(); err != nil {
		from.Process.Kill()
		to.Process.Kill()
		return fmt.Errorf("process error: %w", err)
	}

	return nil
}

// ThroughputStat stores throughput statistics over various intervals.
type ThroughputStat struct {
	mu         sync.Mutex
	totalBytes int64
	dataPoints []dataPoint
}

// dataPoint stores the number of bytes written and the timestamp.
type dataPoint struct {
	bytes     int64
	timestamp time.Time
}

// NewThroughputStat initializes a new ThroughputStat.
func NewThroughputStat() *ThroughputStat {
	return &ThroughputStat{}
}

func (s *ThroughputStat) Write(bs []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bytes := int64(len(bs))
	s.totalBytes += bytes

	// Add the current data point
	s.dataPoints = append(s.dataPoints, dataPoint{bytes: bytes, timestamp: time.Now()})

	// Clean up old data points older than an hour
	oneHourAgo := time.Now().Add(-time.Hour)
	i := 0
	for _, point := range s.dataPoints {
		if point.timestamp.After(oneHourAgo) {
			break
		}
		i++
	}
	s.dataPoints = s.dataPoints[i:]

	return len(bs), nil
}

func (s *ThroughputStat) Log() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	oneMinuteAgo, tenMinutesAgo, oneHourAgo := now.Add(-time.Minute), now.Add(-10*time.Minute), now.Add(-time.Hour)

	minuteBytes, tenMinuteBytes, hourBytes := int64(0), int64(0), int64(0)
	var firstMinuteTimestamp, firstTenMinuteTimestamp, firstHourTimestamp *time.Time

	for _, point := range s.dataPoints {
		if point.timestamp.After(oneHourAgo) {
			hourBytes += point.bytes
			if firstHourTimestamp == nil || point.timestamp.Before(*firstHourTimestamp) {
				firstHourTimestamp = &point.timestamp
			}
		}
		if point.timestamp.After(tenMinutesAgo) {
			tenMinuteBytes += point.bytes
			if firstTenMinuteTimestamp == nil || point.timestamp.Before(*firstTenMinuteTimestamp) {
				firstTenMinuteTimestamp = &point.timestamp
			}
		}
		if point.timestamp.After(oneMinuteAgo) {
			minuteBytes += point.bytes
			if firstMinuteTimestamp == nil || point.timestamp.Before(*firstMinuteTimestamp) {
				firstMinuteTimestamp = &point.timestamp
			}
		}
	}

	getElapsedSeconds := func(firstTimestamp *time.Time, windowSeconds int64) int64 {
		if firstTimestamp == nil {
			return windowSeconds // No data points in the window, default to full window size
		}

		elapsed := now.Sub(*firstTimestamp).Seconds()
		if elapsed > float64(windowSeconds) {
			return windowSeconds
		}
		return int64(elapsed)
	}

	minuteElapsedSeconds := getElapsedSeconds(firstMinuteTimestamp, 60)
	tenMinuteElapsedSeconds := getElapsedSeconds(firstTenMinuteTimestamp, 600)
	hourElapsedSeconds := getElapsedSeconds(firstHourTimestamp, 3600)

	log.Printf("Throughput - Total: %s, Last minute: %s/sec, 10 mins: %s/sec, hour: %s/sec",
		humanize.Bytes(uint64(s.totalBytes)),
		printThroughput(minuteBytes, minuteElapsedSeconds),
		printThroughput(tenMinuteBytes, tenMinuteElapsedSeconds),
		printThroughput(hourBytes, hourElapsedSeconds),
	)
}

func printThroughput(bytes, durationSeconds int64) string {
	if durationSeconds == 0 {
		return humanize.Bytes(uint64(bytes))
	}
	return humanize.Bytes(uint64(float64(bytes) / float64(durationSeconds)))
}
