package desktopipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientStartTurnFramesDesktopFollowerRequest(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	requests := make(chan wireMessage, 2)
	go serveDesktopIPCTest(t, listener, requests)

	client := New(socketPath, time.Second)
	defer client.Close()
	result, err := client.StartTurn(context.Background(), "thread-1", map[string]any{
		"threadId": "thread-1",
		"input": []map[string]any{
			{"type": "text", "text": "hello", "text_elements": []any{}},
		},
	})
	if err != nil {
		t.Fatalf("StartTurn failed: %v", err)
	}
	if got, want := result["ok"], true; got != want {
		t.Fatalf("result ok = %v, want %v", got, want)
	}

	initialize := <-requests
	if got, want := initialize.Method, "initialize"; got != want {
		t.Fatalf("initialize method = %q, want %q", got, want)
	}
	request := <-requests
	if got, want := request.Method, "thread-follower-start-turn"; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
	if got, want := request.Version, 1; got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
	if got, want := request.Params["conversationId"], "thread-1"; got != want {
		t.Fatalf("conversationId = %v, want %q", got, want)
	}
	params, ok := request.Params["turnStartParams"].(map[string]any)
	if !ok {
		t.Fatalf("turnStartParams = %#v, want object", request.Params["turnStartParams"])
	}
	if got, want := params["threadId"], "thread-1"; got != want {
		t.Fatalf("turnStartParams.threadId = %v, want %q", got, want)
	}
}

func TestClientSteerTurnFramesRestoreMessage(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	requests := make(chan wireMessage, 2)
	go serveDesktopIPCTest(t, listener, requests)

	client := New(socketPath, time.Second)
	defer client.Close()
	restoreMessage := map[string]any{
		"cwd": "/Users/example/project",
		"context": map[string]any{
			"workspaceRoots": []string{"/Users/example/project"},
		},
		"responsesapiClientMetadata": map[string]any{},
	}
	result, err := client.SteerTurn(context.Background(), "thread-1", desktopTextInputForTest("hello"), restoreMessage)
	if err != nil {
		t.Fatalf("SteerTurn failed: %v", err)
	}
	if got, want := result["ok"], true; got != want {
		t.Fatalf("result ok = %v, want %v", got, want)
	}

	<-requests
	request := <-requests
	if got, want := request.Method, "thread-follower-steer-turn"; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
	if got, want := request.Params["conversationId"], "thread-1"; got != want {
		t.Fatalf("conversationId = %v, want %q", got, want)
	}
	restore, ok := request.Params["restoreMessage"].(map[string]any)
	if !ok {
		t.Fatalf("restoreMessage = %#v, want object", request.Params["restoreMessage"])
	}
	if got, want := restore["cwd"], "/Users/example/project"; got != want {
		t.Fatalf("restoreMessage.cwd = %v, want %q", got, want)
	}
	contextPayload, ok := restore["context"].(map[string]any)
	if !ok {
		t.Fatalf("restoreMessage.context = %#v, want object", restore["context"])
	}
	roots, ok := contextPayload["workspaceRoots"].([]any)
	if !ok {
		t.Fatalf("workspaceRoots = %#v, want array", contextPayload["workspaceRoots"])
	}
	if len(roots) != 1 || roots[0] != "/Users/example/project" {
		t.Fatalf("workspaceRoots = %#v, want project cwd", roots)
	}
}

func desktopTextInputForTest(text string) []map[string]any {
	return []map[string]any{
		{"type": "text", "text": text, "text_elements": []any{}},
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ctr-ipc-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestClientClassifiesNoClientFound(t *testing.T) {
	t.Parallel()

	err := classifyError("no-client-found")
	if !errors.Is(err, ErrNoClientFound) {
		t.Fatalf("classifyError = %v, want ErrNoClientFound", err)
	}
}

func TestClientRepliesCannotHandleDiscoveryRequests(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	discoveryResponse := make(chan wireMessage, 1)
	go serveDiscoveryTest(t, listener, discoveryResponse)

	client := New(socketPath, time.Second)
	defer client.Close()
	if _, err := client.LoadCompleteHistory(context.Background(), "thread-1"); err != nil {
		t.Fatalf("LoadCompleteHistory failed: %v", err)
	}
	response := <-discoveryResponse
	if got, want := response.Type, "client-discovery-response"; got != want {
		t.Fatalf("response type = %q, want %q", got, want)
	}
	if got, want := response.RequestID, "discover-1"; got != want {
		t.Fatalf("request id = %q, want %q", got, want)
	}
	if got, want := response.Response["canHandle"], false; got != want {
		t.Fatalf("canHandle = %v, want %v", got, want)
	}
}

func TestClientLoadCompleteHistoryAcceptsSnapshotBroadcast(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	requests := make(chan wireMessage, 2)
	go serveLoadHistoryBroadcastTest(t, listener, requests)

	client := New(socketPath, time.Second)
	defer client.Close()
	result, err := client.LoadCompleteHistory(context.Background(), "thread-1")
	if err != nil {
		t.Fatalf("LoadCompleteHistory failed: %v", err)
	}
	if got, want := result["fromBroadcast"], true; got != want {
		t.Fatalf("fromBroadcast = %v, want %v", got, want)
	}
	if got, want := result["ownerClientId"], "desktop-owner"; got != want {
		t.Fatalf("ownerClientId = %v, want %q", got, want)
	}

	<-requests
	request := <-requests
	if got, want := request.Method, "thread-follower-load-complete-history"; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
}

func TestClientStartTurnTargetsOwnerFromSnapshotBroadcast(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	requests := make(chan wireMessage, 3)
	go serveLoadHistoryThenStartTurnTest(t, listener, requests)

	client := New(socketPath, time.Second)
	defer client.Close()
	if _, err := client.LoadCompleteHistory(context.Background(), "thread-1"); err != nil {
		t.Fatalf("LoadCompleteHistory failed: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), "thread-1", map[string]any{"threadId": "thread-1"}); err != nil {
		t.Fatalf("StartTurn failed: %v", err)
	}

	<-requests
	<-requests
	request := <-requests
	if got, want := request.Method, "thread-follower-start-turn"; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
	if got, want := request.TargetClientID, "desktop-owner"; got != want {
		t.Fatalf("targetClientId = %q, want %q", got, want)
	}
}

func TestClientSteerTurnTargetsOwnerFromHandledByClientID(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	requests := make(chan wireMessage, 3)
	go serveLoadHistoryResponseThenSteerTurnTest(t, listener, requests)

	client := New(socketPath, time.Second)
	defer client.Close()
	if _, err := client.LoadCompleteHistory(context.Background(), "thread-1"); err != nil {
		t.Fatalf("LoadCompleteHistory failed: %v", err)
	}
	if _, err := client.SteerTurn(context.Background(), "thread-1", desktopTextInputForTest("hello"), nil); err != nil {
		t.Fatalf("SteerTurn failed: %v", err)
	}

	<-requests
	<-requests
	request := <-requests
	if got, want := request.Method, "thread-follower-steer-turn"; got != want {
		t.Fatalf("method = %q, want %q", got, want)
	}
	if got, want := request.TargetClientID, "desktop-handler"; got != want {
		t.Fatalf("targetClientId = %q, want %q", got, want)
	}
}

func serveDesktopIPCTest(t *testing.T, listener net.Listener, requests chan<- wireMessage) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		t.Errorf("Accept failed: %v", err)
		return
	}
	defer conn.Close()
	for {
		msg, err := readWireMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Errorf("readWireMessage failed: %v", err)
			return
		}
		requests <- msg
		result := map[string]any{"ok": true}
		if msg.Method == "initialize" {
			result = map[string]any{"clientId": "test-client"}
		}
		if err := writeWireMessage(conn, wireMessage{
			Type:              "response",
			RequestID:         msg.RequestID,
			Method:            msg.Method,
			ResultType:        "success",
			HandledByClientID: "desktop-client",
			Result:            result,
		}); err != nil {
			t.Errorf("writeWireMessage failed: %v", err)
			return
		}
	}
}

func serveLoadHistoryThenStartTurnTest(t *testing.T, listener net.Listener, requests chan<- wireMessage) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		t.Errorf("Accept failed: %v", err)
		return
	}
	defer conn.Close()
	for {
		msg, err := readWireMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Errorf("readWireMessage failed: %v", err)
			return
		}
		requests <- msg
		switch msg.Method {
		case "initialize":
			if err := writeWireMessage(conn, wireMessage{
				Type:       "response",
				RequestID:  msg.RequestID,
				Method:     msg.Method,
				ResultType: "success",
				Result:     map[string]any{"clientId": "test-client"},
			}); err != nil {
				t.Errorf("write initialize failed: %v", err)
				return
			}
		case "thread-follower-load-complete-history":
			if err := writeWireMessage(conn, wireMessage{
				Type:           "broadcast",
				Method:         "thread-stream-state-changed",
				SourceClientID: "desktop-owner",
				Params: map[string]any{
					"conversationId": "thread-1",
					"change": map[string]any{
						"type": "snapshot",
					},
				},
			}); err != nil {
				t.Errorf("write broadcast failed: %v", err)
				return
			}
		case "thread-follower-start-turn":
			if err := writeWireMessage(conn, wireMessage{
				Type:       "response",
				RequestID:  msg.RequestID,
				Method:     msg.Method,
				ResultType: "success",
				Result:     map[string]any{"ok": true},
			}); err != nil {
				t.Errorf("write start response failed: %v", err)
			}
			return
		}
	}
}

func serveLoadHistoryResponseThenSteerTurnTest(t *testing.T, listener net.Listener, requests chan<- wireMessage) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		t.Errorf("Accept failed: %v", err)
		return
	}
	defer conn.Close()
	for {
		msg, err := readWireMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Errorf("readWireMessage failed: %v", err)
			return
		}
		requests <- msg
		switch msg.Method {
		case "initialize":
			if err := writeWireMessage(conn, wireMessage{
				Type:       "response",
				RequestID:  msg.RequestID,
				Method:     msg.Method,
				ResultType: "success",
				Result:     map[string]any{"clientId": "test-client"},
			}); err != nil {
				t.Errorf("write initialize failed: %v", err)
				return
			}
		case "thread-follower-load-complete-history":
			if err := writeWireMessage(conn, wireMessage{
				Type:              "response",
				RequestID:         msg.RequestID,
				Method:            msg.Method,
				ResultType:        "success",
				HandledByClientID: "desktop-handler",
				Result:            map[string]any{"ok": true},
			}); err != nil {
				t.Errorf("write load response failed: %v", err)
				return
			}
		case "thread-follower-steer-turn":
			if err := writeWireMessage(conn, wireMessage{
				Type:       "response",
				RequestID:  msg.RequestID,
				Method:     msg.Method,
				ResultType: "success",
				Result:     map[string]any{"ok": true},
			}); err != nil {
				t.Errorf("write steer response failed: %v", err)
			}
			return
		}
	}
}

func serveLoadHistoryBroadcastTest(t *testing.T, listener net.Listener, requests chan<- wireMessage) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		t.Errorf("Accept failed: %v", err)
		return
	}
	defer conn.Close()
	for {
		msg, err := readWireMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Errorf("readWireMessage failed: %v", err)
			return
		}
		requests <- msg
		if msg.Method == "initialize" {
			if err := writeWireMessage(conn, wireMessage{
				Type:              "response",
				RequestID:         msg.RequestID,
				Method:            msg.Method,
				ResultType:        "success",
				HandledByClientID: "desktop-router",
				Result:            map[string]any{"clientId": "test-client"},
			}); err != nil {
				t.Errorf("write initialize failed: %v", err)
				return
			}
			continue
		}
		if msg.Method == "thread-follower-load-complete-history" {
			if err := writeWireMessage(conn, wireMessage{
				Type:           "broadcast",
				Method:         "thread-stream-state-changed",
				SourceClientID: "desktop-owner",
				Params: map[string]any{
					"conversationId": "thread-1",
					"hostId":         "local",
					"change": map[string]any{
						"type":     "snapshot",
						"revision": float64(42),
					},
				},
			}); err != nil {
				t.Errorf("write broadcast failed: %v", err)
			}
			return
		}
	}
}

func serveDiscoveryTest(t *testing.T, listener net.Listener, discoveryResponse chan<- wireMessage) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
			return
		}
		t.Errorf("Accept failed: %v", err)
		return
	}
	defer conn.Close()
	initialize, err := readWireMessage(conn)
	if err != nil {
		t.Errorf("read initialize failed: %v", err)
		return
	}
	if err := writeWireMessage(conn, wireMessage{
		Type:       "response",
		RequestID:  initialize.RequestID,
		Method:     initialize.Method,
		ResultType: "success",
		Result:     map[string]any{"clientId": "test-client"},
	}); err != nil {
		t.Errorf("write initialize failed: %v", err)
		return
	}
	if err := writeWireMessage(conn, wireMessage{
		Type:      "client-discovery-request",
		RequestID: "discover-1",
		Request: map[string]any{
			"method": "ide-context",
		},
	}); err != nil {
		t.Errorf("write discovery failed: %v", err)
		return
	}
	discoverySeen := false
	loadSeen := false
	for {
		msg, err := readWireMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Errorf("read response failed: %v", err)
			return
		}
		if msg.Type == "client-discovery-response" {
			discoveryResponse <- msg
			discoverySeen = true
		}
		if msg.Method == "thread-follower-load-complete-history" {
			if err := writeWireMessage(conn, wireMessage{
				Type:       "response",
				RequestID:  msg.RequestID,
				Method:     msg.Method,
				ResultType: "success",
				Result:     map[string]any{"revision": float64(1)},
			}); err != nil {
				t.Errorf("write request response failed: %v", err)
				return
			}
			loadSeen = true
		}
		if discoverySeen && loadSeen {
			return
		}
	}
}

func readWireMessage(reader io.Reader) (wireMessage, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return wireMessage{}, err
	}
	body := make([]byte, binary.LittleEndian.Uint32(header[:]))
	if _, err := io.ReadFull(reader, body); err != nil {
		return wireMessage{}, err
	}
	var msg wireMessage
	return msg, json.Unmarshal(body, &msg)
}

func writeWireMessage(writer io.Writer, msg wireMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(body)))
	_, err = writer.Write(append(header, body...))
	return err
}
