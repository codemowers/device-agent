package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const version = "0.1.0"

const (
	maxOutputBytes = 1 << 20 // 1 MB
	maxFileBytes   = 1 << 20 // 1 MB
	shellTimeout   = 30 * time.Second
	daemonEnvKey   = "DEVICE_AGENT_DAEMON"
)

type helloMsg struct {
	Type       string            `json:"type"`
	Hostname   string            `json:"hostname"`
	Version    string            `json:"version"`
	LSBRelease map[string]string `json:"lsb_release,omitempty"`
	DefaultIP  string            `json:"default_ip,omitempty"`
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
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	// Fallback: read /etc/hostname directly (works on minimal Linux installs
	// where gethostname() syscall returns empty due to missing hostname config).
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		if h := strings.TrimSpace(string(data)); h != "" {
			return h
		}
	}
	return "unknown"
}

// parseLSBRelease reads /etc/lsb-release and returns its key=value pairs.
func parseLSBRelease() map[string]string {
	f, err := os.Open("/etc/lsb-release")
	if err != nil {
		return nil
	}
	defer f.Close()

	m := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"`)
		m[key] = val
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// defaultRouteIP returns the local IP used for the default route by dialing
// a UDP connection (no packets sent) and reading the local address.
func defaultRouteIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
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

// forkBackground re-execs self with DEVICE_AGENT_DAEMON=1 in a new session,
// then exits the current process. The caller's connection is abandoned; the
// daemon child reconnects via the normal retry loop.
func forkBackground(endpoint string) {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("daemonize: cannot find executable: %v", err)
		return
	}
	cmd := exec.Command(exe, "--endpoint="+endpoint)
	cmd.Env = append(os.Environ(), daemonEnvKey+"=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("daemonize: %v", err)
		return
	}
	log.Printf("forked to background, pid=%d", cmd.Process.Pid)
	os.Exit(0)
}

func runSession(endpoint string, h string, lsb map[string]string, ip string, foreground bool) {
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		log.Printf("dial: %v", err)
		return
	}
	defer conn.Close()

	hello, _ := json.Marshal(helloMsg{
		Type:       "hello",
		Hostname:   h,
		Version:    version,
		LSBRelease: lsb,
		DefaultIP:  ip,
	})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		log.Printf("hello: %v", err)
		return
	}
	log.Printf("connected")

	// Fork to background after first successful connection unless --foreground
	// or already running as the daemon child.
	if !foreground && os.Getenv(daemonEnvKey) == "" {
		forkBackground(endpoint)
		// forkBackground only returns on failure; continue in foreground
	}

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
	foreground := flag.Bool("foreground", false, "stay in foreground instead of forking to background after connection")
	flag.Parse()
	if *endpoint == "" {
		log.Fatal("--endpoint is required")
	}

	h := hostname()
	lsb := parseLSBRelease()
	ip := defaultRouteIP()

	// Print identification info at startup (skip in daemon child to avoid duplicate output).
	if os.Getenv(daemonEnvKey) == "" {
		fmt.Printf("hostname: %s\n", h)
		fmt.Printf("default_ip: %s\n", ip)
		if lsb != nil {
			for k, v := range lsb {
				fmt.Printf("lsb: %s=%s\n", k, v)
			}
		}
	}

	backoff := time.Second
	for {
		runSession(*endpoint, h, lsb, ip, *foreground)
		log.Printf("reconnecting in %v", backoff)
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(60*time.Second)))
	}
}
