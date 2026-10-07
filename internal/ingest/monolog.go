package ingest

import (
    "bufio"
    "encoding/json"
    "log"
    "net"

    "github.com/iniznet/fenrirealm-monitor/internal/store"
)

// Monolog is the drop-in replacement for the Buggregator log socket: a TCP
// listener reading newline-delimited JSON records — the format Monolog's
// SocketHandler + JsonFormatter emits, which is what the backend's
// BuggregatorLogFormatterFactory already ships.
type Monolog struct {
    Store *store.Store
}

func (m *Monolog) ListenAndServe(addr string) error {
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        return err
    }
    for {
        conn, err := ln.Accept()
        if err != nil {
            return err
        }
        go m.serveConn(conn)
    }
}

func (m *Monolog) serveConn(conn net.Conn) {
    defer conn.Close()
    scanner := bufio.NewScanner(conn)
    scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
    for scanner.Scan() {
        line := scanner.Bytes()
        if len(line) == 0 {
            continue
        }
        m.ingestLine(line)
    }
}

func (m *Monolog) ingestLine(line []byte) {
    var record struct {
        ProjectKey  string         `json:"project_key"`
        Channel     string         `json:"channel"`
        LevelName   string         `json:"level_name"`
        Message     string         `json:"message"`
        Context     map[string]any `json:"context"`
        Environment string         `json:"environment"`
    }
    if err := json.Unmarshal(line, &record); err != nil {
        return
    }
    if record.Message == "" {
        return
    }
    projectKey := record.ProjectKey
    if projectKey == "" {
        projectKey = "default"
    }
    channel := record.Channel
    if channel == "" {
        channel = "default"
    }
    level := record.LevelName
    if level == "" {
        level = "info"
    }
    if _, err := m.Store.RecordLog(projectKey, channel, level, record.Message, record.Context, record.Environment); err != nil {
        log.Printf("record log: %v", err)
    }
}