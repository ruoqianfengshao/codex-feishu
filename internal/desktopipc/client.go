package desktopipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultClientType = "codex-feishu"
	defaultTimeout    = 5 * time.Second
)

var ErrNoClientFound = errors.New("codex desktop ipc: no client found")

type Client struct {
	socketPath string
	timeout    time.Duration
	clientType string

	connMu  sync.Mutex
	conn    net.Conn
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]pendingRequest
	nextID    uint64

	ownerMu      sync.RWMutex
	threadOwners map[string]string
}

type response struct {
	Result            map[string]any
	Err               error
	HandledByClientID string
}

type pendingRequest struct {
	ch             chan response
	broadcastMatch func(wireMessage) (map[string]any, bool)
}

type wireMessage struct {
	Type               string         `json:"type"`
	RequestID          string         `json:"requestId,omitempty"`
	Method             string         `json:"method,omitempty"`
	Version            int            `json:"version,omitempty"`
	Params             map[string]any `json:"params,omitempty"`
	ResultType         string         `json:"resultType,omitempty"`
	Result             map[string]any `json:"result,omitempty"`
	Error              string         `json:"error,omitempty"`
	HandledByClientID  string         `json:"handledByClientId,omitempty"`
	SourceClientID     string         `json:"sourceClientId,omitempty"`
	TargetClientID     string         `json:"targetClientId,omitempty"`
	OriginalRequestID  string         `json:"originalRequestId,omitempty"`
	OriginalResultType string         `json:"originalResultType,omitempty"`
	Request            map[string]any `json:"request,omitempty"`
	Response           map[string]any `json:"response,omitempty"`
}

func New(socketPath string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		socketPath:   strings.TrimSpace(socketPath),
		timeout:      timeout,
		clientType:   defaultClientType,
		pending:      map[string]pendingRequest{},
		threadOwners: map[string]string{},
	}
}

func DefaultSocketPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	tmp := os.Getenv("TMPDIR")
	if strings.TrimSpace(tmp) == "" {
		return ""
	}
	return filepath.Join(tmp, "codex-ipc", fmt.Sprintf("ipc-%d.sock", os.Getuid()))
}

func (c *Client) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.failAll(net.ErrClosed)
	return err
}

func (c *Client) LoadCompleteHistory(ctx context.Context, threadID string) (map[string]any, error) {
	threadID = strings.TrimSpace(threadID)
	return c.requestWithBroadcast(ctx, "thread-follower-load-complete-history", 1, map[string]any{
		"conversationId": threadID,
	}, "", threadID, func(msg wireMessage) (map[string]any, bool) {
		if msg.Type != "broadcast" || msg.Method != "thread-stream-state-changed" {
			return nil, false
		}
		if stringParam(msg.Params, "conversationId") != threadID {
			return nil, false
		}
		change, _ := msg.Params["change"].(map[string]any)
		if stringParam(change, "type") != "snapshot" {
			return nil, false
		}
		result := map[string]any{"fromBroadcast": true}
		if revision, ok := change["revision"]; ok {
			result["revision"] = revision
		}
		if msg.SourceClientID != "" {
			result["ownerClientId"] = msg.SourceClientID
		}
		return result, true
	})
}

func (c *Client) StartTurn(ctx context.Context, threadID string, turnStartParams map[string]any) (map[string]any, error) {
	threadID = strings.TrimSpace(threadID)
	return c.requestForThread(ctx, threadID, "thread-follower-start-turn", 1, map[string]any{
		"conversationId":  threadID,
		"turnStartParams": turnStartParams,
	})
}

func (c *Client) SteerTurn(ctx context.Context, threadID string, input []map[string]any, restoreMessage map[string]any) (map[string]any, error) {
	threadID = strings.TrimSpace(threadID)
	params := map[string]any{
		"conversationId": threadID,
		"input":          input,
	}
	if restoreMessage != nil {
		params["restoreMessage"] = restoreMessage
	}
	return c.requestForThread(ctx, threadID, "thread-follower-steer-turn", 1, params)
}

func (c *Client) request(ctx context.Context, method string, version int, params map[string]any) (map[string]any, error) {
	return c.requestWithBroadcast(ctx, method, version, params, "", "", nil)
}

func (c *Client) requestForThread(ctx context.Context, threadID, method string, version int, params map[string]any) (map[string]any, error) {
	return c.requestWithBroadcast(ctx, method, version, params, c.threadOwner(threadID), threadID, nil)
}

func (c *Client) requestWithBroadcast(ctx context.Context, method string, version int, params map[string]any, targetClientID, ownerThreadID string, broadcastMatch func(wireMessage) (map[string]any, bool)) (map[string]any, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	id := c.nextRequestID()
	ch := make(chan response, 1)
	c.pendingMu.Lock()
	c.pending[id] = pendingRequest{ch: ch, broadcastMatch: broadcastMatch}
	c.pendingMu.Unlock()
	msg := wireMessage{
		Type:      "request",
		RequestID: id,
		Method:    method,
		Version:   version,
		Params:    params,
	}
	if targetClientID = strings.TrimSpace(targetClientID); targetClientID != "" {
		msg.TargetClientID = targetClientID
	}
	if err := c.writeMessage(ctx, msg); err != nil {
		c.deletePending(id)
		_ = c.Close()
		return nil, err
	}
	select {
	case resp := <-ch:
		c.rememberThreadOwner(ownerThreadID, resp.HandledByClientID)
		c.rememberThreadOwnerFromResult(ownerThreadID, resp.Result)
		return resp.Result, resp.Err
	case <-ctx.Done():
		c.deletePending(id)
		return nil, ctx.Err()
	}
}

func (c *Client) ensureConnected(ctx context.Context) error {
	c.connMu.Lock()
	if c.conn != nil {
		c.connMu.Unlock()
		return nil
	}
	path := c.socketPath
	if path == "" {
		path = DefaultSocketPath()
	}
	if path == "" {
		c.connMu.Unlock()
		return errors.New("codex desktop ipc socket path is unavailable")
	}
	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		c.connMu.Unlock()
		return err
	}
	c.conn = conn
	c.connMu.Unlock()
	go c.readLoop(conn)
	if _, err := c.requestInitialized(ctx); err != nil {
		_ = conn.Close()
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
		return err
	}
	return nil
}

func (c *Client) requestInitialized(ctx context.Context) (map[string]any, error) {
	id := c.nextRequestID()
	ch := make(chan response, 1)
	c.pendingMu.Lock()
	c.pending[id] = pendingRequest{ch: ch}
	c.pendingMu.Unlock()
	if err := c.writeMessage(ctx, wireMessage{
		Type:      "request",
		RequestID: id,
		Method:    "initialize",
		Params: map[string]any{
			"clientType": c.clientType,
		},
	}); err != nil {
		c.deletePending(id)
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp.Result, resp.Err
	case <-ctx.Done():
		c.deletePending(id)
		return nil, ctx.Err()
	}
}

func (c *Client) readLoop(conn net.Conn) {
	defer c.clearConn(conn)
	for {
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			c.failAll(err)
			return
		}
		length := binary.LittleEndian.Uint32(header[:])
		if length == 0 || length > 64*1024*1024 {
			c.failAll(fmt.Errorf("codex desktop ipc invalid frame length: %d", length))
			_ = c.Close()
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			c.failAll(err)
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		c.handleMessage(msg)
	}
}

func (c *Client) clearConn(conn net.Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
}

func (c *Client) handleMessage(msg wireMessage) {
	if msg.Type == "client-discovery-request" {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		_ = c.writeMessage(ctx, wireMessage{
			Type:      "client-discovery-response",
			RequestID: msg.RequestID,
			Response:  map[string]any{"canHandle": false},
		})
		return
	}
	if msg.Type == "broadcast" {
		c.handleBroadcast(msg)
		return
	}
	if msg.Type != "response" {
		return
	}
	ch := c.deletePending(msg.RequestID)
	if ch == nil {
		return
	}
	if msg.ResultType == "error" || msg.Error != "" {
		ch <- response{Err: classifyError(msg.Error), HandledByClientID: msg.HandledByClientID}
		return
	}
	ch <- response{Result: msg.Result, HandledByClientID: msg.HandledByClientID}
}

func (c *Client) handleBroadcast(msg wireMessage) {
	var deliveries []chan response
	var results []map[string]any
	c.pendingMu.Lock()
	for id, pending := range c.pending {
		if pending.broadcastMatch == nil {
			continue
		}
		result, ok := pending.broadcastMatch(msg)
		if !ok {
			continue
		}
		delete(c.pending, id)
		deliveries = append(deliveries, pending.ch)
		results = append(results, result)
	}
	c.pendingMu.Unlock()
	for i, ch := range deliveries {
		ch <- response{Result: results[i]}
	}
}

func (c *Client) writeMessage(ctx context.Context, msg wireMessage) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return errors.New("codex desktop ipc is not connected")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(body)))
	payload := append(header, body...)
	done := make(chan error, 1)
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		_, err := conn.Write(payload)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) nextRequestID() string {
	return fmt.Sprintf("%d", atomic.AddUint64(&c.nextID, 1))
}

func (c *Client) deletePending(id string) chan response {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	pending := c.pending[id]
	delete(c.pending, id)
	return pending.ch
}

func (c *Client) failAll(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, pending := range c.pending {
		delete(c.pending, id)
		pending.ch <- response{Err: err}
	}
}

func (c *Client) threadOwner(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	return c.threadOwners[threadID]
}

func (c *Client) rememberThreadOwner(threadID, clientID string) {
	threadID = strings.TrimSpace(threadID)
	clientID = strings.TrimSpace(clientID)
	if threadID == "" || clientID == "" {
		return
	}
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	c.threadOwners[threadID] = clientID
}

func (c *Client) rememberThreadOwnerFromResult(threadID string, result map[string]any) {
	if result == nil {
		return
	}
	c.rememberThreadOwner(threadID, stringParam(result, "ownerClientId"))
}

func stringParam(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	value, _ := params[key].(string)
	return strings.TrimSpace(value)
}

func classifyError(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("codex desktop ipc request failed")
	}
	if strings.Contains(strings.ToLower(value), "no-client-found") {
		return fmt.Errorf("%w: %s", ErrNoClientFound, value)
	}
	return errors.New(value)
}
