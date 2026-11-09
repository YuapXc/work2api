package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"work2api/internal/streamwatch"
)

var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

var errStreamNormalTermination = errors.New("upstream stream terminated normally")

var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

type downstreamStreamError struct{ error }

func (e downstreamStreamError) Unwrap() error { return e.error }

type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

type bridgeStreamEvent struct {
	NativeThinking map[string]any
	SealSource     Protocol
	Kind           string
	ResponseID     string
	Model          string
	Text           string
	Signature      string
	ToolKey        string
	ToolID         string
	ToolName       string
	Stop           string
	Error          string
	ErrorType      string
	Encrypted      string
	Usage          *Usage
}

func transcodeStream(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) error {
	_, _, err := transcodeStreamWithUsage(w, reader, from, to, model)
	return err
}

func transcodeStreamWithUsage(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	return TranscodeStream(context.Background(), w, reader, from, to, model)
}

// TranscodeStream is the request-aware form used by the
// gateway. A cancelled client must not receive a synthetic upstream error
// after its connection has gone away.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	return TranscodeStreamWithRewrite(ctx, w, reader, from, to, model, nil)
}

// TranscodeStreamWithRewrite normalizes decoded upstream events before conversion.
// The rewrite receives nil at a Chat terminal marker.
func TranscodeStreamWithRewrite(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string, rewrite func(map[string]any) error) (Usage, bool, error) {
	if _, ok := w.(http.Flusher); !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		generationCtx:     ctx,
		rewrite:           rewrite,
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := newBridgeStreamEmitter(w, w, to, model)
	termination := streamOpen
	readErr := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			termination = streamErrorTermination
			if emitErr := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: err.Error(), ErrorType: "upstream_error"}); emitErr != nil {
				return emitErr
			}
			return errStreamUpstreamFailure
		}
		for i, event := range events {
			switch event.Kind {
			case "done":
				termination = streamNormalTermination
			case "error":
				termination = streamErrorTermination
			}
			if err := emitter.Emit(event); err != nil {
				// The whole frame was parsed even if delivering its delta failed.
				for _, remaining := range events[i+1:] {
					if remaining.Kind == "usage" {
						_ = emitter.Emit(remaining)
					}
				}
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, nil
		}
		var delivery downstreamStreamError
		if ClientCanceled(ctx, readErr) || errors.As(readErr, &delivery) {
			return emitter.usage, emitter.usageReported, readErr
		}
		if termination == streamOpen {
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, emitErr
			}
		}
		return emitter.usage, emitter.usageReported, readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, err
	}
	return emitter.usage, emitter.usageReported, errSSEUnexpectedEOF
}

func emitUnexpectedStreamError(emitter *bridgeStreamEmitter, cause error) error {
	message := "upstream SSE stream ended before a terminal event"
	if cause != nil && !errors.Is(cause, errSSEUnexpectedEOF) {
		message = fmt.Sprintf("upstream SSE stream failed: %v", cause)
	}
	err := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: message, ErrorType: "upstream_error"})
	if errors.Is(err, errStreamUpstreamFailure) {
		return nil
	}
	return err
}

func ClientCanceled(ctx context.Context, streamErr error) bool {
	if errors.Is(streamErr, context.Canceled) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// sseFlushWriter preserves an upstream SSE byte stream while making each
// successful write visible to the client immediately. io.Copy is free to
// choose large writes, so flushing in Write is the only reliable place to
// keep same-protocol streams live.
type sseFlushWriter struct {
	writer io.Writer
	flush  func() error
}

func (writer *sseFlushWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil && n > 0 {
		err = writer.flush()
	}
	if err != nil {
		return n, downstreamStreamError{err}
	}
	return n, nil
}

func forwardSSEWithUsage(w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	return ForwardStream(context.Background(), w, reader, protocol, model)
}

func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	if _, ok := w.(http.Flusher); !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	observer.parser.generationCtx = ctx
	buffer, release := streamwatch.BorrowLineBuffer()
	defer release()
	writer := &sseFlushWriter{writer: w, flush: http.NewResponseController(w).Flush}
	var copyErr error
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			accepted, observeErr := observer.Write(buffer[:n])
			written, err := writer.Write(buffer[:accepted])
			if err != nil {
				copyErr = err
				break
			}
			if written != accepted {
				copyErr = io.ErrShortWrite
				break
			}
			if observeErr != nil {
				copyErr = observeErr
				break
			}
			if observer.NormalTermination() || observer.ErrorTermination() {
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
		if ctx.Err() != nil {
			copyErr = ctx.Err()
			break
		}
	}
	usage := observer.Finish()
	if observer.ErrorTermination() {
		if copyErr != nil {
			return usage, observer.Reported(), copyErr
		}
		return usage, observer.Reported(), errStreamUpstreamFailure
	}
	if observer.NormalTermination() && observer.ParseError() == nil {
		return usage, observer.Reported(), copyErr
	}
	var delivery downstreamStreamError
	if ClientCanceled(ctx, copyErr) || errors.As(copyErr, &delivery) {
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		return usage, observer.Reported(), copyErr
	}
	cause := observer.ParseError()
	if copyErr != nil {
		cause = copyErr
	}
	if cause == nil {
		cause = errSSEUnexpectedEOF
	}
	emitter := newBridgeStreamEmitter(w, w, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), err
	}
	return usage, observer.Reported(), cause
}

type streamUsageObserver struct {
	parser   *bridgeStreamParser
	buffer   []byte
	scan     int
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
}

func newStreamUsageObserver(protocol Protocol) *streamUsageObserver {
	return &streamUsageObserver{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

func (observer *streamUsageObserver) Write(data []byte) (int, error) {
	// Bound append overshoot even when a caller supplies a large single write.
	const fragment = 64 << 10
	accepted := 0
	for len(data) > 0 {
		n := min(len(data), fragment)
		written, err := observer.writeFragment(data[:n])
		accepted += written
		if err != nil || written < n || observer.normal || observer.error {
			return accepted, err
		}
		data = data[n:]
	}
	return accepted, nil
}

func (observer *streamUsageObserver) writeFragment(data []byte) (int, error) {
	previous := len(observer.buffer)
	observer.buffer = append(observer.buffer, data...)
	consumed := 0
	accepted := len(data)
	for {
		index, width := nextSSEBoundary(observer.buffer[consumed+observer.scan:])
		if index < 0 {
			if len(observer.buffer)-consumed > streamwatch.DefaultMaxEventBytes {
				observer.parseErr = streamwatch.ErrSSEEventTooLarge
				observer.buffer = nil
				return max(0, consumed-previous), observer.parseErr
			}
			// Only the last three bytes can form a delimiter with the next
			// fragment; don't rescan a long unfinished frame from its start.
			observer.scan = max(0, len(observer.buffer)-consumed-3)
			break
		}
		// consume is synchronous and does not retain frame bytes; the string
		// parser owns its copy. Do not clone the same frame before parsing it.
		end := consumed + observer.scan + index + width
		if end-consumed > streamwatch.DefaultMaxEventBytes {
			observer.parseErr = streamwatch.ErrSSEEventTooLarge
			observer.buffer = nil
			return max(0, consumed-previous), observer.parseErr
		}
		frame := observer.buffer[consumed:end]
		observer.consume(frame)
		consumed = end
		observer.scan = 0
		if observer.normal || observer.error {
			accepted = max(0, end-previous)
			observer.buffer = observer.buffer[:end]
			break
		}
	}
	if consumed > 0 {
		n := copy(observer.buffer, observer.buffer[consumed:])
		observer.buffer = observer.buffer[:n]
	}
	// Retain normal-size working storage, but release a completed oversized
	// frame without changing how much data the protocol can receive.
	if cap(observer.buffer) > 128<<10 && len(observer.buffer) <= 64<<10 {
		if len(observer.buffer) == 0 {
			observer.buffer = nil
		} else {
			buffer := make([]byte, len(observer.buffer), 64<<10)
			copy(buffer, observer.buffer)
			observer.buffer = buffer
		}
	}
	return accepted, nil
}

func (observer *streamUsageObserver) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	observer.scan = 0
	return observer.usage
}

func (observer *streamUsageObserver) Reported() bool { return observer.reported }

func (observer *streamUsageObserver) NormalTermination() bool { return observer.normal }

func (observer *streamUsageObserver) ErrorTermination() bool { return observer.error }

func (observer *streamUsageObserver) ParseError() error { return observer.parseErr }

func (observer *streamUsageObserver) consume(frame []byte) {
	err := readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		events, err := observer.parser.Parse(eventName, data)
		if err != nil {
			if observer.parseErr == nil {
				observer.parseErr = err
			}
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				observer.normal = true
			case "finish":
				observer.usage.FinishReason = event.Stop
			case "error":
				observer.error = true
			}
			if event.Usage != nil {
				observer.reported = true
				mergeBridgeUsage(&observer.usage, *event.Usage)
			}
		}
		return nil
	})
	if err != nil && observer.parseErr == nil {
		observer.parseErr = err
	}
}

func nextSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	buffer, release := streamwatch.BorrowLineBuffer()
	defer release()
	scanner.Buffer(buffer, streamwatch.DefaultMaxLineBytes)
	var eventName string
	var dataLines []string
	var budget streamwatch.SSEEventBudget
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if err := budget.AddLine(line); err != nil {
			return err
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 {
		// A blank line is the SSE record delimiter. Do not parse a final
		// unterminated record as a complete frame; an EOF without a terminal
		// event is handled by the caller as a truncated stream.
		return errSSEUnexpectedEOF
	}
	return nil
}
