package appserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mideco-tech/codex-tg/internal/control"
	"github.com/mideco-tech/codex-tg/internal/version"
)

type Event = control.Event
type TurnStartOptions = control.TurnStartOptions
type ModelOption = control.ModelOption
type CollaborationModeOption = control.CollaborationModeOption

var _ control.ControlPlane = (*Client)(nil)

type rpcResponse struct {
	Result any
	Error  error
}

type Client struct {
	codexBin       string
	listenURL      string
	cwd            string
	requestTimeout time.Duration

	startMu        sync.Mutex
	mu             sync.Mutex
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	stderr         io.ReadCloser
	ws             *webSocketProxyTransport
	pending        map[uint64]chan rpcResponse
	subscribers    []chan Event
	nextID         uint64
	generation     uint64
	stderrLines    []string
	started        bool
	readerDone     chan struct{}
	stderrDone     chan struct{}
	serverRequests map[string]map[string]any
}

func NewClient(codexBin, listenURL, cwd string, requestTimeout time.Duration) *Client {
	return &Client{
		codexBin:       codexBin,
		listenURL:      listenURL,
		cwd:            cwd,
		requestTimeout: requestTimeout,
		pending:        map[uint64]chan rpcResponse{},
		serverRequests: map[string]map[string]any{},
		readerDone:     make(chan struct{}),
		stderrDone:     make(chan struct{}),
	}
}

type webSocketProxyTransport struct {
	stdin  io.WriteCloser
	reader *bufio.Reader
	mu     sync.Mutex
}

func newWebSocketProxyTransport(ctx context.Context, stdin io.WriteCloser, stdout io.Reader) (*webSocketProxyTransport, error) {
	reader := bufio.NewReader(stdout)
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := "GET / HTTP/1.1\r\n" +
		"Host: codex-app-server\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if err := writeWithContext(ctx, stdin, []byte(request)); err != nil {
		return nil, err
	}
	if err := readWebSocketHandshake(ctx, reader, key); err != nil {
		return nil, err
	}
	return &webSocketProxyTransport{stdin: stdin, reader: reader}, nil
}

func writeWithContext(ctx context.Context, writer io.Writer, data []byte) error {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := writer.Write(data)
		done <- result{n: n, err: err}
	}()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			return outcome.err
		}
		if outcome.n != len(data) {
			return io.ErrShortWrite
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readWebSocketHandshake(ctx context.Context, reader *bufio.Reader, key string) error {
	type result struct {
		headers map[string]string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		headers, err := readWebSocketHandshakeHeaders(reader)
		done <- result{headers: headers, err: err}
	}()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			return outcome.err
		}
		status := strings.ToLower(outcome.headers[":status"])
		if !strings.Contains(status, "101") {
			return fmt.Errorf("websocket upgrade failed: %s", outcome.headers[":status"])
		}
		if !strings.EqualFold(outcome.headers["upgrade"], "websocket") {
			return fmt.Errorf("websocket upgrade missing Upgrade header")
		}
		if !headerContainsToken(outcome.headers["connection"], "upgrade") {
			return fmt.Errorf("websocket upgrade missing Connection header")
		}
		if got, want := outcome.headers["sec-websocket-accept"], webSocketAcceptKey(key); got != want {
			return fmt.Errorf("websocket upgrade accept mismatch")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readWebSocketHandshakeHeaders(reader *bufio.Reader) (map[string]string, error) {
	status, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	headers := map[string]string{":status": strings.TrimSpace(status)}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return headers, nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
}

func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func webSocketAcceptKey(key string) string {
	sum := sha1Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum)
}

func sha1Sum(data []byte) []byte {
	h := sha1New()
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func sha1New() hash.Hash {
	return sha1.New()
}

func (ws *webSocketProxyTransport) Write(payload []byte) (int, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := writeWebSocketFrame(ws.stdin, 0x1, payload, true); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (ws *webSocketProxyTransport) Close() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	_ = writeWebSocketFrame(ws.stdin, 0x8, nil, true)
	return ws.stdin.Close()
}

func (ws *webSocketProxyTransport) ReadText() ([]byte, error) {
	for {
		opcode, payload, err := readWebSocketFrame(ws.reader)
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x1:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			ws.mu.Lock()
			err := writeWebSocketFrame(ws.stdin, 0xA, payload, true)
			ws.mu.Unlock()
			if err != nil {
				return nil, err
			}
		case 0xA:
			continue
		default:
			continue
		}
	}
}

func writeWebSocketFrame(writer io.Writer, opcode byte, payload []byte, mask bool) error {
	header := []byte{0x80 | opcode}
	payloadLen := len(payload)
	switch {
	case payloadLen <= 125:
		header = append(header, byte(payloadLen))
	case payloadLen <= 0xFFFF:
		header = append(header, 126, byte(payloadLen>>8), byte(payloadLen))
	default:
		header = append(header, 127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(payloadLen))
		header = append(header, length[:]...)
	}
	if mask {
		header[1] |= 0x80
		var maskKey [4]byte
		if _, err := rand.Read(maskKey[:]); err != nil {
			return err
		}
		header = append(header, maskKey[:]...)
		masked := make([]byte, payloadLen)
		for i, b := range payload {
			masked[i] = b ^ maskKey[i%4]
		}
		payload = masked
	}
	if _, err := writer.Write(header); err != nil {
		return err
	}
	if payloadLen == 0 {
		return nil
	}
	_, err := writer.Write(payload)
	return err
}

func readWebSocketFrame(reader io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0F
	if !fin {
		return 0, nil, errors.New("fragmented websocket frames are not supported")
	}
	masked := header[1]&0x80 != 0
	payloadLen := uint64(header[1] & 0x7F)
	switch payloadLen {
	case 126:
		var length [2]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return 0, nil, err
		}
		payloadLen = uint64(binary.BigEndian.Uint16(length[:]))
	case 127:
		var length [8]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return 0, nil, err
		}
		payloadLen = binary.BigEndian.Uint64(length[:])
	}
	if payloadLen > 16*1024*1024 {
		return 0, nil, fmt.Errorf("websocket frame too large: %d", payloadLen)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(reader, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(payloadLen))
	if payloadLen > 0 {
		if _, err := io.ReadFull(reader, payload); err != nil {
			return 0, nil, err
		}
	}
	if masked {
		for i, b := range payload {
			payload[i] = b ^ maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

func (c *Client) Start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	cmd, err := c.buildCommand()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	writeCloser := io.WriteCloser(stdin)
	var ws *webSocketProxyTransport
	if proxy, _ := appServerProxyTarget(c.listenURL); proxy {
		ws, err = newWebSocketProxyTransport(ctx, stdin, stdout)
		if err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			_ = stderr.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
			return err
		}
		writeCloser = ws
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = writeCloser
	c.stdout = stdout
	c.stderr = stderr
	c.ws = ws
	c.started = true
	c.generation++
	generation := c.generation
	c.readerDone = make(chan struct{})
	c.stderrDone = make(chan struct{})
	c.mu.Unlock()

	go c.readStdout(generation)
	go c.readStderr(generation)
	if _, err := c.Request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "codex-tg",
			"title":   "codex-tg Telegram bridge",
			"version": version.Version,
		},
	}); err != nil {
		_ = c.closeRunning()
		return err
	}
	if err := c.Notify(ctx, "initialized", nil); err != nil {
		_ = c.closeRunning()
		return err
	}
	return nil
}

func (c *Client) Close() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.closeRunning()
}

func (c *Client) closeRunning() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	cmd := c.cmd
	stdin := c.stdin
	pending := c.pending
	c.pending = map[uint64]chan rpcResponse{}
	c.started = false
	c.generation++
	c.cmd = nil
	c.stdin = nil
	c.stdout = nil
	c.stderr = nil
	c.ws = nil
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	for _, ch := range pending {
		select {
		case ch <- rpcResponse{Error: errors.New("app-server closed before response")}:
		default:
		}
		close(ch)
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	return nil
}

func (c *Client) Subscribe() <-chan Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan Event, 128)
	c.subscribers = append(c.subscribers, ch)
	return ch
}

func (c *Client) Request(ctx context.Context, method string, params map[string]any) (any, error) {
	c.mu.Lock()
	if !c.started || c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("app-server is not running")
	}
	c.nextID++
	id := c.nextID
	reply := make(chan rpcResponse, 1)
	c.pending[id] = reply
	stdin := c.stdin
	c.mu.Unlock()

	message := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(stdin, string(payload)+"\n"); err != nil {
		c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "stdin", "method": method, "error": err.Error(), "stderr_tail": c.StderrTail()}})
		return nil, err
	}

	timeout := c.requestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		timeout = c.requestTimeout
	}

	select {
	case response := <-reply:
		if response.Error != nil {
			return nil, response.Error
		}
		return response.Result, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("request timeout for %s", method)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *Client) Notify(ctx context.Context, method string, params map[string]any) error {
	c.mu.Lock()
	if !c.started || c.stdin == nil {
		c.mu.Unlock()
		return errors.New("app-server is not running")
	}
	stdin := c.stdin
	c.mu.Unlock()
	message := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(stdin, string(payload)+"\n"); err != nil {
		c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "stdin", "method": method, "error": err.Error(), "stderr_tail": c.StderrTail()}})
		return err
	}
	return nil
}

func (c *Client) RespondServerRequest(ctx context.Context, requestID string, result map[string]any) error {
	c.mu.Lock()
	if !c.started || c.stdin == nil {
		c.mu.Unlock()
		return errors.New("app-server is not running")
	}
	stdin := c.stdin
	delete(c.serverRequests, requestID)
	c.mu.Unlock()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"result":  result,
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(stdin, string(payload)+"\n"); err != nil {
		c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "stdin", "method": "serverRequest/respond", "error": err.Error(), "stderr_tail": c.StderrTail()}})
		return err
	}
	return nil
}

func (c *Client) ThreadList(ctx context.Context, limit int, cursor string) (map[string]any, error) {
	params := map[string]any{"limit": limit, "sortKey": "updated_at"}
	if strings.TrimSpace(cursor) != "" {
		params["cursor"] = cursor
	}
	result, err := c.Request(ctx, "thread/list", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadFork(ctx context.Context, threadID, cwd string) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/fork", threadForkParams(threadID, cwd))
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func threadForkParams(threadID, cwd string) map[string]any {
	params := map[string]any{"threadId": threadID}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	return params
}

func (c *Client) ThreadSetName(ctx context.Context, threadID, name string) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/name/set", map[string]any{
		"threadId": threadID,
		"name":     name,
	})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadArchive(ctx context.Context, threadID string) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/archive", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadUnarchive(ctx context.Context, threadID string) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/unarchive", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadCompactStart(ctx context.Context, threadID string) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/compact/start", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadRollback(ctx context.Context, threadID string, numTurns int) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/rollback", map[string]any{
		"threadId": threadID,
		"numTurns": numTurns,
	})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	result, err := c.Request(ctx, "thread/read", map[string]any{
		"threadId":     threadID,
		"includeTurns": includeTurns,
	})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ThreadResume(ctx context.Context, threadID, cwd string) (map[string]any, error) {
	params := map[string]any{
		"threadId":               threadID,
		"persistExtendedHistory": true,
	}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	result, err := c.Request(ctx, "thread/resume", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) TurnStart(ctx context.Context, threadID, message, cwd string, options TurnStartOptions) (map[string]any, error) {
	resolved, err := c.resolveTurnStartOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	params, err := turnStartParams(threadID, message, cwd, resolved)
	if err != nil {
		return nil, err
	}
	result, err := c.Request(ctx, "turn/start", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func TurnStartParams(threadID, message, cwd string, options TurnStartOptions) (map[string]any, error) {
	return turnStartParams(threadID, message, cwd, options)
}

func turnStartParams(threadID, message, cwd string, options TurnStartOptions) (map[string]any, error) {
	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{"type": "text", "text": message, "text_elements": []any{}},
		},
	}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	mode := normalizeCollaborationMode(options.CollaborationMode)
	if mode != "" {
		model := strings.TrimSpace(options.Model)
		if model == "" {
			return nil, fmt.Errorf("codex model is required for collaboration mode %q", mode)
		}
		settings := map[string]any{
			"model":                  model,
			"reasoning_effort":       normalizeReasoningEffort(options.ReasoningEffort),
			"developer_instructions": nil,
		}
		if settings["reasoning_effort"] == "" {
			settings["reasoning_effort"] = nil
		}
		params["collaborationMode"] = map[string]any{
			"mode":     mode,
			"settings": settings,
		}
	}
	return params, nil
}

func (c *Client) resolveTurnStartOptions(ctx context.Context, options TurnStartOptions) (TurnStartOptions, error) {
	options.CollaborationMode = normalizeCollaborationMode(options.CollaborationMode)
	options.Model = strings.TrimSpace(options.Model)
	options.ReasoningEffort = normalizeReasoningEffort(options.ReasoningEffort)
	if options.CollaborationMode == "" {
		return options, nil
	}
	if options.Model == "" {
		model, err := c.defaultModel(ctx)
		if err != nil {
			return options, fmt.Errorf("codex model is required for collaboration mode %q; choose one with /model or fix model/list: %w", options.CollaborationMode, err)
		}
		options.Model = model
	}
	if options.ReasoningEffort == "" {
		if effort, err := c.collaborationModeReasoningEffort(ctx, options.CollaborationMode); err == nil {
			options.ReasoningEffort = effort
		}
	}
	return options, nil
}

func (c *Client) defaultModel(ctx context.Context) (string, error) {
	models, err := c.ModelList(ctx, false)
	if err != nil {
		return "", err
	}
	first := ""
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if first == "" {
			first = model.ID
		}
		if model.IsDefault {
			return model.ID, nil
		}
	}
	if first != "" {
		return first, nil
	}
	return "", errors.New("model/list returned no models")
}

func (c *Client) collaborationModeReasoningEffort(ctx context.Context, mode string) (string, error) {
	modes, err := c.CollaborationModeList(ctx)
	if err != nil {
		return "", err
	}
	for _, preset := range modes {
		if normalizeCollaborationMode(preset.Mode) != mode {
			continue
		}
		return normalizeReasoningEffort(preset.ReasoningEffort), nil
	}
	return "", nil
}

func (c *Client) ModelList(ctx context.Context, includeHidden bool) ([]ModelOption, error) {
	params := map[string]any{"limit": 50}
	if includeHidden {
		params["includeHidden"] = true
	}
	result, err := c.Request(ctx, "model/list", params)
	if err != nil {
		return nil, err
	}
	return modelOptionsFromResult(result), nil
}

func (c *Client) CollaborationModeList(ctx context.Context) ([]CollaborationModeOption, error) {
	result, err := c.Request(ctx, "collaborationMode/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return collaborationModeOptionsFromResult(result), nil
}

func (c *Client) SkillsList(ctx context.Context, cwds []string, forceReload bool) (map[string]any, error) {
	result, err := c.Request(ctx, "skills/list", skillsListParams(cwds, forceReload))
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func skillsListParams(cwds []string, forceReload bool) map[string]any {
	params := map[string]any{}
	if len(cwds) > 0 {
		params["cwds"] = cwds
	}
	if forceReload {
		params["forceReload"] = true
	}
	return params
}

func (c *Client) PluginSkillRead(ctx context.Context, remoteMarketplaceName, remotePluginID, skillName string) (map[string]any, error) {
	result, err := c.Request(ctx, "plugin/skill/read", map[string]any{
		"remoteMarketplaceName": remoteMarketplaceName,
		"remotePluginId":        remotePluginID,
		"skillName":             skillName,
	})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) HooksList(ctx context.Context, cwds []string) (map[string]any, error) {
	params := map[string]any{}
	if len(cwds) > 0 {
		params["cwds"] = cwds
	}
	result, err := c.Request(ctx, "hooks/list", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) MCPServerStatusList(ctx context.Context, limit int, cursor string, detail bool) (map[string]any, error) {
	params := map[string]any{}
	if limit > 0 {
		params["limit"] = limit
	}
	if strings.TrimSpace(cursor) != "" {
		params["cursor"] = cursor
	}
	if detail {
		params["detail"] = true
	}
	result, err := c.Request(ctx, "mcpServerStatus/list", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) AppList(ctx context.Context, limit int, cursor, threadID string, forceRefetch bool) (map[string]any, error) {
	params := map[string]any{}
	if limit > 0 {
		params["limit"] = limit
	}
	if strings.TrimSpace(cursor) != "" {
		params["cursor"] = cursor
	}
	if strings.TrimSpace(threadID) != "" {
		params["threadId"] = threadID
	}
	if forceRefetch {
		params["forceRefetch"] = true
	}
	result, err := c.Request(ctx, "app/list", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) ConfigRead(ctx context.Context, cwd string, includeLayers bool) (map[string]any, error) {
	params := map[string]any{}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	if includeLayers {
		params["includeLayers"] = true
	}
	result, err := c.Request(ctx, "config/read", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func modelOptionsFromResult(result any) []ModelOption {
	data, _ := asMap(result)["data"].([]any)
	out := make([]ModelOption, 0, len(data))
	for _, item := range data {
		model := asMap(item)
		id := strings.TrimSpace(stringValue(model["model"], stringValue(model["id"], "")))
		if id == "" {
			continue
		}
		out = append(out, ModelOption{
			ID:                       id,
			DisplayName:              strings.TrimSpace(stringValue(model["displayName"], "")),
			Description:              strings.TrimSpace(stringValue(model["description"], "")),
			DefaultReasoningEffort:   normalizeReasoningEffort(stringValue(model["defaultReasoningEffort"], "")),
			SupportedReasoningEffort: supportedReasoningEfforts(model["supportedReasoningEfforts"]),
			IsDefault:                boolValue(model["isDefault"]),
			Hidden:                   boolValue(model["hidden"]),
		})
	}
	return out
}

func collaborationModeOptionsFromResult(result any) []CollaborationModeOption {
	data, _ := asMap(result)["data"].([]any)
	out := make([]CollaborationModeOption, 0, len(data))
	for _, item := range data {
		preset := asMap(item)
		out = append(out, CollaborationModeOption{
			Name:            strings.TrimSpace(stringValue(preset["name"], "")),
			Mode:            normalizeCollaborationMode(stringValue(preset["mode"], "")),
			Model:           strings.TrimSpace(stringValue(preset["model"], "")),
			ReasoningEffort: normalizeReasoningEffort(firstStringValue(preset["reasoning_effort"], preset["reasoningEffort"])),
		})
	}
	return out
}

func supportedReasoningEfforts(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		option := asMap(item)
		effort := normalizeReasoningEffort(firstStringValue(option["reasoning_effort"], option["reasoningEffort"], item))
		if effort == "" {
			continue
		}
		if _, ok := seen[effort]; ok {
			continue
		}
		seen[effort] = struct{}{}
		out = append(out, effort)
	}
	return out
}

func normalizeCollaborationMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "plan", "plan_mode", "plan-mode":
		return "plan"
	case "default":
		return "default"
	default:
		return ""
	}
}

func normalizeReasoningEffort(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch normalized {
	case "":
		return ""
	case "x-high", "x_high", "extra-high", "extra_high":
		return "xhigh"
	default:
		return normalized
	}
}

func firstStringValue(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(stringValue(value, "")); text != "" {
			return text
		}
	}
	return ""
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, _ := strconv.ParseBool(typed)
		return parsed
	default:
		return false
	}
}

func (c *Client) ThreadStart(ctx context.Context, cwd string) (map[string]any, error) {
	params := map[string]any{
		"experimentalRawEvents":  false,
		"persistExtendedHistory": true,
	}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}
	result, err := c.Request(ctx, "thread/start", params)
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	_, err := c.Request(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

func (c *Client) TurnSteer(ctx context.Context, threadID, turnID, message string) (map[string]any, error) {
	result, err := c.Request(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input": []map[string]any{
			{
				"type":          "text",
				"text":          message,
				"text_elements": []any{},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return asMap(result), nil
}

func (c *Client) StderrTail() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.stderrLines))
	copy(out, c.stderrLines)
	return out
}

func (c *Client) buildCommand() (*exec.Cmd, error) {
	executable, err := exec.LookPath(c.codexBin)
	if err != nil {
		executable = c.codexBin
	}
	if proxy, sock := appServerProxyTarget(c.listenURL); proxy {
		args := []string{"app-server", "proxy"}
		if sock != "" {
			args = append(args, "--sock", sock)
		}
		cmd := exec.Command(executable, args...)
		cmd.Dir = c.cwd
		return cmd, nil
	}
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(executable))
		if ext == ".cmd" || ext == ".bat" {
			command := fmt.Sprintf("%s app-server --listen %s", executable, c.listenURL)
			cmd := exec.Command(os.Getenv("ComSpec"), "/d", "/c", command)
			cmd.Dir = c.cwd
			return cmd, nil
		}
	}
	cmd := exec.Command(executable, "app-server", "--listen", c.listenURL)
	cmd.Dir = c.cwd
	return cmd, nil
}

func appServerProxyTarget(listenURL string) (bool, string) {
	listenURL = strings.TrimSpace(listenURL)
	switch {
	case listenURL == "proxy", listenURL == "proxy://":
		return true, ""
	case strings.HasPrefix(listenURL, "proxy://"):
		return true, strings.TrimPrefix(listenURL, "proxy://")
	default:
		return false, ""
	}
}

func (c *Client) readStdout(generation uint64) {
	defer close(c.readerDone)
	c.mu.Lock()
	stdout := c.stdout
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		c.readWebSocket(ws, generation)
		return
	}
	if stdout == nil {
		return
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if !c.isStartedGeneration(generation) {
			return
		}
		line := scanner.Bytes()
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "stdout", "generation": generation, "error": err.Error(), "line_len": len(line), "stderr_tail": c.StderrTail()}})
			continue
		}
		c.handlePayload(payload, generation)
	}
	if !c.isStartedGeneration(generation) {
		return
	}
	if err := scanner.Err(); err != nil {
		c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "stdout", "generation": generation, "error": err.Error(), "stderr_tail": c.StderrTail()}})
		return
	}
	c.broadcast(Event{Channel: "transport_closed", Params: map[string]any{"stream": "stdout", "generation": generation, "reason": "eof", "stderr_tail": c.StderrTail()}})
}

func (c *Client) readWebSocket(ws *webSocketProxyTransport, generation uint64) {
	for {
		if !c.isStartedGeneration(generation) {
			return
		}
		message, err := ws.ReadText()
		if err != nil {
			if !c.isStartedGeneration(generation) {
				return
			}
			if errors.Is(err, io.EOF) {
				c.broadcast(Event{Channel: "transport_closed", Params: map[string]any{"stream": "websocket", "generation": generation, "reason": "eof", "stderr_tail": c.StderrTail()}})
			} else {
				c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "websocket", "generation": generation, "error": err.Error(), "stderr_tail": c.StderrTail()}})
			}
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(message, &payload); err != nil {
			c.broadcast(Event{Channel: "transport_error", Params: map[string]any{"stream": "websocket", "generation": generation, "error": err.Error(), "line_len": len(message), "stderr_tail": c.StderrTail()}})
			continue
		}
		c.handlePayload(payload, generation)
	}
}

func (c *Client) isStartedGeneration(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started && c.generation == generation
}

func (c *Client) readStderr(generation uint64) {
	defer close(c.stderrDone)
	c.mu.Lock()
	stderr := c.stderr
	c.mu.Unlock()
	if stderr == nil {
		return
	}
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 512), 64*1024)
	for scanner.Scan() {
		if !c.isStartedGeneration(generation) {
			return
		}
		line := scanner.Text()
		c.mu.Lock()
		c.stderrLines = append(c.stderrLines, line)
		if len(c.stderrLines) > 100 {
			c.stderrLines = c.stderrLines[len(c.stderrLines)-100:]
		}
		c.mu.Unlock()
	}
}

func (c *Client) handlePayload(payload map[string]any, generation uint64) {
	if id, ok := payload["id"]; ok {
		if _, hasResult := payload["result"]; hasResult || payload["error"] != nil {
			responseID := uint64FromAny(id)
			c.mu.Lock()
			if !c.started || c.generation != generation {
				c.mu.Unlock()
				return
			}
			reply := c.pending[responseID]
			delete(c.pending, responseID)
			c.mu.Unlock()
			if reply != nil {
				if payload["error"] != nil {
					reply <- rpcResponse{Error: fmt.Errorf("%v", payload["error"])}
				} else {
					reply <- rpcResponse{Result: payload["result"]}
				}
				close(reply)
			}
			return
		}
		if method, ok := payload["method"].(string); ok {
			requestID := rpcString(id)
			if requestID == "" {
				return
			}
			params := asMap(payload["params"])
			c.mu.Lock()
			if !c.started || c.generation != generation {
				c.mu.Unlock()
				return
			}
			c.serverRequests[requestID] = params
			c.mu.Unlock()
			c.broadcast(Event{Channel: "server_request", Method: method, Params: params, ID: id})
			return
		}
	}
	method, _ := payload["method"].(string)
	params := asMap(payload["params"])
	if strings.EqualFold(method, "serverRequest/resolved") {
		if requestID := rpcString(params["requestId"]); requestID != "" {
			c.mu.Lock()
			if !c.started || c.generation != generation {
				c.mu.Unlock()
				return
			}
			delete(c.serverRequests, requestID)
			c.mu.Unlock()
		}
	}
	if !c.isStartedGeneration(generation) {
		return
	}
	c.broadcast(Event{Channel: "notification", Method: method, Params: params})
}

func rpcString(value any) string {
	if value == nil {
		return ""
	}
	out := strings.TrimSpace(fmt.Sprintf("%v", value))
	if out == "" || out == "<nil>" {
		return ""
	}
	return out
}

func (c *Client) broadcast(event Event) {
	c.mu.Lock()
	subs := append([]chan Event(nil), c.subscribers...)
	c.mu.Unlock()
	for _, subscriber := range subs {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func asMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func uint64FromAny(value any) uint64 {
	switch typed := value.(type) {
	case float64:
		return uint64(typed)
	case int:
		return uint64(typed)
	case int64:
		return uint64(typed)
	case uint64:
		return typed
	default:
		return 0
	}
}
