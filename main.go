package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	apiMetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
)

type entry struct {
	score int64
	file  string
}

func parse(r io.Reader) ([]entry, error) {
	var entries []entry
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 1024*1024)
	for n := 1; s.Scan(); n++ {
		fields := strings.Fields(s.Text())
		if len(fields) != 5 {
			return nil, fmt.Errorf("line %d: expected 5 fields, got %d", n, len(fields))
		}
		score, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil { return nil, fmt.Errorf("line %d: invalid score: %w", n, err) }
		lineInfo := strings.Split(fields[4], ":")
		if len(lineInfo) != 2 { return nil, fmt.Errorf("line %d: invalid location %q", n, fields[4]) }
		line, err := strconv.ParseInt(lineInfo[0], 10, 64)
		if err != nil { return nil, fmt.Errorf("line %d: invalid line number: %w", n, err) }
		entries = append(entries, entry{score: score, file: fields[3]})
	}
	if err := s.Err(); err != nil { return nil, err }
	if len(entries) == 0 { return nil, errors.New("input contains no metrics") }
	return entries, nil
}

func run(ctx context.Context, endpoint string) error {
	cmd := exec.CommandContext(ctx, "gocognit", "-over", "-1", ".")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("run gocognit: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("run gocognit: %w", err)
	}
	entries, err := parse(strings.NewReader(string(output)))
	if err != nil { return err }

	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	if err != nil { return fmt.Errorf("create OTLP exporter: %w", err) }
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))),
		sdkmetric.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("gocognit-metrics"))),
	)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	instrument, err := provider.Meter("gocognit-metrics").Int64Gauge("gocognit.complexity", apiMetric.WithDescription("Cognitive complexity reported by gocognit"))
	if err != nil { return fmt.Errorf("create metric instrument: %w", err) }
	fileScores := make(map[string]int64)
	for _, e := range entries {
		fileScores[e.file] += e.score
	}
	for file, score := range fileScores {
		instrument.Record(ctx, score, apiMetric.WithAttributes(attribute.String("code.filepath", file)))
	}
	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return provider.ForceFlush(flushCtx)
}

func main() {
	endpoint := flag.String("endpoint", "localhost:4317", "OTLP gRPC endpoint (host:port)")
	flag.Parse()
	if err := run(context.Background(), *endpoint); err != nil {
		log.Fatal(err)
	}
}
