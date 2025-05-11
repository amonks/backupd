package env

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"golang.org/x/sync/errgroup"

	"monks.co/backupd/logger"
)

const (
	throughputLogInterval = 60 * time.Second
)

var _ Executor = &LocalExecutor{}

// A LocalExecutor implements the Executor interface by executing commands on
// the local machine.
type LocalExecutor struct {
}

var Local = &LocalExecutor{}

// Exec runs the given command, returning its stdout and stderr as a combined
// slice of lines.
func (*LocalExecutor) Exec(logger logger.Logger, args ...string) ([]string, error) {
	return Exec(logger, args...)
}

// Execf runs the given command, returning its stdout and stderr as a combined
// slice of lines.
func (*LocalExecutor) Execf(logger logger.Logger, s string, args ...any) ([]string, error) {
	return Execf(logger, s, args...)
}

// Exec runs the given command, returning its stdout and stderr as a combined
// slice of lines.
func Exec(logger logger.Logger, args ...string) ([]string, error) {
	name, args := args[0], args[1:]
	var arglog []string
	for _, arg := range args {
		if strings.Contains(arg, " ") {
			arglog = append(arglog, fmt.Sprintf(`"%s"`, arg))
		} else {
			arglog = append(arglog, arg)
		}
	}
	logger.Printf("%s %s", name, strings.Join(arglog, " "))
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.Join(strings.Split(strings.TrimSpace(string(out)), "\n"), "; ")
		return nil, fmt.Errorf("running '%s': %w: %s", name, err, output)
	}
	if string(out) == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// Execf runs the given command, returning its stdout and stderr as a combined
// slice of lines.
func Execf(logger logger.Logger, s string, args ...any) ([]string, error) {
	return Exec(logger, strings.Fields(fmt.Sprintf(s, args...))...)
}

type outputCollector struct {
	logger logger.Logger
	buf    *bytes.Buffer
}

func (oc *outputCollector) Write(bs []byte) (int, error) {
	oc.buf.Write(bs)
	oc.logger.Write(bs)
	return len(bs), nil
}

// Pipe runs `from` and `to`, with `from`'s stdout piped into `to`'s stdin.
// It's expected that this is a long running process, taking hours or more.
// The process can be canceled gracefully using the passed-in context.
// While the process runs, we log details each minute about the throughput of
// the pipe.
func Pipe(ctx context.Context, logger logger.Logger, size int64, from, to *exec.Cmd) error {
	logger.Printf("%s | %s", strings.Join(from.Args, " "), strings.Join(to.Args, " "))

	throughputStat := NewThroughputStat(logger, size)
	defer throughputStat.Log()

	pw, pr := io.Pipe()
	tee := io.TeeReader(pw, throughputStat)
	from.Stdout = pr
	to.Stdin = tee

	out := &outputCollector{logger, &bytes.Buffer{}}
	to.Stdout = out
	to.Stderr = out

	// Start the `to` command.
	if err := to.Start(); err != nil {
		return fmt.Errorf("failed to start 'to' command: %w", err)
	}

	// Start the `from` command. If we fail to start it, kill the `to`
	// command, too.
	if err := from.Start(); err != nil {
		pr.Close()
		pw.Close()
		to.Process.Kill()
		to.Wait()
		return fmt.Errorf("failed to start 'from' command: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)

	// Log every $interval until the context is canceled.
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

	// Wait for the `from` command. If it errors, return that error to the
	// errgroup. If the context is canceled, kill the command before
	// returning to the errgroup.
	g.Go(func() error {
		c := make(chan error)
		go func() { c <- from.Wait() }()

		select {
		case err := <-c:
			if err != nil {
				return fmt.Errorf("'from' command error: %w", err)
			}

			// I think we don't need to cancel the context here,
			// because this pipe closure will cause the `to` command
			// to terminate, which, in turn, will cancel the
			// context.
			pr.Close()

			return nil

		case <-ctx.Done():
			from.Process.Kill()
			return ctx.Err()
		}
	})

	// Wait for the `to` command. If it errors, return that error to the
	// errgroup. If the context is canceled, kill the command before
	// returning to the errgroup.
	g.Go(func() error {
		c := make(chan error)
		go func() { c <- to.Wait() }()

		select {
		case err := <-c:
			if err != nil {
				return fmt.Errorf("'to' command error: %w (%s)", err, out.buf.Bytes())
			}

			cancel()

			return nil

		case <-ctx.Done():
			to.Process.Kill()
			return ctx.Err()
		}
	})

	// Wait for the errgroup.
	if err := g.Wait(); err != nil {

		// XXX: is this necessary? If the errgroup ended, shouldn't
		// the processes have already died?
		from.Process.Kill()
		to.Process.Kill()

		return fmt.Errorf("process error: %w", err)
	}

	return nil
}

// ThroughputStat stores throughput statistics over various intervals.
type ThroughputStat struct {
	mu               sync.Mutex
	logger           logger.Logger
	startedAt        time.Time
	bytesTransferred int64
	size             int64
	dataPoints       []dataPoint
}

// dataPoint stores the number of bytes written and the timestamp.
type dataPoint struct {
	bytes     int64
	timestamp time.Time
}

// NewThroughputStat initializes a new ThroughputStat.
func NewThroughputStat(logger logger.Logger, size int64) *ThroughputStat {
	return &ThroughputStat{
		startedAt: time.Now(),
		logger:    logger,
		size:      size,
	}
}

func (s *ThroughputStat) Write(bs []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bytes := int64(len(bs))
	s.bytesTransferred += bytes

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

	minuteElapsedSeconds := getElapsedSeconds(&now, firstMinuteTimestamp, 60)
	tenMinuteElapsedSeconds := getElapsedSeconds(&now, firstTenMinuteTimestamp, 600)
	hourElapsedSeconds := getElapsedSeconds(&now, firstHourTimestamp, 3600)

	s.logger.Printf("%s\t%.2f%% of %s\tTotal: %s\tLast minute: %s\t10 mins: %s\thour: %s",
		now.Sub(s.startedAt).Truncate(time.Second),
		float64(s.bytesTransferred)/float64(s.size)*100.0,
		humanize.Bytes(uint64(s.size)),
		humanize.Bytes(uint64(s.bytesTransferred)),
		printThroughput(minuteBytes, minuteElapsedSeconds),
		printThroughput(tenMinuteBytes, tenMinuteElapsedSeconds),
		printThroughput(hourBytes, hourElapsedSeconds),
	)
}

// printThroughput calculates and returns the human-readable network throughput given
// the amount of data transferred in bytes and the duration of the transfer in seconds.
// If the duration is zero, it returns the humanized byte size directly to avoid division by zero.
//
// Parameters:
//   - bytes: The total number of bytes transferred.
//   - durationSeconds: The time in seconds over which the data transfer took place.
//
// Returns:
//   - A string representing the human-readable throughput (e.g., "10 MB/s") or the human-readable
//     size if the duration is zero.
func printThroughput(bytes, durationSeconds int64) string {
	if durationSeconds == 0 {
		return humanize.SIWithDigits(float64(bytes*8), 1, "bps")
	}
	return humanize.SIWithDigits(float64(bytes*8)/float64(durationSeconds), 1, "bps")
}

// getElapsedSeconds calculates the number of seconds elapsed between the given
// 'firstTimestamp' and 'now'. If 'firstTimestamp' is nil, the function will
// return the provided 'windowSeconds', indicating that no data points exist
// within the window, thus defaulting to the full window size. If the elapsed
// time exceeds 'windowSeconds', the function returns 'windowSeconds'.
// Otherwise, it returns the actual elapsed time in seconds.
//
// Parameters:
//   - now: A pointer to a time.Time object representing the current time.
//   - firstTimestamp: A pointer to a time.Time object representing the starting
//     point of the time interval. If this value is nil, it signifies that no
//     relevant timestamp is available.
//   - windowSeconds: An int64 representing the maximum window size in seconds.
//
// Returns:
//   - An int64 representing the number of seconds elapsed or the full window
//     size, whichever is smaller.
func getElapsedSeconds(now, firstTimestamp *time.Time, windowSeconds int64) int64 {
	if firstTimestamp == nil {
		return windowSeconds // No data points in the window, default to full window size
	}

	elapsed := now.Sub(*firstTimestamp).Seconds()
	if elapsed > float64(windowSeconds) {
		return windowSeconds
	}
	return int64(elapsed)
}
