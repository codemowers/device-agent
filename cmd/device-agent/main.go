package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
)

const version = "0.1.0"

const (
	maxOutputBytes = 1 << 20 // 1 MB
	maxFileBytes   = 1 << 20 // 1 MB
	shellTimeout   = 30 * time.Second
)

type helloMsg struct {
	Type     string `json:"type"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

type command struct {
	ID   string          `json:"id"`
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args"`
}

type shellArgs struct {
	Command string `json:"command"`
}

type fileReadArgs struct {
	Path string `json:"path"`
}

type fileWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type result struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Content  string `json:"content,omitempty"`
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown"
	}
	return h
}

func doShell(raw json.RawMessage) result {
	var a shellArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return result{OK: false, Error: "invalid args"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.Command)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return result{OK: false, Error: err.Error()}
		}
	}

	out := stdout.String()
	if len(out) > maxOutputBytes {
		out = out[:maxOutputBytes] + "\n[truncated]"
	}
	return result{OK: true, Stdout: out, Stderr: stderr.String(), ExitCode: exitCode}
}

func doFileRead(raw json.RawMessage) result {
	var a fileReadArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return result{OK: false, Error: "invalid args"}
	}
	data, err := os.ReadFile(filepath.Clean(a.Path))
	if err != nil {
		return result{OK: false, Error: err.Error()}
	}
	if len(data) > maxFileBytes {
		return result{OK: false, Error: "file too large"}
	}
	return result{OK: true, Content: string(data)}
}

func doFileWrite(raw json.RawMessage) result {
	var a fileWriteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return result{OK: false, Error: "invalid args"}
	}
	if len(a.Content) > maxFileBytes {
		return result{OK: false, Error: "content too large"}
	}
	path := filepath.Clean(a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result{OK: false, Error: err.Error()}
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return result{OK: false, Error: err.Error()}
	}
	return result{OK: true}
}

func dispatch(cmd command) result {
	var r result
	switch cmd.Cmd {
	case "shell":
		r = doShell(cmd.Args)
	case "file_read":
		r = doFileRead(cmd.Args)
	case "file_write":
		r = doFileWrite(cmd.Args)
	default:
		r = result{OK: false, Error: "unknown command: " + cmd.Cmd}
	}
	r.ID = cmd.ID
	return r
}

func runSession(endpoint string) {
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		log.Printf("dial: %v", err)
		return
	}
	defer conn.Close()

	hello, _ := json.Marshal(helloMsg{Type: "hello", Hostname: hostname(), Version: version})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		log.Printf("hello: %v", err)
		return
	}
	log.Printf("connected")

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read: %v", err)
			return
		}
		var cmd command
		if err := json.Unmarshal(msg, &cmd); err != nil {
			log.Printf("parse: %v", err)
			continue
		}
		resp, _ := json.Marshal(dispatch(cmd))
		if err := conn.WriteMessage(websocket.TextMessage, resp); err != nil {
			log.Printf("write: %v", err)
			return
		}
	}
}

func main() {
	endpoint := flag.String("endpoint", "", "WebSocket endpoint URL, e.g. wss://host/devices/KEY")
	flag.Parse()
	if *endpoint == "" {
		log.Fatal("--endpoint is required")
	}

	backoff := time.Second
	for {
		runSession(*endpoint)
		log.Printf("reconnecting in %v", backoff)
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(60*time.Second)))
	}
}
