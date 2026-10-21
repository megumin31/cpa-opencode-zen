package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func verifyRetiredStream(stream *coreexecutor.StreamResult) error {
	deadline := time.NewTimer(checkTimeout)
	defer deadline.Stop()
	sawCancellation := false
	for {
		select {
		case chunk, open := <-stream.Chunks:
			if !open {
				if !sawCancellation {
					return fmt.Errorf("retired stream closed without cancellation notification")
				}
				return nil
			}
			if chunk.Err != nil {
				// The retiring worker may report cancellation before the explicit
				// generation-close message wins. Both terminate the same old stream;
				// the caller context was independently required to remain live.
				message := chunk.Err.Error()
				if !strings.Contains(message, "plugin reconfigured") && !strings.Contains(message, "request canceled") && !strings.Contains(message, "context canceled") {
					return fmt.Errorf("unexpected retired-stream error: %w", chunk.Err)
				}
				sawCancellation = true
			}
		case <-deadline.C:
			return fmt.Errorf("retired stream did not close after same-instance reconfigure")
		}
	}
}

func verifyFreshRequests(ctx context.Context, host *pluginhost.Host, executor coreauth.ProviderExecutor, auth *coreauth.Auth, upstream *mockUpstream) error {
	request, options := requestFor(host, ctx, "fresh-execute", false)
	type outcome struct {
		response coreexecutor.Response
		err      error
	}
	executed := make(chan outcome, 1)
	go func() {
		response, err := executor.Execute(ctx, auth, request, options)
		executed <- outcome{response, err}
	}()
	select {
	case result := <-executed:
		if result.err != nil {
			return fmt.Errorf("execute after lifecycle transition: %w", result.err)
		}
		var body struct {
			Choices []struct {
				Message      struct{ Content string } `json:"message"`
				FinishReason string                   `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(result.response.Payload, &body) != nil || len(body.Choices) != 1 || body.Choices[0].Message.Content != "Hello native host" || body.Choices[0].FinishReason != "stop" || body.Usage.TotalTokens != 7 {
			return fmt.Errorf("aggregate after lifecycle transition lost content, finish or usage: %s", result.response.Payload)
		}
	case <-time.After(checkTimeout):
		return fmt.Errorf("execute after lifecycle transition did not complete")
	}
	if err := await(upstream.closed["fresh-execute"], "new aggregate upstream closure"); err != nil {
		return err
	}
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "fresh-execute"})

	request, options = requestFor(host, ctx, "fresh-stream", true)
	stream, err := executor.ExecuteStream(ctx, auth, request, options)
	if err != nil {
		return fmt.Errorf("stream after lifecycle transition: %w", err)
	}
	if stream == nil || stream.Chunks == nil {
		return fmt.Errorf("stream after lifecycle transition is missing its channel")
	}
	deadline := time.NewTimer(checkTimeout)
	defer deadline.Stop()
	var received bytes.Buffer
streamLoop:
	for {
		select {
		case chunk, open := <-stream.Chunks:
			if !open {
				break streamLoop
			}
			if chunk.Err != nil {
				return fmt.Errorf("new stream after lifecycle transition: %w", chunk.Err)
			}
			if !json.Valid(chunk.Payload) {
				return fmt.Errorf("Chat executor chunk is not JSON: %q", chunk.Payload)
			}
			received.Write(chunk.Payload)
		case <-deadline.C:
			return fmt.Errorf("stream after lifecycle transition did not complete")
		}
	}
	for _, marker := range []string{"Hello native host", `"total_tokens":7`, `"finish_reason":"stop"`} {
		if !strings.Contains(received.String(), marker) {
			return fmt.Errorf("stream after lifecycle transition missing %q: %s", marker, received.String())
		}
	}
	if err := await(upstream.closed["fresh-stream"], "new streaming upstream closure"); err != nil {
		return err
	}
	host.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "fresh-stream"})
	if ctx.Err() != nil {
		return fmt.Errorf("caller context canceled during fresh requests after lifecycle transition")
	}
	return nil
}
